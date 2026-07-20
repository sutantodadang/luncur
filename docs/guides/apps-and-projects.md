# Apps & projects

Everything in luncur lives inside a **project** (a namespace for related
apps and addons) and every project has **apps** (web services, workers, or
cron jobs) inside it. This page covers logging in, creating projects/apps,
choosing the right app kind, managing who has access, and the web UI that
mirrors all of it.

## Log in

```sh
luncur login http://localhost:8080
luncur whoami
```

## Create a project and an app

```sh
luncur project create myproj
luncur project list
luncur project add-member myproj member@example.com
luncur project add-member myproj viewer@example.com --role viewer

luncur app create myapp --project myproj --port 8080
luncur app list --project myproj
luncur app info myapp --project myproj
luncur app raw myapp --project myproj
```

## Choose an app kind — web, worker, cron

Apps default to `web`; pass `--kind` for the other two:

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

## Keep a web app cluster-only with `--internal`

A web app that should never be reachable from outside the cluster (e.g. an
internal AI/microservice another app in the same project calls over HTTP)
can be created with `--internal`:

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

## Project member roles

Project membership has its own role, separate from the global admin/member
role (below). `--role member` (the default) can create, deploy, and
otherwise modify anything in the project. `--role viewer` grants read-only
access — a viewer can browse apps, deploys, logs, and metrics but any
mutating request (deploy, scale, env changes, add-ons, etc.) is rejected
with a 403 `read_only` error, both via the API and the web UI. Global admins
always have full write access to every project regardless of membership.

## Add teammates and manage tokens

```sh
luncur user add teammate@example.com --password ... [--role admin|member]

# Invite teammates instead of setting a password for them (admin only)
luncur invite create [--role admin|member] [--email addr]  # prints a one-time /ui/register link; --email sends it via SMTP
luncur invite list                           # TOKEN, ROLE, EXPIRES, USED
luncur invite revoke <token>
```

!!! tip
    `luncur invite create` is usually nicer than `luncur user add` for
    onboarding — it hands the teammate a link instead of you choosing a
    password for them.

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

## Web UI

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
see [CONTRIBUTING.md](https://github.com/sutantodadang/luncur/blob/main/CONTRIBUTING.md) for the Tailwind regen command.

**Related:** [Deploying](deploying.md) · [Domains & TLS](domains-and-tls.md) ·
[Addons](addons.md) · [Volumes](volumes.md)
