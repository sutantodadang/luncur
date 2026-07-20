# Addons (Postgres/Redis)

Addons are project-level Postgres/Redis instances (a StatefulSet + PVC +
headless Service + credentials Secret, rendered the same way apps are) that
one or more apps in the same project can attach to. Reach for this instead
of hand-rolling a database Deployment whenever an app needs a datastore.

## Create and attach an addon

```sh
luncur addon create postgres --project myproj                     # provision, unattached
luncur addon create redis --project myproj --name cache --version 7 --size 5

luncur addon add postgres --app myapp --project myproj             # create + attach in one step
luncur addon attach db1 myapp --project myproj                     # attach an existing addon
luncur addon detach db1 myapp --project myproj
```

`addon create` provisions without attaching; `addon add` is sugar for
create-then-attach. Names default to `<type><n>` (e.g. `postgres1`,
`postgres2`) and versions default to `postgres:16` / `redis:7` when not
given.

Attaching an addon injects a connection URL into the app's env at render
time — `DATABASE_URL` for postgres, `REDIS_URL` for redis — with no extra
cluster read needed. If the app already has that env var set (`luncur env
set`), the user's value always wins; the addon's value is silently skipped
and `attach` prints a warning naming the collision instead of erroring.
Attaching a second addon of the same type suffixes the key with the addon's
name (e.g. `DATABASE_URL_POSTGRES2`), so multiple databases can coexist on
one app.

## List addons

```sh
luncur addon list --project myproj                                 # NAME, TYPE, VERSION, READY, ATTACHED
```

## Upgrade an addon

```sh
luncur addon upgrade db1 --project myproj --version 17              # in-place, rolling restart; PVC untouched
```

`addon upgrade` swaps the StatefulSet's image tag and SSA-applies it — a
rolling restart on the same PVC.

!!! warning
    Every upgrade response carries the warning *"major version DB upgrades
    may require manual migration — take a backup first."*: luncur does not
    run `pg_upgrade` or any engine-level data migration.

## Remove an addon

```sh
luncur addon remove db1 --project myproj                            # 409 if still attached
luncur addon remove db1 --project myproj --force                    # remove despite attachments
luncur addon remove db1 --project myproj --force --keep-data        # remove but keep the PVC
```

Addons are never deleted implicitly — destroying an app just detaches its
addons (the underlying instance and its data survive). `addon remove`
refuses to delete an addon that's still attached to any app unless `--force`
is passed, and deletes the PVC along with the StatefulSet/Service/Secret
unless `--keep-data` is also passed.

## Web UI

The web UI mirrors all of this: each project page has an Addons table
(create form + per-row remove-with-force) and each app page has an attached-
addons list (detach buttons) plus an attach form listing the project's
addons.

**Related:** [Deploying](deploying.md) · [Volumes](volumes.md) ·
[Backups & restore](backups.md)
