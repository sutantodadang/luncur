# luncur Plan F — custom domains + pluggable TLS Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Apps serve real custom domains with TLS: `luncur domain add myapp www.example.com` gets a Let's Encrypt cert via the built-in ACME client (default), or delegates to Traefik/cert-manager when configured.

**Architecture:** Domain rows (with cert status) extend the render pipeline: extra Ingress host rules, provider-specific annotations, and TLS blocks referencing per-domain Secrets. A `CertProvider` setting (`builtin` | `traefik` | `cert-manager`, stored in a new key/value `settings` table) decides who issues certs. The builtin provider is a full ACME (RFC 8555) client in `internal/acme` driven by a manager in the server: HTTP-01 challenges are served by luncur itself and routed via a dedicated challenge Ingress in `luncur-system`; issued certs land as `kubernetes.io/tls` Secrets in the app namespace; a renewal sweep re-issues before expiry. Traefik and cert-manager providers are thin: annotations on the app Ingress plus one-time cluster objects (`HelmChartConfig` / `ClusterIssuer`).

**Tech Stack:** Go stdlib (`crypto/x509`, `crypto/ecdsa`, `net`), `golang.org/x/crypto/acme` (in the already-required x/crypto module), client-go dynamic (existing), modernc.org/sqlite, cobra.

## Global Constraints

- Single Go module, one binary from `cmd/luncur`. Allowed new import: `golang.org/x/crypto/acme` (x/crypto family, module already required). No cert-manager/Traefik client libraries — those providers are driven purely by annotations + unstructured manifests.
- Server-side apply everywhere, `fieldManager=luncur`. API error envelope `{"error":{"code":"...","message":"..."}}` via `writeError`.
- All commits conventional style; `go build ./... && go vet ./... && go test ./...` before every commit.
- Tests must not require a cluster, root, or the network: fake dynamic/typed clientsets, an in-test fake ACME directory (signature-free JWS decode), `httptest`.
- **Approved deviations from the Phase 2 spec (record in README):**
  - HTTP-01 challenge routing: the spec said the app's own Ingress routes `/.well-known/acme-challenge/` to the luncur Service — impossible across namespaces (Ingress backends are namespace-local). Instead luncur maintains ONE challenge Ingress `luncur-acme` in `luncur-system` listing every pending-challenge host; Traefik merges same-host rules across namespaces and the longer ACME path wins over the app's `/`.
  - `cert_provider` is fixed per install at `luncur serve` startup (`--cert-provider` flag, persisted to settings; `luncur config set cert_provider ...` takes effect on restart). Live switching would orphan issued state for no Phase-2 benefit.

---

### Task 1: store — domains CRUD + settings table

**Files:**
- Modify: `internal/store/schema.sql` (settings table)
- Modify: `internal/store/store.go` (migrate: domains ALTERs)
- Create: `internal/store/domains.go`
- Create: `internal/store/settings.go`
- Test: `internal/store/domains_test.go`, `internal/store/settings_test.go`

**Interfaces:**
- Consumes: existing `Store`, `ErrNotFound`, `openTest(t)` test helper, `migrate(db)` pattern in `store.go` (pragma_table_info count → ALTER).
- Produces:
  - `type Domain struct { ID, AppID int64; Hostname, CertStatus, CertError, CertExpiresAt string }` — `CertStatus` ∈ `none|pending|issued|failed|external`.
  - `Store.AddDomain(appID int64, hostname string) (Domain, error)` — lowercases, validates shape (`[a-z0-9.-]`, has a dot, no leading/trailing dot/dash), rejects duplicates with a friendly error.
  - `Store.ListDomains(appID int64) ([]Domain, error)` (ordered by id)
  - `Store.AllDomains() ([]Domain, error)` (renewal sweep)
  - `Store.DeleteDomain(appID int64, hostname string) error` — `ErrNotFound` when absent.
  - `Store.SetDomainCert(id int64, status, certErr, expiresAt string) error`
  - `Store.GetSetting(key string) (string, error)` — `ErrNotFound` when unset.
  - `Store.SetSetting(key, value string) error` (upsert)

- [ ] **Step 1: Write the failing tests.**

`internal/store/domains_test.go`:

```go
package store

import (
	"errors"
	"testing"
)

func TestDomainRoundTrip(t *testing.T) {
	s := openTest(t)
	p, err := s.CreateProject("proj")
	if err != nil {
		t.Fatal(err)
	}
	a, err := s.CreateApp(p.ID, "web", 8080)
	if err != nil {
		t.Fatal(err)
	}

	d, err := s.AddDomain(a.ID, "WWW.Example.com")
	if err != nil {
		t.Fatal(err)
	}
	if d.Hostname != "www.example.com" {
		t.Fatalf("hostname = %q, want lowercased", d.Hostname)
	}
	if d.CertStatus != "none" {
		t.Fatalf("cert status = %q, want none", d.CertStatus)
	}

	if _, err := s.AddDomain(a.ID, "www.example.com"); err == nil {
		t.Fatal("duplicate hostname accepted")
	}
	for _, bad := range []string{"", "nodot", "-x.example.com", "x..com", "ex ample.com"} {
		if _, err := s.AddDomain(a.ID, bad); err == nil {
			t.Fatalf("invalid hostname %q accepted", bad)
		}
	}

	if err := s.SetDomainCert(d.ID, "issued", "", "2027-01-01T00:00:00Z"); err != nil {
		t.Fatal(err)
	}
	list, err := s.ListDomains(a.ID)
	if err != nil || len(list) != 1 {
		t.Fatalf("list = %+v err=%v", list, err)
	}
	if list[0].CertStatus != "issued" || list[0].CertExpiresAt == "" {
		t.Fatalf("cert fields not persisted: %+v", list[0])
	}

	all, err := s.AllDomains()
	if err != nil || len(all) != 1 {
		t.Fatalf("all = %+v err=%v", all, err)
	}

	if err := s.DeleteDomain(a.ID, "www.example.com"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteDomain(a.ID, "www.example.com"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete: %v, want ErrNotFound", err)
	}
}
```

`internal/store/settings_test.go`:

```go
package store

import (
	"errors"
	"testing"
)

func TestSettings(t *testing.T) {
	s := openTest(t)
	if _, err := s.GetSetting("cert_provider"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unset key: %v, want ErrNotFound", err)
	}
	if err := s.SetSetting("cert_provider", "builtin"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSetting("cert_provider", "traefik"); err != nil {
		t.Fatal(err) // upsert
	}
	v, err := s.GetSetting("cert_provider")
	if err != nil || v != "traefik" {
		t.Fatalf("got %q err=%v, want traefik", v, err)
	}
}
```

- [ ] **Step 2: Run** `go test ./internal/store/ -run 'TestDomainRoundTrip|TestSettings' -v` — compile failure.

- [ ] **Step 3: Implement.**

`schema.sql` — append:

```sql
CREATE TABLE IF NOT EXISTS settings (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
);
```

`store.go` `migrate()` — extend with the domains columns (same pragma pattern the `expires_at` migration uses; read the existing function and follow it exactly):

```go
	for _, col := range []struct{ table, name, ddl string }{
		{"api_tokens", "expires_at", `ALTER TABLE api_tokens ADD COLUMN expires_at TEXT`}, // existing — restructure the current single check into this loop
		{"domains", "cert_status", `ALTER TABLE domains ADD COLUMN cert_status TEXT NOT NULL DEFAULT 'none'`},
		{"domains", "cert_error", `ALTER TABLE domains ADD COLUMN cert_error TEXT NOT NULL DEFAULT ''`},
		{"domains", "cert_expires_at", `ALTER TABLE domains ADD COLUMN cert_expires_at TEXT NOT NULL DEFAULT ''`},
	} {
		var n int
		if err := db.QueryRow(
			`SELECT count(*) FROM pragma_table_info(?) WHERE name = ?`, col.table, col.name,
		).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			if _, err := db.Exec(col.ddl); err != nil {
				return err
			}
		}
	}
	return nil
```

(If the existing `migrate` body doesn't restructure cleanly, keep its current check and append the loop for just the three domains columns — behavior over beauty.)

`internal/store/domains.go`:

```go
package store

import (
	"fmt"
	"regexp"
	"strings"
)

// Domain is a custom hostname attached to an app, with TLS cert state.
type Domain struct {
	ID            int64
	AppID         int64
	Hostname      string
	CertStatus    string // none|pending|issued|failed|external
	CertError     string
	CertExpiresAt string // RFC3339, empty until issued
}

var hostnameRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$`)

func (s *Store) AddDomain(appID int64, hostname string) (Domain, error) {
	h := strings.ToLower(strings.TrimSpace(hostname))
	if !hostnameRe.MatchString(h) {
		return Domain{}, fmt.Errorf("invalid hostname %q", hostname)
	}
	res, err := s.db.Exec(
		`INSERT INTO domains (app_id, hostname, cert_status) VALUES (?, ?, 'none')`, appID, h)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return Domain{}, fmt.Errorf("hostname %q is already registered", h)
		}
		return Domain{}, err
	}
	id, _ := res.LastInsertId()
	return Domain{ID: id, AppID: appID, Hostname: h, CertStatus: "none"}, nil
}

