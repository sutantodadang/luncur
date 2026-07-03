# luncur Plan B2 — K8s Applier, App API, CLI Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Deploy a prebuilt image end-to-end: `luncur deploy api --project web --image nginx` renders manifests (with overrides), server-side-applies them to K3s, and the app is reachable at `api.<ip>.sslip.io`. Full project/app/env/scale/destroy/raw/edit surface over API + CLI.

**Architecture:** New `internal/kube` wraps a dynamic client: hardcoded GVR map (5 kinds), server-side apply with `fieldManager=luncur`, namespace ensure, object deletion. `internal/server` grows a `Deps` struct (store + sealer + kube + external IP) and project-scoped authz (admin bypass, membership check). Mutations (env/scale/override) re-sync the cluster when a live deployment exists. CLI `edit` computes overrides client-side with `strategicpatch.CreateTwoWayMergePatch` against the base render.

**Tech Stack:** existing + `k8s.io/client-go` (dynamic client + fake for tests).

**Plan sequence (Phase 1):** A: core (merged) · B1: model + renderer (done, this branch) · **B2: this plan** · C: build pipeline + registry · D: web UI + `luncur up`.

## Global Constraints

- Everything from Plans A/B1 still binds (error envelope, `/v1/` routes, bearer auth, no CGO, roles admin|member).
- New dependency allowed by this plan, and only this: `k8s.io/client-go`.
- SSA everywhere: `types.ApplyPatchType`, `FieldManager: "luncur"`, `Force: true`. Never create/update verbs for app objects.
- GVRs are hardcoded (no discovery): Deployment `apps/v1/deployments`, Service `v1/services`, Ingress `networking.k8s.io/v1/ingresses`, Secret `v1/secrets`, Namespace `v1/namespaces`.
- Authz rule: `admin` role sees/does everything; `member` must have a `project_members` row for the project. Project creation and member management are admin-only.
- App URL host: `<app>.<dashed-ip>.sslip.io` where dashed-ip = external IP with dots→dashes.
- Kube-less mode: `server.New` accepts nil kube client; endpoints needing the cluster return 503 `{"error":{"code":"kubernetes_unavailable",...}}`. All other endpoints work (CI runs without a cluster).
- Deploy in B2 is synchronous and does NOT wait for rollout (status `live` = apply accepted). Rollout watching/log streaming is Plan C.
- Kube tests use `k8s.io/client-go/dynamic/fake` with reactors capturing actions — do not rely on the fake tracker emulating SSA semantics.

## File Structure

```
internal/kube/kube.go            Client: New, NewFromDynamic, EnsureNamespace, Apply, DeleteAppObjects
internal/store/members.go        AddMember, IsMember (+ GetUserByEmail)
internal/server/server.go        Deps struct, route table (modified)
internal/server/projects.go      project create/list, add member, authz helper
internal/server/apps.go          app create/list/get/delete, deploy, scale
internal/server/appenv.go        env get/put/delete, override put/delete, raw
internal/server/sync.go          renderApp, syncIfLive, host helpers
internal/cli/serve.go            new flags: --kubeconfig, --secret-key-file, --external-ip (modified)
internal/client/client.go        methods for every new route (modified)
internal/cli/project.go          project create/list/add-member
internal/cli/app.go              app create/list, status, destroy, scale, --raw
internal/cli/deploy.go           deploy command
internal/cli/env.go              env set/unset/list
internal/cli/edit.go             edit command + computeOverride (pure, tested)
```

---

### Task 1: Kube applier

**Files:**
- Create: `internal/kube/kube.go`
- Test: `internal/kube/kube_test.go`

**Interfaces:**
- Consumes: `render.Object` (`{Kind string; JSON []byte}`).
- Produces:
  - `kube.New(kubeconfig string) (*Client, error)` — kubeconfig path; `""` → in-cluster config (`rest.InClusterConfig`).
  - `kube.NewFromDynamic(dyn dynamic.Interface) *Client` — for tests and future fakes.
  - `(*Client) EnsureNamespace(ctx context.Context, name string) error` — SSA-applies a minimal Namespace object with label `app.kubernetes.io/managed-by: luncur`.
  - `(*Client) Apply(ctx context.Context, namespace string, objs []render.Object) error` — SSA-applies each object in order; name extracted from the object JSON's `metadata.name`.
  - `(*Client) DeleteAppObjects(ctx context.Context, namespace, app string) error` — deletes Deployment/Service/Ingress named `app` and Secret `app-env`; NotFound errors ignored.

- [ ] **Step 1: Get client-go**

```bash
go get k8s.io/client-go@latest && go mod tidy
```

- [ ] **Step 2: Write the failing test**

`internal/kube/kube_test.go`:

```go
package kube

import (
	"context"
	"encoding/json"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	ktesting "k8s.io/client-go/testing"
	dynamicfake "k8s.io/client-go/dynamic/fake"

	"github.com/sutantodadang/luncur/internal/render"
)

type recorded struct {
	verb      string
	resource  string
	namespace string
	name      string
	patchType string
}

// fakeClient returns a Client whose dynamic layer records every action.
func fakeClient(t *testing.T) (*Client, *[]recorded) {
	t.Helper()
	scheme := runtime.NewScheme()
	dyn := dynamicfake.NewSimpleDynamicClient(scheme)
	var log []recorded
	dyn.PrependReactor("*", "*", func(action ktesting.Action) (bool, runtime.Object, error) {
		rec := recorded{
			verb:      action.GetVerb(),
			resource:  action.GetResource().Resource,
			namespace: action.GetNamespace(),
		}
		switch a := action.(type) {
		case ktesting.PatchAction:
			rec.name = a.GetName()
			rec.patchType = string(a.GetPatchType())
		case ktesting.DeleteAction:
			rec.name = a.GetName()
		}
		log = append(log, rec)
		return true, nil, nil // short-circuit: we assert on actions, not state
	})
	return NewFromDynamic(dyn), &log
}

func renderedObjects(t *testing.T) []render.Object {
	t.Helper()
	r, err := render.Render(render.Input{
		AppName: "api", Namespace: "luncur-web",
		Image: "nginx", Host: "api.1-2-3-4.sslip.io", Port: 3000, Replicas: 1,
	}, map[string]string{"K": "v"})
	if err != nil {
		t.Fatal(err)
	}
	return r.Objects
}

func TestApplyUsesSSAForEveryObject(t *testing.T) {
	c, log := fakeClient(t)
	if err := c.Apply(context.Background(), "luncur-web", renderedObjects(t)); err != nil {
		t.Fatal(err)
	}
	if len(*log) != 4 {
		t.Fatalf("want 4 actions, got %d: %+v", len(*log), *log)
	}
	wantResources := []string{"secrets", "deployments", "services", "ingresses"}
	for i, rec := range *log {
		if rec.verb != "patch" || rec.patchType != "application/apply-patch+yaml" {
			t.Errorf("action %d: want SSA patch, got %+v", i, rec)
		}
		if rec.resource != wantResources[i] {
			t.Errorf("action %d: want %s, got %s", i, wantResources[i], rec.resource)
		}
		if rec.namespace != "luncur-web" {
			t.Errorf("action %d: namespace %s", i, rec.namespace)
		}
	}
	if (*log)[0].name != "api-env" || (*log)[1].name != "api" {
		t.Errorf("names: %+v", *log)
	}
}

func TestEnsureNamespace(t *testing.T) {
	c, log := fakeClient(t)
	if err := c.EnsureNamespace(context.Background(), "luncur-web"); err != nil {
		t.Fatal(err)
	}
	rec := (*log)[0]
	if rec.verb != "patch" || rec.resource != "namespaces" || rec.name != "luncur-web" {
		t.Fatalf("bad action: %+v", rec)
	}
}

func TestDeleteAppObjectsIgnoresNotFound(t *testing.T) {
	// Default reactor chain (no short-circuit): deleting non-existent
	// objects from the empty fake tracker returns NotFound, which
	// DeleteAppObjects must swallow.
	scheme := runtime.NewScheme()
	c := NewFromDynamic(dynamicfake.NewSimpleDynamicClient(scheme))
	if err := c.DeleteAppObjects(context.Background(), "luncur-web", "api"); err != nil {
		t.Fatalf("NotFound should be ignored: %v", err)
	}
}

func TestApplyRejectsObjectWithoutName(t *testing.T) {
	c, _ := fakeClient(t)
	bad := []render.Object{{Kind: "Service", JSON: json.RawMessage(`{"metadata":{}}`)}}
	if err := c.Apply(context.Background(), "ns", bad); err == nil {
		t.Fatal("want error for object without metadata.name")
	}
}
```

- [ ] **Step 3: Run test to verify it fails**

Run: `go test ./internal/kube/ -v`
Expected: FAIL — `undefined: NewFromDynamic`

- [ ] **Step 4: Write minimal implementation**

`internal/kube/kube.go`:

```go
// Package kube applies luncur-rendered manifests to the cluster with
// server-side apply (fieldManager=luncur), so user edits made through
// luncur's override system merge cleanly with cluster state.
package kube

import (
	"context"
	"encoding/json"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/sutantodadang/luncur/internal/render"
)

var gvrByKind = map[string]schema.GroupVersionResource{
	"Deployment": {Group: "apps", Version: "v1", Resource: "deployments"},
	"Service":    {Group: "", Version: "v1", Resource: "services"},
	"Ingress":    {Group: "networking.k8s.io", Version: "v1", Resource: "ingresses"},
	"Secret":     {Group: "", Version: "v1", Resource: "secrets"},
	"Namespace":  {Group: "", Version: "v1", Resource: "namespaces"},
}

type Client struct {
	dyn dynamic.Interface
}

// New builds a client from a kubeconfig path, or in-cluster config when
// path is empty.
func New(kubeconfig string) (*Client, error) {
	var cfg *rest.Config
	var err error
	if kubeconfig == "" {
		cfg, err = rest.InClusterConfig()
	} else {
		cfg, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
	}
	if err != nil {
		return nil, fmt.Errorf("kube config: %w", err)
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	return &Client{dyn: dyn}, nil
}

func NewFromDynamic(dyn dynamic.Interface) *Client { return &Client{dyn: dyn} }

func applyOpts() metav1.PatchOptions {
	force := true
	return metav1.PatchOptions{FieldManager: "luncur", Force: &force}
}

func nameOf(objJSON []byte) (string, error) {
	var m struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(objJSON, &m); err != nil {
		return "", err
	}
	if m.Metadata.Name == "" {
		return "", fmt.Errorf("object has no metadata.name")
	}
	return m.Metadata.Name, nil
}

func (c *Client) EnsureNamespace(ctx context.Context, name string) error {
	ns := fmt.Sprintf(
		`{"apiVersion":"v1","kind":"Namespace","metadata":{"name":%q,"labels":{"app.kubernetes.io/managed-by":"luncur"}}}`,
		name,
	)
	_, err := c.dyn.Resource(gvrByKind["Namespace"]).Patch(
		ctx, name, types.ApplyPatchType, []byte(ns), applyOpts(),
	)
	return err
}

func (c *Client) Apply(ctx context.Context, namespace string, objs []render.Object) error {
	for _, o := range objs {
		gvr, ok := gvrByKind[o.Kind]
		if !ok {
			return fmt.Errorf("no GVR for kind %q", o.Kind)
		}
		name, err := nameOf(o.JSON)
		if err != nil {
			return fmt.Errorf("%s: %w", o.Kind, err)
		}
		_, err = c.dyn.Resource(gvr).Namespace(namespace).Patch(
			ctx, name, types.ApplyPatchType, o.JSON, applyOpts(),
		)
		if err != nil {
			return fmt.Errorf("apply %s/%s: %w", o.Kind, name, err)
		}
	}
	return nil
}

// DeleteAppObjects removes everything Render produces for an app.
// NotFound is fine — destroy must be idempotent.
func (c *Client) DeleteAppObjects(ctx context.Context, namespace, app string) error {
	targets := []struct{ kind, name string }{
		{"Deployment", app},
		{"Service", app},
		{"Ingress", app},
		{"Secret", render.SecretName(app)},
	}
	for _, t := range targets {
		err := c.dyn.Resource(gvrByKind[t.kind]).Namespace(namespace).Delete(
			ctx, t.name, metav1.DeleteOptions{},
		)
		if err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete %s/%s: %w", t.kind, t.name, err)
		}
	}
	return nil
}
```

