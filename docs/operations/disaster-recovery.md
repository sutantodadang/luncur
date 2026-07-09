# Disaster recovery

luncur's control plane (users, apps, deploy history, settings) lives in a
single SQLite file on the data PVC; the apps it deployed live as normal
Kubernetes objects in K3s. Those two facts drive everything below: most
failures don't touch running apps at all, and the one that does (losing the
data PVC) is a restore, not a rebuild.

## Failure model

| Failure | Effect on running apps | Effect on control plane | Recovery |
|---|---|---|---|
| luncur server pod restarts | Unaffected — Deployments keep serving | API/panel unavailable for seconds while the Deployment reschedules the pod | Automatic (Kubernetes) |
| Node running luncur dies | Apps on other nodes keep serving; apps scheduled on that node reschedule normally | API/panel down until the pod reschedules onto a healthy node — single SQLite writer means one server replica, by design | Automatic (Kubernetes), assuming multi-node cluster |
| Data PVC lost | **Unaffected** — cluster state lives in K3s (etcd), not the SQLite DB | Full loss: users, deploy history, settings, sealer key | Restore from backup (below). RPO = backup interval |
| Whole cluster lost | Down | Down | Reinstall K3s, `luncur up`, `luncur restore`, redeploy apps from git |

The data PVC losing its content is the only scenario that requires a backup
restore. Everything else is ordinary Kubernetes self-healing.

## RTO / RPO

- **RPO** (data loss window): the `backup_schedule` interval — `daily` by
  default, so up to 24h of control-plane metadata (users, deploy history,
  settings) can be lost. Running apps are not affected by RPO at all, since
  their state is Kubernetes-native, not in the SQLite DB.
- **RTO** (time to recover): **~15 minutes (unmeasured estimate — update
  after first drill)** for the single-PVC-loss restore path below, assuming
  the cluster itself is healthy and a backup archive is reachable (local PVC
  copy or S3).

Run the drill below at least once to replace the estimate with a measured
number.

## Restore drill

Steps for restoring after the data PVC is lost or corrupted. Run against a
test cluster first if you haven't done this before.

```sh
# 1. Fetch the latest archive (from the S3 bucket, if configured)
aws s3 --endpoint-url https://<s3-endpoint> cp s3://my-backups/luncur-YYYYMMDD-HHMMSS.tar.gz .
# or, without an external client, restore straight from the bucket (step 3 handles this)

# 2. Verify the archive restores cleanly BEFORE touching the live install
luncur backup verify luncur-YYYYMMDD-HHMMSS.tar.gz
# ok: N files, N tables, integrity=ok, sealer key=true

# 3. Scale the server down (SQLite must be quiescent during restore)
kubectl -n luncur-system scale deploy/luncur --replicas=0

# 4. Restore into the data PVC's path (local archive, or straight from S3)
luncur restore luncur-YYYYMMDD-HHMMSS.tar.gz --data-dir <data-dir> [--force]
# or: luncur restore <prefix>/luncur-....tar.gz --s3-endpoint https://<s3> \
#       --s3-bucket my-backups --s3-access-key ... --s3-secret-key ... --data-dir <data-dir>

# 5. Bring the server back up
kubectl -n luncur-system scale deploy/luncur --replicas=1

# 6. Confirm the restore is healthy
luncur login
luncur doctor
```

`luncur backup verify` restores the archive into a scratch directory, runs
`PRAGMA integrity_check`, and confirms the sealer key is present — it never
touches the live data dir, so it's safe to run against production archives
at any time, not just during an incident. `luncur doctor` after the restart
confirms database, Kubernetes, and cert checks are all `ok`.

Addon data (Postgres/Redis dumps bundled in the same archive) is restored
separately per the guided steps `luncur restore` prints — see
[Backups & restore](../guides/backups.md#restoring) for the full addon
procedure.

## Quarterly drill checklist

Run this every quarter, independent of any incident, so the RTO estimate
above stays honest and the backup pipeline is proven to actually work:

- [ ] Fetch the latest archive from the S3 bucket (or local PVC, if S3 isn't configured).
- [ ] Run `luncur backup verify <archive>` against it.
- [ ] Confirm output: `integrity=ok`, `sealer key=true`, non-zero table count.
- [ ] Time the verify step; if it's meaningfully different from prior runs, investigate (archive size growth, slow storage, etc.).
- [ ] Record the date and duration below (or in your own runbook/ticket).
- [ ] Once a year (or after any real incident), run the *full* restore drill above end-to-end against a scratch cluster, not just `backup verify`, and update the RTO estimate with the measured time.

| Date | Archive | `backup verify` duration | Result | Notes |
|---|---|---|---|---|
| _(fill in)_ | | | | |

## Scaling honesty

luncur's control plane is a single SQLite writer by design — one server
replica, one data PVC. That means the control plane itself (API, panel,
deploy orchestration) is **not highly available**: a node failure causes
seconds-to-minutes of control-plane downtime while Kubernetes reschedules
the pod, and a PVC loss requires a restore. Deployed apps are unaffected by
either, since their state lives in Kubernetes, not luncur's DB.

This is a deliberate trade-off for a self-hosted, single-binary PaaS —
running an HA control plane would mean an external database (e.g. replicated
Postgres) instead of embedded SQLite, which cuts against the zero-dependency
design this project is built on. If a real deployment outgrows this
trade-off, that's the point at which to revisit it; it is out of scope for
now.
