package server

// Uptime checks and incidents.
//
// One goroutine probes every live web app with an enabled check once per
// uptimeInterval (bounded concurrency): the in-cluster Service by default,
// or the public URL with --external. Three consecutive failures open an
// incident and notify app_down; two consecutive passes resolve it and
// notify app_recovered. Results land in uptime_samples (48h) and
// uptime_daily (400d), which the status page (statuspage.go) reads.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sutantodadang/luncur/internal/store"
)

const (
	uptimeDownAfter    = 3
	uptimeRecoverAfter = 2
)

// Test seams.
var uptimeInterval = time.Minute

// uptimeResult is one probe's outcome.
type uptimeResult struct {
	OK        bool
	Code      int
	LatencyMS int
	Err       string
}

// uptimeTarget is one app the prober checks this round.
type uptimeTarget struct {
	p     store.Project
	env   store.Environment
	a     store.App
	check store.UptimeCheck
}

// uptimeTargets lists live web apps whose check is enabled. Internal apps
// default to off (no public promise to keep) but can opt in.
func (s *server) uptimeTargets() []uptimeTarget {
	projects, err := s.st.ListProjects()
	if err != nil {
		return nil
	}
	var out []uptimeTarget
	for _, p := range projects {
		apps, err := s.st.ListApps(p.ID)
		if err != nil {
			continue
		}
		for _, a := range apps {
			if a.Kind != "" && a.Kind != "web" {
				continue
			}
			c, err := s.st.GetUptimeCheck(a.ID)
			if err != nil || !s.uptimeEnabled(a, c) {
				continue
			}
			if d, err := s.st.LatestDeployment(a.ID); err != nil || (d.Status != "live" && d.Status != "deploying") {
				continue
			}
			env, err := s.st.GetEnvironmentByID(a.EnvironmentID)
			if err != nil {
				continue
			}
			out = append(out, uptimeTarget{p: p, env: env, a: a, check: c})
		}
	}
	return out
}

// uptimeEnabled applies the default: on for public web apps, off for
// internal ones (no public promise to keep), unless a choice is stored.
func (s *server) uptimeEnabled(a store.App, c store.UptimeCheck) bool {
	if !c.Stored {
		return !a.Internal
	}
	return c.Enabled
}

// uptimeURL is what a check probes.
func (s *server) uptimeURL(t uptimeTarget) string {
	path := t.check.Path
	if path == "" {
		path = t.a.HealthPath
	}
	if path == "" {
		path = "/"
	}
	if t.check.External {
		return strings.TrimRight(s.appURLForEnv(t.a, t.env.Name, t.p.DefaultEnv), "/") + path
	}
	return "http://" + t.a.Name + "." + t.env.Namespace + ".svc.cluster.local" + path
}

// uptimeProbe makes one request; any status below 500 is up.
func (s *server) uptimeProbe(ctx context.Context, url string) uptimeResult {
	if s.uptimeProbeFn != nil {
		return s.uptimeProbeFn(url)
	}
	pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	start := time.Now()
	req, err := http.NewRequestWithContext(pctx, http.MethodGet, url, nil)
	if err != nil {
		return uptimeResult{Err: err.Error()}
	}
	req.Header.Set("User-Agent", "luncur-uptime/1")
	resp, err := http.DefaultClient.Do(req)
	lat := int(time.Since(start).Milliseconds())
	if err != nil {
		return uptimeResult{LatencyMS: lat, Err: firstLineOf(err.Error())}
	}
	resp.Body.Close()
	r := uptimeResult{OK: resp.StatusCode < 500, Code: resp.StatusCode, LatencyMS: lat}
	if !r.OK {
		r.Err = fmt.Sprintf("HTTP %d", resp.StatusCode)
	}
	return r
}

func firstLineOf(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}

// uptimeRound probes every target once (at most 8 at a time).
func (s *server) uptimeRound(ctx context.Context) {
	targets := s.uptimeTargets()
	sem := make(chan struct{}, 8)
	var wg sync.WaitGroup
	for _, t := range targets {
		t := t
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			s.recordUptime(t, s.uptimeProbe(ctx, s.uptimeURL(t)), time.Now())
		}()
	}
	wg.Wait()
}

