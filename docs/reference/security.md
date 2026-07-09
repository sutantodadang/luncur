# Security

luncur's own access to the cluster is a scoped `ClusterRole` — namespaces,
Deployments, Jobs, Ingresses, and the specific CRDs luncur touches
(HelmChartConfig, cert-manager's ClusterIssuer) — instead of
`cluster-admin`; the rule set is golden-tested in
`internal/up/manifests_test.go`. Every web-UI form (scale, env, domains,
deploy, rollback, login) carries a CSRF token: a `luncur_csrf` cookie
mirrored in a hidden `_csrf` field, checked on every POST before it runs.

The server self-heals this ClusterRole at every boot (`luncur update` only
swaps the Deployment image, so a release that adds a permission — metrics
nodes, then PodDisruptionBudgets — used to leave the field stuck on the old
rule set until someone re-ran `luncur up`). To do this without
`cluster-admin`, the ClusterRole grants itself a narrow `escalate` verb on
`clusterroles`, scoped by `resourceNames` to the single `luncur` ClusterRole
— Kubernetes' escalation-prevention rule otherwise blocks a ServiceAccount
from granting rules it doesn't already hold. This is a deliberate tradeoff:
a compromised luncur server could use it to extend its own role, but
`luncur-system` already runs privileged workloads (the BuildKit builder),
so it isn't a new trust boundary. One-time caveat: upgrading from a version
without this feature still needs one `luncur up` — the old live
ClusterRole predates the `escalate` rule, so the new binary has no
permission to self-apply it until that first manual apply.

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
