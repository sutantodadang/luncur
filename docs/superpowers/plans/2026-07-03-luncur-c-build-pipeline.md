# luncur Plan C — Build Pipeline, Embedded Registry, Source Deploy Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `luncur deploy` in an app directory tars the source, uploads it, the server builds a container image in-cluster (Nixpacks or Dockerfile) via a BuildKit Job, pushes it to an in-cluster registry, then deploys it — reachable at `<app>.<ip>.sslip.io`, build logs viewable.

**Architecture:** A new `internal/build` package renders the BuildKit **Job** manifest, the registry/system infra manifests, and owns the on-disk source+log store (`--data-dir`, shared with the Build Job through a PVC in prod). `internal/kube` gains a Job GVR and `WaitJob`. The deploy handler becomes async: it creates a `building` deployment, saves the uploaded tarball, and spawns a goroutine (`runBuild`) that applies the Job, waits for it, then renders+applies the app manifests against the built image. The CLI packs the current directory (`git archive`, else tar) and polls the deploy to completion. Registry is a stock `registry:2` Deployment (not an embedded library — see Deviations).

**Tech Stack:** existing (Go stdlib, cobra, modernc sqlite, client-go dynamic, k8s.io typed manifests) + `mime/multipart` and `archive/tar` (both stdlib). **No new module dependencies.**

## Deviations from the spec (deliberate, ponytail)

The spec (`docs/superpowers/specs/2026-07-02-luncur-phase1-design.md`) named two things this plan implements more simply. Same external behavior; less code and fewer deps.

1. **Registry: stock `registry:2` Deployment, not an embedded `distribution/distribution` library.** The spec said "embedded OCI registry (distribution lib), blobs on the PVC." Running the registry as a one-container Deployment + Service + PVC in `luncur-system` needs **zero new Go dependencies and no in-process listener**, and BuildKit pushes / kubelet pulls it identically. Embedding the library can come later if single-binary purity matters; it does not change the deploy story or the escape hatch (the actual differentiator). Rendered by `internal/build.SystemObjects`.
2. **Build logs: builder tees to a file on the shared data volume, server serves the file. No pod-log API, no SSE stream yet.** The spec listed live SSE build logs. Phase-1-minimum here is `GET .../deploys/{id}/logs` returning the current log text (the failing tail is what users need). Live `-f`/SSE follow and runtime `luncur logs` move to Plan D.
3. **Source delivery to the Build Job is a shared PVC (single-node), not an internal HTTP fetch.** K3s Phase 1 is single-node (multi-node is out of scope per the spec), so the `luncur serve` pod and each Build Job mount the same `luncur-data` PVC (`ReadWriteOnce`, same node). Marked with a `ponytail:` comment and the upgrade path (RWX / object store) if multi-node ever lands.

## Plan sequence (Phase 1)

