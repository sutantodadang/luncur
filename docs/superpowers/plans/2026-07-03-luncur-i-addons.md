# luncur Plan I — addons (Postgres/Redis) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `luncur addon add postgres --app web --project p` provisions a Postgres instance beside the app and injects `DATABASE_URL`; addons are project-level, attachable to many apps, managed from CLI and UI.

**Architecture:** Addon rows (sealed credentials in SQLite, same pattern as env_vars) render to StatefulSet + PVC + headless Service + credentials Secret in the project namespace, applied with SSA. Attachments inject connection URLs into `renderApp`'s env map (user env wins collisions; second same-type addon gets a suffixed key). pods/exec plumbing (client-go `remotecommand`) lands here for Plans K/L to consume.

**Tech Stack:** Go stdlib, client-go (dynamic + typed + `tools/remotecommand`), modernc.org/sqlite, cobra.

## Global Constraints

- Single Go module, one binary from `cmd/luncur`. **No new dependencies** — `k8s.io/client-go/tools/remotecommand` is part of the existing client-go module.
- Server-side apply everywhere, `fieldManager=luncur`. API error envelope via `writeError`. Conventional commits; `go build ./... && go vet ./... && go test ./...` before every commit.
- Tests must not require a cluster: fake dynamic/typed clients; the exec path hides behind a `PodExecer` interface faked in tests (real SPDY executor is validated manually on the owner's VPS).
- CSRF (Plan G) applies to all new UI forms (`_csrf` hidden field; POSTs via `uiPage`).

---

### Task 1: store — addons + attachments

**Files:**
- Modify: `internal/store/schema.sql`
- Create: `internal/store/addons.go`
- Test: `internal/store/addons_test.go`

**Interfaces:**
- Consumes: `openTest(t)`, `ErrNotFound`, `validName` (in `apps.go` — reuse for addon names).
- Produces:
  - `type Addon struct { ID, ProjectID int64; Type, Name, Version string; SizeGB int; CredsEnc []byte; CreatedAt string }`
  - `Store.CreateAddon(projectID int64, typ, name, version string, sizeGB int, credsEnc []byte) (Addon, error)` — typ ∈ postgres|redis; name validated with `validName`, unique per project.
  - `Store.GetAddon(projectID int64, name string) (Addon, error)`
  - `Store.ListAddons(projectID int64) ([]Addon, error)`
  - `Store.DeleteAddon(id int64) error`
  - `Store.AttachAddon(addonID, appID int64) error` — duplicate attach → friendly error.
  - `Store.DetachAddon(addonID, appID int64) error` — `ErrNotFound` when not attached.
  - `Store.AddonsForApp(appID int64) ([]Addon, error)` (ordered by addon id)
  - `Store.AppsForAddon(addonID int64) ([]App, error)`

- [ ] **Step 1: Failing test** (`internal/store/addons_test.go`):

```go
package store

import (
	"errors"
	"testing"
)

func TestAddonLifecycle(t *testing.T) {
	s := openTest(t)
	p, _ := s.CreateProject("proj")
	a1, _ := s.CreateApp(p.ID, "web", 8080)
	a2, _ := s.CreateApp(p.ID, "worker", 8080)

	ad, err := s.CreateAddon(p.ID, "postgres", "db1", "16", 1, []byte("sealed"))
	if err != nil {
		t.Fatal(err)
	}
	if ad.Type != "postgres" || ad.Name != "db1" || ad.SizeGB != 1 || string(ad.CredsEnc) != "sealed" {
		t.Fatalf("addon = %+v", ad)
	}
	if _, err := s.CreateAddon(p.ID, "mysql", "db2", "8", 1, nil); err == nil {
		t.Fatal("bad type accepted")
	}
	if _, err := s.CreateAddon(p.ID, "postgres", "db1", "16", 1, nil); err == nil {
		t.Fatal("duplicate name accepted")
	}
	if _, err := s.CreateAddon(p.ID, "postgres", "Bad_Name", "16", 1, nil); err == nil {
		t.Fatal("invalid name accepted")
	}

	got, err := s.GetAddon(p.ID, "db1")
	if err != nil || got.ID != ad.ID {
		t.Fatalf("get: %+v err=%v", got, err)
	}
	if _, err := s.GetAddon(p.ID, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing addon: %v", err)
	}

	if err := s.AttachAddon(ad.ID, a1.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.AttachAddon(ad.ID, a1.ID); err == nil {
		t.Fatal("duplicate attach accepted")
	}
	if err := s.AttachAddon(ad.ID, a2.ID); err != nil {
		t.Fatal(err)
	}

	forApp, err := s.AddonsForApp(a1.ID)
	if err != nil || len(forApp) != 1 || forApp[0].Name != "db1" {
		t.Fatalf("addons for app: %+v err=%v", forApp, err)
	}
	apps, err := s.AppsForAddon(ad.ID)
	if err != nil || len(apps) != 2 {
		t.Fatalf("apps for addon: %+v err=%v", apps, err)
	}

	if err := s.DetachAddon(ad.ID, a1.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.DetachAddon(ad.ID, a1.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second detach: %v", err)
	}

	// Destroying an app cascades its attachments but keeps the addon.
	if err := s.DeleteApp(a2.ID); err != nil {
		t.Fatal(err)
	}
	if apps, _ := s.AppsForAddon(ad.ID); len(apps) != 0 {
		t.Fatalf("attachment survived app delete: %+v", apps)
	}
	if _, err := s.GetAddon(p.ID, "db1"); err != nil {
		t.Fatalf("addon deleted with app: %v", err)
	}

	if err := s.DeleteAddon(ad.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteAddon(ad.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete: %v", err)
	}
}
```

- [ ] **Step 2: Run** `go test ./internal/store/ -run TestAddonLifecycle -v` — compile failure.

- [ ] **Step 3: Implement.**

`schema.sql` — append:

```sql
CREATE TABLE IF NOT EXISTS addons (
  id         INTEGER PRIMARY KEY,
  project_id INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  type       TEXT NOT NULL CHECK (type IN ('postgres','redis')),
  name       TEXT NOT NULL,
  version    TEXT NOT NULL,
  size_gb    INTEGER NOT NULL DEFAULT 1,
  creds_enc  BLOB,
  created_at TEXT NOT NULL DEFAULT (datetime('now')),
  UNIQUE (project_id, name)
);

CREATE TABLE IF NOT EXISTS addon_attachments (
  addon_id INTEGER NOT NULL REFERENCES addons(id) ON DELETE CASCADE,
  app_id   INTEGER NOT NULL REFERENCES apps(id) ON DELETE CASCADE,
  PRIMARY KEY (addon_id, app_id)
);
```

`internal/store/addons.go`:

```go
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// Addon is a project-level Postgres/Redis instance; credentials are sealed
// (same pattern as env_vars) and materialized into a K8s Secret at
// provision time.
type Addon struct {
	ID        int64
	ProjectID int64
	Type      string
	Name      string
	Version   string
	SizeGB    int
	CredsEnc  []byte
	CreatedAt string
}

func (s *Store) CreateAddon(projectID int64, typ, name, version string, sizeGB int, credsEnc []byte) (Addon, error) {
	if typ != "postgres" && typ != "redis" {
		return Addon{}, fmt.Errorf("unsupported addon type %q (postgres|redis)", typ)
	}
	if !validName(name) {
		return Addon{}, fmt.Errorf("invalid addon name %q (lowercase letters, digits, dashes; max 40 chars)", name)
	}
	if sizeGB < 1 {
		sizeGB = 1
	}
	res, err := s.db.Exec(
		`INSERT INTO addons (project_id, type, name, version, size_gb, creds_enc)
		 VALUES (?, ?, ?, ?, ?, ?)`, projectID, typ, name, version, sizeGB, credsEnc)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return Addon{}, fmt.Errorf("addon %q already exists in this project", name)
		}
		return Addon{}, err
	}
	id, _ := res.LastInsertId()
	return s.getAddonByID(id)
}

const addonCols = `id, project_id, type, name, version, size_gb, creds_enc, created_at`

func (s *Store) scanAddon(row *sql.Row) (Addon, error) {
	var a Addon
	err := row.Scan(&a.ID, &a.ProjectID, &a.Type, &a.Name, &a.Version, &a.SizeGB, &a.CredsEnc, &a.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Addon{}, ErrNotFound
	}
	return a, err
}

func (s *Store) getAddonByID(id int64) (Addon, error) {
	return s.scanAddon(s.db.QueryRow(`SELECT `+addonCols+` FROM addons WHERE id = ?`, id))
}

func (s *Store) GetAddon(projectID int64, name string) (Addon, error) {
	return s.scanAddon(s.db.QueryRow(
		`SELECT `+addonCols+` FROM addons WHERE project_id = ? AND name = ?`, projectID, name))
}

func (s *Store) listAddons(query string, args ...any) ([]Addon, error) {
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Addon
	for rows.Next() {
		var a Addon
		if err := rows.Scan(&a.ID, &a.ProjectID, &a.Type, &a.Name, &a.Version, &a.SizeGB, &a.CredsEnc, &a.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Store) ListAddons(projectID int64) ([]Addon, error) {
	return s.listAddons(`SELECT `+addonCols+` FROM addons WHERE project_id = ? ORDER BY id`, projectID)
}

func (s *Store) AddonsForApp(appID int64) ([]Addon, error) {
	return s.listAddons(
		`SELECT a.id, a.project_id, a.type, a.name, a.version, a.size_gb, a.creds_enc, a.created_at
		 FROM addons a JOIN addon_attachments t ON t.addon_id = a.id
		 WHERE t.app_id = ? ORDER BY a.id`, appID)
}

func (s *Store) DeleteAddon(id int64) error {
	res, err := s.db.Exec(`DELETE FROM addons WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) AttachAddon(addonID, appID int64) error {
	_, err := s.db.Exec(
		`INSERT INTO addon_attachments (addon_id, app_id) VALUES (?, ?)`, addonID, appID)
	if err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed") {
		return fmt.Errorf("addon is already attached to this app")
	}
	return err
}

