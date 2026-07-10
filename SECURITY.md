# Security Policy

luncur manages other people's servers, secrets, and TLS certificates, so
security reports are taken seriously and handled with priority.

## Supported versions

Only the latest release receives security fixes. Upgrade with
`luncur update` (or re-run `luncur up` when a fix touches RBAC or
rendered manifests — the release notes will say so).

## Reporting a vulnerability

Please do **not** open a public issue for security problems.

- Preferred: [GitHub private vulnerability reporting](https://github.com/sutantodadang/luncur/security/advisories/new)
- Alternatively: email sutantodadang@gmail.com with `[luncur security]`
  in the subject.

Include what you can: affected version (`luncur version`), reproduction
steps, and impact. You will get an acknowledgement within a few days.
Please give us a reasonable window to ship a fix before disclosing
publicly; you will be credited in the release notes unless you prefer
otherwise.

## Scope notes

- The web panel and API are designed to be internet-facing; issues in
  authentication, session handling, RBAC, tenant isolation between
  projects, or the build pipeline are all in scope.
- Secrets are sealed at rest (AES-256-GCM) — anything that leaks
  plaintext env vars, addon credentials, or the sealer key is high
  severity.
- Dependency and Go toolchain vulnerabilities are scanned in CI with
  govulncheck and updated via Dependabot.
