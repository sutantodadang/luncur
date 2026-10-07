# Template gallery

What this documents: one-click installs of popular self-hosted apps, each with
its database, volumes and configuration wired up.

```sh
luncur template list
luncur template install n8n --project tools
luncur template install gitea --project tools --name git --env staging
luncur template install umami --project web -e DISABLE_TELEMETRY=1   # extra env wins over the template
```

| Template | What it is | Brings |
|---|---|---|
| `n8n` | Workflow automation | Postgres, 1Gi volume |
| `umami` | Privacy-friendly web analytics | Postgres |
| `gitea` | Git hosting with issues, PRs and CI | Postgres, 5Gi volume |
| `metabase` | BI dashboards | Postgres |
| `uptime-kuma` | Monitoring with notifications | 1Gi volume |
| `vaultwarden` | Bitwarden-compatible password manager | 1Gi volume |

An install:

1. creates the app (web, the template's port, health path and resources);
2. adds the volumes;
3. creates the addons (named `<app>-<key>`, e.g. `n8n-db`) and attaches them;
4. writes the app's env from the addons' real credentials (stored sealed,
   like any env var);
5. deploys the pinned image once the addons are ready.

Each step is reported (`✓`/`✗`). If one fails, you see exactly what was
created and can finish with ordinary commands. The first deploy goes through
the rollout gate like any other.

The project page's **From template** gallery does the same in one click.

Templates are embedded in the binary and reviewed like code. Their images are
pinned to a tag (never `latest`), and every template is parsed and resolved in
luncur's test suite. To propose a new one, add a YAML file under
`internal/templates/catalog/`:

```yaml
name: n8n
title: n8n
description: Workflow automation with 400+ integrations, backed by Postgres.
category: automation
app: {image: n8nio/n8n:2.42.4, port: 5678, health_path: /healthz, memory: 512Mi}
volumes: [{name: data, path: /home/node/.n8n, size_gb: 1}]
addons: [{key: db, type: postgres}]
env:
  DB_POSTGRESDB_HOST: ${addon.db.host}
  DB_POSTGRESDB_PASSWORD: ${addon.db.password}
  N8N_ENCRYPTION_KEY: ${random:32}
  WEBHOOK_URL: ${app.url}/
```

Placeholders:

- `${addon.<key>.host|port|user|password|database|url}`
- `${random:N}` (8–128 hex characters)
- `${app.url}`
- `${app.host}`

**Related:** [Addons](addons.md) · [Volumes](volumes.md)