func (s *Store) DetachAddon(addonID, appID int64) error {
	res, err := s.db.Exec(
		`DELETE FROM addon_attachments WHERE addon_id = ? AND app_id = ?`, addonID, appID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// AppsForAddon lists the apps an addon is attached to.
func (s *Store) AppsForAddon(addonID int64) ([]App, error) {
	rows, err := s.db.Query(
		`SELECT a.id FROM apps a JOIN addon_attachments t ON t.app_id = a.id
		 WHERE t.addon_id = ? ORDER BY a.id`, addonID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]App, 0, len(ids))
	for _, id := range ids {
		a, err := s.GetAppByID(id)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, nil
}
```

(Confirm `validName` is reachable from `addons.go` — it's unexported in the same package, so yes. Confirm `DeleteApp`'s signature in `apps.go` — the test uses `DeleteApp(a2.ID)`.)

- [ ] **Step 4: Run** `go test ./internal/store/ -v` — all pass.
- [ ] **Step 5: Commit** — `feat: addons + attachments store`

---

### Task 2: internal/addon — manifest rendering

**Files:**
- Create: `internal/addon/addon.go`
- Modify: `internal/kube/kube.go` (StatefulSet GVR + readiness helper)
- Test: `internal/addon/addon_test.go`, `internal/kube/kube_test.go` (append)

**Interfaces:**
- Consumes: `render.Object`, typed k8s structs (appsv1.StatefulSet already available), `up/manifests.go`'s add-closure style.
- Produces:
  - `type Params struct { Namespace, Type, Name, Version string; SizeGB int; Creds Creds }`
  - `type Creds struct { User, Password, DB string }` (Redis uses only Password)
  - `addon.ServiceName(name string) string` → `"addon-" + name`
  - `addon.SecretName(name string) string` → `"addon-" + name + "-creds"`
  - `addon.Render(p Params) ([]render.Object, error)` — StatefulSet (1 replica, image `postgres:<version>-alpine` / `redis:<version>-alpine`, env from the Secret, volumeClaimTemplates 1 PVC `<SizeGB>Gi`, readiness probe: `pg_isready -U app` exec / tcp 6379), headless Service (clusterIP None, port 5432/6379), Secret (stringData: POSTGRES_USER/POSTGRES_PASSWORD/POSTGRES_DB or REDIS_PASSWORD; the redis container gets `--requirepass $(REDIS_PASSWORD)` args). All labeled `app.kubernetes.io/managed-by: luncur` + `luncur.dev/addon: <name>`.
  - kube: `gvrByKind` gains `"StatefulSet": {Group: "apps", Version: "v1", Resource: "statefulsets"}`; `Client.StatefulSetReady(ctx, namespace, name string) (bool, error)` — dynamic get, `status.readyReplicas >= 1`, NotFound → `(false, nil)`.

- [ ] **Step 1: Failing tests.**

`internal/addon/addon_test.go`:

```go
package addon

import (
	"strings"
	"testing"
)

func TestRenderPostgres(t *testing.T) {
	objs, err := Render(Params{
		Namespace: "proj", Type: "postgres", Name: "db1", Version: "16",
		SizeGB: 2, Creds: Creds{User: "app", Password: "pw123", DB: "app"},
	})
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]int{}
	all := ""
	for _, o := range objs {
		kinds[o.Kind]++
		all += string(o.JSON)
	}
	if kinds["StatefulSet"] != 1 || kinds["Service"] != 1 || kinds["Secret"] != 1 {
		t.Fatalf("kinds = %v", kinds)
	}
	for _, want := range []string{
		"postgres:16-alpine", "addon-db1", "addon-db1-creds",
		`"2Gi"`, "POSTGRES_PASSWORD", "pg_isready", `"clusterIP":"None"`,
	} {
		if !strings.Contains(all, want) {
			t.Fatalf("manifests missing %q", want)
		}
	}
}

func TestRenderRedis(t *testing.T) {
	objs, err := Render(Params{
		Namespace: "proj", Type: "redis", Name: "cache", Version: "7",
		SizeGB: 1, Creds: Creds{Password: "pw123"},
	})
	if err != nil {
		t.Fatal(err)
	}
	all := ""
	for _, o := range objs {
		all += string(o.JSON)
	}
	for _, want := range []string{"redis:7-alpine", "requirepass", "REDIS_PASSWORD", "6379"} {
		if !strings.Contains(all, want) {
			t.Fatalf("manifests missing %q", want)
		}
	}
}

func TestRenderRejectsBadType(t *testing.T) {
	if _, err := Render(Params{Namespace: "p", Type: "mysql", Name: "x", Version: "8"}); err == nil {
		t.Fatal("bad type accepted")
	}
}
```

`internal/kube/kube_test.go` — append:

```go
func TestStatefulSetReady(t *testing.T) {
	sts := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apps/v1", "kind": "StatefulSet",
		"metadata": map[string]any{"name": "addon-db1", "namespace": "proj"},
		"status":   map[string]any{"readyReplicas": int64(1)},
	}}
	dyn := newFakeDyn(t, sts) // reuse the file's existing fake-dynamic constructor
	c := NewForTest(dyn, nil)
	ok, err := c.StatefulSetReady(context.Background(), "proj", "addon-db1")
	if err != nil || !ok {
		t.Fatalf("ready = %v err=%v", ok, err)
	}
	ok, err = c.StatefulSetReady(context.Background(), "proj", "absent")
	if err != nil || ok {
		t.Fatalf("absent: ready=%v err=%v, want false nil", ok, err)
	}
}
```

(Adapt `newFakeDyn` to the file's actual fake-dynamic helper; register the StatefulSet list kind in its scheme map if required.)

- [ ] **Step 2: Run** — compile failures.

- [ ] **Step 3: Implement.** `internal/addon/addon.go` follows `up/manifests.go`'s style (typed structs, `add` closure, `ptr` helper). Key shapes:

```go
// Package addon renders managed Postgres/Redis instances: StatefulSet +
// headless Service + credentials Secret, all in the app project's
// namespace.
package addon