func (s *Store) scanDomains(query string, args ...any) ([]Domain, error) {
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Domain
	for rows.Next() {
		var d Domain
		if err := rows.Scan(&d.ID, &d.AppID, &d.Hostname, &d.CertStatus, &d.CertError, &d.CertExpiresAt); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

const domainCols = `id, app_id, hostname, cert_status, cert_error, cert_expires_at`

func (s *Store) ListDomains(appID int64) ([]Domain, error) {
	return s.scanDomains(`SELECT `+domainCols+` FROM domains WHERE app_id = ? ORDER BY id`, appID)
}

func (s *Store) AllDomains() ([]Domain, error) {
	return s.scanDomains(`SELECT ` + domainCols + ` FROM domains ORDER BY id`)
}

func (s *Store) DeleteDomain(appID int64, hostname string) error {
	res, err := s.db.Exec(`DELETE FROM domains WHERE app_id = ? AND hostname = ?`,
		appID, strings.ToLower(hostname))
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) SetDomainCert(id int64, status, certErr, expiresAt string) error {
	_, err := s.db.Exec(
		`UPDATE domains SET cert_status = ?, cert_error = ?, cert_expires_at = ? WHERE id = ?`,
		status, certErr, expiresAt, id)
	return err
}
```

`internal/store/settings.go`:

```go
package store

import (
	"database/sql"
	"errors"
)

// GetSetting reads an install-level key/value setting. ErrNotFound when unset.
func (s *Store) GetSetting(key string) (string, error) {
	var v string
	err := s.db.QueryRow(`SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return v, err
}

func (s *Store) SetSetting(key, value string) error {
	_, err := s.db.Exec(
		`INSERT INTO settings (key, value) VALUES (?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}
```

- [ ] **Step 4: Run** `go test ./internal/store/ -v` — all pass (including the old migration test).
- [ ] **Step 5: Commit** — `feat: domains CRUD with cert state + settings store`

---

### Task 2: render — extra hosts, annotations, TLS blocks

**Files:**
- Modify: `internal/render/render.go`
- Test: `internal/render/render_test.go` (append)

**Interfaces:**
- Consumes: existing `Input`, `Render`.
- Produces (new `Input` fields, all optional — zero values render exactly as today):
  - `ExtraHosts []string` — each becomes an additional Ingress rule with the same backend, after `Host`.
  - `IngressAnnotations map[string]string` — set on the Ingress ObjectMeta.
  - `TLS []netv1.IngressTLS` — set as `spec.tls` verbatim.

- [ ] **Step 1: Failing test** (append to `render_test.go`, following its existing assertion style — it renders and inspects JSON):

```go
func TestRenderCustomDomains(t *testing.T) {
	in := Input{
		AppName: "web", Namespace: "proj", Image: "img:1",
		Host: "web.1-2-3-4.sslip.io", Port: 8080, Replicas: 1,
		ExtraHosts:         []string{"www.example.com"},
		IngressAnnotations: map[string]string{"cert-manager.io/cluster-issuer": "luncur-le"},
		TLS: []netv1.IngressTLS{{
			Hosts: []string{"www.example.com"}, SecretName: "tls-web-abc12345",
		}},
	}
	r, err := Render(in, nil)
	if err != nil {
		t.Fatal(err)
	}
	var ing string
	for _, o := range r.Objects {
		if o.Kind == "Ingress" {
			ing = string(o.JSON)
		}
	}
	for _, want := range []string{
		`"www.example.com"`,
		`"web.1-2-3-4.sslip.io"`,
		`"cert-manager.io/cluster-issuer":"luncur-le"`,
		`"secretName":"tls-web-abc12345"`,
	} {
		if !strings.Contains(ing, want) {
			t.Fatalf("ingress missing %s:\n%s", want, ing)
		}
	}
}
```

(Add `netv1 "k8s.io/api/networking/v1"` and `strings` to the test imports if absent.)

- [ ] **Step 2: Run** `go test ./internal/render/ -run TestRenderCustomDomains -v` — compile failure.

- [ ] **Step 3: Implement.** In `render.go`:

`Input` gains:

```go
	// ExtraHosts adds Ingress rules (same backend) for custom domains.
	ExtraHosts []string
	// IngressAnnotations lands on the Ingress metadata (cert providers).
	IngressAnnotations map[string]string
	// TLS is set as spec.tls verbatim (secret refs per issued domain).
	TLS []netv1.IngressTLS
```

In `Render`, replace the Ingress construction with rule-building over all hosts:

```go
	pathType := netv1.PathTypePrefix
	rule := func(host string) netv1.IngressRule {
		return netv1.IngressRule{
			Host: host,
			IngressRuleValue: netv1.IngressRuleValue{
				HTTP: &netv1.HTTPIngressRuleValue{
					Paths: []netv1.HTTPIngressPath{{
						Path:     "/",
						PathType: &pathType,
						Backend: netv1.IngressBackend{
							Service: &netv1.IngressServiceBackend{
								Name: in.AppName,
								Port: netv1.ServiceBackendPort{Number: 80},
							},
						},
					}},
				},
			},
		}
	}
	rules := []netv1.IngressRule{rule(in.Host)}
	for _, h := range in.ExtraHosts {
		rules = append(rules, rule(h))
	}
	ingMeta := meta(in, in.AppName)
	if len(in.IngressAnnotations) > 0 {
		ingMeta.Annotations = in.IngressAnnotations
	}
	ing := &netv1.Ingress{
		TypeMeta:   metav1.TypeMeta{APIVersion: "networking.k8s.io/v1", Kind: "Ingress"},
		ObjectMeta: ingMeta,
		Spec: netv1.IngressSpec{
			Rules: rules,
			TLS:   in.TLS,
		},
	}
```

- [ ] **Step 4: Run** `go test ./internal/render/ -v` — all pass (existing golden tests unchanged: zero-value new fields must not alter previous output; if a golden test embeds exact JSON with no `tls`, confirm `TLS: nil` omits the field — `IngressSpec.TLS` has `omitempty`).
- [ ] **Step 5: Commit** — `feat: render — extra ingress hosts, annotations, TLS blocks`

---

### Task 3: `internal/acme` — RFC 8555 client + challenge store

**Files:**
- Create: `internal/acme/acme.go`
- Create: `internal/acme/acme_test.go` (includes a fake ACME directory server)

**Interfaces:**
- Consumes: `golang.org/x/crypto/acme` (aliased `xacme`), stdlib crypto.
- Produces:
  - `type Challenges struct` — concurrency-safe token→keyAuth store with `Put(token, keyAuth string)`, `Delete(token string)`, and `ServeHTTP` handling `GET /.well-known/acme-challenge/{token}` (404 unknown). `NewChallenges() *Challenges`.
  - `const ChallengePath = "/.well-known/acme-challenge/"`
  - `type Issuer struct { DirectoryURL string; AccountKey *ecdsa.PrivateKey; Email string; Challenges *Challenges }`
  - `Issuer.Issue(ctx context.Context, domain string) (certPEM, keyPEM []byte, notAfter time.Time, err error)` — registers the account (idempotent), runs one HTTP-01 order end to end.
  - `GenerateAccountKey() (*ecdsa.PrivateKey, error)`, `EncodeAccountKey(k) ([]byte, error)` (PEM), `DecodeAccountKey([]byte) (*ecdsa.PrivateKey, error)` — for persisting the account key in a K8s Secret.
  - `NeedsRenewal(notAfter time.Time, now time.Time) bool` — true within 30 days of expiry.

- [ ] **Step 1: Failing test.** Two parts: the challenge store (pure) and `Issue` against an in-test fake ACME directory. The fake decodes JWS payloads WITHOUT verifying signatures, issues nonces, validates the challenge by actually GETting our challenge server, and signs the CSR with a throwaway CA.

`internal/acme/acme_test.go`:

```go
package acme

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestChallengesServeHTTP(t *testing.T) {
	c := NewChallenges()
	c.Put("tok1", "tok1.keyauth")
	srv := httptest.NewServer(c)
	defer srv.Close()

	resp, err := http.Get(srv.URL + ChallengePath + "tok1")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || string(b) != "tok1.keyauth" {
		t.Fatalf("got %d %q", resp.StatusCode, b)
	}
	if resp, _ := http.Get(srv.URL + ChallengePath + "nope"); resp.StatusCode != 404 {
		t.Fatalf("unknown token: %d, want 404", resp.StatusCode)
	}

	c.Delete("tok1")
	if resp, _ := http.Get(srv.URL + ChallengePath + "tok1"); resp.StatusCode != 404 {
		t.Fatalf("deleted token still served")
	}
}

func TestNeedsRenewal(t *testing.T) {
	now := time.Date(2026, 7, 3, 0, 0, 0, 0, time.UTC)
	if NeedsRenewal(now.Add(60*24*time.Hour), now) {
		t.Fatal("60 days out should not renew")
	}
	if !NeedsRenewal(now.Add(10*24*time.Hour), now) {
		t.Fatal("10 days out should renew")
	}
}

// --- fake ACME directory ---------------------------------------------------

// fakeACME implements just enough of RFC 8555 for x/crypto/acme's client:
// directory, nonce, account, order, authz, http-01 challenge (verified by
// really fetching the challenge URL), finalize (signs the CSR with a test
// CA), and cert download (PEM chain).
type fakeACME struct {
	t         *testing.T
	mux       *http.ServeMux
	srv       *httptest.Server
	caKey     *ecdsa.PrivateKey
	caCert    *x509.Certificate
	chalHost  string // host:port serving the challenge (our Challenges store)
	authzOK   bool
	certDER   []byte
	orderDone bool
}

func newFakeACME(t *testing.T, chalHost string) *fakeACME {
	f := &fakeACME{t: t, mux: http.NewServeMux(), chalHost: chalHost}
	f.srv = httptest.NewServer(f.withNonce(f.mux))
	t.Cleanup(f.srv.Close)

	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	f.caKey = caKey
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "fake ACME CA"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		IsCA:         true, KeyUsage: x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	f.caCert, _ = x509.ParseCertificate(der)

	u := f.srv.URL
	f.mux.HandleFunc("/dir", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{
			"newNonce": u + "/nonce", "newAccount": u + "/acct", "newOrder": u + "/order",
		})
	})
	f.mux.HandleFunc("/nonce", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	f.mux.HandleFunc("/acct", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", u+"/acct/1")
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{"status":"valid"}`)
	})
	f.mux.HandleFunc("/order", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", u+"/order/1")
		w.WriteHeader(http.StatusCreated)
		f.writeOrder(w)
	})
	f.mux.HandleFunc("/order/1", func(w http.ResponseWriter, r *http.Request) {
		f.writeOrder(w)
	})
	f.mux.HandleFunc("/authz/1", func(w http.ResponseWriter, r *http.Request) {
		status := "pending"
		if f.authzOK {
			status = "valid"
		}
		json.NewEncoder(w).Encode(map[string]any{
			"status":     status,
			"identifier": map[string]string{"type": "dns", "value": "www.example.com"},
			"challenges": []map[string]string{{
				"type": "http-01", "url": u + "/chal/1", "token": "tok-e2e", "status": status,
			}},
		})
	})
	f.mux.HandleFunc("/chal/1", func(w http.ResponseWriter, r *http.Request) {
		// "Validate" by fetching the token from the challenge server.
		resp, err := http.Get("http://" + f.chalHost + ChallengePath + "tok-e2e")
		if err != nil || resp.StatusCode != 200 {
			http.Error(w, `{"status":"invalid"}`, http.StatusOK)
			return
		}
		f.authzOK = true
		fmt.Fprint(w, `{"status":"valid"}`)
	})
	f.mux.HandleFunc("/finalize/1", func(w http.ResponseWriter, r *http.Request) {
		body := jwsPayload(t, r)
		var req struct {
			CSR string `json:"csr"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			t.Fatalf("finalize payload: %v", err)
		}
		der, _ := base64.RawURLEncoding.DecodeString(req.CSR)
		csr, err := x509.ParseCertificateRequest(der)
		if err != nil {
			t.Fatalf("parse csr: %v", err)
		}
		leaf := &x509.Certificate{
			SerialNumber: big.NewInt(2),
			Subject:      pkix.Name{CommonName: csr.Subject.CommonName},
			DNSNames:     csr.DNSNames,
			NotBefore:    time.Now().Add(-time.Hour),
			NotAfter:     time.Now().Add(90 * 24 * time.Hour),
			KeyUsage:     x509.KeyUsageDigitalSignature,
			ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		}
		f.certDER, err = x509.CreateCertificate(rand.Reader, leaf, f.caCert, csr.PublicKey, f.caKey)
		if err != nil {
			t.Fatal(err)
		}
		f.orderDone = true
		f.writeOrder(w)
	})
	f.mux.HandleFunc("/cert/1", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/pem-certificate-chain")
		pem.Encode(w, &pem.Block{Type: "CERTIFICATE", Bytes: f.certDER})
	})
	return f
}

