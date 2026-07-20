# luncur

**Empty VPS → running PaaS in under 2 minutes.**

*Deploys as simple as Heroku, on hardware you own — one Go binary, SQLite,
and real Kubernetes underneath, with an escape hatch when you outgrow it.*

## Start here

- **New?** → [Getting started](getting-started.md): install on a fresh VPS and
  deploy your first app in a few commands.
- **Deploying an app** → [Deploying](guides/deploying.md) ·
  [Apps & projects](guides/apps-and-projects.md) ·
  [Domains & TLS](guides/domains-and-tls.md)
- **Databases & storage** → [Addons](guides/addons.md) ·
  [Volumes](guides/volumes.md) · [Backups & restore](guides/backups.md)
- **GPUs & ML** → [GPU cloud](ml/gpu-cloud.md) · [Training](ml/training.md) ·
  [Model serving](ml/model-serving.md)
- **Running it in production** → [Doctor](operations/doctor.md) ·
  [Disaster recovery](operations/disaster-recovery.md) ·
  [Security](reference/security.md)
- **Like luncur?** → [Support the project](support.md).

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

## Support luncur

luncur is free and open source (AGPL-3.0). If it replaced a PaaS bill for you,
[sponsoring](support.md) funds roadmap features, real-provider test
infrastructure, and fast security responses. Can't sponsor?
[Star the repo](https://github.com/sutantodadang/luncur) — it helps others find
it. See [Support luncur](support.md) for every way to help.
