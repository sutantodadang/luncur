# luncur — Phase 2 Design

Date: 2026-07-03
Status: Approved (design review with owner, 2026-07-03)
Builds on: [2026-07-02-luncur-phase1-design.md](2026-07-02-luncur-phase1-design.md) (Phase 1 complete, Plans A-D)

## Goal

Close the gap between "toy PaaS" and "daily driver": deploy by `git push`,
serve real domains with TLS, undo bad deploys, and let a small team in
through the web UI — while keeping the one-binary, minimal-moving-parts
ethos.

## Scope (approved)

| Plan | Contents |
|---|---|
| **E — git push receiver** | SSH server in `luncur serve`, per-user SSH keys, push → build pipeline |
| **F — custom domains + TLS** | domain CRUD, pluggable cert providers: builtin ACME (default) / Traefik / cert-manager |
| **G — rollback + hardening** | rollback CLI+UI, `token list/revoke`, scoped ClusterRole, CSRF tokens |
| **H — web UI growth** | invite links, user management UI, YAML override editor |

Each plan gets its own implementation plan document; E → F → G → H is the
suggested order but F/G/H are independent of each other once E's image
change (git binary) lands.

## Global constraints

- Single Go module, one binary from `cmd/luncur`.
- **Dependency policy (relaxed from Phase 1):** `golang.org/x/crypto/ssh`
  and `golang.org/x/crypto/acme` allowed (x/crypto family). No go-git; git
  operations shell out to the `git` binary, which is **added to the luncur
  server image** by the release pipeline.
- Server-side apply everywhere, `fieldManager=luncur`.
- API error envelope unchanged. All commits conventional style;
  `go build ./... && go vet ./... && go test ./...` before every commit.
- Tests must not require a cluster, root, Linux, or network: fake
  clientsets, in-process SSH clients, and a local ACME test server
  (Pebble in CI only; unit tests use an httptest ACME directory).

## Plan E — git push receiver (SSH)

### Architecture

