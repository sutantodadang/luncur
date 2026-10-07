package server

// Cost and right-sizing insights.
//
// The metrics monitor already samples per-app usage every interval; it
// also folds those samples into hourly per-pod aggregates
// (app_usage_hourly, 30 days). Insights compare each app's requests with
// its observed usage (highest hourly p95 CPU, peak memory, last 7 days)
// and recommend requests with headroom: CPU p95 × 1.2, memory peak × 1.3.
// Costs are estimates from user-entered monthly unit prices
// (cost_cpu_core_month, cost_mem_gb_month, cost_gpu_month).

import (
	"context"
	"log"
	"math"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/sutantodadang/luncur/internal/kube"
	"github.com/sutantodadang/luncur/internal/store"
)

const (
	insightWindow   = 7 * 24 * time.Hour
	insightMinHours = 6 // below this, too little data to recommend
)

// usageBucket accumulates one app's samples for the current hour.
type usageBucket struct {
	hour   string
	cpu    []int64 // per-pod CPU millicores per sample
	memMax int64   // per-pod MiB
}

type usageAccumulator struct {
	mu      sync.Mutex
	buckets map[string]*usageBucket // "<namespace>/<app>"
}

// accumulateUsage folds one monitor tick into the hourly buckets, flushing
// a bucket to the store when its hour ends.
func (s *server) accumulateUsage(now time.Time, apps map[string]kube.AppMetrics) {
	hour := now.UTC().Format("2006-01-02T15")
	var flush map[string]*usageBucket
	s.usage.mu.Lock()
	if s.usage.buckets == nil {
		s.usage.buckets = map[string]*usageBucket{}
	}
	for key, b := range s.usage.buckets {
		if b.hour != hour {
			if flush == nil {
				flush = map[string]*usageBucket{}
			}
			flush[key] = b
			delete(s.usage.buckets, key)
		}
	}
	for key, m := range apps {
		if m.Pods < 1 {
			continue
		}
		b := s.usage.buckets[key]
		if b == nil {
			b = &usageBucket{hour: hour}
			s.usage.buckets[key] = b
		}
		pods := int64(m.Pods)
		b.cpu = append(b.cpu, m.CPUMilli/pods)
		if mem := m.MemoryMiB / pods; mem > b.memMax {
			b.memMax = mem
		}
	}
	s.usage.mu.Unlock()
	if flush != nil {
		s.flushUsage(flush)
	}
}

// flushUsage writes finished hourly buckets, resolving "<ns>/<app>" keys
// to app ids.
func (s *server) flushUsage(buckets map[string]*usageBucket) {
	ids := s.appIDsByNamespaceKey()
	for key, b := range buckets {
		id, ok := ids[key]
		if !ok || len(b.cpu) == 0 {
			continue
		}
		sorted := append([]int64(nil), b.cpu...)
		sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
		p95 := sorted[(len(sorted)*95+99)/100-1]
		if err := s.st.UpsertUsageHour(id, store.UsageHour{Hour: b.hour, CPUP95Milli: p95, CPUMaxMilli: sorted[len(sorted)-1], MemMaxMB: b.memMax, Samples: len(b.cpu)}); err != nil {
			log.Printf("usage flush %s: %v", key, err)
		}
	}
}

func (s *server) appIDsByNamespaceKey() map[string]int64 {
	out := map[string]int64{}
	projects, err := s.st.ListProjects()
	if err != nil {
		return out
	}
	for _, p := range projects {
		apps, err := s.st.ListApps(p.ID)
		if err != nil {
			continue
		}
		for _, a := range apps {
			if ns, err := s.appNamespace(a); err == nil {
				out[ns+"/"+a.Name] = a.ID
			}
		}
	}
	return out
}

// costPrices are the monthly unit prices (0 = not set).
type costPrices struct {
	Currency string  `json:"currency"`
	CPUCore  float64 `json:"cpu_core_month"`
	MemGB    float64 `json:"memory_gb_month"`
	GPU      float64 `json:"gpu_month"`
}

