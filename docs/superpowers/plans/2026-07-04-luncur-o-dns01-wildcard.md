# luncur Plan O — DNS-01 + Wildcard Certs Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Pluggable DNS providers (Cloudflare / Route53 / RFC2136), a DNS-01 ACME issuance path, and `*.example.com` wildcard domains.

**Architecture:** New `internal/dns` package with a two-method `Provider` interface and three impls (Cloudflare REST/JSON, Route53 XML signed by a SigV4 signer extracted from `internal/s3` into `internal/awssig`, RFC2136 shelling out to `nsupdate` via a fakeable `Runner`). `internal/acme.Issuer` grows a pluggable `Solver`; the existing HTTP-01 challenge store becomes the default solver and a `DNS01Solver` presents `base64url(sha256(keyAuth))` TXT records and polls for propagation. The builtin cert manager picks DNS-01 when the hostname is a wildcard OR `dns_provider != none`; `traefik`/`cert-manager` are untouched. The server builds the provider from sealed settings behind an injectable `dnsProvider` factory field (same seam pattern as `mailer`).

**Tech Stack:** Go stdlib (`net/http`, `encoding/json`, `encoding/xml`, `crypto/sha256`, `os/exec`) + existing `golang.org/x/crypto/acme`. No new Go module dependencies. `nsupdate` (bind-tools) added to the release image, used only when the RFC2136 provider is selected — a runtime binary like `git`/`pg_dump`, per the spec's documented deviation.

**Branch:** `plan-o` off `main`.

## Global Constraints (from Phase 4 spec)

- Single Go module, one binary from `cmd/luncur`. **No new Go module dependencies.** `nsupdate` is a runtime binary in the server image, gated on the RFC2136 provider (documented deviation).
- Settings: `dns_provider` = `cloudflare`|`route53`|`rfc2136`|`none`, default none. Sealed write-only creds: `dns_cloudflare_token`, `dns_route53_secret_key`, `dns_rfc2136_tsig_secret`. Plain: `dns_route53_access_key`, `dns_route53_region`, `dns_rfc2136_server`, `dns_rfc2136_tsig_name`, `dns_rfc2136_tsig_algo`.
- `fqdn` passed to providers is `_acme-challenge.<domain>`; value is the TXT contents.
- DNS-01 picked when hostname has a leading `*.` OR `dns_provider != none`; otherwise HTTP-01 as today. `traefik`/`cert-manager` unchanged.
- `AddDomain` accepts `*.example.com` (one leading `*.`, remainder a normal hostname). Wildcard + `dns_provider == none` → 400. UI/CLI surface unchanged.
- Errors: provider API error or propagation timeout → domain `cert_status` `failed` + message (same as HTTP-01 failures).
- Tests: no cluster, network, or real DNS — httptest fakes (Cloudflare JSON, Route53 XML), `Runner` fake (nsupdate), extended fake ACME directory + fake resolver for DNS-01, SigV4 extraction re-runs Plan K's known-answer vector.
- Conventional commits. Before **every** commit: `go build ./... && go vet ./... && go test ./...` — all green.

---

### Task 1: extract SigV4 into `internal/awssig`

**Files:**
- Create: `internal/awssig/awssig.go`, `internal/awssig/awssig_test.go`
- Modify: `internal/s3/s3.go` (sign becomes a thin wrapper; drop the moved helpers)

**Interfaces:**
- Consumes: the current `sign`/`hmacSHA256`/`sha256hex` bodies in `internal/s3/s3.go:104-165`.
- Produces: `awssig.Sign(req *http.Request, accessKey, secretKey, region, service, payloadHash string, t time.Time)` and `awssig.HashPayload(b []byte) string` (hex sha256). Task 3's Route53 impl signs with `service="route53"` and a real payload hash; s3 keeps `service="s3"`, `payloadHash="UNSIGNED-PAYLOAD"`.

- [ ] **Step 1: Create branch**

```bash
git checkout -b plan-o
```

- [ ] **Step 2: Write the failing test**

Create `internal/awssig/awssig_test.go` — the Plan K known-answer vector, re-run through the extracted signer (same request, time, creds as `internal/s3/s3_test.go:TestSignKnownAnswer`; copy the exact expected Authorization header from that test — open the file and paste its `want` constant):

```go
package awssig

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestSignKnownAnswerS3 re-runs Plan K's SigV4 known-answer vector through
// the extracted signer: same request, creds, and timestamp as
// internal/s3's TestSignKnownAnswer, so the expected header is copied
// verbatim from that test.
func TestSignKnownAnswerS3(t *testing.T) {
	req, _ := http.NewRequest("PUT", "https://s3.example.com/bucket/backups/a.tar.gz", strings.NewReader("hi"))
	ts := time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)
	Sign(req, "AKIDEXAMPLE", "SECRET", "us-east-1", "s3", "UNSIGNED-PAYLOAD", ts)

	// <<< copy the exact `want` string from internal/s3/s3_test.go
	// TestSignKnownAnswer before running; the two tests must pin the SAME
	// header. Fill in the credentials scope accordingly. >>>
	got := req.Header.Get("Authorization")
	if !strings.HasPrefix(got, "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20260703/us-east-1/s3/aws4_request") {
		t.Fatalf("authorization = %q", got)
	}
	if !strings.Contains(got, "SignedHeaders=host;x-amz-content-sha256;x-amz-date") {
		t.Fatalf("authorization = %q", got)
	}
}

// TestSignServiceChangesSignature: a different service yields a different
// scope and signature (route53 vs s3).
func TestSignServiceChangesSignature(t *testing.T) {
	ts := time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)
	r1, _ := http.NewRequest("POST", "https://route53.amazonaws.com/2013-04-01/hostedzone/Z1/rrset", strings.NewReader("<xml/>"))
	Sign(r1, "AK", "SK", "us-east-1", "route53", HashPayload([]byte("<xml/>")), ts)
	if !strings.Contains(r1.Header.Get("Authorization"), "/route53/aws4_request") {
		t.Fatalf("scope missing route53: %q", r1.Header.Get("Authorization"))
	}
	if r1.Header.Get("x-amz-content-sha256") == "UNSIGNED-PAYLOAD" {
		t.Fatal("route53 request must carry a real payload hash")
	}
}
```

