# luncur — Phase 3 Design

Date: 2026-07-03
Status: Approved (design review with owner, 2026-07-03)
Builds on: [2026-07-03-luncur-phase2-design.md](2026-07-03-luncur-phase2-design.md) (Phase 2 complete, Plans E-H)

## Goal

Make luncur apps self-sufficient in production: databases and caches on
demand, visibility into resource usage, state that survives a dead VPS,
and clean exits — all still one Go binary, zero new dependencies.

## Scope (approved)

| Plan | Contents |
|---|---|
| **I — addons** | Postgres/Redis instances per project, attachable to apps, env auto-injection; pods/exec plumbing |
| **J — metrics** | per-app CPU/memory via metrics.k8s.io + deploy counts; cert-manager expiry readback |
| **K — backups** | scheduled tar.gz of DB/key/addon dumps on the PVC + S3-compatible upload (stdlib SigV4) |
| **L — eject + registry GC** | one-way detach from management; retention-based image GC with blob reclamation |

Order I → J → K → L (K consumes I's exec plumbing; J/L otherwise independent).

## Global constraints

- Single Go module, one binary from `cmd/luncur`. **No new dependencies** —
  metrics via the dynamic client, pods/exec via client-go `remotecommand`
  (already in the tree), S3 via a stdlib SigV4 signer.
- Server-side apply everywhere, `fieldManager=luncur`. API error envelope
  unchanged. Conventional commits; full build/vet/test before every commit.
- Tests must not require a cluster or network: fake dynamic/typed clients,
  a `PodExecer` interface faked in tests, httptest fakes for S3 and the
  registry API. Real exec/S3 validated manually on the owner's VPS.

## Plan I — addons (Postgres / Redis)

### Model

- Per-project instances, per-app attachments:
  - `addons` (id, project_id, type postgres|redis, name, version,
    size_gb, creds_enc BLOB, created_at) — credentials generated at
    create, sealed with the existing sealer (same pattern as env_vars),
    and materialized into the addon's K8s Secret at provision time.
    Render-time env injection reads the sealed store copy, so `--raw`
    and offline renders keep working without a cluster round-trip.
  - `addon_attachments` (addon_id, app_id).
- CLI:
  - `luncur addon create <type> --project P [--name N] [--size 1]` —
    name defaults `<type>-<n>`.
  - `luncur addon add <type> --app A --project P` — sugar: create + attach.
  - `addon attach <name> <app>`, `addon detach <name> <app>`,
    `addon list --project P` (status column), `addon remove <name>
    [--force] [--keep-data]` — refuses while attached unless `--force`
    (which detaches all); `--keep-data` spares the PVC.
- UI: project page gains an Addons section (list, status, create form);
  app page lists attached addons with detach buttons + attach form.

### Provisioning

Rendered and server-side applied like apps, all in the project namespace,
labeled `app.kubernetes.io/managed-by: luncur`:

- StatefulSet, 1 replica: `postgres:16-alpine` or `redis:7-alpine`
  (version pinned per addon row; flag `--version` overrides tag).
- PVC per instance (default 1Gi, `--size` in Gi).
- Headless Service `addon-<name>`.
- Credentials Secret `addon-<name>-creds`, materialized from the sealed
  store copy: Postgres → user `app`, random password, db `app`; Redis →
  random `requirepass` password.

Status = StatefulSet readiness, shown in `addon list` and the UI.

### Injection

Attachment materializes env into the app's rendered Secret:

- Postgres: `DATABASE_URL=postgres://app:<pw>@addon-<name>.<ns>:5432/app`
- Redis: `REDIS_URL=redis://:<pw>@addon-<name>.<ns>:6379`

`renderApp` merges attachment env under the user's env vars — an explicit
user env var wins on key collision (collision surfaced as a warning in
`addon attach` output and the UI). Attach/detach re-syncs a live app.
Multiple addons of the same type attached to one app: the second and later
get suffixed keys (`DATABASE_URL_<NAME>`).

### Lifecycle

- Addons are never deleted implicitly. `luncur destroy <app>` drops the
  app's attachments only. `addon remove` deletes StatefulSet + Service +
  Secret (+ PVC unless `--keep-data`).
- pods/exec plumbing (used by Plan K dumps and Plan L GC) lands here: a
  `PodExecer` interface with a client-go `remotecommand` implementation on
  the kube client, faked in tests. Scoped ClusterRole gains `pods/exec`
  (create) — and `statefulsets` full verbs.

## Plan J — metrics

- Read `metrics.k8s.io/v1beta1` PodMetrics through the existing dynamic
  client (`gvrByKind` entry; ClusterRole read rule). No typed metrics
  client.
- `GET /v1/projects/{p}/apps/{a}/metrics` → per-app sums: cpu millicores,
  memory MiB (across pods labeled `app.kubernetes.io/name=<app>`), ready/
  desired replicas, total deploy count (DB). metrics-server absent → 200
  with `"available": false` (never an error; K3s bundles metrics-server).
- Surfaces: `luncur status <app>` gains `cpu:` / `memory:` lines;
  app page gets a stats row (server-rendered on page load — no polling).
- **cert-manager expiry readback** (Phase 2 deferral): the daily cert
  sweep also reads the TLS Secret for `external`-status domains under the
  cert-manager provider and fills `cert_expires_at` (status stays
  `external`).

## Plan K — backups

- `luncur backup create [--no-upload]` (admin) → `backups/<ts>.tar.gz` on
  the data PVC containing:
  - SQLite snapshot via `VACUUM INTO` (consistent while serving),
  - the sealer key file,
  - per-addon logical dumps via pods/exec: `pg_dump -Fc` (postgres),
    `redis-cli --rdb -` (redis) — one member file per addon.
- S3-compatible upload: settings `backup_s3_endpoint`, `backup_s3_bucket`,
  `backup_s3_prefix`, `backup_s3_access_key`, `backup_s3_secret_key`
  (sealed with the existing sealer). Uploader = stdlib AWS SigV4 signer +
  `net/http` PUT; unit-tested against known-answer signature vectors and
  an httptest fake.
- `backup list` (id, size, created, uploaded), `backup prune` — keep
  newest `backup_keep` (default 7), local and remote both pruned.
- Schedule: `backup_schedule` setting = `daily` | `off` (default off);
  goroutine started from serve like the cert-manager loop.
- `backups` table (id, path, size_bytes, uploaded, created_at).
- **Restore = documented runbook, not automated**: README gains exact
  steps (fresh `luncur up`, scale down, untar DB+key onto the PVC,
  restart, recreate addons, restore dumps via exec). `luncur restore` is
  explicitly out of scope — untested restore automation is worse than a
  precise runbook.

## Plan L — eject + registry GC

### --eject

- `luncur app eject <app> --project P` (interactive confirm; `--yes`) →
  sets `apps.ejected = 1`.
- Effects: deploy, scale, env, domains, overrides, rollback, git push, and
  sync all refuse with 409 `app_ejected`. Cluster objects untouched. The
  final rendered YAML is printed and saved to `data/ejected/<app>.yaml`.
- The app row remains (listed as "ejected" in CLI/UI) until
  `luncur destroy`, which for an ejected app deletes ONLY the DB rows —
  never the cluster objects. One-way; re-adopt out of scope.

### Registry GC

- Retention: keep the newest `registry_keep` (default 10) images per app
  PLUS any image referenced by a deployment that is `live` or is the
  app's newest row (rollback safety).
- Registry Deployment gains `REGISTRY_STORAGE_DELETE_ENABLED=true`.
- Sweep = weekly goroutine + manual `luncur registry gc`:
  1. list repositories/tags via the registry HTTP API,
  2. DELETE manifests outside retention,
  3. exec `registry garbage-collect /etc/docker/registry/config.yml` in
     the registry pod to reclaim blobs,
  4. log + report bytes reclaimed.
- Retention logic is a pure function tested against a fake registry
  httptest server.

## Data model changes (summary)

- New tables: `addons`, `addon_attachments`, `backups`.
- `apps` gains `ejected INTEGER NOT NULL DEFAULT 0` (migrate ALTER).
- New settings keys: `backup_s3_*`, `backup_schedule`, `backup_keep`,
  `registry_keep`.

## Error handling

- Addon create on missing kube → 503 (existing pattern); attach to an
  ejected app → 409 `app_ejected`; env key collision → warning, user env
  wins.
- Backup: addon dump failure marks the archive member missing but
  completes the backup with a warning list (partial backup better than
  none); S3 failure keeps the local archive and surfaces the error.
- GC: registry API/exec errors abort the sweep with a log line; nothing
  is deleted unless the retention list computed successfully.
- Metrics: any metrics API error → `"available": false`, never a 5xx.

## Testing strategy

- Unit + golden: addon manifests (StatefulSet/Service/Secret/PVC), env
  injection & collision rules, retention pure function, SigV4
  known-answer tests, eject-refusal matrix.
- Fakes: `PodExecer` interface (exec paths), httptest S3 + registry,
  fake dynamic client PodMetrics.
- Manual on owner's VPS: addon create → app connects; backup → S3 object
  appears; restore runbook executed once end-to-end; GC reclaims bytes;
  eject leaves the app serving.

## Out of scope (Phase 3)

Wildcard/DNS-01 certs, invite email delivery, automated restore, addon
version upgrades/major migrations, multi-replica/HA addons, addon metrics,
re-adopting ejected apps, backup encryption (archives contain the sealer
key — the S3 bucket is the trust boundary; documented in README).