func (s *server) costPrices() costPrices {
	f := func(key string) float64 {
		v, _ := strconv.ParseFloat(s.settingOr(key, "0"), 64)
		return v
	}
	return costPrices{Currency: s.settingOr("cost_currency", "$"), CPUCore: f("cost_cpu_core_month"), MemGB: f("cost_mem_gb_month"), GPU: f("cost_gpu_month")}
}

// appInsight is one app's right-sizing and cost line.
type appInsight struct {
	Project  string `json:"project"`
	Env      string `json:"env"`
	App      string `json:"app"`
	Kind     string `json:"kind"`
	Replicas int    `json:"replicas"`
	// Requests per pod (app setting, or the workload default when unset).
	CPURequest int64 `json:"cpu_request_millicores"`
	MemRequest int64 `json:"memory_request_mib"`
	MemLimit   int64 `json:"memory_limit_mib,omitempty"`
	GPU        int64 `json:"gpu,omitempty"`
	store.UsageSummary
	RecCPU int64 `json:"recommended_cpu_millicores,omitempty"`
	RecMem int64 `json:"recommended_memory_mib,omitempty"`
	// Flag: over (requests > 2× recommended), risk (memory peak > 90% of
	// the limit), ok, or nodata.
	Flag     string  `json:"flag"`
	Monthly  float64 `json:"monthly_cost"`
	Savings  float64 `json:"monthly_savings"`
	ScaleCmd string  `json:"scale_command,omitempty"`
}

// insightsReport is the whole report.
type insightsReport struct {
	Prices         costPrices   `json:"prices"`
	MetricsOK      bool         `json:"metrics_available"`
	Apps           []appInsight `json:"apps"`
	MonthlyTotal   float64      `json:"monthly_total"`
	SavingsTotal   float64      `json:"monthly_savings_total"`
	OverProvision  int          `json:"over_provisioned"`
	AtRisk         int          `json:"at_risk"`
	ProjectsCosted []string     `json:"projects"`
}

func roundUp(v float64, step int64) int64 {
	return int64(math.Ceil(v/float64(step))) * step
}

// appInsightFor computes one app's line.
func (s *server) appInsightFor(p store.Project, env store.Environment, a store.App, prices costPrices, now time.Time) appInsight {
	defCPU, defMem := s.workloadDefaults()
	in := appInsight{Project: p.Name, Env: env.Name, App: a.Name, Kind: a.Kind, Replicas: a.Replicas, GPU: a.GPUCount, Flag: "nodata"}
	if in.Kind == "" {
		in.Kind = "web"
	}
	if a.AutoMin > 0 {
		in.Replicas = a.AutoMin
	}
	if in.Kind == "cron" || in.Kind == "job" {
		in.Replicas = 0 // run-to-completion: no standing cost
	}
	in.CPURequest, in.MemRequest = a.CPUMilli, a.MemoryMB
	if in.CPURequest == 0 {
		in.CPURequest = defCPU
	}
	if in.MemRequest == 0 {
		in.MemRequest = defMem
	}
	if a.MemoryMB > 0 {
		in.MemLimit = a.MemoryMB
	}
	r := float64(in.Replicas)
	in.Monthly = r * (float64(in.CPURequest)/1000*prices.CPUCore + float64(in.MemRequest)/1024*prices.MemGB + float64(in.GPU)*prices.GPU)

	u, err := s.st.AppUsageSummary(a.ID, now.Add(-insightWindow))
	if err != nil {
		return in
	}
	in.UsageSummary = u
	if u.Hours < insightMinHours || in.Replicas == 0 {
		return in
	}
	in.RecCPU = roundUp(math.Max(10, float64(u.CPUP95Milli)*1.2), 5)
	in.RecMem = roundUp(math.Max(32, float64(u.MemMaxMB)*1.3), 16)
	in.Flag = "ok"
	switch {
	case in.MemLimit > 0 && float64(u.MemMaxMB) > 0.9*float64(in.MemLimit):
		in.Flag = "risk"
	case in.CPURequest > 2*in.RecCPU || in.MemRequest > 2*in.RecMem:
		in.Flag = "over"
		cpuSave := math.Max(0, float64(in.CPURequest-in.RecCPU)) / 1000 * prices.CPUCore
		memSave := math.Max(0, float64(in.MemRequest-in.RecMem)) / 1024 * prices.MemGB
		in.Savings = r * (cpuSave + memSave)
	}
	if in.Flag != "ok" {
		in.ScaleCmd = "luncur scale " + a.Name + " --cpu " + strconv.FormatInt(in.RecCPU, 10) + "m --memory " + strconv.FormatInt(in.RecMem, 10) + "Mi --project " + p.Name
		if env.Name != p.DefaultEnv {
			in.ScaleCmd += " --env " + env.Name
		}
	}
	return in
}