- [ ] **Step 5: Run test to verify it passes**

Run: `go test ./internal/kube/ -v` → PASS
Run: `go mod tidy && go test ./... && go vet ./...` → all pass

- [ ] **Step 6: Commit**

```bash
git add internal/kube go.mod go.sum
git commit -m "feat: kube applier with server-side apply and hardcoded GVRs"
```

---

### Task 2: Members store + server Deps + project routes

**Files:**
- Create: `internal/store/members.go`
- Modify: `internal/server/server.go` (Deps struct; keep old route registrations working)
- Create: `internal/server/projects.go`
- Modify: `internal/cli/serve.go` (build Deps: flags `--kubeconfig`, `--secret-key-file`, `--external-ip`)
- Modify: `internal/server/server_test.go` + `internal/server/users_test.go` + `internal/client/client_test.go` + `internal/cli/commands_test.go` (adapt to `New(Deps)`)
- Test: `internal/server/projects_test.go`, members test appended to `internal/store/env_test.go` file's package via new `internal/store/members_test.go`

**Interfaces:**
- Produces (store):
  - `(*Store) GetUserByEmail(email string) (User, error)` — ErrNotFound.
  - `(*Store) AddMember(projectID, userID int64) error` — idempotent (INSERT OR IGNORE, role 'member').
  - `(*Store) IsMember(projectID, userID int64) (bool, error)`
- Produces (server):
  - `type Deps struct { Store *store.Store; Sealer *secret.Sealer; Kube *kube.Client; ExternalIP string }`
  - `server.New(d Deps) http.Handler` — SIGNATURE CHANGE; ExternalIP defaults to `"127.0.0.1"` when empty.
  - `(*server) requireProject(w, r, u, name) (store.Project, bool)` — loads project; 404 `not_found` if missing; 403 `forbidden` if `u.Role != "admin"` and not a member. Returns ok=false after writing the error.
  - `(*server) requireKube(w) bool` — writes 503 `kubernetes_unavailable` and returns false when Deps.Kube is nil.
  - Routes: `POST /v1/projects` (adminOnly, body `{"name"}`, 201) · `GET /v1/projects` (authed; admin → all, member → only theirs) · `POST /v1/projects/{project}/members` (adminOnly, body `{"email"}`, 204; 404 when user email or project unknown).
- serve.go: sealer from `--secret-key-file` (default `luncur.key` beside the DB); kube from `--kubeconfig` ("" tries in-cluster, on error log warning and continue with nil kube).

Test code (write first, RED, then implement):

`internal/store/members_test.go`:

```go
package store

import "testing"

func TestMembers(t *testing.T) {
	s := openTest(t)
	p := seedProject(t, s)
	u, err := s.CreateUser("m@b.co", "pw123456", "member")
	if err != nil {
		t.Fatal(err)
	}

	if ok, _ := s.IsMember(p.ID, u.ID); ok {
		t.Fatal("not a member yet")
	}
	if err := s.AddMember(p.ID, u.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.AddMember(p.ID, u.ID); err != nil {
		t.Fatal("second add must be idempotent")
	}
	if ok, _ := s.IsMember(p.ID, u.ID); !ok {
		t.Fatal("want member")
	}

	got, err := s.GetUserByEmail("m@b.co")
	if err != nil || got.ID != u.ID {
		t.Fatalf("GetUserByEmail: %+v %v", got, err)
	}
}
```

`internal/server/projects_test.go`:

```go
package server

import (
	"encoding/json"
	"testing"
)

func TestProjectRoutes(t *testing.T) {
	srv, st := testServer(t)
	admin := seedUserToken(t, st, "root@b.co", "admin")
	member := seedUserToken(t, st, "m@b.co", "member")

	// Create: admin only.
	if resp := doAuthed(t, "POST", srv.URL+"/v1/projects", member, `{"name":"web"}`); resp.StatusCode != 403 {
		t.Fatalf("member create: want 403, got %d", resp.StatusCode)
	}
	if resp := doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"web"}`); resp.StatusCode != 201 {
		t.Fatalf("admin create: want 201, got %d", resp.StatusCode)
	}
	if resp := doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"BAD NAME"}`); resp.StatusCode != 400 {
		t.Fatalf("bad name: want 400, got %d", resp.StatusCode)
	}

	// List: member sees nothing until added.
	var list []map[string]any
	resp := doAuthed(t, "GET", srv.URL+"/v1/projects", member, "")
	json.NewDecoder(resp.Body).Decode(&list)
	resp.Body.Close()
	if len(list) != 0 {
		t.Fatalf("member list before membership: %v", list)
	}

	if resp := doAuthed(t, "POST", srv.URL+"/v1/projects/web/members", admin, `{"email":"m@b.co"}`); resp.StatusCode != 204 {
		t.Fatalf("add member: want 204, got %d", resp.StatusCode)
	}
	if resp := doAuthed(t, "POST", srv.URL+"/v1/projects/web/members", admin, `{"email":"ghost@b.co"}`); resp.StatusCode != 404 {
		t.Fatalf("unknown email: want 404, got %d", resp.StatusCode)
	}

	resp = doAuthed(t, "GET", srv.URL+"/v1/projects", member, "")
	list = nil
	json.NewDecoder(resp.Body).Decode(&list)
	resp.Body.Close()
	if len(list) != 1 || list[0]["name"] != "web" {
		t.Fatalf("member list after membership: %v", list)
	}
}
```

Implementation notes (write the code these tests demand):
- `testServer` helper changes to `httptest.NewServer(New(Deps{Store: st}))` — nil Sealer/Kube fine for existing tests.
- `ListProjects` filtering: for members, add store method inline in projects.go handler via SQL join:

```go
// in internal/store/members.go
func (s *Store) ListProjectsFor(userID int64) ([]Project, error) {
	rows, err := s.db.Query(
		`SELECT p.id, p.name, p.k8s_namespace FROM projects p
		 JOIN project_members m ON m.project_id = p.id WHERE m.user_id = ?
		 ORDER BY p.name`, userID)
	// scan loop identical to ListProjects
}
```

- `internal/server/projects.go` handlers:

```go
func (s *server) handleCreateProject(w http.ResponseWriter, r *http.Request, _ store.User) {
	var req struct{ Name string `json:"name"` }
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	p, err := s.st.CreateProject(req.Name)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, projectJSON(p))
}
```

(plus handleListProjects switching on role, handleAddMember doing GetProject + GetUserByEmail + AddMember, projectJSON helper `{"id","name","namespace"}`), and `requireProject`:

```go
func (s *server) requireProject(w http.ResponseWriter, u store.User, name string) (store.Project, bool) {
	p, err := s.st.GetProject(name)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "no such project")
		return store.Project{}, false
	}
	if err != nil {
		log.Printf("get project: %v", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return store.Project{}, false
	}
	if u.Role != "admin" {
		ok, err := s.st.IsMember(p.ID, u.ID)
		if err != nil || !ok {
			writeError(w, http.StatusForbidden, "forbidden", "not a member of this project")
			return store.Project{}, false
		}
	}
	return p, true
}
```

- serve.go: open sealer `secret.LoadOrCreate(secretKeyFile)`; kube: `kube.New(kubeconfig)`; on error `log.Printf("warning: kubernetes unavailable: %v", err)` and pass nil. Pass `--external-ip` through.

Steps: write tests → RED → implement → `go test ./...` (all packages — the New() signature change touches client and cli tests; fix those call sites) → `go vet` → commit `feat: project routes, membership authz, server Deps wiring`.

---

### Task 3: Sync helpers + app routes (create/list/get/delete/deploy/scale)

**Files:**
- Create: `internal/server/sync.go`
- Create: `internal/server/apps.go`
- Modify: `internal/server/server.go` (register routes)
- Test: `internal/server/apps_test.go`

**Interfaces:**
- Consumes: Task 1 kube.Client, Task 2 requireProject/requireKube, B1 store + render.
- Produces (server internals):
  - `func hostFor(app, externalIP string) string` — `app + "." + strings.ReplaceAll(ip, ".", "-") + ".sslip.io"`.
  - `(*server) renderApp(p store.Project, a store.App, imageRef string) (render.Rendered, error)` — unseals env via Deps.Sealer (nil sealer + non-empty env = error), loads overrides, builds render.Input.
  - `(*server) syncApp(ctx, p, a) error` — LatestDeployment; if status `live`, re-render with its ImageRef and EnsureNamespace+Apply. No live deployment → no-op nil.
- Routes:
  - `POST /v1/projects/{project}/apps` (authed+requireProject) body `{"name","port"}` → 201 `{"id","name","port","replicas","url"}`.
  - `GET /v1/projects/{project}/apps` → list of the same shape.
  - `GET /v1/projects/{project}/apps/{app}` → app + `"status"` (latest deployment status or `"never_deployed"`) + `"image"`.
  - `DELETE /v1/projects/{project}/apps/{app}` → requireKube; kube.DeleteAppObjects then store.DeleteApp → 204.
  - `POST /v1/projects/{project}/apps/{app}/deploy` body `{"image"}` → requireKube; CreateDeployment(deploying) → EnsureNamespace+renderApp(image)+Apply → SetDeploymentStatus(live) → 200 `{"deployment_id","status":"live","url"}`; on apply error → SetDeploymentStatus(failed), 502 `deploy_failed` with the apply error message.
  - `POST /v1/projects/{project}/apps/{app}/scale` body `{"replicas"}` → SetReplicas + syncApp (requireKube only if a live deployment exists — implement: SetReplicas, then if LatestDeployment live: requireKube + syncApp) → 200 `{"replicas"}`.