// recordUptime stores a probe and runs the up/down state machine.
func (s *server) recordUptime(t uptimeTarget, r uptimeResult, now time.Time) {
	if err := s.st.AddUptimeSample(t.a.ID, store.UptimeSample{At: now, OK: r.OK, LatencyMS: r.LatencyMS, Code: r.Code}); err != nil {
		log.Printf("uptime %s/%s: record: %v", t.p.Name, t.a.Name, err)
		return
	}
	c := t.check
	c.LastAt = now.UTC().Format(time.RFC3339)
	if r.OK {
		c.OKStreak++
		c.FailStreak = 0
		c.LastError = ""
		if c.State != "down" {
			c.State = "up"
		} else if c.OKStreak >= uptimeRecoverAfter {
			c.State = "up"
			s.uptimeRecovered(t)
		}
	} else {
		c.FailStreak++
		c.OKStreak = 0
		c.LastError = r.Err
		if c.State != "down" && c.FailStreak >= uptimeDownAfter {
			c.State = "down"
			s.uptimeDown(t, r.Err)
		}
	}
	if err := s.st.SetUptimeState(c); err != nil {
		log.Printf("uptime %s/%s: state: %v", t.p.Name, t.a.Name, err)
	}
}

func (s *server) uptimeDown(t uptimeTarget, reason string) {
	if reason == "" {
		reason = "unreachable"
	}
	in, err := s.st.OpenIncident(t.p.ID, t.a.ID, t.a.Name+" is down", true)
	if err == nil {
		s.st.AddIncidentUpdate(in.ID, fmt.Sprintf("%d consecutive checks failed: %s", uptimeDownAfter, reason))
	}
	s.notify(notifyEvent{Event: "app_down", Project: t.p.Name, App: t.a.Name, Err: reason, URL: s.appURLForEnv(t.a, t.env.Name, t.p.DefaultEnv)})
}

func (s *server) uptimeRecovered(t uptimeTarget) {
	msg := "checks passing again"
	if in, err := s.st.OpenAutoIncident(t.a.ID); err == nil {
		if opened, err := time.ParseInLocation("2006-01-02 15:04:05", in.OpenedAt, time.UTC); err == nil {
			msg = fmt.Sprintf("checks passing again after %s", time.Since(opened).Round(time.Minute))
		}
		s.st.AddIncidentUpdate(in.ID, "Recovered: "+msg)
		s.st.ResolveIncident(in.ID)
	}
	s.notify(notifyEvent{Event: "app_recovered", Project: t.p.Name, App: t.a.Name, Message: msg})
}

// StartUptime runs the prober until ctx ends, and rolls up/prunes hourly.
func (s *server) StartUptime(ctx context.Context) {
	tick := time.NewTicker(uptimeInterval)
	defer tick.Stop()
	lastRollup := time.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		s.uptimeRound(ctx)
		if time.Since(lastRollup) >= time.Hour {
			if err := s.st.RollupUptimeP95(time.Now()); err != nil {
				log.Printf("uptime roll-up: %v", err)
			}
			lastRollup = time.Now()
		}
	}
}

// uptimeView is the API/UI view of an app's check.
type uptimeView struct {
	store.UptimeCheck
	Enabled bool              `json:"enabled"`
	URL     string            `json:"url"`
	Stats   store.UptimeStats `json:"stats"`
	Days    []store.UptimeDay `json:"days"`
}

func (s *server) appUptimeView(p store.Project, env store.Environment, a store.App, days int) (uptimeView, error) {
	c, err := s.st.GetUptimeCheck(a.ID)
	if err != nil {
		return uptimeView{}, err
	}
	v := uptimeView{UptimeCheck: c, Enabled: s.uptimeEnabled(a, c)}
	v.URL = s.uptimeURL(uptimeTarget{p: p, env: env, a: a, check: c})
	if v.Stats, err = s.st.GetUptimeStats(a.ID, time.Now()); err != nil {
		return v, err
	}
	v.Days, err = s.st.UptimeDays(a.ID, days, time.Now())
	return v, err
}

