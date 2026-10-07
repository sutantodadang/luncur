# Security

What this documents: luncur's trust boundaries — cluster RBAC, CSRF, secrets
at rest, and webhook auth — and the deliberate tradeoffs behind each.

## Cluster RBAC & CSRF

luncur's own access to the cluster is a scoped `ClusterRole` — namespaces,
Deployments, Jobs, Ingresses, and the specific CRDs luncur touches
(HelmChartConfig, cert-manager's ClusterIssuer) — instead of
`cluster-admin`; the rule set is golden-tested in
`internal/up/manifests_test.go`. Every web-UI form (scale, env, domains,
deploy, rollback, login) carries a CSRF token: a `luncur_csrf` cookie
mirrored in a hidden `_csrf` field, checked on every POST before it runs.

## Self-healing ClusterRole and the `escalate` tradeoff

The server self-heals this ClusterRole at every boot (`luncur update` only
swaps the Deployment image, so a release that adds a permission — metrics
nodes, then PodDisruptionBudgets — used to leave the field stuck on the old
rule set until someone re-ran `luncur up`). To do this without
`cluster-admin`, the ClusterRole grants itself a narrow `escalate` verb on
`clusterroles`, scoped by `resourceNames` to the single `luncur` ClusterRole
— Kubernetes' escalation-prevention rule otherwise blocks a ServiceAccount
from granting rules it doesn't already hold.

!!! warning "Deliberate tradeoff, not a zero-risk one"
    A compromised luncur server could use the `escalate` verb to extend its
    own role. This is accepted because `luncur-system` already runs
    privileged workloads (the BuildKit builder), so it isn't a new trust
    boundary — but it is a real capability a compromised server would have.

One-time caveat: upgrading from a version without this feature still needs
one `luncur up` — the old live ClusterRole predates the `escalate` rule, so
the new binary has no permission to self-apply it until that first manual
apply.

## Secrets at rest

Secrets never sit in plaintext: env vars, addon credentials, and sensitive
settings (S3 secret key, SMTP password, DNS provider tokens) are sealed at
rest with AES-256-GCM; the sealed settings are write-only through the API
(reads show `(set)`).

## Pod security levels

Every app gets a pod security level (`luncur app set <app> --security …`, or
the Wire → Rollout card):

| Level | What luncur renders |
|---|---|
| `baseline` (default) | seccomp `RuntimeDefault`; `allowPrivilegeEscalation: false`; `NET_RAW` dropped (the rest of the runtime's default capabilities kept, so entrypoints that chown or setuid on start still work); no service-account token mounted |
| `restricted` | baseline + `runAsNonRoot: true` and every capability dropped except `NET_BIND_SERVICE` — for images that run as a non-root user |
| `relaxed` | nothing (luncur's behavior before levels existed) |

`allowPrivilegeEscalation: false` is the one baseline setting that can break
an image: ones that rely on setuid binaries such as `sudo` at runtime. The
rollout gate catches such an image at deploy time and rolls back. Set
`--security relaxed` for that app.

Project namespaces also enforce the Kubernetes `baseline` Pod Security
Admission profile.

## Network isolation and builds

With `network_isolation` on, each environment namespace admits traffic only
from:

- pods in the same namespace;
- the ingress controller (`kube-system`);
- the **luncur server pod** in `luncur-system`, for the panel's addon-UI
  proxies and one-click forwards.

The `luncur-system` peer is a namespace **and** pod selector
(`app.kubernetes.io/name=luncur`), not the whole namespace. User BuildKit
builds run in `luncur-system`, and a build's `RUN` step must not be able to
reach isolated tenants. The policy is re-applied at every server start.

## Webhook auth

The deploy webhook endpoint (`POST /hooks/apps/{project}/{app}`) is
unauthenticated at the HTTP layer by design — a git provider posts to it
directly, so there's no bearer token to present. The HMAC/token check *is*
the auth: every failure (unknown project/app, webhook disabled, unseal
failure, bad signature) answers with the byte-identical 401 body, so the
endpoint can't be used to probe whether a project or app exists. The
request body is capped at 1 MiB before it's read. The webhook secret is
sealed at rest the same way env vars are (AES-256-GCM) and is only ever
shown in plaintext once, in the response to `webhook enable`.

**Replay protection.** A signature only proves a body came from the
provider, so a captured request could otherwise be replayed to start unlimited
deploys or runs.

- After the signature check, app, project and pipeline webhooks record the
  provider's delivery id: `X-GitHub-Delivery`, `X-Gitea-Delivery`,
  `X-Gitlab-Event-UUID`, or `X-Luncur-Delivery` for your own senders.
- A repeat answers `200 {"duplicate":true}` and does nothing.
- Ids are kept for 72 hours. Senders that send no delivery id are accepted as
  before.

## Browser caching

Authenticated web panel responses send `Cache-Control: no-store`. The Wire tab
shows env values, and a shared machine's disk cache or back-forward cache must
not keep them after logout.

**Related:** [Audit log](../operations/audit.md) · [Settings](settings.md) · [Design notes](design-notes.md)
