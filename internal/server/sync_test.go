package server

import (
	"strings"
	"testing"
)

// TestAppURL covers appURL's preference order: an issued/external custom
// domain wins as https, a pending domain still wins but over http, a
// wildcard-only domain is skipped in favor of the sslip.io fallback, and no
// domains at all falls back to the sslip.io host.
func TestAppURL(t *testing.T) {
	st := newTestStore(t)
	s := newServer(Deps{Store: st, ExternalIP: "1.2.3.4"})

	p, err := st.CreateProject("proj")
	if err != nil {
		t.Fatal(err)
	}

	newApp := func(t *testing.T, name string) (id int64, app string) {
		t.Helper()
		a, err := st.CreateApp(p.ID, name, 8080, "web", "")
		if err != nil {
			t.Fatal(err)
		}
		return a.ID, a.Name
	}

	t.Run("no domains falls back to sslip host", func(t *testing.T) {
		id, name := newApp(t, "noneapp")
		a, err := st.GetApp(p.ID, name)
		if err != nil {
			t.Fatal(err)
		}
		want := "http://noneapp.1-2-3-4.sslip.io"
		if got := s.appURL(a); got != want {
			t.Fatalf("appURL = %q, want %q", got, want)
		}
		_ = id
	})

	t.Run("issued domain wins as https", func(t *testing.T) {
		_, name := newApp(t, "issuedapp")
		a, err := st.GetApp(p.ID, name)
		if err != nil {
			t.Fatal(err)
		}
		d, err := st.AddDomain(a.ID, "issued.example.com")
		if err != nil {
			t.Fatal(err)
		}
		if err := st.SetDomainCert(d.ID, "issued", "", "2099-01-01T00:00:00Z"); err != nil {
			t.Fatal(err)
		}
		want := "https://issued.example.com"
		if got := s.appURL(a); got != want {
			t.Fatalf("appURL = %q, want %q", got, want)
		}
	})

	t.Run("pending domain wins as http", func(t *testing.T) {
		_, name := newApp(t, "pendingapp")
		a, err := st.GetApp(p.ID, name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.AddDomain(a.ID, "pending.example.com"); err != nil {
			t.Fatal(err)
		}
		want := "http://pending.example.com"
		if got := s.appURL(a); got != want {
			t.Fatalf("appURL = %q, want %q", got, want)
		}
	})

	t.Run("wildcard-only domain falls back to sslip host", func(t *testing.T) {
		_, name := newApp(t, "wildcardapp")
		a, err := st.GetApp(p.ID, name)
		if err != nil {
			t.Fatal(err)
		}
		d, err := st.AddDomain(a.ID, "*.example.com")
		if err != nil {
			t.Fatal(err)
		}
		if err := st.SetDomainCert(d.ID, "pending", "", ""); err != nil {
			t.Fatal(err)
		}
		want := "http://wildcardapp.1-2-3-4.sslip.io"
		if got := s.appURL(a); got != want {
			t.Fatalf("appURL = %q, want %q", got, want)
		}
	})
}

// TestRenderAppIngressHosts covers which hosts the rendered Ingress carries:
// a routable (non-wildcard) custom domain replaces the assigned sslip.io
// host entirely, no domains keeps the sslip host, and a wildcard-only app
// keeps sslip as the primary host alongside the wildcard rule (appURL still
// points at sslip in that case).
func TestRenderAppIngressHosts(t *testing.T) {
	st := newTestStore(t)
	s := newServer(Deps{Store: st, ExternalIP: "1.2.3.4"})

	p, err := st.CreateProject("proj")
	if err != nil {
		t.Fatal(err)
	}
	p, env := seedDefaultEnv(t, st, p)

	ingressJSON := func(t *testing.T, name string) string {
		t.Helper()
		a, err := st.GetApp(p.ID, name)
		if err != nil {
			t.Fatal(err)
		}
		rendered, err := s.renderApp(p, env, a, "nginx:1", true)
		if err != nil {
			t.Fatal(err)
		}
		for _, o := range rendered.Objects {
			if o.Kind == "Ingress" {
				return string(o.JSON)
			}
		}
		t.Fatal("no Ingress object rendered")
		return ""
	}

	t.Run("no domains keeps sslip host", func(t *testing.T) {
		if _, err := st.CreateApp(p.ID, "plain", 8080, "web", ""); err != nil {
			t.Fatal(err)
		}
		ing := ingressJSON(t, "plain")
		if !strings.Contains(ing, `"host":"plain.1-2-3-4.sslip.io"`) {
			t.Fatalf("want sslip host rule:\n%s", ing)
		}
	})

	t.Run("custom domain replaces sslip host", func(t *testing.T) {
		a, err := st.CreateApp(p.ID, "custom", 8080, "web", "")
		if err != nil {
			t.Fatal(err)
		}
		d, err := st.AddDomain(a.ID, "www.example.com")
		if err != nil {
			t.Fatal(err)
		}
		if err := st.SetDomainCert(d.ID, "issued", "", "2099-01-01T00:00:00Z"); err != nil {
			t.Fatal(err)
		}
		ing := ingressJSON(t, "custom")
		if !strings.Contains(ing, `"host":"www.example.com"`) {
			t.Fatalf("want custom domain rule:\n%s", ing)
		}
		if strings.Contains(ing, "sslip.io") {
			t.Fatalf("sslip host must be replaced by the custom domain:\n%s", ing)
		}
	})

	t.Run("wildcard-only keeps sslip host plus wildcard rule", func(t *testing.T) {
		a, err := st.CreateApp(p.ID, "wild", 8080, "web", "")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.AddDomain(a.ID, "*.example.com"); err != nil {
			t.Fatal(err)
		}
		ing := ingressJSON(t, "wild")
		if !strings.Contains(ing, `"host":"wild.1-2-3-4.sslip.io"`) {
			t.Fatalf("want sslip host rule kept for wildcard-only app:\n%s", ing)
		}
		if !strings.Contains(ing, `"host":"*.example.com"`) {
			t.Fatalf("want wildcard rule:\n%s", ing)
		}
	})
}

// TestHostForEnv covers hostForEnv's default-vs-non-default branching: the
// project's default environment gets the plain hostFor host, any other
// environment gets an "-<env>" suffix on the app name so the same app name
// can coexist across environments.
func TestHostForEnv(t *testing.T) {
	ip := "1.2.3.4"
	if got := hostForEnv("api", "production", "production", ip); got != "api.1-2-3-4.sslip.io" {
		t.Fatalf("default env host = %q", got)
	}
	if got := hostForEnv("api", "develop", "production", ip); got != "api-develop.1-2-3-4.sslip.io" {
		t.Fatalf("non-default host = %q", got)
	}
}