import (
	"encoding/json"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/sutantodadang/luncur/internal/render"
)

type Creds struct{ User, Password, DB string }

type Params struct {
	Namespace, Type, Name, Version string
	SizeGB                         int
	Creds                          Creds
}

func ServiceName(name string) string { return "addon-" + name }
func SecretName(name string) string  { return "addon-" + name + "-creds" }

func ptr[T any](v T) *T { return &v }
```

Postgres container: image `fmt.Sprintf("postgres:%s-alpine", p.Version)`, `EnvFrom` the Secret, port 5432, readiness `Exec: pg_isready -U app`, volumeMount `data` at `/var/lib/postgresql/data` with `PGDATA=/var/lib/postgresql/data/pgdata` env. Redis container: image `redis:<v>-alpine`, `Args: []string{"--requirepass", "$(REDIS_PASSWORD)"}`, `Env` from Secret key, port 6379, readiness TCP 6379, volumeMount at `/data`. StatefulSet `ServiceName: ServiceName(p.Name)`, `VolumeClaimTemplates` with `resource.MustParse(fmt.Sprintf("%dGi", p.SizeGB))`. Headless Service: `ClusterIP: "None"`, selector `luncur.dev/addon: <name>`, port 5432/6379. Secret stringData per type. Labels on everything: `app.kubernetes.io/managed-by: luncur`, `luncur.dev/addon: p.Name`.

kube additions:

```go
	"StatefulSet": {Group: "apps", Version: "v1", Resource: "statefulsets"},
