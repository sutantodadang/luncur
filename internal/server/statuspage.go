package server

// Public status pages: GET /status/{slug} (HTML, no JS), /status/{slug}.json
// and /status/{slug}/badge.svg. Unauthenticated and rate-limited; they show
// only what the project chose to publish — display names, current state,
// 90 daily bars and recent incidents — never namespaces, images or
// internal URLs. Rendered views are cached briefly so a traffic spike on a
// status page can't load the database.

import (
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/sutantodadang/luncur/internal/store"
)

const (
	statusCacheTTL = 30 * time.Second
	statusLimit    = 120 // requests per IP per minute
)

// publicApp is one published component on a status page.
type publicApp struct {
	Name   string            `json:"name"`
	State  string            `json:"state"` // operational|degraded|down|unknown
	Pct90d float64           `json:"uptime_90d"`
	Days   []store.UptimeDay `json:"days"`
}

// publicIncident is an incident as the public sees it.
type publicIncident struct {
	Title      string                 `json:"title"`
	Status     string                 `json:"status"`
	App        string                 `json:"app,omitempty"`
	OpenedAt   string                 `json:"opened_at"`
	ResolvedAt string                 `json:"resolved_at,omitempty"`
	Updates    []store.IncidentUpdate `json:"updates,omitempty"`
}

// publicStatus is a status page's whole view.
type publicStatus struct {
	Title     string           `json:"title"`
	Overall   string           `json:"overall"`
	Apps      []publicApp      `json:"apps"`
	Incidents []publicIncident `json:"incidents"`
	Updated   string           `json:"updated_at"`
}

type statusCacheEntry struct {
	at   time.Time
	view publicStatus
}

type statusCache struct {
	mu sync.Mutex
	m  map[string]statusCacheEntry
}

// publicStatusView builds (or serves from cache) a published page's view.
func (s *server) publicStatusView(slug string) (publicStatus, error) {
	s.statusCache.mu.Lock()
	if e, ok := s.statusCache.m[slug]; ok && time.Since(e.at) < statusCacheTTL {
		s.statusCache.mu.Unlock()
		return e.view, nil
	}
	s.statusCache.mu.Unlock()

	sp, err := s.st.GetStatusPageBySlug(slug)
	if err != nil || !sp.Enabled {
		return publicStatus{}, store.ErrNotFound
	}
	p, err := s.st.GetProjectByID(sp.ProjectID)
	if err != nil {
		return publicStatus{}, err
	}
	v := publicStatus{Title: sp.Title, Overall: "operational", Updated: time.Now().UTC().Format(time.RFC3339)}
	if v.Title == "" {
		v.Title = p.Name
	}
	env, err := s.st.GetEnvironment(p.ID, p.DefaultEnv)
	if err != nil {
		return publicStatus{}, err
	}
	for _, name := range sp.Apps {
		a, err := s.st.GetAppInEnv(env.ID, name)
		if err != nil {
			continue
		}
		c, _ := s.st.GetUptimeCheck(a.ID)
		stats, _ := s.st.GetUptimeStats(a.ID, time.Now())
		days, _ := s.st.UptimeDays(a.ID, 90, time.Now())
		pa := publicApp{Name: a.Name, State: "unknown", Pct90d: stats.Pct90d, Days: days}
		switch {
		case c.State == "down":
			pa.State = "down"
		case c.State == "up" && c.FailStreak > 0:
			pa.State = "degraded"
		case c.State == "up":
			pa.State = "operational"
		}
		v.Apps = append(v.Apps, pa)
		switch {
		case pa.State == "down":
			v.Overall = "down"
		case pa.State == "degraded" && v.Overall == "operational":
			v.Overall = "degraded"
		}
	}
	incidents, _ := s.st.ListIncidents(p.ID, 10)
	published := map[string]bool{}
	for _, n := range sp.Apps {
		published[n] = true
	}
	cutoff := time.Now().UTC().AddDate(0, 0, -14).Format("2006-01-02")
	for _, in := range incidents {
		// Only incidents about published apps (or project-wide ones), and
		// resolved ones from the last two weeks.
		if in.App != "" && !published[in.App] {
			continue
		}
		if in.Status == "resolved" && in.ResolvedAt < cutoff {
			continue
		}
		v.Incidents = append(v.Incidents, publicIncident{Title: in.Title, Status: in.Status, App: in.App,
			OpenedAt: in.OpenedAt, ResolvedAt: in.ResolvedAt, Updates: in.Updates})
		if in.Status == "open" && v.Overall == "operational" {
			v.Overall = "degraded"
		}
	}
	s.statusCache.mu.Lock()
	if s.statusCache.m == nil {
		s.statusCache.m = map[string]statusCacheEntry{}
	}
	s.statusCache.m[slug] = statusCacheEntry{at: time.Now(), view: v}
	s.statusCache.mu.Unlock()
	return v, nil
}

// invalidateStatus drops cached views (after a config change).
func (s *server) invalidateStatus() {
	s.statusCache.mu.Lock()
	s.statusCache.m = nil
	s.statusCache.mu.Unlock()
}

