<div align="center">

# 🚀 luncur

**Empty VPS → running PaaS in under 2 minutes.**

*Deploys as simple as Heroku, on hardware you own — one Go binary, SQLite,
and real Kubernetes underneath, with an escape hatch when you outgrow it.*

[![CI](https://github.com/sutantodadang/luncur/actions/workflows/ci.yml/badge.svg)](https://github.com/sutantodadang/luncur/actions/workflows/ci.yml)
[![Release](https://github.com/sutantodadang/luncur/actions/workflows/release.yml/badge.svg)](https://github.com/sutantodadang/luncur/actions/workflows/release.yml)
[![Go](https://img.shields.io/badge/Go-1.26-00ADD8?logo=go&logoColor=white)](go.mod)
[![License: AGPL-3.0](https://img.shields.io/badge/License-AGPL--3.0-blue.svg)](LICENSE)
[![Latest release](https://img.shields.io/github/v/release/sutantodadang/luncur?include_prereleases)](https://github.com/sutantodadang/luncur/releases)
[![Sponsor](https://img.shields.io/badge/❤-Sponsor-ea4aaa?logo=githubsponsors&logoColor=white)](https://github.com/sponsors/sutantodadang)

[Quickstart](#-quickstart) · [Features](#-features) · [How it works](#-how-it-works) · [Documentation](#-documentation) · [Roadmap](#-roadmap) · [Sponsor](#-sponsor-luncur)

</div>

---

## Why luncur?

You have a side project and a $5 VPS. Your choices today:

- **A managed PaaS** — lovely DX, but the bill scales faster than your app,
  and your data lives on someone else's computer.
- **Raw Kubernetes** — yours forever, but now you maintain ingress
  controllers, cert-manager, registries, and forty YAML files before your
  first deploy.
- **luncur** — `git push` deploys, automatic HTTPS, managed Postgres/Redis,
  backups, a web panel… produced by **one Go binary and a SQLite file**,
  running on K3s on your own box.

The trick: luncur doesn't hide Kubernetes, it *renders* it. Every app is a
plain Deployment/Service/Ingress you can inspect (`luncur app raw`), patch
through a YAML editor that survives redeploys, or — when you outgrow the
training wheels — **eject** and keep running without luncur in the loop.
Changed your mind? **Adopt** it back. No lock-in in either direction.

## ✨ Features

| | |
|---|---|
| 🚢 **Deploy 3 ways** | `git push luncur main`, `luncur deploy` from local source (Nixpacks or Dockerfile, built in-cluster by BuildKit with a per-app registry-backed layer cache), or any pre-built image — or auto-deploy on push via a GitHub/GitLab/Gitea webhook |
| ⏱️ **App kinds** | `web` (Deployment+Service+Ingress), internal `web` (Deployment+Service, no Ingress — cluster-only), `worker` (Deployment only, no URL), and `cron` (Kubernetes CronJob, `Forbid` concurrency) — `luncur logs` streams all four |
| 🔒 **Automatic HTTPS** | Built-in ACME (Let's Encrypt) — HTTP-01 out of the box, **DNS-01 + wildcard certs** (`*.example.com`) via Cloudflare, Route53, or RFC2136/nsupdate |
| 🐘 **Managed addons** | Postgres & Redis as StatefulSets: create, attach (`DATABASE_URL` injected), in-place version upgrades, per-addon logical dumps in every backup |
| 📀 **Persistent volumes** | `luncur volume add myapp /data --size 5` mounts an RWO PVC into a web/worker app — survives redeploys, never deleted implicitly (`--purge` to delete data) |
| 💾 **Backups & restore** | One-command snapshot (SQLite + sealer key + addon dumps) to any S3-compatible bucket — stdlib SigV4 client, no SDK. `luncur restore` rebuilds a box from an archive |
| 🖥️ **Web panel** | Server-rendered UI: deploys, live log streaming (SSE), scaling (with per-app CPU/memory limits), env vars, domains, rollbacks, YAML overrides, user & token management — zero JS frameworks |
| ⏪ **Instant rollback** | Redeploy any previous image in seconds, lineage tracked, from CLI or UI |
| 👥 **Teams** | Projects with members, admin/member roles, single-use invite links (optionally emailed via SMTP), API tokens with self-service revoke |
| 🪂 **The escape hatch** | `app eject` hands you the raw manifests and stops managing; `app adopt` reverses it. Your cluster, your call |
| 📦 **One binary** | Server, CLI, installer, and restore tool are the same `luncur` executable. State is one SQLite file. Secrets sealed at rest (AES-256-GCM) |
| 📝 **Audit trail** | Every successful mutating request — API, web UI, login, webhook-triggered deploy — recorded with who/route/path/when; `luncur audit` and the `/ui/audit` panel, configurable retention |
| 🩺 **One-shot diagnosis** | `luncur doctor` checks database, kubernetes, registry, stuck builds, ingress, certificates, SMTP, notifications, and backups in one call — admin only, exits non-zero on any failing check |

**Zero-bloat scorecard:** 1 Go module · SQLite (no DB server) · stdlib
`html/template` UI (no Node, no bundler) · stdlib S3/SigV4/SMTP/DNS-01
clients (no SDKs) · scoped ClusterRole (not cluster-admin) · every feature
tested without a cluster or network.

## ⚡ Quickstart

On a fresh Linux VPS:

```sh
curl -sfLo luncur https://github.com/sutantodadang/luncur/releases/latest/download/luncur-linux-amd64
chmod +x luncur
sudo ./luncur up        # installs K3s + luncur, prints admin credentials (once — save them!)
```

Deploy your first app from your laptop:

```sh
luncur login http://panel.<ip>.sslip.io
luncur project create demo
luncur app create web --project demo --port 8080

luncur ssh-key add                                        # once per machine
git remote add luncur ssh://git@<ip>:30022/demo/web.git   # once per repo
git push luncur main                                      # 🚀 build streams into your push output
```

Your app is live at `http://web.<ip>.sslip.io` — no DNS setup needed
(sslip.io resolves automatically). Add a real domain and HTTPS:

```sh
luncur domain add web www.example.com --project demo   # cert issues automatically
```

Re-running `sudo ./luncur up` is always safe: every step is
skip-or-repair, so it doubles as upgrade and self-heal.

## 🔍 How it works

```text
you                        your VPS (that's the whole footprint)
────                       ─────────────────────────────────────────────
git push ──────────────►   ┌─ K3s ────────────────────────────────────┐
luncur CLI ────────────►   │  luncur server (1 binary + SQLite + PVC) │
browser (web panel) ───►   │     │ renders manifests, SSA-applies     │
                           │     ├─► BuildKit Job ─► embedded registry│
                           │     ├─► your apps   Deployment/Svc/Ingress
                           │     ├─► addons      Postgres/Redis (PVC) │
                           │     └─► TLS         ACME HTTP-01/DNS-01  │
                           └──────────────────────────────────────────┘
```

Everything luncur creates is a normal Kubernetes object applied with
server-side apply (`fieldManager=luncur`). `kubectl` sees exactly what you
see. Delete luncur and your apps keep running.

## 📚 Documentation

Everything below is the complete operator's manual — luncur has no
separate docs site; this README is it.

<details>
<summary><b>Table of contents</b></summary>

- [Install](#install)
- [Auth, users & tokens](#auth-users--tokens)
- [Projects & apps](#projects--apps)
- [Deploying](#deploying)
- [Web UI](#web-ui)
- [Custom domains & TLS](#custom-domains--tls)
- [Wildcard domains & DNS-01](#wildcard-domains--dns-01)
- [Addons (Postgres/Redis)](#addons-postgresredis)
- [Volumes](#volumes)
- [Backups](#backups)
- [Restoring](#restoring)
- [Ejecting an app](#ejecting-an-app)
- [Registry GC](#registry-gc)
- [Build pipeline](#build-pipeline)
- [Security](#security)
- [Design notes](#design-notes)

</details>

### Install

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
  --builder-image ghcr.io/sutantodadang/luncur-builder:latest \
  --registry-host registry.luncur-system:5000
```

### Auth, users & tokens

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

The web UI mirrors this: every user has a `/ui/tokens` page (nav →
"tokens") listing their own tokens with per-row revoke buttons. The
browser session is the row named `session`; revoking it logs that browser
out.

### Projects & apps

```sh
luncur project create myproj
luncur project list
luncur project add-member myproj member@example.com

luncur app create myapp --project myproj --port 8080
luncur app list --project myproj
luncur app info myapp --project myproj
luncur app raw myapp --project myproj
```

**App kinds — web (default), worker, cron:**
```sh
luncur app create worker1 --project myproj --kind worker
luncur app create nightly --project myproj --kind cron --schedule "0 3 * * *"
```
Workers get a Deployment with no Service/Ingress — no URL, no domains.
Cron apps run as a Kubernetes CronJob (`concurrencyPolicy: Forbid`, so a slow
run never overlaps the next trigger); the schedule is only checked for valid
5-field cron syntax client- and server-side — Kubernetes evaluates it.
`luncur logs` streams pod output for all kinds; scaling replicas only
applies to web/worker (cron scales cpu/memory only), and domains/health
checks are web-only.

**Internal (cluster-only) web apps — `--internal`:** a web app that should
never be reachable from outside the cluster (e.g. an internal AI/microservice
another app in the same project calls over HTTP) can be created with
`--internal`:
```sh
luncur app create ai --project myproj --port 8001 --internal
```
An internal app still gets a Deployment and a ClusterIP Service — so other
apps in the cluster can reach it — but **no Ingress and no public URL**.
`luncur app info`/`app list` show its cluster DNS address instead of a URL:
`http://<service-name>.<project-namespace>:80` (any pod in the cluster can
reach it at that address; from inside the same namespace the short
`http://<service-name>:80` also works). `--internal` only applies to `web`
apps (`worker`/`cron` already have no Service to make internal — combining
`--internal` with `--kind worker` or `--kind cron` is rejected), and adding a
custom domain to an internal app is rejected too — a custom-domain Ingress
would defeat the whole point.

### Deploying

**Image, scale, destroy:**
```sh
luncur deploy myapp --project myproj --image my.registry/my/image:tag
luncur scale myapp --project myproj --replicas 3
luncur destroy myapp --project myproj
```

**Per-app CPU/memory limits:**
```sh
luncur scale myapp --project myproj --cpu 250m --memory 256Mi   # requests==limits
luncur scale myapp --project myproj --cpu "" --memory ""        # clear
```

**Per-app HTTP health checks:**
```sh
luncur health myapp --project myproj --path /healthz   # readiness+liveness probes
luncur health myapp --project myproj --off
```
Readiness gates rollouts and Service endpoints (zero-downtime deploys), while liveness restarts a wedged container.

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

**Monorepo builds (`--path`):** one git repo can back several apps by
pointing each at a different subdirectory as its build context/detection
dir — e.g. a `dashboard/` React app, a `backend/` FastAPI service, and an
`ai/` FastAPI service all living in the same repo. `dashboard` and `backend`
are public; `ai` is only ever called by `backend`, so it's created
`--internal` — no public URL, cluster-only:

```sh
luncur app create dashboard --project myproj --port 3000 \
  --git-url https://github.com/user/monorepo.git --path dashboard
luncur app create backend --project myproj --port 8000 \
  --git-url https://github.com/user/monorepo.git --path backend
luncur app create ai --project myproj --port 8001 --internal \
  --git-url https://github.com/user/monorepo.git --path ai
```

`backend` reaches `ai` at `http://ai.luncur-myproj:80` (in-cluster DNS —
see "Internal (cluster-only) web apps" above). A single webhook push (or
`luncur deploy`/`git push luncur`) to the shared repo redeploys all three —
each app's build only looks inside its own `--path`: a `Dockerfile` there is
used as-is, and nixpacks detection/output also run inside that subdirectory
instead of the repo root. `--path` is validated (relative, no `..`, no
leading `/`, `[a-zA-Z0-9._/-]` only) and is **immutable after creation** —
recreate the app to change it. Omitting `--path` keeps the previous
behavior: the whole repo root is the build context.

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

**Webhook auto-deploy (for git-source apps hosted elsewhere):**
```sh
luncur webhook enable myapp --project myproj
# paste the printed URL + secret into GitHub/GitLab/Gitea's webhook settings (push events)
```
A push to the app's configured branch (default `main`) triggers the same
build pipeline as `luncur deploy`; pushes to other refs are ignored. GitHub's
"ping" event (sent when a webhook is first saved) is answered without
deploying, so saving the hook doesn't kick off a spurious build.
`luncur webhook enable` re-run rotates the secret — update it at the
provider or deploys will stop authenticating. `luncur webhook show` and
`luncur webhook disable` round out the command.

**Environment & editing:**
```sh
luncur env set myapp KEY=value --project myproj
luncur env unset myapp KEY --project myproj
luncur env list myapp --project myproj
luncur edit myapp Deployment --project myproj
```

**Build-time env:**

The same values set with `luncur env set` also reach the build itself, not
just the running container: each var is passed to the builder as a Docker
build-arg (Dockerfile path) or a nixpacks `--env` (nixpacks path). This
matters for frontend frameworks that bake env into the bundle at build time
instead of reading it at runtime — e.g. Vite:

```dockerfile
ARG VITE_API_URL=http://localhost:8000
ENV VITE_API_URL=$VITE_API_URL
RUN npm run build
```

`luncur env set myapp VITE_API_URL=https://api.myapp.example --project myproj`
then makes the next build inline the real URL instead of the Dockerfile's
localhost default. The Dockerfile must declare `ARG <KEY>` for a var to be
picked up; unreferenced vars are simply ignored by `docker build`.

Build-args are not a safe place for high-grade secrets — they can leak into
build cache metadata and image history. Keep secrets runtime-only (don't
reference them from any `ARG`); use build-time env only for values that are
fine to be visible in a built image.

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

### Web UI

After `luncur up`, the panel is served at `http://panel.<ip>.sslip.io/ui/`
(login with the admin credentials printed by `luncur up`, or any user added
via `luncur user add` or an invite). It's a dark, server-rendered dashboard
(Tailwind CSS + htmx, fully embedded and air-gapped — no CDN, no external
requests) covering the CLI end to end — every CLI verb has a UI control,
with `luncur restore` the one deliberate exception (destructive enough to
stay a CLI-only, deliberate action):

- **Projects & apps** — create a project and add members (admin), browse
  projects and apps, scale replicas/cpu/memory, edit env vars and volumes,
  manage domains (including retrying a failed cert), attach/upgrade/detach
  addons, trigger a deploy, roll back to a previous deploy, eject/adopt an
  app, destroy an app, and watch build/runtime logs stream live via
  Server-Sent Events. Each app page shows deploy history, a live status
  chip, a stats line (cpu/memory when `metrics-server` is available,
  ready/desired replicas, deploy count), and a Danger zone for eject/destroy.
- **Audit** (`/ui/audit`, admin) — every mutating action, who did it, and when.
- **Users & invites** (`/ui/users`, admin) — list every user (email, role,
  created, active token count), delete a user, and mint invites — each
  invite is a single-use, 7-day link (`/ui/register?token=...`) shown as a
  copyable field; anyone who opens it registers their own email/password and
  is logged straight in with the invite's role. The invite form takes an
  optional email address: when SMTP is configured (see below) the link is
  mailed and a note reports the outcome; the copyable link works either way.
- **Tokens** (`/ui/tokens`) — list and revoke your own API tokens; your
  browser session lives in the same table, so revoking it logs you out.
- **SSH keys** (`/ui/sshkeys`) — add/remove your own public keys for
  `git push` auth.
- **Backups** (`/ui/backups`, admin) — trigger a backup, prune old ones, and
  see when/how big/whether each was uploaded to S3. Restoring is CLI-only
  (`luncur restore <file>`), called out on the page.
- **Settings** (`/ui/settings`, admin) — every install-level setting
  (certs, DNS-01, SMTP, notifications, backup schedule/S3, registry
  keep/build cache, audit retention), plus a **registry GC** button. Every
  write-only secret (S3 key, SMTP password, DNS provider tokens, notify URL)
  shows `(set)` instead of its value — the plaintext is never echoed back.
- **Doctor** (`/ui/doctor`, admin) — the same 9 health checks as
  `luncur doctor`, one page, "run again" link.

Every app page also has a **YAML override editor**: `edit: Deployment ·
Service · Ingress` links open the currently-rendered manifest for that kind
in a textarea. Saving diffs your edit against a fresh base render and stores
the result as the same strategic-merge-patch override `luncur edit` writes
— so the change survives every future redeploy, and re-syncs a live app
immediately. Invalid YAML re-shows the editor with the error and your text
intact, never discarding the edit.

The UI's CSS and htmx are vendored into the binary (`internal/server/static/`)
— nothing is fetched from a CDN at runtime. If you're changing templates,
see [CONTRIBUTING.md](CONTRIBUTING.md) for the Tailwind regen command.

### Custom domains & TLS

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

### Wildcard domains & DNS-01

`domain add` also accepts `*.example.com` (one leading `*.`). Wildcards
can't be validated over HTTP-01, so they need a DNS provider luncur can
publish TXT records through:

```sh
luncur config set dns_provider cloudflare       # cloudflare | route53 | rfc2136 | none (default)
luncur config set dns_cloudflare_token ...      # write-only: reads show "(set)"

# route53
luncur config set dns_route53_access_key AKIA...
luncur config set dns_route53_secret_key ...    # write-only
luncur config set dns_route53_region us-east-1  # optional

# rfc2136 (nsupdate + TSIG, e.g. BIND)
luncur config set dns_rfc2136_server ns1.example.com
luncur config set dns_rfc2136_tsig_name luncur-key
luncur config set dns_rfc2136_tsig_secret ...   # write-only
luncur config set dns_rfc2136_tsig_algo hmac-sha256  # optional, default
```

With a provider configured, the `builtin` cert manager validates every
domain via DNS-01 (a TXT record at `_acme-challenge.<domain>`, polling the
zone's authoritative nameservers with a ~2 minute timeout) instead of
HTTP-01 — wildcards require it, plain hostnames just use it. A wildcard
without a configured `dns_provider` is refused with a 400. `traefik` and
`cert-manager` providers are unchanged — they own their own solving.
Failures behave like HTTP-01: the domain shows `cert_status failed` with
the message and the app keeps serving.

### Addons (Postgres/Redis)

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

### Volumes

```sh
luncur volume add myapp /data --project myproj --size 5              # 5GB PVC mounted at /data
luncur volume add myapp /var/cache --project myproj --size 1 --name cache
luncur volume list myapp --project myproj                            # NAME, PATH, SIZE
luncur volume remove myapp cache --project myproj                    # detach; PVC + data kept
luncur volume remove myapp cache --project myproj --purge            # detach AND delete the PVC
```

Volumes attach a `ReadWriteOnce` PersistentVolumeClaim (K3s local-path by
default) to a `web` or `worker` app — `cron` apps don't take volumes. The
name defaults to the last path segment; size is 1–1000 GB.

Honest constraints, stated up front:

- **Max 1 replica.** RWO local-path storage binds to one node/pod. Adding a
  volume to an app running more than one replica is refused (409), as is
  scaling a volumed app above 1.
- **Brief downtime per deploy.** A volumed app's Deployment uses the
  `Recreate` strategy (the old pod must release the volume before the new
  one can mount it), so every deploy stops the app for a few seconds.
- **Not in backups.** `luncur backup` snapshots luncur's own state and addon
  dumps — app volume data is *not* included. Back up volume contents
  yourself if they matter.
- **Never deleted implicitly.** `volume remove` only detaches; the PVC and
  its data survive until you pass `--purge`. Destroying an app also leaves
  its PVCs behind — run `volume remove --purge` first, or clean up later
  with `kubectl delete pvc -n <namespace> <app>-<volume>`.

The app page in the web UI has a matching Volumes section (add form,
per-row remove with a "purge data" checkbox), hidden for cron apps.

### Backups

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

**Notifications (deploy & cert events):**

```sh
luncur config set notify_url https://discord.com/api/webhooks/...   # write-only: reads show "(set)"
luncur config set notify_format discord      # generic (default) | discord | slack | telegram
luncur config set notify_events deploy_success,deploy_failed,cert_failed
# telegram: notify_url = https://api.telegram.org/bot<token>/sendMessage
luncur config set notify_telegram_chat 123456789
```

Unset `notify_url` disables the feature entirely. `notify_events` is a CSV
subset of `deploy_success`, `deploy_failed`, `cert_issued`, `cert_failed`;
default when unset is `deploy_failed,cert_failed`. Delivery is best-effort:
one attempt, a 5s timeout, failures logged — a notification never blocks a
deploy or cert issuance. The `generic` format POSTs
`{"event","project","app","deploy_id","status","url","error","time"}`
(`deploy_id` omitted for cert events, `url`/`error` omitted when empty, `time`
RFC3339); `discord`/`slack` POST `{"content"|"text": <message>}`; `telegram`
adds `"chat_id"` from `notify_telegram_chat`.

**Security note:** archives contain the sealer key — whoever can read a
backup can unseal your env vars and addon credentials. The S3 bucket is
the trust boundary; scope its access accordingly.

### Restoring

`luncur restore` automates the DB/key half; addon data stays guided:

```sh
# on the target host, with the server scaled down
luncur restore /path/to/luncur-YYYYMMDD-HHMMSS.tar.gz --data-dir <data-dir> [--force]

# or straight from the backup bucket
luncur restore <prefix>/luncur-....tar.gz --s3-endpoint https://<s3> \
  --s3-bucket my-backups --s3-access-key ... --s3-secret-key ... --data-dir <data-dir>
```

It validates the archive's `manifest.json` before touching anything and
refuses a data dir whose DB already has projects unless `--force` — which
first copies the current `luncur.db`/`luncur.key` into
`pre-restore-<timestamp>/` inside the data dir. On success it extracts
`luncur.db` and `luncur.key` and prints the remaining guided steps.

Full procedure on a fresh VPS:

1. Provision: `luncur up` (fresh install, any admin password — it will be
   replaced by the restored DB).
2. Stop the server so SQLite is quiescent:
   `kubectl -n luncur-system scale deploy/luncur --replicas=0`.
3. Run `luncur restore` against the data PVC's path. The PVC is a
   `local-path` volume on the node — find it with
   `kubectl -n luncur-system get pvc luncur-data -o jsonpath='{.spec.volumeName}'`
   and look under `/var/lib/rancher/k3s/storage/<volume>/`.
4. Start the server: `kubectl -n luncur-system scale deploy/luncur --replicas=1`,
   then `luncur login` with the restored credentials.
5. Re-create each addon (`luncur addon create ...` with the same names) so
   the StatefulSets exist, then restore data into them (the restore
   command prints these same steps for the dumps it found):
   - Postgres: `kubectl -n <project-ns> exec -i addon-<name>-0 -- sh -c
     'PGPASSWORD="$POSTGRES_PASSWORD" pg_restore -U "$POSTGRES_USER" -d "$POSTGRES_DB" --clean' < addons/<project>-<name>.pgdump`
   - Redis: scale the addon StatefulSet to 0, copy the `.rdb` onto its PVC
     as `dump.rdb`, scale back to 1.
6. Redeploy apps (`luncur deploy` / `git push`) and verify with
   `luncur status`.

### Uninstall

`luncur down` reverses `luncur up`. Two tiers:

- **default** — removes luncur itself: the `luncur`/`luncur-ssh`
  Deployment/Services/Ingress/ServiceAccount and RBAC, every luncur-managed
  namespace (`luncur-system` plus each project's `luncur-<name>`, found via
  the `app.kubernetes.io/managed-by=luncur` label), the `luncur-data` PVC,
  and the `registries.yaml` `luncur up` wrote. **K3s itself is left running**
  so another `luncur up` can redeploy onto it.
- **`--all`** — also runs K3s's own `k3s-uninstall.sh`, removing K3s and all
  its cluster data.

```sh
luncur down --dry-run       # print the plan, change nothing
luncur down                 # tear down luncur, keep K3s
luncur down --all           # tear down luncur AND K3s
luncur down --all --yes     # skip the typed confirmation (e.g. scripted)
luncur down --no-backup     # skip the final DB backup
```

Before touching anything (unless `--yes`), it prints what will be destroyed
and requires typing the literal word `luncur` to proceed — anything else
aborts. Unless `--no-backup`, the live `luncur.db` is `cat`'d out of the
running pod first, to `~/luncur-final-backup-<unix-ts>.db`. Each step is
best-effort: a failure is reported (`step failed: ...`) and the remaining
steps still run, with a non-zero exit if anything failed.

`--dry-run` output looks like:

```
dry run — no changes will be made:
1. backup SQLite DB to /home/you/luncur-final-backup-1735689600.db
2. stop luncur (delete Deployment/Services/Ingress/ServiceAccount and RBAC in luncur-system)
3. delete luncur-managed namespaces (luncur-system + every project namespace)
4. remove luncur data volume (PersistentVolumeClaim luncur-data in luncur-system)
5. remove registries config written by `luncur up` (/etc/rancher/k3s/registries.yaml)
```

### Ejecting an app

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

### Registry GC

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

### Audit log

Every successful mutating request records one row: who (email), which route,
the request path, and when. This covers the API, the web UI (session +
CSRF-checked form posts), logins, and webhook-triggered deploys (recorded
under the `webhook` user). Failed and read-only (GET) requests are never
recorded. Secret values are never stored, and routes whose path carries a
secret — like `DELETE /v1/invites/{token}` — record the route pattern
instead of the raw path, so the token itself never lands in the log.

```sh
luncur audit                              # recent rows: ID, TIME, USER, ACTION, TARGET
luncur audit --user alice@example.com     # filter by exact user
luncur audit --contains apps              # filter by substring on action/target
```

Admins can also browse the log at `/ui/audit`. Rows are pruned
opportunistically after each recorded mutation, keeping `audit_retention_days`
(default 90) worth of history; set it to `0` to keep every row forever:

```sh
luncur config set audit_retention_days 30
```

### Doctor / diagnostics

```sh
luncur doctor   # admin only
```

Runs nine checks in one request and prints a table:

```
CHECK          STATUS  DETAIL
database       ok      reachable
kubernetes     ok      1 node(s) ready
registry       ok      3 repositories
builds         ok      no stuck builds
ingress        ok      1/1 traefik pod(s) ready
certificates   ok      2 domain(s), none failing
smtp           warn    not configured — invite emails disabled
notifications  warn    not configured
backups        warn    scheduled backups off
version        ok      client v0.5.0 == server v0.5.0
```

- **database** — the SQLite connection is reachable.
- **kubernetes** — every node's `Ready` condition is true; fails outright if no kubeconfig was ever wired up.
- **registry** — the embedded registry answers its catalog endpoint.
- **builds** — no deployment has been stuck in `building` for over 30 minutes (a sign the builder Job died or the builder image is missing).
- **ingress** — at least one Traefik pod in `kube-system` is ready.
- **certificates** — no custom domain is stuck in `cert_status = failed` (the hostname is named; the underlying ACME error text is never printed here — see `luncur domain list` for that).
- **smtp**, **notifications**, **backups** — whether the corresponding setting (`smtp_host`, `notify_url`, `backup_schedule`) is configured; these only ever warn, never fail.
- **version** — added client-side: compares the CLI binary's version against the server's, since a stale CLI against a newer server (or vice versa) is a common source of confusing behavior.

Exit code is `0` when every check is `ok` or `warn`, and `1` if any check
`fail`s — safe to wire into cron or an uptime check. Each check runs
independently with its own 5-second timeout, so one wedged dependency never
blocks the rest.

### Build pipeline

When you run `luncur deploy` on a local source or git repository, the following happens: source is uploaded (tarball from local cwd or cloned from git URL) → a BuildKit Job runs in the `luncur-system` namespace, applying Nixpacks if no Dockerfile exists or using the Dockerfile if present → the resulting image is pushed to the in-cluster registry (default `registry.luncur-system:5000`) → app manifests are rendered and applied to Kubernetes → the app becomes live at `http://<app>.<ip>.sslip.io`. Build logs are streamed on demand via `luncur logs`.

Builds reuse a per-app BuildKit cache stored as an image manifest in the
embedded registry (`luncur-cache/<project>-<app>:buildcache`), so repeat
deploys skip unchanged layers instead of rebuilding from scratch every time.
Disable it with `luncur config set build_cache off`. Cache manifests are
kept by registry GC for as long as the app exists and swept once it's
destroyed, same as any other repo (see [Registry GC](#registry-gc) above).
Since the cache is exported with `mode=max` (intermediate layers, not just
the final image), factor that into disk sizing for the registry's PVC
alongside app images.

A build is allotted a fixed time budget before it's given up on and marked
failed — 15 minutes by default. Override it with
`luncur config set build_timeout_minutes 30` (1-720); the new budget applies
to builds started afterward, including ones re-attached by restart
reconciliation (see below).

**Serve flags for build infrastructure:**
- `--data-dir` — path where build sources and logs are persisted; becomes a Kubernetes PVC in production (default `./data`)
- `--builder-image` — OCI image for the build environment (default `ghcr.io/sutantodadang/luncur-builder:latest`); built and published to ghcr.io by the release pipeline
- The system namespace (`luncur-system`) that build Jobs run in is provisioned at PodSecurity level `privileged`, not `restricted`: rootless BuildKit needs setuid `newuidmap` to remap uids (forbidden by `restricted` as privilege escalation) and unconfined seccomp/AppArmor profiles for its mount-namespace setup (forbidden by `baseline`). The build pod itself still runs as the unprivileged uid 1000 — the namespace label only stops Kubernetes from rejecting those profile settings. Project/app namespaces are unaffected and stay `restricted`.
- `--registry-host` — in-cluster registry address (default `registry.luncur-system:5000`); K3s requires an insecure-registry entry for this host, written to `/etc/rancher/k3s/registries.yaml` by `luncur up`

#### Troubleshooting: build stuck or no logs

`luncur logs <app> --project <project> --deploy <id> -f` streams the deployment's build log, which
now carries two kinds of lines: server-written milestones prefixed
`[luncur]` (e.g. `[luncur] 2026-01-01T00:00:00Z rendering build job`,
`...applying build job to cluster`, `...waiting for builder pod`, and
periodic `...builder pod: Pending (ImagePullBackOff)` updates), and — once
the builder pod starts — its own entrypoint output appended after. If the
log only ever shows `[luncur]` lines and never progresses past "waiting for
builder pod", the builder pod itself isn't starting.

- **`builder pod: Pending (ImagePullBackOff)`** — the cluster can't pull the
  builder image (`--builder-image`, default
  `ghcr.io/sutantodadang/luncur-builder:latest`, published by the release
  pipeline). If you're pinning a custom image, publish it to a registry the
  cluster can reach, or rebuild/retag it locally and make sure the node has it
  (`docker save`/`k3s ctr images import` for single-node setups).
- **`no builder pod created yet — job events: ...`** — the Build Job never
  managed to create a pod at all (PodSecurity admission rejecting the pod
  spec, a ResourceQuota with no room left, a validating admission webhook,
  etc.), so `JobPodStatus` never sees a pod to report on. After ~30s of
  seeing no pod, the log now prints the Job's own recent Kubernetes events —
  a `Warning FailedCreate: pods "build-<id>-" is forbidden: violates
  PodSecurity "restricted:latest"` line is the usual PodSecurity case — so
  the reason for the stall shows up in the LOGS pane instead of nothing.
  The manual equivalent, if you're at a shell:
  `kubectl -n luncur-system describe job build-<id>` (its Events section is
  exactly what gets mirrored into the log).
- **`pod watcher error: ...`** — the watcher itself couldn't list pods for
  the Build Job (e.g. missing RBAC on `pods` in `luncur-system`). Logged
  once, not spammed every poll; fix the permission and the next build's
  watcher will succeed.
- **Build never finishes** — it's cut off after `build_timeout_minutes`
  (default 15) and marked failed; raise it with
  `luncur config set build_timeout_minutes <n>` if your builds are simply
  slow (large images, cold BuildKit cache).
- **`luncur doctor`'s `builds` check** reports `fail` when a deployment has
  sat in `building` for over 30 minutes — the same signal, surfaced without
  needing to know which app it is.
- **Server restarted mid-build** — on startup, luncur reconciles every
  deployment left in `building`/`deploying` by the previous process: if the
  Build Job is still running it re-attaches and resumes normally (a
  `[luncur] ... server restarted — re-attached to running build job` line
  appears in the log); if the Job is gone too, or the resumed build/deploy
  fails, the deployment is marked `failed` rather than left stuck forever.

### Security

luncur's own access to the cluster is a scoped `ClusterRole` — namespaces,
Deployments, Jobs, Ingresses, and the specific CRDs luncur touches
(HelmChartConfig, cert-manager's ClusterIssuer) — instead of
`cluster-admin`; the rule set is golden-tested in
`internal/up/manifests_test.go`. Every web-UI form (scale, env, domains,
deploy, rollback, login) carries a CSRF token: a `luncur_csrf` cookie
mirrored in a hidden `_csrf` field, checked on every POST before it runs.

Secrets never sit in plaintext: env vars, addon credentials, and sensitive
settings (S3 secret key, SMTP password, DNS provider tokens) are sealed at
rest with AES-256-GCM; the sealed settings are write-only through the API
(reads show `(set)`).

The deploy webhook endpoint (`POST /hooks/apps/{project}/{app}`) is
unauthenticated at the HTTP layer by design — a git provider posts to it
directly, so there's no bearer token to present. The HMAC/token check *is*
the auth: every failure (unknown project/app, webhook disabled, unseal
failure, bad signature) answers with the byte-identical 401 body, so the
endpoint can't be used to probe whether a project or app exists. The
request body is capped at 1 MiB before it's read. The webhook secret is
sealed at rest the same way env vars are (AES-256-GCM) and is only ever
shown in plaintext once, in the response to `webhook enable`.

### Design notes

<details>
<summary>Deliberate deviations & implementation notes (click to expand)</summary>

- Web UI uses stdlib `html/template` (zero codegen) + htmx + one vanilla-JS `EventSource` block for log streaming, styled with Tailwind CSS compiled ahead of time into a vendored, air-gapped stylesheet — no CDN, no client build step at runtime.
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
- Deploy/cert notifications read `notify_*` settings at send time (no restart needed to pick up a change) and make a single delivery attempt — no retry queue by design, matching the rest of the best-effort notification surface (invite email, backup upload).
- Registration marks the invite used *after* creating the user (two non-atomic statements against a single-instance SQLite server); a burned-but-unused invite can't happen, since validation failures abort before the user is created and a duplicate-email failure aborts before the invite is marked used.
- Registry GC's "bytes reclaimed" figure is measured with `du -sk` inside the registry pod before/after `garbage-collect` (busybox `du`, KiB resolution) rather than precise blob accounting; the manifest-delete phase always runs and is counted accurately regardless, and when the exec phase itself fails (no kube, no pod, exec error) bytes reclaimed is reported as unknown (`-1`) rather than blocking the sweep.
- RFC2136 DNS support shells out to `nsupdate` (bind-tools, in the release image); the TSIG secret rides the script on stdin, never argv (which would be visible in `ps`). It's a runtime binary, not a Go module — the no-new-dependencies rule is about Go modules (`git`, `pg_dump`, `nsupdate` are all selected on demand).
- Per-app CPU/memory limits set requests==limits (Guaranteed QoS) deliberately, rather than exposing separate requests/limits fields — the YAML override editor is the escape hatch for anyone who needs a split.
- Health check probe timings (readiness period/threshold, liveness initial delay/period/threshold) are fixed, not configurable — the YAML override editor is the escape hatch for anyone who needs to tune them.
- Cron schedules are validated as syntactically-correct 5-field cron expressions only (field count + per-position numeric bounds); Kubernetes' CronJob controller is what actually evaluates them at runtime.
- CronJob history limits (`successfulJobsHistoryLimit`/`failedJobsHistoryLimit`: 3/3, `backoffLimit`: 2) are fixed, not configurable — same escape hatch as above.
- `CronJob` joined `Deployment`/`Service`/`Ingress` as an overridable manifest kind — the YAML override editor works for cron apps too.
- App volumes force the Deployment's `Recreate` strategy because an RWO node-local PVC can't be mounted by the old and new pod simultaneously — a rolling update would deadlock waiting for a volume the outgoing pod still holds.
- App volumes share the addons' never-implicit-delete philosophy: removing a volume (or destroying the app) leaves the PVC and its data in the cluster; only an explicit `--purge` deletes it. PVCs are not an overridable manifest kind.
- A webhook-triggered push is deduped against an in-progress build: if the app's latest deployment is still `building`, the webhook returns the existing deployment id (202) instead of stacking a second build on the same app.
- The webhook and the `git push luncur` SSH remote are independent trigger paths into the same `deployGitApp` core — an app can have both configured at once (e.g. push to luncur directly for fast iteration, webhook for a mirror hosted on GitHub/GitLab).
- Re-enabling an already-enabled webhook always rotates the secret rather than offering a separate "rotate" verb — one action, no ambiguity about which secret is currently live.
- The BuildKit layer cache is registry-backed rather than a shared PVC: concurrent builds across apps share nothing (no RWO contention on a single cache volume), and it piggybacks on registry GC's existing keep-set/sweep logic instead of needing its own retention mechanism.

Every feature shipped with a written spec, a TDD implementation plan, and
a full test suite that runs without a cluster or network.

</details>

## 🗺️ Roadmap

**Shipped:**

- [x] One-command install on a fresh VPS (`luncur up`), safe to re-run
- [x] Deploys: image, local source, git URL, `git push` over SSH
- [x] In-cluster builds (BuildKit + Nixpacks/Dockerfile) + embedded registry with GC
- [x] Web panel with live log streaming, YAML override editor, users/invites/tokens
- [x] Custom domains, three TLS providers, wildcard certs via DNS-01 (Cloudflare/Route53/RFC2136)
- [x] Postgres/Redis addons with in-place upgrades
- [x] S3 backups (scheduled) + `luncur restore`
- [x] Rollbacks, metrics, eject/adopt escape hatch
- [x] App kinds: background workers and cron jobs (Kubernetes CronJob)

**Exploring next** (sponsor input shapes the order — open an issue!):

- [ ] Cron run history (last-run status), pause/suspend, manual "run now"
- [ ] More DNS providers (deSEC, Hetzner, DigitalOcean, …)
- [ ] Automated addon-data restore (today: guided one-liners)
- [ ] HTML invite/notification emails
- [ ] Preview environments per branch
- [ ] Multi-node K3s support
- [ ] ARM builds & Raspberry Pi story

## 💖 Sponsor luncur

luncur is free, open source (AGPL-3.0), and built with care: **every feature ships
with a written design spec, a reviewed implementation plan, and tests that
run without a cluster** — 230+ of them. That rigor takes time, and time is
the thing sponsorship buys.

**What your sponsorship funds:**

- 🧑‍💻 Dedicated development time on the roadmap above
- 🖥️ Real-world test infrastructure (VPSes, domains, DNS zones for
  wildcard-cert testing across providers)
- 🩹 Fast security responses and dependable maintenance — the boring work
  that keeps a self-hosted tool trustworthy

**Who should sponsor:** if luncur replaced a PaaS bill for your side
project, agency, or startup — consider sending a slice of the difference.
One month of a hobby-dyno bill funds real features here.

<div align="center">

[![Sponsor on GitHub](https://img.shields.io/badge/Sponsor_on_GitHub-❤-ea4aaa?style=for-the-badge&logo=githubsponsors&logoColor=white)](https://github.com/sponsors/sutantodadang)

⭐ **Can't sponsor? Star the repo** — it genuinely helps others find luncur.

</div>

## 🤝 Contributing

Bug reports, feature discussions, and PRs welcome — see
[CONTRIBUTING.md](CONTRIBUTING.md) for the dev loop (spoiler: `go test
./...`, no cluster needed) and the ground rules (one module, no new
dependencies, TDD).

## 📄 License

[AGPL-3.0](LICENSE) © 2026 Dadang Sutanto

luncur is genuinely open source — use it, self-host it, modify it, fork
it. The AGPL asks one thing: if you run a **modified** luncur as a service
for others, share those modifications back.

**Need different terms?** For proprietary embedding, OEM redistribution,
or offering luncur as a managed service without AGPL obligations, a
separate **commercial license** is available from the copyright holder —
open a GitHub issue or reach out directly.
