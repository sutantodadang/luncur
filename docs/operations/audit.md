# Audit log

luncur logs every mutation — who did what, when — so you can answer "who
deployed this" or "who changed that setting" after the fact. Run `luncur
audit` to see recent activity, or browse the same log in the web UI.

```sh
luncur audit                              # recent rows: ID, TIME, USER, ACTION, TARGET
luncur audit --user alice@example.com     # filter by exact user
luncur audit --contains apps              # filter by substring on action/target
```

Admins can also browse the log at `/ui/audit`.

## What gets logged

Every successful mutating request records one row: who (email), which route,
the request path, and when. This covers the API, the web UI (session +
CSRF-checked form posts), logins, and webhook-triggered deploys (recorded
under the `webhook` user). Failed and read-only (GET) requests are never
recorded. Secret values are never stored, and routes whose path carries a
secret — like `DELETE /v1/invites/{token}` — record the route pattern
instead of the raw path, so the token itself never lands in the log.

## Retention

Rows are pruned opportunistically after each recorded mutation, keeping
`audit_retention_days` (default 90) worth of history; set it to `0` to keep
every row forever:

```sh
luncur config set audit_retention_days 30
```

**Related:** [Settings](../reference/settings.md) · [Security](../reference/security.md)