func (f *fakeACME) writeOrder(w http.ResponseWriter) {
	u := f.srv.URL
	status := "pending"
	if f.authzOK {
		status = "ready"
	}
	if f.orderDone {
		status = "valid"
	}
	json.NewEncoder(w).Encode(map[string]any{
		"status":         status,
		"authorizations": []string{u + "/authz/1"},
		"finalize":       u + "/finalize/1",
		"certificate":    u + "/cert/1",
	})
}

// withNonce stamps a Replay-Nonce on every response (the client demands it).
func (f *fakeACME) withNonce(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Replay-Nonce", "nonce-"+fmt.Sprint(time.Now().UnixNano()))
		w.Header().Set("Content-Type", "application/json")
		next.ServeHTTP(w, r)
	})
}

// jwsPayload extracts the base64url payload from a JWS body, skipping
// signature verification entirely (this is a test double).
func jwsPayload(t *testing.T, r *http.Request) []byte {
	t.Helper()
	var env struct {
		Payload string `json:"payload"`
	}
	if err := json.NewDecoder(r.Body).Decode(&env); err != nil {
		t.Fatalf("jws decode: %v", err)
	}
	if env.Payload == "" {
		return nil // POST-as-GET
	}
	b, err := base64.RawURLEncoding.DecodeString(env.Payload)
	if err != nil {
		t.Fatalf("jws payload b64: %v", err)
	}
	return b
}

func TestIssueEndToEnd(t *testing.T) {
	ch := NewChallenges()
	chalSrv := httptest.NewServer(ch)
	defer chalSrv.Close()

	fake := newFakeACME(t, strings.TrimPrefix(chalSrv.URL, "http://"))

	key, err := GenerateAccountKey()
	if err != nil {
		t.Fatal(err)
	}
	iss := &Issuer{
		DirectoryURL: fake.srv.URL + "/dir",
		AccountKey:   key,
		Email:        "admin@example.com",
		Challenges:   ch,
	}
	ctx, cancel := contextWithTimeout(t)
	defer cancel()
	certPEM, keyPEM, notAfter, err := iss.Issue(ctx, "www.example.com")
	if err != nil {
		t.Fatal(err)
	}
	blk, _ := pem.Decode(certPEM)
	if blk == nil || blk.Type != "CERTIFICATE" {
		t.Fatal("no certificate PEM")
	}
	cert, err := x509.ParseCertificate(blk.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if len(cert.DNSNames) == 0 || cert.DNSNames[0] != "www.example.com" {
		t.Fatalf("dns names = %v", cert.DNSNames)
	}
	if kb, _ := pem.Decode(keyPEM); kb == nil || kb.Type != "EC PRIVATE KEY" {
		t.Fatal("no key PEM")
	}
	if notAfter.Before(time.Now().Add(24 * time.Hour)) {
		t.Fatalf("notAfter too soon: %v", notAfter)
	}

	// Account key round-trips through PEM (for the K8s Secret).
	enc, err := EncodeAccountKey(key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeAccountKey(enc); err != nil {
		t.Fatal(err)
	}
}
```

with the small helper (avoids importing context twice in examples):

```go
func contextWithTimeout(t *testing.T) (context.Context, context.CancelFunc) {
	t.Helper()
	return context.WithTimeout(context.Background(), 30*time.Second)
}
```

(add `"context"` import).

- [ ] **Step 2: Run** `go test ./internal/acme/ -v` — package missing.

- [ ] **Step 3: Implement** `internal/acme/acme.go`:

```go
// Package acme issues TLS certificates via RFC 8555 (Let's Encrypt) using
// HTTP-01 challenges served by luncur itself.
package acme

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	xacme "golang.org/x/crypto/acme"
)

// LetsEncryptDirectory is the production directory URL.
const LetsEncryptDirectory = "https://acme-v02.api.letsencrypt.org/directory"

const ChallengePath = "/.well-known/acme-challenge/"

// Challenges is a concurrency-safe token → keyAuthorization store that
// doubles as the HTTP handler for the well-known challenge path.
type Challenges struct {
	mu sync.Mutex
	m  map[string]string
}

func NewChallenges() *Challenges { return &Challenges{m: map[string]string{}} }

func (c *Challenges) Put(token, keyAuth string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m[token] = keyAuth
}

func (c *Challenges) Delete(token string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.m, token)
}

func (c *Challenges) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	token := strings.TrimPrefix(r.URL.Path, ChallengePath)
	if token == "" || token == r.URL.Path {
		http.NotFound(w, r)
		return
	}
	c.mu.Lock()
	keyAuth, ok := c.m[token]
	c.mu.Unlock()
	if !ok {
		http.NotFound(w, r)
		return
	}
	fmt.Fprint(w, keyAuth)
}

// Issuer drives one ACME account.
type Issuer struct {
	DirectoryURL string
	AccountKey   *ecdsa.PrivateKey
	Email        string
	Challenges   *Challenges
}

func GenerateAccountKey() (*ecdsa.PrivateKey, error) {
	return ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
}

func EncodeAccountKey(k *ecdsa.PrivateKey) ([]byte, error) {
	der, err := x509.MarshalECPrivateKey(k)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}), nil
}

func DecodeAccountKey(b []byte) (*ecdsa.PrivateKey, error) {
	blk, _ := pem.Decode(b)
	if blk == nil {
		return nil, fmt.Errorf("no PEM block in account key")
	}
	return x509.ParseECPrivateKey(blk.Bytes)
}

// NeedsRenewal reports whether a cert expiring at notAfter should be
// re-issued now (within 30 days of expiry).
func NeedsRenewal(notAfter, now time.Time) bool {
	return now.Add(30 * 24 * time.Hour).After(notAfter)
}