- SSH listener inside `luncur serve` (`x/crypto/ssh`), port 2222 in-pod,
  exposed as NodePort **30022** Service `luncur-ssh` (same pattern as the
  registry's 30500). `--ssh-listen` flag; empty disables.
- Host key: ed25519, generated on first boot, persisted as K8s Secret
  `luncur-ssh-hostkey` (survives pod restarts; clients don't see key
  changes).
- Auth: public-key only. Key fingerprint → `ssh_keys` row → user. No
  passwords, no interactive sessions — the only accepted exec requests are
  `git-receive-pack '/<project>/<app>.git'` (and `git-upload-pack` is
  rejected with "luncur is push-only").

### Data model

```
ssh_keys (id, user_id, name, public_key, fingerprint UNIQUE, created_at)
```

CLI: `luncur ssh-key add [--name] [path-to-pub]` (defaults to
`~/.ssh/id_*.pub` discovery), `ssh-key list`, `ssh-key remove <id>`.

### Push flow

```
git push ssh://git@<ip>:30022/<project>/<app>.git main
  → auth fingerprint → user; user must be member of <project>
  → temp dir: git init --bare; git-receive-pack streams the pack in
  → pushed ref must be the app's configured branch (default main);
    other refs rejected with a helpful message
  → git archive <branch> → tarball → existing deploy pipeline
    (create deployment row, build Job, render, apply, rollout watch)
  → build/rollout progress streamed back to the git client on the
    side-band channel (Heroku-style); non-zero exit on failure
  → temp bare repo deleted (no server-side repo storage)
```

- `luncur init` offers to add a git remote named `luncur`.
- App model unchanged: a push supplies source to an existing app exactly
  like a tarball deploy does; `apps.source_type` keeps its current value,
  so git-URL apps keep their deploy-from-git button.

## Plan F — custom domains + TLS

### Domain CRUD (provider-independent)

- `luncur domain add <app> <hostname>`, `domain list <app>`,
  `domain remove <app> <hostname>`; same actions in the app page UI.
- On add: DNS A/AAAA lookup — warn (not block) when it doesn't resolve to
  the node IP. Ingress re-rendered with the extra host rule. The sslip.io
  host always remains.
- `domains` table gains `cert_status` (none|pending|issued|failed|external),
  `cert_error`, `cert_expires_at`.

### Cert providers

Install-level setting `cert_provider` = `builtin` (default) | `traefik` |
`cert-manager`, stored in a new `settings` table (key/value), set via
`luncur up --cert-provider` or changed with `luncur config set`.
Provider choice affects only Ingress rendering and who issues certs;
domain CRUD, DNS checks, and UI are shared.

**builtin** — full ACME implementation in `luncur serve`:
- Account key in K8s Secret `luncur-acme-account`; `acme_email` +
  `acme_directory` settings (directory overridable for staging/Pebble).
- HTTP-01: custom-domain Ingresses route
  `/.well-known/acme-challenge/` → luncur Service; challenges served from
  an in-memory store.
- Issued cert+key → TLS Secret `tls-<app>-<sha8(hostname)>` in the app
  namespace; Ingress `tls:` block references it.
- Renewal goroutine: daily sweep, re-issue when < 30 days to expiry;
  status transitions recorded in `domains`.

**traefik** — thin, K3s-only:
- `luncur up` writes a `HelmChartConfig` (namespace kube-system) enabling
  Traefik's ACME resolver `le` + a small PVC for `acme.json`.
- App Ingress rendered with
  `traefik.ingress.kubernetes.io/router.tls.certresolver: le` annotation,
  no `tls:` secret block. UI shows `cert_status=external`
  ("managed by Traefik").
- Selecting this provider on a non-K3s `--kubeconfig` install is an error.

**cert-manager** — thin integration, NOT installed by luncur:
- Selection checks the cert-manager CRDs exist (clear error otherwise).
- luncur creates ClusterIssuer `luncur-le` (LE HTTP-01, ingress class
  traefik) and renders app Ingresses with
  `cert-manager.io/cluster-issuer: luncur-le` + `tls:` block (cert-manager
  populates the Secret). UI reads expiry from the Secret.

## Plan G — rollback + hardening

### Rollback

- `luncur rollback <app> [--deploy N]` — default target: newest deployment
  with status `live` older than the current one. UI: rollback button per
  history row.
- Mechanics: verify the target image still exists in the embedded registry
  (HEAD manifest); create a NEW deployment row (`status=deploying`,
  `image_ref` copied, `rolled_back_from=<old id>`); run the existing
  render+apply+watch path — no build. Registry GC stays out of scope
  (Phase 3).
- Missing image → 409 with explanation.

### Token lifecycle

- `luncur token list` (id, name, created, last_used, expires) and
  `luncur token revoke <id>`. Web sessions appear as name `session`.
  Revoking a session token logs that browser out.

### Scoped ClusterRole

- Replace the `cluster-admin` binding with ClusterRole `luncur` enumerating
  what luncur actually touches: namespaces, deployments, replicasets(read),
  services, ingresses, secrets, configmaps, pods(+log), jobs, pvcs,
  nodes(read), serviceaccounts, events(read); plus
  `helm.cattle.io/HelmChartConfig` (traefik provider) and cert-manager
  CRDs (cert-manager provider). Golden test pins the rule list.

### CSRF

- Per-session random token, set alongside the session cookie, embedded as
  a hidden field in every `/ui/` form, verified on every `/ui/` POST.
  `SameSite=Strict` stays as defense-in-depth.

## Plan H — web UI growth

### Invites + user management

- Admin-only page `/ui/users`: user list (email, role, created, token
  count), delete user; "create invite" (choose role, 7-day expiry) →
  copyable link `/ui/register?token=...`. No email sending.
- `/ui/register`: email + password form, validates + consumes the invite
  (single use), creates user, logs them in.
- CLI parity: `luncur invite create --role member`, `invite list`,
  `invite revoke <token>`. `invites` table gains `created_by`, `used_by`,
  `used_at`.

### YAML override editor

- App page → "edit YAML" per rendered kind: GET shows the final rendered
  YAML in a `<textarea>`; POST diffs submitted YAML against the base
  render and stores the strategic-merge patch in `overrides` — the exact
  code path `luncur edit` uses — then re-syncs when the app is live.
  Invalid YAML / unknown fields → error re-render with message.
- Templates stay stdlib `html/template`. **No templ swap** (YAGNI;
  revisit only if UI complexity forces it).

## Data model changes (summary)

- New: `ssh_keys`, `settings` (key/value).
- `domains`: + `cert_status`, `cert_error`, `cert_expires_at`.
- `deployments`: + `rolled_back_from`.
- `invites`: + `created_by`, `used_by`, `used_at`.
- All via the existing `migrate()` ALTER-if-missing mechanism.

## Error handling

- ACME failure → `cert_status=failed` + `cert_error` shown in UI/CLI; app
  keeps serving HTTP and the sslip.io host. Retry on next renewal sweep or
  explicit `luncur domain retry <app> <hostname>`.
- Push errors reach the git client on stderr: auth failure, non-member,
  wrong branch, build failure (log tail). Exit code non-zero.
- Rollback to a garbage-collected/missing image → 409, message names the
  image.
- Provider misconfiguration (traefik on non-K3s, cert-manager CRDs absent)
  → fail at selection time, not at first domain add.

## Testing strategy

- **Unit + golden:** Ingress rendering per provider × domain set;
  ClusterRole rules; SSH auth (in-process `x/crypto/ssh` client against
  the server with a fake store); ACME client against an httptest directory
  server; override diff round-trip (editor path).
- **Integration (CI, k3d):** push → build → live e2e (git CLI in the test
  job); rollback e2e; domain add with builtin provider against Pebble.
- **Manual (owner's VPS + real domain):** real Let's Encrypt issuance on
  all three providers; git push from a laptop; invite flow in a second
  browser.

## Out of scope (Phase 2)

Registry GC, `--eject`, addons (Postgres/Redis), metrics, backups,
multi-node, wildcard certs / DNS-01, per-app cert provider override,
email delivery for invites, git pull/clone from luncur (push-only).
