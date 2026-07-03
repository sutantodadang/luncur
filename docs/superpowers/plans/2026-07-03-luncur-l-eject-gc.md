# luncur Plan L — eject + registry GC Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `luncur app eject` detaches an app from luncur's management one-way (objects keep running, luncur refuses further mutations); `luncur registry gc` reclaims registry disk with a retention policy (weekly sweep + manual trigger).

**Architecture:** Eject is a flag on the app row enforced by one guard helper at every mutating entry point (API, UI, git push, opportunistic sync); the final rendered YAML is returned and archived under `data/ejected/`. Registry GC computes a keep-set purely from the DB (newest `registry_keep` images per app + everything live/newest), diffs it against the registry HTTP API's catalog, DELETEs manifests by digest, then execs `registry garbage-collect` in the registry pod — bytes reclaimed measured with `du` before/after.

**Tech Stack:** Go stdlib, existing registry HTTP API (`/v2/`), `kube.PodExecer` + `AppPods` (Plan I), modernc.org/sqlite.

## Global Constraints

- Single Go module, one binary from `cmd/luncur`. **No new dependencies.**
- Server-side apply everywhere. API error envelope via `writeError`; the eject refusal is ALWAYS `409` code `app_ejected`. Conventional commits; `go build ./... && go vet ./... && go test ./...` before every commit.
- Tests must not require a cluster or network: retention is a pure function; the registry API is an httptest fake; pod exec is the `PodExecer` fake.
- **Approved deviations from the Phase 3 spec (record in README):**
  - "Bytes reclaimed" is measured with `du -sk` inside the registry pod before/after `garbage-collect` (busybox `du`, KiB resolution). When exec fails, the report falls back to the deleted-manifest count with bytes `-1` ("unknown").

---

### Task 1: store — apps.ejected flag

**Files:**
- Modify: `internal/store/store.go` (migrate)
- Modify: `internal/store/apps.go`
- Test: `internal/store/apps_test.go` (append)

**Interfaces:**
- Consumes: the migrate column loop; `App` struct and its scan sites in `apps.go` (`GetApp`, `GetAppByID`, `ListApps` — read the file; every SELECT/scan gains the column).
- Produces:
  - migrate: `{"apps", "ejected", "ALTER TABLE apps ADD COLUMN ejected INTEGER NOT NULL DEFAULT 0"}`.
  - `App` gains `Ejected bool`.
  - `Store.SetAppEjected(id int64) error` — `ErrNotFound` when absent; one-way (no un-eject setter).

- [ ] **Step 1: Failing test** (append to `apps_test.go`):

```go
func TestAppEjected(t *testing.T) {
	s := openTest(t)
	p, _ := s.CreateProject("proj")
	a, _ := s.CreateApp(p.ID, "web", 8080)
	if a.Ejected {
		t.Fatal("new app born ejected")
	}
	if err := s.SetAppEjected(a.ID); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetApp(p.ID, "web")
	if err != nil || !got.Ejected {
		t.Fatalf("ejected not persisted: %+v err=%v", got, err)
	}
	byID, err := s.GetAppByID(a.ID)
	if err != nil || !byID.Ejected {
		t.Fatalf("GetAppByID: %+v err=%v", byID, err)
	}
	list, err := s.ListApps(p.ID)
	if err != nil || len(list) != 1 || !list[0].Ejected {
		t.Fatalf("ListApps: %+v err=%v", list, err)
	}
	if err := s.SetAppEjected(a.ID + 99); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing app: %v", err)
	}
}
```

- [ ] **Step 2: Run** `go test ./internal/store/ -run TestAppEjected -v` — compile failure.
- [ ] **Step 3: Implement** — migrate row; `Ejected bool` on `App`; every apps SELECT gains `ejected` with a direct bool scan (modernc converts 0/1); `SetAppEjected` mirrors `SetReplicas`' UPDATE + RowsAffected/ErrNotFound shape.
- [ ] **Step 4: Run** `go test ./internal/store/ -v` — pass.
- [ ] **Step 5: Commit** — `feat: apps.ejected flag in store`

---

### Task 2: server — eject endpoint + mutation guards

