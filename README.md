# luncur

Tiny self-hosted PaaS on K3s. One Go binary, SQLite, deploys as simple as
Heroku — with an escape hatch to the real Kubernetes objects.

Status: Phase 3 complete (Plans I-L) — addons, metrics, backups, eject +
registry GC, on top of Phase 2 (Plans E-H: git push deploys, custom domains
+ TLS, rollback + hardening, invites + user admin + the web YAML editor)
and Phase 1's PaaS core (Plans A-D). Working today:

## Install

On a fresh Linux VPS:

```sh
curl -sfLo luncur https://github.com/sutantodadang/luncur/releases/latest/download/luncur-linux-amd64
chmod +x luncur
sudo ./luncur up
```

`luncur up` is a one-command installer: it installs K3s (pinned version),
writes the containerd `registries.yaml` mirror for the in-cluster registry,
applies luncur's own system infrastructure (namespace, registry, PVCs) and
its own Deployment/Service/Ingress, waits for the rollout, and logs the CLI
in. On first run it prints the generated admin email + password — **this is
shown once, store it now**. Re-running `luncur up` is safe: every step is
skip-or-repair (server-side apply), so it's also how you upgrade or repair
an install.

Flags:
- `--ip` — public IP to advertise (default: detected from the node's
  `ExternalIP`, falling back to `InternalIP`)
- `--image` — luncur server image (default: `ghcr.io/sutantodadang/luncur:<version>`)
- `--kubeconfig` — point at an existing cluster instead of installing K3s
  (skips the K3s/registries host steps)

**Server (manual, e.g. for local dev):**
```sh
luncur serve --db luncur.db \
  --listen :8080 \
  --bootstrap-admin admin@example.com:password \
  --kubeconfig /path/to/kubeconfig \
  --external-ip 10.0.0.1 \
  --secret-key-file luncur.key \
  --data-dir /var/lib/luncur \
  --builder-image luncur/builder:latest \
  --registry-host registry.luncur-system:5000
```

**Auth:**
```sh
luncur login http://localhost:8080
luncur whoami
luncur user add teammate@example.com --password ... [--role admin|member]

# Invite teammates instead of setting a password for them (admin only)
luncur invite create [--role admin|member] [--email addr]  # prints a one-time /ui/register link; --email sends it via SMTP
luncur invite list                           # TOKEN, ROLE, EXPIRES, USED
luncur invite revoke <token>
```

**API tokens:**
```sh
luncur token list           # id, name, created, last used, expires
luncur token revoke <id>    # revoke a token — if it's the browser's own
                             # session cookie, that login is logged out too
```

**Projects & apps:**
```sh
luncur project create myproj
luncur project list
luncur project add-member myproj member@example.com

luncur app create myapp --project myproj --port 8080
luncur app list --project myproj
luncur app info myapp --project myproj
luncur app raw myapp --project myproj
```

**Deployment & scaling:**
```sh
luncur deploy myapp --project myproj --image my.registry/my/image:tag
luncur scale myapp --project myproj --replicas 3
luncur destroy myapp --project myproj
```

**Rollback:**
```sh
luncur rollback myapp --project myproj               # back to the previous live deploy
luncur rollback myapp --project myproj --deploy 12   # back to a specific deployment id
```

Rolling back redeploys an earlier deployment's image directly — no rebuild —
and records the new deployment row's lineage, shown in the web UI's Deploys
table as "(rollback of N)". The app page also has a `rollback` button on
every history row except the newest and any row with no image. Only images
hosted in luncur's embedded registry are HEAD-checked before rolling back
(a 409 naming the image if it's gone); externally-hosted image refs (e.g.
`docker.io/...`) are assumed present since luncur has no credentials to
verify them.

**Source build & deployment:**
```sh
# Initialize an app config in the current directory
luncur init

# Deploy from local source (tars cwd, uploads, builds in cluster, streams to completion)
luncur deploy myapp --project myproj

# Deploy from git repository (registering a git-source app)
luncur app create myapp --project myproj --port 8080 \
  --git-url https://github.com/user/repo.git --branch main
luncur deploy myapp --project myproj

# Follow build logs for a specific deploy
luncur logs myapp --project myproj --deploy 1 -f

# Stream runtime (pod) logs — omit --deploy
luncur logs myapp --project myproj -f
```

