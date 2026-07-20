# Ejecting an app

Ejecting hands an app off to plain Kubernetes: luncur stops managing it but
leaves the running objects untouched, so you can take over with raw
`kubectl` without downtime. It's the escape hatch for when an app outgrows
what luncur's UI/CLI can express.

```sh
luncur app eject myapp --project myproj [--yes]
luncur app adopt myapp --project myproj    # reverse it later
```

## What eject does

luncur renders the app's final manifest (current overrides plus its latest
image), prints the YAML to stdout, and archives a copy under
`data/ejected/<project>-<app>.yaml` on the server. The Kubernetes objects
luncur rendered keep running untouched — ejecting doesn't delete or modify
anything in the cluster, it only stops luncur from touching it further.

From then on every mutation — deploy, scale, env, domains, overrides,
rollback, addon attach/detach, and further `git push` — is refused with
`409 app_ejected`; reads (status, logs, metrics, raw YAML, the app page)
keep working exactly as before. `luncur destroy` on an ejected app removes
luncur's own records (DB rows) only, leaving the running objects in place.

Ejected apps are marked `(ejected)` in `luncur app list` and with an
"ejected" badge in the web UI, which also hides the now-inert
scale/deploy/env/domains/rollback/edit forms in favor of a one-line note.

!!! warning "Confirmation required"
    Without `--yes`, `app eject` asks you to type the app's name back to confirm before doing anything irreversible.

## Re-adopting

`luncur app adopt` reverses eject: it clears the ejected flag and re-applies
luncur's rendered state onto the running objects (reclaiming
`fieldManager=luncur`). Any manual drift made to those objects while ejected
is overwritten. The web UI's ejected note carries an adopt button that does
the same; after adopting, the normal management UI returns.

**Related:** [Build pipeline](../reference/build-pipeline.md) · [Uninstall](uninstall.md)
