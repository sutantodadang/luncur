# luncur — Phase 4 Design

Date: 2026-07-04
Status: Approved (design review with owner, 2026-07-04)
Builds on: [2026-07-03-luncur-phase3-design.md](2026-07-03-luncur-phase3-design.md) (Phase 3 complete, Plans I-L)

## Goal

Close the operational backlog parked across Phases 2-3: email the invites,
automate the disaster-recovery restore, manage tokens from the browser,
issue wildcard certs via DNS-01, upgrade addons in place, and re-adopt an
ejected app. Still one Go binary; still no new Go module dependencies.

## Scope (approved)

| Plan | Contents |
|---|---|
| **M — invite email** | SMTP send of invite links (stdlib net/smtp) |
| **N — restore + token UI** | host-side `luncur restore`; `/ui/tokens` page |
| **O — DNS-01 + wildcard** | pluggable DNS provider (Cloudflare/Route53/RFC2136), DNS-01 ACME path, `*.example.com` domains |
| **P — addon upgrade + adopt** | `addon upgrade --version`; `app adopt` (reverse of eject) |

Order M → P → N → O (small/independent first, biggest last). All four are
largely independent; each gets its own implementation plan.

## Global constraints

- Single Go module, one binary from `cmd/luncur`. **No new Go module
  dependencies.** One binary addition — `nsupdate` (bind-tools) in the
  server image — is gated behind selecting the RFC2136 DNS provider,
  same pattern as `git` and `pg_dump` (see Plan O deviation).
- Server-side apply everywhere, `fieldManager=luncur`. API error envelope
  unchanged. Conventional commits; full build/vet/test before every commit.
- Secrets in settings (SMTP password, DNS provider creds) use the existing
  sealed write-only pattern: sealed at rest (`sealed:` + hex), reads return
  `"(set)"`.
- Tests must not require a cluster, network, or real DNS/SMTP: `Mailer`,
  `dns.Provider`, and command `Runner` interfaces faked; httptest fakes for
  Cloudflare/Route53/S3; the existing fake ACME directory extended for
  DNS-01; restore operates on temp dirs.

## Plan M — invite email

- Settings (all optional; `smtp_pass` sealed write-only): `smtp_host`,
  `smtp_port` (default 587), `smtp_user`, `smtp_pass`, `smtp_from`.
- `internal/mail`: `type Mailer interface { Send(to, subject, body string) error }`;
  a `net/smtp` implementation with STARTTLS built from the settings, and a
  `mailUnconfigured` sentinel when `smtp_host` is unset.
- `POST /v1/invites` body gains optional `"email"`. When set and SMTP is
  configured, the invite link (`<server-url>/ui/register?token=...`) is
  emailed; the API response gains `"emailed": bool`. Unconfigured or send
  failure → invite still created, `"emailed": false` + a `"warning"`.
- CLI: `luncur invite create --email addr@example.com`. UI: an optional
  email field on the invite-create form (users page); a flash note reports
  whether it was sent.

## Plan N — restore + token UI

### `luncur restore` (host command)

- `luncur restore <source> --data-dir <path> [--force]` — `source` is a
  local archive path OR an S3 key (with `--s3-endpoint --s3-bucket
  --s3-access-key --s3-secret-key`; the key is downloaded first). Runs on
  the host like `luncur up`, NOT through the API (a running server can't
  overwrite its own open SQLite).
- Steps: open the tar.gz; validate `manifest.json`; **bootstrap guard** —
  if `<data-dir>/luncur.db` exists and contains any projects, refuse unless
  `--force`; with `--force`, copy the current `luncur.db` + `luncur.key`
  into `<data-dir>/pre-restore-<ts>/` before overwriting. Extract
  `luncur.db` and `luncur.key` into the data dir. Print the addon-data
  restore commands derived from the archive's `addons/*` members (the same
  `pg_restore` / redis rdb steps the Plan K runbook documents) and the
  `kubectl scale` reminder.
- Restore automates the DB/key half; addon-data restore and the cluster
  scale-down/up stay guided (documented one-liners — automating cluster
  lifecycle from a possibly-remote CLI is fragile).

### Token UI

- `/ui/tokens` (uiPage, any user) — table of the caller's own tokens (name,
  created, last used, expires) with per-row revoke buttons (CSRF); the
  session token appears as `session` and revoking it logs the browser out.
  Nav gains a "tokens" link for everyone. Mirrors `luncur token list/revoke`.

## Plan O — DNS-01 + wildcard certs

### DNS provider abstraction

- `internal/dns`: `type Provider interface { Present(ctx, fqdn, value string) error; CleanUp(ctx, fqdn, value string) error }` where `fqdn` is
  `_acme-challenge.<domain>` and `value` is the TXT contents. Three impls:
  - **Cloudflare** — REST (`https://api.cloudflare.com/client/v4`), bearer
    token; resolve the zone by longest-suffix match, upsert then delete the
    TXT record. Pure `net/http` + `encoding/json`.
  - **Route53** — `ChangeResourceRecordSets`; SigV4 reuses the Plan K
    signer (extract it to `internal/awssig` shared by `s3` and `dns`);
    hosted-zone lookup by domain; XML request/response via `encoding/xml`.
  - **RFC2136** — shells out to `nsupdate` (bind-tools): writes a script
    (server, TSIG key + algorithm, `update add`/`update delete` for the
    TXT), pipes it to `nsupdate` via a `Runner` interface (faked in tests).