A: core (merged, PR #1) · B1+B2: model + K8s applier + escape hatch (merged, PR #2) · **C: this plan** · D: web UI + `luncur up` + SSE logs + custom domains.

## Global Constraints

- Everything from Plans A/B still binds: JSON error envelope `{"error":{"code":"<snake_case>","message":"..."}}`; all routes under `/v1/`; bearer-token auth; roles `admin`|`member`; project membership authz via `requireProject`; **no CGO** (pure-Go build); SSA everywhere (`FieldManager: "luncur"`, `Force: true`).
- **No new go.mod dependencies.** Everything is stdlib or already vendored (client-go, k8s.io typed manifests, cobra).
- Error bucketing convention (unchanged, enforced by reviewers): validation → 400 via `store.ValidationError` + `validationErrorf` + `errors.As`; duplicate → 409; not found → 404; anything else → `log.Printf` the real error + generic 500 `"internal error"`. **Never** put a raw store/driver/kube error string in a client response.
- Kube-less mode still holds: `server.New` accepts a nil `Kube`; any endpoint needing the cluster returns 503 `kubernetes_unavailable` via `requireKube`. All build/registry tests use the `k8s.io/client-go/dynamic/fake` client with **reactors** (do not rely on the fake tracker emulating SSA).
- Names: image ref `<registryHost>/<project>-<app>:<deployID>` (e.g. `registry.luncur-system:5000/web-api:42`); Build Job `build-<deployID>` in namespace `luncur-system`; system namespace constant `luncur-system`; data layout `<dataDir>/sources/<deployID>.tar.gz` and `<dataDir>/logs/<deployID>.log`.
- Deploy statuses (DB `CHECK` already allows all four): `building` → `deploying` → `live` | `failed`. `building` = Build Job running; `deploying` = image built, applying app manifests; `live` = applied; `failed` = build or apply failed (log has the tail).
- Deploy remains fire-and-forget re: rollout: `live` means the manifests were server-side-applied, not that pods are Ready (rollout watching stays out of scope).

## File Structure

```
internal/build/source.go        Source: on-disk tarball + log store rooted at data-dir
internal/build/source_test.go
internal/build/job.go           RenderBuildJob, ImageRef  (BuildKit Job manifest)
internal/build/job_test.go
internal/build/infra.go         SystemObjects (luncur-system ns, registry Deployment/Service/PVC)
internal/build/infra_test.go
internal/kube/kube.go           +Job GVR, +WaitJob                       (modified)
internal/kube/kube_test.go      +WaitJob test                            (modified)
internal/store/deployments.go   +createdBy, +LogPath, SetDeploymentImage/Log, GetDeployment (modified)
internal/store/apps.go          +SourceType/GitURL/GitBranch on App, CreateGitApp, scans (modified)
internal/store/deployments_test.go / apps_test.go                        (modified)
internal/server/server.go       Deps + build config fields, deploy/log routes (modified)
internal/server/build.go        startBuild, runBuild (the async state machine)
internal/server/build_test.go
internal/server/apps.go         handleDeployApp rewrite, handleGetDeploy, handleDeployLogs (modified)
internal/server/apps_test.go                                             (modified)
internal/client/client.go       DeploySource, GetDeploy, DeployLogs, CreateGitApp (modified)
internal/cli/archive.go         packSource (git archive | tar)
internal/cli/archive_test.go
internal/cli/deploy.go          deploy: source-or-image + poll             (modified)
internal/cli/init.go            luncur init → luncur.toml
internal/cli/app.go             app create --git-url/--branch             (modified)
internal/cli/logs.go            luncur logs <app> (latest build log)
internal/cli/serve.go           --data-dir/--builder-image/--registry-host, graceful shutdown, EnsureSystem (modified)
internal/cli/root.go            register init, logs                       (modified)
internal/store/overrides.go     Service externalIPs/loadBalancerIP denylist (modified, hygiene)
internal/store/store.go         DSN path escaping                          (modified, hygiene)
build/builder/Dockerfile        luncur/builder image (buildkit rootless + nixpacks + git)
build/builder/entrypoint.sh     fetch source → build → push → tee logs
README.md                                                                (modified)
```

---

### Task 1: Source + log store, deployment columns

**Files:**
- Create: `internal/build/source.go`, `internal/build/source_test.go`
- Modify: `internal/store/deployments.go`, `internal/store/deployments_test.go`

**Interfaces:**
- Consumes: nothing new.
- Produces:
  - `build.Source` with `func NewSource(dataDir string) (*Source, error)` (creates `<dataDir>/sources` and `<dataDir>/logs`, 0700); `func (s *Source) Save(deployID int64, r io.Reader) (string, error)` (writes `<dataDir>/sources/<id>.tar.gz`, returns abs path); `func (s *Source) TarballPath(id int64) string`; `func (s *Source) LogPath(id int64) string`; `func (s *Source) ReadLog(id int64) ([]byte, error)` (returns `nil, nil` if the log file does not exist yet).
  - `store.Deployment` gains `LogPath string` and `CreatedBy sql.NullInt64`.
  - `store.CreateDeployment(appID int64, status, imageRef string, createdBy int64) (Deployment, error)` — **signature change** (adds `createdBy`; pass `0` to mean "unattributed" → stored as SQL NULL).
  - `store.SetDeploymentImage(id int64, imageRef string) error`; `store.SetDeploymentLog(id int64, logPath string) error`; `store.GetDeployment(id int64) (Deployment, error)`.

- [ ] **Step 1: Write failing test for `build.Source`**

```go
package build

import (
	"strings"
	"testing"
)

func TestSourceSaveAndRead(t *testing.T) {
	s, err := NewSource(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	path, err := s.Save(42, strings.NewReader("tarbytes"))
	if err != nil {
		t.Fatal(err)
	}
	if got := s.TarballPath(42); got != path {
		t.Fatalf("TarballPath=%q want %q", got, path)
	}
	// No log written yet → ReadLog returns (nil, nil), not an error.
	log, err := s.ReadLog(42)
	if err != nil || log != nil {
		t.Fatalf("ReadLog on missing = (%q, %v), want (nil, nil)", log, err)
	}
}
```

- [ ] **Step 2: Run, expect FAIL** — `go test ./internal/build/ -run TestSourceSaveAndRead` → undefined: NewSource.

- [ ] **Step 3: Implement `internal/build/source.go`**

```go
// Package build renders luncur's in-cluster build pipeline: the BuildKit
// Job that turns app source into an image, the registry/system infra it
// pushes to, and the on-disk source+log store shared with the Job.
package build

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// Source is the on-disk store for uploaded build tarballs and build logs,
// rooted at the server's --data-dir. In production the same directory is a
// PVC mounted into both the luncur pod and each Build Job.
type Source struct{ dir string }

func NewSource(dataDir string) (*Source, error) {
	for _, sub := range []string{"sources", "logs"} {
		if err := os.MkdirAll(filepath.Join(dataDir, sub), 0o700); err != nil {
			return nil, fmt.Errorf("create %s dir: %w", sub, err)
		}
	}
	return &Source{dir: dataDir}, nil
}

func (s *Source) TarballPath(deployID int64) string {
	return filepath.Join(s.dir, "sources", fmt.Sprintf("%d.tar.gz", deployID))
}

func (s *Source) LogPath(deployID int64) string {
	return filepath.Join(s.dir, "logs", fmt.Sprintf("%d.log", deployID))
}

func (s *Source) Save(deployID int64, r io.Reader) (string, error) {
	path := s.TarballPath(deployID)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if _, err := io.Copy(f, r); err != nil {
		return "", err
	}
	return path, nil
}

func (s *Source) ReadLog(deployID int64) ([]byte, error) {
	b, err := os.ReadFile(s.LogPath(deployID))
	if os.IsNotExist(err) {
		return nil, nil
	}
	return b, err
}
```

- [ ] **Step 4: Run, expect PASS** — `go test ./internal/build/ -run TestSourceSaveAndRead`.

- [ ] **Step 5: Write failing test for deployment column changes** in `internal/store/deployments_test.go` (add; keep existing tests, updating any `CreateDeployment(` call to the new 4-arg form):

```go
func TestCreateDeploymentAttribution(t *testing.T) {
	st := newTestStore(t) // existing helper in the store test package
	u, err := st.CreateUser("dev@x.io", "password123", "admin")
	if err != nil {
		t.Fatal(err)
	}
	p, _ := st.CreateProject("web")
	a, _ := st.CreateApp(p.ID, "api", 8080)

	d, err := st.CreateDeployment(a.ID, "building", "", u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetDeploymentImage(d.ID, "reg/web-api:1"); err != nil {
		t.Fatal(err)
	}
	if err := st.SetDeploymentLog(d.ID, "/data/logs/1.log"); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetDeployment(d.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ImageRef != "reg/web-api:1" || got.LogPath != "/data/logs/1.log" {
		t.Fatalf("image/log not persisted: %+v", got)
	}
	if !got.CreatedBy.Valid || got.CreatedBy.Int64 != u.ID {
		t.Fatalf("created_by = %+v, want %d", got.CreatedBy, u.ID)
	}
}
```

> If `newTestStore`/`CreateProject` helper names differ in the existing test file, use whatever the sibling tests already use — match, don't invent.

- [ ] **Step 6: Run, expect FAIL** — `go test ./internal/store/ -run TestCreateDeploymentAttribution`.

- [ ] **Step 7: Update `internal/store/deployments.go`**

```go
type Deployment struct {
	ID        int64
	AppID     int64
	Status    string
	ImageRef  string
	LogPath   string
	CreatedBy sql.NullInt64
	CreatedAt string
}

// CreateDeployment inserts a deployment row. createdBy of 0 is stored as
// NULL (unattributed).
func (s *Store) CreateDeployment(appID int64, status, imageRef string, createdBy int64) (Deployment, error) {
	var by any
	if createdBy != 0 {
		by = createdBy
	}
	res, err := s.db.Exec(
		`INSERT INTO deployments (app_id, status, image_ref, created_by) VALUES (?, ?, ?, ?)`,
		appID, status, imageRef, by,
	)
	if err != nil {
		return Deployment{}, fmt.Errorf("insert deployment: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Deployment{}, err
	}
	return Deployment{ID: id, AppID: appID, Status: status, ImageRef: imageRef,
		CreatedBy: sql.NullInt64{Int64: createdBy, Valid: createdBy != 0}}, nil
}

func (s *Store) SetDeploymentImage(id int64, imageRef string) error {
	res, err := s.db.Exec(`UPDATE deployments SET image_ref = ? WHERE id = ?`, imageRef, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) SetDeploymentLog(id int64, logPath string) error {
	res, err := s.db.Exec(`UPDATE deployments SET log_path = ? WHERE id = ?`, logPath, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) GetDeployment(id int64) (Deployment, error) {
	var d Deployment
	var img, logp sql.NullString
	err := s.db.QueryRow(
		`SELECT id, app_id, status, image_ref, log_path, created_by, created_at
		 FROM deployments WHERE id = ?`, id,
	).Scan(&d.ID, &d.AppID, &d.Status, &img, &logp, &d.CreatedBy, &d.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Deployment{}, ErrNotFound
	}
	d.ImageRef, d.LogPath = img.String, logp.String
	return d, err
}
```

Also update `LatestDeployment` to scan the two new nullable columns (`image_ref`, `log_path`, `created_by`) the same way (use `sql.NullString` for `image_ref`/`log_path`), so it keeps working with rows that have NULLs.

- [ ] **Step 8: Fix the existing 3-arg `CreateDeployment` caller** in `internal/server/apps.go` (the B2 deploy handler) to pass `0` for now: `s.st.CreateDeployment(a.ID, "deploying", req.Image, 0)`. (Task 5 rewrites this handler; this keeps the tree compiling in between.)

- [ ] **Step 9: Run, expect PASS** — `go test ./internal/store/ ./internal/build/`.

- [ ] **Step 10: Commit**

```bash
git add internal/build/source.go internal/build/source_test.go internal/store/deployments.go internal/store/deployments_test.go internal/server/apps.go
git commit -m "feat: build source store + deployment image/log/created_by columns"
```

---

### Task 2: BuildKit Job renderer + image ref

**Files:**
- Create: `internal/build/job.go`, `internal/build/job_test.go`

**Interfaces:**
- Consumes: `render.Object` (`{Kind string; JSON []byte}`).
- Produces:
  - `func ImageRef(registryHost, project, app string, deployID int64) string` → `"<registryHost>/<project>-<app>:<deployID>"`.
  - `type BuildParams struct { Namespace, Name, BuilderImage, DataPVC, ImageRef, RegistryHost, SourceType, GitURL, GitBranch string; DeployID int64 }`.
  - `func RenderBuildJob(p BuildParams) (render.Object, error)` → a `batch/v1` `Job` (Kind `"Job"`) that mounts `DataPVC` at `/data`, runs `BuilderImage`, `restartPolicy: Never`, `backoffLimit: 0`, and passes the build inputs as env vars: `LUNCUR_DEPLOY_ID`, `LUNCUR_IMAGE_REF`, `LUNCUR_REGISTRY_HOST`, `LUNCUR_SOURCE_TYPE`, `LUNCUR_GIT_URL`, `LUNCUR_GIT_BRANCH`. Tarball path and log path are derived by the entrypoint from `/data` + `LUNCUR_DEPLOY_ID`.

- [ ] **Step 1: Write failing test**

```go
package build

import (
	"encoding/json"
	"testing"
)

func TestImageRef(t *testing.T) {
	if got := ImageRef("registry.luncur-system:5000", "web", "api", 42); got != "registry.luncur-system:5000/web-api:42" {
		t.Fatalf("ImageRef=%q", got)
	}
}

func TestRenderBuildJob(t *testing.T) {
	obj, err := RenderBuildJob(BuildParams{
		Namespace: "luncur-system", Name: "build-42", BuilderImage: "luncur/builder:latest",
		DataPVC: "luncur-data", ImageRef: "registry.luncur-system:5000/web-api:42",
		RegistryHost: "registry.luncur-system:5000", SourceType: "tarball", DeployID: 42,
	})
	if err != nil {
		t.Fatal(err)
	}
	if obj.Kind != "Job" {
		t.Fatalf("Kind=%q", obj.Kind)
	}
	var j struct {
		APIVersion string `json:"apiVersion"`
		Metadata   struct{ Name, Namespace string } `json:"metadata"`
		Spec       struct {
			BackoffLimit *int32 `json:"backoffLimit"`
			Template     struct {
				Spec struct {
					RestartPolicy string `json:"restartPolicy"`
					Containers    []struct {
						Image        string `json:"image"`
						Env          []struct{ Name, Value string } `json:"env"`
						VolumeMounts []struct{ Name, MountPath string } `json:"volumeMounts"`
					} `json:"containers"`
					Volumes []struct {
						Name                  string `json:"name"`
						PersistentVolumeClaim struct{ ClaimName string } `json:"persistentVolumeClaim"`
					} `json:"volumes"`
				} `json:"spec"`
			} `json:"template"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(obj.JSON, &j); err != nil {
		t.Fatal(err)
	}
	if j.APIVersion != "batch/v1" || j.Metadata.Namespace != "luncur-system" {
		t.Fatalf("bad meta: %+v", j.Metadata)
	}
	if j.Spec.BackoffLimit == nil || *j.Spec.BackoffLimit != 0 {
		t.Fatalf("backoffLimit not 0")
	}
	c := j.Spec.Template.Spec.Containers[0]
	if c.Image != "luncur/builder:latest" {
		t.Fatalf("image=%q", c.Image)
	}
	env := map[string]string{}
	for _, e := range c.Env {
		env[e.Name] = e.Value
	}
	if env["LUNCUR_IMAGE_REF"] != "registry.luncur-system:5000/web-api:42" || env["LUNCUR_DEPLOY_ID"] != "42" {
		t.Fatalf("env missing/wrong: %+v", env)
	}
	if c.VolumeMounts[0].MountPath != "/data" || j.Spec.Template.Spec.Volumes[0].PersistentVolumeClaim.ClaimName != "luncur-data" {
		t.Fatalf("data volume not wired")
	}
	if j.Spec.Template.Spec.RestartPolicy != "Never" {
		t.Fatalf("restartPolicy=%q", j.Spec.Template.Spec.RestartPolicy)
	}
}
```

- [ ] **Step 2: Run, expect FAIL.**

- [ ] **Step 3: Implement `internal/build/job.go`** using the typed `k8s.io/api/batch/v1` + `core/v1` structs (same pattern as `internal/render`), marshalling to JSON. Set `TypeMeta{APIVersion:"batch/v1", Kind:"Job"}`, `ObjectMeta{Name, Namespace, Labels:{"app.kubernetes.io/managed-by":"luncur"}}`, `Spec.BackoffLimit = ptr(int32(0))`, `Template.Spec.RestartPolicy = "Never"`, one container (`BuilderImage`, the six `LUNCUR_*` env vars, `VolumeMounts: [{Name:"data", MountPath:"/data"}]`, and `SecurityContext` allowing the rootless buildkit the builder image expects — leave to the builder image, do not set privileged), and `Volumes: [{Name:"data", PersistentVolumeClaim:{ClaimName: DataPVC}}]`. `ImageRef` is `fmt.Sprintf("%s/%s-%s:%d", registryHost, project, app, deployID)`.

```go
func ptr[T any](v T) *T { return &v }

