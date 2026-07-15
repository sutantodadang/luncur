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

On a **Raspberry Pi** or other 64-bit ARM board, swap the binary for
`luncur-linux-arm64` — the container images are multi-arch, so everything else
is identical. See [Raspberry Pi & ARM](docs/operations/raspberry-pi.md).

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

### Access internal apps

Internal apps (`--internal`, no public URL) stay cluster-only — until you
need to peek at one:

```sh
luncur forward demo/web 8080     # localhost:8080 → the app, any TCP protocol
```

No kubeconfig needed — the tunnel rides through the luncur server with your
API token. Or click **open** on the app's panel page: it serves the app at
`http://web--demo.<your-domain>` behind your panel login (project members
only, HTTP for now unless your ingress terminates TLS for it).

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

Full operator manual: **[sutantodadang.github.io/luncur](https://sutantodadang.github.io/luncur/)**

- [Getting started](docs/getting-started.md) — install, first deploy, multi-node
- [Deploying](docs/guides/deploying.md) · [Apps & projects](docs/guides/apps-and-projects.md) · [Domains & TLS](docs/guides/domains-and-tls.md)
- [Addons](docs/guides/addons.md) · [Volumes](docs/guides/volumes.md) · [Backups](docs/guides/backups.md)
- [GPU cloud](docs/ml/gpu-cloud.md) · [Training](docs/ml/training.md) · [Model serving](docs/ml/model-serving.md) · [Sweeps](docs/ml/sweeps.md) · [Pipelines](docs/ml/pipelines.md)
- [Operations](docs/operations/audit.md) · [Reference](docs/reference/build-pipeline.md)

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

- [x] Cron run history (last-run status), pause/suspend, manual "run now"
- [x] More DNS providers (deSEC, Hetzner, DigitalOcean, …)
- [x] Automated addon-data restore
- [x] HTML invite/notification emails
- [x] Preview environments per branch (+ per-project environments)
- [x] Multi-node K3s support
- [x] ARM builds & Raspberry Pi story

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
