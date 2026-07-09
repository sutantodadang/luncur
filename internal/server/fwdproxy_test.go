package server

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/sutantodadang/luncur/internal/store"
)

func TestForwardHostParsing(t *testing.T) {
	st := newTestStore(t)
	srv := newServer(Deps{Store: st, ExternalIP: "1.2.3.4"})

	admin := seedUserToken(t, st, "root@b.co", "admin")
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()
	doAuthed(t, "POST", ts.URL+"/v1/projects", admin, `{"name":"waku"}`).Body.Close()
	doAuthed(t, "POST", ts.URL+"/v1/projects/waku/apps", admin, `{"name":"waku-simpaniz","port":8080}`).Body.Close()
	wantAppID := appID(t, st, "waku", "waku-simpaniz")

	cases := []struct {
		host string
		ok   bool
	}{
		{"waku-simpaniz--waku.1.2.3.4.sslip.io", true},
		{"waku-simpaniz--waku.1.2.3.4.sslip.io:443", true},
		{"nope--waku.1.2.3.4.sslip.io", false},
		{"waku-simpaniz--nope.1.2.3.4.sslip.io", false},
		{"panel.1.2.3.4.sslip.io", false},
		{"x.y.waku-simpaniz--waku.1.2.3.4.sslip.io", false},
	}
	for _, c := range cases {
		p, a, ok := srv.forwardAppFromHost(c.host)
		if ok != c.ok {
			t.Fatalf("%q: ok=%v want %v (p=%+v a=%+v)", c.host, ok, c.ok, p, a)
		}
		if ok && (p.Name != "waku" || a.ID != wantAppID) {
			t.Fatalf("%q: resolved wrong app: p=%+v a=%+v", c.host, p, a)
		}
	}

	// With a custom panel_domain, forward hosts hang off it instead of the
	// sslip.io fallback.
	if err := st.SetSetting("panel_domain", "example.com"); err != nil {
		t.Fatal(err)
	}
	p, a, ok := srv.forwardAppFromHost("waku-simpaniz--waku.example.com")
	if !ok || p.Name != "waku" || a.ID != wantAppID {
		t.Fatalf("panel_domain host: ok=%v p=%+v a=%+v", ok, p, a)
	}
	// The old sslip.io host no longer resolves once panel_domain is set.
	if _, _, ok := srv.forwardAppFromHost("waku-simpaniz--waku.1.2.3.4.sslip.io"); ok {
		t.Fatal("stale sslip.io host should not resolve once panel_domain is set")
	}
}

func TestForwardOpenRedirect(t *testing.T) {
	st := newTestStore(t)
	srv := newServer(Deps{Store: st, ExternalIP: "1.2.3.4"}) // no kube: apply is skipped
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	admin := seedUserToken(t, st, "root@b.co", "admin")
	doAuthed(t, "POST", ts.URL+"/v1/projects", admin, `{"name":"waku"}`).Body.Close()
	doAuthed(t, "POST", ts.URL+"/v1/projects/waku/apps", admin, `{"name":"waku-simpaniz","port":8080}`).Body.Close()
	wantAppID := appID(t, st, "waku", "waku-simpaniz")

	u, err := st.GetUserByEmail("root@b.co")
	if err != nil {
		t.Fatal(err)
	}
	ck := uiSessionCookie(t, st, u.ID)

	client := noRedirectClient()
	req, err := http.NewRequest("GET", ts.URL+"/ui/projects/waku/apps/waku-simpaniz/open", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.AddCookie(ck)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("want 303, got %d", resp.StatusCode)
	}
	loc := resp.Header.Get("Location")
	wantPrefix := "http://waku-simpaniz--waku.1.2.3.4.sslip.io" + fwdAuthPath + "?t="
	if !strings.HasPrefix(loc, wantPrefix) {
		t.Fatalf("redirect location %q, want prefix %q", loc, wantPrefix)
	}
	tokEnc := strings.TrimPrefix(loc, wantPrefix)
	tok, err := url.QueryUnescape(tokEnc)
	if err != nil {
		t.Fatal(err)
	}
	appIDGot, ok := verifyFwdToken(srv.fwdKey, tok, srv.nowFn())
	if !ok || appIDGot != wantAppID {
		t.Fatalf("token invalid or wrong app: id=%d ok=%v want %d", appIDGot, ok, wantAppID)
	}
}

func TestForwardOpenRejectsDoubleDash(t *testing.T) {
	st := newTestStore(t)
	srv := newServer(Deps{Store: st, ExternalIP: "1.2.3.4"})
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	admin := seedUserToken(t, st, "root@b.co", "admin")
	doAuthed(t, "POST", ts.URL+"/v1/projects", admin, `{"name":"waku"}`).Body.Close()
	// App name containing "--" is a valid DNS-1123 label (nameRe allows
	// consecutive hyphens), but it would make the {app}--{project} host
	// label ambiguous to split back apart, so /open must refuse it.
	doAuthed(t, "POST", ts.URL+"/v1/projects/waku/apps", admin, `{"name":"ab--cd","port":8080}`).Body.Close()

	u, err := st.GetUserByEmail("root@b.co")
	if err != nil {
		t.Fatal(err)
	}
	ck := uiSessionCookie(t, st, u.ID)

	client := noRedirectClient()
	req, err := http.NewRequest("GET", ts.URL+"/ui/projects/waku/apps/ab--cd/open", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.AddCookie(ck)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", resp.StatusCode)
	}
}