NOTE for the implementer: before running, open `internal/s3/s3_test.go` `TestSignKnownAnswer`, copy its exact expected Authorization value and creds into `TestSignKnownAnswerS3` so the vector is pinned bit-for-bit (the skeleton above only asserts prefix/headers; upgrade it to a full equality check using that value, adjusting creds to the s3 test's).

- [ ] **Step 3: Run test to verify it fails**

Run: `go test ./internal/awssig/ -v`
Expected: FAIL — package does not exist.

- [ ] **Step 4: Implement**

Create `internal/awssig/awssig.go` by MOVING the body of `sign` (and `hmacSHA256`, `sha256hex`) from `internal/s3/s3.go`, generalized over service + payload hash:

```go
// Package awssig implements AWS Signature Version 4 for single-chunk
// requests. Shared by the s3 client (UNSIGNED-PAYLOAD) and the Route53
// DNS provider (hashed XML payload).
package awssig

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// HashPayload returns the hex SHA-256 of a request body, for services
// that require a signed payload hash.
func HashPayload(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Sign signs req in place. payloadHash is the hex SHA-256 of the request
// body, or the literal "UNSIGNED-PAYLOAD" (S3 only). Signed headers:
// host, x-amz-content-sha256, x-amz-date.
func Sign(req *http.Request, accessKey, secretKey, region, service, payloadHash string, t time.Time) {
	amzDate := t.Format("20060102T150405Z")
	shortDate := t.Format("20060102")

	req.Header.Set("x-amz-date", amzDate)
	req.Header.Set("x-amz-content-sha256", payloadHash)

	host := req.Host
	if host == "" {
		host = req.URL.Host
	}

	canonicalHeaders := "host:" + host + "\n" +
		"x-amz-content-sha256:" + payloadHash + "\n" +
		"x-amz-date:" + amzDate + "\n"
	signedHeaders := "host;x-amz-content-sha256;x-amz-date"

	canonicalRequest := strings.Join([]string{
		req.Method,
		req.URL.EscapedPath(),
		req.URL.RawQuery,
		canonicalHeaders,
		signedHeaders,
		payloadHash,
	}, "\n")

	scope := strings.Join([]string{shortDate, region, service, "aws4_request"}, "/")
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256",
		amzDate,
		scope,
		sha256hex(canonicalRequest),
	}, "\n")

	signingKey := hmacSHA256(
		hmacSHA256(
			hmacSHA256(
				hmacSHA256([]byte("AWS4"+secretKey), []byte(shortDate)),
				[]byte(region)),
			[]byte(service)),
		[]byte("aws4_request"))
	signature := hex.EncodeToString(hmacSHA256(signingKey, []byte(stringToSign)))

	req.Header.Set("Authorization", fmt.Sprintf(
		"AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		accessKey, scope, signedHeaders, signature))
}

func hmacSHA256(key, data []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write(data)
	return h.Sum(nil)
}

func sha256hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
```

In `internal/s3/s3.go`: delete `hmacSHA256`, `sha256hex`, and the body of `sign`; replace with

```go
// sign delegates to the shared SigV4 signer with S3's UNSIGNED-PAYLOAD.
func sign(req *http.Request, accessKey, secretKey, region string, t time.Time) {
	awssig.Sign(req, accessKey, secretKey, region, "s3", "UNSIGNED-PAYLOAD", t)
}
```

and add the `github.com/sutantodadang/luncur/internal/awssig` import; drop the now-unused `crypto/hmac`, `crypto/sha256`, `encoding/hex` imports.

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./internal/awssig/ ./internal/s3/ -v`
Expected: PASS — including s3's original `TestSignKnownAnswer` (the wrapper must produce a byte-identical header).

- [ ] **Step 6: Full verify + commit**

```bash
go build ./... && go vet ./... && go test ./...
git add internal/awssig/ internal/s3/
git commit -m "refactor: extract SigV4 signer to internal/awssig"
```

---

### Task 2: `internal/dns` — Provider interface + Cloudflare

**Files:**
- Create: `internal/dns/dns.go`, `internal/dns/cloudflare.go`
- Test: `internal/dns/cloudflare_test.go`

**Interfaces:**
- Produces: `type Provider interface { Present(ctx context.Context, fqdn, value string) error; CleanUp(ctx context.Context, fqdn, value string) error }`; `type Cloudflare struct { Token, BaseURL string; HTTPClient *http.Client }` (BaseURL default `https://api.cloudflare.com/client/v4`). Tasks 5–7 consume `Provider`.

- [ ] **Step 1: Write the failing test**

Create `internal/dns/cloudflare_test.go`:

```go
package dns

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeCF implements just enough of the Cloudflare v4 API: zone lookup by
// name, TXT record create/list/delete.
func fakeCF(t *testing.T) (*httptest.Server, *[]string) {
	t.Helper()
	var log []string
	mux := http.NewServeMux()
	mux.HandleFunc("GET /zones", func(w http.ResponseWriter, r *http.Request) {
		name := r.URL.Query().Get("name")
		log = append(log, "zones?"+name)
		if r.Header.Get("Authorization") != "Bearer tok123" {
			http.Error(w, `{"success":false}`, http.StatusForbidden)
			return
		}
		if name == "example.com" {
			w.Write([]byte(`{"success":true,"result":[{"id":"z1","name":"example.com"}]}`))
			return
		}
		w.Write([]byte(`{"success":true,"result":[]}`))
	})
	mux.HandleFunc("POST /zones/z1/dns_records", func(w http.ResponseWriter, r *http.Request) {
		var rec struct {
			Type, Name, Content string
		}
		json.NewDecoder(r.Body).Decode(&rec)
		log = append(log, "create "+rec.Type+" "+rec.Name+" "+rec.Content)
		w.Write([]byte(`{"success":true,"result":{"id":"r1"}}`))
	})
	mux.HandleFunc("GET /zones/z1/dns_records", func(w http.ResponseWriter, r *http.Request) {
		log = append(log, "list "+r.URL.Query().Get("name"))
		w.Write([]byte(`{"success":true,"result":[{"id":"r1","type":"TXT","name":"_acme-challenge.www.example.com","content":"txtval"}]}`))
	})
	mux.HandleFunc("DELETE /zones/z1/dns_records/r1", func(w http.ResponseWriter, r *http.Request) {
		log = append(log, "delete r1")
		w.Write([]byte(`{"success":true}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &log
}

func TestCloudflarePresentCleanUp(t *testing.T) {
	srv, log := fakeCF(t)
	cf := &Cloudflare{Token: "tok123", BaseURL: srv.URL}

	if err := cf.Present(context.Background(), "_acme-challenge.www.example.com", "txtval"); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(*log, "|")
	// Longest-suffix zone walk: tries the full name first, lands on example.com.
	if !strings.Contains(joined, "zones?example.com") {
		t.Fatalf("zone walk missing: %s", joined)
	}
	if !strings.Contains(joined, "create TXT _acme-challenge.www.example.com txtval") {
		t.Fatalf("create missing: %s", joined)
	}

	if err := cf.CleanUp(context.Background(), "_acme-challenge.www.example.com", "txtval"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(*log, "|"), "delete r1") {
		t.Fatalf("delete missing: %v", *log)
	}
}

