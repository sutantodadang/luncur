# Uninstall

`luncur down` reverses `luncur up`. Two tiers:

- **default** — removes luncur itself: the `luncur`/`luncur-ssh`
  Deployment/Services/Ingress/ServiceAccount and RBAC, every luncur-managed
  namespace (`luncur-system` plus each project's `luncur-<name>`, found via
  the `app.kubernetes.io/managed-by=luncur` label), the `luncur-data` PVC,
  and the `registries.yaml` `luncur up` wrote. **K3s itself is left running**
  so another `luncur up` can redeploy onto it.
- **`--all`** — also runs K3s's own `k3s-uninstall.sh`, removing K3s and all
  its cluster data.

```sh
luncur down --dry-run       # print the plan, change nothing
luncur down                 # tear down luncur, keep K3s
luncur down --all           # tear down luncur AND K3s
luncur down --all --yes     # skip the typed confirmation (e.g. scripted)
luncur down --no-backup     # skip the final DB backup
```

Before touching anything (unless `--yes`), it prints what will be destroyed
and requires typing the literal word `luncur` to proceed — anything else
aborts. Unless `--no-backup`, the live `luncur.db` is `cat`'d out of the
running pod first, to `~/luncur-final-backup-<unix-ts>.db`. Each step is
best-effort: a failure is reported (`step failed: ...`) and the remaining
steps still run, with a non-zero exit if anything failed.

`--dry-run` output looks like:

```
dry run — no changes will be made:
1. backup SQLite DB to /home/you/luncur-final-backup-1735689600.db
2. stop luncur (delete Deployment/Services/Ingress/ServiceAccount and RBAC in luncur-system)
3. delete luncur-managed namespaces (luncur-system + every project namespace)
4. remove luncur data volume (PersistentVolumeClaim luncur-data in luncur-system)
5. remove registries config written by `luncur up` (/etc/rancher/k3s/registries.yaml)
```