func ImageRef(registryHost, project, app string, deployID int64) string {
	return fmt.Sprintf("%s/%s-%s:%d", registryHost, project, app, deployID)
}
```

- [ ] **Step 4: Run, expect PASS** — `go test ./internal/build/`.

- [ ] **Step 5: Commit** (`feat: render BuildKit Job manifest for source builds`).

---

### Task 3: Job GVR + WaitJob in kube

**Files:**
- Modify: `internal/kube/kube.go`, `internal/kube/kube_test.go`

**Interfaces:**
- Consumes: `gvrByKind`, the dynamic client.
- Produces: `func (c *Client) WaitJob(ctx context.Context, namespace, name string, poll time.Duration) (bool, error)` — polls the Job every `poll` until `.status.succeeded >= 1` (returns `true`), `.status.failed >= 1` (returns `false`), or `ctx` is done (returns `ctx.Err()`). Job GVR `{Group:"batch", Version:"v1", Resource:"jobs"}` added to `gvrByKind` so `Apply` can also apply Jobs.

- [ ] **Step 1: Write failing test** using `dynamicfake` with a reactor that returns a succeeded Job:

```go
func TestWaitJobSucceeded(t *testing.T) {
	scheme := runtime.NewScheme()
	dyn := dynamicfake.NewSimpleDynamicClient(scheme)
	dyn.PrependReactor("get", "jobs", func(action k8stesting.Action) (bool, runtime.Object, error) {
		u := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "batch/v1", "kind": "Job",
			"metadata": map[string]any{"name": "build-1", "namespace": "luncur-system"},
			"status":   map[string]any{"succeeded": int64(1)},
		}}
		return true, u, nil
	})
	c := NewFromDynamic(dyn)
	ok, err := c.WaitJob(context.Background(), "luncur-system", "build-1", time.Millisecond)
	if err != nil || !ok {
		t.Fatalf("WaitJob = (%v, %v), want (true, nil)", ok, err)
	}
}
```

(Add whatever imports the file lacks: `runtime`, `unstructured`, `dynamicfake`, `k8stesting "k8s.io/client-go/testing"`, `time`. Match the import style already in `kube_test.go`.)

- [ ] **Step 2: Run, expect FAIL.**

- [ ] **Step 3: Implement.** Add `"Job": {Group: "batch", Version: "v1", Resource: "jobs"}` to `gvrByKind`, then:

```go
// WaitJob polls a Job until it succeeds (true), fails (false), or ctx ends.
func (c *Client) WaitJob(ctx context.Context, namespace, name string, poll time.Duration) (bool, error) {
	for {
		u, err := c.dyn.Resource(gvrByKind["Job"]).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return false, fmt.Errorf("get job %s: %w", name, err)
		}
		if n, _, _ := unstructured.NestedInt64(u.Object, "status", "succeeded"); n >= 1 {
			return true, nil
		}
		if n, _, _ := unstructured.NestedInt64(u.Object, "status", "failed"); n >= 1 {
			return false, nil
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-time.After(poll):
		}
	}
}
```

Add imports `"time"` and `"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"` to `kube.go`.

- [ ] **Step 4: Run, expect PASS** — `go test ./internal/kube/`.

- [ ] **Step 5: Commit** (`feat: Job GVR and WaitJob polling in kube client`).

---

### Task 4: Registry + system infra manifests

**Files:**
- Create: `internal/build/infra.go`, `internal/build/infra_test.go`

**Interfaces:**
- Produces: `func SystemObjects(dataPVC, registryPVC, registryImage string) ([]render.Object, error)` → the objects `EnsureSystem` applies once at boot: the `luncur-system` Namespace (managed-by label; **no** `pod-security.kubernetes.io/enforce: restricted` — the BuildKit builder needs latitude the restricted profile denies, and this namespace holds only luncur-operated workloads, not tenant apps), a `luncur-data` PVC, a `luncur-registry` PVC, and a `registry` Deployment + Service (`registry:2`, containerPort 5000, Service port 5000, registry data on `registryPVC`).

- [ ] **Step 1: Write failing test** asserting the returned kinds/names:

```go
func TestSystemObjects(t *testing.T) {
	objs, err := SystemObjects("luncur-data", "luncur-registry", "registry:2")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, o := range objs {
		var m struct{ Metadata struct{ Name string } `json:"metadata"` }
		json.Unmarshal(o.JSON, &m)
		got[o.Kind+"/"+m.Metadata.Name] = true
	}
	for _, want := range []string{
		"Namespace/luncur-system", "PersistentVolumeClaim/luncur-data",
		"PersistentVolumeClaim/luncur-registry", "Deployment/registry", "Service/registry",
	} {
		if !got[want] {
			t.Fatalf("missing %s (have %v)", want, got)
		}
	}
}
```

- [ ] **Step 2: Run, expect FAIL.**

- [ ] **Step 3: Implement `SystemObjects`** with typed structs (PVCs: `corev1.PersistentVolumeClaim`, `AccessModes: [ReadWriteOnce]`, `Resources.Requests[storage]=resource.MustParse("2Gi")` for data, `"10Gi"` for registry; registry Deployment: 1 replica, container `registry:2` port 5000, volumeMount `/var/lib/registry` ← registryPVC; Service selector on the registry pod label, port 5000→5000). Add `"PersistentVolumeClaim": {Group:"", Version:"v1", Resource:"persistentvolumeclaims"}` to `kube.gvrByKind` in this task so `Apply` can create them. Include a `ponytail:` comment on the PVC noting `ReadWriteOnce`/single-node assumption and the RWX upgrade path.

- [ ] **Step 4: Run, expect PASS** — `go test ./internal/build/ ./internal/kube/`.

- [ ] **Step 5: Commit** (`feat: system infra manifests (namespace, registry, PVCs)`).

---

### Task 5: Async build orchestration (runBuild) + deploy handler rewrite

**Files:**
- Create: `internal/server/build.go`, `internal/server/build_test.go`
- Modify: `internal/server/server.go` (Deps + fields + routes), `internal/server/apps.go` (handleDeployApp rewrite + handleGetDeploy + handleDeployLogs), `internal/server/apps_test.go`

**Interfaces:**
- Consumes: `build.RenderBuildJob`, `build.ImageRef`, `build.Source`, `kube.WaitJob`, `kube.Apply`, `kube.EnsureNamespace`, `s.renderApp`.
- Produces:
  - `server.Deps` gains: `DataDir string`, `BuilderImage string`, `RegistryHost string`, `SystemNamespace string`, `DataPVC string`. `server.New` fills defaults when empty (`SystemNamespace="luncur-system"`, `RegistryHost="registry.luncur-system:5000"`, `BuilderImage="luncur/builder:latest"`, `DataPVC="luncur-data"`) and, when `DataDir != ""`, builds a `*build.Source` on the `server` struct (`src`).
  - `func (s *server) startBuild(p store.Project, a store.App, d store.Deployment)` — spawns a goroutine with a fresh `context.Background()` + 15-min timeout calling `runBuild`, logging any error.
  - `func (s *server) runBuild(ctx context.Context, p store.Project, a store.App, d store.Deployment) error` — renders+applies the Build Job into `SystemNamespace`, `WaitJob`, then: on success → `SetDeploymentImage`, `SetDeploymentStatus(deploying)`, `renderApp` against the built image, `EnsureNamespace`+`Apply` into the project namespace, `SetDeploymentStatus(live)`; on build failure or any apply error → `SetDeploymentStatus(failed)` and return the error. Sets `SetDeploymentLog(d.ID, src.LogPath(d.ID))` up front so the log endpoint works during the build.
  - Routes: `POST .../deploy` (rewritten), `GET /v1/projects/{project}/apps/{app}/deploys/{id}` → `handleGetDeploy`, `GET /v1/projects/{project}/apps/{app}/deploys/{id}/logs` → `handleDeployLogs`.

- [ ] **Step 1: Write failing test for `runBuild` success path** (fake client: Job reports succeeded; assert final status `live` and that a Deployment was applied into the project namespace):

```go
func TestRunBuildSuccess(t *testing.T) {
	// server_test.go already has a helper that builds a *server with a
	// store, sealer, and a fake dynamic client recording actions. Reuse it;
	// if it doesn't expose build config, set s.src/s.systemNamespace/etc here.
	srv, rec, st := newBuildTestServer(t) // returns *server, *[]recorded, *store.Store
	p, _ := st.CreateProject("web")
	a, _ := st.CreateApp(p.ID, "api", 8080)
	d, _ := st.CreateDeployment(a.ID, "building", "", 0)

	if err := srv.runBuild(context.Background(), p, a, d); err != nil {
		t.Fatalf("runBuild: %v", err)
	}
	got, _ := st.GetDeployment(d.ID)
	if got.Status != "live" {
		t.Fatalf("status=%q want live", got.Status)
	}
	if got.ImageRef == "" {
		t.Fatalf("image ref not set")
	}
	// A Deployment must have been applied into the project namespace.
	if !recordedApply(rec, p.Namespace, "deployments", a.Name) {
		t.Fatalf("app Deployment not applied; actions=%v", *rec)
	}
}
```

The fake dynamic client's reactor for `get jobs` returns a succeeded Job (as in Task 3). `recordedApply` inspects the recorded actions for a `patch` on the app Deployment in the project namespace (mirror the existing B2 `recorded`/reactor helpers). Add a `newBuildTestServer` helper (or extend the existing server test helper) that wires `s.src = must(build.NewSource(t.TempDir()))`, `s.systemNamespace="luncur-system"`, `s.registryHost="reg:5000"`, `s.builderImage="b"`, `s.dataPVC="luncur-data"`.

- [ ] **Step 2: Run, expect FAIL.**

- [ ] **Step 3: Implement `internal/server/build.go`**

```go
package server

