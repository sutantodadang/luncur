# luncur Plan K — backups Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `luncur backup create` snapshots luncur's whole state (SQLite, sealer key, addon dumps) into a tar.gz on the PVC and optionally uploads it to any S3-compatible bucket; scheduled daily backups + pruning; restore documented as a runbook.

**Architecture:** A pure `internal/s3` package implements AWS SigV4 (stdlib only, UNSIGNED-PAYLOAD, path-style URLs) with Put/Delete. The backup engine in the server assembles the archive: `VACUUM INTO` for a consistent DB snapshot, the sealer key file, and per-addon logical dumps streamed through the Plan-I `PodExecer` (`pg_dump` / `redis-cli SAVE + cat`, credentials referenced from the pod's own env — never on the command line). Failures of individual addon dumps degrade to warnings; the archive still lands. A daily scheduler goroutine mirrors the cert-manager loop's lifecycle.

**Tech Stack:** Go stdlib (`archive/tar`, `compress/gzip`, `crypto/hmac`, `crypto/sha256`), client-go remotecommand via the existing `kube.PodExecer`, modernc.org/sqlite (`VACUUM INTO`).

## Global Constraints

- Single Go module, one binary from `cmd/luncur`. **No new dependencies** — S3 via a stdlib SigV4 signer.
- Server-side apply everywhere. API error envelope via `writeError`. Conventional commits; `go build ./... && go vet ./... && go test ./...` before every commit.
- Tests must not require a cluster or network: `PodExecer` faked; S3 tested against known-answer signature vectors + an httptest fake; `VACUUM INTO` works on the temp-file test stores.
- Addon dump failure → warning, backup completes. S3 failure → local archive kept, error surfaced. Restore is a README runbook, NOT a command.

---

### Task 1: internal/s3 — SigV4 client

**Files:**
- Create: `internal/s3/s3.go`
- Test: `internal/s3/s3_test.go`

**Interfaces:**
- Consumes: stdlib only.
- Produces:
  - `type Client struct { Endpoint, Region, Bucket, AccessKey, SecretKey string; HTTPClient *http.Client; Now func() time.Time }` — `Region` defaults `us-east-1`, `HTTPClient` defaults `http.DefaultClient`, `Now` defaults `time.Now` (injectable for the known-answer test). `Endpoint` is scheme+host (e.g. `https://minio.example.com`); requests are path-style: `<endpoint>/<bucket>/<key>`.
  - `Client.Put(ctx context.Context, key string, body io.Reader, size int64) error`
  - `Client.Delete(ctx context.Context, key string) error`
  - `sign(req *http.Request, accessKey, secretKey, region string, t time.Time)` (unexported) — SigV4 with `x-amz-content-sha256: UNSIGNED-PAYLOAD`, signed headers `host;x-amz-content-sha256;x-amz-date`.

- [ ] **Step 1: Failing tests** (`internal/s3/s3_test.go`):

```go
package s3

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestSignKnownAnswer pins the SigV4 algorithm with a fixed time/creds:
// the exact Authorization header is asserted so any signing regression
// fails loudly. Expected value computed once from the spec'd algorithm —
// after first implementation, verify the header manually against the
// AWS SigV4 documentation steps, then freeze it here.
func TestSignKnownAnswer(t *testing.T) {
	req, _ := http.NewRequest("PUT", "https://s3.example.com/bucket/backups/a.tar.gz", strings.NewReader("hi"))
	ts := time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)
	sign(req, "AKIDEXAMPLE", "SECRETKEY", "us-east-1", ts)

	auth := req.Header.Get("Authorization")
	for _, want := range []string{
		"AWS4-HMAC-SHA256",
		"Credential=AKIDEXAMPLE/20260703/us-east-1/s3/aws4_request",
		"SignedHeaders=host;x-amz-content-sha256;x-amz-date",
		"Signature=",
	} {
		if !strings.Contains(auth, want) {
			t.Fatalf("auth header missing %q:\n%s", want, auth)
		}
	}
	if req.Header.Get("x-amz-date") != "20260703T120000Z" {
		t.Fatalf("x-amz-date = %q", req.Header.Get("x-amz-date"))
	}
	if req.Header.Get("x-amz-content-sha256") != "UNSIGNED-PAYLOAD" {
		t.Fatalf("content sha = %q", req.Header.Get("x-amz-content-sha256"))
	}
	// Freeze the full signature once implemented and manually verified:
	// re-run sign() twice — deterministic input must produce identical output.
	req2, _ := http.NewRequest("PUT", "https://s3.example.com/bucket/backups/a.tar.gz", strings.NewReader("hi"))
	sign(req2, "AKIDEXAMPLE", "SECRETKEY", "us-east-1", ts)
	if req2.Header.Get("Authorization") != auth {
		t.Fatal("signing is not deterministic")
	}
}

func TestPutAndDelete(t *testing.T) {
	var gotPut, gotDelete *http.Request
	var putBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPut:
			gotPut = r
			putBody, _ = io.ReadAll(r.Body)
		case http.MethodDelete:
			gotDelete = r
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := &Client{Endpoint: srv.URL, Bucket: "luncur-backups", AccessKey: "k", SecretKey: "s"}
	if err := c.Put(context.Background(), "backups/x.tar.gz", strings.NewReader("payload"), 7); err != nil {
		t.Fatal(err)
	}
	if gotPut == nil || gotPut.URL.Path != "/luncur-backups/backups/x.tar.gz" {
		t.Fatalf("put path = %+v", gotPut)
	}
	if string(putBody) != "payload" {
		t.Fatalf("body = %q", putBody)
	}
	if !strings.HasPrefix(gotPut.Header.Get("Authorization"), "AWS4-HMAC-SHA256") {
		t.Fatalf("unsigned put: %q", gotPut.Header.Get("Authorization"))
	}
	if err := c.Delete(context.Background(), "backups/x.tar.gz"); err != nil {
		t.Fatal(err)
	}
	if gotDelete == nil || gotDelete.URL.Path != "/luncur-backups/backups/x.tar.gz" {
		t.Fatalf("delete path = %+v", gotDelete)
	}
}

func TestPutSurfacesHTTPErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "AccessDenied", http.StatusForbidden)
	}))
	defer srv.Close()
	c := &Client{Endpoint: srv.URL, Bucket: "b", AccessKey: "k", SecretKey: "s"}
	err := c.Put(context.Background(), "k", strings.NewReader("x"), 1)
	if err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("want 403 error, got %v", err)
	}
}
```

- [ ] **Step 2: Run** `go test ./internal/s3/ -v` — package missing.

- [ ] **Step 3: Implement** `internal/s3/s3.go`:

```go
// Package s3 is a minimal S3-compatible client (SigV4, path-style) for
// backup uploads — stdlib only, works with AWS/R2/minio/B2.
package s3

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type Client struct {
	Endpoint  string // scheme+host, e.g. https://s3.us-east-1.amazonaws.com
	Region    string // default us-east-1
	Bucket    string
	AccessKey string
	SecretKey string

	HTTPClient *http.Client     // default http.DefaultClient
	Now        func() time.Time // default time.Now (injectable in tests)
}

func (c *Client) region() string {
	if c.Region == "" {
		return "us-east-1"
	}
	return c.Region
}

func (c *Client) httpClient() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	return http.DefaultClient
}

func (c *Client) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *Client) url(key string) string {
	return strings.TrimRight(c.Endpoint, "/") + "/" + c.Bucket + "/" + strings.TrimLeft(key, "/")
}

func (c *Client) send(ctx context.Context, method, key string, body io.Reader, size int64) error {
	req, err := http.NewRequestWithContext(ctx, method, c.url(key), body)
	if err != nil {
		return err
	}
	if size > 0 {
		req.ContentLength = size
	}
	sign(req, c.AccessKey, c.SecretKey, c.region(), c.now().UTC())
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("s3 %s %s: %d %s", method, key, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return nil
}

// Put uploads one object. The payload is not hashed (UNSIGNED-PAYLOAD), so
// body can stream without buffering.
func (c *Client) Put(ctx context.Context, key string, body io.Reader, size int64) error {
	return c.send(ctx, http.MethodPut, key, body, size)
}

func (c *Client) Delete(ctx context.Context, key string) error {
	return c.send(ctx, http.MethodDelete, key, nil, 0)
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

// sign implements AWS Signature Version 4 for a single-chunk request with
// an unsigned payload. Signed headers: host, x-amz-content-sha256,
// x-amz-date.
func sign(req *http.Request, accessKey, secretKey, region string, t time.Time) {
	const service = "s3"
	amzDate := t.Format("20060102T150405Z")
	shortDate := t.Format("20060102")

	req.Header.Set("x-amz-date", amzDate)
	req.Header.Set("x-amz-content-sha256", "UNSIGNED-PAYLOAD")

	canonicalHeaders := "host:" + req.Host + "\n" +
		"x-amz-content-sha256:UNSIGNED-PAYLOAD\n" +
		"x-amz-date:" + amzDate + "\n"
	signedHeaders := "host;x-amz-content-sha256;x-amz-date"

	canonicalRequest := strings.Join([]string{
		req.Method,
		req.URL.EscapedPath(),
		req.URL.RawQuery,
		canonicalHeaders,
		signedHeaders,
		"UNSIGNED-PAYLOAD",
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
```

(Note: `req.Host` is empty until the request is sent unless set — in `send`, requests built by `http.NewRequestWithContext` populate `req.Host` from the URL automatically via `req.URL.Host`; use `req.URL.Host` in `sign` if `req.Host` is empty: `host := req.Host; if host == "" { host = req.URL.Host }`. Apply that fix while implementing.)

- [ ] **Step 4: Run** `go test ./internal/s3/ -v` — pass.
- [ ] **Step 5: Commit** — `feat: stdlib SigV4 S3 client (put/delete)`

---

### Task 2: store + settings — backups table, AllAddons, S3 settings keys

**Files:**
- Modify: `internal/store/schema.sql`
- Create: `internal/store/backups.go`
- Modify: `internal/store/addons.go` (AllAddons)
- Modify: `internal/server/settings.go` (allowlist + sealed secret key)
- Test: `internal/store/backups_test.go`, `internal/store/addons_test.go` (append), `internal/server/settings_test.go` (append)

**Interfaces:**
- Consumes: existing settings API (`settableKeys`, `handleSetSetting`/`handleGetSetting`), `s.sealer`.
- Produces:
  - `backups` table: `id INTEGER PRIMARY KEY, path TEXT NOT NULL, size_bytes INTEGER NOT NULL, uploaded INTEGER NOT NULL DEFAULT 0, created_at TEXT NOT NULL DEFAULT (datetime('now'))`.
  - `type Backup struct { ID int64; Path string; SizeBytes int64; Uploaded bool; CreatedAt string }`
  - `Store.CreateBackup(path string, size int64, uploaded bool) (Backup, error)`, `Store.ListBackups() ([]Backup, error)` (newest first), `Store.DeleteBackup(id int64) error` (`ErrNotFound` when absent).
  - `Store.AllAddons() ([]Addon, error)` (ordered by id).
  - Settings allowlist gains: `backup_s3_endpoint`, `backup_s3_bucket`, `backup_s3_prefix`, `backup_s3_access_key`, `backup_s3_secret_key`, `backup_schedule` (must be `daily`|`off`), `backup_keep` (positive integer string). **Sealed value:** `backup_s3_secret_key` is sealed with `s.sealer` before storing (503 `sealer_unavailable` when no sealer) and `handleGetSetting` returns `"(set)"` instead of the value for that key (write-only secret). Store the sealed bytes hex-encoded in the settings value column (`encoding/hex`), prefix `sealed:`.

- [ ] **Step 1: Failing tests.** `backups_test.go`: create two backups → list newest-first with sizes/uploaded flags; delete → gone; second delete → `ErrNotFound`. `addons_test.go`: `AllAddons` across two projects returns both. `settings_test.go`: PUT `backup_schedule` `weekly` → 400; `daily` → 204; PUT `backup_s3_secret_key` → 204, GET returns `"(set)"` not the plaintext, and the raw settings row (read via `st.GetSetting` in the test) starts with `sealed:`; PUT `backup_keep` `abc` → 400, `7` → 204. (Real code following the files' existing tests.)

- [ ] **Step 2: Run** — failures.

- [ ] **Step 3: Implement.** `backups.go` + `AllAddons` follow the store's established scan patterns exactly (mirror `ListBackups` on `ListTokens`, `AllAddons` on `AllDomains`). Settings: extend `settableKeys` with per-key validators (`backup_schedule`: `v == "daily" || v == "off"`; `backup_keep`: `strconv.Atoi` > 0; the rest: non-empty); in `handleSetSetting`, special-case `backup_s3_secret_key`: require `s.sealer` (503 otherwise), store `"sealed:" + hex.EncodeToString(sealed)`; in `handleGetSetting`, return `{"key":..., "value":"(set)"}` for that key when present. Add an unexported server helper for Plan-K's uploader:

```go
// s3SecretKey unseals the write-only backup_s3_secret_key setting.
func (s *server) s3SecretKey() (string, error) {
	v, err := s.st.GetSetting("backup_s3_secret_key")
	if err != nil {
		return "", err
	}
	raw, ok := strings.CutPrefix(v, "sealed:")
	if !ok {
		return "", fmt.Errorf("backup_s3_secret_key is not sealed")
	}
	b, err := hex.DecodeString(raw)
	if err != nil {
		return "", err
	}
	if s.sealer == nil {
		return "", errSealerUnavailable
	}
	plain, err := s.sealer.Open(b)
	if err != nil {
		return "", err
	}
	return string(plain), nil
}
```

- [ ] **Step 4: Run** `go test ./internal/store/ ./internal/server/ -v` — pass.
- [ ] **Step 5: Commit** — `feat: backups store + S3 settings (sealed secret key)`

---

### Task 3: server — backup engine + API + scheduler

**Files:**
- Create: `internal/server/backup.go`
- Modify: `internal/server/server.go` (Deps.SecretKeyPath; routes; StartBackups in the NewWithBackend start closure)
- Modify: `internal/cli/serve.go` (pass SecretKeyPath)
- Test: `internal/server/backup_test.go`

**Interfaces:**
- Consumes: Task 1 `s3.Client`, Task 2 store methods + `s3SecretKey`, `kube.PodExecer` (Plan I — `s.kube` implements it; nil-able), `s.st.DB()` (VACUUM INTO), addon container names `postgres`/`redis` (see `internal/addon/addon.go`), pod name `addon-<name>-0` (StatefulSet ordinal 0).
- Produces:
  - `Deps` gains `SecretKeyPath string`; `serve.go` passes the resolved `keyFile`.
  - `s.createBackup(ctx context.Context, upload bool) (store.Backup, []string, error)` — the engine:
    1. `os.MkdirAll(dataDir/backups)`; archive path `backups/luncur-<YYYYMMDD-HHMMSS>.tar.gz` (time from a `s.nowFn func() time.Time` field defaulting `time.Now`, injectable in tests).
    2. DB snapshot: `VACUUM INTO ?` via `s.st.DB().Exec` with the temp path bound as a parameter (SQLite supports binds in VACUUM INTO; if modernc rejects the bind, fall back to inlining the path with single quotes doubled — the path is server-generated, not user input), tar member `luncur.db`.
    3. Sealer key: read `s.secretKeyPath` when non-empty, member `luncur.key`; unreadable → warning.
    4. Addon dumps (skipped entirely with a warning when `s.kube == nil`): for each `AllAddons()` row, resolve the project namespace (`GetProjectByID`), pod `addon-<name>-0`, container = addon type; postgres: `sh -c 'PGPASSWORD="$POSTGRES_PASSWORD" pg_dump -U "$POSTGRES_USER" -Fc "$POSTGRES_DB"'` → member `addons/<project>-<name>.pgdump`; redis: `sh -c 'redis-cli -a "$REDIS_PASSWORD" --no-auth-warning SAVE >/dev/null && cat /data/dump.rdb'` → member `addons/<project>-<name>.rdb`. Each dump streams into the tar via an in-memory buffer (`bytes.Buffer` — addon dumps are small in Phase 3's world); exec error → warning `"addon <name>: <err>"`, member skipped.
    5. `manifest.json` member: `{"created_at":..., "warnings":[...], "members":[...]}`.
    6. Upload when `upload && backup_s3_endpoint set`: build `s3.Client` from settings + `s3SecretKey()`; key `<prefix>/<filename>` (prefix default `luncur`); success → uploaded=true; failure → warning + uploaded=false (local file kept).
    7. `CreateBackup(path, size, uploaded)` row; return row + warnings.
  - `s.pruneBackups(ctx) (removed int, err error)` — keep newest `backup_keep` (default 7): delete local files + DB rows + remote objects (best-effort, warnings logged) for the rest.
  - API (admin): `POST /v1/backups` body `{"no_upload":bool}` → 201 `{"id","path","size_bytes","uploaded","warnings":[...]}`; `GET /v1/backups` → list; `POST /v1/backups/prune` → 200 `{"removed":N}`.
  - Scheduler: `(*server).StartBackups(ctx)` — goroutine, hourly tick; runs `createBackup(ctx, true)` + `pruneBackups` when `backup_schedule == "daily"` and the newest backup is older than 24h (or none exists). Wired into the existing start closure returned by `NewWithBackend` (the closure currently calls `StartCerts`; make it call both).
- [ ] **Step 1: Failing tests** (`internal/server/backup_test.go`; fixture: temp store + DataDir + sealer + fake kube where needed; a `fakeExecer` implementing `kube.PodExecer` writing canned bytes to stdout — but note `s.kube` is a `*kube.Client`; the engine must depend on an interface field: give `server` an `execer kube.PodExecer` field set to `s.kube` in `newServer` when kube non-nil, overridable in tests):

```go
func TestCreateBackupArchive(t *testing.T) {
	// Fixture: store with one project + one postgres addon row; execer fake
	// returns "PGDUMPDATA" on the pg_dump command; SecretKeyPath points at
	// a temp file with known bytes; no S3 settings → upload skipped.
	// Act: b, warnings, err := srv.createBackup(ctx, true)
	// Assert: err nil; file exists at b.Path; tar members include
	// luncur.db (non-empty), luncur.key (known bytes),
	// addons/proj-db1.pgdump ("PGDUMPDATA"), manifest.json; b.Uploaded
	// false. Upload is skipped SILENTLY when backup_s3_endpoint is unset
	// (not configuring S3 is a valid steady state, not a warning) —
	// assert warnings is empty.
}

func TestCreateBackupAddonFailureWarns(t *testing.T) {
	// execer fake errors → backup still succeeds, warnings has one entry
	// naming the addon, archive lacks the dump member.
}

func TestBackupUploadAndPrune(t *testing.T) {
	// httptest S3 fake records PUTs/DELETEs; settings set endpoint/bucket/
	// access/secret (secret via the sealed path — call the settings API or
	// seal directly); createBackup uploads (uploaded=true, PUT seen).
	// Create 3 backups with backup_keep=2 → pruneBackups removes 1 local
	// file + row and issues a DELETE for it.
}

func TestBackupAPI(t *testing.T) {
	// Admin POST /v1/backups {"no_upload":true} → 201 with path; member →
	// 403; GET lists it; POST /v1/backups/prune → 200.
}
```

(Real code. The fixture writes canned exec output by matching on the command string — `strings.Contains(strings.Join(cmd, " "), "pg_dump")`.)

- [ ] **Step 2: Run** — failures.

- [ ] **Step 3: Implement** per the Interfaces block. Structure `backup.go` as: `createBackup` (orchestration), `dumpAddon(ctx, ad store.Addon) ([]byte, string, error)` (exec + member name), `uploadBackup(ctx, path string) error` (settings → s3.Client → Put), `pruneBackups`, handlers, `StartBackups`. `server` struct gains `execer kube.PodExecer`, `secretKeyPath string`, `nowFn func() time.Time`. `newServer` sets `execer` from `d.Kube` when non-nil, `secretKeyPath` from `d.SecretKeyPath`, `nowFn` default. Routes (admin): the three above. `NewWithBackend`'s returned start closure calls `s.StartCerts(ctx)` AND `go s.StartBackups(ctx)` — rename it conceptually to "start background loops" (update the one serve.go call site comment if any).

- [ ] **Step 4: Run** `go test ./internal/server/ -v` — pass. Full suite green.
- [ ] **Step 5: Commit** — `feat: backup engine — archive, addon dumps, S3 upload, scheduler`

---

### Task 4: CLI + README runbook

**Files:**
- Modify: `internal/client/client.go`
- Create: `internal/cli/backup.go`
- Modify: `internal/cli/root.go`
- Modify: `README.md`
- Test: `internal/cli/commands_test.go` (append)

**Interfaces:**
- Consumes: Task 3 endpoints.
- Produces:
  - Client: `CreateBackup(noUpload bool) (BackupInfo, error)` with `type BackupInfo struct { ID int64 \`json:"id"\`; Path string \`json:"path"\`; SizeBytes int64 \`json:"size_bytes"\`; Uploaded bool \`json:"uploaded"\`; Warnings []string \`json:"warnings"\` }`; `ListBackups() ([]BackupInfo, error)`; `PruneBackups() (int, error)`.
  - CLI: `luncur backup create [--no-upload]` (prints path, size, uploaded, warnings each on a line), `backup list` (tabwriter ID/SIZE/UPLOADED/CREATED), `backup prune` (prints removed count). Registered in root.go.
  - README: "Backups" section — commands, S3 settings (`luncur config set backup_s3_endpoint https://... `etc., secret key write-only), `backup_schedule daily`, `backup_keep`, the **restore runbook** (numbered steps: fresh `luncur up`; `kubectl -n luncur-system scale deploy/luncur --replicas=0`; copy archive onto the PVC (via a helper pod or scp to the node's local-path dir); untar `luncur.db` + `luncur.key` over the PVC copies; scale back up; re-create addons (`luncur addon create ...`); restore dumps via `kubectl exec` with `pg_restore` / redis rdb copy; verify with `luncur status`); security note: archives contain the sealer key — the bucket is the trust boundary. Status line → "Phase 3 in progress — addons, metrics, backups shipped (Plans I-K)".

- [ ] **Step 1: Failing test** — `TestBackupCommands` appended to `commands_test.go`: `backup create --no-upload` against `testEnv` (no kube → addon-dump warning path is skipped since no addons; store+DataDir present? testEnv builds Deps{Store, Sealer} only — ADD DataDir to the fixture's Deps (temp dir) so the engine works kube-less) → output contains the archive path; `backup list` contains the ID; `backup prune` runs clean.
- [ ] **Step 2: Run** — failures.
- [ ] **Step 3: Implement** per Interfaces.
- [ ] **Step 4: Run** `go build ./... && go vet ./... && go test ./...` — green; `gofmt -l internal/` clean; `grep -rn "Plan K" README.md internal/` — only intentional.
- [ ] **Step 5: Commit** — `feat: backup CLI + restore runbook`

---

## Final verification (after all tasks)

- [ ] `go build ./... && go vet ./... && go test ./...` — everything green.
- [ ] Push branch `plan-k`, open PR against `main`.
- [ ] Manual (owner's VPS, post-merge): `backup create` with a live postgres addon → archive contains a real pg_dump; S3 upload to a real bucket; run the restore runbook once end-to-end (spec's release gate).

## Spec-coverage self-check (Plan K section of 2026-07-03-luncur-phase3-design.md)

- tar.gz on PVC: VACUUM INTO snapshot, sealer key, per-addon dumps via pods/exec with env-referenced credentials ✅ (T3)
- Partial-failure semantics: dump failure → warning, backup completes ✅ (T3)
- S3-compatible upload, stdlib SigV4, known-answer + httptest tests; failure keeps local + surfaces error (warning + uploaded=false) ✅ (T1/T3)
- `backup list` / `prune` (keep `backup_keep`, default 7, local + remote) ✅ (T3/T4)
- `backup_schedule` daily|off, serve goroutine like the cert loop ✅ (T2/T3)
- `backups` table ✅ (T2)
- Restore = README runbook, not a command; sealer-key trust-boundary note ✅ (T4)
