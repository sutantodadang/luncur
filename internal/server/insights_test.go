package server

import (
	"strings"
	"testing"
	"time"

	"github.com/sutantodadang/luncur/internal/kube"
	"github.com/sutantodadang/luncur/internal/store"
)

// Monitor samples fold into hourly per-pod aggregates, flushed when the
// hour rolls over.
func TestUsageAccumulatesAndFlushesHourly(t *testing.T) {
	s := aiTestServer(t)
	p, _ := s.st.CreateProject("shop")
	_, env := seedDefaultEnv(t, s.st, p)
	a, _ := s.st.CreateAppInEnv(env.ID, "web", 8080, "web", "")
	key := env.Namespace + "/web"
	h0 := time.Date(2026, 10, 7, 10, 5, 0, 0, time.UTC)
	for i, cpu := range []int64{100, 200, 300, 400} { // 2 pods: per-pod 50..200
		s.accumulateUsage(h0.Add(time.Duration(i)*time.Minute), map[string]kube.AppMetrics{key: {CPUMilli: cpu, MemoryMiB: 256 + int64(i)*64, Pods: 2}})
	}
	if u, _ := s.st.AppUsageSummary(a.ID, h0.Add(-time.Hour)); u.Hours != 0 {
		t.Fatal("flushed before the hour ended")
	}
	s.accumulateUsage(h0.Add(time.Hour), nil) // next hour: flush
	u, err := s.st.AppUsageSummary(a.ID, h0.Add(-time.Hour))
	if err != nil || u.Hours != 1 || u.CPUP95Milli != 200 || u.CPUMaxMilli != 200 || u.MemMaxMB != 224 {
		t.Fatalf("summary = %+v %v", u, err)
	}
}

func TestInsightFlagsAndCosts(t *testing.T) {
	s := aiTestServer(t)
	p, _ := s.st.CreateProject("shop")
	p, env := seedDefaultEnv(t, s.st, p)
	a, _ := s.st.CreateAppInEnv(env.ID, "web", 8080, "web", "")
	s.st.SetResources(a.ID, 2000, 2048) // 2 cores, 2Gi
	a, _ = s.st.GetAppByID(a.ID)
	s.st.SetSetting("cost_cpu_core_month", "20")
	s.st.SetSetting("cost_mem_gb_month", "5")
	now := time.Now().UTC()
	prices := s.costPrices()

	in := s.appInsightFor(p, env, a, prices, now)
	if in.Flag != "nodata" || in.Monthly != 50 {
		t.Fatalf("no data: %+v", in)
	}
	for h := 1; h <= 8; h++ {
		s.st.UpsertUsageHour(a.ID, store.UsageHour{Hour: now.Add(-time.Duration(h) * time.Hour).Format("2006-01-02T15"), CPUP95Milli: 100, CPUMaxMilli: 150, MemMaxMB: 200, Samples: 60})
	}
	in = s.appInsightFor(p, env, a, prices, now)
	if in.Flag != "over" || in.RecCPU != 120 || in.RecMem != 272 || in.Savings <= 0 || !strings.Contains(in.ScaleCmd, "--cpu 120m --memory 272Mi") {
		t.Fatalf("over-provisioned: %+v", in)
	}

	// Memory peak near the limit is "at risk".
	s.st.SetResources(a.ID, 200, 220)
	a, _ = s.st.GetAppByID(a.ID)
	if in = s.appInsightFor(p, env, a, prices, now); in.Flag != "risk" {
		t.Fatalf("at risk: %+v", in)
	}

	// Members see only their projects in the report.
	other, _ := s.st.CreateProject("other")
	seedDefaultEnv(t, s.st, other)
	member, _ := s.st.CreateUser("m@b.co", "pw-123456", "member")
	s.st.AddMember(p.ID, member.ID, "member")
	rep, err := s.insights(t.Context(), member, "")
	if err != nil || len(rep.ProjectsCosted) != 1 || rep.ProjectsCosted[0] != "shop" {
		t.Fatalf("member report = %+v %v", rep, err)
	}
}

func TestUIInsightsPage(t *testing.T) {
	u := newUIAI(t, nil)
	page := u.get(t, "/ui/insights")
	for _, want := range []string{"Insights", "luncur insights", "shop/web", `href="/ui/insights"`} {
		if !strings.Contains(page, want) {
			t.Fatalf("insights page missing %q", want)
		}
	}
	obs := u.get(t, "/ui/projects/shop/apps/web?tab=observe")
	if !strings.Contains(obs, `id="rightsize"`) {
		t.Fatal("observe tab has no right-size card")
	}
}