func (s *server) handleGetUptime(w http.ResponseWriter, r *http.Request, u store.User) {
	p, env, ok := s.requireEnv(w, r, u, r.PathValue("project"), r.PathValue("env"))
	if !ok {
		return
	}
	a, ok := s.requireApp(w, p, env, r.PathValue("app"))
	if !ok {
		return
	}
	v, err := s.appUptimeView(p, env, a, 90)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *server) handlePutUptime(w http.ResponseWriter, r *http.Request, u store.User) {
	p, env, ok := s.requireEnvWrite(w, r, u, r.PathValue("project"), r.PathValue("env"))
	if !ok {
		return
	}
	a, ok := s.requireApp(w, p, env, r.PathValue("app"))
	if !ok {
		return
	}
	var req struct {
		Enabled  *bool   `json:"enabled"`
		External *bool   `json:"external"`
		Path     *string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	if err := s.setUptime(a, req.Enabled, req.External, req.Path); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	v, _ := s.appUptimeView(p, env, a, 90)
	writeJSON(w, http.StatusOK, v)
}

// setUptime merges a partial check update.
func (s *server) setUptime(a store.App, enabled, external *bool, path *string) error {
	if a.Kind != "" && a.Kind != "web" {
		return fmt.Errorf("uptime checks are for web apps")
	}
	c, err := s.st.GetUptimeCheck(a.ID)
	if err != nil {
		return err
	}
	c.Enabled = s.uptimeEnabled(a, c)
	if enabled != nil {
		c.Enabled = *enabled
	}
	if external != nil {
		c.External = *external
	}
	if path != nil {
		c.Path = strings.TrimSpace(*path)
	}
	return s.st.SetUptimeConfig(a.ID, c.Enabled, c.External, c.Path)
}

// ---- incidents ----

func (s *server) handleListIncidents(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.requireProject(w, u, r.PathValue("project"))
	if !ok {
		return
	}
	list, err := s.st.ListIncidents(p.ID, 50)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"incidents": list})
}

func (s *server) handleOpenIncident(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.requireProjectWrite(w, u, r.PathValue("project"))
	if !ok {
		return
	}
	var req struct {
		Title string `json:"title"`
		App   string `json:"app"`
		Body  string `json:"body"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	in, err := s.openIncident(p, req.Title, req.App, req.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, in)
}

func (s *server) openIncident(p store.Project, title, app, body string) (store.Incident, error) {
	var appID int64
	if app != "" {
		a, err := s.st.GetApp(p.ID, app)
		if err != nil {
			return store.Incident{}, fmt.Errorf("no app %q in %s", app, p.Name)
		}
		appID = a.ID
	}
	in, err := s.st.OpenIncident(p.ID, appID, title, false)
	if err != nil {
		return store.Incident{}, err
	}
	if strings.TrimSpace(body) != "" {
		if err := s.st.AddIncidentUpdate(in.ID, body); err != nil {
			return in, err
		}
	}
	return s.st.GetIncident(in.ID)
}

// projectIncident loads an incident and checks it belongs to p.
func (s *server) projectIncident(w http.ResponseWriter, p store.Project, idStr string) (store.Incident, bool) {
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid incident id")
		return store.Incident{}, false
	}
	in, err := s.st.GetIncident(id)
	if errors.Is(err, store.ErrNotFound) || (err == nil && in.ProjectID != p.ID) {
		writeError(w, http.StatusNotFound, "not_found", "no such incident")
		return store.Incident{}, false
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return store.Incident{}, false
	}
	return in, true
}

func (s *server) handleIncidentUpdate(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.requireProjectWrite(w, u, r.PathValue("project"))
	if !ok {
		return
	}
	in, ok := s.projectIncident(w, p, r.PathValue("id"))
	if !ok {
		return
	}
	var req struct {
		Body string `json:"body"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	if err := s.st.AddIncidentUpdate(in.ID, req.Body); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	in, _ = s.st.GetIncident(in.ID)
	writeJSON(w, http.StatusOK, in)
}

func (s *server) handleResolveIncident(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.requireProjectWrite(w, u, r.PathValue("project"))
	if !ok {
		return
	}
	in, ok := s.projectIncident(w, p, r.PathValue("id"))
	if !ok {
		return
	}
	if err := s.st.ResolveIncident(in.ID); err != nil {
		writeError(w, http.StatusConflict, "conflict", err.Error())
		return
	}
	in, _ = s.st.GetIncident(in.ID)
	writeJSON(w, http.StatusOK, in)
}
