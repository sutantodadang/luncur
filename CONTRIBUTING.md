# Contributing to luncur

Thanks for considering a contribution! luncur is intentionally small and
boring on the inside — that's a feature. This page tells you how to get a
dev loop running and what a good PR looks like.

## Dev setup

All you need is Go (see `go.mod` for the version). No cluster required for
the test suite — Kubernetes, DNS, SMTP, S3, and ACME are all faked.

```sh
git clone https://github.com/sutantodadang/luncur.git
cd luncur
go build ./... && go vet ./... && go test ./...
```

Run the server locally against a kubeconfig (optional — most endpoints work
without a cluster and answer 503 for the ones that need it):

```sh
go run ./cmd/luncur serve --db luncur.db --listen :8080 \
  --bootstrap-admin admin@example.com:password
```

## Ground rules

- **One Go module, one binary.** No new Go module dependencies — the stack
  is stdlib + a handful of pinned deps (cobra, x/crypto, k8s client). If a
  feature seems to need a new dependency, open an issue first; so far
  everything (SigV4, SMTP, ACME DNS-01, S3) has fit in the stdlib.
- **Tests must not require a network or a cluster.** Fake it: httptest
  servers, the fake dynamic kube client, the fake ACME directory
  (`internal/acme/acmetest`), interface seams (`Mailer`, `dns.Provider`,
  `Runner`).
- **TDD, small commits.** Write the failing test, make it pass, commit.
  Conventional commit messages (`feat:`, `fix:`, `docs:`, `refactor:`).
- **Full verify before every commit:**
  `go build ./... && go vet ./... && go test ./...`
- **Server-side apply everywhere**, `fieldManager=luncur`. Keep the API
  error envelope (`{"error":{"code":...,"message":...}}`) intact.

## Where things live

| Path | What |
|---|---|
| `cmd/luncur` | main — everything else is `internal/` |
| `internal/cli` | cobra commands (thin; logic lives in the client/server) |
| `internal/server` | REST API + web UI (stdlib `html/template` + SSE) |
| `internal/store` | SQLite (schema, queries) |
| `internal/render` | app manifests (Deployment/Service/Ingress) |
| `internal/acme`, `internal/dns` | TLS issuance (HTTP-01 + DNS-01) |
| `internal/kube`, `internal/build` | cluster client, build pipeline |
| `docs/superpowers/specs` | phase design docs — the "why" behind features |
| `docs/superpowers/plans` | per-feature implementation plans |

Every shipped feature has a design spec and an implementation plan in
`docs/superpowers/` — reading the relevant one is the fastest way to
understand a subsystem before changing it.

## Licensing of contributions

luncur is licensed under the [AGPL-3.0](LICENSE), with commercial licenses
offered separately by the maintainer (dual licensing). By submitting a
contribution you agree that it is licensed under the AGPL-3.0 and that you
grant the maintainer the right to also distribute it under luncur's
commercial license terms. If that's a problem for a specific contribution,
say so in the PR and we'll talk before merging.

## Reporting bugs / proposing features

Open a GitHub issue. For features, a sentence on the use case beats a long
spec — scope discussions happen in the issue before any code.
