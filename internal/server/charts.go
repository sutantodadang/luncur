package server

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/sutantodadang/luncur/internal/store"
)

// chartWidth/chartHeight size every sparkline's SVG viewBox.
const (
	chartWidth  = 600
	chartHeight = 48
)

// sparkPoints renders samples as an SVG polyline points list on a w×h
// viewBox, oldest→newest left→right, y scaled to the series max (min 1 so
// a flat zero line sits on the floor). Fewer than 2 samples → "".
func sparkPoints(samples []metricSample, pick func(metricSample) int64, w, h int) string {
	n := len(samples)
	if n < 2 {
		return ""
	}
	var max int64 = 1
	for _, s := range samples {
		if v := pick(s); v > max {
			max = v
		}
	}
	var b strings.Builder
	for i, s := range samples {
		x := i * (w - 1) / (n - 1)
		y := (h - 1) - int(pick(s)*int64(h-2)/max)
		if i > 0 {
			b.WriteByte(' ')
		}
		fmt.Fprintf(&b, "%d,%d", x, y)
	}
	return b.String()
}

// chartView is the "metricschart" template block's view model.
type chartView struct {
	Title                string
	CPUPoints, MemPoints string
	CPUNow, CPUPeak      int64 // millicores
	MemNow, MemPeak      int64 // MiB
	Collecting           bool  // <2 samples
}

// chartViewFrom builds one chart's view model from its ring's snapshot,
// oldest-first. samples == nil (no ring yet) behaves like too few samples.
func chartViewFrom(title string, samples []metricSample) chartView {
	v := chartView{Title: title, Collecting: len(samples) < 2}
	if len(samples) == 0 {
		return v
	}
	last := samples[len(samples)-1]
	v.CPUNow, v.MemNow = last.CPUMilli, last.MemoryMiB
	for _, s := range samples {
		if s.CPUMilli > v.CPUPeak {
			v.CPUPeak = s.CPUMilli
		}
		if s.MemoryMiB > v.MemPeak {
			v.MemPeak = s.MemoryMiB
		}
	}
	if !v.Collecting {
		v.CPUPoints = sparkPoints(samples, func(s metricSample) int64 { return s.CPUMilli }, chartWidth, chartHeight)
		v.MemPoints = sparkPoints(samples, func(s metricSample) int64 { return s.MemoryMiB }, chartWidth, chartHeight)
	}
	return v
}

// handleUIAppChart is app.html's "Live metrics" card polling fragment —
// htmx re-fetches it every 15s, same cadence as the sampler.
func (s *server) handleUIAppChart(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.uiProject(w, r, u)
	if !ok {
		return
	}
	a, ok := s.uiApp(w, r, p)
	if !ok {
		return
	}
	env, err := s.st.GetEnvironmentByID(a.EnvironmentID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "get environment: "+err.Error())
		return
	}
	view := chartViewFrom(a.Name, s.mon.appSamples(env.Namespace+"/"+a.Name))
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.ExecuteTemplate(w, "metricschart", view); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "render metricschart: "+err.Error())
	}
}

// handleUINodeCharts is the nodes page's polling fragment — one chart per
// node currently tracked by the monitor. No kube configured, or no nodes
// sampled yet, both render as an empty/"collecting" fragment rather than
// erroring.
func (s *server) handleUINodeCharts(w http.ResponseWriter, r *http.Request, u store.User) {
	if !s.uiAdmin(w, u) {
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if s.kube == nil {
		if err := s.tmpl.ExecuteTemplate(w, "nodecharts", nil); err != nil {
			writeError(w, http.StatusInternalServerError, "internal", "render nodecharts: "+err.Error())
		}
		return
	}
	keys := s.mon.nodeKeys()
	views := make([]chartView, 0, len(keys))
	for _, name := range keys {
		views = append(views, chartViewFrom(name, s.mon.nodeSamples(name)))
	}
	if err := s.tmpl.ExecuteTemplate(w, "nodecharts", views); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "render nodecharts: "+err.Error())
	}
}

// handleAppMetricsHistory returns an app's sampled usage history (last
// ~30 minutes) as JSON — the CLI's `luncur metrics` command and any other
// non-browser consumer.
func (s *server) handleAppMetricsHistory(w http.ResponseWriter, r *http.Request, u store.User) {
	p, env, ok := s.requireEnv(w, r, u, r.PathValue("project"), r.PathValue("env"))
	if !ok {
		return
	}
	a, ok := s.requireApp(w, p, env, r.PathValue("app"))
	if !ok {
		return
	}
	samples := s.mon.appSamples(env.Namespace + "/" + a.Name)
	out := make([]map[string]any, 0, len(samples))
	for _, sm := range samples {
		out = append(out, map[string]any{
			"at": sm.At.UTC().Format(time.RFC3339), "cpu_millicores": sm.CPUMilli, "memory_mib": sm.MemoryMiB,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"samples": out})
}
