# luncur Plan J — metrics + cert expiry readback Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `luncur status <app>` and the app page show live CPU/memory (via metrics-server) plus deploy counts; cert-manager-managed domains get their expiry read back from the TLS Secret.

**Architecture:** PodMetrics are read through the existing dynamic client (`metrics.k8s.io/v1beta1`, resource `pods`) — no typed metrics client. A kube helper sums container usage across an app's pods; a new `/metrics` endpoint feeds both CLI and UI. The daily cert sweep additionally parses the TLS Secret's leaf cert for `external`-status domains under the cert-manager provider.

**Tech Stack:** Go stdlib, client-go dynamic + `k8s.io/apimachinery/pkg/api/resource` (existing), modernc.org/sqlite.

## Global Constraints

- Single Go module, one binary from `cmd/luncur`. **No new dependencies** — metrics via the dynamic client.
- Server-side apply everywhere, `fieldManager=luncur`. API error envelope via `writeError`. Conventional commits; `go build ./... && go vet ./... && go test ./...` before every commit.
- Tests must not require a cluster: fake dynamic client PodMetrics objects; metrics API errors → `"available": false`, never a 5xx.

---

### Task 1: kube — app metrics via PodMetrics

**Files:**
- Modify: `internal/kube/kube.go`
- Modify: `internal/up/manifests.go` (ClusterRole rule)
- Test: `internal/kube/kube_test.go` (append), `internal/up/manifests_test.go` (extend)

**Interfaces:**
- Consumes: existing `gvrByKind`, dynamic client, `resource.Quantity` parsing.
- Produces:
  - `gvrByKind` gains `"PodMetrics": {Group: "metrics.k8s.io", Version: "v1beta1", Resource: "pods"}`.
  - `type AppMetrics struct { CPUMilli, MemoryMiB int64; Pods int }`
  - `Client.AppMetrics(ctx context.Context, namespace, app string) (AppMetrics, bool)` — lists PodMetrics with label selector `app.kubernetes.io/name=<app>`, sums every container's `usage.cpu` (→ millicores) and `usage.memory` (→ MiB). Any list error (metrics-server absent) folds into `(zero, false)`; success returns `(sum, true)`.
  - `Client.DeploymentStatus(ctx context.Context, namespace, name string) (ready, desired int64, err error)` — dynamic get on the Deployment; NotFound → (0, 0, nil).
  - ClusterRole: new rule `rule([]string{"metrics.k8s.io"}, []string{"pods"}, read...)`.

- [ ] **Step 1: Failing tests.**