// Issue runs one HTTP-01 order end to end and returns the PEM-encoded cert
// chain + private key and the leaf's NotAfter.
func (i *Issuer) Issue(ctx context.Context, domain string) (certPEM, keyPEM []byte, notAfter time.Time, err error) {
	cl := &xacme.Client{Key: i.AccountKey, DirectoryURL: i.DirectoryURL}

	// Idempotent registration: AlreadyRegistered is fine.
	_, err = cl.Register(ctx, &xacme.Account{Contact: []string{"mailto:" + i.Email}},
		xacme.AcceptTOS)
	if err != nil && err != xacme.ErrAccountAlreadyExists {
		return nil, nil, time.Time{}, fmt.Errorf("acme register: %w", err)
	}

	order, err := cl.AuthorizeOrder(ctx, xacme.DomainIDs(domain))
	if err != nil {
		return nil, nil, time.Time{}, fmt.Errorf("acme new order: %w", err)
	}

	for _, zurl := range order.AuthzURLs {
		z, err := cl.GetAuthorization(ctx, zurl)
		if err != nil {
			return nil, nil, time.Time{}, fmt.Errorf("acme authz: %w", err)
		}
		if z.Status == xacme.StatusValid {
			continue
		}
		var chal *xacme.Challenge
		for _, c := range z.Challenges {
			if c.Type == "http-01" {
				chal = c
				break
			}
		}
		if chal == nil {
			return nil, nil, time.Time{}, fmt.Errorf("no http-01 challenge offered for %s", domain)
		}
		keyAuth, err := cl.HTTP01ChallengeResponse(chal.Token)
		if err != nil {
			return nil, nil, time.Time{}, err
		}
		i.Challenges.Put(chal.Token, keyAuth)
		defer i.Challenges.Delete(chal.Token)

		if _, err := cl.Accept(ctx, chal); err != nil {
			return nil, nil, time.Time{}, fmt.Errorf("acme accept: %w", err)
		}
		if _, err := cl.WaitAuthorization(ctx, z.URI); err != nil {
			return nil, nil, time.Time{}, fmt.Errorf("acme authorization failed: %w", err)
		}
	}

	if _, err := cl.WaitOrder(ctx, order.URI); err != nil {
		return nil, nil, time.Time{}, fmt.Errorf("acme order: %w", err)
	}

	certKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, time.Time{}, err
	}
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject:  pkix.Name{CommonName: domain},
		DNSNames: []string{domain},
	}, certKey)
	if err != nil {
		return nil, nil, time.Time{}, err
	}

	der, _, err := cl.CreateOrderCert(ctx, order.FinalizeURL, csr, true)
	if err != nil {
		return nil, nil, time.Time{}, fmt.Errorf("acme finalize: %w", err)
	}

	for _, b := range der {
		certPEM = append(certPEM, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: b})...)
	}
	leaf, err := x509.ParseCertificate(der[0])
	if err != nil {
		return nil, nil, time.Time{}, err
	}
	keyDER, err := x509.MarshalECPrivateKey(certKey)
	if err != nil {
		return nil, nil, time.Time{}, err
	}
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM, leaf.NotAfter, nil
}
```

**Iteration note:** the fake directory is a test double for x/crypto/acme's real client — if the client demands a field the fake omits (e.g. order `identifiers`, authz `wildcard`), extend the FAKE, never special-case the production code. Keep signatures unverified.

- [ ] **Step 4: Run** `go test ./internal/acme/ -v` — all pass.
- [ ] **Step 5: Commit** — `feat: internal/acme — HTTP-01 issuer + challenge store`

---

### Task 4: kube — provider CRD kinds + detection

**Files:**
- Modify: `internal/kube/kube.go`
- Test: `internal/kube/kube_test.go` (append)

**Interfaces:**
- Consumes: existing `gvrByKind`, `clusterScoped`, `Apply`, `Client.cs` (typed clientset).
- Produces:
  - `gvrByKind` gains: `"HelmChartConfig": {Group: "helm.cattle.io", Version: "v1", Resource: "helmchartconfigs"}`, `"ClusterIssuer": {Group: "cert-manager.io", Version: "v1", Resource: "clusterissuers"}`.
  - `clusterScoped` gains `"ClusterIssuer": true`.
  - `Client.HasGroupVersion(ctx context.Context, gv string) (bool, error)` — discovery check (`helm.cattle.io/v1`, `cert-manager.io/v1`), false (not error) when the group is absent.

- [ ] **Step 1: Failing test** (append to `kube_test.go`, reusing its fake patterns):

```go
func TestHasGroupVersion(t *testing.T) {
	cs := k8sfake.NewSimpleClientset()
	cs.Resources = []*metav1.APIResourceList{
		{GroupVersion: "helm.cattle.io/v1", APIResources: []metav1.APIResource{{Name: "helmchartconfigs"}}},
	}
	c := NewForTest(nil, cs)
	ok, err := c.HasGroupVersion(context.Background(), "helm.cattle.io/v1")
	if err != nil || !ok {
		t.Fatalf("helm gv: ok=%v err=%v", ok, err)
	}
	ok, err = c.HasGroupVersion(context.Background(), "cert-manager.io/v1")
	if err != nil || ok {
		t.Fatalf("absent gv: ok=%v err=%v, want false nil", ok, err)
	}
}

func TestApplyClusterIssuerIsClusterScoped(t *testing.T) {
	// Mirror TestApplyClusterRoleBindingSki... (existing test for the
	// cluster-scoped path): apply a ClusterIssuer object and assert the
	// fake dynamic client saw a cluster-scoped (no-namespace) patch.
	// Copy that test's arrange verbatim, swapping kind/GVR:
	//   apiVersion cert-manager.io/v1, kind ClusterIssuer, name luncur-le.
}
```

Write the second test as real code by copying the existing cluster-scoped test in the file (it exists for ClusterRoleBinding — find it and mirror).

- [ ] **Step 2: Run** — compile failure (`HasGroupVersion` undefined) / missing gvr entries.

- [ ] **Step 3: Implement** in `kube.go`:

```go
// gvrByKind additions:
	"HelmChartConfig": {Group: "helm.cattle.io", Version: "v1", Resource: "helmchartconfigs"},
	"ClusterIssuer":   {Group: "cert-manager.io", Version: "v1", Resource: "clusterissuers"},

// clusterScoped addition:
	"ClusterIssuer": true,
```

```go
// HasGroupVersion reports whether the cluster serves the given
// group/version (e.g. "cert-manager.io/v1") — used to detect optional
// provider CRDs before selecting them.
func (c *Client) HasGroupVersion(ctx context.Context, gv string) (bool, error) {
	_, err := c.cs.Discovery().ServerResourcesForGroupVersion(gv)
	if err != nil {
		if apierrors.IsNotFound(err) || strings.Contains(err.Error(), "not found") {
			return false, nil
		}
		return false, err
	}
	return true, nil
}
```

(Add `"strings"` import if absent. The fake discovery returns a plain error for unknown GVs — the `strings.Contains` fallback covers both fake and real servers.)

- [ ] **Step 4: Run** `go test ./internal/kube/ -v` — pass.
- [ ] **Step 5: Commit** — `feat: kube — provider CRD kinds + group-version detection`

---

### Task 5: server — domains API + provider-aware rendering

**Files:**
- Create: `internal/server/domains.go`
- Modify: `internal/server/sync.go` (renderApp loads domains + provider config)
- Modify: `internal/server/server.go` (routes; `certProvider` accessor)
- Test: `internal/server/domains_test.go`

**Interfaces:**
- Consumes: Task 1 store methods, Task 2 render fields, `requireProject`/`requireApp`, `syncIfLive`.
- Produces:
  - `POST /v1/projects/{project}/apps/{app}/domains` body `{"hostname":"..."}` → 201 `{"hostname":...,"cert_status":...,"dns_warning":"..."}`; `dns_warning` non-empty when the hostname doesn't resolve to `s.externalIP` (use `net.LookupHost` with a 3s context; lookup failure → warning, never a hard error). Kicks the cert manager (Task 6) when provider is `builtin`.
  - `GET .../domains` → 200 list with cert fields.
  - `DELETE .../domains/{hostname}` → 204 (+ re-sync).
  - `POST .../domains/{hostname}/retry` → 202, resets status to `none` and re-kicks issuance (builtin only; other providers → 409 `wrong_provider`).
  - `s.certProviderName() string` — reads setting `cert_provider`, defaults `"builtin"`.
  - `renderApp` gains domain awareness: loads `ListDomains`, adds `ExtraHosts` (all domains), and per provider:
    - `builtin`: TLS blocks only for `cert_status == "issued"` domains, `SecretName: certSecretName(app, hostname)`.
    - `traefik`: annotations `traefik.ingress.kubernetes.io/router.tls: "true"` and `traefik.ingress.kubernetes.io/router.tls.certresolver: "le"` when any domain exists; no TLS blocks.
    - `cert-manager`: annotation `cert-manager.io/cluster-issuer: "luncur-le"` + TLS block per domain (any status — cert-manager fills the Secret).
  - `func certSecretName(app, hostname string) string` — `"tls-" + app + "-" + hex(sha256(hostname))[:8]`.

- [ ] **Step 1: Failing tests** (`internal/server/domains_test.go`, following `apps_test.go` arrange style — seeded store, `New(Deps{...})`, authed requests):

```go
func TestDomainCRUDAndRender(t *testing.T) {
	// Arrange: store + server with ExternalIP "1.2.3.4", admin token,
	// project "proj", app "web" (port 8080) with a live deployment and
	// fake kube (copy the fixture apps_test.go uses for scale/sync tests).
	// 1. POST domains {"hostname":"www.example.com"} → 201; body has
	//    cert_status "none" and a non-empty dns_warning (www.example.com
	//    does not resolve to 1.2.3.4 in test).
	// 2. GET domains → list of 1.
	// 3. GET /v1/projects/proj/apps/web/raw → body contains
	//    "www.example.com" (render picked the domain up as an extra host).
	// 4. DELETE .../domains/www.example.com → 204; GET → empty.
	// 5. POST bad hostname {"hostname":"nodot"} → 400.
	// Real assertions, real helpers from the package's other tests.
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

func TestRenderProviderAnnotations(t *testing.T) {
	// Arrange server + seeded app with one domain, then for each provider
	// setting (st.SetSetting("cert_provider", ...)) call s.renderApp
	// directly (same package) and inspect the Ingress JSON:
	//   builtin + status none  → no tls block, no annotations
	//   builtin + status issued (st.SetDomainCert(...)) → tls block with
	//     certSecretName, no provider annotations
	//   traefik → router.tls.certresolver annotation, no tls block
	//   cert-manager → cluster-issuer annotation + tls block
}
```

Write all three with real code.

- [ ] **Step 2: Run** — failures.

- [ ] **Step 3: Implement.**

`internal/server/domains.go`:

```go
package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"time"

	"github.com/sutantodadang/luncur/internal/store"
)

// certSecretName is the TLS Secret an app's domain cert is stored in —
// deterministic so render and issuance agree without coordination.
func certSecretName(app, hostname string) string {
	sum := sha256.Sum256([]byte(hostname))
	return "tls-" + app + "-" + hex.EncodeToString(sum[:])[:8]
}

// certProviderName reads the install-level provider setting.
func (s *server) certProviderName() string {
	v, err := s.st.GetSetting("cert_provider")
	if err != nil || v == "" {
		return "builtin"
	}
	return v
}

// dnsWarning checks that hostname resolves to the advertised IP. Returns a
// human warning ("" when all good). Never blocks domain creation.
func dnsWarning(ctx context.Context, hostname, wantIP string) string {
	rctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	addrs, err := (&net.Resolver{}).LookupHost(rctx, hostname)
	if err != nil {
		return fmt.Sprintf("DNS lookup failed for %s — point an A record at %s", hostname, wantIP)
	}
	for _, a := range addrs {
		if a == wantIP {
			return ""
		}
	}
	return fmt.Sprintf("%s resolves to %v, not %s — TLS issuance will fail until DNS points here", hostname, addrs, wantIP)
}