func TestCloudflareNoZone(t *testing.T) {
	srv, _ := fakeCF(t)
	cf := &Cloudflare{Token: "tok123", BaseURL: srv.URL}
	if err := cf.Present(context.Background(), "_acme-challenge.other.net", "v"); err == nil ||
		!strings.Contains(err.Error(), "no zone") {
		t.Fatalf("want no-zone error, got %v", err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/dns/ -v`
Expected: FAIL — package does not exist.

- [ ] **Step 3: Implement**

Create `internal/dns/dns.go`:

```go
// Package dns abstracts DNS record management for ACME DNS-01 challenges.
// fqdn is always the full challenge name (_acme-challenge.<domain>);
// value is the TXT record contents.
package dns

import "context"

// Provider creates and removes one TXT record.
type Provider interface {
	Present(ctx context.Context, fqdn, value string) error
	CleanUp(ctx context.Context, fqdn, value string) error
}
```

Create `internal/dns/cloudflare.go`:

```go
package dns

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// Cloudflare manages TXT records via the v4 REST API with a bearer token.
type Cloudflare struct {
	Token      string
	BaseURL    string // default https://api.cloudflare.com/client/v4
	HTTPClient *http.Client
}

func (c *Cloudflare) base() string {
	if c.BaseURL != "" {
		return strings.TrimRight(c.BaseURL, "/")
	}
	return "https://api.cloudflare.com/client/v4"
}

func (c *Cloudflare) client() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	return http.DefaultClient
}

// do performs one authenticated API call and decodes the standard
// {"success":..,"result":..} envelope into result (may be nil).
func (c *Cloudflare) do(ctx context.Context, method, path string, body, result any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base()+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	var env struct {
		Success bool            `json:"success"`
		Result  json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(raw, &env); err != nil || !env.Success {
		return fmt.Errorf("cloudflare %s %s: %d %s", method, path, resp.StatusCode, truncate(raw))
	}
	if result != nil {
		return json.Unmarshal(env.Result, result)
	}
	return nil
}

func truncate(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 256 {
		return s[:256]
	}
	return s
}

// zoneID resolves the zone containing fqdn by longest-suffix match:
// walk the label suffixes from longest to shortest and take the first
// zone the API returns.
func (c *Cloudflare) zoneID(ctx context.Context, fqdn string) (string, error) {
	labels := strings.Split(strings.TrimSuffix(fqdn, "."), ".")
	for i := 0; i <= len(labels)-2; i++ {
		cand := strings.Join(labels[i:], ".")
		var zones []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		}
		if err := c.do(ctx, http.MethodGet, "/zones?name="+url.QueryEscape(cand), nil, &zones); err != nil {
			return "", err
		}
		if len(zones) > 0 {
			return zones[0].ID, nil
		}
	}
	return "", fmt.Errorf("cloudflare: no zone found for %s", fqdn)
}

func (c *Cloudflare) Present(ctx context.Context, fqdn, value string) error {
	zone, err := c.zoneID(ctx, fqdn)
	if err != nil {
		return err
	}
	rec := map[string]any{"type": "TXT", "name": fqdn, "content": value, "ttl": 60}
	return c.do(ctx, http.MethodPost, "/zones/"+zone+"/dns_records", rec, nil)
}

func (c *Cloudflare) CleanUp(ctx context.Context, fqdn, value string) error {
	zone, err := c.zoneID(ctx, fqdn)
	if err != nil {
		return err
	}
	var recs []struct {
		ID      string `json:"id"`
		Content string `json:"content"`
	}
	q := "/zones/" + zone + "/dns_records?type=TXT&name=" + url.QueryEscape(fqdn)
	if err := c.do(ctx, http.MethodGet, q, nil, &recs); err != nil {
		return err
	}
	for _, r := range recs {
		if r.Content != value {
			continue
		}
		if err := c.do(ctx, http.MethodDelete, "/zones/"+zone+"/dns_records/"+r.ID, nil, nil); err != nil {
			return err
		}
	}
	return nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/dns/ -v`
Expected: PASS.

NOTE: the fake's mux patterns (`GET /zones` etc.) require the query string be excluded from the pattern — Go 1.22 ServeMux matches path only; this works as written. The zone walk for `_acme-challenge.other.net` queries `_acme-challenge.other.net` then `other.net`, both returning empty → "no zone".

- [ ] **Step 5: Full verify + commit**

```bash
go build ./... && go vet ./... && go test ./...
git add internal/dns/
git commit -m "feat: dns provider interface + Cloudflare impl"
```

---

### Task 3: `internal/dns` — Route53

**Files:**
- Create: `internal/dns/route53.go`
- Test: `internal/dns/route53_test.go`

**Interfaces:**
- Consumes: `awssig.Sign`, `awssig.HashPayload` (Task 1), `Provider` (Task 2).
- Produces: `type Route53 struct { AccessKey, SecretKey, Region, BaseURL string; HTTPClient *http.Client; Now func() time.Time }` implementing `Provider` (BaseURL default `https://route53.amazonaws.com`; Region defaults to `us-east-1` when empty — Route53 is a global service signed against us-east-1 unless overridden).

- [ ] **Step 1: Write the failing test**

Create `internal/dns/route53_test.go`:

```go
package dns

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func fakeR53(t *testing.T) (*httptest.Server, *[]string) {
	t.Helper()
	var log []string
	mux := http.NewServeMux()
	mux.HandleFunc("GET /2013-04-01/hostedzone", func(w http.ResponseWriter, r *http.Request) {
		name := r.URL.Query().Get("dnsname")
		log = append(log, "zones?"+name)
		if r.Header.Get("Authorization") == "" {
			http.Error(w, "unsigned", http.StatusForbidden)
			return
		}
		if name == "example.com" {
			w.Write([]byte(`<?xml version="1.0"?>
<ListHostedZonesByNameResponse><HostedZones><HostedZone>
  <Id>/hostedzone/Z123</Id><Name>example.com.</Name>
</HostedZone></HostedZones></ListHostedZonesByNameResponse>`))
			return
		}
		w.Write([]byte(`<?xml version="1.0"?><ListHostedZonesByNameResponse><HostedZones></HostedZones></ListHostedZonesByNameResponse>`))
	})
	mux.HandleFunc("POST /2013-04-01/hostedzone/Z123/rrset", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		log = append(log, "change "+string(body))
		w.Write([]byte(`<?xml version="1.0"?><ChangeResourceRecordSetsResponse/>`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &log
}

func TestRoute53PresentCleanUp(t *testing.T) {
	srv, log := fakeR53(t)
	r53 := &Route53{AccessKey: "AK", SecretKey: "SK", BaseURL: srv.URL}

	if err := r53.Present(context.Background(), "_acme-challenge.www.example.com", "txtval"); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(*log, "|")
	if !strings.Contains(joined, "zones?example.com") {
		t.Fatalf("zone walk missing: %s", joined)
	}
	if !strings.Contains(joined, "<Action>UPSERT</Action>") ||
		!strings.Contains(joined, "<Name>_acme-challenge.www.example.com.</Name>") ||
		!strings.Contains(joined, `<Value>&#34;txtval&#34;</Value>`) {
		t.Fatalf("upsert body wrong: %s", joined)
	}

	if err := r53.CleanUp(context.Background(), "_acme-challenge.www.example.com", "txtval"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(*log, "|"), "<Action>DELETE</Action>") {
		t.Fatalf("delete missing: %v", *log)
	}
}

func TestRoute53NoZone(t *testing.T) {
	srv, _ := fakeR53(t)
	r53 := &Route53{AccessKey: "AK", SecretKey: "SK", BaseURL: srv.URL}
	if err := r53.Present(context.Background(), "_acme-challenge.nope.net", "v"); err == nil ||
		!strings.Contains(err.Error(), "no hosted zone") {
		t.Fatalf("want no-zone error, got %v", err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/dns/ -run TestRoute53 -v`
Expected: FAIL — `Route53` undefined.

- [ ] **Step 3: Implement**

Create `internal/dns/route53.go`:

```go
package dns

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/sutantodadang/luncur/internal/awssig"
)

// Route53 manages TXT records via ChangeResourceRecordSets, signed with
// the shared SigV4 signer (XML request/response, stdlib only).
type Route53 struct {
	AccessKey  string
	SecretKey  string
	Region     string // signing region, default us-east-1 (Route53 is global)
	BaseURL    string // default https://route53.amazonaws.com
	HTTPClient *http.Client
	Now        func() time.Time
}

func (r *Route53) base() string {
	if r.BaseURL != "" {
		return strings.TrimRight(r.BaseURL, "/")
	}
	return "https://route53.amazonaws.com"
}

func (r *Route53) region() string {
	if r.Region != "" {
		return r.Region
	}
	return "us-east-1"
}

func (r *Route53) client() *http.Client {
	if r.HTTPClient != nil {
		return r.HTTPClient
	}
	return http.DefaultClient
}

func (r *Route53) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *Route53) do(ctx context.Context, method, path string, body []byte, out any) error {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, r.base()+path, rd)
	if err != nil {
		return err
	}
	hash := awssig.HashPayload(body)
	awssig.Sign(req, r.AccessKey, r.SecretKey, r.region(), "route53", hash, r.now().UTC())
	resp, err := r.client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		return fmt.Errorf("route53 %s %s: %d %s", method, path, resp.StatusCode, truncate(raw))
	}
	if out != nil {
		return xml.Unmarshal(raw, out)
	}
	return nil
}

// zoneID resolves the hosted zone containing fqdn by longest-suffix match
// via ListHostedZonesByName.
func (r *Route53) zoneID(ctx context.Context, fqdn string) (string, error) {
	labels := strings.Split(strings.TrimSuffix(fqdn, "."), ".")
	for i := 0; i <= len(labels)-2; i++ {
		cand := strings.Join(labels[i:], ".")
		var out struct {
			HostedZones struct {
				HostedZone []struct {
					Id   string `xml:"Id"`
					Name string `xml:"Name"`
				} `xml:"HostedZone"`
			} `xml:"HostedZones"`
		}
		path := "/2013-04-01/hostedzone?dnsname=" + url.QueryEscape(cand) + "&maxitems=1"
		if err := r.do(ctx, http.MethodGet, path, nil, &out); err != nil {
			return "", err
		}
		for _, z := range out.HostedZones.HostedZone {
			if strings.TrimSuffix(z.Name, ".") == cand {
				return strings.TrimPrefix(z.Id, "/hostedzone/"), nil
			}
		}
	}
	return "", fmt.Errorf("route53: no hosted zone found for %s", fqdn)
}

type r53Change struct {
	XMLName xml.Name `xml:"https://route53.amazonaws.com/doc/2013-04-01/ ChangeResourceRecordSetsRequest"`
	Changes []struct {
		Action string `xml:"Action"`
		RRSet  struct {
			Name string `xml:"Name"`
			Type string `xml:"Type"`
			TTL  int    `xml:"TTL"`
			Recs struct {
				Records []struct {
					Value string `xml:"Value"`
				} `xml:"ResourceRecord"`
			} `xml:"ResourceRecords"`
		} `xml:"ResourceRecordSet"`
	} `xml:"ChangeBatch>Changes>Change"`
}

func (r *Route53) change(ctx context.Context, action, fqdn, value string) error {
	zone, err := r.zoneID(ctx, fqdn)
	if err != nil {
		return err
	}
	var req r53Change
	req.Changes = make([]struct {
		Action string `xml:"Action"`
		RRSet  struct {
			Name string `xml:"Name"`
			Type string `xml:"Type"`
			TTL  int    `xml:"TTL"`
			Recs struct {
				Records []struct {
					Value string `xml:"Value"`
				} `xml:"ResourceRecord"`
			} `xml:"ResourceRecords"`
		} `xml:"ResourceRecordSet"`
	}, 1)
	c := &req.Changes[0]
	c.Action = action
	c.RRSet.Name = strings.TrimSuffix(fqdn, ".") + "."
	c.RRSet.Type = "TXT"
	c.RRSet.TTL = 60
	c.RRSet.Recs.Records = []struct {
		Value string `xml:"Value"`
	}{{Value: `"` + value + `"`}}

	body, err := xml.Marshal(req)
	if err != nil {
		return err
	}
	return r.do(ctx, http.MethodPost, "/2013-04-01/hostedzone/"+zone+"/rrset", body, nil)
}

func (r *Route53) Present(ctx context.Context, fqdn, value string) error {
	return r.change(ctx, "UPSERT", fqdn, value)
}

func (r *Route53) CleanUp(ctx context.Context, fqdn, value string) error {
	return r.change(ctx, "DELETE", fqdn, value)
}
```

NOTE: if the anonymous-struct XML plumbing fights you, define named structs (`r53RRSet`, `r53Record`, …) — any shape is fine as long as the marshaled body contains `<Action>`, `<Name>`, `<Type>TXT</Type>`, `<TTL>60</TTL>`, and the quoted `<Value>`; adjust the test's assertions only if the XML escaping of `&#34;` differs.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/dns/ -v`
Expected: PASS (Cloudflare + Route53).

- [ ] **Step 5: Full verify + commit**

```bash
go build ./... && go vet ./... && go test ./...
git add internal/dns/route53.go internal/dns/route53_test.go
git commit -m "feat: Route53 dns provider — SigV4-signed XML rrset changes"
```

---

### Task 4: `internal/dns` — RFC2136 via nsupdate

**Files:**
- Create: `internal/dns/rfc2136.go`
- Test: `internal/dns/rfc2136_test.go`