**Deploy with git push:**
```sh
# Once per machine: register your SSH public key
luncur ssh-key add            # picks up ~/.ssh/id_*.pub; or pass a path
luncur ssh-key list
luncur ssh-key remove <id>

# Once per repo: add the luncur remote
git remote add luncur ssh://git@<ip>:30022/myproj/myapp.git

# Deploy
git push luncur main
```

The push streams the whole build into your `git push` output (as
`remote:` lines) and prints the app URL when it goes live. Only a push to
the app's configured branch (default `main`) triggers a deploy; the
receiver is push-only (`git pull`/`clone` from luncur is rejected) and no
repository is stored server-side — each push is archived straight into the
same build pipeline `luncur deploy` uses.

**Environment & editing:**
```sh
luncur env set myapp KEY=value --project myproj
luncur env unset myapp KEY --project myproj
luncur env list myapp --project myproj
luncur edit myapp Deployment --project myproj
```

**Status:**
```sh
# List apps in a project (name + URL)
luncur status --project myproj

# Show one app's status (replicas, image, URL, live cpu/memory, deploy count)
luncur status myapp --project myproj
```

Live cpu/memory come from the cluster's `metrics-server` (K3s bundles it by
default); if it's not installed or unreachable, `status` prints `metrics:
unavailable` instead — the deploy count is always shown regardless.

## Web UI

After `luncur up`, the panel is served at `http://panel.<ip>.sslip.io/ui/`
(login with the admin credentials printed by `luncur up`, or any user added
via `luncur user add` or an invite). From there you can browse projects and
apps, scale and edit env vars, trigger a deploy, roll back to a previous
deploy, and watch build/runtime logs stream live via Server-Sent Events — no
CLI required. Each app page shows a stats line (cpu/memory when
`metrics-server` is available, ready/desired replicas, deploy count) under
the title.

Admins get a **users page** (`/ui/users`) to list every user (email, role,
created, active token count), delete a user, and mint invites — each invite
is a single-use, 7-day link (`/ui/register?token=...`) shown as a copyable
field; anyone who opens it registers their own email/password and is logged
straight in with the invite's role. The invite form takes an optional email
address: when SMTP is configured (see below) the link is mailed and a note
reports the outcome; the copyable link works either way.

Every app page also has a **YAML override editor**: `edit: Deployment ·
Service · Ingress` links open the currently-rendered manifest for that kind
in a textarea. Saving diffs your edit against a fresh base render and stores
the result as the same strategic-merge-patch override `luncur edit` writes
— so the change survives every future redeploy, and re-syncs a live app
immediately. Invalid YAML re-shows the editor with the error and your text
intact, never discarding the edit.

## Custom domains & TLS

```sh
luncur domain add myapp www.example.com --project myproj
luncur domain list myapp --project myproj
luncur domain remove myapp www.example.com --project myproj
luncur domain retry myapp www.example.com --project myproj   # builtin provider only
```

Point a DNS A record for the hostname at the server's advertised IP (the
`--ip` luncur was installed with, printed by `luncur up`). `domain add`
checks this immediately and returns a warning — shown once in the CLI
output and, in the web UI, above the Domains table — if the hostname
doesn't resolve there yet; the domain is still created, since DNS often
lands after the request.

TLS certs come from one of three providers, chosen with `luncur up
--cert-provider <name>` (fixed for the life of the install; re-run `luncur
up --cert-provider ...` to change it):