```

```go
// StatefulSetReady reports whether a StatefulSet has at least one ready
// replica. Absent → (false, nil): callers poll during provisioning.
func (c *Client) StatefulSetReady(ctx context.Context, namespace, name string) (bool, error) {
	u, err := c.dyn.Resource(gvrByKind["StatefulSet"]).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	n, _, _ := unstructured.NestedInt64(u.Object, "status", "readyReplicas")
	return n >= 1, nil
}
```

- [ ] **Step 4: Run** `go test ./internal/addon/ ./internal/kube/ -v` — pass.
- [ ] **Step 5: Commit** — `feat: addon manifest rendering + statefulset plumbing`

---

### Task 3: kube — pods/exec plumbing + ClusterRole update

**Files:**
- Modify: `internal/kube/kube.go` (retain rest.Config; ExecPod)
- Modify: `internal/up/manifests.go` (ClusterRole rules)
- Test: `internal/up/manifests_test.go` (extend), `internal/kube/kube_test.go` (compile-level assertion)

**Interfaces:**
- Consumes: existing `Client`, `New`.
- Produces:
  - `Client` gains `cfg *rest.Config` (set in `New`; nil in test constructors).
  - `type PodExecer interface { ExecPod(ctx context.Context, namespace, pod, container string, cmd []string, stdout, stderr io.Writer) error }` — declared in kube; `*Client` implements it via `remotecommand.NewSPDYExecutor`. `ExecPod` on a cfg-less client returns a clear error (`exec unavailable: no rest config`).
  - ClusterRole: `pods/exec` create; `statefulsets` full verbs.

- [ ] **Step 1: Failing tests.** `internal/up/manifests_test.go`: add `"pods/exec"` and `"statefulsets"` to the required-substring list in `TestLuncurObjects`. `internal/kube/kube_test.go`: compile-level check —

```go
func TestClientImplementsPodExecer(t *testing.T) {
	var _ PodExecer = (*Client)(nil)
	c := NewForTest(nil, nil)
	err := c.ExecPod(context.Background(), "ns", "pod", "c", []string{"true"}, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "exec unavailable") {
		t.Fatalf("cfg-less exec: %v", err)
	}
}
```

- [ ] **Step 2: Run** — failures.

- [ ] **Step 3: Implement.** `Client` struct gains `cfg *rest.Config`; `New` sets it. ExecPod:

```go
// PodExecer runs a command inside a pod container. Faked in tests; the
// real implementation streams over SPDY.
type PodExecer interface {
	ExecPod(ctx context.Context, namespace, pod, container string, cmd []string, stdout, stderr io.Writer) error
}

