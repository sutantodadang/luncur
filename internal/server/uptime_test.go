package server

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// The prober opens an incident after 3 failures (app_down) and resolves it
// after 2 passes (app_recovered); every probe lands in the samples.
func TestUptimeStateMachineOpensAndResolvesIncidents(t *testing.T) {
	s := aiTestServer(t)
	p, _ := s.st.CreateProject("shop")
	p, env := seedDefaultEnv(t, s.st, p)
	a, _ := s.st.CreateAppInEnv(env.ID, "web", 8080, "web", "")
	s.st.CreateDeployment(a.ID, "live", "nginx:1", 0)

	var mu sync.Mutex
	ok := true
	var urls []string
	s.uptimeProbeFn = func(url string) uptimeResult {
		mu.Lock()
		defer mu.Unlock()
		urls = append(urls, url)
		if ok {
			return uptimeResult{OK: true, Code: 200, LatencyMS: 12}
		}
		return uptimeResult{Err: "connection refused"}
	}
	round := func(n int) {
		for i := 0; i < n; i++ {
			s.uptimeRound(t.Context())
		}
	}
	round(1)
	if c, _ := s.st.GetUptimeCheck(a.ID); c.State != "up" {
		t.Fatalf("state = %q, want up", c.State)
	}
	if !strings.HasPrefix(urls[0], "http://web."+env.Namespace+".svc.cluster.local/") {
		t.Fatalf("probe url = %q, want the in-cluster Service", urls[0])
	}
	mu.Lock()
	ok = false
	mu.Unlock()
	round(2)
	if c, _ := s.st.GetUptimeCheck(a.ID); c.State != "up" || c.FailStreak != 2 {
		t.Fatalf("after 2 failures = %+v, want still up", c)
	}
	round(1)
	if c, _ := s.st.GetUptimeCheck(a.ID); c.State != "down" {
		t.Fatalf("after 3 failures = %q, want down", c.State)
	}
	in, err := s.st.OpenAutoIncident(a.ID)
	if err != nil || in.Title != "web is down" {
		t.Fatalf("auto incident = %+v %v", in, err)
	}
	mu.Lock()
	ok = true
	mu.Unlock()
	round(2)
	if c, _ := s.st.GetUptimeCheck(a.ID); c.State != "up" {
		t.Fatalf("after recovery = %q", c.State)
	}
	if got, _ := s.st.GetIncident(in.ID); got.Status != "resolved" || len(got.Updates) != 2 {
		t.Fatalf("incident after recovery = %+v", got)
	}
	stats, _ := s.st.GetUptimeStats(a.ID, time.Now())
	if stats.Pct24h != 50 {
		t.Fatalf("24h uptime = %v, want 50 (3 of 6 failed)", stats.Pct24h)
	}

	// Internal apps are off by default but can opt in.
	in2, _ := s.st.CreateAppInEnv(env.ID, "api", 8080, "web", "")
	s.st.SetInternal(in2.ID, true)
	in2, _ = s.st.GetAppByID(in2.ID)
	c, _ := s.st.GetUptimeCheck(in2.ID)
	if s.uptimeEnabled(in2, c) {
		t.Fatal("internal app checked by default")
	}
	on := true
	if err := s.setUptime(in2, &on, nil, nil); err != nil {
		t.Fatal(err)
	}
	c, _ = s.st.GetUptimeCheck(in2.ID)
	if !s.uptimeEnabled(in2, c) {
		t.Fatal("opt-in ignored")
	}
}