func domainJSON(d store.Domain, warning string) map[string]any {
	out := map[string]any{
		"hostname": d.Hostname, "cert_status": d.CertStatus,
		"cert_error": d.CertError, "cert_expires_at": d.CertExpiresAt,
	}
	if warning != "" {
		out["dns_warning"] = warning
	}
	return out
}

func (s *server) handleAddDomain(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.requireProject(w, u, r.PathValue("project"))
	if !ok {
		return
	}
	a, ok := s.requireApp(w, p, r.PathValue("app"))
	if !ok {
		return
	}
	var req struct {
		Hostname string `json:"hostname"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	d, err := s.st.AddDomain(a.ID, req.Hostname)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	// Non-builtin providers own issuance — mark the row so the UI/CLI show
	// "external" instead of a forever-"none".
	if s.certProviderName() != "builtin" {
		if err := s.st.SetDomainCert(d.ID, "external", "", ""); err == nil {
			d.CertStatus = "external"
		}
	}
	warning := dnsWarning(r.Context(), d.Hostname, s.externalIP)
	s.syncIfLive(r.Context(), p, a)
	s.kickCerts(p, a, d) // Task 6 wires this; stub in this task (see below)
	writeJSON(w, http.StatusCreated, domainJSON(d, warning))
}

func (s *server) handleListDomains(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.requireProject(w, u, r.PathValue("project"))
	if !ok {
		return
	}
	a, ok := s.requireApp(w, p, r.PathValue("app"))
	if !ok {
		return
	}
	list, err := s.st.ListDomains(a.ID)
	if err != nil {
		log.Printf("list domains: %v", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	out := make([]map[string]any, 0, len(list))
	for _, d := range list {
		out = append(out, domainJSON(d, ""))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *server) handleDeleteDomain(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.requireProject(w, u, r.PathValue("project"))
	if !ok {
		return
	}
	a, ok := s.requireApp(w, p, r.PathValue("app"))
	if !ok {
		return
	}
	if err := s.st.DeleteDomain(a.ID, r.PathValue("hostname")); errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "no such domain")
		return
	} else if err != nil {
		log.Printf("delete domain: %v", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	s.syncIfLive(r.Context(), p, a)
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) handleRetryDomain(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.requireProject(w, u, r.PathValue("project"))
	if !ok {
		return
	}
	a, ok := s.requireApp(w, p, r.PathValue("app"))
	if !ok {
		return
	}
	if s.certProviderName() != "builtin" {
		writeError(w, http.StatusConflict, "wrong_provider", "cert retry only applies to the builtin provider")
		return
	}
	list, err := s.st.ListDomains(a.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	for _, d := range list {
		if d.Hostname == r.PathValue("hostname") {
			if err := s.st.SetDomainCert(d.ID, "none", "", ""); err != nil {
				writeError(w, http.StatusInternalServerError, "internal", "internal error")
				return
			}
			d.CertStatus, d.CertError = "none", ""
			s.kickCerts(p, a, d)
			w.WriteHeader(http.StatusAccepted)
			return
		}
	}
	writeError(w, http.StatusNotFound, "not_found", "no such domain")
}
```

For THIS task, `kickCerts` is a no-op stub (Task 6 replaces it):

```go
// kickCerts nudges the cert manager about a domain. Wired by the builtin
// provider's manager; a nil manager (tests, non-builtin providers) is a
// no-op.
func (s *server) kickCerts(p store.Project, a store.App, d store.Domain) {
	if s.certs != nil {
		s.certs.Kick(p, a, d)
	}
}
```

and `server` struct gains a `certs *certManager` field with `type certManager struct{}` placeholder having a `Kick(store.Project, store.App, store.Domain)` method that does nothing — Task 6 fills it in. (Keeping the field now avoids touching Task 5's files again.)

`sync.go` — `renderApp` gains, after loading overrides:

```go
	domains, err := s.st.ListDomains(a.ID)
	if err != nil {
		return render.Rendered{}, fmt.Errorf("list domains: %w", err)
	}
	var extraHosts []string
	var tls []netv1.IngressTLS
	annotations := map[string]string{}
	provider := s.certProviderName()
	for _, d := range domains {
		extraHosts = append(extraHosts, d.Hostname)
		switch provider {
		case "builtin":
			if d.CertStatus == "issued" {
				tls = append(tls, netv1.IngressTLS{
					Hosts: []string{d.Hostname}, SecretName: certSecretName(a.Name, d.Hostname),
				})
			}
		case "cert-manager":
			tls = append(tls, netv1.IngressTLS{
				Hosts: []string{d.Hostname}, SecretName: certSecretName(a.Name, d.Hostname),
			})
		}
	}
	if len(domains) > 0 {
		switch provider {
		case "traefik":
			annotations["traefik.ingress.kubernetes.io/router.tls"] = "true"
			annotations["traefik.ingress.kubernetes.io/router.tls.certresolver"] = "le"
		case "cert-manager":
			annotations["cert-manager.io/cluster-issuer"] = "luncur-le"
		}
	}
	if len(annotations) == 0 {
		annotations = nil
	}
```

and the `render.Input` literal gains `ExtraHosts: extraHosts, IngressAnnotations: annotations, TLS: tls` (import `netv1 "k8s.io/api/networking/v1"`).

Routes in `server.go` (with the other app routes):

```go
	mux.HandleFunc("POST /v1/projects/{project}/apps/{app}/domains", s.authed(s.handleAddDomain))
	mux.HandleFunc("GET /v1/projects/{project}/apps/{app}/domains", s.authed(s.handleListDomains))
	mux.HandleFunc("DELETE /v1/projects/{project}/apps/{app}/domains/{hostname}", s.authed(s.handleDeleteDomain))
	mux.HandleFunc("POST /v1/projects/{project}/apps/{app}/domains/{hostname}/retry", s.authed(s.handleRetryDomain))
```

- [ ] **Step 4: Run** `go test ./internal/server/ -v` — pass.
- [ ] **Step 5: Commit** — `feat: domains API + provider-aware ingress rendering`

---

### Task 6: server — builtin cert manager (issuance + renewal)

**Files:**
- Create: `internal/server/certs.go`
- Modify: `internal/server/server.go` (mount challenge handler, construct manager)
- Modify: `internal/server/domains.go` (only if the Task-5 placeholder needs its type replaced)
- Test: `internal/server/certs_test.go`

**Interfaces:**
- Consumes: `internal/acme` (Task 3), `kube.Apply`/`GetSecretData`, store domain methods, `render.Object`.
- Produces:
  - `type certManager struct { ... }` (replaces Task 5's placeholder) with:
    - `newCertManager(s *server, directoryURL string) *certManager`
    - `Kick(p store.Project, a store.App, d store.Domain)` — non-blocking enqueue.
    - `Run(ctx context.Context)` — worker loop + daily renewal sweep; exported entry `(*server).StartCerts(ctx)` called by serve.go (Task 8) and tests.
    - `Challenges() http.Handler` — the acme challenge store.
  - Server mux: `GET /.well-known/acme-challenge/{token}` → challenge store (NO auth).
  - Issuance flow per domain: status→`pending` → ensure ACME account key (Secret `luncur-acme-account` in `luncur-system` via `kube.GetSecretData`; generate+apply when missing) → ensure challenge Ingress `luncur-acme` in `luncur-system` contains the host (path `/.well-known/acme-challenge/` → Service `luncur:80`) → `Issuer.Issue` → apply TLS Secret `certSecretName(app, host)` (type `kubernetes.io/tls`, app namespace) → status `issued` + expiry → remove host from challenge Ingress → re-sync app. Failure → status `failed` + error message.
  - Renewal sweep: daily ticker; for every domain with status `issued` whose `cert_expires_at` parses and `acme.NeedsRenewal(...)`, re-enqueue.
  - Settings consumed: `acme_email` (default `admin@<externalIP>.sslip.io`... no — default `admin@luncur.local` is invalid for LE; use setting or empty contact), `acme_directory` (default `acme.LetsEncryptDirectory`). Constructor argument wins (tests pass the fake directory URL).

- [ ] **Step 1: Failing test** (`internal/server/certs_test.go`) — reuse Task 3's fake ACME directory by exporting it? No — copy the fake into this package's test file? Too heavy. Instead: move the fake ACME server from `internal/acme/acme_test.go` into an exported test helper package `internal/acme/acmetest/acmetest.go` (non-test package so server tests can import it), with `func New(t *testing.T, chalHost string) *Server` exposing `DirectoryURL() string`. Adjust Task 3's own test to import it. Then:

```go
func TestCertManagerIssuesAndRenews(t *testing.T) {
	// Arrange: server with fake dynamic kube (record Apply calls),
	// fake typed clientset, temp store; seed project/app/domain
	// (status none); challenge handler mounted on an httptest server
	// (mux from s.handler()); fake ACME directory pointed at that
	// httptest server's host.
	// Act: cm := newCertManager(srv, fakeDir.DirectoryURL()); go cm.Run(ctx);
	//       cm.Kick(p, a, d)
	// Assert (poll with deadline):
	//   - domains row reaches cert_status "issued" with non-empty expiry
	//   - fake dynamic client saw an applied Secret named
	//     certSecretName("web", "www.example.com") of type kubernetes.io/tls
	//     in the app namespace
	//   - challenge Ingress "luncur-acme" in luncur-system was applied
	//     (host present during issuance)
}

func TestCertManagerFailureMarksDomain(t *testing.T) {
	// Same arrange but the fake ACME's challenge validation will fail
	// (point fake at an unreachable chalHost): domain ends "failed" with
	// non-empty cert_error.
}
```

Write with real code; the package's existing fake-kube fixtures (`apps_test.go`, `build_test.go`) show dynamic-fake construction and reaction recording.

- [ ] **Step 2: Run** — compile failures.

- [ ] **Step 3: Implement** `internal/server/certs.go`:

```go
package server

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/sutantodadang/luncur/internal/acme"
	"github.com/sutantodadang/luncur/internal/render"
	"github.com/sutantodadang/luncur/internal/store"
)

const acmeAccountSecret = "luncur-acme-account"
const challengeIngress = "luncur-acme"

type certJob struct {
	p store.Project
	a store.App
	d store.Domain
}

// certManager drives builtin-provider cert issuance and renewal.
type certManager struct {
	s            *server
	directoryURL string
	challenges   *acme.Challenges

	jobs chan certJob

	mu           sync.Mutex
	pendingHosts map[string]bool // hosts currently in the challenge Ingress
}

func newCertManager(s *server, directoryURL string) *certManager {
	if directoryURL == "" {
		if v, err := s.st.GetSetting("acme_directory"); err == nil && v != "" {
			directoryURL = v
		} else {
			directoryURL = acme.LetsEncryptDirectory
		}
	}
	return &certManager{
		s: s, directoryURL: directoryURL,
		challenges:   acme.NewChallenges(),
		jobs:         make(chan certJob, 64),
		pendingHosts: map[string]bool{},
	}
}

func (m *certManager) Challenges() http.Handler { return m.challenges }

// Kick enqueues issuance for a domain; drops silently when the queue is
// full (the renewal sweep will pick it up again).
func (m *certManager) Kick(p store.Project, a store.App, d store.Domain) {
	select {
	case m.jobs <- certJob{p, a, d}:
	default:
	}
}

// Run processes issuance jobs and sweeps daily for renewals until ctx ends.
func (m *certManager) Run(ctx context.Context) {
	tick := time.NewTicker(24 * time.Hour)
	defer tick.Stop()
	m.sweep(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case j := <-m.jobs:
			m.issue(ctx, j)
		case <-tick.C:
			m.sweep(ctx)
		}
	}
}

// sweep re-enqueues unissued domains and soon-to-expire certs.
func (m *certManager) sweep(ctx context.Context) {
	domains, err := m.s.st.AllDomains()
	if err != nil {
		log.Printf("cert sweep: %v", err)
		return
	}
	for _, d := range domains {
		renew := false
		switch d.CertStatus {
		case "none", "pending":
			renew = true
		case "issued":
			if exp, err := time.Parse(time.RFC3339, d.CertExpiresAt); err == nil {
				renew = acme.NeedsRenewal(exp, time.Now())
			}
		}
		if !renew {
			continue
		}
		p, a, err := m.s.projectAppForDomain(d)
		if err != nil {
			log.Printf("cert sweep domain %s: %v", d.Hostname, err)
			continue
		}
		m.Kick(p, a, d)
	}
}

// issue runs one domain's issuance end to end.
func (m *certManager) issue(ctx context.Context, j certJob) {
	if m.s.kube == nil {
		return
	}
	st := m.s.st
	fail := func(err error) {
		log.Printf("cert %s: %v", j.d.Hostname, err)
		if e := st.SetDomainCert(j.d.ID, "failed", err.Error(), ""); e != nil {
			log.Printf("mark cert failed: %v", e)
		}
	}
	if err := st.SetDomainCert(j.d.ID, "pending", "", j.d.CertExpiresAt); err != nil {
		fail(err)
		return
	}

	key, err := m.accountKey(ctx)
	if err != nil {
		fail(fmt.Errorf("acme account key: %w", err))
		return
	}
	if err := m.setChallengeHost(ctx, j.d.Hostname, true); err != nil {
		fail(fmt.Errorf("challenge ingress: %w", err))
		return
	}
	defer func() {
		if err := m.setChallengeHost(ctx, j.d.Hostname, false); err != nil {
			log.Printf("remove challenge host %s: %v", j.d.Hostname, err)
		}
	}()

	email, _ := st.GetSetting("acme_email")
	iss := &acme.Issuer{
		DirectoryURL: m.directoryURL, AccountKey: key,
		Email: email, Challenges: m.challenges,
	}
	ictx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	certPEM, keyPEM, notAfter, err := iss.Issue(ictx, j.d.Hostname)
	if err != nil {
		fail(err)
		return
	}

	secJSON, err := json.Marshal(map[string]any{
		"apiVersion": "v1", "kind": "Secret",
		"metadata": map[string]any{
			"name": certSecretName(j.a.Name, j.d.Hostname), "namespace": j.p.Namespace,
			"labels": map[string]string{"app.kubernetes.io/managed-by": "luncur"},
		},
		"type": "kubernetes.io/tls",
		"stringData": map[string]string{
			"tls.crt": string(certPEM), "tls.key": string(keyPEM),
		},
	})
	if err != nil {
		fail(err)
		return
	}
	if err := m.s.kube.Apply(ctx, j.p.Namespace, []render.Object{{Kind: "Secret", JSON: secJSON}}); err != nil {
		fail(fmt.Errorf("apply tls secret: %w", err))
		return
	}
	if err := st.SetDomainCert(j.d.ID, "issued", "", notAfter.UTC().Format(time.RFC3339)); err != nil {
		fail(err)
		return
	}
	if err := m.s.syncApp(ctx, j.p, j.a); err != nil {
		log.Printf("sync after cert %s: %v", j.d.Hostname, err)
	}
	log.Printf("cert issued for %s (expires %s)", j.d.Hostname, notAfter.Format(time.RFC3339))
}

// (accountKey below; projectAppForDomain at the end of the file.)
```

`accountKey` in full:

```go
func (m *certManager) accountKey(ctx context.Context) (*ecdsa.PrivateKey, error) {
	data, err := m.s.kube.GetSecretData(ctx, m.s.systemNamespace, acmeAccountSecret)
	if err != nil {
		return nil, err
	}
	if pemBytes, ok := data["key.pem"]; ok {
		return acme.DecodeAccountKey(pemBytes)
	}
	key, err := acme.GenerateAccountKey()
	if err != nil {
		return nil, err
	}
	pemBytes, err := acme.EncodeAccountKey(key)
	if err != nil {
		return nil, err
	}
	secJSON, err := json.Marshal(map[string]any{
		"apiVersion": "v1", "kind": "Secret",
		"metadata": map[string]any{
			"name": acmeAccountSecret, "namespace": m.s.systemNamespace,
			"labels": map[string]string{"app.kubernetes.io/managed-by": "luncur"},
		},
		"type":       "Opaque",
		"stringData": map[string]string{"key.pem": string(pemBytes)},
	})
	if err != nil {
		return nil, err
	}
	if err := m.s.kube.Apply(ctx, m.s.systemNamespace, []render.Object{{Kind: "Secret", JSON: secJSON}}); err != nil {
		return nil, err
	}
	return key, nil
}
```

(import `"crypto/ecdsa"`.)

`setChallengeHost` maintains the shared challenge Ingress:

```go
// setChallengeHost adds/removes a host on the luncur-acme Ingress in
// luncur-system, which routes ONLY the ACME challenge path to luncur.
// Traefik merges same-host rules across namespaces; the longer challenge
// path wins over the app's "/" rule during validation.
func (m *certManager) setChallengeHost(ctx context.Context, host string, present bool) error {
	m.mu.Lock()
	if present {
		m.pendingHosts[host] = true
	} else {
		delete(m.pendingHosts, host)
	}
	hosts := make([]string, 0, len(m.pendingHosts))
	for h := range m.pendingHosts {
		hosts = append(hosts, h)
	}
	m.mu.Unlock()
	sort.Strings(hosts)

	rules := make([]map[string]any, 0, len(hosts))
	for _, h := range hosts {
		rules = append(rules, map[string]any{
			"host": h,
			"http": map[string]any{
				"paths": []map[string]any{{
					"path": acme.ChallengePath, "pathType": "Prefix",
					"backend": map[string]any{"service": map[string]any{
						"name": "luncur", "port": map[string]any{"number": int64(80)},
					}},
				}},
			},
		})
	}
	ing := map[string]any{
		"apiVersion": "networking.k8s.io/v1", "kind": "Ingress",
		"metadata": map[string]any{
			"name": challengeIngress, "namespace": m.s.systemNamespace,
			"labels": map[string]string{"app.kubernetes.io/managed-by": "luncur"},
		},
		"spec": map[string]any{"rules": rules},
	}
	b, err := json.Marshal(ing)
	if err != nil {
		return err
	}
	return m.s.kube.Apply(ctx, m.s.systemNamespace, []render.Object{{Kind: "Ingress", JSON: b}})
}
```

(import `"sort"`. An Ingress with zero rules is legal — it just routes nothing.)

`projectAppForDomain` — small store-walk helper on `*server` (in `certs.go`). The store has no `GetAppByID`/`GetProjectByID` yet — add the two methods to `internal/store/apps.go` / `projects.go` following their `GetApp`/`GetProject` shapes (`SELECT ... WHERE id = ?`, `ErrNotFound` on no rows), with short tests appended to the respective store test files. Then:

```go
// projectAppForDomain resolves the app + project a domain row belongs to.
func (s *server) projectAppForDomain(d store.Domain) (store.Project, store.App, error) {
	a, err := s.st.GetAppByID(d.AppID)
	if err != nil {
		return store.Project{}, store.App{}, err
	}
	p, err := s.st.GetProjectByID(a.ProjectID)
	if err != nil {
		return store.Project{}, store.App{}, err
	}
	return p, a, nil
}
```

(Confirm `store.App` has a `ProjectID` field — it does, per the apps schema; adapt the field name to the struct.)

`server.go` wiring:

```go
// in type server: replace the Task-5 placeholder field
	certs *certManager

// in newServer, after s.tmpl assignment:
	if d.Store != nil {
		s.certs = newCertManager(s, d.ACMEDirectory)
	}

// Deps gains (for tests/serve):
	ACMEDirectory string // override ACME directory URL ("" = setting/Let's Encrypt)

// in handler(), before the "/" fallback — NO auth on the challenge path
// (nil-guarded: tests may build a server without a store/manager):
	if s.certs != nil {
		mux.Handle("GET "+acme.ChallengePath+"{token}", s.certs.Challenges())
	}

// exported starter for serve.go and tests:
// StartCerts launches the builtin cert manager loop when the provider is
// builtin; call in a goroutine-managing context (serve.go).
func (s *server) StartCerts(ctx context.Context) {
	if s.certProviderName() != "builtin" || s.kube == nil {
		return
	}
	go s.certs.Run(ctx)
}
```

Since `handler()` is built from `New`/`NewWithBackend`, tests reach `StartCerts` via the same `newServer` instance; `NewWithBackend` should now return the handler, backend, AND keep working — check whether exposing `*server` further is needed for serve.go: add `func NewFull(d Deps) (http.Handler, *PushBackend, *Server)`? NO — keep it simple: change `NewWithBackend` to also return a `StartCerts func(context.Context)` closure? Simplest consistent shape: add third return `start func(ctx context.Context)`:

```go
func NewWithBackend(d Deps) (http.Handler, *PushBackend, func(ctx context.Context)) {
	s := newServer(d)
	return s.handler(), &PushBackend{s: s}, s.StartCerts
}
```

and update `New` + the one call in `internal/cli/serve.go` (`handler, pushBackend, startCerts := server.NewWithBackend(...)`; call `startCerts(ctx)` after the signal context exists — Task 8 finalizes serve wiring; in THIS task just keep serve.go compiling by discarding the third value: `handler, pushBackend, _ :=`).

- [ ] **Step 4: Run** `go test ./internal/server/ ./internal/acme/ -v` — pass. Full `go test ./...` — green.
- [ ] **Step 5: Commit** — `feat: builtin cert manager — ACME issuance, renewal sweep, challenge routing`

---

### Task 7: CLI — domain commands + config set/get

**Files:**
- Modify: `internal/client/client.go`
- Create: `internal/cli/domain.go`
- Create: `internal/cli/config.go`
- Modify: `internal/cli/root.go` (register both)
- Modify: `internal/server/server.go` + create `internal/server/settings.go` (settings API)
- Test: `internal/cli/commands_test.go` (append), `internal/server/settings_test.go`

**Interfaces:**
- Consumes: Task 5 endpoints; existing `Client.do`, `apiClient()`, `adminOnly` middleware.
- Produces:
  - API: `GET /v1/settings/{key}` (admin) → `{"key":...,"value":...}` (404 unset); `PUT /v1/settings/{key}` body `{"value":"..."}` (admin) → 204. Allowed keys only: `cert_provider` (must be builtin|traefik|cert-manager), `acme_email`, `acme_directory` — anything else 400.
  - Client: `AddDomain(project, app, hostname string) (DomainInfo, error)`, `ListDomains(project, app string) ([]DomainInfo, error)`, `DeleteDomain(project, app, hostname string) error`, `RetryDomain(project, app, hostname string) error`, `GetSetting(key string) (string, error)`, `SetSetting(key, value string) error`. `type DomainInfo struct { Hostname, CertStatus, CertError, CertExpiresAt, DNSWarning string }` (json tags matching Task 5's response).
  - CLI: `luncur domain add <app> <hostname> --project P` (prints DNS warning when present), `domain list <app> --project P` (tabwriter: HOSTNAME/CERT/EXPIRES/ERROR), `domain remove <app> <hostname> --project P`, `domain retry <app> <hostname> --project P`; `luncur config set <key> <value>`, `luncur config get <key>`.

- [ ] **Step 1: Failing tests.**

`internal/server/settings_test.go`: admin can PUT+GET `cert_provider`; member gets 403; unknown key → 400; bad provider value → 400; unset key GET → 404. (Follow `users_test.go` patterns.)

Append to `internal/cli/commands_test.go`:

```go
func TestDomainAndConfigCommands(t *testing.T) {
	// testEnv + admin login as in the file's other tests; create project
	// "proj" + app "web" via the existing commands.
	// domain add web www.example.com --project proj → output contains
	//   "www.example.com" (a DNS warning line is fine — don't assert it).
	// domain list web --project proj → contains "www.example.com" and "none".
	// domain remove web www.example.com --project proj → list no longer
	//   contains it.
	// config set cert_provider traefik → ok; config get cert_provider →
	//   output contains "traefik".
	// config set cert_provider bogus → command returns an error.
}
```

(Real code, matching the file's `run` helper semantics.)

- [ ] **Step 2: Run** — failures.

- [ ] **Step 3: Implement.**

`internal/server/settings.go`:

```go
package server

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/sutantodadang/luncur/internal/store"
)

// settableKeys guards the settings API: only install-level knobs luncur
// understands, with per-key validation.
var settableKeys = map[string]func(string) bool{
	"cert_provider": func(v string) bool {
		return v == "builtin" || v == "traefik" || v == "cert-manager"
	},
	"acme_email":     func(v string) bool { return true },
	"acme_directory": func(v string) bool { return true },
}

func (s *server) handleGetSetting(w http.ResponseWriter, r *http.Request, _ store.User) {
	key := r.PathValue("key")
	if _, ok := settableKeys[key]; !ok {
		writeError(w, http.StatusBadRequest, "bad_request", "unknown setting")
		return
	}
	v, err := s.st.GetSetting(key)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "setting not set")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"key": key, "value": v})
}

func (s *server) handleSetSetting(w http.ResponseWriter, r *http.Request, _ store.User) {
	key := r.PathValue("key")
	valid, ok := settableKeys[key]
	if !ok {
		writeError(w, http.StatusBadRequest, "bad_request", "unknown setting")
		return
	}
	var req struct {
		Value string `json:"value"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || !valid(req.Value) {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid value")
		return
	}
	if err := s.st.SetSetting(key, req.Value); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
```

Routes (`server.go`, adminOnly):

```go
	mux.HandleFunc("GET /v1/settings/{key}", s.adminOnly(s.handleGetSetting))
	mux.HandleFunc("PUT /v1/settings/{key}", s.adminOnly(s.handleSetSetting))
```

`client.go` additions (same `do` helper):

```go
type DomainInfo struct {
	Hostname      string `json:"hostname"`
	CertStatus    string `json:"cert_status"`
	CertError     string `json:"cert_error"`
	CertExpiresAt string `json:"cert_expires_at"`
	DNSWarning    string `json:"dns_warning"`
}

func (c *Client) AddDomain(project, app, hostname string) (DomainInfo, error) {
	var out DomainInfo
	err := c.do("POST", fmt.Sprintf("/v1/projects/%s/apps/%s/domains", project, app),
		map[string]string{"hostname": hostname}, &out)
	return out, err
}

func (c *Client) ListDomains(project, app string) ([]DomainInfo, error) {
	var out []DomainInfo
	err := c.do("GET", fmt.Sprintf("/v1/projects/%s/apps/%s/domains", project, app), nil, &out)
	return out, err
}

func (c *Client) DeleteDomain(project, app, hostname string) error {
	return c.do("DELETE", fmt.Sprintf("/v1/projects/%s/apps/%s/domains/%s", project, app, hostname), nil, nil)
}

func (c *Client) RetryDomain(project, app, hostname string) error {
	return c.do("POST", fmt.Sprintf("/v1/projects/%s/apps/%s/domains/%s/retry", project, app, hostname), nil, nil)
}

func (c *Client) GetSetting(key string) (string, error) {
	var out struct {
		Value string `json:"value"`
	}
	err := c.do("GET", "/v1/settings/"+key, nil, &out)
	return out.Value, err
}

func (c *Client) SetSetting(key, value string) error {
	return c.do("PUT", "/v1/settings/"+key, map[string]string{"value": value}, nil)
}
```

`internal/cli/domain.go` — four subcommands in the established cobra style (mirror `sshkey.go`'s structure exactly): `add` prints `added <hostname>` then the `DNSWarning` on its own line when non-empty; `list` uses tabwriter columns `HOSTNAME\tCERT\tEXPIRES\tERROR`; `remove`/`retry` call through. All take one or two positional args plus required `--project`.

`internal/cli/config.go`:

```go
package cli

import "github.com/spf13/cobra"

func configCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Read or change install settings (admin)",
	}
	cmd.AddCommand(&cobra.Command{
		Use:   "get <key>",
		Short: "Read a setting",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient()
			if err != nil {
				return err
			}
			v, err := c.GetSetting(args[0])
			if err != nil {
				return err
			}
			cmd.Println(v)
			return nil
		},
	})
	cmd.AddCommand(&cobra.Command{
		Use:   "set <key> <value>",
		Short: "Change a setting",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient()
			if err != nil {
				return err
			}
			return c.SetSetting(args[0], args[1])
		},
	})
	return cmd
}
```

Register both in `root.go`: `domainCmd()`, `configCmd()`.

- [ ] **Step 4: Run** `go test ./internal/server/ ./internal/client/ ./internal/cli/ -v` — pass.
- [ ] **Step 5: Commit** — `feat: domain + config CLI, settings API`

---

### Task 8: providers — traefik HelmChartConfig, cert-manager ClusterIssuer, serve/up wiring

**Files:**
- Create: `internal/up/providers.go`
- Modify: `internal/cli/up.go` (`--cert-provider`, `--acme-email` flags; provider objects)
- Modify: `internal/cli/serve.go` (`--cert-provider` flag persisted to settings; `startCerts(ctx)` call; provider validation at boot)
- Test: `internal/up/providers_test.go`, `internal/cli/serve_test.go` (append flag test)

**Interfaces:**
- Consumes: `kube.Apply`, `kube.HasGroupVersion` (Task 4), `store.SetSetting`, `server.NewWithBackend` third return (Task 6).
- Produces:
  - `up.TraefikACMEConfig(email string) (render.Object, error)` — `HelmChartConfig` name `traefik`, namespace `kube-system`, `spec.valuesContent` YAML string enabling the `le` resolver:

```yaml
persistence:
  enabled: true
additionalArguments:
  - "--certificatesresolvers.le.acme.email=EMAIL"
  - "--certificatesresolvers.le.acme.storage=/data/acme.json"
  - "--certificatesresolvers.le.acme.httpchallenge.entrypoint=web"
```

  - `up.ClusterIssuer(email string) (render.Object, error)` — `cert-manager.io/v1` `ClusterIssuer` named `luncur-le`, ACME HTTP-01 with ingress class `traefik`, prod Let's Encrypt server, `privateKeySecretRef` name `luncur-le-account`.
  - `luncur up` gains `--cert-provider` (builtin|traefik|cert-manager, default builtin) and `--acme-email`; after the main objects apply: traefik → `HasGroupVersion("helm.cattle.io/v1")` must be true (else error naming `--kubeconfig`/non-K3s), apply `TraefikACMEConfig`; cert-manager → `HasGroupVersion("cert-manager.io/v1")` must be true (else error telling the user to install cert-manager), apply `ClusterIssuer`. The Deployment args gain `"--cert-provider", <value>` (extend `up.Params` with `CertProvider string`, default "builtin", threaded into the container args), and `--acme-email` becomes a `luncur config set acme_email` equivalent: pass `"--acme-email", <value>` arg to serve when set.
  - `luncur serve` gains `--cert-provider` (persist non-empty value to settings at boot BEFORE constructing the server), `--acme-email` (same), and calls the `startCerts` closure with the signal ctx. Boot validation: provider traefik/cert-manager without the matching GroupVersion → log a prominent warning (not fatal — kube may be temporarily down).

- [ ] **Step 1: Failing tests.**

`internal/up/providers_test.go`:

```go
func TestTraefikACMEConfig(t *testing.T) {
	o, err := TraefikACMEConfig("ops@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if o.Kind != "HelmChartConfig" {
		t.Fatalf("kind = %s", o.Kind)
	}
	s := string(o.JSON)
	for _, want := range []string{
		"kube-system", "certificatesresolvers.le.acme", "ops@example.com", "acme.json",
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("missing %q:\n%s", want, s)
		}
	}
}

func TestClusterIssuer(t *testing.T) {
	o, err := ClusterIssuer("ops@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if o.Kind != "ClusterIssuer" {
		t.Fatalf("kind = %s", o.Kind)
	}
	s := string(o.JSON)
	for _, want := range []string{
		"luncur-le", "http01", "traefik", "ops@example.com", "letsencrypt",
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("missing %q:\n%s", want, s)
		}
	}
}
```

`internal/cli/serve_test.go` — append to the flags test: `--cert-provider` default `""` and `--acme-email` exists. Also `internal/cli/up_test.go` if it asserts flag sets: `--cert-provider` default `"builtin"`.

- [ ] **Step 2: Run** — failures.

- [ ] **Step 3: Implement.** `internal/up/providers.go` builds both objects as `map[string]any` → `json.Marshal` → `render.Object` (unstructured — no typed structs exist for these CRDs). ClusterIssuer spec shape:

```go
	spec := map[string]any{
		"acme": map[string]any{
			"email":  email,
			"server": "https://acme-v02.api.letsencrypt.org/directory",
			"privateKeySecretRef": map[string]any{"name": "luncur-le-account"},
			"solvers": []map[string]any{{
				"http01": map[string]any{"ingress": map[string]any{"class": "traefik"}},
			}},
		},
	}