Test code (RED first) — `internal/server/apps_test.go`:

```go
package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	ktesting "k8s.io/client-go/testing"
	dynamicfake "k8s.io/client-go/dynamic/fake"

	"github.com/sutantodadang/luncur/internal/kube"
	"github.com/sutantodadang/luncur/internal/secret"
	"github.com/sutantodadang/luncur/internal/store"
)

// kubeServer builds a test server with a fake kube layer that records actions.
func kubeServer(t *testing.T) (*httptestServer, *store.Store, *[]string) {
	t.Helper()
	st := newTestStore(t)
	scheme := runtime.NewScheme()
	dyn := dynamicfake.NewSimpleDynamicClient(scheme)
	var actions []string
	dyn.PrependReactor("*", "*", func(a ktesting.Action) (bool, runtime.Object, error) {
		actions = append(actions, a.GetVerb()+" "+a.GetResource().Resource)
		return true, nil, nil
	})
	sealer, err := secret.New(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	srv := newHTTPTest(t, Deps{Store: st, Kube: kube.NewFromDynamic(dyn), Sealer: sealer, ExternalIP: "1.2.3.4"})
	return srv, st, &actions
}
```

NOTE to implementer: `testServer` from Plan A returns `(*httptest.Server, *store.Store)`. Refactor minimally: extract `newTestStore(t)` and `newHTTPTest(t, Deps)` helpers in server_test.go, keep `testServer` as a thin wrapper (`newHTTPTest(t, Deps{Store: newTestStore(t)})`) so existing tests stay untouched. `httptestServer` above is whatever type `newHTTPTest` returns (`*httptest.Server`).

```go
func TestAppLifecycle(t *testing.T) {
	srv, st, actions := kubeServer(t)
	admin := seedUserToken(t, st, "root@b.co", "admin")
	doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"web"}`).Body.Close()

	// Create app.
	resp := doAuthed(t, "POST", srv.URL+"/v1/projects/web/apps", admin, `{"name":"api","port":3000}`)
	if resp.StatusCode != 201 {
		t.Fatalf("create app: want 201, got %d", resp.StatusCode)
	}
	var app map[string]any
	json.NewDecoder(resp.Body).Decode(&app)
	resp.Body.Close()
	if app["url"] != "http://api.1-2-3-4.sslip.io" {
		t.Fatalf("url: %v", app["url"])
	}

	// Status before deploy.
	resp = doAuthed(t, "GET", srv.URL+"/v1/projects/web/apps/api", admin, "")
	var got map[string]any
	json.NewDecoder(resp.Body).Decode(&got)
	resp.Body.Close()
	if got["status"] != "never_deployed" {
		t.Fatalf("status: %v", got["status"])
	}

	// Deploy.
	resp = doAuthed(t, "POST", srv.URL+"/v1/projects/web/apps/api/deploy", admin, `{"image":"nginx:1"}`)
	if resp.StatusCode != 200 {
		t.Fatalf("deploy: want 200, got %d", resp.StatusCode)
	}
	joined := strings.Join(*actions, ",")
	if !strings.Contains(joined, "patch namespaces") || !strings.Contains(joined, "patch deployments") {
		t.Fatalf("kube actions missing: %s", joined)
	}
	d, err := st.LatestDeployment(appID(t, st, "web", "api"))
	if err != nil || d.Status != "live" || d.ImageRef != "nginx:1" {
		t.Fatalf("deployment row: %+v %v", d, err)
	}

	// Scale re-applies (live deployment exists).
	before := len(*actions)
	resp = doAuthed(t, "POST", srv.URL+"/v1/projects/web/apps/api/scale", admin, `{"replicas":3}`)
	if resp.StatusCode != 200 {
		t.Fatalf("scale: want 200, got %d", resp.StatusCode)
	}
	if len(*actions) <= before {
		t.Fatal("scale should re-apply to cluster")
	}

	// Destroy.
	resp = doAuthed(t, "DELETE", srv.URL+"/v1/projects/web/apps/api", admin, "")
	if resp.StatusCode != 204 {
		t.Fatalf("destroy: want 204, got %d", resp.StatusCode)
	}
	joined = strings.Join(*actions, ",")
	if !strings.Contains(joined, "delete deployments") {
		t.Fatalf("no delete actions: %s", joined)
	}
}

func appID(t *testing.T, st *store.Store, project, app string) int64 {
	t.Helper()
	p, err := st.GetProject(project)
	if err != nil {
		t.Fatal(err)
	}
	a, err := st.GetApp(p.ID, app)
	if err != nil {
		t.Fatal(err)
	}
	return a.ID
}