import (
	"context"
	"log"
	"time"

	"github.com/sutantodadang/luncur/internal/build"
	"github.com/sutantodadang/luncur/internal/store"
)

func (s *server) startBuild(p store.Project, a store.App, d store.Deployment) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
		defer cancel()
		if err := s.runBuild(ctx, p, a, d); err != nil {
			log.Printf("build deploy %d failed: %v", d.ID, err)
		}
	}()
}

func (s *server) runBuild(ctx context.Context, p store.Project, a store.App, d store.Deployment) error {
	fail := func(err error) error {
		if e := s.st.SetDeploymentStatus(d.ID, "failed"); e != nil {
			log.Printf("mark deploy %d failed: %v", d.ID, e)
		}
		return err
	}

	if s.src != nil {
		if err := s.st.SetDeploymentLog(d.ID, s.src.LogPath(d.ID)); err != nil {
			log.Printf("set deploy %d log path: %v", d.ID, err)
		}
	}

	imageRef := build.ImageRef(s.registryHost, projectSlug(p), a.Name, d.ID)
	job, err := build.RenderBuildJob(build.BuildParams{
		Namespace: s.systemNamespace, Name: buildJobName(d.ID), BuilderImage: s.builderImage,
		DataPVC: s.dataPVC, ImageRef: imageRef, RegistryHost: s.registryHost,
		SourceType: a.SourceType, GitURL: a.GitURL, GitBranch: a.GitBranch, DeployID: d.ID,
	})
	if err != nil {
		return fail(err)
	}
	if err := s.kube.EnsureNamespace(ctx, s.systemNamespace); err != nil {
		return fail(err)
	}
	if err := s.kube.Apply(ctx, s.systemNamespace, []render.Object{job}); err != nil {
		return fail(err)
	}

	ok, err := s.kube.WaitJob(ctx, s.systemNamespace, buildJobName(d.ID), 2*time.Second)
	if err != nil {
		return fail(err)
	}
	if !ok {
		return fail(fmt.Errorf("build job failed"))
	}

	if err := s.st.SetDeploymentImage(d.ID, imageRef); err != nil {
		return fail(err)
	}
	if err := s.st.SetDeploymentStatus(d.ID, "deploying"); err != nil {
		return fail(err)
	}

	rendered, err := s.renderApp(p, a, imageRef, true)
	if err != nil {
		return fail(err)
	}
	if err := s.kube.EnsureNamespace(ctx, p.Namespace); err != nil {
		return fail(err)
	}
	if err := s.kube.Apply(ctx, p.Namespace, rendered.Objects); err != nil {
		return fail(err)
	}
	return s.st.SetDeploymentStatus(d.ID, "live")
}
```

Add small helpers (in `build.go` or `sync.go`): `func buildJobName(id int64) string { return fmt.Sprintf("build-%d", id) }` and `func projectSlug(p store.Project) string { return p.Name }` (project names are already DNS-safe). Add the `render` and `fmt` imports. Add the config fields (`src *build.Source`, `builderImage, registryHost, systemNamespace, dataPVC string`) to the `server` struct and populate them in `New` with the defaults noted above.

- [ ] **Step 4: Run, expect PASS** — `go test ./internal/server/ -run TestRunBuild`.

- [ ] **Step 5: Write failing test for the rewritten deploy handler** (multipart tarball upload → 202 building, source saved, goroutine NOT asserted directly — instead assert the row is `building` and a tarball file exists). Also test `GET .../deploys/{id}/logs` returns the stored log bytes. Put these in `apps_test.go` mirroring existing handler-test helpers.

- [ ] **Step 6: Run, expect FAIL.**

- [ ] **Step 7: Rewrite `handleDeployApp`** in `internal/server/apps.go`:

Behavior:
1. `requireProject` → `requireApp`.
2. `requireKube` (build needs the cluster).
3. If `Content-Type` is `multipart/form-data`: this is a **tarball** deploy. Create `building` deployment `CreateDeployment(a.ID, "building", "", u.ID)`, read the `source` file part, `s.src.Save(d.ID, part)`, then `s.startBuild(p, a, d)`, respond `202` `{"deployment_id":d.ID,"status":"building"}`.
4. Else decode JSON. If body has `{"image":"..."}` (non-empty) → the **prebuilt-image** path (unchanged B2 behavior: synchronous render+apply, status `live`), keeping direct image deploys working. If no image and the app `source_type=="git"` → create `building` deployment and `startBuild` (no tarball; the Job clones). If no image and not a git app → `400 bad_request` "provide a source tarball or an image".
5. Guard: if `s.src == nil` (server started without `--data-dir`) and a build is requested → `503 build_unavailable` "server has no data directory configured".

Keep the prebuilt-image branch as a small helper `deployImage(...)` reusing the existing code so the two paths are readable.

- [ ] **Step 8: Add `handleGetDeploy` and `handleDeployLogs`.**

```go
func (s *server) handleGetDeploy(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.requireProject(w, u, r.PathValue("project"))
	if !ok { return }
	a, ok := s.requireApp(w, p, r.PathValue("app"))
	if !ok { return }
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil { writeError(w, http.StatusBadRequest, "bad_request", "invalid deploy id"); return }
	d, err := s.st.GetDeployment(id)
	if errors.Is(err, store.ErrNotFound) || (err == nil && d.AppID != a.ID) {
		writeError(w, http.StatusNotFound, "not_found", "no such deploy"); return
	}
	if err != nil { log.Printf("get deploy: %v", err); writeError(w, http.StatusInternalServerError, "internal", "internal error"); return }
	writeJSON(w, http.StatusOK, map[string]any{
		"deployment_id": d.ID, "status": d.Status, "image": d.ImageRef,
		"url": "http://" + hostFor(a.Name, s.externalIP),
	})
}