**Files:**
- Create: `internal/server/eject.go`
- Modify: `internal/server/apps.go` (deploy/scale/destroy guards), `appenv.go` (env + override cores), `domains.go` (addDomain + delete/retry), `rollback.go` (shared core), `addons.go` (attach/detach), `push.go` (Branch), `sync.go` (syncIfLive/syncApp skip), `ui.go` (eject badge data comes in Task 4; here only shared cores already guard the UI twins)
- Modify: `internal/server/server.go` (route)
- Test: `internal/server/eject_test.go`

**Interfaces:**
- Consumes: Task 1 (`App.Ejected`, `SetAppEjected`), `renderApp` + `render.YAML`, `appImage` (Plan H helper in `appenv.go`), `s.dataDir`.
- Produces:
  - `errAppEjected = errors.New("app is ejected from luncur management")` (in `eject.go`).
  - `s.refuseEjected(w http.ResponseWriter, a store.App) bool` — writes `409` `app_ejected` and returns true when `a.Ejected` (JSON envelope; UI twins that use shared cores surface the sentinel instead).
  - Guard placement (the matrix the test pins):
    - `handleDeployApp` (all three branches — multipart, image, git) — guard right after `requireApp`.
    - `scaleApp` shared core returns `errAppEjected` (API 409 / UI 409 plain text).
    - `setAppEnv`/`unsetAppEnv` cores return `errAppEjected`; handlers map to 409.
    - `addDomain` core + `handleDeleteDomain`/`handleRetryDomain` + UI delete twin.
    - `handleSetOverride`/`handleDeleteOverride` via the `setOverride` core.
    - rollback shared core (`s.rollback`) returns `errAppEjected` → API/UI 409.
    - addon `attachAddon` core + `handleDetachAddon` → 409.
    - git push: `PushBackend.Branch` returns the sentinel's message (client sees it on stderr).
    - `syncIfLive`/`syncApp` return/no-op silently for ejected apps (opportunistic syncs must not error).
    - Reads stay open: status, logs, metrics, raw YAML, app page all keep working.
  - `POST /v1/projects/{project}/apps/{app}/eject` (authed) → marks ejected, renders the final YAML (current overrides + latest image via `appImage`), writes it to `<dataDir>/ejected/<project>-<app>.yaml`, responds 200 `{"yaml":"...","saved_to":"..."}`. Second call → 409 `app_ejected`. No kube needed (render is store-only); missing image (never deployed) renders with the placeholder image the raw endpoint uses.
  - `handleDeleteApp` on an ejected app: skips ALL kube object deletion, removes DB rows only (existing store cascade). Non-ejected behavior unchanged.

- [ ] **Step 1: Failing tests** (`internal/server/eject_test.go`, package fixtures):

```go
func TestEjectFlow(t *testing.T) {
	// Seeded project/app with a live deployment + fake kube + DataDir.
	// 1. POST .../eject → 200; body yaml contains "kind: Deployment";
	//    file exists under dataDir/ejected/.
	// 2. Second POST .../eject → 409 app_ejected.
	// 3. Guard matrix — each of these now returns 409 app_ejected:
	//    POST deploy (json image body), POST scale, PUT env, POST domains,
	//    PUT overrides/Deployment, POST rollback, POST addons/{n}/attach
	//    (create an addon first).
	// 4. Reads still work: GET app (200), GET raw (200), GET metrics (200).
	// 5. DELETE app → 204; fake kube saw NO delete actions for the app's
	//    objects (assert on the recorded actions); app row gone.
}

func TestPushRefusesEjected(t *testing.T) {
	// PushBackend.Branch on an ejected app returns an error containing
	// "ejected".
}
```

