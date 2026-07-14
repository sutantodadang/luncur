# Environments + Preview Environments Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Insert a first-class `environment` layer between `project` and `app` — each environment owns a Kubernetes namespace with its own apps, addons, and isolation — and add webhook-driven, data-seeded preview environments per git branch.

**Architecture:** New `environments` table; `apps` and `addons` gain `environment_id`; a project's namespace ownership moves to its environments (`luncur-<project>-<env>`). Handlers resolve an `Environment` (via a new `requireEnv` resolver) and use `env.Namespace` where they used `project.Namespace`. Legacy `/apps/...` routes alias to the project's default env. Previews are ephemeral environments cloned from a base env with their addon data seeded via the existing `dumpAddon`→`restoreAddon` path.

**Tech Stack:** Go, SQLite (`database/sql`), client-go dynamic+typed, cobra CLI, html/template UI. Follows existing luncur patterns.

Spec: `docs/superpowers/specs/2026-07-14-environments-design.md`.

## Global Constraints

- SQLite migrations are additive column-adds in `store.migrate()` guarded by a `pragma_table_info` existence check; new tables go in `schema.sql` (`CREATE TABLE IF NOT EXISTS`). Copy the exact idiom already in `internal/store/store.go`.
- Names are DNS-1123 labels via `validName` (lowercase, digits, dashes, ≤40). Env names and sanitized branch names must pass it.
- Every task keeps `go build ./...`, `go vet ./...`, and `go test ./...` green. Run `gofmt -w` on touched files.
- Do NOT break the default-env path: after each phase, existing `--project`-only CLI and legacy routes behave exactly as before migration.
- Error envelopes: `writeError(w, status, code, msg)` / `writeJSON(w, status, obj)`. UI: `flash` + `uiRedirect`. Secrets stay sealed; nothing new logged in plaintext.
- Commit after every task with a `feat:`/`refactor:`/`test:` message ending `Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>`.

---

# Phase 1 — Data model (no behavior change)

Everything still runs in the migrated default (`production`) environment; the cluster/routing code is untouched. Deliverable: schema, migration, and store env layer, fully tested.

### Task 1: `environments` table + store CRUD

**Files:**
- Modify: `internal/store/schema.sql` (add `environments` table)
- Modify: `internal/store/store.go` (`migrate()` — create table on legacy DBs)
- Create: `internal/store/environments.go`
- Test: `internal/store/environments_test.go`

**Interfaces:**
- Produces:
  - `type Environment struct { ID, ProjectID int64; Name, Namespace, Kind string; IsDefault bool; BaseBranch, SourceBranch, LastActiveAt, CreatedAt string }`
  - `func (s *Store) CreateEnvironment(projectID int64, name, kind, baseBranch string) (Environment, error)`
  - `func (s *Store) ListEnvironments(projectID int64) ([]Environment, error)`
  - `func (s *Store) GetEnvironment(projectID int64, name string) (Environment, error)`
  - `func (s *Store) GetEnvironmentByID(id int64) (Environment, error)`
  - `func (s *Store) DeleteEnvironment(id int64) error`
  - `func (s *Store) SetDefaultEnvironment(projectID, envID int64) error` (clears others, sets one)
  - `func (s *Store) TouchEnvironment(id int64) error` (sets `last_active_at = datetime('now')`)
  - `func envNamespace(project, env string) string { return "luncur-" + project + "-" + env }`

- [ ] **Step 1: Write failing test** — `internal/store/environments_test.go`:

