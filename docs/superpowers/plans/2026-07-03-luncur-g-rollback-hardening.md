# luncur Plan G — rollback + hardening Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Undo bad deploys (`luncur rollback` + UI button), manage API tokens (`luncur token list/revoke`), drop cluster-admin for a scoped ClusterRole, and add CSRF tokens to every web-UI form.

**Architecture:** Rollback creates a new deployment row (`rolled_back_from` column) pointing at a previous deployment's image and re-runs the existing render+apply path — no build; the embedded registry is HEAD-checked first. Token lifecycle is a thin store+API+CLI layer over the existing `api_tokens` table. The scoped ClusterRole replaces the `cluster-admin` RoleRef in luncur's self-deploy manifests with an enumerated rule set (golden-tested). CSRF uses the double-submit-cookie pattern: a random `luncur_csrf` cookie mirrored in a `_csrf` hidden field, verified centrally in `uiPage` for every POST.

**Tech Stack:** Go stdlib (`crypto/rand`, `crypto/subtle`, `net/http`), client-go (existing), modernc.org/sqlite, cobra.

## Global Constraints

- Single Go module, one binary from `cmd/luncur`. No new dependencies.
- Server-side apply everywhere, `fieldManager=luncur`. API error envelope `{"error":{"code":"...","message":"..."}}` via `writeError`.
- All commits conventional style; `go build ./... && go vet ./... && go test ./...` before every commit.
- Tests must not require a cluster or network: fake clientsets, `httptest` (the registry HEAD check is tested against an `httptest` server standing in for the embedded registry).
- **Approved deviations from the Phase 2 spec (record in README):**
  - CSRF is the stateless double-submit-cookie pattern (random cookie + matching hidden field) rather than a server-stored per-session token — same protection for this threat model (forms, no JS API), zero schema.
  - Rollback registry check only applies to images hosted in the embedded registry (`ref` prefixed with the configured registry host); external image refs (e.g. `docker.io/...`) are assumed present since luncur has no credentials to check them.

---

### Task 1: store — rollback lineage + token listing/revocation

**Files:**
- Modify: `internal/store/store.go` (migrate: `deployments.rolled_back_from`)
- Modify: `internal/store/deployments.go`
- Modify: `internal/store/tokens.go`
- Test: `internal/store/deployments_test.go`, `internal/store/tokens_test.go` (append)

**Interfaces:**
- Consumes: existing `migrate()` column loop (Plan F restructured it — add one row), `Deployment` struct, `openTest(t)`.
- Produces:
  - `Deployment` gains `RolledBackFrom int64` (0 = not a rollback; column is nullable INTEGER).
  - `Store.CreateRollbackDeployment(appID int64, imageRef string, createdBy, rolledBackFrom int64) (Deployment, error)` — inserts status `deploying` with `rolled_back_from` set.
  - `GetDeployment` / `LatestDeployment` / `ListDeployments` scan the new column (NULL → 0).
  - `type TokenInfo struct { ID int64; Name, CreatedAt, LastUsedAt, ExpiresAt string }` (nullable strings → "").
  - `Store.ListTokens(userID int64) ([]TokenInfo, error)` — newest first.
  - `Store.RevokeToken(userID, id int64) error` — `ErrNotFound` when the row doesn't exist or belongs to another user.

- [ ] **Step 1: Failing tests.**

Append to `internal/store/deployments_test.go`:

```go
func TestRollbackDeployment(t *testing.T) {
	st := openTest(t)
	p, _ := st.CreateProject("proj")
	a, _ := st.CreateApp(p.ID, "web", 8080)
	d1, err := st.CreateDeployment(a.ID, "live", "img:1", 0)
	if err != nil {
		t.Fatal(err)
	}
	rb, err := st.CreateRollbackDeployment(a.ID, "img:1", 7, d1.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rb.Status != "deploying" || rb.ImageRef != "img:1" || rb.RolledBackFrom != d1.ID {
		t.Fatalf("rollback row = %+v", rb)
	}
	got, err := st.GetDeployment(rb.ID)
	if err != nil || got.RolledBackFrom != d1.ID {
		t.Fatalf("get: %+v err=%v", got, err)
	}
	// Non-rollback rows read back 0.
	got, err = st.GetDeployment(d1.ID)
	if err != nil || got.RolledBackFrom != 0 {
		t.Fatalf("plain row rolled_back_from = %d err=%v", got.RolledBackFrom, err)
	}
}
```

Append to `internal/store/tokens_test.go`:

```go
func TestListAndRevokeTokens(t *testing.T) {
	st := openTest(t)
	u, _ := st.CreateUser("tok2@example.com", "password123", "member")
	other, _ := st.CreateUser("other@example.com", "password123", "member")
	if _, err := st.CreateToken(u.ID, "laptop"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateToken(u.ID, "ci"); err != nil {
		t.Fatal(err)
	}
	list, err := st.ListTokens(u.ID)
	if err != nil || len(list) != 2 {
		t.Fatalf("list = %+v err=%v", list, err)
	}
	if list[0].Name != "ci" { // newest first
		t.Fatalf("order: %+v", list)
	}
	if list[0].ExpiresAt == "" {
		t.Fatal("expires_at missing")
	}
	// Foreign revoke → ErrNotFound; own revoke works and kills auth.
	if err := st.RevokeToken(other.ID, list[0].ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign revoke: %v", err)
	}
	if err := st.RevokeToken(u.ID, list[0].ID); err != nil {
		t.Fatal(err)
	}
	if l, _ := st.ListTokens(u.ID); len(l) != 1 {
		t.Fatalf("after revoke: %+v", l)
	}
}
```

- [ ] **Step 2: Run** `go test ./internal/store/ -run 'TestRollbackDeployment|TestListAndRevokeTokens' -v` — compile failure.

- [ ] **Step 3: Implement.**

`store.go` migrate loop — add:

```go
	{"deployments", "rolled_back_from", `ALTER TABLE deployments ADD COLUMN rolled_back_from INTEGER`},
```