(Real code. For the kube-action assertion reuse the fixtures' recorded-actions pattern from `addons_test.go`/`rollback_test.go`.)

- [ ] **Step 2: Run** — failures.
- [ ] **Step 3: Implement** per the Interfaces block. `eject.go` shape:

```go
package server

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"

	"github.com/sutantodadang/luncur/internal/render"
	"github.com/sutantodadang/luncur/internal/store"
)

var errAppEjected = errors.New("app is ejected from luncur management")

// refuseEjected 409s mutations on ejected apps. Reads never call this.
func (s *server) refuseEjected(w http.ResponseWriter, a store.App) bool {
	if !a.Ejected {
		return false
	}
	writeError(w, http.StatusConflict, "app_ejected", errAppEjected.Error())
	return true
}

func (s *server) handleEjectApp(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.requireProject(w, u, r.PathValue("project"))
	if !ok {
		return
	}
	a, ok := s.requireApp(w, p, r.PathValue("app"))
	if !ok {
		return
	}
	if s.refuseEjected(w, a) {
		return
	}

	image, err := s.appImage(a)
	if err != nil {
		log.Printf("eject %s: image: %v", a.Name, err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	rendered, err := s.renderApp(p, a, image, true)
	if err != nil {
		log.Printf("eject %s: render: %v", a.Name, err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	y, err := render.YAML(rendered)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}

	if err := s.st.SetAppEjected(a.ID); err != nil {
		log.Printf("eject %s: %v", a.Name, err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}

	saved := ""
	if s.dataDir != "" {
		dir := filepath.Join(s.dataDir, "ejected")
		if err := os.MkdirAll(dir, 0o700); err == nil {
			saved = filepath.Join(dir, fmt.Sprintf("%s-%s.yaml", p.Name, a.Name))
			if err := os.WriteFile(saved, y, 0o600); err != nil {
				log.Printf("eject %s: save yaml: %v", a.Name, err)
				saved = ""
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"yaml": string(y), "saved_to": saved})
}
```

Guards: handlers that own their flow call `refuseEjected` right after `requireApp`; shared cores (`scaleApp`, `setAppEnv`, `unsetAppEnv`, `addDomain`, `setOverride`, `rollback`, `attachAddon`) start with `if a.Ejected { return ...errAppEjected }` and their API callers map `errors.Is(err, errAppEjected)` → `writeError(w, 409, "app_ejected", ...)`, UI callers → `http.Error(w, err.Error(), http.StatusConflict)`. `PushBackend.Branch`: after loading the app, `if a.Ejected { return "", errAppEjected }`. `syncIfLive`/`syncApp`: `if a.Ejected { return nil }` (silent). `handleDeleteApp`: read its current shape; wrap the kube-deletion block in `if !a.Ejected { ... }`. Route: `mux.HandleFunc("POST /v1/projects/{project}/apps/{app}/eject", s.authed(s.handleEjectApp))`.

- [ ] **Step 4: Run** `go test ./internal/server/ -v` — pass (existing tests unchanged — non-ejected paths must behave identically).
- [ ] **Step 5: Commit** — `feat: app eject — one-way detach with mutation guards`

---

### Task 3: registry GC — client, retention, sweep

**Files:**
- Create: `internal/registry/registry.go` (HTTP client + retention pure function)
- Create: `internal/server/registrygc.go`
- Modify: `internal/build/infra.go` (delete-enabled env)
- Modify: `internal/server/settings.go` (registry_keep key)
- Modify: `internal/server/server.go` (route; weekly loop in the start closure)
- Test: `internal/registry/registry_test.go`, `internal/server/registrygc_test.go`, `internal/build/infra_test.go` (extend), `internal/server/settings_test.go` (append)

**Interfaces:**
- Consumes: registry HTTP API (`GET /v2/_catalog`, `GET /v2/<name>/tags/list`, `HEAD /v2/<name>/manifests/<tag>` → `Docker-Content-Digest`, `DELETE /v2/<name>/manifests/<digest>`); `kube.AppPods(ctx, "luncur-system", "registry")` + `PodExecer` (container `registry`, data dir `/var/lib/registry`); `store.ListApps`/`ListProjects`/`ListDeployments`.
- Produces:
  - `registry.Client{Host string; HTTPClient *http.Client}` (Host = `host:port`, plain HTTP like `imageInRegistry`):
    - `Repositories(ctx) ([]string, error)`
    - `Tags(ctx, repo string) ([]string, error)` (404 → empty, nil)
    - `Digest(ctx, repo, tag string) (string, error)` (HEAD with the two manifest Accept types)
    - `DeleteManifest(ctx, repo, digest string) error` (202/404 both OK)
  - `registry.KeepTags(deployments []registry.DeployRef, keep int) map[string]map[string]bool` — pure retention: input `type DeployRef struct { Repo, Tag string; Live, Newest bool }` (one per deployment row with an in-registry image, newest-first per app); output repo→tag→keep. Rule: per repo, the first `keep` refs are kept PLUS every `Live` or `Newest` ref regardless of position.
  - `s.runRegistryGC(ctx) (gcReport, error)` with `type gcReport struct { DeletedManifests int; BytesReclaimed int64; Warnings []string }`:
    1. Build DeployRefs from the DB (every project → apps → `ListDeployments`; refs prefixed `s.registryHost+"/"`; repo/tag split like `imageInRegistry`; `Live` = status live, `Newest` = first row per app), `keep` from setting `registry_keep` (default 10).
    2. Registry catalog diff → DELETE manifests not in the keep-set (repos absent from the DB entirely → all tags deleted).
    3. `du -sk /var/lib/registry` via exec on the registry pod (label `registry`), run `registry garbage-collect --delete-untagged=false /etc/docker/registry/config.yml`, `du -sk` again → `BytesReclaimed = (before-after)*1024`; any exec failure → warning + `BytesReclaimed = -1`.
  - `POST /v1/registry/gc` (admin) → 200 `{"deleted_manifests":N,"bytes_reclaimed":M,"warnings":[...]}`; kube nil → GC still runs the manifest-delete phase, warns about the exec phase.
  - Weekly loop `StartRegistryGC(ctx)` (ticker 24h, runs when the last run — kept in memory only — is >7 days old or never) added to the start closure alongside certs/backups.
  - `build.SystemObjects`' registry container gains `Env: []corev1.EnvVar{{Name: "REGISTRY_STORAGE_DELETE_ENABLED", Value: "true"}}` — extend the infra golden test.
  - Settings allowlist gains `registry_keep` (positive integer).

- [ ] **Step 1: Failing tests.**

`internal/registry/registry_test.go`: `TestKeepTags` — table: 12 refs one repo keep 10 → first 10 kept + an 11th marked Live kept + 12th dropped; refs across two repos independent; Newest always kept. `TestClientAgainstFake` — httptest fake serving `_catalog` (two repos), `tags/list`, HEAD manifest returning `Docker-Content-Digest: sha256:abc`, DELETE recording the digest path; assert round-trip + 404-tags → empty.

`internal/server/registrygc_test.go`: `TestRunRegistryGC` — seeded store (project/app with 3 deployments: ids 1,2 old + 3 live/newest; images on `s.registryHost`), `registry_keep=1`, fake registry (catalog lists the repo + a stray `orphan/repo`) → deletes exactly the out-of-retention digest(s) + all orphan tags; execer fake returns `du` outputs 5000 then 3000 KiB → `BytesReclaimed == 2048000`; report counts match. `TestRegistryGCAPI` — admin POST → 200 with the fields; member → 403.

`internal/build/infra_test.go`: registry JSON contains `REGISTRY_STORAGE_DELETE_ENABLED`. `settings_test.go`: `registry_keep` `0` → 400, `10` → 204.

- [ ] **Step 2: Run** — failures.
- [ ] **Step 3: Implement** per the Interfaces block. `registry.Client` mirrors `imageInRegistry`'s plain-HTTP style; `KeepTags` is ~20 lines of map-building; `runRegistryGC` lives in `registrygc.go` with the exec phase isolated in `execRegistryGC(ctx) (int64, error)` (find pod via `s.kube.AppPods(ctx, s.systemNamespace, "registry")`, parse `du -sk` first field). The weekly loop mirrors `StartBackups`' shape. Wire `StartRegistryGC` into the `NewWithBackend` start closure.
- [ ] **Step 4: Run** `go test ./internal/registry/ ./internal/server/ ./internal/build/ -v` — pass.
- [ ] **Step 5: Commit** — `feat: registry GC — retention sweep with blob reclamation`

---

### Task 4: CLI + UI + README + final

**Files:**
- Modify: `internal/client/client.go`
- Create: `internal/cli/eject.go` (subcommand of `app` — read `internal/cli/app.go` and add `eject` beside its siblings), `internal/cli/registry.go`
- Modify: `internal/cli/root.go` (registry cmd; eject rides `app`)
- Modify: `internal/server/ui.go` + `templates/app.html`, `templates/apps.html` (ejected badge; hide mutation forms)
- Modify: `README.md`
- Test: `internal/cli/commands_test.go` (append), `internal/server/ui_test.go` (append)

**Interfaces:**
- Consumes: Tasks 2-3 endpoints.
- Produces:
  - Client: `EjectApp(project, app string) (yaml, savedTo string, err error)`; `RegistryGC() (RegistryGCReport, error)` with `type RegistryGCReport struct { DeletedManifests int \`json:"deleted_manifests"\`; BytesReclaimed int64 \`json:"bytes_reclaimed"\`; Warnings []string \`json:"warnings"\` }`.
  - CLI: `luncur app eject <name> --project P [--yes]` — interactive confirm (`fmt.Fscanln` on stdin, "this is one-way; luncur will stop managing <name>. Type the app name to confirm:") skipped by `--yes`; prints the YAML to stdout and `saved to: <path>` to stderr-style final line. `luncur registry gc` (admin) — prints deleted count + human bytes (or "unknown" for -1) + warnings.
  - UI: app page — when `.App.Ejected`, show an `ejected` badge next to the status and REPLACE the scale/env/domains/deploy/rollback/edit forms with a single note ("This app is ejected — luncur no longer manages it."); apps list page shows `(ejected)` beside the name. Template-level `{{if .App.Ejected}}` guards; handlers already 409 (defense in depth).
  - README: "Ejecting an app" section (one-way semantics, YAML archive location, destroy-keeps-objects note); "Registry GC" section (retention rule, `registry_keep`, weekly sweep, manual `luncur registry gc`, delete-enabled registry env, du-based bytes deviation); status line → "Phase 3 complete (Plans I-L)".
- [ ] **Step 1: Failing tests** — CLI: `TestEjectAndRegistryCommands` (eject with `--yes` against testEnv → output contains "kind:"; second eject errors with "ejected"; `registry gc` against kube-less testEnv → still 200 path, prints "deleted 0"); UI: ejected app's page contains "ejected" and does NOT contain `action=".../scale"`.
- [ ] **Step 2: Run** — failures.
- [ ] **Step 3: Implement** per Interfaces.
- [ ] **Step 4: Run** `go build ./... && go vet ./... && go test ./...` — green; `gofmt -l internal/` clean; `grep -rn "Plan L" README.md internal/` — only intentional.
- [ ] **Step 5: Commit** — `feat: eject + registry gc CLI/UI, phase 3 docs`

---

## Final verification (after all tasks)

- [ ] `go build ./... && go vet ./... && go test ./...` — everything green.
- [ ] Push branch `plan-l`, open PR against `main`.
- [ ] Manual (owner's VPS, post-merge): eject a test app → it keeps serving, mutations 409, destroy leaves it running; deploy 12+ times then `registry gc` → manifests deleted, bytes reclaimed reported.

## Spec-coverage self-check (Plan L section of 2026-07-03-luncur-phase3-design.md)

- Eject: confirm prompt + `--yes` ✅ (T4); flag ✅ (T1); refusal matrix incl. git push + sync ✅ (T2); objects untouched; YAML printed + saved to `data/ejected/` ✅ (T2/T4); listed as ejected in CLI/UI ✅ (T4); destroy deletes rows only ✅ (T2); one-way ✅ (no un-eject anywhere).
- GC: retention = newest `registry_keep` (default 10) per app + live + newest ✅ (T3, pure function); `REGISTRY_STORAGE_DELETE_ENABLED=true` ✅ (T3); weekly goroutine + manual `luncur registry gc` ✅ (T3/T4); list→delete-manifests→garbage-collect exec→bytes reported ✅ (T3, du deviation documented); nothing deleted unless retention computed successfully ✅ (keep-set built before any DELETE; DB errors abort).
