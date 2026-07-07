# Registry GC

```sh
luncur registry gc   # admin only
```

luncur's embedded registry accumulates one image per deploy; `registry gc`
reclaims that storage with a retention sweep. For each app, the newest
`registry_keep` images (default 10) are kept, plus whichever image is
currently live and whichever is newest, regardless of position; everything
else — including whole repositories with no matching app left in the DB —
is deleted from the registry's manifest catalog. `registry
garbage-collect` then runs inside the registry pod to reclaim the
underlying blob storage. Change the retention count with:

```sh
luncur config set registry_keep 20
```

A sweep also runs automatically once a week. The registry container's
`REGISTRY_STORAGE_DELETE_ENABLED` env var is set to `true` by luncur's own
system manifests, since the registry's HTTP API rejects manifest DELETEs
outright without it.
