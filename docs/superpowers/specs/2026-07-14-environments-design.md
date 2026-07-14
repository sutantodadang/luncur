# luncur — Environments + Preview Environments Design

Date: 2026-07-14
Status: Approved (design review with owner, 2026-07-14)

## Pitch

Give every project first-class **environments** — `develop`, `staging`,
`production` out of the box, plus custom ones — each an isolated set of apps
and addons in its own Kubernetes namespace. On top of that, **preview
environments**: push a branch, get an ephemeral copy of the app set (seeded
with real data) at its own URL; close the PR (or let it idle out) and it tears
itself down.

Today `project` **is** a namespace: a project owns apps, addons, domains, and a
per-namespace NetworkPolicy / ResourceQuota / PodSecurity boundary. An
environment is exactly that same unit. So this design keeps every piece of the
per-namespace machinery unchanged and inserts an `environment` layer between
`project` and `app`; `project` becomes a pure grouping entity.

## Decisions (from design review)

1. **App model:** apps are *independent per environment* — `develop/backend`
   and `staging/backend` are separate app rows. No shared app identity, no
   built-in promotion in v1.
2. **Namespace:** each environment owns a namespace `luncur-<project>-<env>`.
   All existing per-namespace features (isolation, quota, PodSecurity, addons)
   are reused verbatim, keyed on the env namespace.
3. **Scope:** one combined spec — standing environments **and** preview
   environments. Implementation may still be phased (see Plan).
4. **Env creation:** a new project auto-creates `develop`, `staging`,
   `production`; users may add/remove custom standing envs. One env is the
   project **default** (seeded `production`).
5. **Routing:** `production` keeps the bare `<app>.<domain>` host (existing URLs
   survive migration); non-prod uses `<app>-<env>.<domain>`; preview uses
   `<app>-<sanitized-branch>.<domain>`. Custom domains attach per app-per-env
   exactly as today.
6. **Addressing:** REST paths gain an `/envs/{env}` segment; the CLI gains an
   optional `--env` flag defaulting to the project's `default_env`. Legacy
   `/v1/projects/{project}/apps/...` paths remain as aliases resolving to the
   default env.
7. **Preview lifecycle:** webhook-driven create (clone base env app specs + build
   the branch), auto-teardown on PR-close / branch-delete / idle-TTL, plus manual.
8. **Preview addons:** each preview provisions its **own** addon instances,
   **seeded from the base env's data** by reusing the existing
   `dumpAddon`→`restoreAddon` path.

### Defaulted (approved)

- Preview **base env** = per-project `preview_base_env`, default `develop`.
- **Standing branch→env mapping** via each env's `base_branch` (e.g. `main`→
  production, `develop`→develop); a pushed branch with no standing mapping →
  preview.
- **Preview TTL** = 7 idle days, install setting `preview_ttl_days`.

## Non-goals (v1)

- Cross-env promotion / shared app identity (apps are independent per env).
- Copying **custom domains** into preview envs (previews use the generated
  sslip.io host only).
- Preview envs for non-git (image/tarball) apps — preview requires a git source
  to build a branch.
- minio/mlflow **data** seeding into previews (their addon *instances* are
  cloned empty; only postgres/redis data is seeded, matching what
  `dumpAddon`/`restoreAddon` supports).

---

## Data model

### New table `environments`
| column | type | notes |
|---|---|---|
| `id` | INTEGER PK | |
| `project_id` | INTEGER FK → projects | ON DELETE handled by app/env teardown, not bare cascade |
| `name` | TEXT | DNS-1123 label; unique per project |
| `k8s_namespace` | TEXT | `luncur-<project>-<env>`; for the migrated prod env, the *existing* `luncur-<project>` value |
| `kind` | TEXT | `standing` \| `preview` |
| `is_default` | INTEGER | exactly one per project |
| `base_branch` | TEXT | standing env's git branch trigger; "" if none |
| `source_branch` | TEXT | preview only: the branch that spawned it |
| `last_active_at` | TEXT | bumped on every deploy; drives idle-TTL teardown |
| `created_at` | TEXT | |

