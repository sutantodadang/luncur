# luncur Plan D — `luncur up`, live logs, web UI, status, token expiry

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Finish Phase 1 of the luncur spec: the `luncur up` installer, live (SSE) build + runtime logs, a minimal web UI, `luncur status`, and API-token expiry.

**Architecture:** Everything stays in the one Go binary. The kube client gains a typed clientset (pod logs, node IPs, rollout waits) alongside the existing dynamic client. Log streaming uses Server-Sent Events over the existing REST API; the CLI and the web UI both consume the same SSE endpoints (the `authed` middleware learns to accept a session cookie as well as a bearer token). The web UI is server-rendered `html/template` pages embedded via `embed.FS`, with one small vanilla-JS `EventSource` hook for live logs. `luncur up` is a step-scoped, idempotent installer: ensure K3s, write `registries.yaml`, apply system + luncur manifests with server-side apply, bootstrap the admin, mint a CLI token.

**Tech Stack:** Go stdlib (`html/template`, `net/http`, `crypto/rand`), client-go (`kubernetes.Interface` added), modernc.org/sqlite, cobra.

## Global Constraints

- Single Go module, one binary from `cmd/luncur`. No new third-party dependencies beyond what `go.mod` already has (client-go's `kubernetes` package is part of the existing client-go dependency).
- Server-side apply everywhere, `fieldManager=luncur` (existing `kube.applyOpts`).
- API errors always use the envelope `{"error":{"code":"...","message":"..."}}` via `writeError`.
- All commits conventional-commit style; run `go build ./... && go vet ./... && go test ./...` before every commit.
- Windows dev box: tests must not require a cluster, root, or Linux — everything cluster-shaped is tested with `k8s.io/client-go/dynamic/fake` and `k8s.io/client-go/kubernetes/fake`.

**Approved deviations from the design spec (record in README):**
- Web UI uses stdlib `html/template` + one vanilla-JS `EventSource` block instead of templ + HTMX. Zero codegen, zero vendored JS; same server-rendered pages + SSE behavior. Upgrade path: swap templates for templ in Phase 2 when the YAML editor lands.
- Public-IP detection: node `ExternalIP` → node `InternalIP` → `--ip` flag. No outbound HTTP probe (node addresses cover the VPS case; `--ip` covers the rest).
- CSRF protection for UI forms = `SameSite=Strict` session cookie + POST-only mutations. Token-based CSRF is Phase 2.
- In-cluster registry is reachable from containerd via a **NodePort (30500)** + `registries.yaml` mirror to `http://127.0.0.1:30500` (containerd on the node cannot resolve cluster-DNS names like `registry.luncur-system`).
- `luncur status` with no app argument lists apps in the project (name + URL); per-app statuses require `luncur status <app>`.
- Token lifecycle Phase 1 = expiry enforcement only (90 days). `luncur token list/revoke` is Phase 2.

---

### Task 1: API-token expiry

**Files:**
- Modify: `internal/store/schema.sql` (api_tokens table)
- Modify: `internal/store/store.go` (post-schema migration)
- Modify: `internal/store/tokens.go`
- Test: `internal/store/tokens_test.go` (exists — add cases)

**Interfaces:**
- Consumes: existing `Store.CreateToken(userID int64, name string) (string, error)`, `Store.UserForToken(plaintext string) (User, error)`.
- Produces: same signatures; new tokens carry `expires_at = now + 90 days`; expired tokens fail auth with `ErrAuthFailed`. Also `migrate(db *sql.DB) error` (unexported, called from `Open`).

- [ ] **Step 1: Write the failing test** (append to `internal/store/tokens_test.go`, matching its existing style — it opens a temp-file store and creates a user):

```go
func TestExpiredTokenRejected(t *testing.T) {
	st := testStore(t) // reuse the file's existing helper for opening a temp store
	u, err := st.CreateUser("tok@example.com", "password123", "member")
	if err != nil {
		t.Fatal(err)
	}
	tok, err := st.CreateToken(u.ID, "test")
	if err != nil {
		t.Fatal(err)
	}
	// Fresh token authenticates and has an expiry ~90 days out.
	if _, err := st.UserForToken(tok); err != nil {
		t.Fatalf("fresh token rejected: %v", err)
	}
	var exp string
	if err := st.DB().QueryRow(`SELECT expires_at FROM api_tokens WHERE name = 'test'`).Scan(&exp); err != nil {
		t.Fatalf("expires_at not set: %v", err)
	}
	// Force it into the past; auth must now fail.
	if _, err := st.DB().Exec(`UPDATE api_tokens SET expires_at = datetime('now', '-1 day')`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.UserForToken(tok); !errors.Is(err, ErrAuthFailed) {
		t.Fatalf("expired token: got %v, want ErrAuthFailed", err)
	}
}
```

If `tokens_test.go` has no shared `testStore` helper, use whatever pattern its existing tests use to open a store (do not invent a new helper style).

- [ ] **Step 2: Run it, expect failure** — `go test ./internal/store/ -run TestExpiredTokenRejected -v` fails: `expires_at` column missing / not set.

- [ ] **Step 3: Implement.**

`internal/store/schema.sql` — add the column to the CREATE TABLE (fresh DBs):

```sql
CREATE TABLE IF NOT EXISTS api_tokens (
  id           INTEGER PRIMARY KEY,
  user_id      INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  hash         TEXT NOT NULL UNIQUE,
  name         TEXT NOT NULL,
  last_used_at TEXT,
  expires_at   TEXT,
  created_at   TEXT NOT NULL DEFAULT (datetime('now'))
);
```

`internal/store/store.go` — `schema.sql` only creates missing tables, so existing DBs need an explicit ALTER. In `Open`, after the `db.Exec(schemaSQL)` block:

```go
	if err := migrate(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
```

and add:

```go
// migrate adds columns introduced after a table first shipped; schema.sql
// only creates missing tables, so pre-existing DBs need explicit ALTERs.
func migrate(db *sql.DB) error {
	var n int
	if err := db.QueryRow(
		`SELECT count(*) FROM pragma_table_info('api_tokens') WHERE name = 'expires_at'`,
	).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		if _, err := db.Exec(`ALTER TABLE api_tokens ADD COLUMN expires_at TEXT`); err != nil {
			return err
		}
	}
	return nil
}
```

`internal/store/tokens.go` — expiry on mint, enforcement on lookup:

```go
// CreateToken: INSERT gains expires_at (90-day lifetime; NULL rows from
// older DBs keep working — treated as non-expiring).
	_, err := s.db.Exec(
		`INSERT INTO api_tokens (user_id, hash, name, expires_at)
		 VALUES (?, ?, ?, datetime('now', '+90 days'))`,
		userID, hex.EncodeToString(sum[:]), name,
	)
```

```go
// UserForToken: WHERE clause gains the expiry guard.
	err := s.db.QueryRow(
		`SELECT u.id, u.email, u.role FROM api_tokens t
		 JOIN users u ON u.id = t.user_id
		 WHERE t.hash = ? AND (t.expires_at IS NULL OR t.expires_at > datetime('now'))`, h,
	).Scan(&u.ID, &u.Email, &u.Role)
```

- [ ] **Step 4: Run** `go test ./internal/store/ -v` — all pass.
- [ ] **Step 5: Commit** — `feat: api tokens expire after 90 days`

---

### Task 2: kube client — typed clientset (pods, logs, rollout wait, node IP)

**Files:**
- Modify: `internal/kube/kube.go`
- Test: `internal/kube/kube_test.go` (exists — add cases; follow its fake-dynamic patterns)

**Interfaces:**
- Consumes: existing `Client{dyn dynamic.Interface}`, `gvrByKind`, `WaitJob` polling pattern.
- Produces (all on `*Client`):
  - `New(kubeconfig string) (*Client, error)` — unchanged signature, now also builds `cs kubernetes.Interface`.
  - `NewForTest(dyn dynamic.Interface, cs kubernetes.Interface) *Client`
  - `AppPods(ctx context.Context, namespace, app string) ([]string, error)` — pod names with label `app.kubernetes.io/name=<app>`.
  - `PodLogStream(ctx context.Context, namespace, pod string, follow bool) (io.ReadCloser, error)`
  - `WaitDeployment(ctx context.Context, namespace, name string, poll time.Duration) error` — until `status.readyReplicas >= 1`.
  - `NodeIP(ctx context.Context) (string, error)` — first node `ExternalIP`, falling back to `InternalIP`.
  - `GetSecretData(ctx context.Context, namespace, name string) (map[string][]byte, error)` — nil map + no error when NotFound.
  - `gvrByKind` gains `ServiceAccount` and `ClusterRoleBinding` entries.

- [ ] **Step 1: Write the failing tests** (append to `kube_test.go`; reuse its existing fake-dynamic scheme setup for the dynamic parts, `k8s.io/client-go/kubernetes/fake` for the typed parts):

```go
func TestAppPodsAndNodeIP(t *testing.T) {
	cs := k8sfake.NewSimpleClientset(
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{
			Name: "web-1", Namespace: "proj",
			Labels: map[string]string{"app.kubernetes.io/name": "web"},
		}},
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{
			Name: "other-1", Namespace: "proj",
			Labels: map[string]string{"app.kubernetes.io/name": "other"},
		}},
		&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: "n1"},
			Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{
				{Type: corev1.NodeInternalIP, Address: "10.0.0.5"},
				{Type: corev1.NodeExternalIP, Address: "203.0.113.9"},
			}},
		},
	)
	c := NewForTest(nil, cs)

	pods, err := c.AppPods(context.Background(), "proj", "web")
	if err != nil {
		t.Fatal(err)
	}
	if len(pods) != 1 || pods[0] != "web-1" {
		t.Fatalf("pods = %v, want [web-1]", pods)
	}

	ip, err := c.NodeIP(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if ip != "203.0.113.9" {
		t.Fatalf("ip = %q, want ExternalIP preferred", ip)
	}
}

func TestWaitDeployment(t *testing.T) {
	dep := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apps/v1", "kind": "Deployment",
		"metadata": map[string]any{"name": "luncur", "namespace": "luncur-system"},
		"status":   map[string]any{"readyReplicas": int64(1)},
	}}
	dyn := newFakeDyn(t, dep) // reuse the file's existing fake-dynamic constructor pattern
	c := NewForTest(dyn, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := c.WaitDeployment(ctx, "luncur-system", "luncur", 10*time.Millisecond); err != nil {
		t.Fatal(err)
	}
}
```

Adapt `newFakeDyn` to however `kube_test.go` already constructs its fake dynamic client (scheme + list-kinds map) — do not duplicate scheme setup if a helper exists.

- [ ] **Step 2: Run** `go test ./internal/kube/ -run 'TestAppPods|TestWaitDeployment' -v` — compile failure (`NewForTest` undefined).

- [ ] **Step 3: Implement** in `internal/kube/kube.go`:

```go
// imports add: "io", corev1 "k8s.io/api/core/v1", "k8s.io/client-go/kubernetes"

type Client struct {
	dyn dynamic.Interface
	cs  kubernetes.Interface
}

// gvrByKind additions:
	"ServiceAccount":         {Group: "", Version: "v1", Resource: "serviceaccounts"},
	"ClusterRoleBinding":     {Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterrolebindings"},

// New: after dynamic.NewForConfig succeeds:
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	return &Client{dyn: dyn, cs: cs}, nil

func NewFromDynamic(dyn dynamic.Interface) *Client { return &Client{dyn: dyn} }

// NewForTest wires both halves explicitly; either may be nil.
func NewForTest(dyn dynamic.Interface, cs kubernetes.Interface) *Client {
	return &Client{dyn: dyn, cs: cs}
}

// AppPods lists pod names carrying the app label Render stamps on
// every workload (app.kubernetes.io/name=<app>).
func (c *Client) AppPods(ctx context.Context, namespace, app string) ([]string, error) {
	list, err := c.cs.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "app.kubernetes.io/name=" + app,
	})
	if err != nil {
		return nil, fmt.Errorf("list pods: %w", err)
	}
	names := make([]string, 0, len(list.Items))
	for _, p := range list.Items {
		names = append(names, p.Name)
	}
	return names, nil
}

func (c *Client) PodLogStream(ctx context.Context, namespace, pod string, follow bool) (io.ReadCloser, error) {
	return c.cs.CoreV1().Pods(namespace).GetLogs(pod, &corev1.PodLogOptions{Follow: follow}).Stream(ctx)
}

// WaitDeployment polls until the Deployment has at least one ready replica
// or ctx ends. Same shape as WaitJob.
func (c *Client) WaitDeployment(ctx context.Context, namespace, name string, poll time.Duration) error {
	for {
		u, err := c.dyn.Resource(gvrByKind["Deployment"]).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
		if err == nil {
			if n, _, _ := unstructured.NestedInt64(u.Object, "status", "readyReplicas"); n >= 1 {
				return nil
			}
		} else if !apierrors.IsNotFound(err) {
			return fmt.Errorf("get deployment %s: %w", name, err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(poll):
		}
	}
}

// NodeIP returns the first node's ExternalIP, falling back to InternalIP.
// Single-node K3s is the Phase 1 target, so "first node" is the node.
func (c *Client) NodeIP(ctx context.Context) (string, error) {
	nodes, err := c.cs.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return "", fmt.Errorf("list nodes: %w", err)
	}
	var internal string
	for _, n := range nodes.Items {
		for _, a := range n.Status.Addresses {
			switch a.Type {
			case corev1.NodeExternalIP:
				return a.Address, nil
			case corev1.NodeInternalIP:
				if internal == "" {
					internal = a.Address
				}
			}
		}
	}
	if internal != "" {
		return internal, nil
	}
	return "", fmt.Errorf("no node addresses found")
}

// GetSecretData reads a Secret's decoded data; nil map when it doesn't exist.
func (c *Client) GetSecretData(ctx context.Context, namespace, name string) (map[string][]byte, error) {
	sec, err := c.cs.CoreV1().Secrets(namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return sec.Data, nil
}
```

- [ ] **Step 4: Run** `go test ./internal/kube/ -v` — all pass (existing tests too).
- [ ] **Step 5: Commit** — `feat: kube clientset — pod logs, rollout wait, node IP, secret read`

---

### Task 3: store.ListDeployments (deploy history)

**Files:**
- Modify: `internal/store/deployments.go`
- Test: `internal/store/deployments_test.go` (append)

**Interfaces:**
- Produces: `Store.ListDeployments(appID int64) ([]Deployment, error)` — newest first, capped at 50.

- [ ] **Step 1: Failing test** (append, following the file's existing fixture style):

```go
func TestListDeployments(t *testing.T) {
	st := testStore(t) // or the file's existing store+app fixture pattern
	// create a project + app the way the file's other tests do, then:
	d1, _ := st.CreateDeployment(app.ID, "failed", "img:1", 0)
	d2, _ := st.CreateDeployment(app.ID, "live", "img:2", 0)
	list, err := st.ListDeployments(app.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].ID != d2.ID || list[1].ID != d1.ID {
		t.Fatalf("want [d2 d1] newest-first, got %+v", list)
	}
}
```

- [ ] **Step 2: Run** — fails to compile (`ListDeployments` undefined).
- [ ] **Step 3: Implement:**

```go
// ListDeployments returns an app's deploy history, newest first.
// ponytail: hard cap 50 — paging when someone actually has 51 deploys to read.
func (s *Store) ListDeployments(appID int64) ([]Deployment, error) {
	rows, err := s.db.Query(
		`SELECT id, app_id, status, image_ref, log_path, created_by, created_at
		 FROM deployments WHERE app_id = ? ORDER BY id DESC LIMIT 50`, appID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Deployment
	for rows.Next() {
		var d Deployment
		var img, logp sql.NullString
		if err := rows.Scan(&d.ID, &d.AppID, &d.Status, &img, &logp, &d.CreatedBy, &d.CreatedAt); err != nil {
			return nil, err
		}
		d.ImageRef, d.LogPath = img.String, logp.String
		out = append(out, d)
	}
	return out, rows.Err()
}
```

- [ ] **Step 4: Run** `go test ./internal/store/ -v` — pass.
- [ ] **Step 5: Commit** — `feat: deploy history listing in store`

---

### Task 4: SSE build-log follow (`?follow=1`)

**Files:**
- Create: `internal/server/sse.go`
- Modify: `internal/server/apps.go:317-343` (`handleDeployLogs`)
- Test: `internal/server/sse_test.go`

**Interfaces:**
- Consumes: `s.src.LogPath(id)`, `s.st.GetDeployment(id)`, existing `handleDeployLogs` auth chain.
- Produces:
  - `GET /v1/.../deploys/{id}/logs?follow=1` → `text/event-stream`; log lines as `data:` events; a final `event: end` + `data: <status>` when the deployment reaches `live`/`failed` and the file is drained. Without `follow=1` behavior is unchanged.
  - Helpers in `sse.go` used again by Task 5: `sseStart(w http.ResponseWriter) (http.Flusher, bool)`, `sseData(w http.ResponseWriter, fl http.Flusher, line string)`, `sseEnd(w http.ResponseWriter, fl http.Flusher, msg string)`.

- [ ] **Step 1: Failing test** in `internal/server/sse_test.go` (use the package's existing test bootstrap — most server tests build `Deps` with a temp store and call `New(...)` or `newServer(...)`; mirror that):

```go
func TestDeployLogsFollow(t *testing.T) {
	// Arrange: temp store + data dir; project, app, deployment already in
	// terminal state with a written log file — the stream should replay the
	// file then emit the end event immediately.
	//   dataDir := t.TempDir()
	//   write dataDir/logs/<id>.log containing "line one\nline two\n"
	//   deployment status = "failed"
	// Act: GET .../deploys/{id}/logs?follow=1 with a valid bearer token.
	// Assert on the raw body:
	body := rec.Body.String()
	for _, want := range []string{
		"data: line one\n",
		"data: line two\n",
		"event: end\n",
		"data: failed\n",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("body missing %q:\n%s", want, body)
		}
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type = %q", ct)
	}
}
```

(`httptest.NewRecorder` implements `http.Flusher`, so the handler can be exercised without a real server. Reuse the package's existing helpers for creating a user/token/project/app/deployment — `apps_test.go` and `build_test.go` show the pattern.)

- [ ] **Step 2: Run** `go test ./internal/server/ -run TestDeployLogsFollow -v` — fails (plain text response, no SSE).

- [ ] **Step 3: Implement.** `internal/server/sse.go`:

```go
package server

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// sseStart switches the response into event-stream mode. Returns false
// (having written a 500) when the writer can't flush.
func sseStart(w http.ResponseWriter) (http.Flusher, bool) {
	fl, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "internal", "streaming unsupported")
		return nil, false
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	return fl, true
}

func sseData(w http.ResponseWriter, fl http.Flusher, line string) {
	fmt.Fprintf(w, "data: %s\n\n", strings.TrimRight(line, "\r\n"))
	fl.Flush()
}

func sseEnd(w http.ResponseWriter, fl http.Flusher, msg string) {
	fmt.Fprintf(w, "event: end\ndata: %s\n\n", msg)
	fl.Flush()
}

// followFile tails path from offset, emitting complete lines as SSE data
// events. done() is polled between reads; when it reports true AND the
// file is drained, the final status is sent as the end event.
func (s *server) followFile(w http.ResponseWriter, fl http.Flusher, r *http.Request, path string, done func() (bool, string)) {
	var off int64
	var partial string
	for {
		f, err := os.Open(path)
		if err == nil {
			if _, err = f.Seek(off, io.SeekStart); err == nil {
				b, _ := io.ReadAll(f)
				off += int64(len(b))
				partial += string(b)
				for {
					line, rest, found := strings.Cut(partial, "\n")
					if !found {
						break
					}
					sseData(w, fl, line)
					partial = rest
				}
			}
			f.Close()
		}
		finished, status := done()
		if finished {
			if partial != "" {
				sseData(w, fl, partial)
			}
			sseEnd(w, fl, status)
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-time.After(500 * time.Millisecond):
		}
	}
}
```

`internal/server/apps.go` — in `handleDeployLogs`, after the `s.src == nil` guard, branch on follow:

```go
	if r.URL.Query().Get("follow") == "1" {
		fl, ok := sseStart(w)
		if !ok {
			return
		}
		s.followFile(w, fl, r, s.src.LogPath(d.ID), func() (bool, string) {
			cur, err := s.st.GetDeployment(d.ID)
			if err != nil {
				return true, "unknown"
			}
			return cur.Status == "live" || cur.Status == "failed", cur.Status
		})
		return
	}
```

Note the drain ordering: `followFile` reads the file **before** calling `done()`, so a terminal status never truncates the tail (bytes written before the status flip are already consumed; `partial` catches an unterminated last line).

- [ ] **Step 4: Run** `go test ./internal/server/ -v` — all pass.
- [ ] **Step 5: Commit** — `feat: SSE follow for build logs`

---

### Task 5: runtime pod logs endpoint

**Files:**
- Create: `internal/server/logs.go`
- Modify: `internal/server/server.go` (route)
- Test: `internal/server/logs_test.go`

**Interfaces:**
- Consumes: `kube.AppPods`, `kube.PodLogStream` (Task 2), SSE helpers (Task 4).
- Produces: `GET /v1/projects/{project}/apps/{app}/logs[?follow=1]` (authed) → SSE stream of pod log lines prefixed `[pod-name] `; `event: end` with `data: eof` when all pod streams finish; `404 no_pods` when the app has no running pods.

- [ ] **Step 1: Failing test** in `internal/server/logs_test.go`. The fake clientset serves canned `"fake logs"` for any pod-log request:

```go
func TestRuntimeLogs(t *testing.T) {
	// Arrange: server Deps with kube = kube.NewForTest(nil, k8sfake.NewSimpleClientset(pod))
	// where pod is in the project's namespace with label app.kubernetes.io/name=<app>.
	// Act: GET /v1/projects/p/apps/a/logs with a valid token.
	body := rec.Body.String()
	if !strings.Contains(body, "data: [web-1] fake logs") {
		t.Fatalf("missing pod log line:\n%s", body)
	}
	if !strings.Contains(body, "event: end") {
		t.Fatalf("missing end event:\n%s", body)
	}
}

func TestRuntimeLogsNoPods(t *testing.T) {
	// Same arrangement, no pods → expect 404 with code no_pods.
}
```

- [ ] **Step 2: Run** — 404 `not_found` (route missing).

- [ ] **Step 3: Implement.** `internal/server/logs.go`:

```go
package server

import (
	"bufio"
	"log"
	"net/http"
	"sync"

	"github.com/sutantodadang/luncur/internal/store"
)

// handleRuntimeLogs streams the app's pod logs as SSE, each line prefixed
// with its pod name. Follow mode holds the kube streams open.
func (s *server) handleRuntimeLogs(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.requireProject(w, u, r.PathValue("project"))
	if !ok {
		return
	}
	a, ok := s.requireApp(w, p, r.PathValue("app"))
	if !ok {
		return
	}
	if !s.requireKube(w) {
		return
	}

	follow := r.URL.Query().Get("follow") == "1"
	pods, err := s.kube.AppPods(r.Context(), p.Namespace, a.Name)
	if err != nil {
		log.Printf("list app pods: %v", err)
		writeError(w, http.StatusBadGateway, "kube_error", "could not list pods")
		return
	}
	if len(pods) == 0 {
		writeError(w, http.StatusNotFound, "no_pods", "app has no running pods")
		return
	}

	fl, ok := sseStart(w)
	if !ok {
		return
	}

	lines := make(chan string, 64)
	var wg sync.WaitGroup
	for _, pod := range pods {
		wg.Add(1)
		go func(pod string) {
			defer wg.Done()
			rc, err := s.kube.PodLogStream(r.Context(), p.Namespace, pod, follow)
			if err != nil {
				lines <- "[" + pod + "] error: " + err.Error()
				return
			}
			defer rc.Close()
			sc := bufio.NewScanner(rc)
			sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
			for sc.Scan() {
				lines <- "[" + pod + "] " + sc.Text()
			}
		}(pod)
	}
	go func() { wg.Wait(); close(lines) }()

	for {
		select {
		case line, more := <-lines:
			if !more {
				sseEnd(w, fl, "eof")
				return
			}
			sseData(w, fl, line)
		case <-r.Context().Done():
			return
		}
	}
}
```

Route in `server.go` `handler()` (next to the other app routes):

```go
	mux.HandleFunc("GET /v1/projects/{project}/apps/{app}/logs", s.authed(s.handleRuntimeLogs))
```

- [ ] **Step 4: Run** `go test ./internal/server/ -v` — pass.
- [ ] **Step 5: Commit** — `feat: runtime pod-log streaming endpoint`

---

### Task 6: client SSE consumption + CLI `logs -f` and `status`

**Files:**
- Modify: `internal/client/client.go`
- Modify: `internal/cli/logs.go`
- Create: `internal/cli/status.go`
- Modify: `internal/cli/root.go` (register `statusCmd()`)
- Test: `internal/client/client_test.go`, `internal/cli/commands_test.go` (append)

**Interfaces:**
- Consumes: SSE endpoints from Tasks 4-5; existing `Client.do/doRaw`, `GetApp`, `ListApps`, `AppInfo` (has `Name, Port, Replicas, URL` and — via GetApp — `Status, Image` fields; extend `AppInfo` with `Status string \`json:"status"\`` and `Image string \`json:"image"\`` if not present).
- Produces:
  - `Client.stream(path string, w io.Writer) error` (unexported) — GETs an SSE endpoint, writes each `data:` payload as a line to `w`, returns nil on `event: end`.
  - `Client.FollowDeployLogs(project, app string, id int64, w io.Writer) error`
  - `Client.RuntimeLogs(project, app string, follow bool, w io.Writer) error`
  - CLI: `luncur logs <app> --project P [--deploy N] [-f]` — `--deploy` no longer required; without it streams runtime logs; `-f` follows either mode.
  - CLI: `luncur status [app] --project P`.

- [ ] **Step 1: Failing client test** (append to `client_test.go`, mirroring its httptest style):

```go
func TestStreamSSE(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: hello\n\ndata: world\n\nevent: end\ndata: live\n\n")
	}))
	defer srv.Close()
	var buf bytes.Buffer
	c := New(srv.URL, "tok")
	if err := c.FollowDeployLogs("p", "a", 1, &buf); err != nil {
		t.Fatal(err)
	}
	if got := buf.String(); got != "hello\nworld\n" {
		t.Fatalf("got %q", got)
	}
}
```

- [ ] **Step 2: Run** — compile failure.

- [ ] **Step 3: Implement** in `client.go`:

```go
// stream consumes an SSE endpoint, writing each data payload as one line.
// A terminating "event: end" ends the stream cleanly; its data payload is
// the final status and is not written.
func (c *Client) stream(path string, w io.Writer) error {
	req, err := http.NewRequest("GET", c.base+path, nil)
	if err != nil {
		return err
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		var env struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(body, &env) == nil && env.Error.Code != "" {
			return fmt.Errorf("%s (%s)", env.Error.Message, env.Error.Code)
		}
		return fmt.Errorf("server returned %s", resp.Status)
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	ending := false
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "event: end":
			ending = true
		case strings.HasPrefix(line, "data: "):
			if ending {
				return nil
			}
			fmt.Fprintln(w, line[len("data: "):])
		}
	}
	return sc.Err()
}

func (c *Client) FollowDeployLogs(project, app string, id int64, w io.Writer) error {
	return c.stream(fmt.Sprintf("/v1/projects/%s/apps/%s/deploys/%d/logs?follow=1", project, app, id), w)
}

func (c *Client) RuntimeLogs(project, app string, follow bool, w io.Writer) error {
	p := fmt.Sprintf("/v1/projects/%s/apps/%s/logs", project, app)
	if follow {
		p += "?follow=1"
	}
	return c.stream(p, w)
}
```

(Add `bufio` and `strings` to imports. If `AppInfo` lacks `Status`/`Image`, add both fields.)

`internal/cli/logs.go` — rework:

```go
// logsCmd prints or follows logs. With --deploy it reads that deployment's
// build log (add -f to follow a build in progress); without it, it streams
// the app's runtime pod logs.
func logsCmd() *cobra.Command {
	var project string
	var deploy int64
	var follow bool
	cmd := &cobra.Command{
		Use:   "logs <app>",
		Short: "Print build or runtime logs",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient()
			if err != nil {
				return err
			}
			switch {
			case deploy != 0 && follow:
				return c.FollowDeployLogs(project, args[0], deploy, cmd.OutOrStdout())
			case deploy != 0:
				b, err := c.DeployLogs(project, args[0], deploy)
				if err != nil {
					return err
				}
				cmd.Print(string(b))
				return nil
			default:
				return c.RuntimeLogs(project, args[0], follow, cmd.OutOrStdout())
			}
		},
	}
	cmd.Flags().StringVar(&project, "project", "", "project name")
	cmd.MarkFlagRequired("project")
	cmd.Flags().Int64Var(&deploy, "deploy", 0, "deployment id (build log; omit for runtime logs)")
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "stream live")
	return cmd
}
```

`internal/cli/status.go`:

```go
package cli

import (
	"fmt"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

// statusCmd shows app status. With no app argument it lists the project's
// apps (name + URL); with one it shows the latest deployment detail.
func statusCmd() *cobra.Command {
	var project string
	cmd := &cobra.Command{
		Use:   "status [app]",
		Short: "Show app / deployment status",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient()
			if err != nil {
				return err
			}
			if len(args) == 1 {
				a, err := c.GetApp(project, args[0])
				if err != nil {
					return err
				}
				cmd.Printf("app:      %s\nstatus:   %s\nreplicas: %d\nimage:    %s\nurl:      %s\n",
					a.Name, a.Status, a.Replicas, a.Image, a.URL)
				return nil
			}
			apps, err := c.ListApps(project)
			if err != nil {
				return err
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "NAME\tREPLICAS\tURL")
			for _, a := range apps {
				fmt.Fprintf(tw, "%s\t%d\t%s\n", a.Name, a.Replicas, a.URL)
			}
			return tw.Flush()
		},
	}
	cmd.Flags().StringVar(&project, "project", "", "project name")
	cmd.MarkFlagRequired("project")
	return cmd
}
```

Register in `root.go`: `root.AddCommand(statusCmd())`.

- [ ] **Step 4: Add a CLI status test** to `commands_test.go` following its fake-server pattern (GET app returns status/replicas/url; assert output contains them), then run `go test ./internal/client/ ./internal/cli/ -v` — pass.
- [ ] **Step 5: Commit** — `feat: logs -f (SSE) and status commands`

---

### Task 7: web UI — session auth (login/logout, cookie-aware middleware)

**Files:**
- Modify: `internal/server/auth.go` (cookie fallback in `authed`)
- Create: `internal/server/ui.go`
- Create: `internal/server/templates/base.html`, `internal/server/templates/login.html`
- Modify: `internal/server/server.go` (embed templates, UI routes, `/` redirect)
- Test: `internal/server/ui_test.go`

**Interfaces:**
- Consumes: `store.Authenticate(email, password)`, `store.CreateToken`, `store.UserForToken`.
- Produces:
  - `authed` accepts EITHER `Authorization: Bearer <tok>` OR cookie `luncur_session=<tok>` (bearer wins when both present). API SSE endpoints thereby work from the browser.
  - `GET /ui/login` (form), `POST /ui/login` (sets cookie, 303 → `/ui/`), `POST /ui/logout` (clears cookie, 303 → `/ui/login`).
  - `s.uiUser(r *http.Request) (store.User, bool)` — resolves the session cookie.
  - `s.uiPage(next func(w, r, u store.User)) http.HandlerFunc` — like `authed` but redirects to `/ui/login` instead of writing JSON 401.
  - `s.renderPage(w http.ResponseWriter, page string, data any)` — executes `base.html` + named page template.
  - `GET /` redirects to `/ui/`; every other unmatched path keeps the JSON 404 envelope.

- [ ] **Step 1: Failing tests** in `ui_test.go`:

```go
func TestUILoginFlow(t *testing.T) {
	// Arrange: store with user u@example.com / password123, handler := New(deps).
	// 1. GET /ui/ without cookie → 303 Location /ui/login
	// 2. POST /ui/login form email/password → 303 Location /ui/, Set-Cookie luncur_session
	//    with HttpOnly and SameSite=Strict.
	// 3. GET /ui/ with that cookie → 200, body contains "Projects".
	// 4. API also accepts the cookie: GET /v1/me with only the cookie → 200.
}

func TestUILoginBadPassword(t *testing.T) {
	// POST /ui/login with wrong password → 200 login page re-rendered
	// containing "wrong email or password", no Set-Cookie.
}
```

Write these as real assertions (status codes, `Location` header, `Set-Cookie` inspection via `http.Response.Cookies()` on `rec.Result()`).

- [ ] **Step 2: Run** — fails (routes missing).

- [ ] **Step 3: Implement.**

`auth.go` — replace the token extraction inside `authed` (keep the rest):

```go
	return func(w http.ResponseWriter, r *http.Request) {
		const prefix = "Bearer "
		var tok string
		if h := r.Header.Get("Authorization"); len(h) > len(prefix) && h[:len(prefix)] == prefix {
			tok = h[len(prefix):]
		} else if ck, err := r.Cookie("luncur_session"); err == nil {
			tok = ck.Value
		}
		if tok == "" {
			writeError(w, http.StatusUnauthorized, "unauthorized", "missing bearer token or session")
			return
		}
		u, err := s.st.UserForToken(tok)
		if err != nil {
			writeError(w, http.StatusUnauthorized, "unauthorized", "invalid token")
			return
		}
		next(w, r, u)
	}
```

`server.go` — embed + parse templates and mount UI routes:

```go
// imports add: "embed", "html/template"

//go:embed templates/*.html
var templateFS embed.FS

// in type server: add field
	tmpl *template.Template

// in newServer, before return:
	s.tmpl = template.Must(template.ParseFS(templateFS, "templates/*.html"))

// in handler(), after the API routes:
	s.uiRoutes(mux)

// replace the "/" fallback:
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			http.Redirect(w, r, "/ui/", http.StatusSeeOther)
			return
		}
		writeError(w, http.StatusNotFound, "not_found", "no such endpoint")
	})
```

`internal/server/ui.go`:

```go
package server

import (
	"errors"
	"log"
	"net/http"
	"time"

	"github.com/sutantodadang/luncur/internal/store"
)

const sessionCookie = "luncur_session"

func (s *server) uiRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /ui/login", s.handleUILoginPage)
	mux.HandleFunc("POST /ui/login", s.handleUILogin)
	mux.HandleFunc("POST /ui/logout", s.handleUILogout)
	mux.HandleFunc("GET /ui/", s.uiPage(s.handleUIProjects)) // Task 8 fills this in
}

// uiUser resolves the session cookie to a user.
func (s *server) uiUser(r *http.Request) (store.User, bool) {
	ck, err := r.Cookie(sessionCookie)
	if err != nil {
		return store.User{}, false
	}
	u, err := s.st.UserForToken(ck.Value)
	if err != nil {
		return store.User{}, false
	}
	return u, true
}

// uiPage is authed's HTML twin: unauthenticated browsers get redirected to
// the login form instead of a JSON 401.
func (s *server) uiPage(next func(http.ResponseWriter, *http.Request, store.User)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u, ok := s.uiUser(r)
		if !ok {
			http.Redirect(w, r, "/ui/login", http.StatusSeeOther)
			return
		}
		next(w, r, u)
	}
}

func (s *server) renderPage(w http.ResponseWriter, page string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.ExecuteTemplate(w, page, data); err != nil {
		log.Printf("render %s: %v", page, err)
	}
}

func (s *server) handleUILoginPage(w http.ResponseWriter, r *http.Request) {
	s.renderPage(w, "login.html", map[string]any{})
}

func (s *server) handleUILogin(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.renderPage(w, "login.html", map[string]any{"Error": "invalid form"})
		return
	}
	u, err := s.st.Authenticate(r.PostFormValue("email"), r.PostFormValue("password"))
	if errors.Is(err, store.ErrAuthFailed) {
		s.renderPage(w, "login.html", map[string]any{"Error": "wrong email or password"})
		return
	}
	if err != nil {
		log.Printf("ui login: %v", err)
		s.renderPage(w, "login.html", map[string]any{"Error": "internal error"})
		return
	}
	tok, err := s.st.CreateToken(u.ID, "session")
	if err != nil {
		log.Printf("ui session token: %v", err)
		s.renderPage(w, "login.html", map[string]any{"Error": "internal error"})
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: tok, Path: "/",
		HttpOnly: true, SameSite: http.SameSiteStrictMode,
		Expires: time.Now().Add(7 * 24 * time.Hour),
	})
	http.Redirect(w, r, "/ui/", http.StatusSeeOther)
}

func (s *server) handleUILogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: "", Path: "/", HttpOnly: true, MaxAge: -1,
	})
	http.Redirect(w, r, "/ui/login", http.StatusSeeOther)
}
```

For this task, add a temporary minimal `handleUIProjects` (Task 8 replaces it):

```go
func (s *server) handleUIProjects(w http.ResponseWriter, r *http.Request, u store.User) {
	s.renderPage(w, "projects.html", map[string]any{"User": u, "Projects": nil})
}
```

`templates/base.html`:

```html
{{define "head"}}
<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>luncur</title>
<style>
body{font-family:system-ui,sans-serif;max-width:60rem;margin:2rem auto;padding:0 1rem;color:#222}
nav{display:flex;justify-content:space-between;border-bottom:1px solid #ddd;padding-bottom:.5rem;margin-bottom:1.5rem}
table{border-collapse:collapse;width:100%}
th,td{text-align:left;padding:.4rem .6rem;border-bottom:1px solid #eee}
form.inline{display:inline}
input,button{font:inherit;padding:.3rem .5rem}
pre.logs{background:#111;color:#ddd;padding:1rem;overflow:auto;max-height:24rem;font-size:.85rem}
.err{color:#b00}
.status-live{color:#080}.status-failed{color:#b00}.status-building,.status-deploying{color:#a60}
</style>
</head>
<body>
{{end}}

{{define "nav"}}
<nav>
  <strong><a href="/ui/">luncur</a></strong>
  <form class="inline" method="post" action="/ui/logout">
    <span>{{.User.Email}}</span> <button type="submit">log out</button>
  </form>
</nav>
{{end}}

{{define "foot"}}
</body>
</html>
{{end}}
```

`templates/login.html`:

```html
{{define "login.html"}}
{{template "head" .}}
<h1>luncur</h1>
{{if .Error}}<p class="err">{{.Error}}</p>{{end}}
<form method="post" action="/ui/login">
  <p><label>email <input type="email" name="email" required autofocus></label></p>
  <p><label>password <input type="password" name="password" required></label></p>
  <p><button type="submit">log in</button></p>
</form>
{{template "foot" .}}
{{end}}
```

Also create a placeholder `templates/projects.html` so parsing succeeds (Task 8 replaces the body):

```html
{{define "projects.html"}}
{{template "head" .}}{{template "nav" .}}
<h1>Projects</h1>
{{template "foot" .}}
{{end}}
```

- [ ] **Step 4: Run** `go test ./internal/server/ -v` — pass (including the older auth tests, which still use bearer tokens).
- [ ] **Step 5: Commit** — `feat: web UI session auth (login/logout, cookie-aware API auth)`

---

### Task 8: web UI — project list, app list, app detail + actions

**Files:**
- Modify: `internal/server/ui.go` (page handlers + action handlers + routes)
- Modify: `internal/server/projects.go` (extract `visibleProjects` helper from `handleListProjects`)
- Create: `internal/server/templates/projects.html` (replace placeholder), `templates/apps.html`, `templates/app.html`
- Test: `internal/server/ui_test.go` (append)

**Interfaces:**
- Consumes: `visibleProjects(u store.User) ([]store.Project, error)` — extract the exact project-visibility logic `handleListProjects` already implements (admin sees all, member sees membership) into this method and call it from both places. Also `store.ListApps`, `store.GetApp`, `store.LatestDeployment`, `store.ListDeployments` (Task 3), `store.ListEnv`/env store methods as used by `appenv.go`, `store.SetReplicas` + `s.syncApp` (mirror `handleScaleApp` semantics), `s.startBuild` (git apps).
- Produces routes (all `s.uiPage(...)`):
  - `GET /ui/` — project list.
  - `GET /ui/projects/{project}` — app table (name, replicas, URL).
  - `GET /ui/projects/{project}/apps/{app}` — detail: status, URL, image, deploy history table, env var keys + set/unset forms, scale form, "Deploy from git" button (git apps only), raw-YAML link, logs section (filled by Task 9).
  - `POST /ui/projects/{project}/apps/{app}/scale` — form field `replicas`; mirrors `handleScaleApp` ordering (live-check before persisting); redirects back.
  - `POST /ui/projects/{project}/apps/{app}/env` — form fields `key`, `value`; reuse the same store/sync path `handleSetEnv` uses; redirect back.
  - `POST /ui/projects/{project}/apps/{app}/env/delete` — form field `key`; mirror `handleUnsetEnv`; redirect back.
  - `POST /ui/projects/{project}/apps/{app}/deploy` — git-source apps only: create `building` deployment, `s.startBuild`, redirect back; non-git apps → redirect back (UI hides the button anyway).
- UI membership rule: reuse `requireProject`-equivalent checks — factor a `uiProject(w, r, u) (store.Project, bool)` and `uiApp(w, r, p) (store.App, bool)` pair that mirror `requireProject`/`requireApp` but respond with `http.Error(w, "...", 404)` plain text (browser context). Never let a member see a project they don't belong to.

- [ ] **Step 1: Failing tests** (append to `ui_test.go`): a logged-in cookie session can (1) see a created project on `/ui/`, (2) see an app on the project page, (3) see app detail with its latest status, (4) POST scale → 303 and replicas persisted (use a nil-kube deps so app must be non-live — mirror `handleScaleApp` test setup from `apps_test.go`), (5) member user cannot see another project's page (404).

- [ ] **Step 2: Run** — failures/compile errors.

- [ ] **Step 3: Implement.** Key handler shapes (adapt store call names to what the files actually export — read `appenv.go` and `projects.go` first):

```go
// visibleProjects in projects.go — extracted verbatim from handleListProjects'
// body so the API handler and the UI share one visibility rule.
func (s *server) visibleProjects(u store.User) ([]store.Project, error) { ... }

func (s *server) handleUIProjects(w http.ResponseWriter, r *http.Request, u store.User) {
	list, err := s.visibleProjects(u)
	if err != nil {
		log.Printf("ui projects: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.renderPage(w, "projects.html", map[string]any{"User": u, "Projects": list})
}
```

App detail assembles one view-model map:

```go
	d, derr := s.st.LatestDeployment(a.ID)
	status := "never_deployed"
	if derr == nil {
		status = d.Status
	}
	history, _ := s.st.ListDeployments(a.ID)
	envKeys := ... // keys only, via the same store listing appenv.go uses; values stay sealed
	s.renderPage(w, "app.html", map[string]any{
		"User": u, "Project": p, "App": a,
		"Status": status, "URL": "http://" + hostFor(a.Name, s.externalIP),
		"History": history, "EnvKeys": envKeys,
		"IsGit": a.SourceType == "git",
	})
```

Templates — `projects.html`:

```html
{{define "projects.html"}}
{{template "head" .}}{{template "nav" .}}
<h1>Projects</h1>
{{if not .Projects}}<p>No projects yet. Create one with <code>luncur project create</code>.</p>{{end}}
<table><tr><th>Name</th><th>Namespace</th></tr>
{{range .Projects}}<tr><td><a href="/ui/projects/{{.Name}}">{{.Name}}</a></td><td>{{.Namespace}}</td></tr>{{end}}
</table>
{{template "foot" .}}
{{end}}
```

`apps.html`:

```html
{{define "apps.html"}}
{{template "head" .}}{{template "nav" .}}
<h1>{{.Project.Name}}</h1>
{{if not .Apps}}<p>No apps yet. Create one with <code>luncur app create</code>.</p>{{end}}
<table><tr><th>App</th><th>Replicas</th><th>URL</th></tr>
{{range .Apps}}<tr>
  <td><a href="/ui/projects/{{$.Project.Name}}/apps/{{.Name}}">{{.Name}}</a></td>
  <td>{{.Replicas}}</td>
  <td><a href="{{.URL}}">{{.URL}}</a></td>
</tr>{{end}}
</table>
{{template "foot" .}}
{{end}}
```

`app.html` (logs `<pre>` + script land in Task 9; include the empty `<pre id="logs">` now):

```html
{{define "app.html"}}
{{template "head" .}}{{template "nav" .}}
<p><a href="/ui/projects/{{.Project.Name}}">&larr; {{.Project.Name}}</a></p>
<h1>{{.App.Name}} <span class="status-{{.Status}}">{{.Status}}</span></h1>
<p><a href="{{.URL}}">{{.URL}}</a> &middot;
   <a href="/v1/projects/{{.Project.Name}}/apps/{{.App.Name}}/raw">raw YAML</a></p>

<h2>Scale</h2>
<form method="post" action="/ui/projects/{{.Project.Name}}/apps/{{.App.Name}}/scale">
  <input type="number" name="replicas" min="0" value="{{.App.Replicas}}">
  <button type="submit">scale</button>
</form>

{{if .IsGit}}
<h2>Deploy</h2>
<form method="post" action="/ui/projects/{{.Project.Name}}/apps/{{.App.Name}}/deploy">
  <button type="submit">Deploy from git</button>
</form>
{{end}}

<h2>Environment</h2>
<table>{{range .EnvKeys}}<tr><td><code>{{.}}</code></td><td>
  <form class="inline" method="post" action="/ui/projects/{{$.Project.Name}}/apps/{{$.App.Name}}/env/delete">
    <input type="hidden" name="key" value="{{.}}"><button type="submit">unset</button>
  </form></td></tr>{{end}}
</table>
<form method="post" action="/ui/projects/{{.Project.Name}}/apps/{{.App.Name}}/env">
  <input name="key" placeholder="KEY" required>
  <input name="value" placeholder="value" required>
  <button type="submit">set</button>
</form>

<h2>Deploys</h2>
<table><tr><th>ID</th><th>Status</th><th>Image</th><th>When</th></tr>
{{range .History}}<tr>
  <td>{{.ID}}</td><td class="status-{{.Status}}">{{.Status}}</td>
  <td><code>{{.ImageRef}}</code></td><td>{{.CreatedAt}}</td>
</tr>{{end}}
</table>

<h2>Logs</h2>
<pre class="logs" id="logs" data-project="{{.Project.Name}}" data-app="{{.App.Name}}"></pre>
{{template "foot" .}}
{{end}}
```

Routes added in `uiRoutes`:

```go
	mux.HandleFunc("GET /ui/projects/{project}", s.uiPage(s.handleUIApps))
	mux.HandleFunc("GET /ui/projects/{project}/apps/{app}", s.uiPage(s.handleUIApp))
	mux.HandleFunc("POST /ui/projects/{project}/apps/{app}/scale", s.uiPage(s.handleUIScale))
	mux.HandleFunc("POST /ui/projects/{project}/apps/{app}/env", s.uiPage(s.handleUIEnvSet))
	mux.HandleFunc("POST /ui/projects/{project}/apps/{app}/env/delete", s.uiPage(s.handleUIEnvUnset))
	mux.HandleFunc("POST /ui/projects/{project}/apps/{app}/deploy", s.uiPage(s.handleUIDeploy))
```

Action handlers: parse the form, perform the same store+kube sequence the corresponding API handler performs (scale MUST keep the live-check-before-persist ordering from `handleScaleApp`; env set/unset MUST seal values with `s.sealer` exactly as `appenv.go` does and trigger the same `syncApp` when live), then `http.Redirect(w, r, "/ui/projects/"+p.Name+"/apps/"+a.Name, http.StatusSeeOther)`. Where the logic exceeds a few lines, extract the API handler's core into a shared unexported method (e.g. `s.scaleApp(ctx, p, a, replicas) error`) and call it from both handlers rather than duplicating.

- [ ] **Step 4: Run** `go test ./internal/server/ -v` — pass.
- [ ] **Step 5: Commit** — `feat: web UI pages — projects, apps, app detail with scale/env/deploy`

---

### Task 9: web UI — live logs (EventSource)

**Files:**
- Modify: `internal/server/templates/app.html` (script)
- Test: `internal/server/ui_test.go` (append: app page contains the EventSource script and log element)

**Interfaces:**
- Consumes: `GET /v1/.../logs?follow=1` and `GET /v1/.../deploys/{id}/logs?follow=1` (cookie-authenticated since Task 7).
- Produces: app detail page auto-streams runtime logs; when the latest deployment is `building`/`deploying` it streams that build's log instead.

- [ ] **Step 1: Failing test:** app page body contains `new EventSource` and `id="logs"`.

- [ ] **Step 2: Run** — fails.

- [ ] **Step 3: Implement** — append to `app.html` before `{{template "foot" .}}`; the server passes `"LatestID"` (latest deployment id, 0 when none) and `Status` already exists:

```html
<script>
(function () {
  var pre = document.getElementById("logs");
  var p = pre.dataset.project, a = pre.dataset.app;
  var status = "{{.Status}}", latest = {{.LatestID}};
  var url = "/v1/projects/" + p + "/apps/" + a + "/logs?follow=1";
  if ((status === "building" || status === "deploying") && latest > 0) {
    url = "/v1/projects/" + p + "/apps/" + a + "/deploys/" + latest + "/logs?follow=1";
  }
  var es = new EventSource(url);
  es.onmessage = function (e) {
    pre.textContent += e.data + "\n";
    pre.scrollTop = pre.scrollHeight;
  };
  es.addEventListener("end", function (e) {
    pre.textContent += "--- " + e.data + " ---\n";
    es.close();
  });
  es.onerror = function () { es.close(); };
})();
</script>
```

Add `"LatestID"` to the app-detail view model in `ui.go` (`d.ID` when `derr == nil`, else `0`).

- [ ] **Step 4: Run** `go test ./internal/server/ -v` — pass.
- [ ] **Step 5: Commit** — `feat: live log streaming in app page via EventSource`

---

### Task 10: `internal/up` — luncur's own manifests

**Files:**
- Create: `internal/up/manifests.go`
- Test: `internal/up/manifests_test.go`

**Interfaces:**
- Consumes: `render.Object`, k8s typed APIs already in go.mod (`appsv1`, `corev1`, `netv1`, `rbacv1 "k8s.io/api/rbac/v1"`).
- Produces:
  - `up.LuncurObjects(p Params) ([]render.Object, error)` with

```go
type Params struct {
	Image        string // luncur server image
	ExternalIP   string
	BuilderImage string
}
```

  Objects (namespace `luncur-system`, all labeled `app.kubernetes.io/managed-by: luncur`):
  1. `ServiceAccount` `luncur`
  2. `ClusterRoleBinding` `luncur-admin` → ClusterRole `cluster-admin`, subject SA `luncur/luncur-system`
  3. `Deployment` `luncur` — 1 replica, SA `luncur`, container `luncur` running `Image`, args
     `["serve","--listen",":8080","--db","/var/lib/luncur/luncur.db","--data-dir","/var/lib/luncur/data","--secret-key-file","/var/lib/luncur/luncur.key","--external-ip",p.ExternalIP,"--builder-image",p.BuilderImage,"--bootstrap-admin","$(BOOTSTRAP_ADMIN)"]`,
     env `BOOTSTRAP_ADMIN` from Secret `luncur-bootstrap` key `admin` (`SecretKeyRef`), port 8080, PVC `luncur-data` mounted at `/var/lib/luncur`, readinessProbe HTTP GET `/v1/health` port 8080.
  4. `Service` `luncur` — port 80 → targetPort 8080, selector `app.kubernetes.io/name: luncur`.
  5. `Ingress` `luncur` — host `panel.<ExternalIP>.sslip.io`, path `/` Prefix → Service `luncur:80`.
  - `up.PanelHost(ip string) string` → `"panel." + ip + ".sslip.io"`.
  - `up.BootstrapSecretName = "luncur-bootstrap"` (const).

- [ ] **Step 1: Failing test:**

```go
func TestLuncurObjects(t *testing.T) {
	objs, err := LuncurObjects(Params{
		Image: "ghcr.io/sutantodadang/luncur:v1", ExternalIP: "1.2.3.4",
		BuilderImage: "luncur/builder:v1",
	})
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]bool{}
	for _, o := range objs {
		kinds[o.Kind] = true
	}
	for _, k := range []string{"ServiceAccount", "ClusterRoleBinding", "Deployment", "Service", "Ingress"} {
		if !kinds[k] {
			t.Fatalf("missing kind %s", k)
		}
	}
	all := ""
	for _, o := range objs {
		all += string(o.JSON)
	}
	for _, want := range []string{
		"panel.1.2.3.4.sslip.io",
		"ghcr.io/sutantodadang/luncur:v1",
		"$(BOOTSTRAP_ADMIN)",
		"luncur-bootstrap",
		"cluster-admin",
		"/v1/health",
	} {
		if !strings.Contains(all, want) {
			t.Fatalf("manifests missing %q", want)
		}
	}
	if PanelHost("1.2.3.4") != "panel.1.2.3.4.sslip.io" {
		t.Fatal("PanelHost")
	}
}
```

- [ ] **Step 2: Run** `go test ./internal/up/ -v` — package doesn't exist.

- [ ] **Step 3: Implement** `manifests.go` following `build/infra.go`'s exact style (typed structs + `json.Marshal` into `render.Object`, an `add` closure, `ptr` helper). Ingress spec shape:

```go
	pathType := netv1.PathTypePrefix
	ing := &netv1.Ingress{
		TypeMeta:   metav1.TypeMeta{APIVersion: "networking.k8s.io/v1", Kind: "Ingress"},
		ObjectMeta: metav1.ObjectMeta{Name: "luncur", Namespace: systemNamespace, Labels: labels},
		Spec: netv1.IngressSpec{Rules: []netv1.IngressRule{{
			Host: PanelHost(p.ExternalIP),
			IngressRuleValue: netv1.IngressRuleValue{HTTP: &netv1.HTTPIngressRuleValue{
				Paths: []netv1.HTTPIngressPath{{
					Path: "/", PathType: &pathType,
					Backend: netv1.IngressBackend{Service: &netv1.IngressServiceBackend{
						Name: "luncur", Port: netv1.ServiceBackendPort{Number: 80},
					}},
				}},
			}},
		}}},
	}
```

ClusterRoleBinding:

```go
	crb := &rbacv1.ClusterRoleBinding{
		TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "ClusterRoleBinding"},
		ObjectMeta: metav1.ObjectMeta{Name: "luncur-admin", Labels: labels},
		// ponytail: cluster-admin — luncur manages arbitrary namespaces/CRDs;
		// a scoped ClusterRole is the Phase 2 hardening path.
		RoleRef:  rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: "cluster-admin"},
		Subjects: []rbacv1.Subject{{Kind: "ServiceAccount", Name: "luncur", Namespace: systemNamespace}},
	}
```

Deployment env-from-secret:

```go
	Env: []corev1.EnvVar{{
		Name: "BOOTSTRAP_ADMIN",
		ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: BootstrapSecretName},
			Key:                  "admin",
		}},
	}},
```

(`systemNamespace` const = `"luncur-system"` — duplicate the const locally; don't import `build` for one string.)

- [ ] **Step 4: Run** `go test ./internal/up/ -v` — pass.
- [ ] **Step 5: Commit** — `feat: luncur self-deploy manifests (SA, RBAC, Deployment, Service, Ingress)`

---

### Task 11: `internal/up` — host steps (K3s install, registries.yaml) + registry NodePort

**Files:**
- Create: `internal/up/host.go`
- Modify: `internal/build/infra.go` (registry Service → NodePort 30500)
- Test: `internal/up/host_test.go`, `internal/build/infra_test.go` (update)

**Interfaces:**
- Produces:

```go
// Runner shells out; swapped for a fake in tests.
type Runner interface {
	Run(name string, args ...string) ([]byte, error)
}

const K3sVersion = "v1.32.5+k3s1" // pinned; bumped deliberately, never floated
const K3sKubeconfig = "/etc/rancher/k3s/k3s.yaml"
const RegistriesPath = "/etc/rancher/k3s/registries.yaml"
const RegistryNodePort = 30500

// EnsureK3s installs K3s via the official script when the binary is absent.
// Returns whether an install ran.
func EnsureK3s(r Runner) (installed bool, err error)

// WriteRegistriesYAML writes the insecure-registry mirror config; returns
// whether the file changed (caller restarts k3s only then).
func WriteRegistriesYAML(path string) (changed bool, err error)

func RegistriesYAML() string // the exact file content (exported for the test)
```

- `build.SystemObjects`: registry `Service` becomes `Type: NodePort` with `NodePort: 30500` (containerd on the node reaches the in-cluster registry via `http://127.0.0.1:30500`; cluster-internal pulls keep resolving `registry.luncur-system:5000`).

- [ ] **Step 1: Failing tests:**

`internal/up/host_test.go`:

```go
type fakeRunner struct{ cmds [][]string }

func (f *fakeRunner) Run(name string, args ...string) ([]byte, error) {
	f.cmds = append(f.cmds, append([]string{name}, args...))
	if name == "which" { // k3s missing
		return nil, fmt.Errorf("not found")
	}
	return nil, nil
}

func TestEnsureK3sInstallsWhenMissing(t *testing.T) {
	f := &fakeRunner{}
	installed, err := EnsureK3s(f)
	if err != nil {
		t.Fatal(err)
	}
	if !installed {
		t.Fatal("expected install")
	}
	joined := fmt.Sprint(f.cmds)
	if !strings.Contains(joined, "get.k3s.io") || !strings.Contains(joined, K3sVersion) {
		t.Fatalf("install command wrong: %v", f.cmds)
	}
}

func TestWriteRegistriesYAML(t *testing.T) {
	p := filepath.Join(t.TempDir(), "registries.yaml")
	changed, err := WriteRegistriesYAML(p)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("first write must report changed")
	}
	b, _ := os.ReadFile(p)
	for _, want := range []string{"registry.luncur-system:5000", "http://127.0.0.1:30500"} {
		if !strings.Contains(string(b), want) {
			t.Fatalf("registries.yaml missing %q:\n%s", want, b)
		}
	}
	changed, err = WriteRegistriesYAML(p)
	if err != nil || changed {
		t.Fatalf("second write: changed=%v err=%v, want false nil", changed, err)
	}
}
```

`internal/build/infra_test.go` — extend the existing SystemObjects test: the registry Service JSON contains `"type":"NodePort"` and `"nodePort":30500`.

- [ ] **Step 2: Run** — compile failures.

- [ ] **Step 3: Implement** `internal/up/host.go`:

```go
package up

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

type Runner interface {
	Run(name string, args ...string) ([]byte, error)
}

// ExecRunner shells out for real; `luncur up` uses it, tests fake it.
type ExecRunner struct{}

func (ExecRunner) Run(name string, args ...string) ([]byte, error) {
	return exec.Command(name, args...).CombinedOutput()
}

const (
	K3sVersion       = "v1.32.5+k3s1"
	K3sKubeconfig    = "/etc/rancher/k3s/k3s.yaml"
	RegistriesPath   = "/etc/rancher/k3s/registries.yaml"
	RegistryNodePort = 30500
)

// EnsureK3s installs K3s (official script, pinned version) when missing.
func EnsureK3s(r Runner) (bool, error) {
	if _, err := r.Run("which", "k3s"); err == nil {
		return false, nil
	}
	script := fmt.Sprintf(
		"curl -sfL https://get.k3s.io | INSTALL_K3S_VERSION=%s sh -", K3sVersion)
	if out, err := r.Run("sh", "-c", script); err != nil {
		return false, fmt.Errorf("k3s install failed: %v\n%s", err, out)
	}
	return true, nil
}

// RegistriesYAML maps the in-cluster registry hostname to the localhost
// NodePort — containerd on the node cannot resolve cluster-DNS names.
func RegistriesYAML() string {
	return fmt.Sprintf(`mirrors:
  "registry.luncur-system:5000":
    endpoint:
      - "http://127.0.0.1:%d"
`, RegistryNodePort)
}

func WriteRegistriesYAML(path string) (bool, error) {
	want := []byte(RegistriesYAML())
	if cur, err := os.ReadFile(path); err == nil && bytes.Equal(cur, want) {
		return false, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, err
	}
	if err := os.WriteFile(path, want, 0o644); err != nil {
		return false, err
	}
	return true, nil
}
```

`internal/build/infra.go` — registry Service spec gains:

```go
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeNodePort,
			Selector: registryLabels,
			Ports: []corev1.ServicePort{{
				Port:       5000,
				TargetPort: intstr.FromInt32(5000),
				NodePort:   30500,
			}},
		},
```

with the comment: `// NodePort 30500: containerd pulls via http://127.0.0.1:30500 (see up.RegistriesYAML); in-cluster clients keep using registry.luncur-system:5000.`

- [ ] **Step 4: Run** `go test ./internal/up/ ./internal/build/ -v` — pass.
- [ ] **Step 5: Commit** — `feat: k3s install + registries.yaml host steps; registry NodePort`

---

### Task 12: `luncur up` command + README

**Files:**
- Create: `internal/cli/up.go`
- Modify: `internal/cli/root.go` (register)
- Modify: `README.md`
- Test: `internal/cli/up_test.go`

**Interfaces:**
- Consumes: `up.EnsureK3s`, `up.WriteRegistriesYAML`, `up.LuncurObjects`, `up.PanelHost`, `up.BootstrapSecretName`, `kube.New`, `kube.Apply`, `kube.WaitDeployment`, `kube.NodeIP`, `kube.GetSecretData`, `build.EnsureSystem`, `client.New(...).Login`, `saveConfig`.
- Produces: `luncur up [--ip <ip>] [--image <ref>] [--builder-image <ref>] [--kubeconfig <path>]`. Linux-only (errors out otherwise unless `--kubeconfig` is set, which skips the K3s/registries host steps for the "point at an existing cluster" case). Idempotent: every step is skip-or-repair.

Step sequence (each step prints a `==> ...` progress line):

1. Host steps (skipped when `--kubeconfig` set): `runtime.GOOS == "linux"` guard; `up.EnsureK3s(runner)`; `up.WriteRegistriesYAML(up.RegistriesPath)`; if the file changed AND k3s was already installed → `runner.Run("systemctl", "restart", "k3s")`.
2. Kube client: `kube.New(kubeconfigPath)` (default `up.K3sKubeconfig`), retry every 2s up to 60s (k3s may still be starting).
3. IP: `--ip` flag if set, else `kubeClient.NodeIP(ctx)`.
4. System infra: `build.EnsureSystem(ctx, kubeClient, "luncur-system", "luncur-data", "luncur-registry", "registry:2")`.
5. Bootstrap secret: `kube.GetSecretData(ctx, "luncur-system", up.BootstrapSecretName)`; when nil, generate `admin@luncur.local` + 16-byte hex password (`crypto/rand`), apply a Secret manifest (`data.admin = email:password`) via `kube.Apply`; remember whether it was newly created.
6. Luncur objects: `up.LuncurObjects(up.Params{Image: image, ExternalIP: ip, BuilderImage: builderImage})` → `kube.Apply(ctx, "luncur-system", objs)`.
7. Wait: `kube.WaitDeployment(ctx, "luncur-system", "luncur", 2*time.Second)` with a 5-minute timeout.
8. Token: parse `email:password` out of the bootstrap secret; `client.New("http://"+up.PanelHost(ip), "").Login(email, password)` retrying every 2s up to 60s (ingress propagation); `saveConfig(Config{Server: url, Token: tok})`. A login failure here is a warning, not a fatal error (the admin may have rotated the password) — print `run 'luncur login <url>' manually`.
9. Print summary: panel URL; on fresh bootstrap ALSO print the admin email + password with a "shown once — store it now" warning.

`--image` default: `"ghcr.io/sutantodadang/luncur:" + version` (from `root.go`'s `version` var; when `version == "dev"` use tag `latest`).

- [ ] **Step 1: Failing test** — the orchestration is thin glue over tested parts, so test what's testable without a cluster:

```go
func TestUpDefaults(t *testing.T) {
	cmd := upCmd()
	if cmd.Use != "up" {
		t.Fatal("use")
	}
	img, _ := cmd.Flags().GetString("image")
	if img != "ghcr.io/sutantodadang/luncur:latest" { // version == "dev" in tests
		t.Fatalf("image default = %q", img)
	}
}

func TestUpRefusesNonLinuxWithoutKubeconfig(t *testing.T) {
	if runtime.GOOS == "linux" {
		t.Skip("linux host")
	}
	cmd := upCmd()
	cmd.SetArgs([]string{})
	if err := cmd.Execute(); err == nil ||
		!strings.Contains(err.Error(), "linux") {
		t.Fatalf("want linux-only error, got %v", err)
	}
}
```

- [ ] **Step 2: Run** — compile failure.

- [ ] **Step 3: Implement** `internal/cli/up.go` per the step sequence above. Skeleton:

```go
package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/sutantodadang/luncur/internal/build"
	"github.com/sutantodadang/luncur/internal/client"
	"github.com/sutantodadang/luncur/internal/kube"
	"github.com/sutantodadang/luncur/internal/render"
	"github.com/sutantodadang/luncur/internal/up"
)

func defaultImage() string {
	tag := version
	if tag == "dev" {
		tag = "latest"
	}
	return "ghcr.io/sutantodadang/luncur:" + tag
}

func upCmd() *cobra.Command {
	var ip, image, builderImage, kubeconfig string
	cmd := &cobra.Command{
		Use:   "up",
		Short: "Install (or repair) luncur on this machine's K3s",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			runner := up.ExecRunner{}
			kubeconfigPath := kubeconfig

			if kubeconfig == "" {
				if runtime.GOOS != "linux" {
					return fmt.Errorf("luncur up installs K3s and must run on linux (use --kubeconfig to target an existing cluster)")
				}
				cmd.Println("==> ensuring K3s")
				installed, err := up.EnsureK3s(runner)
				if err != nil {
					return err
				}
				cmd.Println("==> writing registries.yaml")
				changed, err := up.WriteRegistriesYAML(up.RegistriesPath)
				if err != nil {
					return err
				}
				if changed && !installed {
					cmd.Println("==> restarting k3s (registry config changed)")
					if out, err := runner.Run("systemctl", "restart", "k3s"); err != nil {
						return fmt.Errorf("restart k3s: %v\n%s", err, out)
					}
				}
				kubeconfigPath = up.K3sKubeconfig
			}

			cmd.Println("==> connecting to kubernetes")
			kc, err := waitKube(ctx, kubeconfigPath)
			if err != nil {
				return err
			}

			if ip == "" {
				if ip, err = kc.NodeIP(ctx); err != nil {
					return fmt.Errorf("detect IP (use --ip): %w", err)
				}
			}
			cmd.Printf("==> external IP %s\n", ip)

			cmd.Println("==> applying system infrastructure")
			if err := build.EnsureSystem(ctx, kc, "luncur-system", "luncur-data", "luncur-registry", "registry:2"); err != nil {
				return err
			}

			email, password, fresh, err := ensureBootstrapSecret(ctx, kc)
			if err != nil {
				return err
			}

			cmd.Println("==> deploying luncur")
			objs, err := up.LuncurObjects(up.Params{Image: image, ExternalIP: ip, BuilderImage: builderImage})
			if err != nil {
				return err
			}
			if err := kc.Apply(ctx, "luncur-system", objs); err != nil {
				return err
			}

			cmd.Println("==> waiting for rollout")
			waitCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
			defer cancel()
			if err := kc.WaitDeployment(waitCtx, "luncur-system", "luncur", 2*time.Second); err != nil {
				return fmt.Errorf("luncur deployment not ready: %w", err)
			}

			serverURL := "http://" + up.PanelHost(ip)
			cmd.Println("==> logging in")
			if err := mintToken(ctx, serverURL, email, password); err != nil {
				cmd.Printf("warning: automatic login failed (%v)\nrun: luncur login %s\n", err, serverURL)
			}

			cmd.Printf("\nluncur is up: %s\n", serverURL)
			if fresh {
				cmd.Printf("\nadmin login (shown once — store it now):\n  email:    %s\n  password: %s\n", email, password)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&ip, "ip", "", "public IP (default: detect from the node)")
	cmd.Flags().StringVar(&image, "image", defaultImage(), "luncur server image")
	cmd.Flags().StringVar(&builderImage, "builder-image", "luncur/builder:latest", "builder image")
	cmd.Flags().StringVar(&kubeconfig, "kubeconfig", "", "target an existing cluster (skips K3s install)")
	return cmd
}

// waitKube retries kube.New — right after a K3s install the apiserver may
// still be coming up.
func waitKube(ctx context.Context, kubeconfig string) (*kube.Client, error) {
	deadline := time.Now().Add(60 * time.Second)
	for {
		kc, err := kube.New(kubeconfig)
		if err == nil {
			if _, ipErr := kc.NodeIP(ctx); ipErr == nil {
				return kc, nil
			} else {
				err = ipErr
			}
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("kubernetes not reachable: %w", err)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// ensureBootstrapSecret returns the admin credentials, creating them (and
// the Secret) on first run. fresh reports whether they were just minted.
func ensureBootstrapSecret(ctx context.Context, kc *kube.Client) (email, password string, fresh bool, err error) {
	data, err := kc.GetSecretData(ctx, "luncur-system", up.BootstrapSecretName)
	if err != nil {
		return "", "", false, err
	}
	if v, ok := data["admin"]; ok {
		e, p, found := strings.Cut(string(v), ":")
		if !found {
			return "", "", false, fmt.Errorf("bootstrap secret is malformed")
		}
		return e, p, false, nil
	}
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", "", false, err
	}
	email, password = "admin@luncur.local", hex.EncodeToString(raw)
	secJSON, err := json.Marshal(map[string]any{
		"apiVersion": "v1", "kind": "Secret",
		"metadata": map[string]any{
			"name": up.BootstrapSecretName, "namespace": "luncur-system",
			"labels": map[string]string{"app.kubernetes.io/managed-by": "luncur"},
		},
		"type":       "Opaque",
		"stringData": map[string]string{"admin": email + ":" + password},
	})
	if err != nil {
		return "", "", false, err
	}
	if err := kc.Apply(ctx, "luncur-system", []render.Object{{Kind: "Secret", JSON: secJSON}}); err != nil {
		return "", "", false, err
	}
	return email, password, true, nil
}

// mintToken logs in (retrying while ingress propagates) and saves the CLI config.
func mintToken(ctx context.Context, serverURL, email, password string) error {
	deadline := time.Now().Add(60 * time.Second)
	for {
		tok, err := client.New(serverURL, "").Login(email, password)
		if err == nil {
			return saveConfig(Config{Server: serverURL, Token: tok})
		}
		if time.Now().After(deadline) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}
```

Register in `root.go`: `root.AddCommand(upCmd())`.

- [ ] **Step 4: Run** `go test ./internal/cli/ -v` and `go build ./...` — pass.

- [ ] **Step 5: Update README.md** — new sections:
  - "Install" at the top: `luncur up` on a fresh Linux VPS (what it installs, that credentials print once, `--ip`/`--image`/`--kubeconfig` flags, idempotent re-runs).
  - "Web UI": `http://panel.<ip>.sslip.io/ui/` — login, projects, apps, scale/env/deploy, live logs.
  - Update the Plan D references: registries.yaml is now written by `luncur up`; `luncur logs` follows builds (`--deploy N -f`) and streams runtime logs (no `--deploy`); `luncur status`.
  - Status line: "Phase 1 complete (Plans A-D)."
  - Deviations noted in Global Constraints (html/template UI, NodePort registry, IP detection, token expiry).

- [ ] **Step 6: Commit** — `feat: luncur up — one-command install on a fresh VPS` (include README).

---

### Task 13: Plan C deferred fixes (backlog burn-down)

**Files:**
- Modify: `internal/server/apps.go` (upload cap), `internal/build/job.go` (Job hygiene), `internal/cli/serve.go` (errors.Is), `internal/cli/archive.go` (stderr), `internal/store/deployments.go` (scan-err shape)
- Test: extend the touching packages' existing test files where behavior changes

**Interfaces:** none new — these are the Plan C final-review deferrals recorded in the progress ledger.

- [ ] **Step 1: M1 — upload size cap.** In `handleDeployApp`, before `ParseMultipartForm`, wrap the body: `r.Body = http.MaxBytesReader(w, r.Body, 256<<20)` (256 MiB; a source tarball larger than that is a mistake, not an app). On `ParseMultipartForm` error the existing `bad_request` path already fires. Add a test posting a multipart body with an over-limit Content-Length? Too slow — instead assert the wrap exists behaviorally with a small limit is not configurable; keep the test to: existing multipart deploy tests still pass.
- [ ] **Step 2: M2 — build Job hygiene.** In `RenderBuildJob`: set `TTLSecondsAfterFinished: ptr(int32(3600))`, `ActiveDeadlineSeconds: ptr(int64(900))`, and container resources (requests cpu `100m`/memory `256Mi`, limits memory `2Gi`). Extend the existing job render test to assert the three fields.
- [ ] **Step 3: M5 — `errors.Is(err, http.ErrServerClosed)`** in `internal/cli/serve.go`.
- [ ] **Step 4: M6 — `archive.go` git-archive uses `CombinedOutput`** (or captures stderr into the returned error) so failures carry git's message. Adjust its test if one asserts the error text.
- [ ] **Step 5: M7 — store scan-err shape.** In `GetDeployment`/`LatestDeployment`, only assign `d.ImageRef, d.LogPath = img.String, logp.String` when `err == nil`.
- [ ] **Step 6: Run** `go test ./...` — green. **Commit** — `fix: plan C review backlog (upload cap, job hygiene, error handling)`

---

## Final verification (after all tasks)

- [ ] `go build ./... && go vet ./... && go test ./...` — everything green.
- [ ] Grep for leftover references: `grep -rn "Plan D" README.md internal/` — no stale "deferred to Plan D" comments survive (fix `internal/cli/logs.go`'s old comment, README's registries.yaml note).
- [ ] Push branch `plan-d`, open PR against `main`.

## Spec-coverage self-check (from 2026-07-02-luncur-phase1-design.md)

- `luncur up`: install K3s pinned ✅ (T11/T12), namespace/PVC/Deployment/Service/Ingress panel host ✅ (T10/T12), bootstrap admin + print once + CLI token ✅ (T12), idempotent ✅ (SSA + skip-or-repair steps), registries.yaml ✅ (T11), IP detection ✅ (deviation documented).
- Build + rollout logs stream to CLI and UI via SSE ✅ (T4/T5/T6/T9).
- Web UI: login → project list → app list → app detail (status, URL, deploy history, live logs, env editor, scale, deploy-from-git button, raw YAML read-only) ✅ (T7/T8/T9).
- `luncur status [app]` ✅ (T6). `luncur logs <app> [-f]` runtime ✅ (T5/T6).
- Token lifecycle (deferred from Plan B/C) ✅ expiry (T1); list/revoke deliberately Phase 2.
- Escape hatch, deploy pipeline, env, scale, destroy, user add — already shipped in Plans A-C; untouched.