// ExecPod implements PodExecer via the pods/exec subresource.
func (c *Client) ExecPod(ctx context.Context, namespace, pod, container string, cmd []string, stdout, stderr io.Writer) error {
	if c.cfg == nil {
		return fmt.Errorf("exec unavailable: no rest config (test client?)")
	}
	req := c.cs.CoreV1().RESTClient().Post().
		Resource("pods").Namespace(namespace).Name(pod).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: container, Command: cmd,
			Stdout: true, Stderr: true,
		}, scheme.ParameterCodec)
	exec, err := remotecommand.NewSPDYExecutor(c.cfg, "POST", req.URL())
	if err != nil {
		return err
	}
	return exec.StreamWithContext(ctx, remotecommand.StreamOptions{Stdout: stdout, Stderr: stderr})
}
```

(imports: `k8s.io/client-go/tools/remotecommand`, `k8s.io/client-go/kubernetes/scheme`.) ClusterRole rules in `manifests.go`: change the `apps` full rule to include statefulsets — `rule([]string{"apps"}, []string{"deployments", "statefulsets"}, full...)` — and add `rule([]string{""}, []string{"pods/exec"}, "create")`.

- [ ] **Step 4: Run** `go test ./internal/kube/ ./internal/up/ -v && go build ./...` — pass.
- [ ] **Step 5: Commit** — `feat: pods/exec plumbing + statefulset RBAC`

---

### Task 4: server — addon API + env injection

**Files:**
- Create: `internal/server/addons.go`
- Modify: `internal/server/sync.go` (injection in renderApp)
- Modify: `internal/server/server.go` (routes)
- Test: `internal/server/addons_test.go`

**Interfaces:**
- Consumes: Tasks 1-2 (`store` addon methods, `addon.Render/ServiceName/SecretName`), `s.sealer` (Seal/Open — see `appenv.go`), `requireProject`/`requireApp`/`requireKube`, `kube.Apply`/`StatefulSetReady`, `syncIfLive`.
- Produces:
  - `POST /v1/projects/{project}/addons` body `{"type":"postgres","name":"","version":"","size_gb":1,"app":""}` → creates (name default `<type><n>` — count existing of type +1; version default 16/7), seals creds JSON `{"user":"app","password":<24-hex>,"db":"app"}` (redis: password only), applies manifests, optionally attaches to `app` (the CLI `addon add` sugar) → 201 `{"name","type","version","status":"provisioning","attached_to":[...]}`. Kube absent → 503.
  - `GET /v1/projects/{project}/addons` → list with `"ready": bool` (StatefulSetReady; false when kube nil) + `"attached_to": [app names]`.
  - `POST /v1/projects/{project}/addons/{name}/attach` body `{"app":"web"}` → 204 + re-sync; response header-less; collision warning in body `{"warning":"..."}` with 200 instead of 204 when the injected key collides with a user env var.
  - `POST /v1/projects/{project}/addons/{name}/detach` body `{"app":"web"}` → 204 + re-sync.
  - `DELETE /v1/projects/{project}/addons/{name}?force=1&keep_data=1` → 204; attached and no force → 409 `addon_attached`; deletes StatefulSet/Service/Secret (+ PVCs `data-addon-<name>-0` unless keep_data).
  - `renderApp` injection: `s.addonEnv(a store.App) (map[string]string, []string, error)` — returns env map + collision-warning keys computed against the user env; connection URLs per spec (`DATABASE_URL`, `REDIS_URL`; second+ same-type addon → `DATABASE_URL_<NAME>` with name uppercased, dashes→underscores). User env wins collisions.
  - `addonCreds` helpers: `newAddonCreds(typ string) (addon.Creds, error)` (crypto/rand 12 bytes hex password), seal/unseal via JSON round-trip.

- [ ] **Step 1: Failing tests** (`internal/server/addons_test.go`, fake-kube fixture from `apps_test.go` — reactor records applies):

```go
func TestAddonCreateAttachInject(t *testing.T) {
	// Arrange: seeded project "proj" + app "web" with a live deployment,
	// fake kube recording Apply patches, sealer configured.
	// 1. POST /v1/projects/proj/addons {"type":"postgres","app":"web"} →
	//    201; name "postgres1"; fake kube saw a StatefulSet "addon-postgres1"
	//    and Secret "addon-postgres1-creds" applied in namespace "proj".
	// 2. GET .../addons → one row, attached_to ["web"].
	// 3. renderApp (call directly, same package) → app Secret JSON contains
	//    DATABASE_URL with "addon-postgres1.proj" and ":5432/app".
	// 4. Set user env DATABASE_URL=custom (setAppEnv) → renderApp Secret
	//    contains "custom" (user wins); POST attach again → 409 duplicate...
	//    (attach duplicate returns 400 bad_request with the store's message).
	// 5. POST .../addons/postgres1/detach {"app":"web"} → 204; renderApp no
	//    longer injects DATABASE_URL (user env var remains).
}