func (s *server) handleDeployLogs(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.requireProject(w, u, r.PathValue("project"))
	if !ok { return }
	a, ok := s.requireApp(w, p, r.PathValue("app"))
	if !ok { return }
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil { writeError(w, http.StatusBadRequest, "bad_request", "invalid deploy id"); return }
	d, err := s.st.GetDeployment(id)
	if errors.Is(err, store.ErrNotFound) || (err == nil && d.AppID != a.ID) {
		writeError(w, http.StatusNotFound, "not_found", "no such deploy"); return
	}
	if err != nil { log.Printf("get deploy: %v", err); writeError(w, http.StatusInternalServerError, "internal", "internal error"); return }
	if s.src == nil { writeError(w, http.StatusServiceUnavailable, "build_unavailable", "no build logs available"); return }
	logBytes, err := s.src.ReadLog(id)
	if err != nil { log.Printf("read log: %v", err); writeError(w, http.StatusInternalServerError, "internal", "internal error"); return }
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	w.Write(logBytes)
}
```

Register the two GET routes and keep the existing POST deploy route in `server.go`. Add `"strconv"` import where needed.

- [ ] **Step 9: Run, expect PASS** — `go test ./internal/server/`.

- [ ] **Step 10: Commit** (`feat: async source-build deploy pipeline (Job → wait → apply)`).

---

### Task 6: App source model (git) + store scans

**Files:**
- Modify: `internal/store/apps.go`, `internal/store/apps_test.go`

**Interfaces:**
- Produces: `store.App` gains `SourceType, GitURL, GitBranch string`. `GetApp`/`ListApps` select and scan the new columns. New `func (s *Store) CreateGitApp(projectID int64, name string, port int, gitURL, gitBranch string) (App, error)` — validates name/port like `CreateApp`, requires non-empty `gitURL`, inserts `source_type='git'`. (Git token encryption reuses the sealer at deploy time in a later phase; the `git_token_enc` column stays unused in Phase 1 — public repos / token-in-URL only. Note this in a comment.)

- [ ] **Step 1: Write failing test** for `CreateGitApp` + that `GetApp` returns `SourceType/GitURL/GitBranch`.
- [ ] **Step 2: Run, expect FAIL.**
- [ ] **Step 3: Implement.** Update the `App` struct, add `git_url`/`git_branch`/`source_type` to the `SELECT` lists in `GetApp`/`ListApps` (scan into `sql.NullString`, assign `.String`), add `CreateGitApp`. `CreateApp` keeps inserting `source_type='tarball'`; set `App.SourceType="tarball"` in its return for consistency.
- [ ] **Step 4: Run, expect PASS** — `go test ./internal/store/`.
- [ ] **Step 5: Commit** (`feat: git-source apps in store`).

---

### Task 7: CLI source deploy, packSource, init, logs, client methods

**Files:**
- Create: `internal/cli/archive.go`, `internal/cli/archive_test.go`, `internal/cli/init.go`, `internal/cli/logs.go`
- Modify: `internal/client/client.go`, `internal/cli/deploy.go`, `internal/cli/app.go`, `internal/cli/root.go`

**Interfaces:**
- Consumes: existing `apiClient()` helper, `client.Client`.
- Produces:
  - `client.DeploySource(project, app string, tarball io.Reader) (DeployResult, error)` — POST multipart (`source` file field) to `.../deploy`; decodes `{deployment_id,status}`.
  - `client.GetDeploy(project, app string, id int64) (DeployResult, error)`; `client.DeployLogs(project, app string, id int64) ([]byte, error)` (uses `doRaw`).
  - `client.CreateGitApp(project, name string, port int, gitURL, branch string) (AppInfo, error)`.
  - `cli.packSource(dir string) (io.Reader, error)` — if `<dir>/.git` exists, run `git archive --format=tar.gz HEAD` (captures stdout to a buffer); else walk `dir` into a `tar.gz`, skipping `.git`, `node_modules`, and `.luncur`.
  - `luncur init` → writes `luncur.toml` (`app`, `project`, `port`) in the cwd.
  - `luncur logs <app> --project P` → prints the latest deploy's build log.

- [ ] **Step 1: Write failing test for `packSource`** (tar path): create a temp dir with `main.go` + a `node_modules/x` file, call `packSource`, gunzip+untar the result, assert `main.go` is present and `node_modules/x` is absent.
- [ ] **Step 2: Run, expect FAIL.**
- [ ] **Step 3: Implement `archive.go`.** Use `os/exec` for the git path (`exec.Command("git", "-C", dir, "archive", "--format=tar.gz", "HEAD")`, `cmd.Output()` into a `bytes.Buffer`); fall back to a `archive/tar` + `compress/gzip` walker (`filepath.WalkDir`) with the skip list for the non-git path. Return an `io.Reader` over the buffer.
- [ ] **Step 4: Run, expect PASS** — `go test ./internal/cli/ -run TestPackSource`.
- [ ] **Step 5: Implement client methods** (`DeploySource` builds the multipart body with `mime/multipart`; `GetDeploy`/`DeployLogs`/`CreateGitApp` mirror existing method style).
- [ ] **Step 6: Rewrite `deployCmd`**: keep `--image` (direct image deploy → `c.Deploy`, unchanged); add `--project` (still required). When `--image` is empty: `packSource(".")`, `c.DeploySource(...)`, then poll `c.GetDeploy` every 2s until status is `live`/`failed`, printing status transitions; on `failed` fetch+print the log tail and return a non-nil error; on `live` print the URL. Add `init.go` (`luncur init` writes `luncur.toml`; no network) and `logs.go` (`luncur logs`: `GetApp` for the latest deploy id? — simplest: call a small `client.LatestDeploy` — instead reuse `GetApp` which already returns latest status, and add `--deploy <id>` optional flag; if absent, print "run with --deploy <id>"). Keep `logs` minimal: `luncur logs <app> --project P --deploy N` prints `DeployLogs`. Register `init` and `logs` in `root.go`; add `--git-url`/`--branch` to `app create` calling `CreateGitApp` when `--git-url` set.
- [ ] **Step 7: Run, expect PASS** — `go test ./internal/cli/ ./internal/client/`.
- [ ] **Step 8: Commit** (`feat: CLI source deploy, packSource, init, logs`).

---

### Task 8: Builder image (Dockerfile + entrypoint)

**Files:**
- Create: `build/builder/Dockerfile`, `build/builder/entrypoint.sh`

**Interfaces:** none (container image, built by the release pipeline — no Go, no unit test).

- [ ] **Step 1: Write `build/builder/Dockerfile`** — base `moby/buildkit:rootless`, add `git`, `nixpacks` (download the release binary), and `entrypoint.sh`. Document the required env (`LUNCUR_DEPLOY_ID`, `LUNCUR_IMAGE_REF`, `LUNCUR_REGISTRY_HOST`, `LUNCUR_SOURCE_TYPE`, `LUNCUR_GIT_URL`, `LUNCUR_GIT_BRANCH`) and the `/data` mount in a comment.
- [ ] **Step 2: Write `build/builder/entrypoint.sh`** (`set -euo pipefail`):
  1. `LOG=/data/logs/${LUNCUR_DEPLOY_ID}.log`; redirect all output through `tee "$LOG"` (`exec > >(tee -a "$LOG") 2>&1`).
  2. Prepare `/workspace`: if `LUNCUR_SOURCE_TYPE=git` → `git clone --depth 1 --branch "${LUNCUR_GIT_BRANCH:-main}" "$LUNCUR_GIT_URL" /workspace`; else `mkdir -p /workspace && tar -xzf "/data/sources/${LUNCUR_DEPLOY_ID}.tar.gz" -C /workspace`.
  3. Build: if `/workspace/Dockerfile` exists → `buildctl build --frontend dockerfile.v0 --local context=/workspace --local dockerfile=/workspace --output type=image,name=$LUNCUR_IMAGE_REF,push=true,registry.insecure=true`; else `nixpacks build /workspace --out /workspace/.nixpacks` to generate a Dockerfile, then `buildctl` with `--local dockerfile=/workspace/.nixpacks`.
  4. `buildctl` needs `buildkitd`; the rootless base image's standard pattern is `buildctl-daemonless.sh buildctl build ...` — use that wrapper. Comment that `registry.insecure=true` pairs with the K3s `registries.yaml` entry `luncur up` writes (Plan D).
- [ ] **Step 3: Commit** (`feat: luncur/builder image (buildkit rootless + nixpacks)`). No test — reviewer gates by inspection.

---

### Task 9: Deferred hygiene (denylist, DSN escaping, graceful shutdown, EnsureSystem)

**Files:**
- Modify: `internal/store/overrides.go`, `internal/store/overrides_test.go`, `internal/store/store.go`, `internal/cli/serve.go`

**Interfaces:** no new exported surface except `func EnsureSystem(ctx context.Context, k *kube.Client, systemNS, dataPVC, registryPVC string) error` in `internal/build` (applies `SystemObjects` via `k.EnsureNamespace` + `k.Apply`).

- [ ] **Step 1: Write failing test** in `overrides_test.go`: `SetOverride(id, "Service", `{"spec":{"externalIPs":["1.2.3.4"]}}`)` must return a validation error, and likewise `loadBalancerIP`. (Closes the B2 residual: a member could otherwise intercept arbitrary cluster IPs.)
- [ ] **Step 2: Run, expect FAIL.**
- [ ] **Step 3: Implement.** In `rejectDangerousOverride`, add a `Service` branch that scans `collectKeys(patch)` for `externalIPs` and `loadBalancerIP` and rejects them (reuse the existing `collectKeys` walker and `validationErrorf`).
- [ ] **Step 4: Run, expect PASS** — `go test ./internal/store/`.
- [ ] **Step 5: DSN escaping** in `store.Open`: the DSN is `path + "?_pragma=..."`; if `path` contains `?` or `&` the query string breaks. Wrap with a `ponytail:` comment and escape by building the DSN with the path as an opaque prefix — minimally, reject a `path` containing `?` in `Open` (`return nil, fmt.Errorf("db path may not contain '?'")`) since real paths never do; that closes the injection without url-parsing gymnastics. (Add a one-line test.)
- [ ] **Step 6: Graceful shutdown** in `serveCmd`: replace `return srv.ListenAndServe()` with a `signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)` pattern — run `ListenAndServe` in a goroutine, on ctx done call `srv.Shutdown(context.WithTimeout(... 10s))`. (SIGTERM matters because luncur runs as a Deployment and K8s sends SIGTERM on rollout.)
- [ ] **Step 7: Wire `--data-dir`, `--builder-image`, `--registry-host` flags + `EnsureSystem`** in `serveCmd`: pass `DataDir`/`BuilderImage`/`RegistryHost` into `server.Deps`; after the kube client is built, if non-nil call `build.EnsureSystem(ctx, kubeClient, "luncur-system", "luncur-data", "luncur-registry")` (log a warning on error, don't abort boot — CI/kube-less skips it because `kubeClient` is nil).
- [ ] **Step 8: Run, expect PASS** — `go test ./...`.
- [ ] **Step 9: Commit** (`fix: override IP denylist, DSN guard, graceful shutdown, ensure system infra`).

---

### Task 10: README + final gate

**Files:**
- Modify: `README.md`

- [ ] **Step 1: Document** the build pipeline: `luncur init`, `luncur deploy` (source), `luncur deploy --image` (prebuilt, still supported), `luncur app create --git-url`, `luncur logs --deploy`, the `--data-dir`/`--builder-image`/`--registry-host` serve flags, and a one-paragraph "how a build works" (upload → Job → registry → apply). Note the `build/builder` image is built by the release pipeline and that K3s needs the insecure-registry entry (`luncur up`, Plan D).
- [ ] **Step 2: Run the full gate.**

```bash
CGO_ENABLED=0 go build ./...
go test ./...
go vet ./...
```

Expected: build succeeds with CGO disabled, all tests pass, vet clean.

- [ ] **Step 3: Commit** (`docs: build pipeline and source-deploy CLI in README`).

---

## Self-Review (against the spec)

- **Spec "Deploy pipeline" (source tarball → build Job → nixpacks/Dockerfile → push to registry → render → apply → live, URL)** — Tasks 1–8 cover the whole chain; deviations 1–3 documented above (registry as Deployment, file-based logs, shared-PVC source). ✅
- **Spec "Git URL" source** — Tasks 6 (model) + 7 (`app create --git-url`) + 8 (entrypoint clone). Private-repo token encryption is Phase-1-noted-as-public-only (column exists, unused). ✅ (partial, documented)
- **Spec "Build + rollout logs stream via SSE"** — Phase-1-minimum: `GET .../deploys/{id}/logs` returns log text (Task 5/7). Live SSE follow + runtime `luncur logs -f` → Plan D. Documented. ✅ (scoped down)
- **Spec "embedded registry (distribution lib)"** — replaced with `registry:2` Deployment, rationale in Deviations. ✅ (deliberate)
- **Spec escape hatch / overrides** — untouched and still applied in `runBuild` via `renderApp(..., true)`; plus B2 residual `Service externalIPs/loadBalancerIP` denylist closed (Task 9). ✅
- **Deferred B-plan items** — `created_by` attribution (Task 1/5), graceful shutdown (Task 9), DSN escaping (Task 9), `Service` IP denylist (Task 9). Token lifecycle/expiry remains deferred to Plan D (auth-focused). ✅
- **No new deps** — everything stdlib or already vendored. ✅
- **Untestable-without-cluster reality** — the actual buildkit/nixpacks/registry round trip is integration (k3d, per spec's testing strategy); unit tests cover source store, Job/infra rendering, `WaitJob`, and the `runBuild` state machine via the fake dynamic client. The builder image (Task 8) is inspection-gated. Documented. ✅
