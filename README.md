# luncur

Tiny self-hosted PaaS on K3s. One Go binary, SQLite, deploys as simple as
Heroku — with an escape hatch to the real Kubernetes objects.

Status: Phase 1 in progress. Working today:

```sh
luncur serve --db luncur.db --bootstrap-admin you@example.com:yourpassword
luncur login http://localhost:8080
luncur whoami
luncur user add teammate@example.com --password ...
```

Design docs: `docs/superpowers/specs/`. Plans: `docs/superpowers/plans/`.