func TestAddonRemoveGuard(t *testing.T) {
	// create addon attached to an app: DELETE without force → 409
	// addon_attached; DELETE ?force=1 → 204, fake kube saw deletions
	// (kube.Delete calls or applied absence — assert via the fake's
	// recorded actions for statefulsets delete).
}

func TestAddonSecondSameTypeSuffix(t *testing.T) {
	// two postgres addons attached to one app → renderApp env has
	// DATABASE_URL (first) and DATABASE_URL_<SECONDNAME> (second).
}
```

(Real code. For deletion the kube client needs a delete helper — check `internal/kube/kube.go` for an existing Delete method (`destroy` uses one; find it — `handleDeleteApp` path); reuse it. If it deletes by kind+name list, follow that pattern for StatefulSet/Service/Secret/PVC.)

- [ ] **Step 2: Run** — failures.

- [ ] **Step 3: Implement.** `internal/server/addons.go` — handlers per the Interfaces block. Core pieces:

```go
// newAddonCreds mints credentials for a new addon instance.
func newAddonCreds(typ string) (addon.Creds, error) {
	raw := make([]byte, 12)
	if _, err := rand.Read(raw); err != nil {
		return addon.Creds{}, err
	}
	pw := hex.EncodeToString(raw)
	if typ == "postgres" {
		return addon.Creds{User: "app", Password: pw, DB: "app"}, nil
	}
	return addon.Creds{Password: pw}, nil
}