- `builtin` (default) — luncur runs its own ACME (Let's Encrypt) HTTP-01
  client: certs are requested automatically per domain, stored as
  Kubernetes Secrets, and renewed within 30 days of expiry. Status
  (`none → pending → issued`, or `failed` with an error) shows in `domain
  list` and the web UI; `luncur domain retry` re-kicks a stuck domain.
- `traefik` — `luncur up --cert-provider traefik` delegates TLS
  termination and ACME to K3s's bundled Traefik (K3s-only).
- `cert-manager` — `luncur up --cert-provider cert-manager` delegates via a
  `ClusterIssuer` to an already-installed cert-manager.

Both `traefik` and `cert-manager` issue certs themselves — luncur just
annotates the Ingress and marks the domain `external` rather than tracking
issuance state. For `cert-manager`, the daily cert sweep additionally reads
the issued cert's expiry back from the TLS Secret cert-manager maintains, so
`domain list` and the web UI show a real `cert_expires_at` once cert-manager
has issued it.

Set the ACME account email (used by the `builtin` provider and passed to
`traefik`/`cert-manager`'s issuer config) with:

```sh
luncur config set acme_email you@example.com
```

## Addons (Postgres/Redis)

```sh
luncur addon create postgres --project myproj                     # provision, unattached
luncur addon create redis --project myproj --name cache --version 7 --size 5

luncur addon add postgres --app myapp --project myproj             # create + attach in one step
luncur addon attach db1 myapp --project myproj                     # attach an existing addon
luncur addon detach db1 myapp --project myproj

luncur addon list --project myproj                                 # NAME, TYPE, VERSION, READY, ATTACHED
luncur addon upgrade db1 --project myproj --version 17              # in-place, rolling restart; PVC untouched
luncur addon remove db1 --project myproj                            # 409 if still attached
luncur addon remove db1 --project myproj --force                    # remove despite attachments
luncur addon remove db1 --project myproj --force --keep-data        # remove but keep the PVC
```

`addon upgrade` swaps the StatefulSet's image tag and SSA-applies it — a
rolling restart on the same PVC. Every upgrade response carries the
warning *"major version DB upgrades may require manual migration — take a
backup first."*: luncur does not run `pg_upgrade` or any engine-level data
migration.

Addons are project-level Postgres/Redis instances (a StatefulSet + PVC +
headless Service + credentials Secret, rendered the same way apps are) that
attach to one or more apps in the same project. `addon create` provisions
without attaching; `addon add` is sugar for create-then-attach. Names
default to `<type><n>` (e.g. `postgres1`, `postgres2`) and versions default
to `postgres:16` / `redis:7` when not given.

Attaching an addon injects a connection URL into the app's env at render
time — `DATABASE_URL` for postgres, `REDIS_URL` for redis — with no extra
cluster read needed. If the app already has that env var set (`luncur env
set`), the user's value always wins; the addon's value is silently skipped
and `attach` prints a warning naming the collision instead of erroring.
Attaching a second addon of the same type suffixes the key with the addon's
name (e.g. `DATABASE_URL_POSTGRES2`), so multiple databases can coexist on
one app.

Addons are never deleted implicitly — destroying an app just detaches its
addons (the underlying instance and its data survive). `addon remove`
refuses to delete an addon that's still attached to any app unless `--force`
is passed, and deletes the PVC along with the StatefulSet/Service/Secret
unless `--keep-data` is also passed.

The web UI mirrors all of this: each project page has an Addons table
(create form + per-row remove-with-force) and each app page has an attached-
addons list (detach buttons) plus an attach form listing the project's
addons.

## Backups

`luncur backup create` (admin) snapshots luncur's whole state into a
tar.gz under `backups/` on the data PVC: a consistent SQLite snapshot
(`VACUUM INTO`), the sealer key, and one logical dump per addon
(`pg_dump -Fc` / redis `SAVE`, taken via pods/exec — credentials never
leave the pod's environment). A failing addon dump becomes a warning; the
backup still completes.

```sh
luncur backup create [--no-upload]
luncur backup list
luncur backup prune            # keep newest backup_keep (default 7)
```

Off-box uploads go to any S3-compatible bucket (AWS/R2/minio/B2) — a
built-in SigV4 client, no SDK:

```sh
luncur config set backup_s3_endpoint https://<s3-endpoint>
luncur config set backup_s3_bucket   my-backups
luncur config set backup_s3_access_key AKIA...
luncur config set backup_s3_secret_key ...   # write-only: reads show "(set)"
luncur config set backup_s3_prefix   luncur  # optional
luncur config set backup_schedule    daily   # or off (default)
luncur config set backup_keep        7
```

With `backup_schedule daily`, the server takes and prunes backups
automatically. An upload failure keeps the local archive and surfaces a
warning.

**Invite email (SMTP):**

```sh
luncur config set smtp_host mail.example.com   # unset = invite emails disabled
luncur config set smtp_port 587                # optional, default 587
luncur config set smtp_user luncur@example.com # optional; enables PLAIN auth
luncur config set smtp_pass ...                # write-only: reads show "(set)"
luncur config set smtp_from luncur@example.com # optional, defaults to smtp_user
```

STARTTLS is used when the server offers it. A send failure (or
unconfigured SMTP) never blocks invite creation — the API returns
`emailed:false` plus a warning, and the invite link can be copied as
before.

**Security note:** archives contain the sealer key — whoever can read a
backup can unseal your env vars and addon credentials. The S3 bucket is
the trust boundary; scope its access accordingly.

### Restore runbook

Restore is deliberately a documented procedure, not a command:

1. Provision the new VPS: `luncur up` (fresh install, any admin password —
   it will be replaced by the restored DB).
2. Stop the server so SQLite is quiescent:
   `kubectl -n luncur-system scale deploy/luncur --replicas=0`.
3. Copy the backup archive onto the node and untar it. The data PVC is a
   `local-path` volume on the node — find it with
   `kubectl -n luncur-system get pvc luncur-data -o jsonpath='{.spec.volumeName}'`
   and look under `/var/lib/rancher/k3s/storage/<volume>/`.
4. Replace `luncur.db` and `luncur.key` on the PVC with the archive's
   copies.
5. Start the server: `kubectl -n luncur-system scale deploy/luncur --replicas=1`,
   then `luncur login` with the restored credentials.
6. Re-create each addon (`luncur addon create ...` with the same names) so
   the StatefulSets exist, then restore data into them:
   - Postgres: `kubectl -n <project-ns> exec -i addon-<name>-0 -- sh -c
     'PGPASSWORD="$POSTGRES_PASSWORD" pg_restore -U "$POSTGRES_USER" -d "$POSTGRES_DB" --clean' < addons/<project>-<name>.pgdump`
   - Redis: scale the addon StatefulSet to 0, copy the `.rdb` onto its PVC
     as `dump.rdb`, scale back to 1.
7. Redeploy apps (`luncur deploy` / `git push`) and verify with
   `luncur status`.

## Ejecting an app

```sh
luncur app eject myapp --project myproj [--yes]
luncur app adopt myapp --project myproj    # reverse it later
```

Ejecting detaches an app from luncur's management (reversible via
`app adopt`, below). luncur renders the app's final manifest (current overrides plus
its latest image), prints the YAML to stdout, and archives a copy under
`data/ejected/<project>-<app>.yaml` on the server. From then on every
mutation — deploy, scale, env, domains, overrides, rollback, addon
attach/detach, and further `git push` — is refused with `409 app_ejected`;
reads (status, logs, metrics, raw YAML, the app page) keep working exactly
as before. The Kubernetes objects luncur rendered keep running untouched:
ejecting doesn't delete or modify anything in the cluster, it only stops
luncur from touching it further. `luncur destroy` on an ejected app removes
luncur's own records (DB rows) only, leaving the running objects in place.
Ejected apps are marked `(ejected)` in `luncur app list` and with an
"ejected" badge in the web UI, which also hides the now-inert
scale/deploy/env/domains/rollback/edit forms in favor of a one-line note.

`luncur app adopt` reverses eject: it clears the ejected flag and
re-applies luncur's rendered state onto the running objects (reclaiming
`fieldManager=luncur`). Any manual drift made to those objects while
ejected is overwritten. The web UI's ejected note carries an adopt button
that does the same; after adopting, the normal management UI returns.