```go
package store

import "testing"

func TestEnvironmentCRUD(t *testing.T) {
	s := openTest(t)
	p, err := s.CreateProject("proj")
	if err != nil { t.Fatal(err) }

	e, err := s.CreateEnvironment(p.ID, "develop", "standing", "develop")
	if err != nil { t.Fatalf("create: %v", err) }
	if e.Namespace != "luncur-proj-develop" { t.Fatalf("ns = %q", e.Namespace) }
	if e.IsDefault { t.Fatal("new env should not be default") }

	if _, err := s.CreateEnvironment(p.ID, "develop", "standing", ""); err == nil {
		t.Fatal("want duplicate-name error")
	}

	got, err := s.GetEnvironment(p.ID, "develop")
	if err != nil || got.ID != e.ID { t.Fatalf("get: %v %+v", err, got) }

	if err := s.SetDefaultEnvironment(p.ID, e.ID); err != nil { t.Fatal(err) }
	got, _ = s.GetEnvironment(p.ID, "develop")
	if !got.IsDefault { t.Fatal("want default after set") }

	e2, _ := s.CreateEnvironment(p.ID, "staging", "standing", "")
	if err := s.SetDefaultEnvironment(p.ID, e2.ID); err != nil { t.Fatal(err) }
	got, _ = s.GetEnvironment(p.ID, "develop")
	if got.IsDefault { t.Fatal("old default must be cleared") }

	if err := s.DeleteEnvironment(e.ID); err != nil { t.Fatal(err) }
	if _, err := s.GetEnvironment(p.ID, "develop"); err != ErrNotFound {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}
```

- [ ] **Step 2: Run test, verify fail** — `go test ./internal/store/ -run TestEnvironmentCRUD` → FAIL (undefined `CreateEnvironment`).

- [ ] **Step 3: Add the table** to `schema.sql`:

```sql
CREATE TABLE IF NOT EXISTS environments (
  id              INTEGER PRIMARY KEY,
  project_id      INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  name            TEXT NOT NULL,
  k8s_namespace   TEXT NOT NULL,
  kind            TEXT NOT NULL DEFAULT 'standing' CHECK (kind IN ('standing','preview')),
  is_default      INTEGER NOT NULL DEFAULT 0,
  base_branch     TEXT NOT NULL DEFAULT '',
  source_branch   TEXT NOT NULL DEFAULT '',
  last_active_at  TEXT NOT NULL DEFAULT (datetime('now')),
  created_at      TEXT NOT NULL DEFAULT (datetime('now')),
  UNIQUE (project_id, name)
);
```

In `store.go` `migrate()`, after the ALTER loop, create the table for legacy DBs (schema.sql only runs on fresh opens for missing tables, which is fine, but add an explicit `CREATE TABLE IF NOT EXISTS environments (...)` exec mirroring the deployments index precedent so an already-open legacy DB gains it). Use the same DDL.

- [ ] **Step 4: Implement** `internal/store/environments.go` with the struct, `envNamespace`, and all methods. `CreateEnvironment` validates name via `validName`, computes `k8s_namespace` from the project name (look up the project), inserts, returns the row. Map `UNIQUE constraint failed` → a `validationErrorf("environment %q already exists", name)`. `SetDefaultEnvironment` runs `UPDATE environments SET is_default=0 WHERE project_id=?` then `UPDATE ... SET is_default=1 WHERE id=?` in a transaction. Scan helpers mirror `projects.go`.

- [ ] **Step 5: Run tests** — `go test ./internal/store/ -run TestEnvironmentCRUD` → PASS.

- [ ] **Step 6: Commit** — `git add internal/store/{schema.sql,store.go,environments.go,environments_test.go} && git commit -m "feat: environments table and store CRUD"`.

---

### Task 2: `environment_id` on apps + addons (add, backfill-ready)

**Files:**
- Modify: `internal/store/store.go` (`migrate()` ALTER loop)
- Modify: `internal/store/apps.go` (App struct + SELECT/scan + create)
- Modify: `internal/store/addons.go` (Addon struct + SELECT/scan + create)
- Test: `internal/store/apps_test.go`

**Interfaces:**
- Produces: `App.EnvironmentID int64`, `Addon.EnvironmentID int64`; new
  `func (s *Store) CreateAppInEnv(envID int64, name string, port int, kind, schedule string) (App, error)`
  and `func (s *Store) ListAppsInEnv(envID int64) ([]App, error)`,
  `func (s *Store) GetAppInEnv(envID int64, name string) (App, error)`.
  Keep the existing `project_id`-based methods working (they operate on the migrated default env; see Task 3) so nothing else breaks yet.