```

`up.go`: `Params` gains `CertProvider string`; Deployment args append `"--cert-provider", p.CertProvider` (and `"--acme-email", p.ACMEEmail` when non-empty — add `ACMEEmail string` to Params too); extend `internal/up/manifests_test.go` with a `"--cert-provider"` content assertion. `upCmd` flags + the post-apply provider steps with the exact error messages from the Interfaces block.

`serve.go`: flags `--cert-provider` + `--acme-email`; before `server.NewWithBackend`: 

```go
	if certProvider != "" {
		if err := st.SetSetting("cert_provider", certProvider); err != nil {
			return err
		}
	}
	if acmeEmail != "" {
		if err := st.SetSetting("acme_email", acmeEmail); err != nil {
			return err
		}
	}
```

then `handler, pushBackend, startCerts := server.NewWithBackend(...)` and after the signal context is created: `startCerts(ctx)`. Provider warning: after kube client construction, when provider is traefik/cert-manager and `kubeClient != nil`, call `HasGroupVersion` and `log.Printf("warning: ...")` when false.

- [ ] **Step 4: Run** `go build ./... && go vet ./... && go test ./...` — green.
- [ ] **Step 5: Commit** — `feat: traefik + cert-manager providers, serve/up cert wiring`

---

### Task 9: web UI domains + README + final verification

**Files:**
- Modify: `internal/server/ui.go` (domain rows in app view-model + add/remove handlers + routes)
- Modify: `internal/server/templates/app.html` (Domains section)
- Modify: `README.md`
- Test: `internal/server/ui_test.go` (append)

**Interfaces:**
- Consumes: Task 5 store/status logic, existing `uiPage`/`uiProject`/`uiApp` helpers and form-post patterns in `ui.go` (env set/unset show the exact shape to copy).
- Produces:
  - App page gains a "Domains" section: table (hostname, cert status with the existing `status-*` CSS classes — add `.status-issued{color:#080}.status-pending{color:#a60}.status-none{color:#666}` to `base.html`'s style block, reusing `status-failed`), remove button per row, add-domain form. DNS warning (returned by the same `dnsWarning` helper) rendered as a `.err` paragraph after add via redirect query param `?warn=` (URL-encoded, read and displayed once).
  - Routes: `POST /ui/projects/{project}/apps/{app}/domains` (form field `hostname`), `POST /ui/projects/{project}/apps/{app}/domains/delete` (form field `hostname`); both mirror the API handlers' store+sync calls (extract shared unexported helpers if the logic exceeds a few lines — e.g. `s.addDomain(ctx, p, a, hostname) (store.Domain, string, error)` used by BOTH the API and UI handler).
  - README: "Custom domains & TLS" section — `luncur domain add`, DNS pointing, the three providers (builtin default; `luncur up --cert-provider traefik|cert-manager`), `luncur config set acme_email you@example.com`, retry command; Plan F deviations added to the deviations list (challenge Ingress in luncur-system; provider fixed at startup); status line mentions Plan F.

- [ ] **Step 1: Failing test** (append to `ui_test.go`, following its login-flow fixtures): logged-in admin adds a domain via `POST /ui/projects/proj/apps/web/domains` (form) → 303 back to the app page; `GET` app page contains `www.example.com`; `POST .../domains/delete` removes it; app page no longer lists it.

- [ ] **Step 2: Run** — failures.

- [ ] **Step 3: Implement** per the Interfaces block. `app.html` section (before the Logs section):

```html
<h2>Domains</h2>
{{if .DNSWarning}}<p class="err">{{.DNSWarning}}</p>{{end}}
<table><tr><th>Hostname</th><th>Cert</th><th>Expires</th><th></th></tr>
{{range .Domains}}<tr>
  <td>{{.Hostname}}</td>
  <td class="status-{{.CertStatus}}">{{.CertStatus}}{{if .CertError}} — {{.CertError}}{{end}}</td>
  <td>{{.CertExpiresAt}}</td>
  <td><form class="inline" method="post" action="/ui/projects/{{$.Project.Name}}/apps/{{$.App.Name}}/domains/delete">
    <input type="hidden" name="hostname" value="{{.Hostname}}"><button type="submit">remove</button>
  </form></td>
</tr>{{end}}
</table>
<form method="post" action="/ui/projects/{{.Project.Name}}/apps/{{.App.Name}}/domains">
  <input name="hostname" placeholder="www.example.com" required>
  <button type="submit">add domain</button>
</form>
```

(App view-model gains `"Domains": domains, "DNSWarning": warn` entries.)

- [ ] **Step 4: Run** `go build ./... && go vet ./... && go test ./...` — green. `gofmt -l` — clean.
- [ ] **Step 5: Commit** — `feat: web UI domains + custom-domain docs`

---

## Final verification (after all tasks)

- [ ] `go build ./... && go vet ./... && go test ./...` — everything green.
- [ ] `gofmt -l internal/ cmd/` — clean.
- [ ] `grep -rn "Plan F" README.md internal/` — no stale references.
- [ ] Push branch `plan-f`, open PR against `main`.
- [ ] Manual (owner's VPS + real domain, post-merge): `luncur up`, `domain add`, watch cert go `pending → issued`, HTTPS loads; repeat with `--cert-provider traefik` and `cert-manager` installs.

## Spec-coverage self-check (Plan F section of 2026-07-03-luncur-phase2-design.md)

- Domain CRUD CLI + UI ✅ (T5/T7/T9); DNS warn-not-block ✅ (T5); sslip.io host stays ✅ (T5 render keeps `Host` first)
- `domains` cert columns ✅ (T1); `settings` table + `cert_provider` ✅ (T1/T7/T8)
- builtin: x/crypto/acme, account Secret, HTTP-01 via luncur, TLS Secrets `tls-<app>-<hash>`, renewal <30d, status in UI/CLI, `--acme-directory` equivalent (`acme_directory` setting + `Deps.ACMEDirectory` for tests) ✅ (T3/T6)
- traefik: HelmChartConfig + PVC persistence + annotation, K3s-only check, `external` status ✅ (T5/T8) — `external` status set is implicit (no builtin manager running); UI shows domain rows with status none/`external`? → T5's renderer doesn't depend on it; ensure `handleAddDomain` sets status `external` when provider != builtin (add that line in T5: after AddDomain, `if s.certProviderName() != "builtin" { s.st.SetDomainCert(d.ID, "external", "", ""); d.CertStatus = "external" }`).
- cert-manager: CRD check, ClusterIssuer luncur-le, annotation, expiry from Secret (deferred: UI shows `external`; reading Secret expiry is a nice-to-have — record as Phase-2 backlog note in README) ⚠ partial, documented.
- Retry: `luncur domain retry` + API ✅ (T5/T7)
- Error handling: failed status + message, app keeps serving HTTP ✅ (T6)