func TestDeployWithoutKube503(t *testing.T) {
	srv, st := testServer(t) // no kube
	admin := seedUserToken(t, st, "root@b.co", "admin")
	doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"web"}`).Body.Close()
	doAuthed(t, "POST", srv.URL+"/v1/projects/web/apps", admin, `{"name":"api","port":3000}`).Body.Close()
	resp := doAuthed(t, "POST", srv.URL+"/v1/projects/web/apps/api/deploy", admin, `{"image":"x"}`)
	defer resp.Body.Close()
	if resp.StatusCode != 503 {
		t.Fatalf("want 503 without kube, got %d", resp.StatusCode)
	}
}

func TestMemberForbiddenOnForeignProject(t *testing.T) {
	srv, st, _ := kubeServer(t)
	admin := seedUserToken(t, st, "root@b.co", "admin")
	member := seedUserToken(t, st, "m@b.co", "member")
	doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"web"}`).Body.Close()
	resp := doAuthed(t, "GET", srv.URL+"/v1/projects/web/apps", member, "")
	defer resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatalf("want 403, got %d", resp.StatusCode)
	}
}
```

Implementation: handlers follow the established writeError/writeJSON patterns; `hostFor` in sync.go; deploy handler sequence per interface block above. Route registration uses Go 1.22 path params (`r.PathValue("project")`, `r.PathValue("app")`).

Steps: tests → RED → implement sync.go + apps.go + routes → `go test ./... ` + vet → commit `feat: app lifecycle API (create, deploy, scale, destroy)`.

---

### Task 4: Env, override, raw routes

**Files:**
- Create: `internal/server/appenv.go`
- Modify: `internal/server/server.go` (register routes)
- Test: `internal/server/appenv_test.go`

**Interfaces:**
- Routes (all under requireProject; mutations call `syncApp` after the store write — sync errors are logged, not fatal to the request, EXCEPT when requireKube already failed for ops that only make sense with a cluster: env/override mutations do NOT require kube; they sync opportunistically when kube present and latest deployment live):
  - `GET .../env` → 200 `{"KEY":"value",...}` (unsealed plaintext).
  - `PUT .../env` body `{"key","value"}` → 204. Seals with Deps.Sealer (nil sealer → 503 `sealer_unavailable`).
  - `DELETE .../env/{key}` → 204; 404 when key absent.
  - `PUT .../overrides/{kind}` body = raw strategic-merge-patch JSON (the request body IS the patch) → 204; 400 on invalid kind/patch (store validates).
  - `DELETE .../overrides/{kind}` → 204; 404 when absent.
  - `GET .../raw` → 200 `text/yaml`, multi-doc YAML of the CURRENT render (latest image; `never_deployed` → use image `"<pending-first-deploy>"` placeholder so raw works pre-deploy). Query `?base=1` renders WITHOUT overrides (CLI edit uses this as diff base).

Test code (RED first) — `internal/server/appenv_test.go`:

```go
package server

import (
	"encoding/json"
	"io"
	"strings"
	"testing"
)

func seedWebApi(t *testing.T, srv *httptestServer, admin string) {
	t.Helper()
	doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"web"}`).Body.Close()
	doAuthed(t, "POST", srv.URL+"/v1/projects/web/apps", admin, `{"name":"api","port":3000}`).Body.Close()
}

func TestEnvRoundTrip(t *testing.T) {
	srv, st, _ := kubeServer(t)
	admin := seedUserToken(t, st, "root@b.co", "admin")
	seedWebApi(t, srv, admin)

	if resp := doAuthed(t, "PUT", srv.URL+"/v1/projects/web/apps/api/env", admin, `{"key":"DB_URL","value":"postgres://x"}`); resp.StatusCode != 204 {
		t.Fatalf("put env: want 204, got %d", resp.StatusCode)
	}
	resp := doAuthed(t, "GET", srv.URL+"/v1/projects/web/apps/api/env", admin, "")
	var env map[string]string
	json.NewDecoder(resp.Body).Decode(&env)
	resp.Body.Close()
	if env["DB_URL"] != "postgres://x" {
		t.Fatalf("env: %v", env)
	}
	// Sealed at rest: raw store bytes must not contain plaintext.
	var raw []byte
	st.DB().QueryRow(`SELECT value_enc FROM env_vars LIMIT 1`).Scan(&raw)
	if strings.Contains(string(raw), "postgres") {
		t.Fatal("env value stored unsealed")
	}
	if resp := doAuthed(t, "DELETE", srv.URL+"/v1/projects/web/apps/api/env/DB_URL", admin, ""); resp.StatusCode != 204 {
		t.Fatalf("delete env: want 204, got %d", resp.StatusCode)
	}
	if resp := doAuthed(t, "DELETE", srv.URL+"/v1/projects/web/apps/api/env/DB_URL", admin, ""); resp.StatusCode != 404 {
		t.Fatalf("second delete: want 404, got %d", resp.StatusCode)
	}
}

func TestOverrideAndRaw(t *testing.T) {
	srv, st, _ := kubeServer(t)
	admin := seedUserToken(t, st, "root@b.co", "admin")
	seedWebApi(t, srv, admin)

	patch := `{"metadata":{"labels":{"team":"x"}}}`
	if resp := doAuthed(t, "PUT", srv.URL+"/v1/projects/web/apps/api/overrides/Deployment", admin, patch); resp.StatusCode != 204 {
		t.Fatalf("put override: want 204, got %d", resp.StatusCode)
	}
	if resp := doAuthed(t, "PUT", srv.URL+"/v1/projects/web/apps/api/overrides/Pod", admin, `{}`); resp.StatusCode != 400 {
		t.Fatalf("bad kind: want 400, got %d", resp.StatusCode)
	}

	resp := doAuthed(t, "GET", srv.URL+"/v1/projects/web/apps/api/raw", admin, "")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "yaml") {
		t.Fatalf("content type: %s", ct)
	}
	if !strings.Contains(string(body), "team: x") {
		t.Fatalf("override missing from raw:\n%s", body)
	}

	respBase := doAuthed(t, "GET", srv.URL+"/v1/projects/web/apps/api/raw?base=1", admin, "")
	baseBody, _ := io.ReadAll(respBase.Body)
	respBase.Body.Close()
	if strings.Contains(string(baseBody), "team: x") {
		t.Fatal("base render must exclude overrides")
	}
}
```