- Settings (`dns_provider` = `cloudflare`|`route53`|`rfc2136`|`none`,
  default none; provider creds sealed write-only): `dns_cloudflare_token`;
  `dns_route53_access_key`, `dns_route53_secret_key`, `dns_route53_region`;
  `dns_rfc2136_server`, `dns_rfc2136_tsig_name`, `dns_rfc2136_tsig_secret`,
  `dns_rfc2136_tsig_algo`.

### DNS-01 issuance path

- `internal/acme.Issuer` gains a pluggable `Solver` so the order machinery
  is shared: `type Solver interface { Setup(ctx, domain, token, keyAuth string) (cleanup func(), err error) }`. The existing HTTP-01 challenge
  store becomes the default solver; a DNS-01 solver computes the TXT value
  (`base64url(sha256(keyAuth))`), calls `dns.Provider.Present`, and waits
  for propagation by polling the domain's authoritative nameservers for the
  TXT (timeout ~2 min) before returning.
- The builtin cert manager picks DNS-01 when the hostname is a wildcard
  (`*.`) OR when `dns_provider != none` for that install; otherwise HTTP-01
  as today. `traefik`/`cert-manager` providers are unchanged (they own
  their own solving).
- Domains: `AddDomain` accepts `*.example.com` (one leading `*.`, remainder
  a normal hostname). A wildcard domain with `dns_provider == none` → 400
  (wildcards can't be validated over HTTP-01). Cert issuance uses DNS-01
  transparently; UI/CLI surface unchanged.

### Deviation

- RFC2136 requires the `nsupdate` binary in the server image (added to the
  release image; documented). This is a binary tool, not a Go module — the
  no-new-dependencies rule is about Go modules; `git`, `pg_dump`, and now
  `nsupdate` are runtime binaries selected on demand. Cloudflare and
  Route53 need no binaries.

## Plan P — addon upgrade + re-adopt

- `luncur addon upgrade <name> --version V --project P` /
  `POST /v1/projects/{p}/addons/{name}/upgrade` `{"version":"V"}` — updates
  the addon row's `version`, re-renders the StatefulSet with the new image
  tag, SSA applies (rolling restart). Response/CLI print a warning: "major
  version DB upgrades may require manual migration — take a backup first."
  Data (the PVC) is untouched.
- `luncur app adopt <name> --project P` /
  `POST /v1/projects/{p}/apps/{app}/adopt` — only valid on an ejected app
  (else 409 `not_ejected`); clears `ejected`, re-renders current state and
  SSA-applies it onto the still-running objects (reclaiming
  `fieldManager=luncur`). Eject becomes reversible. Store gains
  `Store.SetAppAdopted(id int64) error` (sets ejected=0). UI: the ejected
  note gains an "adopt" button; on adopt the normal management UI returns.

## Data model changes (summary)

- **No new tables.** New settings keys only (SMTP + DNS provider creds, all
  sealed write-only). `addons.version` (exists) is updated by upgrade;
  `apps.ejected` (exists) is cleared by adopt; wildcard domains are ordinary
  `domains.hostname` values.

## Error handling

- Email: unconfigured or send failure → invite still created, `emailed:false`
  + warning (never blocks invite creation).
- Restore: non-empty DB without `--force` → refuse with a clear message;
  corrupt/missing manifest → abort before touching the data dir; S3 download
  failure → abort. Pre-restore backup copy is taken before any overwrite.
- DNS-01: provider API error or propagation timeout → domain `cert_status`
  `failed` + message (same as HTTP-01 failures); the app keeps serving.
- Addon upgrade on a missing addon → 404; adopt on a non-ejected app → 409
  `not_ejected`.

## Testing strategy

- Unit + fakes: `Mailer` (email send), `dns.Provider` per impl against
  httptest (Cloudflare JSON, Route53 XML) and a `Runner` fake (nsupdate),
  DNS-01 solver against the extended fake ACME directory + a fake resolver,
  restore against temp dirs (guard / force+pre-backup / S3-source via
  httptest), SigV4 extraction re-runs Plan K's known-answer vectors.
- Server tests with fake kube: token UI, addon upgrade (StatefulSet image
  changed), adopt (ejected→managed, objects re-applied).
- Manual on the owner's VPS: email an invite; restore a real archive onto a
  fresh box; issue a wildcard cert via each DNS provider; upgrade a postgres
  addon; eject then adopt an app.

## Out of scope (Phase 4)

Email templating beyond the invite link (HTML mail, branding), DNS
providers beyond the three, automated restore of addon *data* (stays
guided), addon major-version auto-migration, HA/multi-replica addons,
per-app DNS-provider override (install-level only), un-eject preserving a
diverged cluster state (adopt re-applies luncur's rendered state, winning
any drift).