Without `--yes`, `app eject` asks you to type the app's name back to
confirm before doing anything irreversible.

## Registry GC

```sh
luncur registry gc   # admin only
```

luncur's embedded registry accumulates one image per deploy; `registry gc`
reclaims that storage with a retention sweep. For each app, the newest
`registry_keep` images (default 10) are kept, plus whichever image is
currently live and whichever is newest, regardless of position; everything
else — including whole repositories with no matching app left in the DB —
is deleted from the registry's manifest catalog. `registry
garbage-collect` then runs inside the registry pod to reclaim the
underlying blob storage. Change the retention count with:

```sh
luncur config set registry_keep 20
```

A sweep also runs automatically once a week. The registry container's
`REGISTRY_STORAGE_DELETE_ENABLED` env var is set to `true` by luncur's own
system manifests, since the registry's HTTP API rejects manifest DELETEs
outright without it.

## Build Pipeline

When you run `luncur deploy` on a local source or git repository, the following happens: source is uploaded (tarball from local cwd or cloned from git URL) → a BuildKit Job runs in the `luncur-system` namespace, applying Nixpacks if no Dockerfile exists or using the Dockerfile if present → the resulting image is pushed to the in-cluster registry (default `registry.luncur-system:5000`) → app manifests are rendered and applied to Kubernetes → the app becomes live at `http://<app>.<ip>.sslip.io`. Build logs are streamed on demand via `luncur logs`.

