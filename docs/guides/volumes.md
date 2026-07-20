# Volumes

Attach persistent disk storage to an app so data survives redeploys and pod
restarts — for things like uploaded files, SQLite databases, or a local
cache directory that shouldn't reset every deploy.

## Add a volume

```sh
luncur volume add myapp /data --project myproj --size 5              # 5GB PVC mounted at /data
luncur volume add myapp /var/cache --project myproj --size 1 --name cache
```

Volumes attach a `ReadWriteOnce` PersistentVolumeClaim (K3s local-path by
default) to a `web` or `worker` app — `cron` apps don't take volumes. The
name defaults to the last path segment; size is 1–1000 GB.

## List and remove volumes

```sh
luncur volume list myapp --project myproj                            # NAME, PATH, SIZE
luncur volume remove myapp cache --project myproj                    # detach; PVC + data kept
luncur volume remove myapp cache --project myproj --purge            # detach AND delete the PVC
```

The app page in the web UI has a matching Volumes section (add form,
per-row remove with a "purge data" checkbox), hidden for cron apps.

## Constraints to know before you add one

!!! warning
    - **Max 1 replica.** RWO local-path storage binds to one node/pod. Adding a
      volume to an app running more than one replica is refused (409), as is
      scaling a volumed app above 1.
    - **Brief downtime per deploy.** A volumed app's Deployment uses the
      `Recreate` strategy (the old pod must release the volume before the new
      one can mount it), so every deploy stops the app for a few seconds.
    - **Not in backups.** `luncur backup` snapshots luncur's own state and addon
      dumps — app volume data is *not* included. Back up volume contents
      yourself if they matter.
    - **Never deleted implicitly.** `volume remove` only detaches; the PVC and
      its data survive until you pass `--purge`. Destroying an app also leaves
      its PVCs behind — run `volume remove --purge` first, or clean up later
      with `kubectl delete pvc -n <namespace> <app>-<volume>`.

**Related:** [Deploying](deploying.md) · [Addons](addons.md) ·
[Backups & restore](backups.md)