- [ ] **Step 1: Failing test** — extend `apps_test.go`:

```go
func TestAppInEnv(t *testing.T) {
	s := openTest(t)
	p, _ := s.CreateProject("proj")
	e, _ := s.CreateEnvironment(p.ID, "develop", "standing", "")
	a, err := s.CreateAppInEnv(e.ID, "api", 8080, "web", "")
	if err != nil { t.Fatal(err) }
	if a.EnvironmentID != e.ID { t.Fatalf("env id = %d", a.EnvironmentID) }
	list, _ := s.ListAppsInEnv(e.ID)
	if len(list) != 1 || list[0].Name != "api" { t.Fatalf("list = %+v", list) }
	got, err := s.GetAppInEnv(e.ID, "api")
	if err != nil || got.ID != a.ID { t.Fatalf("get: %v", err) }
}
```

- [ ] **Step 2: Run, verify fail.**

- [ ] **Step 3: Migrate columns** — add to the `migrate()` ALTER slice in `store.go`:

```go
{"apps", "environment_id", `ALTER TABLE apps ADD COLUMN environment_id INTEGER NOT NULL DEFAULT 0`},
{"addons", "environment_id", `ALTER TABLE addons ADD COLUMN environment_id INTEGER NOT NULL DEFAULT 0`},
```

Add the same columns to the `CREATE TABLE` bodies in `schema.sql`.