**Interfaces:**
- Produces: `type Runner interface { Run(ctx context.Context, stdin string, args ...string) error }`; `type RFC2136 struct { Server, TSIGName, TSIGSecret, TSIGAlgo string; Runner Runner }` implementing `Provider`. The TSIG secret rides stdin (a `key` script line), never argv.

- [ ] **Step 1: Write the failing test**

Create `internal/dns/rfc2136_test.go`:

```go
package dns

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

type fakeRunner struct {
	stdins []string
	args   [][]string
	err    error
}

func (f *fakeRunner) Run(ctx context.Context, stdin string, args ...string) error {
	f.stdins = append(f.stdins, stdin)
	f.args = append(f.args, args)
	return f.err
}

func TestRFC2136PresentCleanUp(t *testing.T) {
	fr := &fakeRunner{}
	p := &RFC2136{Server: "ns1.example.com", TSIGName: "luncur-key", TSIGSecret: "c2VjcmV0", Runner: fr}

	if err := p.Present(context.Background(), "_acme-challenge.www.example.com", "txtval"); err != nil {
		t.Fatal(err)
	}
	if len(fr.stdins) != 1 || fr.args[0][0] != "nsupdate" {
		t.Fatalf("runner calls: %+v", fr)
	}
	script := fr.stdins[0]
	for _, want := range []string{
		"server ns1.example.com",
		"key hmac-sha256:luncur-key c2VjcmV0", // default algo
		`update add _acme-challenge.www.example.com. 60 TXT "txtval"`,
		"send",
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("script missing %q:\n%s", want, script)
		}
	}
	// Secret never on argv.
	if strings.Contains(strings.Join(fr.args[0], " "), "c2VjcmV0") {
		t.Fatal("TSIG secret leaked to argv")
	}

	if err := p.CleanUp(context.Background(), "_acme-challenge.www.example.com", "txtval"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(fr.stdins[1], `update delete _acme-challenge.www.example.com. TXT "txtval"`) {
		t.Fatalf("delete script:\n%s", fr.stdins[1])
	}
}

func TestRFC2136CustomAlgoAndError(t *testing.T) {
	fr := &fakeRunner{err: fmt.Errorf("SERVFAIL")}
	p := &RFC2136{Server: "ns1", TSIGName: "k", TSIGSecret: "s", TSIGAlgo: "hmac-sha512", Runner: fr}
	err := p.Present(context.Background(), "_acme-challenge.x.io", "v")
	if err == nil || !strings.Contains(err.Error(), "SERVFAIL") {
		t.Fatalf("want runner error, got %v", err)
	}
	if !strings.Contains(fr.stdins[0], "key hmac-sha512:k s") {
		t.Fatalf("custom algo missing:\n%s", fr.stdins[0])
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/dns/ -run TestRFC2136 -v`
Expected: FAIL — `RFC2136` undefined.

- [ ] **Step 3: Implement**

Create `internal/dns/rfc2136.go`:

```go
package dns

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// Runner executes an external command with a stdin script. Faked in tests;
// the real one shells out (nsupdate).
type Runner interface {
	Run(ctx context.Context, stdin string, args ...string) error
}

// ExecRunner runs the command for real.
type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, stdin string, args ...string) error {
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %v: %s", args[0], err, bytes.TrimSpace(out))
	}
	return nil
}

// RFC2136 updates TXT records by piping an nsupdate script (server, TSIG
// key, update add/delete) to the nsupdate binary. The TSIG secret rides
// stdin's `key` line — never argv, which would be visible in `ps`.
type RFC2136 struct {
	Server     string
	TSIGName   string
	TSIGSecret string
	TSIGAlgo   string // default hmac-sha256
	Runner     Runner // default ExecRunner
}

func (p *RFC2136) algo() string {
	if p.TSIGAlgo != "" {
		return p.TSIGAlgo
	}
	return "hmac-sha256"
}

func (p *RFC2136) runner() Runner {
	if p.Runner != nil {
		return p.Runner
	}
	return ExecRunner{}
}

func (p *RFC2136) run(ctx context.Context, update string) error {
	script := "server " + p.Server + "\n" +
		"key " + p.algo() + ":" + p.TSIGName + " " + p.TSIGSecret + "\n" +
		update + "\n" +
		"send\n"
	return p.runner().Run(ctx, script, "nsupdate")
}

func (p *RFC2136) Present(ctx context.Context, fqdn, value string) error {
	return p.run(ctx, fmt.Sprintf("update add %s. 60 TXT %q", strings.TrimSuffix(fqdn, "."), value))
}

func (p *RFC2136) CleanUp(ctx context.Context, fqdn, value string) error {
	return p.run(ctx, fmt.Sprintf("update delete %s. TXT %q", strings.TrimSuffix(fqdn, "."), value))
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/dns/ -v`
Expected: PASS (all three providers).

- [ ] **Step 5: Full verify + commit**

```bash
go build ./... && go vet ./... && go test ./...
git add internal/dns/rfc2136.go internal/dns/rfc2136_test.go
git commit -m "feat: RFC2136 dns provider — nsupdate via fakeable Runner"
```

---

### Task 5: dns settings + server provider factory

**Files:**
- Modify: `internal/server/settings.go` (settableKeys + sealedKeys)
- Create: `internal/server/dnsprovider.go`
- Modify: `internal/server/server.go` (injectable `dnsProvider` field, wired in `newServer`)
- Test: `internal/server/settings_test.go`, `internal/server/dnsprovider_test.go`

**Interfaces:**
- Consumes: `dns.Cloudflare/Route53/RFC2136` (Tasks 2–4), `s.sealedSetting`, `sealedKeys`, `settableKeys`.
- Produces: settings keys per Global Constraints; `errNoDNS` sentinel; `func (s *server) dnsProviderName() string` (default "none"); `func (s *server) dnsProviderFromSettings() (dns.Provider, error)`; server field `dnsProvider func() (dns.Provider, error)` (tests override — same seam as `mailer`). Task 7 consumes `dnsProviderName` + `s.dnsProvider`.

- [ ] **Step 1: Write the failing tests**

Append to `internal/server/settings_test.go`:

```go
// TestSettingsDNSKeys: provider enum enforced; a sealed dns cred masks.
func TestSettingsDNSKeys(t *testing.T) {
	sealer, err := secret.New(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	st := newTestStore(t)
	srv := newHTTPTest(t, Deps{Store: st, Sealer: sealer})
	admin := seedUserToken(t, st, "root@b.co", "admin")

	resp := doAuthed(t, "PUT", srv.URL+"/v1/settings/dns_provider", admin, `{"value":"gandi"}`)
	if resp.StatusCode != 400 {
		t.Fatalf("bad provider: want 400, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	for _, v := range []string{"cloudflare", "route53", "rfc2136", "none"} {
		resp = doAuthed(t, "PUT", srv.URL+"/v1/settings/dns_provider", admin, `{"value":"`+v+`"}`)
		if resp.StatusCode != 204 {
			t.Fatalf("provider %s: want 204, got %d", v, resp.StatusCode)
		}
		resp.Body.Close()
	}

	resp = doAuthed(t, "PUT", srv.URL+"/v1/settings/dns_cloudflare_token", admin, `{"value":"tok"}`)
	if resp.StatusCode != 204 {
		t.Fatalf("put token: want 204, got %d", resp.StatusCode)
	}
	resp.Body.Close()
	resp = doAuthed(t, "GET", srv.URL+"/v1/settings/dns_cloudflare_token", admin, "")
	var out struct {
		Value string `json:"value"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if out.Value != "(set)" {
		t.Fatalf("token read = %q, want (set)", out.Value)
	}
}
```

Create `internal/server/dnsprovider_test.go`:

```go
package server

import (
	"errors"
	"net/http/httptest"
	"testing"

	"github.com/sutantodadang/luncur/internal/dns"
	"github.com/sutantodadang/luncur/internal/secret"
)

