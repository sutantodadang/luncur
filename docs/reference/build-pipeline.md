# Build pipeline

What this documents: what happens between `luncur deploy` and a running app, and the flags/settings that control each stage.

## Pipeline stages

When you run `luncur deploy` on a local source or git repository: source is
uploaded (tarball from local cwd or cloned from git URL) → a BuildKit Job
runs in the `luncur-system` namespace, applying Nixpacks if no Dockerfile
exists or using the Dockerfile if present → the resulting image is pushed to
the in-cluster registry (default `registry.luncur-system:5000`) → app
manifests are rendered and applied to Kubernetes → the app becomes live at
`http://<app>.<ip>.sslip.io`. Build logs are streamed on demand via `luncur
logs`.

## Deploy numbering

Deploys are numbered per app, Heroku-style (`#1`, `#2`, ...) — that's the
number shown in `luncur status`, the web UI's Deploy history table, and
`luncur rollback --deploy N`. It's also the image tag luncur pushes
(`<registry>/<project>-<app>:<N>`). Every deployment also has an opaque
internal id (a random 12-character id, not a counter) — it backs Build Job
names, log/tarball filenames, and the rollback API's request body, but the
per-app `#N` above is the only deploy number you should ever need to type or
read.

!!! note "Beta note"
    An upgrade from a pre-nanoid luncur regenerates every deployment's
    internal id in place — history and `#N` numbering are preserved, but any
    build log/tarball already on disk under the old id orphans.

## Build cache

Builds reuse a per-app BuildKit cache stored as an image manifest in the
embedded registry (`luncur-cache/<project>-<app>:buildcache`), so repeat
deploys skip unchanged layers instead of rebuilding from scratch every time.
Disable it with `luncur config set build_cache off`. Cache manifests are
kept by registry GC for as long as the app exists and swept once it's
destroyed, same as any other repo (see [Registry GC](../operations/registry-gc.md#registry-gc) above).
Since the cache is exported with `mode=max` (intermediate layers, not just
the final image), factor that into disk sizing for the registry's PVC
alongside app images.

## Build timeout

A build is allotted a fixed time budget before it's given up on and marked
failed — 15 minutes by default. Override it with
`luncur config set build_timeout_minutes 30` (1-720); the new budget applies
to builds started afterward, including ones re-attached by restart
reconciliation (see below).

## Serve flags for build infrastructure

- `--data-dir` — path where build sources and logs are persisted; becomes a Kubernetes PVC in production (default `./data`)
- `--builder-image` — OCI image for the build environment (default `ghcr.io/sutantodadang/luncur-builder:latest`); built and published to ghcr.io by the release pipeline
- The system namespace (`luncur-system`) that build Jobs run in is provisioned at PodSecurity level `privileged`, not `restricted`: rootless BuildKit needs setuid `newuidmap` to remap uids (forbidden by `restricted` as privilege escalation) and unconfined seccomp/AppArmor profiles for its mount-namespace setup (forbidden by `baseline`). The build pod itself still runs as the unprivileged uid 1000 — the namespace label only stops Kubernetes from rejecting those profile settings. Project/app namespaces are unaffected and stay `restricted`.
- `--registry-host` — in-cluster registry address (default `registry.luncur-system:5000`); K3s requires an insecure-registry entry for this host, written to `/etc/rancher/k3s/registries.yaml` by `luncur up`

## Troubleshooting: build stuck or no logs

`luncur logs <app> --project <project> --deploy <id> -f` streams the deployment's build log, which
now carries two kinds of lines: server-written milestones prefixed
`[luncur]` (e.g. `[luncur] 2026-01-01T00:00:00Z rendering build job`,
`...applying build job to cluster`, `...waiting for builder pod`, and
periodic `...builder pod: Pending (ImagePullBackOff)` updates), and — once
the builder pod starts — its own entrypoint output appended after. If the
log only ever shows `[luncur]` lines and never progresses past "waiting for
builder pod", the builder pod itself isn't starting.

- **`builder pod: Pending (ImagePullBackOff)`** — the cluster can't pull the
  builder image (`--builder-image`, default
  `ghcr.io/sutantodadang/luncur-builder:latest`, published by the release
  pipeline). If you're pinning a custom image, publish it to a registry the
  cluster can reach, or rebuild/retag it locally and make sure the node has it
  (`docker save`/`k3s ctr images import` for single-node setups).
- **`no builder pod created yet — job events: ...`** — the Build Job never
  managed to create a pod at all (PodSecurity admission rejecting the pod
  spec, a ResourceQuota with no room left, a validating admission webhook,
  etc.), so `JobPodStatus` never sees a pod to report on. After ~30s of
  seeing no pod, the log now prints the Job's own recent Kubernetes events —
  a `Warning FailedCreate: pods "build-<id>-" is forbidden: violates
  PodSecurity "restricted:latest"` line is the usual PodSecurity case — so
  the reason for the stall shows up in the LOGS pane instead of nothing.
  The manual equivalent, if you're at a shell:
  `kubectl -n luncur-system describe job build-<id>` (its Events section is
  exactly what gets mirrored into the log).
- **`pod watcher error: ...`** — the watcher itself couldn't list pods for
  the Build Job (e.g. missing RBAC on `pods` in `luncur-system`). Logged
  once, not spammed every poll; fix the permission and the next build's
  watcher will succeed.
- **Build never finishes** — it's cut off after `build_timeout_minutes`
  (default 15) and marked failed; raise it with
  `luncur config set build_timeout_minutes <n>` if your builds are simply
  slow (large images, cold BuildKit cache).
- **`luncur doctor`'s `builds` check** reports `fail` when a deployment has
  sat in `building` for over 30 minutes — the same signal, surfaced without
  needing to know which app it is.
- **Server restarted mid-build** — on startup, luncur reconciles every
  deployment left in `building`/`deploying` by the previous process: if the
  Build Job is still running it re-attaches and resumes normally (a
  `[luncur] ... server restarted — re-attached to running build job` line
  appears in the log); if the Job is gone too, or the resumed build/deploy
  fails, the deployment is marked `failed` rather than left stuck forever.

**Related:** [Registry GC](../operations/registry-gc.md) · [Doctor / diagnostics](../operations/doctor.md) · [Settings](settings.md)
