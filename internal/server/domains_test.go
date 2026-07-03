package server

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// TestDomainCRUDAndRender exercises the domains API end to end against a
// live app: add (with DNS warning), list, raw-manifest pickup, delete, and
// a rejected malformed hostname.
func TestDomainCRUDAndRender(t *testing.T) {
	srv, st, _ := kubeServer(t)
	admin := seedUserToken(t, st, "root@b.co", "admin")
	doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"proj"}`).Body.Close()
	doAuthed(t, "POST", srv.URL+"/v1/projects/proj/apps", admin, `{"name":"web","port":8080}`).Body.Close()

	resp := doAuthed(t, "POST", srv.URL+"/v1/projects/proj/apps/web/deploy", admin, `{"image":"nginx:1"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("deploy: want 200, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// 1. Add domain: 201, cert_status "none", non-empty dns_warning (the
	// test hostname does not resolve to the server's ExternalIP).
	resp = doAuthed(t, "POST", srv.URL+"/v1/projects/proj/apps/web/domains", admin, `{"hostname":"www.example.com"}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("add domain: want 201, got %d", resp.StatusCode)
	}
	var added map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&added); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if added["cert_status"] != "none" {
		t.Fatalf("cert_status = %v, want none", added["cert_status"])
	}
	warning, _ := added["dns_warning"].(string)
	if warning == "" {
		t.Fatal("expected non-empty dns_warning for a hostname that doesn't resolve to ExternalIP")
	}

	// 2. List domains: one entry.
	resp = doAuthed(t, "GET", srv.URL+"/v1/projects/proj/apps/web/domains", admin, "")
	var list []map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if len(list) != 1 || list[0]["hostname"] != "www.example.com" {
		t.Fatalf("list = %+v, want one www.example.com entry", list)
	}

	// 3. Raw manifest picks the domain up as an extra Ingress host.
	resp = doAuthed(t, "GET", srv.URL+"/v1/projects/proj/apps/web/raw", admin, "")
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if !strings.Contains(string(body), "www.example.com") {
		t.Fatalf("raw manifest missing custom domain:\n%s", body)
	}

	// 4. Delete domain: 204, then list is empty.
	resp = doAuthed(t, "DELETE", srv.URL+"/v1/projects/proj/apps/web/domains/www.example.com", admin, "")
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete domain: want 204, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = doAuthed(t, "GET", srv.URL+"/v1/projects/proj/apps/web/domains", admin, "")
	list = nil
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if len(list) != 0 {
		t.Fatalf("list after delete = %+v, want empty", list)
	}

	// 5. Malformed hostname rejected.
	resp = doAuthed(t, "POST", srv.URL+"/v1/projects/proj/apps/web/domains", admin, `{"hostname":"nodot"}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad hostname: want 400, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestCertSecretName(t *testing.T) {
	got := certSecretName("web", "www.example.com")
	if !strings.HasPrefix(got, "tls-web-") || len(got) != len("tls-web-")+8 {
		t.Fatalf("secret name = %q", got)
	}
	if got != certSecretName("web", "www.example.com") {
		t.Fatal("not deterministic")
	}
}

// TestRenderProviderAnnotations calls s.renderApp directly, one provider
// setting at a time, and inspects the rendered Ingress JSON for the
// annotations/TLS block each provider is supposed to produce.
func TestRenderProviderAnnotations(t *testing.T) {
	st := newTestStore(t)
	s := newServer(Deps{Store: st, ExternalIP: "1.2.3.4"})

	p, err := st.CreateProject("proj")
	if err != nil {
		t.Fatal(err)
	}
	a, err := st.CreateApp(p.ID, "web", 8080)
	if err != nil {
		t.Fatal(err)
	}
	d, err := st.AddDomain(a.ID, "www.example.com")
	if err != nil {
		t.Fatal(err)
	}
	wantSecret := certSecretName("web", "www.example.com")

	ingressJSON := func(t *testing.T) string {
		t.Helper()
		rendered, err := s.renderApp(p, a, "nginx:1", true)
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

	// builtin + status none: no tls block, no annotations.
	if err := st.SetSetting("cert_provider", "builtin"); err != nil {
		t.Fatal(err)
	}
	ing := ingressJSON(t)
	if strings.Contains(ing, `"tls"`) {
		t.Fatalf("builtin+none: unexpected tls block:\n%s", ing)
	}
	if strings.Contains(ing, `"annotations"`) {
		t.Fatalf("builtin+none: unexpected annotations:\n%s", ing)
	}

	// builtin + status issued: tls block w/ certSecretName, no annotations.
	if err := st.SetDomainCert(d.ID, "issued", "", "2027-01-01T00:00:00Z"); err != nil {
		t.Fatal(err)
	}
	ing = ingressJSON(t)
	if !strings.Contains(ing, `"secretName":"`+wantSecret+`"`) {
		t.Fatalf("builtin+issued: missing tls secret %q:\n%s", wantSecret, ing)
	}
	if strings.Contains(ing, `"annotations"`) {
		t.Fatalf("builtin+issued: unexpected annotations:\n%s", ing)
	}

	// traefik: certresolver annotation, no tls block.
	if err := st.SetSetting("cert_provider", "traefik"); err != nil {
		t.Fatal(err)
	}
	ing = ingressJSON(t)
	if !strings.Contains(ing, `"traefik.ingress.kubernetes.io/router.tls.certresolver":"le"`) {
		t.Fatalf("traefik: missing certresolver annotation:\n%s", ing)
	}
	if strings.Contains(ing, `"tls"`) {
		t.Fatalf("traefik: unexpected tls block:\n%s", ing)
	}

	// cert-manager: cluster-issuer annotation + tls block.
	if err := st.SetSetting("cert_provider", "cert-manager"); err != nil {
		t.Fatal(err)
	}
	ing = ingressJSON(t)
	if !strings.Contains(ing, `"cert-manager.io/cluster-issuer":"luncur-le"`) {
		t.Fatalf("cert-manager: missing cluster-issuer annotation:\n%s", ing)
	}
	if !strings.Contains(ing, `"secretName":"`+wantSecret+`"`) {
		t.Fatalf("cert-manager: missing tls secret:\n%s", ing)
	}
}