Append to `internal/kube/kube_test.go` (adapt `newFakeDyn` usage to the file's helper; the fake dynamic client needs the PodMetrics list kind registered — pass a `map[schema.GroupVersionResource]string{{Group:"metrics.k8s.io",Version:"v1beta1",Resource:"pods"}: "PodMetricsList"}` if the helper supports it, or construct `dynamicfake.NewSimpleDynamicClientWithCustomListKinds` directly in this test):

```go
func podMetricsObj(name, app, cpu, mem string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "metrics.k8s.io/v1beta1", "kind": "PodMetrics",
		"metadata": map[string]any{
			"name": name, "namespace": "proj",
			"labels": map[string]any{"app.kubernetes.io/name": app},
		},
		"containers": []any{
			map[string]any{"name": "app", "usage": map[string]any{"cpu": cpu, "memory": mem}},
		},
	}}
}

func TestAppMetrics(t *testing.T) {
	scheme := runtime.NewScheme()
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme,
		map[schema.GroupVersionResource]string{
			{Group: "metrics.k8s.io", Version: "v1beta1", Resource: "pods"}: "PodMetricsList",
		},
		podMetricsObj("web-1", "web", "250m", "128Mi"),
		podMetricsObj("web-2", "web", "150m", "64Mi"),
		podMetricsObj("other-1", "other", "999m", "999Mi"),
	)
	c := NewForTest(dyn, nil)
	m, ok := c.AppMetrics(context.Background(), "proj", "web")
	if !ok {
		t.Fatal("metrics unavailable")
	}
	if m.CPUMilli != 400 || m.MemoryMiB != 192 || m.Pods != 2 {
		t.Fatalf("metrics = %+v, want 400m/192MiB/2 pods", m)
	}
}

func TestAppMetricsUnavailable(t *testing.T) {
	// A fake WITHOUT the custom list kind makes List return an error —
	// exactly the metrics-server-absent shape.
	dyn := newFakeDynEmpty(t) // or the file's plain fake constructor with no PodMetrics kind
	c := NewForTest(dyn, nil)
	if _, ok := c.AppMetrics(context.Background(), "proj", "web"); ok {
		t.Fatal("want unavailable when metrics API missing")
	}
}
```

(If the plain fake actually tolerates unknown list kinds, force the error instead with a reactor that fails `list` on the pods metrics resource — the assertion stands: `ok == false`.) Also extend `TestLuncurObjects` required substrings with `"metrics.k8s.io"`.

- [ ] **Step 2: Run** — compile failure.

- [ ] **Step 3: Implement** in `kube.go`:

```go
// AppMetrics sums CPU/memory usage across an app's pods via the
// metrics.k8s.io API. ok=false when metrics-server isn't available —
// callers render "metrics unavailable", never an error.
type AppMetrics struct {
	CPUMilli  int64
	MemoryMiB int64
	Pods      int
}

func (c *Client) AppMetrics(ctx context.Context, namespace, app string) (AppMetrics, bool) {
	list, err := c.dyn.Resource(gvrByKind["PodMetrics"]).Namespace(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "app.kubernetes.io/name=" + app,
	})
	if err != nil {
		return AppMetrics{}, false
	}
	var out AppMetrics
	for _, item := range list.Items {
		out.Pods++
		containers, _, _ := unstructured.NestedSlice(item.Object, "containers")
		for _, ci := range containers {
			cm, ok := ci.(map[string]any)
			if !ok {
				continue
			}
			usage, _, _ := unstructured.NestedStringMap(cm, "usage")
			if q, err := resource.ParseQuantity(usage["cpu"]); err == nil {
				out.CPUMilli += q.MilliValue()
			}
			if q, err := resource.ParseQuantity(usage["memory"]); err == nil {
				out.MemoryMiB += q.Value() / (1 << 20)
			}
		}
	}
	return out, true
}

// DeploymentStatus reports ready/desired replicas; absent → zeros.
func (c *Client) DeploymentStatus(ctx context.Context, namespace, name string) (ready, desired int64, err error) {
	u, err := c.dyn.Resource(gvrByKind["Deployment"]).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return 0, 0, nil
	}
	if err != nil {
		return 0, 0, err
	}
	ready, _, _ = unstructured.NestedInt64(u.Object, "status", "readyReplicas")
	desired, _, _ = unstructured.NestedInt64(u.Object, "spec", "replicas")
	return ready, desired, nil
}
```

(import `"k8s.io/apimachinery/pkg/api/resource"`.) ClusterRole rule in `manifests.go`: `rule([]string{"metrics.k8s.io"}, []string{"pods"}, read...)`.

- [ ] **Step 4: Run** `go test ./internal/kube/ ./internal/up/ -v` — pass.
- [ ] **Step 5: Commit** — `feat: kube app metrics via metrics.k8s.io + deployment status`

---

### Task 2: server + store — metrics endpoint

**Files:**
- Modify: `internal/store/deployments.go` (CountDeployments)
- Create: `internal/server/metrics.go`
- Modify: `internal/server/server.go` (route)
- Test: `internal/store/deployments_test.go` (append), `internal/server/metrics_test.go`

**Interfaces:**
- Consumes: Task 1 (`kube.AppMetrics`, `DeploymentStatus`), `requireProject`/`requireApp`.
- Produces:
  - `Store.CountDeployments(appID int64) (int64, error)`.
  - `GET /v1/projects/{project}/apps/{app}/metrics` (authed) → 200:

```json
{"available":true,"cpu_millicores":400,"memory_mib":192,"pods":2,
 "ready_replicas":1,"desired_replicas":1,"deploy_count":7}
```

  kube nil OR metrics unavailable → `{"available":false,"deploy_count":N,...zeros}` (deploy_count always real). Never 5xx for metrics availability.

- [ ] **Step 1: Failing tests.** Store: `TestCountDeployments` (two rows → 2; empty app → 0) appended to `deployments_test.go`. Server: `internal/server/metrics_test.go` — fixture with fake dynamic kube carrying two PodMetrics objects (reuse Task 1's `podMetricsObj` shape inline; the server fixture's fake needs the custom list kind — mirror how `addons_test.go`/`apps_test.go` build their fakes and add the list-kind map) + a Deployment with status; assert the full JSON shape; second test with kube nil → available false + deploy_count still populated.

- [ ] **Step 2: Run** — failures.

- [ ] **Step 3: Implement.** `CountDeployments`:

```go
// CountDeployments returns an app's total deploy count (history table cap
// notwithstanding — COUNT is exact).
func (s *Store) CountDeployments(appID int64) (int64, error) {
	var n int64
	err := s.db.QueryRow(`SELECT count(*) FROM deployments WHERE app_id = ?`, appID).Scan(&n)
	return n, err
}
```

`internal/server/metrics.go`:

```go
package server

import (
	"log"
	"net/http"

	"github.com/sutantodadang/luncur/internal/store"
)

func (s *server) handleAppMetrics(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.requireProject(w, u, r.PathValue("project"))
	if !ok {
		return
	}
	a, ok := s.requireApp(w, p, r.PathValue("app"))
	if !ok {
		return
	}
	deploys, err := s.st.CountDeployments(a.ID)
	if err != nil {
		log.Printf("count deployments: %v", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	out := map[string]any{
		"available": false, "cpu_millicores": int64(0), "memory_mib": int64(0),
		"pods": 0, "ready_replicas": int64(0), "desired_replicas": int64(0),
		"deploy_count": deploys,
	}
	if s.kube != nil {
		if m, ok := s.kube.AppMetrics(r.Context(), p.Namespace, a.Name); ok {
			out["available"] = true
			out["cpu_millicores"] = m.CPUMilli
			out["memory_mib"] = m.MemoryMiB
			out["pods"] = m.Pods
		}
		if ready, desired, err := s.kube.DeploymentStatus(r.Context(), p.Namespace, a.Name); err == nil {
			out["ready_replicas"] = ready
			out["desired_replicas"] = desired
		}
	}
	writeJSON(w, http.StatusOK, out)
}
```

Route: `mux.HandleFunc("GET /v1/projects/{project}/apps/{app}/metrics", s.authed(s.handleAppMetrics))`.

- [ ] **Step 4: Run** `go test ./internal/store/ ./internal/server/ -v` — pass.
- [ ] **Step 5: Commit** — `feat: per-app metrics endpoint`

---

### Task 3: CLI + UI surfaces

**Files:**
- Modify: `internal/client/client.go`
- Modify: `internal/cli/status.go`
- Modify: `internal/server/ui.go` + `internal/server/templates/app.html`
- Test: `internal/cli/commands_test.go` (append/extend status test), `internal/server/ui_test.go` (append)

**Interfaces:**
- Consumes: Task 2 endpoint.
- Produces:
  - `Client.AppMetrics(project, app string) (MetricsInfo, error)` with `type MetricsInfo struct { Available bool \`json:"available"\`; CPUMillicores int64 \`json:"cpu_millicores"\`; MemoryMiB int64 \`json:"memory_mib"\`; Pods int \`json:"pods"\`; ReadyReplicas int64 \`json:"ready_replicas"\`; DesiredReplicas int64 \`json:"desired_replicas"\`; DeployCount int64 \`json:"deploy_count"\` }`.
  - `luncur status <app>` gains, after the existing lines: `cpu: 400m` / `memory: 192Mi` / `deploys: 7` (or `metrics: unavailable` when `Available` false — deploys line always shown).
  - App page: a stats line under the title — `{{if .Metrics.Available}}cpu {{.Metrics.CPUMillicores}}m · mem {{.Metrics.MemoryMiB}}Mi · {{end}}{{.Metrics.ReadyReplicas}}/{{.Metrics.DesiredReplicas}} ready · {{.Metrics.DeployCount}} deploys` — view-model `"Metrics"` built by calling the same code path (`s.appMetricsData(ctx, p, a)` — extract the map-building core from `handleAppMetrics` into a helper returning a small struct both use).
- [ ] **Step 1: Failing tests** — CLI: extend the existing status test (`TestStatusAppAndList` or append a new one) asserting `deploys:` appears in `status <app>` output (testEnv has no kube → also `metrics: unavailable`). UI: append assertion to an existing app-page test (or new small test) that the page contains `deploys`.
- [ ] **Step 2: Run** — failures.
- [ ] **Step 3: Implement** per Interfaces (extract `appMetricsData` struct+helper in `metrics.go`; `handleAppMetrics` and `handleUIApp` both consume it; client method; status.go lines).
- [ ] **Step 4: Run** `go test ./internal/client/ ./internal/cli/ ./internal/server/ -v` — pass.
- [ ] **Step 5: Commit** — `feat: metrics in status CLI and app page`

---

### Task 4: cert-manager expiry readback + README

**Files:**
- Modify: `internal/server/certs.go`
- Modify: `README.md`
- Test: `internal/server/certs_test.go` (append)

**Interfaces:**
- Consumes: existing `certManager.sweep`, `s.kube.GetSecretData`, `certSecretName`, `projectAppForDomain`, `s.certProviderName()`.
- Produces: during `sweep`, when the provider is `cert-manager`, every domain with `cert_status == "external"` gets its TLS Secret read (`certSecretName(app, hostname)` in the app namespace); if `tls.crt` parses, `cert_expires_at` is updated (status stays `external`). Absent Secret / parse failure → skip silently (cert-manager may not have issued yet).

- [ ] **Step 1: Failing test** (append to `certs_test.go`): fixture with provider setting `cert-manager`, a domain with status `external`, and a fake typed clientset holding Secret `certSecretName("web", host)` in the app namespace whose `tls.crt` is a self-signed cert PEM (generate in-test with crypto/x509, NotAfter 90 days out — reuse the cert-generation shape from `internal/acme/acmetest`). Run `cm.sweep(ctx)` directly; assert the domain row's `cert_expires_at` is now non-empty and parses to the cert's NotAfter (RFC3339, UTC).

- [ ] **Step 2: Run** — fails (expiry stays empty).

- [ ] **Step 3: Implement** — in `sweep`, alongside the existing renew logic:

```go
	provider := m.s.certProviderName()
	// ...inside the domains loop, before the renew switch:
	if provider == "cert-manager" && d.CertStatus == "external" {
		m.readbackExpiry(ctx, d)
		continue
	}
```

```go
// readbackExpiry fills cert_expires_at for cert-manager-managed domains by
// parsing the leaf cert out of the TLS Secret cert-manager maintains.
func (m *certManager) readbackExpiry(ctx context.Context, d store.Domain) {
	if m.s.kube == nil {
		return
	}
	p, a, err := m.s.projectAppForDomain(d)
	if err != nil {
		return
	}
	data, err := m.s.kube.GetSecretData(ctx, p.Namespace, certSecretName(a.Name, d.Hostname))
	if err != nil || data == nil {
		return // not issued yet
	}
	blk, _ := pem.Decode(data["tls.crt"])
	if blk == nil {
		return
	}
	cert, err := x509.ParseCertificate(blk.Bytes)
	if err != nil {
		return
	}
	exp := cert.NotAfter.UTC().Format(time.RFC3339)
	if exp != d.CertExpiresAt {
		if err := m.s.st.SetDomainCert(d.ID, "external", "", exp); err != nil {
			log.Printf("cert expiry readback %s: %v", d.Hostname, err)
		}
	}
}
```

(imports: `crypto/x509`, `encoding/pem`.)

- [ ] **Step 4: README** — Web UI/status docs gain a "Metrics" note (needs metrics-server; K3s bundles it; "metrics unavailable" otherwise); domains section notes cert-manager expiry now shown; status line → "Phase 3 in progress — addons + metrics shipped (Plans I-J)".
- [ ] **Step 5: Run** `go build ./... && go vet ./... && go test ./...` — green; `gofmt -l internal/` clean; `grep -rn "Plan J" README.md internal/` — only intentional.
- [ ] **Step 6: Commit** — `feat: cert-manager expiry readback + metrics docs`

---

## Final verification (after all tasks)

- [ ] `go build ./... && go vet ./... && go test ./...` — everything green.
- [ ] Push branch `plan-j`, open PR against `main`.
- [ ] Manual (owner's VPS, post-merge): `luncur status web` shows cpu/memory; app page stats line; cert-manager domain shows expiry after a sweep.

## Spec-coverage self-check (Plan J section of 2026-07-03-luncur-phase3-design.md)

- PodMetrics via dynamic client, no typed metrics client ✅ (T1); ClusterRole read rule ✅ (T1)
- `/metrics` endpoint: cpu millicores, memory MiB, ready/desired, deploy count; `available:false` never an error ✅ (T2)
- `luncur status` cpu/memory lines; app page stats row (page-load render, no polling) ✅ (T3)
- cert-manager expiry readback in the daily sweep, status stays `external` ✅ (T4)
