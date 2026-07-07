# Security

luncur's own access to the cluster is a scoped `ClusterRole` — namespaces,
Deployments, Jobs, Ingresses, and the specific CRDs luncur touches
(HelmChartConfig, cert-manager's ClusterIssuer) — instead of
`cluster-admin`; the rule set is golden-tested in
`internal/up/manifests_test.go`. Every web-UI form (scale, env, domains,
deploy, rollback, login) carries a CSRF token: a `luncur_csrf` cookie
mirrored in a hidden `_csrf` field, checked on every POST before it runs.

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