func TestForwardAuthSetsCookieAndProxies(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("hello from app"))
	}))
	defer backend.Close()
	backendURL, err := url.Parse(backend.URL)
	if err != nil {
		t.Fatal(err)
	}

	st := newTestStore(t)
	srv := newServer(Deps{Store: st, ExternalIP: "1.2.3.4"})
	srv.fwdProxyTargetFn = func(_ store.Project, _ store.App) *url.URL { return backendURL }
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	admin := seedUserToken(t, st, "root@b.co", "admin")
	doAuthed(t, "POST", ts.URL+"/v1/projects", admin, `{"name":"waku"}`).Body.Close()
	doAuthed(t, "POST", ts.URL+"/v1/projects/waku/apps", admin, `{"name":"waku-simpaniz","port":8080}`).Body.Close()
	wantAppID := appID(t, st, "waku", "waku-simpaniz")

	forwardHost := "waku-simpaniz--waku.1.2.3.4.sslip.io"
	client := noRedirectClient()

	// (5) bad token -> 403
	{
		req, _ := http.NewRequest("GET", ts.URL+fwdAuthPath+"?t=garbage", nil)
		req.Host = forwardHost
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("bad token: want 403, got %d", resp.StatusCode)
		}
	}

	// (3) no cookie -> 401
	{
		req, _ := http.NewRequest("GET", ts.URL+"/", nil)
		req.Host = forwardHost
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("no cookie: want 401, got %d", resp.StatusCode)
		}
	}

	// (4) expired/wrong-app cookie -> 401
	{
		badCk := mintFwdToken(srv.fwdKey, wantAppID+999, srv.nowFn().Add(fwdSessionTTL))
		req, _ := http.NewRequest("GET", ts.URL+"/", nil)
		req.Host = forwardHost
		req.AddCookie(&http.Cookie{Name: fwdCookie, Value: badCk})
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("wrong-app cookie: want 401, got %d", resp.StatusCode)
		}

		expiredCk := mintFwdToken(srv.fwdKey, wantAppID, srv.nowFn().Add(-time.Second))
		req2, _ := http.NewRequest("GET", ts.URL+"/", nil)
		req2.Host = forwardHost
		req2.AddCookie(&http.Cookie{Name: fwdCookie, Value: expiredCk})
		resp2, err := client.Do(req2)
		if err != nil {
			t.Fatal(err)
		}
		resp2.Body.Close()
		if resp2.StatusCode != http.StatusUnauthorized {
			t.Fatalf("expired cookie: want 401, got %d", resp2.StatusCode)
		}
	}

	// (1) valid handoff token -> 303 to "/", Set-Cookie luncur_fwd.
	tok := mintFwdToken(srv.fwdKey, wantAppID, srv.nowFn().Add(fwdHandoffTTL))
	var sessionCk *http.Cookie
	{
		req, _ := http.NewRequest("GET", ts.URL+fwdAuthPath+"?t="+url.QueryEscape(tok), nil)
		req.Host = forwardHost
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusSeeOther {
			t.Fatalf("auth handoff: want 303, got %d", resp.StatusCode)
		}
		if loc := resp.Header.Get("Location"); loc != "/" {
			t.Fatalf("auth handoff: want redirect to /, got %q", loc)
		}
		for _, c := range resp.Cookies() {
			if c.Name == fwdCookie {
				sessionCk = c
			}
		}
		if sessionCk == nil {
			t.Fatal("no luncur_fwd cookie set")
		}
		if !sessionCk.HttpOnly {
			t.Fatal("luncur_fwd cookie must be HttpOnly")
		}
		if sessionCk.SameSite != http.SameSiteLaxMode {
			t.Fatalf("luncur_fwd cookie SameSite: got %v want Lax", sessionCk.SameSite)
		}
	}

	// (2) with the cookie, request proxies through to the backend.
	{
		req, _ := http.NewRequest("GET", ts.URL+"/", nil)
		req.Host = forwardHost
		req.AddCookie(sessionCk)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("proxied request: want 200, got %d", resp.StatusCode)
		}
		body := make([]byte, 64)
		n, _ := resp.Body.Read(body)
		if got := string(body[:n]); got != "hello from app" {
			t.Fatalf("proxied body: got %q", got)
		}
	}
}

func TestDestroyAppDeletesForwardIngress(t *testing.T) {
	srv, st, actions := kubeServer(t)
	admin := seedUserToken(t, st, "root@b.co", "admin")
	doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"waku"}`).Body.Close()
	doAuthed(t, "POST", srv.URL+"/v1/projects/waku/apps", admin, `{"name":"waku-simpaniz","port":8080}`).Body.Close()

	resp := doAuthed(t, "DELETE", srv.URL+"/v1/projects/waku/apps/waku-simpaniz", admin, "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete app: want 204, got %d", resp.StatusCode)
	}

	// DeleteAppObjects already deletes the app's own Ingress; destroyApp
	// now also deletes the forward Ingress in the system namespace, so the
	// fake dynamic client should record two "delete ingresses" actions.
	count := strings.Count(strings.Join(*actions, ","), "delete ingresses")
	if count < 2 {
		t.Fatalf("want >=2 ingress deletes (app + forward), got %d: %v", count, *actions)
	}
}
