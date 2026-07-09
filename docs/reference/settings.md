# Settings

`luncur config set <key> <value>` writes an install-level setting; secret
values are write-only (reads show `(set)`). This table collects every
setting documented elsewhere in this manual, assembled from the pages
where each one is explained in context.

| Key | Default | Effect | Where documented |
|---|---|---|---|
| `acme_email` | unset | ACME account email used by the `builtin` cert provider and passed to `traefik`/`cert-manager`'s issuer config | [Domains & TLS](../guides/domains-and-tls.md) |
| `dns_provider` | `none` | Enables DNS-01 validation (`cloudflare`, `route53`, `rfc2136`, or `none`) — required for wildcard domains | [Domains & TLS](../guides/domains-and-tls.md) |
| `dns_cloudflare_token` | unset (write-only) | Cloudflare API token for publishing DNS-01 TXT records | [Domains & TLS](../guides/domains-and-tls.md) |
| `dns_route53_access_key` / `dns_route53_secret_key` | unset (secret write-only) | Route53 credentials for DNS-01 | [Domains & TLS](../guides/domains-and-tls.md) |
| `dns_route53_region` | unset (optional) | Route53 region override | [Domains & TLS](../guides/domains-and-tls.md) |
| `dns_rfc2136_server` / `dns_rfc2136_tsig_name` / `dns_rfc2136_tsig_secret` | unset (secret write-only) | nsupdate/TSIG target and credentials for RFC2136 DNS-01 (e.g. BIND) | [Domains & TLS](../guides/domains-and-tls.md) |
| `dns_rfc2136_tsig_algo` | `hmac-sha256` | TSIG algorithm for RFC2136 | [Domains & TLS](../guides/domains-and-tls.md) |
| `registry_keep` | `10` | Images kept per app by `registry gc`, plus the live and newest images regardless of position | [Registry GC](../operations/registry-gc.md) |
| `build_cache` | on | Set to `off` to disable the per-app BuildKit registry-backed layer cache | [Build pipeline](build-pipeline.md) |
| `build_timeout_minutes` | `15` (1-720) | Time budget before an in-progress build is given up on and marked failed | [Build pipeline](build-pipeline.md) |
| `backup_s3_endpoint` / `backup_s3_bucket` / `backup_s3_access_key` / `backup_s3_secret_key` | unset (secret write-only) | S3-compatible bucket target for off-box backup uploads | [Backups & restore](../guides/backups.md) |
| `backup_s3_prefix` | unset (optional) | Key prefix for uploaded backup archives | [Backups & restore](../guides/backups.md) |
| `backup_schedule` | `off` | Set to `daily` to take and prune backups automatically | [Backups & restore](../guides/backups.md) |
| `backup_keep` | `7` | Newest backups kept by `luncur backup prune` | [Backups & restore](../guides/backups.md) |
| `smtp_host` | unset | Unset disables invite emails entirely | [Backups & restore](../guides/backups.md) |
| `smtp_port` | `587` | SMTP port | [Backups & restore](../guides/backups.md) |
| `smtp_user` / `smtp_pass` | unset (password write-only) | SMTP credentials; setting `smtp_user` enables PLAIN auth | [Backups & restore](../guides/backups.md) |
| `smtp_from` | defaults to `smtp_user` | From address for invite emails | [Backups & restore](../guides/backups.md) |
| `notify_url` | unset (write-only) | Unset disables deploy/cert notifications entirely | [Backups & restore](../guides/backups.md) |
| `notify_format` | `generic` | Webhook payload shape: `generic`, `discord`, `slack`, or `telegram` | [Backups & restore](../guides/backups.md) |
| `notify_events` | `deploy_failed,cert_failed,app_unhealthy,backup_failed` | CSV subset of `deploy_success`, `deploy_failed`, `cert_issued`, `cert_failed`, `app_unhealthy`, `backup_failed` | [Backups & restore](../guides/backups.md) |
| `notify_telegram_chat` | unset | Chat id for the `telegram` notify format | [Backups & restore](../guides/backups.md) |
| `audit_retention_days` | `90` | Audit rows older than this are pruned opportunistically; `0` keeps every row forever | [Audit log](../operations/audit.md) |
| `gpu_idle_minutes` | unset (disabled) | Per-instance idle scale-to-zero timeout for rented GPU nodes with no GPU pod scheduled | [GPU cloud](../ml/gpu-cloud.md) |
| `train_gang_timeout_minutes` | `10` | How long a multi-node training run waits for all pods to schedule together before the Job is torn down; `0` disables the guard | [Training](../ml/training.md) |
| `pipeline_engine` | `native` | Default orchestrator engine for pipeline runs when a pipeline doesn't pin its own `engine`: `native` or `argo` (`luncur argo install` first) | [Pipelines](../ml/pipelines.md) |
| `metrics_token` | unset (write-only) | Bearer token gating `GET /metrics/prometheus`; unset 404s the endpoint | — |