Implementation: appenv.go handlers reuse `requireProject` + `GetApp`; raw handler builds `renderApp` variants (add a `withOverrides bool` parameter to renderApp or an internal helper). Commit `feat: env, override, and raw manifest API`.

---

### Task 5: Client methods for the new API

**Files:**
- Modify: `internal/client/client.go`
- Test: append to `internal/client/client_test.go`

**Interfaces (all decode the standard envelope on error, like existing methods):**

```go
type ProjectInfo struct { ID int64 `json:"id"`; Name string `json:"name"`; Namespace string `json:"namespace"` }
type AppInfo struct {
	ID int64 `json:"id"`; Name string `json:"name"`; Port int `json:"port"`
	Replicas int `json:"replicas"`; URL string `json:"url"`
	Status string `json:"status,omitempty"`; Image string `json:"image,omitempty"`
}
type DeployResult struct { DeploymentID int64 `json:"deployment_id"`; Status string `json:"status"`; URL string `json:"url"` }

func (c *Client) CreateProject(name string) (ProjectInfo, error)
func (c *Client) ListProjects() ([]ProjectInfo, error)
func (c *Client) AddMember(project, email string) error
func (c *Client) CreateApp(project, name string, port int) (AppInfo, error)
func (c *Client) ListApps(project string) ([]AppInfo, error)
func (c *Client) GetApp(project, app string) (AppInfo, error)
func (c *Client) DeleteApp(project, app string) error
func (c *Client) Deploy(project, app, image string) (DeployResult, error)
func (c *Client) Scale(project, app string, replicas int) error
func (c *Client) EnvSet(project, app, key, value string) error
func (c *Client) EnvUnset(project, app, key string) error
func (c *Client) EnvList(project, app string) (map[string]string, error)
func (c *Client) Raw(project, app string, base bool) ([]byte, error)      // GET .../raw[?base=1], returns raw YAML bytes
func (c *Client) PutOverride(project, app, kind, patchJSON string) error  // body = raw patch
func (c *Client) DeleteOverride(project, app, kind string) error
```

`Raw` and `PutOverride` don't fit the JSON `do()` helper: add a `doRaw(method, path string, body []byte) ([]byte, error)` helper that sends/receives raw bytes but still decodes the JSON error envelope on non-2xx.

Test (RED first, append to client_test.go): spin the standard `testAPI` server but constructed with a fake-kube Deps (import the same dynamicfake pattern — or simpler: exercise only non-kube paths: CreateProject → CreateApp → EnvSet → EnvList → Raw(base) → PutOverride → Raw(with override contains label) → DeleteApp expecting 503 since no kube). Assert:

```go
func TestClientProjectAppEnvRawFlow(t *testing.T) {
	srv, st := testAPI(t) // existing helper, no kube
	st.CreateUser("root@b.co", "pw123456", "admin")
	c := New(srv.URL, "")
	tok, _ := c.Login("root@b.co", "pw123456")
	c = New(srv.URL, tok)

	if _, err := c.CreateProject("web"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.CreateApp("web", "api", 3000); err != nil {
		t.Fatal(err)
	}
	if err := c.EnvSet("web", "api", "K", "v"); err != nil {
		t.Fatal(err)
	}
	env, err := c.EnvList("web", "api")
	if err != nil || env["K"] != "v" {
		t.Fatalf("env: %v %v", env, err)
	}
	if err := c.PutOverride("web", "api", "Deployment", `{"metadata":{"labels":{"t":"x"}}}`); err != nil {
		t.Fatal(err)
	}
	y, err := c.Raw("web", "api", false)
	if err != nil || !strings.Contains(string(y), "t: x") {
		t.Fatalf("raw: %v\n%s", err, y)
	}
	if err := c.DeleteApp("web", "api"); err == nil || !strings.Contains(err.Error(), "kubernetes_unavailable") {
		t.Fatalf("want kubernetes_unavailable, got %v", err)
	}
}
```

NOTE: testAPI's server must construct `New(Deps{Store: st, Sealer: <32-zero-byte sealer>})` — update the helper (env routes need a sealer).

Commit `feat: API client methods for projects, apps, env, overrides`.

---

### Task 6: CLI — project, app, deploy, status, env, scale, destroy

**Files:**
- Create: `internal/cli/project.go`, `internal/cli/app.go`, `internal/cli/deploy.go`, `internal/cli/env.go`
- Modify: `internal/cli/root.go` (register)
- Test: `internal/cli/b2_commands_test.go`

**Command surface (all use `apiClient()` from Plan A):**

```
luncur project create <name>
luncur project list
luncur project add-member <project> <email>
luncur app create <name> --project <p> --port <n>       (port required)
luncur app list --project <p>
luncur app info <name> --project <p>                    (status+url+image; "app info" not "status" — cobra subcommand)
luncur deploy <app> --project <p> --image <ref>
luncur scale <app> --project <p> --replicas <n>
luncur destroy <app> --project <p>
luncur env set <app> KEY=VALUE --project <p>
luncur env unset <app> KEY --project <p>
luncur env list <app> --project <p>
```