**Serve flags for build infrastructure:**
- `--data-dir` — path where build sources and logs are persisted; becomes a Kubernetes PVC in production (default `./data`)
- `--builder-image` — OCI image for the build environment (default `luncur/builder:latest`); built by the release pipeline
- `--registry-host` — in-cluster registry address (default `registry.luncur-system:5000`); K3s requires an insecure-registry entry for this host, written to `/etc/rancher/k3s/registries.yaml` by `luncur up`

## Security

luncur's own access to the cluster is a scoped `ClusterRole` — namespaces,
Deployments, Jobs, Ingresses, and the specific CRDs luncur touches
(HelmChartConfig, cert-manager's ClusterIssuer) — instead of
`cluster-admin`; the rule set is golden-tested in
`internal/up/manifests_test.go`. Every web-UI form (scale, env, domains,
deploy, rollback, login) carries a CSRF token: a `luncur_csrf` cookie
mirrored in a hidden `_csrf` field, checked on every POST before it runs.

## Approved deviations from the design spec

- Web UI uses stdlib `html/template` + one vanilla-JS `EventSource` block instead of templ + HTMX. Zero codegen, zero vendored JS; same server-rendered pages + SSE behavior.
- In-cluster registry is reachable from containerd via a NodePort (30500) + `registries.yaml` mirror to `http://127.0.0.1:30500`, since containerd on the node cannot resolve cluster-DNS names like `registry.luncur-system`.
- Public-IP detection: node `ExternalIP` → node `InternalIP` → `--ip` flag. No outbound HTTP probe.
- API tokens expire after 90 days; `luncur token list/revoke` manages them — session cookies live in the same table, so revoking one logs that browser out.
- The git-push SSH host key persists as a file on the data PVC (beside the DB), not a K8s Secret — same durability, no kube dependency at SSH boot.
- Push progress streams via a `post-receive` hook, so the `git push` exit code cannot reflect a build failure (refs land before the hook runs). The client still sees the full build log and a final `BUILD FAILED`/`app live` line.
- The builtin ACME provider's HTTP-01 challenge Ingress lives in the `luncur-system` namespace, not the app's own namespace, because an Ingress's backend Service must be in the same namespace and the challenge responder runs as part of luncur itself.
- The TLS cert provider (`builtin`/`traefik`/`cert-manager`) is fixed when `luncur serve` starts, read once from the `cert_provider` setting; changing it requires a restart (`luncur up --cert-provider ...` triggers one).
- `cert-manager` mode reports domain status as `external`; its cert's expiry is read back from the issued TLS Secret during the daily cert sweep rather than on every request, so `cert_expires_at` can lag briefly right after issuance or renewal.
- CSRF is the stateless double-submit-cookie pattern (a random `luncur_csrf` cookie plus a matching `_csrf` hidden form field, compared with `crypto/subtle`) rather than a server-stored per-session token — same protection for this threat model (browser forms, no JS API), zero schema.
- Rollback's registry-presence check only applies to images hosted in the embedded registry (ref prefixed with the configured `--registry-host`); externally-hosted image refs are assumed present since luncur has no credentials to check them.
- Invite email is best-effort — unconfigured SMTP or a send failure never blocks invite creation; the API returns `emailed:false` plus a warning and the `/ui/register?token=...` link can still be shared out of band.
- Registration marks the invite used *after* creating the user (two non-atomic statements against a single-instance SQLite server); a burned-but-unused invite can't happen, since validation failures abort before the user is created and a duplicate-email failure aborts before the invite is marked used.
- Registry GC's "bytes reclaimed" figure is measured with `du -sk` inside the registry pod before/after `garbage-collect` (busybox `du`, KiB resolution) rather than precise blob accounting; the manifest-delete phase always runs and is counted accurately regardless, and when the exec phase itself fails (no kube, no pod, exec error) bytes reclaimed is reported as unknown (`-1`) rather than blocking the sweep.

Design docs: `docs/superpowers/specs/`. Plans: `docs/superpowers/plans/`.
