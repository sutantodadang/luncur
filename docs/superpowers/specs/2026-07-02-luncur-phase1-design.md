# luncur — Phase 1 (MVP) Design

Date: 2026-07-02
Status: Approved (design review with owner, 2026-07-02)

## Pitch

Empty VPS → running PaaS in under 2 minutes. Deploys as simple as Heroku,
with an escape hatch to the real Kubernetes objects when you need it.
One Go binary, SQLite, K3s.

**Positioning vs alternatives:**

- Heroku: simple, but total abstraction — power users hit a wall. luncur keeps
  the simplicity but lets you see and patch the real K8s objects, and your
  patches survive redeploys.
- Sailbox (thin K8s UI): powerful, but you must think in K8s from minute one.
  luncur's default path never shows you Kubernetes at all.
- Coolify/Dokploy: heavy runtimes (PHP/Node + Postgres + Redis). luncur's
  control plane is a single Go binary + SQLite, target < 50 MB RSS.

## Architecture

```
luncur (ONE Go binary):
├── luncur up          → installs K3s if missing, deploys luncur itself
│                        as a Deployment in the cluster
├── luncur serve       → API server (runs in-cluster)
│     ├── REST API + web UI (templ + HTMX + SSE, served from embed.FS)
│     ├── SQLite (modernc.org/sqlite, WAL mode, on a local-path PVC)
│     ├── client-go → K3s API (server-side apply, fieldManager=luncur)
│     └── embedded OCI registry (distribution/distribution as a library)
└── luncur <cmd>       → CLI client; talks to the REST API with a token
```

Runtime moving parts: K3s + one luncur Deployment + K3s's bundled Traefik.
No Postgres, no Node runtime, no external registry.

### Key components

| Component | Choice | Notes |
|---|---|---|
| Language | Go (single module) | `cmd/luncur` produces the one binary |
| K8s client | client-go | server-side apply everywhere, fieldManager=`luncur` |
| DB | modernc.org/sqlite | pure Go (no CGO), WAL mode; file on PVC |
| Web UI | templ + HTMX + SSE | embedded via `embed.FS`; no JS build step |
| Ingress | Traefik (K3s default) | IngressRoute or plain Ingress objects |
| Builder | BuildKit rootless Job + Nixpacks | custom `luncur/builder` image |
| Registry | embedded in `luncur serve` | distribution lib; blobs on the PVC |
| Default domains | `<app>.<ip>.sslip.io` | zero DNS setup for first deploy |

## Install story (`luncur up`)

Run on a fresh VPS as root (or with sudo):

1. Detect K3s. If missing, install via the official K3s install script
   (pinned version), with Traefik enabled (default).
2. Create namespace `luncur-system`, PVC (local-path), Deployment for
   `luncur serve`, Service, Ingress at `panel.<ip>.sslip.io`.
   The luncur image is built/published by our release pipeline; `luncur up`
   references the version matching the CLI.
3. Bootstrap: generate the initial admin user + password, print login URL
   and credentials once. Also mint an API token and write it to the local
   `~/.config/luncur/config` so the CLI on that machine works immediately.
4. Idempotent: re-running `luncur up` upgrades/repairs the install.

Public IP detection: ask the cluster (node ExternalIP), fall back to an
outbound probe, overridable with `--ip`.

## Deploy pipeline

Two source inputs in Phase 1 (git-push-over-SSH is Phase 2):

- **CLI tarball**: `luncur deploy` in an app directory. Uses `git archive`
  when in a git repo (respects .gitignore semantics), otherwise tars the
  directory (with a default exclude list). Uploads to the API.
- **Git URL**: app configured with a repo URL + branch (+ optional token for
  private repos, stored encrypted). Server clones on deploy trigger.

Pipeline (server side):

```
source tarball
  → create `deployments` row (status=building)
  → Build Job in namespace luncur-system, image luncur/builder
      • Dockerfile present?  → buildctl build with it
      • else                 → nixpacks plan + generate Dockerfile → buildctl
      • push image to the embedded registry: registry.luncur-system:5000/<app>:<deploy-id>
  → server renders manifests (Deployment, Service, Ingress) from app model
  → apply stored user overrides (see Escape hatch)
  → server-side apply, watch rollout
  → status=live, URL http://<app>.<ip>.sslip.io
```

- Build + rollout logs stream to CLI and UI via SSE.
- `luncur/builder` image: buildkitd rootless + nixpacks CLI + a small
  entrypoint script. Built and versioned by our release pipeline.
- Registry note: in-cluster pulls reference the registry via a Service
  hostname; K3s is configured (registries.yaml written by `luncur up`) to
  treat it as an insecure/internal registry.