// statusLimited applies the public status page rate limit.
func (s *server) statusLimited(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ip, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			ip = r.RemoteAddr
		}
		if !s.statusLimiter.allow(ip) {
			http.Error(w, "too many requests", http.StatusTooManyRequests)
			return
		}
		next(w, r)
	}
}

// handlePublicStatus serves /status/{slug}, /status/{slug}.json and
// /status/{slug}/badge.svg (the last two via handlePublicStatusAsset).
func (s *server) handlePublicStatus(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	if rest, ok := strings.CutSuffix(slug, ".json"); ok {
		s.servePublicStatusJSON(w, rest)
		return
	}
	v, err := s.publicStatusView(slug)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "public, max-age=30")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.ExecuteTemplate(w, "status_public.html", map[string]any{"S": v, "Slug": slug}); err != nil {
		http.Error(w, "render error", http.StatusInternalServerError)
	}
}

func (s *server) servePublicStatusJSON(w http.ResponseWriter, slug string) {
	v, err := s.publicStatusView(slug)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "no such status page")
		return
	}
	w.Header().Set("Cache-Control", "public, max-age=30")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	writeJSON(w, http.StatusOK, v)
}

// handlePublicStatusBadge serves a shields-style SVG badge.
func (s *server) handlePublicStatusBadge(w http.ResponseWriter, r *http.Request) {
	v, err := s.publicStatusView(r.PathValue("slug"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	label, color := "operational", "#1fa55c"
	switch v.Overall {
	case "degraded":
		label, color = "degraded", "#b87a00"
	case "down":
		label, color = "down", "#d93336"
	}
	w.Header().Set("Content-Type", "image/svg+xml")
	w.Header().Set("Cache-Control", "public, max-age=60")
	lw, rw := 46, 8+7*len(label)
	fmt.Fprintf(w, `<svg xmlns="http://www.w3.org/2000/svg" width="%d" height="20" role="img" aria-label="status: %s">`+
		`<rect width="%d" height="20" fill="#26262b"/><rect x="%d" width="%d" height="20" fill="%s"/>`+
		`<g fill="#fff" font-family="IBM Plex Mono,monospace" font-size="11"><text x="6" y="14">status</text><text x="%d" y="14">%s</text></g></svg>`,
		lw+rw, html.EscapeString(label), lw, lw, rw, color, lw+4, html.EscapeString(label))
}

// ---- management API ----

func (s *server) handleGetStatusPage(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.requireProject(w, u, r.PathValue("project"))
	if !ok {
		return
	}
	sp, err := s.st.GetStatusPage(p.ID)
	if errors.Is(err, store.ErrNotFound) {
		writeJSON(w, http.StatusOK, map[string]any{"enabled": false})
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"enabled": sp.Enabled, "slug": sp.Slug, "title": sp.Title, "apps": sp.Apps, "path": "/status/" + sp.Slug})
}

func (s *server) handlePutStatusPage(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.requireProjectWrite(w, u, r.PathValue("project"))
	if !ok {
		return
	}
	var req struct {
		Slug    string   `json:"slug"`
		Title   string   `json:"title"`
		Apps    []string `json:"apps"`
		Enabled *bool    `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	sp, err := s.setStatusPage(p, req.Slug, req.Title, req.Apps, enabled)
	if err != nil {
		code := http.StatusBadRequest
		if errors.Is(err, store.ErrSlugTaken) {
			code = http.StatusConflict
		}
		writeError(w, code, "bad_request", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"enabled": sp.Enabled, "slug": sp.Slug, "title": sp.Title, "apps": sp.Apps, "path": "/status/" + sp.Slug})
}

// setStatusPage validates the published apps (web apps of the project's
// default environment) and stores the page.
func (s *server) setStatusPage(p store.Project, slug, title string, apps []string, enabled bool) (store.StatusPage, error) {
	env, err := s.st.GetEnvironment(p.ID, p.DefaultEnv)
	if err != nil {
		return store.StatusPage{}, err
	}
	var clean []string
	for _, name := range apps {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		a, err := s.st.GetAppInEnv(env.ID, name)
		if err != nil {
			return store.StatusPage{}, fmt.Errorf("no app %q in %s/%s", name, p.Name, env.Name)
		}
		if a.Kind != "" && a.Kind != "web" {
			return store.StatusPage{}, fmt.Errorf("%s is a %s app; status pages show web apps", name, a.Kind)
		}
		clean = append(clean, name)
	}
	if slug == "" {
		slug = p.Name
	}
	sp := store.StatusPage{ProjectID: p.ID, Slug: slug, Title: strings.TrimSpace(title), Apps: clean, Enabled: enabled}
	if err := s.st.SetStatusPage(sp); err != nil {
		return store.StatusPage{}, err
	}
	s.invalidateStatus()
	return sp, nil
}

func (s *server) handleDeleteStatusPage(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.requireProjectWrite(w, u, r.PathValue("project"))
	if !ok {
		return
	}
	if err := s.st.DeleteStatusPage(p.ID); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	s.invalidateStatus()
	writeJSON(w, http.StatusOK, map[string]any{"enabled": false})
}