Unique index `(project_id, name)`. Unique partial index enforcing one
`is_default` per project.

### Changed tables
- `apps`: `project_id` → **`environment_id`** (FK → environments). App name
  becomes unique per **environment**. Every existing app-scoped query re-keys on
  `environment_id`. `GetProjectByID`-style lookups that need the project walk
  `env.project_id`.
- `addons`: gains `environment_id` (was `project_id`); addons are namespace-
  scoped, so they belong to an env. Same re-key.
- `projects`: gains `default_env TEXT` (seeded `production`) and
  `preview_base_env TEXT` (default `develop`). The project **loses** its own
  `k8s_namespace` as a live target — the column may remain for migration but is
  no longer used to place objects.

### Migration (backward-compatible, no object moves)
For each existing project on `store.Open`/`migrate`:
1. Create an `environments` row `name='production'`, `kind='standing'`,
   `is_default=1`, `k8s_namespace` = the project's existing `luncur-<project>`
   namespace, `base_branch='main'`.
2. Re-parent every `apps` and `addons` row of that project to the new production
   env (`environment_id`).
3. Create empty `develop` (base, `base_branch='develop'`) and `staging`
   (`base_branch='staging'`) env rows; their namespaces are **not** created until
   the first deploy into them (lazy — avoids empty namespaces).
4. Set `projects.default_env='production'`, `preview_base_env='develop'`.

Idempotent: guarded by "does this project already have any environments row".
Existing cluster objects never move; the production env simply *is* the old
namespace. Existing app URLs are unchanged (bare host on production).

---

## Kubernetes / routing

- **Namespace ensure:** the current `ensureProjectNamespace(ns)` becomes
  `ensureEnvNamespace(env)` operating on `env.k8s_namespace`; it applies the same
  labels, PodSecurity, NetworkPolicy, and ResourceQuota. Called lazily on first
  deploy per env.
- **Teardown:** deleting an env deletes its namespace (cascading its apps,
  addons, PVCs) and its rows — the existing `DeleteNamespace` + per-row teardown
  path, scoped to the env.
- **Hostname (`hostFor`):** gains the env. Rule:
  - env is the project default (production) → `<app>.<ip>.sslip.io` (unchanged).
  - else → `<app>-<env>.<ip>.sslip.io` (env is already a DNS label).
  - preview env name = sanitized branch, so preview hosts fall out naturally as
    `<app>-<branch>.<ip>.sslip.io`.
  App names are unique per env, so within an env the host is unambiguous; the
  env suffix disambiguates across envs. (Cross-*project* host collision is a
  pre-existing property of the flat sslip.io scheme and is unchanged here.)
- **Quotas:** GPU/CPU/mem quotas move to the env (per namespace). Project-level
  aggregate quota is out of scope for v1 (each env enforces its own).

---

## API & CLI surface

### REST
- New canonical paths: `/v1/projects/{project}/envs/{env}/apps/{app}/...` for
  every current app/deploy/domain/env-var/addon/volume/scale/etc. route.
- New env routes: `POST/GET /v1/projects/{project}/envs`,
  `GET/DELETE /v1/projects/{project}/envs/{env}`,
  `PUT /v1/projects/{project}/envs/{env}/default`,
  `PUT /v1/projects/{project}/preview-base`.
- Preview routes: `GET /v1/projects/{project}/previews`,
  `DELETE /v1/projects/{project}/previews/{name}`,
  `POST /v1/projects/{project}/previews` (manual create `{branch, from}`).
- **Legacy alias:** the existing `/v1/projects/{project}/apps/...` handlers are
  kept, resolving `{env}` = the project's `default_env`, then delegating to the
  env-scoped handler. One thin resolver, no duplicated logic.

### CLI
- Global optional `--env` on app/deploy/addon/domain/scale/logs commands;
  unset → project `default_env`. A resolver in the client injects the segment.