// sealCreds / unsealCreds JSON-round-trip addon.Creds through the sealer.
```

`s.addonEnv`:

```go
// addonEnv computes injected connection env for an app's attachments.
// Returns the env map and the keys that collide with user env (user wins).
func (s *server) addonEnv(a store.App, userEnv map[string]string) (map[string]string, []string, error) {
	addons, err := s.st.AddonsForApp(a.ID)
	if err != nil {
		return nil, nil, err
	}
	out := map[string]string{}
	var collisions []string
	seenType := map[string]bool{}
	for _, ad := range addons {
		creds, err := s.unsealCreds(ad)
		if err != nil {
			return nil, nil, fmt.Errorf("unseal addon %s creds: %w", ad.Name, err)
		}
		var key, url string
		host := addon.ServiceName(ad.Name) + "." + /* project namespace: */ nsForAddon
		switch ad.Type {
		case "postgres":
			key = "DATABASE_URL"
			url = fmt.Sprintf("postgres://%s:%s@%s:5432/%s", creds.User, creds.Password, host, creds.DB)
		case "redis":
			key = "REDIS_URL"
			url = fmt.Sprintf("redis://:%s@%s:6379", creds.Password, host)
		}
		if seenType[ad.Type] {
			key = key + "_" + strings.ToUpper(strings.ReplaceAll(ad.Name, "-", "_"))
		}
		seenType[ad.Type] = true
		if _, taken := userEnv[key]; taken {
			collisions = append(collisions, key)
			continue
		}
		out[key] = url
	}
	return out, collisions, nil
}
```

(The namespace comes from the caller — pass `p.Namespace` as a parameter instead of the `nsForAddon` placeholder shown here; final signature `addonEnv(p store.Project, a store.App, userEnv map[string]string)`.) Wire into `renderApp` in `sync.go`: after the user env map is built, call `addonEnv(p, a, env)` and merge `out` into `env` (log collisions at render time). Provisioning: `addon.Render` → `EnsureNamespace` → `Apply`. Deletion: reuse the kube delete pathway `handleDeleteApp` uses (read `apps.go`/`kube.go` for the exact helper — likely `kube.Delete(ctx, ns, kind, name)` or similar list-based form) for StatefulSet `addon-<name>`, Service, Secret, and PVC `data-addon-<name>-0` (the StatefulSet volumeClaimTemplate PVC name; skip when `keep_data=1`).

Routes:

```go
	mux.HandleFunc("POST /v1/projects/{project}/addons", s.authed(s.handleCreateAddon))
	mux.HandleFunc("GET /v1/projects/{project}/addons", s.authed(s.handleListAddons))
	mux.HandleFunc("POST /v1/projects/{project}/addons/{name}/attach", s.authed(s.handleAttachAddon))
	mux.HandleFunc("POST /v1/projects/{project}/addons/{name}/detach", s.authed(s.handleDetachAddon))
	mux.HandleFunc("DELETE /v1/projects/{project}/addons/{name}", s.authed(s.handleDeleteAddon))