// dnsSettingsServer builds a *server with a sealer plus its HTTP frontend
// so tests can write sealed settings through the API, then call the
// factory directly.
func dnsSettingsServer(t *testing.T) (*server, *httptest.Server) {
	t.Helper()
	sealer, err := secret.New(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	st := newTestStore(t)
	s := newServer(Deps{Store: st, Sealer: sealer})
	srv := httptest.NewServer(s.handler())
	t.Cleanup(srv.Close)
	return s, srv
}

func TestDNSProviderFactory(t *testing.T) {
	s, srv := dnsSettingsServer(t)
	admin := seedUserToken(t, s.st, "root@b.co", "admin")

	// Unset -> errNoDNS.
	if _, err := s.dnsProviderFromSettings(); !errors.Is(err, errNoDNS) {
		t.Fatalf("unset: %v, want errNoDNS", err)
	}

	// none -> errNoDNS.
	doAuthed(t, "PUT", srv.URL+"/v1/settings/dns_provider", admin, `{"value":"none"}`).Body.Close()
	if _, err := s.dnsProviderFromSettings(); !errors.Is(err, errNoDNS) {
		t.Fatalf("none: %v, want errNoDNS", err)
	}

	// cloudflare without token -> hard error (not errNoDNS).
	doAuthed(t, "PUT", srv.URL+"/v1/settings/dns_provider", admin, `{"value":"cloudflare"}`).Body.Close()
	if _, err := s.dnsProviderFromSettings(); err == nil || errors.Is(err, errNoDNS) {
		t.Fatalf("missing token: %v", err)
	}

	// cloudflare with token -> *dns.Cloudflare carrying the unsealed token.
	doAuthed(t, "PUT", srv.URL+"/v1/settings/dns_cloudflare_token", admin, `{"value":"tok123"}`).Body.Close()
	p, err := s.dnsProviderFromSettings()
	if err != nil {
		t.Fatal(err)
	}
	cf, ok := p.(*dns.Cloudflare)
	if !ok || cf.Token != "tok123" {
		t.Fatalf("provider = %#v", p)
	}

	// route53.
	doAuthed(t, "PUT", srv.URL+"/v1/settings/dns_provider", admin, `{"value":"route53"}`).Body.Close()
	doAuthed(t, "PUT", srv.URL+"/v1/settings/dns_route53_access_key", admin, `{"value":"AK"}`).Body.Close()
	doAuthed(t, "PUT", srv.URL+"/v1/settings/dns_route53_secret_key", admin, `{"value":"SK"}`).Body.Close()
	doAuthed(t, "PUT", srv.URL+"/v1/settings/dns_route53_region", admin, `{"value":"eu-west-1"}`).Body.Close()
	p, err = s.dnsProviderFromSettings()
	if err != nil {
		t.Fatal(err)
	}
	r53, ok := p.(*dns.Route53)
	if !ok || r53.AccessKey != "AK" || r53.SecretKey != "SK" || r53.Region != "eu-west-1" {
		t.Fatalf("provider = %#v", p)
	}

	// rfc2136.
	doAuthed(t, "PUT", srv.URL+"/v1/settings/dns_provider", admin, `{"value":"rfc2136"}`).Body.Close()
	doAuthed(t, "PUT", srv.URL+"/v1/settings/dns_rfc2136_server", admin, `{"value":"ns1.example.com"}`).Body.Close()
	doAuthed(t, "PUT", srv.URL+"/v1/settings/dns_rfc2136_tsig_name", admin, `{"value":"luncur"}`).Body.Close()
	doAuthed(t, "PUT", srv.URL+"/v1/settings/dns_rfc2136_tsig_secret", admin, `{"value":"c2Vj"}`).Body.Close()
	p, err = s.dnsProviderFromSettings()
	if err != nil {
		t.Fatal(err)
	}
	rp, ok := p.(*dns.RFC2136)
	if !ok || rp.Server != "ns1.example.com" || rp.TSIGSecret != "c2Vj" {
		t.Fatalf("provider = %#v", p)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/server/ -run 'TestSettingsDNSKeys|TestDNSProviderFactory' -v`
Expected: FAIL — unknown setting / `dnsProviderFromSettings` undefined.

- [ ] **Step 3: Implement**

1. `internal/server/settings.go` — append to `settableKeys`:

```go
	"dns_provider": func(v string) bool {
		return v == "cloudflare" || v == "route53" || v == "rfc2136" || v == "none"
	},
	"dns_cloudflare_token":    func(v string) bool { return v != "" },
	"dns_route53_access_key":  func(v string) bool { return v != "" },
	"dns_route53_secret_key":  func(v string) bool { return v != "" },
	"dns_route53_region":      func(v string) bool { return v != "" },
	"dns_rfc2136_server":      func(v string) bool { return v != "" },
	"dns_rfc2136_tsig_name":   func(v string) bool { return v != "" },
	"dns_rfc2136_tsig_secret": func(v string) bool { return v != "" },
	"dns_rfc2136_tsig_algo":   func(v string) bool { return v != "" },
```

and to `sealedKeys`:

```go
	"dns_cloudflare_token":    true,
	"dns_route53_secret_key":  true,
	"dns_rfc2136_tsig_secret": true,
```

2. Create `internal/server/dnsprovider.go`:

```go
package server

import (
	"errors"
	"fmt"

	"github.com/sutantodadang/luncur/internal/dns"
	"github.com/sutantodadang/luncur/internal/store"
)

// errNoDNS means dns_provider is none/unset — a valid steady state, not a
// configuration error.
var errNoDNS = errors.New("no dns provider configured")

// dnsProviderName reads the install-level dns_provider setting.
func (s *server) dnsProviderName() string {
	v, err := s.st.GetSetting("dns_provider")
	if err != nil || v == "" {
		return "none"
	}
	return v
}

// plainSetting reads a non-sealed setting, mapping ErrNotFound to a
// missing-key error mentioning the key.
func (s *server) plainSetting(key string) (string, error) {
	v, err := s.st.GetSetting(key)
	if errors.Is(err, store.ErrNotFound) {
		return "", fmt.Errorf("%s not set", key)
	}
	return v, err
}

// dnsProviderFromSettings is the default dnsProvider factory: build the
// configured provider from (sealed) settings. errNoDNS when none.
func (s *server) dnsProviderFromSettings() (dns.Provider, error) {
	switch s.dnsProviderName() {
	case "cloudflare":
		token, err := s.sealedSetting("dns_cloudflare_token")
		if err != nil {
			return nil, fmt.Errorf("dns_cloudflare_token: %w", err)
		}
		return &dns.Cloudflare{Token: token}, nil
	case "route53":
		access, err := s.plainSetting("dns_route53_access_key")
		if err != nil {
			return nil, err
		}
		secretKey, err := s.sealedSetting("dns_route53_secret_key")
		if err != nil {
			return nil, fmt.Errorf("dns_route53_secret_key: %w", err)
		}
		region, _ := s.st.GetSetting("dns_route53_region") // optional, default us-east-1
		return &dns.Route53{AccessKey: access, SecretKey: secretKey, Region: region}, nil
	case "rfc2136":
		server, err := s.plainSetting("dns_rfc2136_server")
		if err != nil {
			return nil, err
		}
		name, err := s.plainSetting("dns_rfc2136_tsig_name")
		if err != nil {
			return nil, err
		}
		secretVal, err := s.sealedSetting("dns_rfc2136_tsig_secret")
		if err != nil {
			return nil, fmt.Errorf("dns_rfc2136_tsig_secret: %w", err)
		}
		algo, _ := s.st.GetSetting("dns_rfc2136_tsig_algo") // optional, default hmac-sha256
		return &dns.RFC2136{Server: server, TSIGName: name, TSIGSecret: secretVal, TSIGAlgo: algo}, nil
	default:
		return nil, errNoDNS
	}
}
```

3. `internal/server/server.go` — below the `mailer` field on the struct add:

```go
	// dnsProvider builds the DNS-01 provider from settings; tests override.
	dnsProvider func() (dns.Provider, error)
```

with import `"github.com/sutantodadang/luncur/internal/dns"`, and in `newServer` next to `s.mailer = s.smtpMailer`:

```go
	s.dnsProvider = s.dnsProviderFromSettings
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/server/ -run 'TestSettings|TestDNSProviderFactory' -v`
Expected: PASS.

- [ ] **Step 5: Full verify + commit**

```bash
go build ./... && go vet ./... && go test ./...
git add internal/server/settings.go internal/server/settings_test.go internal/server/dnsprovider.go internal/server/dnsprovider_test.go internal/server/server.go
git commit -m "feat: dns provider settings (sealed creds) + server factory"
```

---

### Task 6: acme Solver refactor + DNS01Solver

**Files:**
- Modify: `internal/acme/acme.go` (Solver interface, HTTP01Solver, Issue refactor)
- Create: `internal/acme/dns01.go`
- Modify: `internal/acme/acmetest/acmetest.go` (DNS-01 mode)
- Test: `internal/acme/acme_test.go`

**Interfaces:**
- Consumes: `dns.Provider` (Task 2).
- Produces: `type Solver interface { Type() string; Setup(ctx context.Context, domain, token, keyAuth string) (cleanup func(), err error) }`; `type HTTP01Solver struct { Challenges *Challenges }`; `Issuer.Solver Solver` (nil → HTTP-01 via `Issuer.Challenges`, fully backward compatible); `type DNS01Solver struct { Provider dns.Provider; LookupTXT func(ctx context.Context, fqdn string) ([]string, error); Timeout, Interval time.Duration }` (Timeout default 2m, Interval default 2s, LookupTXT default authoritative lookup). acmetest gains `SetTXTLookup(func(fqdn string) []string)` switching the fake to dns-01 challenges. Task 7 consumes `DNS01Solver` and `Issuer.Solver`.

- [ ] **Step 1: Write the failing test**

Append to `internal/acme/acme_test.go` (existing imports cover `context`, `strings`, `testing`; add `sync` if not present, plus the dns import):

```go
// recordingProvider is a dns.Provider that records Present/CleanUp calls.
type recordingProvider struct {
	mu       sync.Mutex
	presents map[string]string // fqdn -> value
	cleaned  []string
}

func (p *recordingProvider) Present(ctx context.Context, fqdn, value string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.presents == nil {
		p.presents = map[string]string{}
	}
	p.presents[fqdn] = value
	return nil
}

func (p *recordingProvider) CleanUp(ctx context.Context, fqdn, value string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cleaned = append(p.cleaned, fqdn)
	return nil
}

func (p *recordingProvider) txt(fqdn string) []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if v, ok := p.presents[fqdn]; ok {
		return []string{v}
	}
	return nil
}

// TestIssueDNS01Wildcard drives a full dns-01 order for *.example.com
// against the fake directory: the fake "validates" by looking up the TXT
// the solver presented via the recording provider.
func TestIssueDNS01Wildcard(t *testing.T) {
	prov := &recordingProvider{}
	fake := acmetest.New(t, "")
	fake.SetTXTLookup(prov.txt)

	key, err := acme.GenerateAccountKey()
	if err != nil {
		t.Fatal(err)
	}
	solver := &acme.DNS01Solver{
		Provider: prov,
		LookupTXT: func(ctx context.Context, fqdn string) ([]string, error) {
			return prov.txt(fqdn), nil // instant "propagation"
		},
	}
	iss := &acme.Issuer{
		DirectoryURL: fake.DirectoryURL(), AccountKey: key,
		Email: "a@b.co", Solver: solver,
	}
	certPEM, keyPEM, notAfter, err := iss.Issue(context.Background(), "*.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if len(certPEM) == 0 || len(keyPEM) == 0 || notAfter.IsZero() {
		t.Fatal("empty issuance result")
	}
	// The challenge TXT went to the base domain, not *.example.com.
	if v := prov.txt("_acme-challenge.example.com"); len(v) == 0 {
		t.Fatalf("no TXT presented; presents = %v", prov.presents)
	}
	if len(prov.cleaned) == 0 {
		t.Fatal("TXT record never cleaned up")
	}
}

// TestDNS01PropagationTimeout: the solver gives up when the TXT never
// appears, and cleans up after itself.
func TestDNS01PropagationTimeout(t *testing.T) {
	prov := &recordingProvider{}
	solver := &acme.DNS01Solver{
		Provider: prov,
		LookupTXT: func(ctx context.Context, fqdn string) ([]string, error) {
			return nil, nil // never propagates
		},
		Timeout:  50 * time.Millisecond,
		Interval: 10 * time.Millisecond,
	}
	_, err := solver.Setup(context.Background(), "example.com", "tok", "tok.thumb")
	if err == nil || !strings.Contains(err.Error(), "propagat") {
		t.Fatalf("want propagation timeout, got %v", err)
	}
	if len(prov.cleaned) == 0 {
		t.Fatal("failed setup must clean up its record")
	}
}
```

(Add `"time"` and `"sync"` and `github.com/sutantodadang/luncur/internal/acme/acmetest` / `github.com/sutantodadang/luncur/internal/acme` imports as the file's existing style requires — the file is `package acme_test` or `package acme`; check line 1 and match. If it's an internal test (`package acme`), drop the `acme.` qualifiers.)

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/acme/ -run 'TestIssueDNS01|TestDNS01' -v`
Expected: FAIL — `SetTXTLookup`/`DNS01Solver`/`Solver` undefined.

- [ ] **Step 3: Implement — acme.go Solver refactor**

In `internal/acme/acme.go`:

1. Add after the `Challenges` type:

```go
// Solver answers one ACME challenge type. Setup publishes the challenge
// response and blocks until it is servable; cleanup removes it.
type Solver interface {
	Type() string // "http-01" or "dns-01"
	Setup(ctx context.Context, domain, token, keyAuth string) (cleanup func(), err error)
}

// HTTP01Solver serves challenges from the in-process Challenges store —
// the default solver, preserving the pre-Solver behavior.
type HTTP01Solver struct{ Challenges *Challenges }

func (s HTTP01Solver) Type() string { return "http-01" }

func (s HTTP01Solver) Setup(ctx context.Context, domain, token, keyAuth string) (func(), error) {
	s.Challenges.Put(token, keyAuth)
	return func() { s.Challenges.Delete(token) }, nil
}
```

2. Add a `Solver Solver` field to `Issuer` (after `Challenges`), and a resolver helper:

```go
// solver returns the configured Solver, defaulting to HTTP-01 backed by
// i.Challenges.
func (i *Issuer) solver() Solver {
	if i.Solver != nil {
		return i.Solver
	}
	return HTTP01Solver{Challenges: i.Challenges}
}
```

3. In `Issue`, replace the challenge-selection block (the `for _, c := range z.Challenges` loop through `WaitAuthorization`) with the solver-driven version:

```go
		solver := i.solver()
		var chal *xacme.Challenge
		for _, c := range z.Challenges {
			if c.Type == solver.Type() {
				chal = c
				break
			}
		}
		if chal == nil {
			return nil, nil, time.Time{}, fmt.Errorf("no %s challenge offered for %s", solver.Type(), domain)
		}
		// HTTP01ChallengeResponse returns the raw keyAuthorization
		// (token.thumbprint) — the input both challenge types derive their
		// response from.
		keyAuth, err := cl.HTTP01ChallengeResponse(chal.Token)
		if err != nil {
			return nil, nil, time.Time{}, err
		}
		cleanup, err := solver.Setup(ctx, domain, chal.Token, keyAuth)
		if err != nil {
			return nil, nil, time.Time{}, fmt.Errorf("challenge setup: %w", err)
		}
		defer cleanup()

		if _, err := cl.Accept(ctx, chal); err != nil {
			return nil, nil, time.Time{}, fmt.Errorf("acme accept: %w", err)
		}
		if _, err := cl.WaitAuthorization(ctx, z.URI); err != nil {
			return nil, nil, time.Time{}, fmt.Errorf("acme authorization failed: %w", err)
		}
```

(Also update the package doc comment: "using HTTP-01 or DNS-01 challenges".)

- [ ] **Step 4: Implement — dns01.go**

Create `internal/acme/dns01.go`:

```go
package acme

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/sutantodadang/luncur/internal/dns"
)

// DNS01Solver answers dns-01 challenges through a dns.Provider: publish
// TXT _acme-challenge.<domain> = base64url(sha256(keyAuth)), wait until
// the record is visible, hand back a cleanup that removes it.
type DNS01Solver struct {
	Provider dns.Provider

	// LookupTXT polls for propagation; default queries the domain's
	// authoritative nameservers directly (recursive caches would hold a
	// stale NXDOMAIN for the freshly created record).
	LookupTXT func(ctx context.Context, fqdn string) ([]string, error)

	Timeout  time.Duration // total propagation wait, default 2m
	Interval time.Duration // poll interval, default 2s
}

func (s *DNS01Solver) Type() string { return "dns-01" }

func (s *DNS01Solver) Setup(ctx context.Context, domain, token, keyAuth string) (func(), error) {
	fqdn := "_acme-challenge." + strings.TrimPrefix(domain, "*.")
	sum := sha256.Sum256([]byte(keyAuth))
	value := base64.RawURLEncoding.EncodeToString(sum[:])

	if err := s.Provider.Present(ctx, fqdn, value); err != nil {
		return nil, fmt.Errorf("dns present %s: %w", fqdn, err)
	}
	cleanup := func() {
		cctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := s.Provider.CleanUp(cctx, fqdn, value); err != nil {
			// Best-effort: a leftover TXT record is harmless.
			_ = err
		}
	}

	if err := s.waitPropagation(ctx, fqdn, value); err != nil {
		cleanup()
		return nil, err
	}
	return cleanup, nil
}

func (s *DNS01Solver) waitPropagation(ctx context.Context, fqdn, value string) error {
	timeout := s.Timeout
	if timeout == 0 {
		timeout = 2 * time.Minute
	}
	interval := s.Interval
	if interval == 0 {
		interval = 2 * time.Second
	}
	lookup := s.LookupTXT
	if lookup == nil {
		lookup = authoritativeTXT
	}

	wctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for {
		vals, err := lookup(wctx, fqdn)
		if err == nil {
			for _, v := range vals {
				if v == value {
					return nil
				}
			}
		}
		select {
		case <-wctx.Done():
			return fmt.Errorf("dns-01: TXT %s did not propagate within %s", fqdn, timeout)
		case <-time.After(interval):
		}
	}
}

// authoritativeTXT resolves fqdn's TXT records against the zone's own
// authoritative nameserver: walk parent labels until an NS record set is
// found, then query the first NS directly.
func authoritativeTXT(ctx context.Context, fqdn string) ([]string, error) {
	labels := strings.Split(strings.TrimSuffix(fqdn, "."), ".")
	var nss []*net.NS
	for i := 1; i < len(labels)-1; i++ {
		zone := strings.Join(labels[i:], ".")
		if found, err := net.DefaultResolver.LookupNS(ctx, zone); err == nil && len(found) > 0 {
			nss = found
			break
		}
	}
	if len(nss) == 0 {
		// Fall back to the system resolver.
		return net.DefaultResolver.LookupTXT(ctx, fqdn)
	}
	r := &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, net.JoinHostPort(strings.TrimSuffix(nss[0].Host, "."), "53"))
		},
	}
	return r.LookupTXT(ctx, fqdn)
}
```

- [ ] **Step 5: Implement — acmetest DNS-01 mode**

In `internal/acme/acmetest/acmetest.go`:

1. Add fields to `Server`:

```go
	txtLookup func(fqdn string) []string // non-nil => offer dns-01
	domain    string                     // identifier from the order request
```

and initialize `domain: "www.example.com"` in `New` (keeps HTTP-01 tests unchanged).

2. Add the setter:

```go
// SetTXTLookup switches the fake to dns-01: authorizations offer a dns-01
// challenge, validated by checking lookup("_acme-challenge.<domain>")
// returns at least one TXT value.
func (f *Server) SetTXTLookup(lookup func(fqdn string) []string) { f.txtLookup = lookup }
```

3. In the `/order` handler, capture the requested identifier (the client POSTs `{"identifiers":[{"type":"dns","value":"..."}]}`):

```go
	f.mux.HandleFunc("/order", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Identifiers []struct {
				Value string `json:"value"`
			} `json:"identifiers"`
		}
		if b := jwsPayload(t, r); b != nil {
			_ = json.Unmarshal(b, &req)
		}
		if len(req.Identifiers) > 0 && req.Identifiers[0].Value != "" {
			f.domain = req.Identifiers[0].Value
		}
		w.Header().Set("Location", u+"/order/1")
		w.WriteHeader(http.StatusCreated)
		f.writeOrder(w)
	})
```

4. In the `/authz/1` handler, replace the hardcoded identifier/challenge with mode-dependent values:

```go
	f.mux.HandleFunc("/authz/1", func(w http.ResponseWriter, r *http.Request) {
		status := "pending"
		if f.authzOK {
			status = "valid"
		}
		chalType := "http-01"
		if f.txtLookup != nil {
			chalType = "dns-01"
		}
		json.NewEncoder(w).Encode(map[string]any{
			"status":     status,
			"identifier": map[string]string{"type": "dns", "value": strings.TrimPrefix(f.domain, "*.")},
			"challenges": []map[string]string{{
				"type": chalType, "url": u + "/chal/1", "token": "tok-e2e", "status": status,
			}},
		})
	})
```

(add `"strings"` to imports.)

5. In the `/chal/1` handler, branch on mode:

```go
	f.mux.HandleFunc("/chal/1", func(w http.ResponseWriter, r *http.Request) {
		if f.txtLookup != nil {
			fqdn := "_acme-challenge." + strings.TrimPrefix(f.domain, "*.")
			if len(f.txtLookup(fqdn)) == 0 {
				http.Error(w, `{"status":"invalid"}`, http.StatusOK)
				return
			}
			f.authzOK = true
			fmt.Fprint(w, `{"status":"valid"}`)
			return
		}
		resp, err := http.Get("http://" + f.chalHost + acme.ChallengePath + "tok-e2e")
		if err != nil || resp.StatusCode != 200 {
			http.Error(w, `{"status":"invalid"}`, http.StatusOK)
			return
		}
		f.authzOK = true
		fmt.Fprint(w, `{"status":"valid"}`)
	})
```

- [ ] **Step 6: Run tests to verify they pass**

Run: `go test ./internal/acme/ -v`
Expected: PASS — the two new tests AND the existing HTTP-01 end-to-end test (`Solver == nil` defaults to the old path; acmetest defaults to http-01 mode).

- [ ] **Step 7: Full verify + commit**

```bash
go build ./... && go vet ./... && go test ./...
git add internal/acme/
git commit -m "feat: pluggable acme Solver + DNS-01 solver with propagation wait"
```

---

### Task 7: cert manager DNS-01 pick + wildcard domains

**Files:**
- Modify: `internal/store/domains.go` (wildcard-aware validation)
- Modify: `internal/server/domains.go` (wildcard-needs-DNS guard, skip dnsWarning for wildcards)
- Modify: `internal/server/certs.go` (solver selection in `issue`)
- Test: `internal/store/domains_test.go`, `internal/server/domains_test.go`, `internal/server/certs_test.go`

**Interfaces:**
- Consumes: `s.dnsProviderName()`, `s.dnsProvider` (Task 5), `acme.DNS01Solver`, `Issuer.Solver` (Task 6).
- Produces: wildcard hostnames stored as ordinary `domains.hostname` values; 400 on wildcard-without-provider; DNS-01 issuance path in `certManager.issue`.

- [ ] **Step 1: Write the failing tests**

Append to `internal/store/domains_test.go`:

```go
func TestAddDomainWildcard(t *testing.T) {
	s := openTest(t)
	p, err := s.CreateProject("proj")
	if err != nil {
		t.Fatal(err)
	}
	a, err := s.CreateApp(p.ID, "web", 8080)
	if err != nil {
		t.Fatal(err)
	}

	d, err := s.AddDomain(a.ID, "*.Example.COM")
	if err != nil {
		t.Fatal(err)
	}
	if d.Hostname != "*.example.com" {
		t.Fatalf("hostname = %q", d.Hostname)
	}

	for _, bad := range []string{"*", "*.", "*.*.example.com", "foo.*.example.com", "*example.com"} {
		if _, err := s.AddDomain(a.ID, bad); err == nil {
			t.Fatalf("%q accepted", bad)
		}
	}
}
```

Append to `internal/server/domains_test.go` (check its fixture style at the top of the file — it uses `testServer`/`kubeServer`-style helpers; follow the existing `TestDomainCRUDAndRender` pattern for seeding a project/app):

```go
// TestWildcardDomainNeedsDNSProvider: wildcard + dns_provider none -> 400;
// with a provider set the row is created.
func TestWildcardDomainNeedsDNSProvider(t *testing.T) {
	srv, st := testServer(t)
	admin := seedUserToken(t, st, "root@b.co", "admin")
	doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"proj"}`).Body.Close()
	doAuthed(t, "POST", srv.URL+"/v1/projects/proj/apps", admin, `{"name":"web","port":8080}`).Body.Close()

	resp := doAuthed(t, "POST", srv.URL+"/v1/projects/proj/apps/web/domains", admin, `{"hostname":"*.example.com"}`)
	if resp.StatusCode != 400 {
		t.Fatalf("wildcard without provider: want 400, got %d", resp.StatusCode)
	}
	var env struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if !strings.Contains(env.Error.Message, "dns_provider") {
		t.Fatalf("message = %q", env.Error.Message)
	}

	if err := st.SetSetting("dns_provider", "cloudflare"); err != nil {
		t.Fatal(err)
	}
	resp = doAuthed(t, "POST", srv.URL+"/v1/projects/proj/apps/web/domains", admin, `{"hostname":"*.example.com"}`)
	if resp.StatusCode != 201 {
		t.Fatalf("wildcard with provider: want 201, got %d", resp.StatusCode)
	}
	var out struct {
		Hostname   string `json:"hostname"`
		DNSWarning string `json:"dns_warning"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if out.Hostname != "*.example.com" {
		t.Fatalf("hostname = %q", out.Hostname)
	}
	if out.DNSWarning != "" {
		t.Fatalf("wildcard must skip the A-record warning, got %q", out.DNSWarning)
	}
}
```

Append to `internal/server/certs_test.go` (mirror the existing builtin-issuance test's fixture — `certTestServer` — and its store seeding; the key point is the injected fake provider + DNS-01 fake directory):

```go
// TestIssueDNS01PickedForWildcard: with a dns provider configured, the
// cert manager issues a wildcard via dns-01 (no challenge-ingress writes)
// and stores the cert.
func TestIssueDNS01PickedForWildcard(t *testing.T) {
	s, st, patches, _ := certTestServer(t) // adjust to the fixture's actual signature

	prov := &recordingServerProvider{}
	s.dnsProvider = func() (dns.Provider, error) { return prov, nil }
	if err := st.SetSetting("dns_provider", "cloudflare"); err != nil {
		t.Fatal(err)
	}

	fakeDir := acmetest.New(t, "127.0.0.1:1") // http fetch must never happen
	fakeDir.SetTXTLookup(prov.txt)
	s.certs.directoryURL = fakeDir.DirectoryURL()

	// Seed project/app/domain the same way the existing issuance test does,
	// with hostname "*.example.com", then:
	//   p, a, d := ... (from the seeding)
	//   s.certs.issue(context.Background(), certJob{p, a, d})
	// and assert:
	//   - store domain cert_status == "issued"
	//   - prov recorded a Present for "_acme-challenge.example.com"
	//   - no patch in *patches touched the luncur-acme challenge Ingress
	_ = patches
}

// recordingServerProvider mirrors internal/acme's test recorder for use in
// package server tests.
type recordingServerProvider struct {
	mu       sync.Mutex
	presents map[string]string
}

func (p *recordingServerProvider) Present(ctx context.Context, fqdn, value string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.presents == nil {
		p.presents = map[string]string{}
	}
	p.presents[fqdn] = value
	return nil
}

func (p *recordingServerProvider) CleanUp(ctx context.Context, fqdn, value string) error { return nil }

func (p *recordingServerProvider) txt(fqdn string) []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if v, ok := p.presents[fqdn]; ok {
		return []string{v}
	}
	return nil
}
```

IMPLEMENTER NOTE: `certTestServer`'s exact return signature and the existing issuance test's seeding live at `internal/server/certs_test.go:44` and `:143` — read that file first and complete the skeleton above to match (this is fixture plumbing, not design; the assertions listed in the comment are the contract). The DNS01Solver used by `issue` must get an instant `LookupTXT` in tests — see Step 3's `newDNS01Solver` seam.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/store/ -run TestAddDomainWildcard -v && go test ./internal/server/ -run 'TestWildcard|TestIssueDNS01' -v`
Expected: FAIL — store rejects `*.example.com`; server tests don't compile / 400 missing.

- [ ] **Step 3: Implement**

1. `internal/store/domains.go` — wildcard-aware validation in `AddDomain`:

```go
	h := strings.ToLower(strings.TrimSpace(hostname))
	base, isWildcard := strings.CutPrefix(h, "*.")
	if strings.Contains(base, "*") || !hostnameRe.MatchString(base) {
		return Domain{}, fmt.Errorf("invalid hostname %q", hostname)
	}
	_ = isWildcard // stored as-is; policy (provider required) is the server's job
```

(keep inserting `h`, which retains the `*.` prefix.)

2. `internal/server/domains.go`:

```go
// errWildcardNeedsDNS gates wildcard hostnames: they can only be validated
// over DNS-01, which needs a configured provider.
var errWildcardNeedsDNS = errors.New("wildcard domains require a configured dns_provider (settings: dns_provider)")
```

In `addDomain`, before `s.st.AddDomain`:

```go
	isWildcard := strings.HasPrefix(strings.TrimSpace(hostname), "*.")
	if isWildcard && s.dnsProviderName() == "none" {
		return store.Domain{}, "", errWildcardNeedsDNS
	}
```

and replace the `dnsWarning` call:

```go
	warning := ""
	if !isWildcard {
		warning = dnsWarning(ctx, d.Hostname, s.externalIP)
	}
```

In `handleAddDomain`'s error switch add (before the generic 400):

```go
		if errors.Is(err, errWildcardNeedsDNS) {
			writeError(w, http.StatusBadRequest, "bad_request", err.Error())
			return
		}
```

(the generic 400 branch already returns `err.Error()`; the explicit branch just documents intent — fold it in if the linter flags duplication, keeping the 400 + message.) Add `"strings"` to imports. The UI twin `handleUIDomainAdd` already surfaces `err.Error()` on 400 — unchanged.

3. `internal/server/certs.go` — in `issue`, replace the challenge-ingress setup + Issuer construction (`internal/server/certs.go:139-161`) with solver selection:

```go
	key, err := m.accountKey(ctx)
	if err != nil {
		fail(fmt.Errorf("acme account key: %w", err))
		return
	}

	useDNS := strings.HasPrefix(j.d.Hostname, "*.") || m.s.dnsProviderName() != "none"
	var solver acme.Solver
	if useDNS {
		prov, err := m.s.dnsProvider()
		if err != nil {
			fail(fmt.Errorf("dns provider: %w", err))
			return
		}
		solver = m.newDNS01Solver(prov)
	} else {
		if err := m.setChallengeHost(ctx, j.d.Hostname, true); err != nil {
			fail(fmt.Errorf("challenge ingress: %w", err))
			return
		}
		defer func() {
			if err := m.setChallengeHost(ctx, j.d.Hostname, false); err != nil {
				log.Printf("remove challenge host %s: %v", j.d.Hostname, err)
			}
		}()
	}

	email, _ := st.GetSetting("acme_email")
	iss := &acme.Issuer{
		DirectoryURL: m.directoryURL, AccountKey: key,
		Email: email, Challenges: m.challenges, Solver: solver,
	}
```

and add the seam + import:

```go
// newDNS01Solver builds the dns-01 solver; tests override lookupTXT for
// instant propagation.
func (m *certManager) newDNS01Solver(prov dns.Provider) *acme.DNS01Solver {
	return &acme.DNS01Solver{Provider: prov, LookupTXT: m.lookupTXT}
}
```

with a `lookupTXT func(ctx context.Context, fqdn string) ([]string, error)` field on `certManager` (nil = solver default authoritative lookup); tests set `s.certs.lookupTXT = func(...)...` for instant propagation. Import `"strings"` and `"github.com/sutantodadang/luncur/internal/dns"`.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/store/ ./internal/server/ -v 2>&1 | tail -20`
Expected: PASS — new tests plus every existing cert/domain test (HTTP-01 path must be byte-for-byte behavior-identical when no provider is configured and the hostname isn't a wildcard).

- [ ] **Step 5: Full verify + commit**

```bash
go build ./... && go vet ./... && go test ./...
git add internal/store/domains.go internal/store/domains_test.go internal/server/domains.go internal/server/domains_test.go internal/server/certs.go internal/server/certs_test.go
git commit -m "feat: wildcard domains + DNS-01 issuance path in the cert manager"
```

---

### Task 8: release image + docs

**Files:**
- Modify: `Dockerfile` (nsupdate via bind-tools)
- Modify: `README.md`

- [ ] **Step 1: Add nsupdate to the release image**

In `Dockerfile`, extend the runtime stage's apk line:

```dockerfile
RUN apk add --no-cache git ca-certificates bind-tools
```

with a comment above it:

```dockerfile
# bind-tools ships nsupdate, used only when dns_provider=rfc2136 —
# a runtime binary selected on demand, like git (deploys) and the
# in-pod pg_dump (backups).
```

- [ ] **Step 2: Update README**

1. Domains/TLS section: document wildcard domains + DNS providers:

```sh
luncur config set dns_provider cloudflare       # cloudflare | route53 | rfc2136 | none (default)
luncur config set dns_cloudflare_token ...      # write-only: reads show "(set)"

# route53
luncur config set dns_route53_access_key AKIA...
luncur config set dns_route53_secret_key ...    # write-only
luncur config set dns_route53_region us-east-1  # optional

# rfc2136 (nsupdate + TSIG)
luncur config set dns_rfc2136_server ns1.example.com
luncur config set dns_rfc2136_tsig_name luncur-key
luncur config set dns_rfc2136_tsig_secret ...   # write-only
luncur config set dns_rfc2136_tsig_algo hmac-sha256  # optional
```

Prose: with a provider configured, the builtin cert manager validates via DNS-01 (TXT `_acme-challenge.<domain>`, polling the zone's authoritative nameservers, ~2 min timeout) instead of HTTP-01 — required for `*.example.com` wildcard domains, which `domain add` now accepts. A wildcard without a configured `dns_provider` is refused (wildcards can't be validated over HTTP-01). `traefik`/`cert-manager` cert providers are unchanged — they own their own solving. Failure behavior matches HTTP-01: the domain shows `cert_status failed` with the message; the app keeps serving.

2. Design-notes bullet: RFC2136 support shells out to `nsupdate` (bind-tools, in the release image); the TSIG secret is passed on stdin, not argv. This is a runtime binary, not a Go module — the no-new-dependencies rule is about Go modules (`git`, `pg_dump`, `nsupdate`).

- [ ] **Step 3: Full verify + commit**

```bash
go build ./... && go vet ./... && go test ./...
git add Dockerfile README.md
git commit -m "docs: dns providers + wildcard certs; nsupdate in release image"
```

---

## Manual verification (owner's VPS, after merge)

Per the Phase 4 test strategy: issue a wildcard cert via each DNS provider (Cloudflare token, Route53 keys, RFC2136 against a BIND with TSIG); confirm `*.example.com` serves HTTPS on an arbitrary subdomain; confirm a plain domain still issues over HTTP-01 when `dns_provider=none`.

## Self-review notes

- Spec coverage: Provider interface + three impls (Tasks 2–4), settings + sealed creds (Task 5), awssig extraction with Plan K vector (Task 1), Solver + DNS-01 solver with TXT hash + propagation polling ~2min (Task 6), builtin manager picks DNS-01 on wildcard OR provider-set with traefik/cert-manager untouched (Task 7), AddDomain wildcard validation + 400-without-provider (Task 7), nsupdate image deviation + docs (Task 8). Error table: provider error/propagation timeout → `cert_status failed` via the existing `fail()` path (Task 7 wiring; timeout tested in Task 6).
- Type consistency: `dns.Provider` methods `(ctx, fqdn, value)` used identically by all three impls, the solver, and the fakes; `DNS01Solver{Provider, LookupTXT, Timeout, Interval}` matches Tasks 6–7 call sites; `s.dnsProvider func() (dns.Provider, error)` seam mirrors `s.mailer`.
- Known fixture-dependent spots (flagged in-line): the exact Plan K known-answer header string (Task 1) and `certTestServer`'s signature/seeding (Task 7) must be read from the existing test files at execution time — both are copy-from-neighbor steps, not design gaps.
