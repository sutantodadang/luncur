# Doctor / diagnostics

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