## Escape hatch (the differentiator)

- Every app's K8s objects are **rendered** from the app model, then a stored
  set of **overrides** (strategic-merge patches, one per app+kind, kept in
  SQLite) is applied on top, then server-side applied to the cluster.
- `luncur app <name> --raw` → print the final rendered YAML.
- `luncur edit <name> <kind>` → opens the rendered YAML in $EDITOR; on save,
  luncur diffs it against the base render and stores the diff as the
  override. Applied on every subsequent deploy — user customizations
  survive redeploys.
- Conflicts: if a later base render conflicts with an override (e.g. field
  removed), surface a warning on deploy; override wins where mergeable.
- `--eject` (detach app from luncur management) is Phase 3.

## Data model (SQLite)

- `users` (id, email, password_hash, role: admin|member, created_at)
- `api_tokens` (id, user_id, hash, name, last_used_at)
- `projects` (id, name, k8s_namespace)
- `project_members` (project_id, user_id, role)
- `apps` (id, project_id, name, source_type: tarball|git, git_url, git_branch,
  git_token_enc, port, replicas, created_at)
- `deployments` (id, app_id, status: building|deploying|live|failed,
  image_ref, log_path, created_by, created_at)
- `env_vars` (app_id, key, value_enc) → materialized as a K8s Secret
- `domains` (app_id, hostname, tls: bool) — Phase 1: sslip.io rows only;
  custom domains + Let's Encrypt are Phase 2
- `overrides` (app_id, kind, patch_json, updated_at)
- `invites` (token, role, expires_at) — schema only in Phase 1; invite UI is
  Phase 2

Secrets at rest (env values, git tokens) encrypted with a key generated at
`luncur up`, stored as a K8s Secret in luncur-system.

Source of truth split: cluster state lives in etcd (K3s); SQLite holds
luncur's own metadata (users, apps, deploy history, overrides).

## Auth (Phase 1 scope)

- Multi-user schema from day 1 (users, roles, project membership).
- Admin bootstrap at `luncur up`; more users via `luncur user add` (CLI only
  in Phase 1 — invite links and user management UI are Phase 2).
- Web UI: session cookie (email+password login). CLI: bearer API token
  (`luncur login` prompts and stores it).
- Roles: `admin` (everything), `member` (only projects they belong to).

## CLI surface (Phase 1)

```
luncur up                      # install/upgrade on this machine
luncur login <server-url>      # store API token
luncur init                    # create app in current dir (writes luncur.toml: app name, project, port)
luncur deploy                  # tarball deploy of current dir
luncur status [app]            # app/deploy status
luncur logs <app> [-f]         # runtime logs (SSE)
luncur env set/unset/list <app> KEY=VAL
luncur scale <app> <replicas>
luncur destroy <app>
luncur app <name> --raw        # rendered K8s YAML
luncur edit <name> <kind>      # override editor
luncur user add <email>        # admin only
```

## Web UI surface (Phase 1)

Login → project list → app list → app detail (status, URL, deploy history,
live logs via SSE, env var editor, scale control, deploy-from-git-URL button,
"view raw YAML" read-only). Deliberately small; parity grows in Phase 2.

## Error handling

- Build failure → deployment status `failed`, full log stored; CLI/UI show
  the tail with the error highlighted.
- Rollout failure → watch detects stuck rollout, surface pod events +
  container crash logs, status `failed`; previous ReplicaSet keeps serving.
- API errors: consistent JSON error envelope; CLI renders human messages.
- `luncur up` failures are step-scoped and re-runnable (idempotent).

## Testing strategy

- **Unit + golden tests**: manifest renderer and override merge (the riskiest
  logic) get golden-file tests per app-model permutation.
- **Integration**: CI spins up k3d; test deploy pipeline end-to-end with a
  tiny sample app (Dockerfile and nixpacks paths).
- **Manual**: `luncur up` on a throwaway VM per release.

## Phasing (approved)

| Phase | Contents |
|---|---|
| **1 (this spec)** | `luncur up`, API + basic web UI, CLI (deploy tarball + git URL, logs, env, status, scale, destroy), nixpacks/Dockerfile builds, embedded registry, sslip.io routing, multi-user schema + `luncur user add`, escape hatch (`--raw`, `edit`) |
| 2 | git push receiver (SSH), custom domains + Let's Encrypt, invite links, YAML editor in web UI, rollback |
| 3 | one-click addons (Postgres/Redis), metrics/usage, backups, `--eject` |

Each later phase gets its own spec + plan.

## Out of scope (Phase 1)

Multi-node clusters, autoscaling, preview environments, teams beyond
project membership, billing, Windows containers, non-HTTP workloads
(TCP services), cron jobs.
