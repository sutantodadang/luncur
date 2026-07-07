# Design notes

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