- `luncur env`: `list`, `create <name> [--base-branch b]`, `rm <name>`,
  `set-default <name>`, and `set-base <name>` (preview base).
- `luncur preview`: `ls`, `create <branch> [--from <base>]`, `rm <branch>`.

---

## Preview environments

### Trigger (webhook)
A **project-level** build webhook (project webhook secret) receives branch push
events. On push to branch `B`:
1. If any standing env in the project has `base_branch == B` → deploy that env's
   git apps from `B` (normal build path, per app). Bump `last_active_at`.
2. Else → **preview flow**:
   - Compute env name = `sanitize(B)` (lowercase, `/`→`-`, strip invalid,
     truncate to fit the namespace label budget, dedupe on collision).
   - If a preview env for `B` already exists → deploy `B` into it (update), bump
     `last_active_at`, done.
   - Else create it (below).

Non-git projects and branches with no buildable git app are ignored (logged).

### Preview creation
1. Create `environments` row `kind='preview'`, `source_branch=B`, non-default.
2. Ensure its namespace.
3. Clone the **base env** (`preview_base_env`) app specs: for each app in the
   base env, create a matching app in the preview env with the same kind, port,
   resources, replicas (capped low for previews), health path, git source, env
   vars (copied from base), and internal/GPU flags. Git apps get `git_branch=B`.
4. Clone base env **addons**: for each base addon, provision a fresh instance in
   the preview namespace, then seed its data by `dumpAddon(base addon)` →
   `restoreAddon(preview addon, dump)` (postgres/redis only; minio/mlflow cloned
   empty). Wire app→addon env injection the same way create-time attachment does.
5. Build + deploy each git app from `B`.

### Teardown
Any of:
- Webhook PR-closed / branch-deleted event for `B`.
- Idle: `now - last_active_at > preview_ttl_days` (install setting, default 7),
  checked by a periodic loop (mirror `StartBackups`/cert loops).
- Manual `luncur preview rm <name>` / DELETE route.

Teardown deletes the env namespace (cascades apps/addons/PVCs) and the rows.

---

## Error handling

- Env create with a duplicate name → 409-style validation error.
- Deleting the default env is refused (must reassign default first).
- Deleting the last env / an env with running apps → same confirm/force
  semantics as project/app destroy today.
- Preview create is best-effort per step and logs; a failed addon seed degrades
  to an empty addon with a warning rather than aborting the whole preview
  (partial preview beats none — mirrors backup's per-addon warning policy).
- Legacy alias when a project has no `default_env` (shouldn't happen post-
  migration) → 500 with a clear log.

## Testing

- **Migration:** existing project → one production env owning the old namespace;
  apps + addons re-parented; develop/staging rows created; no app URL change;
  idempotent on re-run.
- **Env CRUD:** create/list/delete, unique-per-project, single-default invariant,
  default reassignment, refuse-delete-default.
- **Namespace/routing:** `ensureEnvNamespace` applies isolation/quota/PodSecurity;
  hostFor yields bare host for default env, `-<env>` suffix otherwise.
- **Addressing:** env-scoped routes; legacy alias resolves to default env and
  hits the same handler.
- **Preview:** webhook routes standing-branch → standing deploy, unmapped branch
  → preview create (specs cloned, addons seeded via dump/restore), repeat push →
  update-in-place, PR-close/TTL/manual → teardown deletes namespace + rows.
- Mirror existing store/server test harnesses (fake kube + execer, httptest).

## Rollout / phasing (implementation may split the PR internally)

1. Schema + migration + store env layer (no behavior change: everything runs in
   the migrated default env).
2. Env-scoped namespace/routing/hostFor + env CRUD API/CLI + legacy aliases.
3. Standing multi-env usage (create/deploy into develop/staging).
4. Preview envs: project webhook branch routing, clone+seed, teardown loop, CLI.

Each phase keeps `go build`/`go test` green and the default-env path behaving
exactly as today.