- [ ] **Step 4: Struct + methods** — add `EnvironmentID int64` to `App` and `Addon`. Add `environment_id` to every apps/addons SELECT column list and matching `&a.EnvironmentID` scan target (the three app SELECT sites: `GetApp`, `GetAppByID`, `ListApps`; the addon scan sites). Implement `CreateAppInEnv` (same as `CreateApp` but inserts `environment_id`; also set `project_id` to the env's project so legacy queries still work — look up `GetEnvironmentByID(envID).ProjectID`), `ListAppsInEnv`, `GetAppInEnv` (WHERE `environment_id=? AND name=?`).

- [ ] **Step 5: Run** `go test ./internal/store/` → PASS.

- [ ] **Step 6: Commit** — `refactor: add environment_id to apps and addons`.

---

### Task 3: Backfill migration — existing projects → production env

**Files:**
- Modify: `internal/store/store.go` (`migrate()` — add `backfillEnvironments(db)` after the ALTER loop)
- Test: `internal/store/migrate_test.go`

**Interfaces:**
- Produces: `func backfillEnvironments(db *sql.DB) error` — idempotent.

- [ ] **Step 1: Failing test** — `migrate_test.go`:

```go
func TestBackfillEnvironments(t *testing.T) {
	s := openTest(t)
	p, _ := s.CreateProject("legacy")
	// simulate a pre-environments app: insert directly with project_id, env 0.
	if _, err := s.DB().Exec(
		`INSERT INTO apps (project_id, name, source_type, port, kind, schedule, environment_id)
		 VALUES (?, 'api', 'tarball', 8080, 'web', '', 0)`, p.ID); err != nil {
		t.Fatal(err)
	}
	if err := backfillEnvironments(s.DB()); err != nil { t.Fatal(err) }

	prod, err := s.GetEnvironment(p.ID, "production")
	if err != nil { t.Fatalf("no production env: %v", err) }
	if !prod.IsDefault { t.Fatal("production must be default") }
	if prod.Namespace != "luncur-legacy" { t.Fatalf("prod ns = %q (must keep old ns)", prod.Namespace) }
	for _, n := range []string{"develop", "staging"} {
		if _, err := s.GetEnvironment(p.ID, n); err != nil { t.Fatalf("missing %s: %v", n, err) }
	}
	// app re-parented to production
	a, _ := s.GetAppInEnv(prod.ID, "api")
	if a.Name != "api" { t.Fatal("app not re-parented to production") }
	// idempotent
	if err := backfillEnvironments(s.DB()); err != nil { t.Fatal(err) }
	envs, _ := s.ListEnvironments(p.ID)
	if len(envs) != 3 { t.Fatalf("want 3 envs after re-run, got %d", len(envs)) }
}
```

- [ ] **Step 2: Run, verify fail.**

- [ ] **Step 3: Implement** `backfillEnvironments`: for each project lacking any `environments` row:
  1. Insert `production` with `k8s_namespace = <project.k8s_namespace>` (the existing ns), `is_default=1`, `base_branch='main'`.
  2. `UPDATE apps SET environment_id=? WHERE project_id=? AND environment_id=0` and same for `addons`.
  3. Insert `develop` (`base_branch='develop'`) and `staging` (`base_branch='staging'`), `k8s_namespace = luncur-<project>-<env>`, not default.
  4. `UPDATE projects SET default_env='production', preview_base_env='develop'` (Task 4 adds those columns — order Task 4 before this in `migrate()`).
  Guard the whole per-project block with `SELECT count(*) FROM environments WHERE project_id=?` == 0. Wire `backfillEnvironments(db)` into `migrate()` after Task 4's column adds.

- [ ] **Step 4: Run** `go test ./internal/store/` → PASS.

- [ ] **Step 5: Commit** — `feat: migrate existing projects to a production environment`.

---

### Task 4: `default_env` + `preview_base_env` on projects

**Files:**
- Modify: `internal/store/store.go` (`migrate()` ALTER loop — add both columns; place BEFORE the `backfillEnvironments` call)
- Modify: `internal/store/schema.sql` (projects table body)
- Modify: `internal/store/projects.go` (Project struct + SELECT/scan + setters)
- Test: `internal/store/projects_test.go`

**Interfaces:**
- Produces: `Project.DefaultEnv string`, `Project.PreviewBaseEnv string`;
  `func (s *Store) SetDefaultEnv(projectID int64, env string) error`,
  `func (s *Store) SetPreviewBaseEnv(projectID int64, env string) error`.
  New projects: `CreateProject` seeds nothing here (the server layer creates the 3 envs + sets defaults; see Task 8) — but the columns default to `'production'` / `'develop'` at the SQL level so a bare row is sane.

- [ ] **Step 1: Failing test** — assert a freshly created project reads `DefaultEnv=="production"` and `PreviewBaseEnv=="develop"`, and the setters round-trip. (Write the test; run; fail.)

- [ ] **Step 2–4:** add ALTER lines
  `{"projects", "default_env", `ALTER TABLE projects ADD COLUMN default_env TEXT NOT NULL DEFAULT 'production'`}` and
  `{"projects", "preview_base_env", `ALTER TABLE projects ADD COLUMN preview_base_env TEXT NOT NULL DEFAULT 'develop'`}`;
  add the columns to `schema.sql`; add struct fields + scan them in all three project SELECTs; implement the two setters (mirror `SetProjectGPUQuota`). Run → PASS.

- [ ] **Step 5: Commit** — `feat: project default and preview-base environment settings`.

---

# Phase 2 — Env-scoped routing, CRUD, and legacy aliases

Now the cluster/routing/handlers become env-aware. After this phase, users can create/list envs and everything still resolves to the default env for legacy callers.

### Task 5: `requireEnv` resolver + `hostFor` env suffix

**Files:**
- Modify: `internal/server/projects.go` (add `requireEnv`, `requireEnvWrite`)
- Modify: `internal/server/sync.go` (`hostFor`, `appURL`)
- Test: `internal/server/environments_test.go` (new), `internal/server/sync_test.go`

**Interfaces:**
- Produces:
  - `func (s *server) requireEnv(w, r, u, project, envName string) (store.Project, store.Environment, bool)` — resolves the project (membership-checked like `requireProject`), then the env; when `envName==""` uses `project.DefaultEnv`. 404s a missing env.
  - `func (s *server) requireEnvWrite(...)` — same with write-role check.
  - `func hostForEnv(app, env, defaultEnv, externalIP string) string` — returns `hostFor(app, ip)` when `env==defaultEnv`, else `hostFor(app+"-"+env, ip)`.

- [ ] **Step 1: Failing test** — `sync_test.go`:

```go
func TestHostForEnv(t *testing.T) {
	ip := "1.2.3.4"
	if got := hostForEnv("api", "production", "production", ip); got != "api.1-2-3-4.sslip.io" {
		t.Fatalf("default env host = %q", got)
	}
	if got := hostForEnv("api", "develop", "production", ip); got != "api-develop.1-2-3-4.sslip.io" {
		t.Fatalf("non-default host = %q", got)
	}
}
```

- [ ] **Step 2: Run, fail. Step 3:** implement `hostForEnv` (wrap `hostFor`); make `appURL` accept the env and call `hostForEnv(a.Name, env.Name, project.DefaultEnv, s.externalIP)`; implement `requireEnv`/`requireEnvWrite` in `projects.go` (reuse `requireProject`/`requireProjectWrite`, then `s.st.GetEnvironment`). **Step 4:** run → PASS. **Step 5:** commit `feat: env-aware host resolution and requireEnv resolver`.

---

### Task 6: `ensureEnvNamespace` — isolation keyed on the env namespace

**Files:**
- Modify: `internal/server/sync.go` (rename/adapt `ensureProjectNamespace` → `ensureEnvNamespace(ctx, env)`; keep a thin `ensureProjectNamespace` wrapper resolving the default env for any not-yet-migrated caller)
- Test: `internal/server/sync_test.go`

**Interfaces:**
- Produces: `func (s *server) ensureEnvNamespace(ctx context.Context, env store.Environment) error` — applies the same PodSecurity/NetworkPolicy/ResourceQuota to `env.Namespace` that `ensureProjectNamespace` applied to `p.Namespace`. Quota values read from the env's project for now (per-env quota is a later refinement; v1 reuses project quota).

- [ ] Steps: write a test asserting `ensureEnvNamespace` calls the fake kube `EnsureNamespaceWithPolicy` with `env.Namespace`; implement by moving the body of `ensureProjectNamespace` to take a namespace string + project (for quota), call it from both wrappers; run; commit `refactor: ensure namespaces per environment`.

---

### Task 7: Env-scoped app/addon reads in handlers (swap namespace source)

**Files:**
- Modify: `internal/server/apps.go` (`requireApp` gains an env; app handlers take env)
- Modify: every handler file using `p.Namespace` for app/addon/domain/deploy work: `apps.go, addons.go, build.go, certs.go, cron.go, forward.go, charts.go, argo.go, addonrestore.go, backup.go` (and others surfaced by grep)
- Test: existing server tests must stay green; add `environments_test.go` cases

**Interfaces:**
- Produces: `func (s *server) requireApp(w, p store.Project, env store.Environment, name string) (store.App, bool)` — looks up via `GetAppInEnv(env.ID, name)`.

- [ ] **Transformation rule (mechanical, apply precisely):** every current app-scoped handler flow that does `p, ok := requireProject(...)` then `a, ok := requireApp(w, p, name)` then uses `p.Namespace` becomes `p, env, ok := requireEnv(w, r, u, project, r.PathValue("env"))` then `a, ok := requireApp(w, p, env, name)` then uses `env.Namespace`. The `r.PathValue("env")` is `""` for legacy routes (Task 9 injects the default). Search for `p.Namespace` in each file and replace with `env.Namespace` at app-scoped sites (project-scoped sites — e.g. project delete — resolve the relevant env(s) explicitly). Keep changes behavior-neutral: with the default env, `env.Namespace == p.Namespace` (production) so tests pass unchanged.
- [ ] Steps: do it file-by-file, running `go test ./internal/server/` after each file, committing per file group (`refactor: env-scope <file> handlers`). This is the widest task — treat each file as its own review gate.

---

### Task 8: Environment CRUD API + project-create seeds 3 envs

**Files:**
- Create: `internal/server/environments.go` (handlers)
- Modify: `internal/server/server.go` (routes), `internal/server/projects.go` (`handleCreateProject` seeds envs)
- Test: `internal/server/environments_test.go`

**Interfaces:**
- Produces handlers: `handleListEnvs, handleCreateEnv, handleDeleteEnv, handleSetDefaultEnv, handleSetPreviewBase`; routes:
  `GET/POST /v1/projects/{project}/envs`, `DELETE /v1/projects/{project}/envs/{env}`,
  `PUT /v1/projects/{project}/envs/{env}/default`, `PUT /v1/projects/{project}/preview-base`.
- On project create: after `CreateProject`, create `develop`(base `develop`), `staging`(base `staging`), `production`(base `main`), then `SetDefaultEnvironment(production)` and `SetDefaultEnv="production"`, `SetPreviewBaseEnv="develop"`.

- [ ] Steps: TDD each handler (create env → 201 + row; duplicate → 400; delete default → 409 refuse; set-default reassigns; list). Refuse deleting an env with live apps unless `?force=1` (mirror project destroy). Deleting an env triggers namespace teardown (`DeleteNamespace(env.Namespace)`) + row delete; guard on `s.kube`. Commit `feat: environment CRUD API and default-env seeding on project create`.

---

### Task 9: Env-scoped routes + legacy aliases

**Files:**
- Modify: `internal/server/server.go` (register `/envs/{env}/...` variants; keep legacy `/apps/...`)
- Modify: `internal/server/apps.go` etc. (handlers read `r.PathValue("env")`, `""` → default)
- Test: `internal/server/environments_test.go`

**Interfaces:**
- Produces: for every app/addon/domain/deploy/scale/etc route under `/v1/projects/{project}/apps/...`, register a twin under `/v1/projects/{project}/envs/{env}/apps/...` bound to the SAME handler. The handler resolves env from `r.PathValue("env")` (empty on the legacy path). `requireEnv` maps `""` → `project.DefaultEnv`.

- [ ] **Step 1: Failing test** — deploy via the legacy path and via `/envs/production/...` both hit the production namespace; deploy via `/envs/develop/...` hits `luncur-<p>-develop`. **Steps 2-4:** register the twin routes (a small loop/helper if the mux allows, else explicit lines mirroring the existing block), implement `env := r.PathValue("env")` resolution in the shared `requireEnv`. Run full server tests → PASS. **Step 5:** commit `feat: env-scoped routes with legacy default-env aliases`.

---

### Task 10: CLI — `--env` flag + `luncur env` command group

**Files:**
- Modify: `internal/client/client.go` (env-scoped path builder + env methods)
- Modify: `internal/cli/*.go` (thread optional `--env` into app/deploy/addon/domain/scale/logs commands)
- Create: `internal/cli/env.go` (`luncur env list|create|rm|set-default|set-base`)
- Modify: `internal/cli/root.go` (register `envCmd`)
- Test: `internal/cli/env_test.go`, `internal/client/*_test.go`

**Interfaces:**
- Produces: client `EnvPath(project, env string) string` (env `""` → the `/apps` legacy base; non-empty → `/envs/<env>`); `ListEnvs/CreateEnv/DeleteEnv/SetDefaultEnv/SetPreviewBase`. CLI: a shared persistent `--env` string flag; commands pass it to the client path builder.

- [ ] Steps: TDD the client path builder (env `""` vs set); add `luncur env` subcommands mirroring `addon.go` structure; thread `--env` through the app/deploy/addon commands (default empty → server resolves default). Commit `feat: luncur --env flag and env command group`.

---

# Phase 3 — Multi-env usage end-to-end

### Task 11: Deploy/scale/logs/domains verified across a non-default env

**Files:**
- Test: `internal/server/environments_test.go` (integration-style with fake kube)
- Modify: any site still assuming the default namespace surfaced by the tests

**Interfaces:** none new — this task proves Phase 2 works end-to-end in `develop`.

- [ ] **Step 1: Failing/あれ integration test** — create project, create app in `develop`, deploy an image, assert: objects applied to `luncur-<p>-develop`, `appURL` returns `api-develop.<ip>.sslip.io`, scale/logs/domain-add all target the develop namespace, and the same app name can coexist in `production` and `develop` independently. **Steps:** fix any handler that still hard-resolves the default env; run; commit `test: end-to-end multi-environment app lifecycle`.
- [ ] **Step 2:** UI — env selector on the project page and app pages (a dropdown listing the project's envs; links carry `/envs/<env>`). Add `Env`/`Envs` to the relevant view models in `ui.go`, and an env chip on the app page. Read `DESIGN.md` first; reuse `chip`/nav patterns. Commit `feat: environment selector in the UI`.

---

# Phase 4 — Preview environments

### Task 12: Project-level build webhook + branch routing

**Files:**
- Modify: `internal/store/projects.go` (`webhook_secret` sealed column on projects) + `store.go` migrate
- Create: `internal/server/preview.go` (webhook handler + branch router)
- Modify: `internal/server/server.go` (route `POST /v1/projects/{project}/webhook`)
- Test: `internal/server/preview_test.go`

**Interfaces:**
- Produces: `func (s *server) handleProjectWebhook(w, r)` — verifies the project webhook HMAC (reuse the app-webhook verify helper), extracts the pushed branch, then `routeBranch(project, branch)`:
  - standing env with `base_branch == branch` → deploy that env's git apps (reuse the per-app deploy path). Touch env.
  - else → `ensurePreview(project, branch)` (Task 13).
  - PR-closed / branch-deleted payload → `teardownPreview` (Task 15).

- [ ] Steps: TDD branch→standing routing first (fake payload, assert the right env's apps deploy), then the fall-through calls a stubbed `ensurePreview`. Add the sealed `webhook_secret` project column + a `POST .../webhook/secret` generate route (mirror the app webhook). Commit `feat: project webhook with branch-to-environment routing`.

### Task 13: `sanitizeBranch` + preview env create (clone base specs)

**Files:**
- Create/modify: `internal/server/preview.go`
- Test: `internal/server/preview_test.go`

**Interfaces:**
- Produces:
  - `func sanitizeBranch(b string) string` — lowercase, `/`→`-`, strip chars outside `[a-z0-9-]`, collapse dashes, trim to ≤ (40 − len("<longest-app>-")) so `<app>-<env>` still fits a DNS label; ensure it passes `validName`.
  - `func (s *server) ensurePreview(ctx, p store.Project, branch string) (store.Environment, error)` — if a preview env named `sanitizeBranch(branch)` exists, return it (caller redeploys); else create env `kind='preview'`, `source_branch=branch`; ensure namespace; clone base env (`p.PreviewBaseEnv`) app specs via `CreateAppInEnv` copying kind/port/resources/replicas(capped)/health/git-source/internal/GPU, copy env vars (`ListEnv`→`SetEnv`), set `git_branch=branch` on git apps.

- [ ] **Step 1: Failing test** — `sanitizeBranch("feature/Fix_Login")=="feature-fix-login"`; `ensurePreview` clones the base env's app (same port/kind, git_branch set to the pushed branch) into `luncur-<p>-<sanitized>`. **Steps:** implement; run; commit `feat: preview environment creation cloning the base env`.

### Task 14: Preview addon seeding via dump→restore

**Files:**
- Modify: `internal/server/preview.go`
- Test: `internal/server/preview_test.go`

**Interfaces:**
- Produces: `func (s *server) clonePreviewAddons(ctx, base, preview store.Environment) []string` (warnings). For each addon in `base`: create the same-typed addon in `preview` (reuse `createAddon` core), then for postgres/redis: `dump, _, _ := s.dumpAddon(ctx, baseAddon)` → `s.restoreAddon(ctx, previewAddon, dump)`. minio/mlflow: create empty, warn. Per-addon failures degrade to warnings (partial preview beats none), mirroring backup.

- [ ] Steps: TDD with a fake execer capturing that a pg dump is piped into the preview addon's restore; implement; commit `feat: seed preview addon data from the base environment`.

### Task 15: Teardown — PR-close, idle TTL loop, manual

**Files:**
- Modify: `internal/server/preview.go` (teardown), add `StartPreviewReaper` loop
- Modify: `internal/server/server.go` (start the reaper; `preview_ttl_days` in `settableKeys`)
- Modify: `internal/server/settings.go` (`preview_ttl_days` validator, int ≥ 1)
- Test: `internal/server/preview_test.go`

**Interfaces:**
- Produces: `func (s *server) teardownPreview(ctx, p store.Project, env store.Environment) error` — delete namespace + env rows (cascade apps/addons/PVCs); `func (s *server) reapPreviews(ctx)` — for each `kind='preview'` env with `now - last_active_at > preview_ttl_days`, teardown; `func (s *server) StartPreviewReaper(ctx)` mirrors `StartBackups` (hourly tick).

- [ ] Steps: TDD `reapPreviews` (a preview with an old `last_active_at` is torn down; a fresh one is kept; the TTL setting is honored), and the PR-close webhook path → teardown. `TouchEnvironment` on every deploy so active previews survive. Commit `feat: preview teardown via PR close, idle TTL, and manual delete`.

### Task 16: CLI + UI for previews

**Files:**
- Create: `internal/cli/preview.go` (`luncur preview ls|create|rm`)
- Modify: `internal/cli/root.go`; `internal/client/client.go` (preview methods)
- Modify: `internal/server/environments.go`/`preview.go` (`GET/POST/DELETE /v1/projects/{project}/previews[/{name}]`)
- Modify: UI project page (list previews with their URLs + a delete button)
- Test: `internal/cli/preview_test.go`, `internal/server/preview_test.go`

- [ ] Steps: TDD the list/create/delete endpoints and client methods; add the CLI group (mirror `addon.go`); add a Previews card to the project page (read `DESIGN.md`; reuse `card`/`tbl`/`btn-ghost`). Manual `create <branch> --from <base>` calls `ensurePreview` with an explicit base. Commit `feat: preview environment CLI and UI`.

---

## Self-Review

- **Spec coverage:** data model (T1–4), migration/back-compat (T3), namespace/routing (T5–7), addressing+aliases (T9), env CRUD+defaults (T8), CLI (T10), multi-env usage+UI (T11), preview trigger (T12), create+clone (T13), addon seeding (T14), teardown/TTL (T15), preview CLI/UI (T16). All spec sections mapped.
- **Placeholder scan:** transformation rules in T7/T9 are precise (exact replace pattern + per-file gating), not vague — each names the files and the `p.Namespace → env.Namespace` rule. No TBDs.
- **Type consistency:** `Environment` fields, `envNamespace`, `CreateAppInEnv/GetAppInEnv/ListAppsInEnv`, `requireEnv`, `hostForEnv`, `ensureEnvNamespace`, `ensurePreview/sanitizeBranch/clonePreviewAddons/teardownPreview/reapPreviews` are defined once and reused with the same signatures throughout.
- **Phasing:** each phase leaves build/tests green and the default-env path byte-behavior-identical; Phase 1 is invisible to users, Phase 4 is additive.

## Notes for the executor

Tasks 7 and 9 are the widest and riskiest (the `p.Namespace → env.Namespace` swap across ~20 files). Treat each modified file as its own commit + test-run gate. Because with the default env `env.Namespace == project.Namespace`, the existing server test suite is the safety net — it must stay green through every file in Task 7 with zero test changes. If a test needs changing to pass, that's a signal the swap altered default-env behavior — stop and reconcile rather than editing the test.