```

- [ ] **Step 4: Run** `go test ./internal/server/ -v` — pass.
- [ ] **Step 5: Commit** — `feat: addon API — provision, attach, env injection`

---

### Task 5: CLI — addon commands

**Files:**
- Modify: `internal/client/client.go`
- Create: `internal/cli/addon.go`
- Modify: `internal/cli/root.go`
- Test: `internal/cli/commands_test.go` (append)

**Interfaces:**
- Consumes: Task 4 endpoints, `Client.do`, `apiClient()`, cobra patterns (`domain.go`).
- Produces:
  - Client: `CreateAddon(project string, req AddonCreate) (AddonInfo, error)` with `type AddonCreate struct { Type, Name, Version, App string; SizeGB int }` / `type AddonInfo struct { Name, Type, Version, Status string; Ready bool; AttachedTo []string; Warning string }` (json tags matching Task 4 responses); `ListAddons(project) ([]AddonInfo, error)`; `AttachAddon(project, name, app string) (warning string, err error)`; `DetachAddon(project, name, app string) error`; `RemoveAddon(project, name string, force, keepData bool) error`.
  - CLI: `luncur addon create <type> --project P [--name N] [--version V] [--size 1]`; `addon add <type> --app A --project P [...]` (create+attach sugar → same endpoint with `app` set); `addon attach <name> <app> --project P` (prints warning if any); `addon detach <name> <app> --project P`; `addon list --project P` (tabwriter NAME/TYPE/VERSION/READY/ATTACHED); `addon remove <name> --project P [--force] [--keep-data]`.

- [ ] **Step 1: Failing test** — append `TestAddonCommands` to `commands_test.go`: `testEnv` has no kube, so `addon create` surfaces the server's 503 — assert the command errors mentioning "kubernetes". That's the honest CLI-level test without a cluster; wire-level behavior is covered by Task 4's server tests. Also assert `addon list` on an empty project succeeds with just the header.
- [ ] **Step 2: Run** — compile failure.
- [ ] **Step 3: Implement** per Interfaces (client methods with the `do` helper; `addon.go` mirroring `domain.go`'s subcommand structure; register `addonCmd()` in `root.go`).
- [ ] **Step 4: Run** `go test ./internal/client/ ./internal/cli/ -v` — pass.
- [ ] **Step 5: Commit** — `feat: luncur addon CLI`

---

### Task 6: web UI — addons sections

**Files:**
- Modify: `internal/server/ui.go` (project + app page view-models, handlers, routes)
- Modify: `internal/server/templates/apps.html` (project-level Addons section)
- Modify: `internal/server/templates/app.html` (attached addons + attach form)
- Test: `internal/server/ui_test.go` (append)

**Interfaces:**
- Consumes: Task 4 handlers' shared logic (extract cores where >a few lines: `s.createAddon(...)`, `s.removeAddon(...)` used by both API and UI, following the `addDomain` precedent), CSRF helpers.
- Produces:
  - Project page (`apps.html`): "Addons" section — table (name, type, version, ready, attached apps) + create form (type select, name optional) + remove buttons (with `force` checkbox); routes `POST /ui/projects/{project}/addons`, `POST /ui/projects/{project}/addons/delete`.
  - App page (`app.html`): "Addons" section — attached list with detach buttons + attach form (select over the project's addons); routes `POST /ui/projects/{project}/apps/{app}/addons/attach`, `.../addons/detach`. Attach collision warning rides the `?warn=` query param like domains.
  - All forms carry `_csrf`; all handlers `uiPage`-wrapped.

- [ ] **Step 1: Failing test** — append `TestUIAddons` to `ui_test.go`: with the fake-kube fixture, admin posts the project-page create form (type postgres) → 303; project page lists `postgres1` ; app-page attach form → 303; app page shows the addon; detach → gone; project-page delete with force → addon gone. (CSRF-correct posts via the existing helpers.)
- [ ] **Step 2: Run** — failures.
- [ ] **Step 3: Implement** per Interfaces — extract shared cores from the Task 4 handlers where needed so API and UI don't duplicate; template sections follow the Domains section's table+form idiom (copy its structure, swap fields).
- [ ] **Step 4: Run** `go test ./internal/server/ -v` — pass.
- [ ] **Step 5: Commit** — `feat: web UI addons management`

---

### Task 7: README + final verification

**Files:**
- Modify: `README.md`

- [ ] **Step 1: README** — "Addons (Postgres/Redis)" section under deployment docs: create/add/attach/detach/list/remove commands, injected env var names + collision rule + suffix rule, `--keep-data`, the "addons are never deleted implicitly" lifecycle note, UI mention. Status line: "Phase 3 in progress — addons shipped (Plan I)".
- [ ] **Step 2: Run** `go build ./... && go vet ./... && go test ./...` — green; `gofmt -l internal/ cmd/` clean; `grep -rn "Plan I" README.md internal/` — only the intentional mention.
- [ ] **Step 3: Commit** — `docs: addons usage`

---

## Final verification (after all tasks)

- [ ] `go build ./... && go vet ./... && go test ./...` — everything green.
- [ ] Push branch `plan-i`, open PR against `main`.
- [ ] Manual (owner's VPS, post-merge): `addon add postgres --app web` → app's `DATABASE_URL` connects; redis variant; detach/remove/keep-data paths.

## Spec-coverage self-check (Plan I section of 2026-07-03-luncur-phase3-design.md)

- Per-project instances + per-app attachments, sugar `addon add` ✅ (T1/T4/T5)
- Sealed creds in SQLite, materialized to Secret; render-time injection without cluster reads ✅ (T1/T4)
- StatefulSet/PVC/headless Service/Secret manifests, pinned alpine images, `--size` ✅ (T2)
- Status from StatefulSet readiness in CLI + UI ✅ (T2/T4/T5/T6)
- DATABASE_URL/REDIS_URL injection; user-env wins + warning; suffix for second same-type ✅ (T4)
- Never deleted implicitly; `--force`, `--keep-data` ✅ (T4/T5)
- pods/exec plumbing + PodExecer + ClusterRole (pods/exec, statefulsets) ✅ (T3)
- UI project + app sections ✅ (T6)