Output style: one line per entity, tab-separated key facts (match `whoami`'s plain style). `deploy` prints `deployed <app> → <url> (deployment <id>)`.

Test (RED first) — same in-process pattern as Plan A's commands_test.go (testEnv helper boots a real server; extend it to include a sealer in Deps; kube stays nil so deploy/destroy assert the 503 error surfaces as a readable CLI error):

```go
func TestProjectAppEnvCommands(t *testing.T) {
	srv := testEnv(t) // existing helper from Plan A tests (login as root@b.co happens via `run` login below)
	if _, err := run(t, "login", srv.URL, "--email", "root@b.co", "--password", "pw123456"); err != nil {
		t.Fatal(err)
	}
	if out, err := run(t, "project", "create", "web"); err != nil || !strings.Contains(out, "web") {
		t.Fatalf("project create: %v %q", err, out)
	}
	if out, err := run(t, "app", "create", "api", "--project", "web", "--port", "3000"); err != nil || !strings.Contains(out, "api") {
		t.Fatalf("app create: %v %q", err, out)
	}
	if _, err := run(t, "env", "set", "api", "K=v", "--project", "web"); err != nil {
		t.Fatal(err)
	}
	if out, _ := run(t, "env", "list", "api", "--project", "web"); !strings.Contains(out, "K=v") {
		t.Fatalf("env list: %q", out)
	}
	if out, _ := run(t, "app", "list", "--project", "web"); !strings.Contains(out, "api") {
		t.Fatalf("app list: %q", out)
	}
	// No kube in test server: deploy must surface the 503 cleanly.
	if _, err := run(t, "deploy", "api", "--project", "web", "--image", "nginx:1"); err == nil || !strings.Contains(err.Error(), "kubernetes_unavailable") {
		t.Fatalf("deploy: want kubernetes_unavailable error, got %v", err)
	}
}
```

Commit `feat: project, app, deploy, env, scale, destroy CLI commands`.

---

### Task 7: CLI — raw + edit

**Files:**
- Create: `internal/cli/edit.go` (also hosts the `app raw` wiring into app.go's command or its own `raw` subcommand under app)
- Modify: `internal/cli/app.go` (add `--raw` flag handling on `app info`? No — add `luncur app raw <name> --project <p>`; spec's `luncur app <name> --raw` maps to this)
- Test: `internal/cli/edit_test.go`

**Interfaces:**
- `luncur app raw <name> --project <p>` → prints the multi-doc YAML from `client.Raw(project, app, false)`.
- `luncur edit <app> <kind> --project <p>`:
  1. `base := client.Raw(project, app, true)`, `current := client.Raw(project, app, false)` — extract the single document whose `kind:` matches (helper `extractDoc(yamlMulti []byte, kind string) ([]byte, error)`).
  2. Write current doc to temp file, launch `$EDITOR` (error `"$EDITOR not set"` if unset), read back.
  3. `patch, err := computeOverride(kind, baseDoc, editedDoc)` — pure function: yaml→json both sides (`sigs.k8s.io/yaml.YAMLToJSON`), then `strategicpatch.CreateTwoWayMergePatch(baseJSON, editedJSON, dataStruct)` with the typed zero value (same switch as render's dataStructFor — duplicate the 3-case switch locally in cli; do NOT export from render for this).
  4. Patch `"{}"` → print "no changes" and skip. Otherwise `client.PutOverride(...)` then print `override saved; takes effect on next deploy (or immediately if app is live)`.
- `computeOverride(kind string, baseYAML, editedYAML []byte) (string, error)` and `extractDoc` are pure and unit-tested; the interactive editor path is NOT tested (same policy as the login TTY prompt).

Test (RED first) — `internal/cli/edit_test.go`:

```go
package cli

import (
	"strings"
	"testing"
)

const twoDocYAML = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: api
spec:
  replicas: 1
---
apiVersion: v1
kind: Service
metadata:
  name: api
`

func TestExtractDoc(t *testing.T) {
	doc, err := extractDoc([]byte(twoDocYAML), "Service")
	if err != nil || !strings.Contains(string(doc), "kind: Service") {
		t.Fatalf("extract: %v\n%s", err, doc)
	}
	if _, err := extractDoc([]byte(twoDocYAML), "Ingress"); err == nil {
		t.Fatal("want error for missing kind")
	}
}

func TestComputeOverride(t *testing.T) {
	base := `apiVersion: apps/v1
kind: Deployment
metadata:
  name: api
spec:
  replicas: 1
`
	edited := `apiVersion: apps/v1
kind: Deployment
metadata:
  name: api
  labels:
    team: x
spec:
  replicas: 1
`
	patch, err := computeOverride("Deployment", []byte(base), []byte(edited))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(patch, `"team":"x"`) {
		t.Fatalf("patch: %s", patch)
	}
	// No edit → empty patch.
	same, err := computeOverride("Deployment", []byte(base), []byte(base))
	if err != nil {
		t.Fatal(err)
	}
	if same != "{}" {
		t.Fatalf("want {} for no changes, got %s", same)
	}
}
```

Commit `feat: app raw and edit commands with client-side override diff`.

---

### Task 8: README update + full-suite gate

**Files:**
- Modify: `README.md`

- [ ] Update README "Working today" section with the B1+B2 surface (project/app/env/deploy/scale/destroy/raw/edit; note deploy needs a K3s cluster + `--kubeconfig`, and `--external-ip` controls sslip.io hosts). Keep it honest — no build pipeline yet (that's Plan C).
- [ ] Run the full gate: `go test ./...`, `go vet ./...`, PowerShell `$env:CGO_ENABLED='0'; go build ./...` — all clean.
- [ ] Commit `docs: README for app lifecycle surface`.
