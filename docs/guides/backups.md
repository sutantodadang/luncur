# Backups & restore

## Backups

`luncur backup create` (admin) snapshots luncur's whole state into a
tar.gz under `backups/` on the data PVC: a consistent SQLite snapshot
(`VACUUM INTO`), the sealer key, and one logical dump per addon
(`pg_dump -Fc` / redis `SAVE`, taken via pods/exec — credentials never
leave the pod's environment). A failing addon dump becomes a warning; the
backup still completes.

```sh
luncur backup create [--no-upload]
luncur backup list
luncur backup prune            # keep newest backup_keep (default 7)
```

Off-box uploads go to any S3-compatible bucket (AWS/R2/minio/B2) — a
built-in SigV4 client, no SDK:

```sh
luncur config set backup_s3_endpoint https://<s3-endpoint>
luncur config set backup_s3_bucket   my-backups
luncur config set backup_s3_access_key AKIA...
luncur config set backup_s3_secret_key ...   # write-only: reads show "(set)"
luncur config set backup_s3_prefix   luncur  # optional
luncur config set backup_schedule    daily   # or off (default)
luncur config set backup_keep        7
```

With `backup_schedule daily`, the server takes and prunes backups
automatically. An upload failure keeps the local archive and surfaces a
warning.

**Invite email (SMTP):**

```sh
luncur config set smtp_host mail.example.com   # unset = invite emails disabled
luncur config set smtp_port 587                # optional, default 587
luncur config set smtp_user luncur@example.com # optional; enables PLAIN auth
luncur config set smtp_pass ...                # write-only: reads show "(set)"
luncur config set smtp_from luncur@example.com # optional, defaults to smtp_user
```

STARTTLS is used when the server offers it. A send failure (or
unconfigured SMTP) never blocks invite creation — the API returns
`emailed:false` plus a warning, and the invite link can be copied as
before.

**Notifications (deploy & cert events):**

```sh
luncur config set notify_url https://discord.com/api/webhooks/...   # write-only: reads show "(set)"
luncur config set notify_format discord      # generic (default) | discord | slack | telegram
luncur config set notify_events deploy_success,deploy_failed,cert_failed
# telegram: notify_url = https://api.telegram.org/bot<token>/sendMessage
luncur config set notify_telegram_chat 123456789
```

Unset `notify_url` disables the feature entirely. `notify_events` is a CSV
subset of `deploy_success`, `deploy_failed`, `cert_issued`, `cert_failed`;
default when unset is `deploy_failed,cert_failed`. Delivery is best-effort:
one attempt, a 5s timeout, failures logged — a notification never blocks a
deploy or cert issuance. The `generic` format POSTs
`{"event","project","app","deploy_id","status","url","error","time"}`
(`deploy_id` omitted for cert events, `url`/`error` omitted when empty, `time`
RFC3339); `discord`/`slack` POST `{"content"|"text": <message>}`; `telegram`
adds `"chat_id"` from `notify_telegram_chat`.

**Security note:** archives contain the sealer key — whoever can read a
backup can unseal your env vars and addon credentials. The S3 bucket is
the trust boundary; scope its access accordingly.

## Verify

```sh
luncur backup verify /path/to/luncur-YYYYMMDD-HHMMSS.tar.gz
# ok: N files, N tables, integrity=ok, sealer key=true
```

Restores the archive into a throwaway scratch directory, runs `PRAGMA
integrity_check` against the restored DB, and confirms the sealer key is
present — the live data dir is never touched, so it's safe to run against
production archives any time, not just during an incident. This is the
automated restore drill: a backup nobody has restored is not a backup. Run
it after every `backup create`, and on a quarterly cadence against the
latest S3 archive — see
[Disaster recovery](../operations/disaster-recovery.md) for the full drill
checklist, RTO/RPO numbers, and failure-mode breakdown.

## Restoring

`luncur restore` automates the DB/key half; addon data stays guided:

```sh
# on the target host, with the server scaled down
luncur restore /path/to/luncur-YYYYMMDD-HHMMSS.tar.gz --data-dir <data-dir> [--force]

# or straight from the backup bucket
luncur restore <prefix>/luncur-....tar.gz --s3-endpoint https://<s3> \
  --s3-bucket my-backups --s3-access-key ... --s3-secret-key ... --data-dir <data-dir>
```

It validates the archive's `manifest.json` before touching anything and
refuses a data dir whose DB already has projects unless `--force` — which
first copies the current `luncur.db`/`luncur.key` into
`pre-restore-<timestamp>/` inside the data dir. On success it extracts
`luncur.db` and `luncur.key` and prints the remaining guided steps.

Full procedure on a fresh VPS:

1. Provision: `luncur up` (fresh install, any admin password — it will be
   replaced by the restored DB).
2. Stop the server so SQLite is quiescent:
   `kubectl -n luncur-system scale deploy/luncur --replicas=0`.
3. Run `luncur restore` against the data PVC's path. The PVC is a
   `local-path` volume on the node — find it with
   `kubectl -n luncur-system get pvc luncur-data -o jsonpath='{.spec.volumeName}'`
   and look under `/var/lib/rancher/k3s/storage/<volume>/`.
4. Start the server: `kubectl -n luncur-system scale deploy/luncur --replicas=1`,
   then `luncur login` with the restored credentials.
5. Re-create each addon (`luncur addon create ...` with the same names) so
   the StatefulSets exist, then restore data into them (the restore
   command prints these same steps for the dumps it found):
   - Postgres: `kubectl -n <project-ns> exec -i addon-<name>-0 -- sh -c
     'PGPASSWORD="$POSTGRES_PASSWORD" pg_restore -U "$POSTGRES_USER" -d "$POSTGRES_DB" --clean' < addons/<project>-<name>.pgdump`
   - Redis: scale the addon StatefulSet to 0, copy the `.rdb` onto its PVC
     as `dump.rdb`, scale back to 1.
6. Redeploy apps (`luncur deploy` / `git push`) and verify with
   `luncur status`.