// insights builds the report over the projects u can see (optionally one).
func (s *server) insights(ctx context.Context, u store.User, project string) (insightsReport, error) {
	var projects []store.Project
	var err error
	if u.Role == "admin" {
		projects, err = s.st.ListProjects()
	} else {
		projects, err = s.st.ListProjectsFor(u.ID)
	}
	if err != nil {
		return insightsReport{}, err
	}
	rep := insightsReport{Prices: s.costPrices()}
	if s.kube != nil {
		_, rep.MetricsOK = s.kube.ClusterPodUsage(ctx)
	}
	now := time.Now()
	for _, p := range projects {
		if project != "" && p.Name != project {
			continue
		}
		rep.ProjectsCosted = append(rep.ProjectsCosted, p.Name)
		apps, err := s.st.ListApps(p.ID)
		if err != nil {
			continue
		}
		for _, a := range apps {
			env, err := s.st.GetEnvironmentByID(a.EnvironmentID)
			if err != nil {
				continue
			}
			in := s.appInsightFor(p, env, a, rep.Prices, now)
			rep.Apps = append(rep.Apps, in)
			rep.MonthlyTotal += in.Monthly
			rep.SavingsTotal += in.Savings
			switch in.Flag {
			case "over":
				rep.OverProvision++
			case "risk":
				rep.AtRisk++
			}
		}
	}
	sort.SliceStable(rep.Apps, func(i, j int) bool {
		rank := map[string]int{"risk": 0, "over": 1, "ok": 2, "nodata": 3}
		if rank[rep.Apps[i].Flag] != rank[rep.Apps[j].Flag] {
			return rank[rep.Apps[i].Flag] < rank[rep.Apps[j].Flag]
		}
		return rep.Apps[i].Monthly > rep.Apps[j].Monthly
	})
	return rep, nil
}

func (s *server) handleInsights(w http.ResponseWriter, r *http.Request, u store.User) {
	project := r.URL.Query().Get("project")
	if project != "" {
		if _, ok := s.requireProject(w, u, project); !ok {
			return
		}
	}
	rep, err := s.insights(r.Context(), u, project)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	writeJSON(w, http.StatusOK, rep)
}

// handleUIInsights renders the Insights page.
func (s *server) handleUIInsights(w http.ResponseWriter, r *http.Request, u store.User) {
	rep, err := s.insights(r.Context(), u, r.URL.Query().Get("project"))
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.renderPage(w, r, "insights.html", map[string]any{"User": u, "R": rep, "CSRF": s.csrf(w, r), "IsAdmin": u.Role == "admin"})
}

// uiInsight is the Observe tab's right-size card (long-running kinds).
func (s *server) uiInsight(p store.Project, env store.Environment, a store.App, tab uiTab) *appInsight {
	if tab != tabObserve || a.Kind == "cron" || a.Kind == "job" {
		return nil
	}
	in := s.appInsightFor(p, env, a, s.costPrices(), time.Now())
	return &in
}