`deployments.go`:
- `Deployment` struct: add `RolledBackFrom int64`.
- Every SELECT gains `rolled_back_from`; scan via `sql.NullInt64` and assign `.Int64` only when `err == nil && valid` (follow the file's existing NullString handling for image_ref/log_path).
- New method:

```go
// CreateRollbackDeployment records a redeploy of an earlier deployment's
// image: status starts at "deploying" (no build phase) and rolled_back_from
// preserves the lineage for history displays.
func (s *Store) CreateRollbackDeployment(appID int64, imageRef string, createdBy, rolledBackFrom int64) (Deployment, error) {
	res, err := s.db.Exec(
		`INSERT INTO deployments (app_id, status, image_ref, created_by, rolled_back_from)
		 VALUES (?, 'deploying', ?, ?, ?)`,
		appID, imageRef, nullableID(createdBy), rolledBackFrom)
	if err != nil {
		return Deployment{}, err
	}
	id, _ := res.LastInsertId()
	return s.GetDeployment(id)
}
```

where `nullableID` mirrors however `CreateDeployment` currently handles `createdBy == 0` → NULL (read it first; if it inserts 0 directly, do the same — consistency over cleverness, and drop the helper).

`tokens.go`:

```go
// TokenInfo is the listing view of an API token — never the hash.
type TokenInfo struct {
	ID         int64
	Name       string
	CreatedAt  string
	LastUsedAt string
	ExpiresAt  string
}

// ListTokens returns a user's tokens, newest first.
func (s *Store) ListTokens(userID int64) ([]TokenInfo, error) {
	rows, err := s.db.Query(
		`SELECT id, name, created_at, COALESCE(last_used_at, ''), COALESCE(expires_at, '')
		 FROM api_tokens WHERE user_id = ? ORDER BY id DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TokenInfo
	for rows.Next() {
		var t TokenInfo
		if err := rows.Scan(&t.ID, &t.Name, &t.CreatedAt, &t.LastUsedAt, &t.ExpiresAt); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// RevokeToken deletes one of userID's tokens. ErrNotFound covers both a
// missing id and someone else's token.
func (s *Store) RevokeToken(userID, id int64) error {
	res, err := s.db.Exec(`DELETE FROM api_tokens WHERE id = ? AND user_id = ?`, id, userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}
```

- [ ] **Step 4: Run** `go test ./internal/store/ -v` — all pass.
- [ ] **Step 5: Commit** — `feat: rollback lineage + token list/revoke in store`

---

### Task 2: server — rollback endpoint

**Files:**
- Create: `internal/server/rollback.go`
- Modify: `internal/server/apps.go` (extract `applyImageDeploy` from `deployImage`)
- Modify: `internal/server/server.go` (route)
- Test: `internal/server/rollback_test.go`

**Interfaces:**
- Consumes: Task 1 store methods; `deployImage` (`apps.go` — read it fully; you extract its render+apply+status core); `s.registryHost`.
- Produces:
  - `POST /v1/projects/{project}/apps/{app}/rollback` body `{"deploy_id": N}` (0/omitted = auto-pick) → runs the same synchronous render+apply as image deploys, then 202 `{"deployment_id":M,"status":"live"}` (a failed apply is a 500 and the row ends `failed`). Errors: 404 `not_found` (no such deploy / deploy of another app), 409 `no_target` (no previous live deploy to roll back to), 409 `image_missing` (registry HEAD says gone; message names the image), 503 kube unavailable.
  - `s.applyImageDeploy(ctx context.Context, p store.Project, a store.App, d store.Deployment, image string) error` — extracted core shared by `deployImage` and rollback: render, ensure namespace, apply, set status live (or failed + return error).
  - `s.imageInRegistry(ctx context.Context, ref string) (bool, error)` — HEAD `http://<registryHost>/v2/<name>/manifests/<tag>`; refs not prefixed `s.registryHost+"/"` return `(true, nil)` (external images unverifiable).
  - `s.rollbackTarget(a store.App, deployID int64) (store.Deployment, error)` — explicit id: `GetDeployment` + must belong to app + `ImageRef != ""`; auto: newest deployment with status `live`, non-empty image, and ID < latest deployment's ID. Errors are sentinel-wrapped for the handler (`errNoRollbackTarget`).

- [ ] **Step 1: Failing tests** (`internal/server/rollback_test.go`). Reuse the fake-kube + seeded-store fixture from `apps_test.go` (the deployImage/scale tests show it). An `httptest.NewServer` stands in for the registry — return 200 for known manifest paths, 404 otherwise — and its `Listener.Addr().String()` becomes `Deps.RegistryHost`:

```go
func TestRollbackHappyPath(t *testing.T) {
	// Arrange: registry httptest server that 200s
	// HEAD /v2/proj/web/manifests/1; Deps.RegistryHost = its host:port.
	// Seed: project proj, app web; deployment 1 (status live,
	// image_ref "<registryHost>/proj/web:1"); deployment 2 (status live,
	// image_ref "<registryHost>/proj/web:2").
	// Act: POST .../rollback with {} (auto-pick).
	// Assert: 202; response deployment_id = 3; store row 3 has
	// status "live" (fake kube apply succeeds), image "<...>/proj/web:1",
	// rolled_back_from = 1.
}

func TestRollbackExplicitAndErrors(t *testing.T) {
	// Same fixture.
	// - explicit {"deploy_id": 1} → 202, image :1.
	// - {"deploy_id": 99} → 404 not_found.
	// - registry 404s the manifest (roll back to a deploy whose tag the
	//   fake registry doesn't know) → 409 image_missing, message contains
	//   the image ref.
	// - app with a single deployment → auto-pick → 409 no_target.
}
```

(Real code. HEAD requests hit the httptest server because `imageInRegistry` builds `http://<RegistryHost>/...` and RegistryHost is the test server's host:port.)

- [ ] **Step 2: Run** — compile failure.

- [ ] **Step 3: Implement.**

`apps.go` — extract from `deployImage` (keep `deployImage`'s HTTP shape; it now delegates):

```go
// applyImageDeploy is the synchronous render+apply core shared by prebuilt
// image deploys and rollbacks: apply the app at `image`, then mark the
// deployment live — or failed, returning the error.
func (s *server) applyImageDeploy(ctx context.Context, p store.Project, a store.App, d store.Deployment, image string) error {
	rendered, err := s.renderApp(p, a, image, true)
	if err == nil {
		if err = s.kube.EnsureNamespace(ctx, p.Namespace); err == nil {
			err = s.kube.Apply(ctx, p.Namespace, rendered.Objects)
		}
	}
	if err != nil {
		if e := s.st.SetDeploymentStatus(d.ID, "failed"); e != nil {
			log.Printf("mark deploy %d failed: %v", d.ID, e)
		}
		return err
	}
	if err := s.st.SetDeploymentStatus(d.ID, "live"); err != nil {
		log.Printf("mark deploy %d live (apply already succeeded): %v", d.ID, err)
	}
	return nil
}
```

`deployImage` becomes: create row (as today) → `if err := s.applyImageDeploy(r.Context(), p, a, d, image); err != nil { <the existing error-response branch> }` → the existing success response. Preserve its current response codes/bodies exactly — the existing tests must keep passing unmodified.

`internal/server/rollback.go`:

```go
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"

	"github.com/sutantodadang/luncur/internal/store"
)

var errNoRollbackTarget = errors.New("no previous live deployment to roll back to")

// rollbackTarget picks the deployment to roll back to: an explicit id (must
// be this app's, with an image), or the newest live deployment older than
// the latest one.
func (s *server) rollbackTarget(a store.App, deployID int64) (store.Deployment, error) {
	if deployID != 0 {
		d, err := s.st.GetDeployment(deployID)
		if err != nil || d.AppID != a.ID {
			return store.Deployment{}, store.ErrNotFound
		}
		if d.ImageRef == "" {
			return store.Deployment{}, errNoRollbackTarget
		}
		return d, nil
	}
	latest, err := s.st.LatestDeployment(a.ID)
	if err != nil {
		return store.Deployment{}, errNoRollbackTarget
	}
	history, err := s.st.ListDeployments(a.ID)
	if err != nil {
		return store.Deployment{}, err
	}
	for _, d := range history { // newest first
		if d.ID < latest.ID && d.Status == "live" && d.ImageRef != "" {
			return d, nil
		}
	}
	return store.Deployment{}, errNoRollbackTarget
}

// imageInRegistry HEAD-checks the embedded registry for ref's manifest.
// External refs (different host prefix) are assumed present — luncur has no
// credentials to verify them.
func (s *server) imageInRegistry(ctx context.Context, ref string) (bool, error) {
	rest, ok := strings.CutPrefix(ref, s.registryHost+"/")
	if !ok {
		return true, nil
	}
	name, tag, ok := strings.Cut(rest, ":")
	if !ok {
		return true, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodHead,
		"http://"+s.registryHost+"/v2/"+name+"/manifests/"+tag, nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("Accept",
		"application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.v2+json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false, err
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK, nil
}

func (s *server) handleRollback(w http.ResponseWriter, r *http.Request, u store.User) {
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
	var req struct {
		DeployID int64 `json:"deploy_id"`
	}
	if r.Body != nil {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil && err.Error() != "EOF" {
			writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
			return
		}
	}

	target, err := s.rollbackTarget(a, req.DeployID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", "no such deployment for this app")
		return
	case errors.Is(err, errNoRollbackTarget):
		writeError(w, http.StatusConflict, "no_target", errNoRollbackTarget.Error())
		return
	case err != nil:
		log.Printf("rollback target: %v", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}

	present, err := s.imageInRegistry(r.Context(), target.ImageRef)
	if err != nil {
		log.Printf("registry check: %v", err)
		writeError(w, http.StatusBadGateway, "registry_error", "could not verify image in registry")
		return
	}
	if !present {
		writeError(w, http.StatusConflict, "image_missing",
			fmt.Sprintf("image %s is no longer in the registry", target.ImageRef))
		return
	}

	d, err := s.st.CreateRollbackDeployment(a.ID, target.ImageRef, u.ID, target.ID)
	if err != nil {
		log.Printf("create rollback deployment: %v", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	if err := s.applyImageDeploy(r.Context(), p, a, d, target.ImageRef); err != nil {
		log.Printf("rollback apply: %v", err)
		writeError(w, http.StatusInternalServerError, "internal", "rollback apply failed")
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"deployment_id": d.ID, "status": "live"})
}
```

Route in `server.go`:

```go
	mux.HandleFunc("POST /v1/projects/{project}/apps/{app}/rollback", s.authed(s.handleRollback))
```

- [ ] **Step 4: Run** `go test ./internal/server/ -v` — pass (including the untouched deployImage tests).
- [ ] **Step 5: Commit** — `feat: rollback endpoint — redeploy a previous image, no build`

---

### Task 3: CLI + API — rollback command, token list/revoke

**Files:**
- Create: `internal/server/tokens.go`
- Modify: `internal/server/server.go` (routes)
- Modify: `internal/client/client.go`
- Create: `internal/cli/rollback.go`, `internal/cli/token.go`
- Modify: `internal/cli/root.go`
- Test: `internal/server/tokens_test.go`, `internal/cli/commands_test.go` (append)

**Interfaces:**
- Consumes: Tasks 1-2; `Client.do`; `apiClient()`; the cobra patterns in `internal/cli/domain.go`.
- Produces:
  - API: `GET /v1/tokens` (authed, own tokens) → `[{"id","name","created_at","last_used_at","expires_at"}]`; `DELETE /v1/tokens/{id}` → 204 / 404.
  - Client: `Rollback(project, app string, deployID int64) (int64, error)` (returns new deployment id), `ListTokens() ([]TokenInfo, error)` with `type TokenInfo struct { ID int64 \`json:"id"\`; Name string \`json:"name"\`; CreatedAt string \`json:"created_at"\`; LastUsedAt string \`json:"last_used_at"\`; ExpiresAt string \`json:"expires_at"\` }`, `RevokeToken(id int64) error`.
  - CLI: `luncur rollback <app> --project P [--deploy N]` — prints `rolled back to deploy N (new deploy M)`; `luncur token list` (tabwriter ID/NAME/CREATED/LAST USED/EXPIRES), `luncur token revoke <id>`.

- [ ] **Step 1: Failing tests.**

`internal/server/tokens_test.go` (follow `users_test.go` fixtures): list returns the caller's tokens only (create two users, one token each; each sees exactly their own); revoke own → 204 and the token stops authenticating (a follow-up `/v1/me` with it → 401); revoke someone else's id → 404.

Append to `internal/cli/commands_test.go`:

```go
func TestTokenAndRollbackCommands(t *testing.T) {
	// testEnv + login (admin) as usual.
	// token list → output contains the login-created token's name.
	// token revoke <that id> → subsequent `token list` errors (401) OR —
	//   simpler: create a SECOND token via the API first, revoke that one,
	//   and assert it's gone from `token list` while the session survives.
	// rollback requires kube; testEnv has none → `rollback web --project p`
	//   surfaces the server's kubernetes_unavailable error — assert the
	//   command returns an error mentioning "kubernetes".
}
```

(Real code following the file's helpers.)

- [ ] **Step 2: Run** — failures.

- [ ] **Step 3: Implement.**

`internal/server/tokens.go`:

```go
package server

import (
	"errors"
	"log"
	"net/http"
	"strconv"

	"github.com/sutantodadang/luncur/internal/store"
)

func (s *server) handleListTokens(w http.ResponseWriter, r *http.Request, u store.User) {
	list, err := s.st.ListTokens(u.ID)
	if err != nil {
		log.Printf("list tokens: %v", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	out := make([]map[string]any, 0, len(list))
	for _, t := range list {
		out = append(out, map[string]any{
			"id": t.ID, "name": t.Name, "created_at": t.CreatedAt,
			"last_used_at": t.LastUsedAt, "expires_at": t.ExpiresAt,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *server) handleRevokeToken(w http.ResponseWriter, r *http.Request, u store.User) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid token id")
		return
	}
	if err := s.st.RevokeToken(u.ID, id); errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "no such token")
		return
	} else if err != nil {
		log.Printf("revoke token: %v", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
```

Routes: `GET /v1/tokens` + `DELETE /v1/tokens/{id}`, both `s.authed`.

`client.go`: the three methods + `TokenInfo` (shapes in Interfaces; same `do` helper; `Rollback` POSTs `map[string]int64{"deploy_id": deployID}` and decodes `{"deployment_id":...}`).

`internal/cli/rollback.go`:

```go
package cli

import (
	"github.com/spf13/cobra"
)

func rollbackCmd() *cobra.Command {
	var project string
	var deploy int64
	cmd := &cobra.Command{
		Use:   "rollback <app>",
		Short: "Redeploy a previous deployment's image",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient()
			if err != nil {
				return err
			}
			newID, err := c.Rollback(project, args[0], deploy)
			if err != nil {
				return err
			}
			cmd.Printf("rolled back (new deploy %d)\n", newID)
			return nil
		},
	}
	cmd.Flags().StringVar(&project, "project", "", "project name")
	cmd.MarkFlagRequired("project")
	cmd.Flags().Int64Var(&deploy, "deploy", 0, "deployment id to roll back to (default: previous live)")
	return cmd
}
```

`internal/cli/token.go` — `tokenCmd()` with `list` (tabwriter `ID\tNAME\tCREATED\tLAST USED\tEXPIRES`) and `revoke <id>` subcommands, mirroring `sshkey.go`'s shape. Register `rollbackCmd()` + `tokenCmd()` in `root.go`.

- [ ] **Step 4: Run** `go test ./internal/server/ ./internal/client/ ./internal/cli/ -v` — pass.
- [ ] **Step 5: Commit** — `feat: rollback + token CLI, token API`

---

### Task 4: up/kube — scoped ClusterRole replaces cluster-admin

**Files:**
- Modify: `internal/kube/kube.go` (ClusterRole GVR)
- Modify: `internal/up/manifests.go`
- Test: `internal/up/manifests_test.go` (extend), `internal/kube/kube_test.go` (only if a new Apply path needs it — it doesn't; GVR entry is exercised via the manifests golden test + Apply reuse)

**Interfaces:**
- Consumes: existing `LuncurObjects`, `gvrByKind`/`clusterScoped`.
- Produces:
  - `gvrByKind` gains `"ClusterRole": {Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterroles"}`; `clusterScoped` gains `"ClusterRole": true`.
  - `LuncurObjects` emits a `ClusterRole` named `luncur` and the existing `ClusterRoleBinding` now references it (RoleRef `ClusterRole luncur`, not `cluster-admin`). Rules (golden-pinned):

| apiGroups | resources | verbs |
|---|---|---|
| `""` | namespaces, services, secrets, configmaps, serviceaccounts, persistentvolumeclaims | get, list, watch, create, update, patch, delete |
| `""` | pods, pods/log, events, nodes | get, list, watch |
| `apps` | deployments | get, list, watch, create, update, patch, delete |
| `apps` | replicasets | get, list, watch |
| `batch` | jobs | get, list, watch, create, update, patch, delete |
| `networking.k8s.io` | ingresses | get, list, watch, create, update, patch, delete |
| `helm.cattle.io` | helmchartconfigs | get, list, watch, create, update, patch |
| `cert-manager.io` | clusterissuers | get, list, watch, create, update, patch |

- [ ] **Step 1: Failing test** — extend `TestLuncurObjects` in `manifests_test.go`: kinds set must now include `"ClusterRole"`; the aggregate JSON must contain `"pods/log"`, `"helmchartconfigs"`, `"clusterissuers"`; and must NOT contain `"cluster-admin"` (the binding now points at `luncur`):

```go
	if strings.Contains(all, "cluster-admin") {
		t.Fatal("cluster-admin binding must be gone")
	}
	for _, want := range []string{`"ClusterRole"`, "pods/log", "helmchartconfigs", "clusterissuers"} {
		if !strings.Contains(all, want) {
			t.Fatalf("manifests missing %q", want)
		}
	}
```

(Also update the existing assertion that expects `"cluster-admin"` — Plan D's test asserted it; flip it.)

- [ ] **Step 2: Run** `go test ./internal/up/ -v` — fails.

- [ ] **Step 3: Implement.** In `manifests.go`, before the ClusterRoleBinding, build the role with a small helper:

```go
	rule := func(groups []string, resources []string, verbs ...string) rbacv1.PolicyRule {
		return rbacv1.PolicyRule{APIGroups: groups, Resources: resources, Verbs: verbs}
	}
	full := []string{"get", "list", "watch", "create", "update", "patch", "delete"}
	read := []string{"get", "list", "watch"}
	manage := []string{"get", "list", "watch", "create", "update", "patch"}
	cr := &rbacv1.ClusterRole{
		TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "ClusterRole"},
		ObjectMeta: metav1.ObjectMeta{Name: "luncur", Labels: labels},
		Rules: []rbacv1.PolicyRule{
			rule([]string{""}, []string{"namespaces", "services", "secrets", "configmaps", "serviceaccounts", "persistentvolumeclaims"}, full...),
			rule([]string{""}, []string{"pods", "pods/log", "events", "nodes"}, read...),
			rule([]string{"apps"}, []string{"deployments"}, full...),
			rule([]string{"apps"}, []string{"replicasets"}, read...),
			rule([]string{"batch"}, []string{"jobs"}, full...),
			rule([]string{"networking.k8s.io"}, []string{"ingresses"}, full...),
			rule([]string{"helm.cattle.io"}, []string{"helmchartconfigs"}, manage...),
			rule([]string{"cert-manager.io"}, []string{"clusterissuers"}, manage...),
		},
	}
	if err := add("ClusterRole", cr); err != nil {
		return nil, err
	}
```

and change the binding's RoleRef to `Name: "luncur"` (delete the old ponytail comment about cluster-admin — replace with `// Scoped role: rules enumerate exactly what serve touches; extend here when a new kind is applied.`). Add the two `kube.go` map entries.

- [ ] **Step 4: Run** `go test ./internal/up/ ./internal/kube/ -v` — pass.
- [ ] **Step 5: Commit** — `feat: scoped ClusterRole replaces cluster-admin binding`

---

### Task 5: web UI — CSRF tokens on every form

**Files:**
- Modify: `internal/server/ui.go`
- Modify: `internal/server/templates/base.html`, `login.html`, `app.html`, `projects.html`, `apps.html`
- Test: `internal/server/ui_test.go` (append + update existing form posts)

**Interfaces:**
- Consumes: existing `uiPage`, `handleUILogin`, all `/ui/` POST handlers and templates.
- Produces:
  - Cookie `luncur_csrf`: 32-hex random, `Path=/`, `HttpOnly`, `SameSite=Strict`, no expiry (session cookie). Created by `s.csrf(w, r) string` — returns the existing value or mints+sets one.
  - `uiPage` verifies EVERY POST: `ParseForm`, then `subtle.ConstantTimeCompare` of cookie value vs `_csrf` form field → 403 `invalid CSRF token` on mismatch/absence.
  - `handleUILogin` (not uiPage-wrapped) does the same check itself; `handleUILoginPage` calls `s.csrf` and passes `"CSRF"` to the template. `handleUILogout` moves under the same verification (wrap its body with the shared check — extract `s.checkCSRF(w, r) bool`).
  - Every template form gains `<input type="hidden" name="_csrf" value="{{.CSRF}}">` (`{{$.CSRF}}` inside `range` blocks). All page view-models gain `"CSRF": s.csrf(w, r)`.

- [ ] **Step 1: Failing test** (append to `ui_test.go`):

```go
func TestUIPostRequiresCSRF(t *testing.T) {
	// Arrange: full login flow (existing fixture) — capture BOTH cookies
	// (session + csrf) from the login response.
	// 1. POST /ui/projects/p/apps/a/scale with session cookie but NO _csrf
	//    field → 403.
	// 2. Same POST with _csrf=wrong → 403.
	// 3. Same POST with the real csrf cookie value in _csrf → not 403
	//    (any of 303/400/503 acceptable depending on fixture kube).
	// 4. POST /ui/login without _csrf → 403 (fetch GET /ui/login first to
	//    obtain the csrf cookie, then omit the field).
}
```

**Also update every existing `ui_test.go` (and `domains` UI test) form POST** to fetch the login page / use the login response's csrf cookie and include `_csrf` — they will otherwise fail with 403 after this task. Grep the test file for `PostForm`/`strings.NewReader(` form bodies.

- [ ] **Step 2: Run** — new test fails (no 403), existing pass.

- [ ] **Step 3: Implement** in `ui.go`:

```go
const csrfCookie = "luncur_csrf"

// csrf returns the request's CSRF token, minting the cookie on first use.
// Double-submit pattern: the value is only ever compared against the same
// browser's form field, so no server-side state is needed.
func (s *server) csrf(w http.ResponseWriter, r *http.Request) string {
	if ck, err := r.Cookie(csrfCookie); err == nil && ck.Value != "" {
		return ck.Value
	}
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		log.Printf("csrf rand: %v", err)
		return ""
	}
	v := hex.EncodeToString(raw)
	http.SetCookie(w, &http.Cookie{
		Name: csrfCookie, Value: v, Path: "/",
		HttpOnly: true, SameSite: http.SameSiteStrictMode,
	})
	return v
}

// checkCSRF verifies a POST's _csrf field against the cookie. Writes the
// 403 itself so callers can just return.
func (s *server) checkCSRF(w http.ResponseWriter, r *http.Request) bool {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return false
	}
	ck, err := r.Cookie(csrfCookie)
	if err != nil || ck.Value == "" ||
		subtle.ConstantTimeCompare([]byte(ck.Value), []byte(r.PostFormValue("_csrf"))) != 1 {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)
		return false
	}
	return true
}
```

(imports: `crypto/rand`, `crypto/subtle`, `encoding/hex`.) `uiPage` gains, after the session check:

```go
		if r.Method == http.MethodPost && !s.checkCSRF(w, r) {
			return
		}
```

`handleUILogin` starts with `if !s.checkCSRF(w, r) { return }`; `handleUILoginPage` renders with `map[string]any{"CSRF": s.csrf(w, r)}`; `handleUILogout` starts with the same check. Every `renderPage(...)` data map in `ui.go` gains `"CSRF": s.csrf(w, r)`. Templates: add the hidden input to the nav logout form (`{{.CSRF}}` — nav is invoked with the page's dot), the login form, and every form in `app.html` (`{{$.CSRF}}` inside `{{range}}`).

- [ ] **Step 4: Run** `go test ./internal/server/ -v` — all pass.
- [ ] **Step 5: Commit** — `feat: CSRF tokens on all web UI forms`

---

### Task 6: UI rollback button + README + final verification

**Files:**
- Modify: `internal/server/ui.go` (rollback handler + route)
- Modify: `internal/server/templates/app.html`
- Modify: `README.md`
- Test: `internal/server/ui_test.go` (append)

**Interfaces:**
- Consumes: Task 2's `rollbackTarget`/`imageInRegistry`/`CreateRollbackDeployment`/`applyImageDeploy`; Task 5's CSRF (form includes `_csrf`).
- Produces:
  - `POST /ui/projects/{project}/apps/{app}/rollback` (form field `deploy_id`) — same core as the API handler (extract `s.rollback(ctx, p, a, u, deployID) (store.Deployment, error)` shared by both; API handler maps sentinel errors to envelope codes, UI handler to plain-text statuses), redirect back to the app page.
  - `app.html` deploy-history rows gain a `rollback` button (hidden for the newest row and for rows without an image):

```html
{{range .History}}<tr>
  <td>{{.ID}}</td><td class="status-{{.Status}}">{{.Status}}</td>
  <td><code>{{.ImageRef}}</code>{{if .RolledBackFrom}} <em>(rollback of {{.RolledBackFrom}})</em>{{end}}</td>
  <td>{{.CreatedAt}}</td>
  <td>{{if and .ImageRef (ne .ID $.LatestID)}}
    <form class="inline" method="post" action="/ui/projects/{{$.Project.Name}}/apps/{{$.App.Name}}/rollback">
      <input type="hidden" name="_csrf" value="{{$.CSRF}}">
      <input type="hidden" name="deploy_id" value="{{.ID}}">
      <button type="submit">rollback</button>
    </form>
  {{end}}</td>
</tr>{{end}}
```

(Replace the existing History table block with this — note the added `<th></th>` column and the `(rollback of N)` marker.)
  - README: "Rollback" subsection under deployment docs (`luncur rollback`, UI button, registry-check caveat); token subsection (`luncur token list/revoke`); security paragraph (scoped ClusterRole, CSRF); Plan G deviations appended; status line mentions Plan G.

- [ ] **Step 1: Failing test** — append to `ui_test.go`: with the fake-kube fixture and two live deployments, a CSRF-correct `POST /ui/.../rollback` with `deploy_id=1` → 303; the app page then shows `(rollback of 1)`.

- [ ] **Step 2: Run** — fails.

- [ ] **Step 3: Implement**: extract the shared `s.rollback(...)` core in `rollback.go` (move the target/registry/create/apply sequence out of `handleRollback`; both handlers call it — the API handler keeps its exact response mapping from Task 2 so its tests stay green); add `handleUIRollback` in `ui.go` (uiPage-wrapped, form `deploy_id` via `strconv.ParseInt`, errors → `http.Error` with 404/409/503/500 mirroring the API mapping, success → `uiRedirect`); route in `uiRoutes`; template block above; README updates.

- [ ] **Step 4: Run** `go build ./... && go vet ./... && go test ./...` — green. `gofmt -l internal/ cmd/` — clean (pre-existing `client.go` flag exempted). `grep -rn "Plan G" README.md internal/` — only the intentional README mention.
- [ ] **Step 5: Commit** — `feat: UI rollback + hardening docs`

---

## Final verification (after all tasks)

- [ ] `go build ./... && go vet ./... && go test ./...` — everything green.
- [ ] Push branch `plan-g`, open PR against `main`.
- [ ] Manual (owner's VPS, post-merge): deploy twice, roll back via CLI and UI; `luncur token list/revoke`; confirm apps still deploy under the scoped ClusterRole (watch for RBAC denials in luncur logs — the rule table above is the fix-it reference).

## Spec-coverage self-check (Plan G section of 2026-07-03-luncur-phase2-design.md)

- Rollback CLI (`--deploy N` optional, default previous live) ✅ (T2/T3); UI button per history row ✅ (T6); new deployment row with `rolled_back_from` ✅ (T1/T2); no build ✅; registry HEAD check + 409 naming the image ✅ (T2, external-ref deviation documented); registry GC out of scope ✅ (untouched).
- `luncur token list` (name/created/last-used/expiry) + `revoke`; sessions appear as `session` rows; revoking one logs that browser out ✅ (T1/T3 — session tokens live in the same table, so revocation kills the cookie's auth).
- Scoped ClusterRole enumerating luncur's surface + HelmChartConfig + cert-manager CRDs; golden test pins rules ✅ (T4).
- CSRF: per-session token in hidden form field checked on every /ui/ POST; SameSite=Strict stays ✅ (T5, double-submit deviation documented).