// The public page shows only what was published, never internals; JSON,
// badge and rate limiting work; unpublished slugs 404.
func TestPublicStatusPage(t *testing.T) {
	st := newTestStore(t)
	srv := newHTTPTest(t, Deps{Store: st, ExternalIP: "1.2.3.4"})
	admin := seedUserToken(t, st, "root@b.co", "admin")
	doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"shop"}`).Body.Close()
	doAuthed(t, "POST", srv.URL+"/v1/projects/shop/apps", admin, `{"name":"web","port":8080}`).Body.Close()
	doAuthed(t, "POST", srv.URL+"/v1/projects/shop/apps", admin, `{"name":"secret-admin","port":8080}`).Body.Close()

	resp := doAuthed(t, "PUT", srv.URL+"/v1/projects/shop/status-page", admin, `{"slug":"acme","title":"Acme","apps":["web"]}`)
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(b), `"path":"/status/acme"`) {
		t.Fatalf("enable = %d %s", resp.StatusCode, b)
	}
	resp = doAuthed(t, "POST", srv.URL+"/v1/projects/shop/incidents", admin, `{"title":"Planned maintenance","body":"DB upgrade at 02:00 UTC"}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("open incident = %d", resp.StatusCode)
	}

	get := func(path string) (int, string, http.Header) {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b), resp.Header
	}
	code, page, _ := get("/status/acme")
	if code != http.StatusOK || !strings.Contains(page, "Acme") || !strings.Contains(page, "Planned maintenance") || !strings.Contains(page, "sp-bar") {
		t.Fatalf("page = %d\n%s", code, page)
	}
	for _, leak := range []string{"secret-admin", "luncur-shop", "svc.cluster.local", "nginx"} {
		if strings.Contains(page, leak) {
			t.Fatalf("public page leaks %q", leak)
		}
	}
	code, js, hdr := get("/status/acme.json")
	var v publicStatus
	if code != http.StatusOK || json.Unmarshal([]byte(js), &v) != nil || v.Overall != "degraded" || len(v.Apps) != 1 || len(v.Apps[0].Days) != 90 {
		t.Fatalf("json = %d %s", code, js)
	}
	if hdr.Get("Access-Control-Allow-Origin") != "*" {
		t.Fatal("json not embeddable (CORS)")
	}
	if code, svg, hdr := get("/status/acme/badge.svg"); code != http.StatusOK || !strings.Contains(svg, "degraded") || hdr.Get("Content-Type") != "image/svg+xml" {
		t.Fatalf("badge = %d %s", code, svg)
	}
	if code, _, _ := get("/status/nope"); code != http.StatusNotFound {
		t.Fatalf("unknown slug = %d", code)
	}
	// Taken slug.
	doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"other"}`).Body.Close()
	if resp := doAuthed(t, "PUT", srv.URL+"/v1/projects/other/status-page", admin, `{"slug":"acme"}`); resp.StatusCode != http.StatusConflict {
		t.Fatalf("taken slug = %d", resp.StatusCode)
	}
	// Rate limited.
	limited := false
	for i := 0; i < statusLimit+5; i++ {
		if code, _, _ := get("/status/acme.json"); code == http.StatusTooManyRequests {
			limited = true
			break
		}
	}
	if !limited {
		t.Fatal("public status page is not rate limited")
	}
}

func TestUIUptimeStatusPageAndIncidents(t *testing.T) {
	u := newUIAI(t, nil)
	page := u.get(t, "/ui/projects/shop/apps/web?tab=observe")
	if !strings.Contains(page, `id="uptime"`) || !strings.Contains(page, "luncur uptime status web --project shop") {
		t.Fatalf("observe tab has no uptime card:\n%s", page)
	}
	resp, _ := u.htmx(t, "/ui/projects/shop/status-page", map[string][]string{"slug": {"shop-status"}, "apps": {"web"}})
	if resp.StatusCode >= 400 {
		t.Fatalf("publish = %d", resp.StatusCode)
	}
	resp, _ = u.htmx(t, "/ui/projects/shop/incidents", map[string][]string{"action": {"open"}, "title": {"DB maintenance"}})
	if resp.StatusCode >= 400 {
		t.Fatalf("open incident = %d", resp.StatusCode)
	}
	proj := u.get(t, "/ui/projects/shop")
	for _, want := range []string{`id="status-page"`, "/status/shop-status", "DB maintenance"} {
		if !strings.Contains(proj, want) {
			t.Fatalf("project page missing %q", want)
		}
	}
}
