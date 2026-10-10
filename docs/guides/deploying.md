# Deploying

Get your code running as a live app — from source, from a pre-built image,
or via `git push` — then scale it, watch its logs, and roll back when a
deploy goes wrong.

## Deploy from source (most common path)

```sh
# Initialize an app config in the current directory
luncur init

# Deploy from local source (tars cwd, uploads, builds in cluster, streams to completion)
luncur deploy myapp --project myproj

# Deploy from git repository (registering a git-source app)
luncur app create myapp --project myproj --port 8080 \
  --git-url https://github.com/user/repo.git --branch main
luncur deploy myapp --project myproj

# Follow build logs for a specific deploy
luncur logs myapp --project myproj --deploy 1 -f

# Stream runtime (pod) logs — omit --deploy
luncur logs myapp --project myproj -f

# Bound runtime logs to the last 200 lines, or the last 15 minutes
luncur logs myapp --project myproj -f --tail 200
luncur logs myapp --project myproj -f --since 15m
```

## Deploy a pre-built image

Already build and push images elsewhere? Point `deploy` straight at the
image instead of building in-cluster:

```sh
luncur deploy myapp --project myproj --image my.registry/my/image:tag
```

## Deploy with `git push`

```sh
# Once per machine: register your SSH public key
luncur ssh-key add            # picks up ~/.ssh/id_*.pub; or pass a path
luncur ssh-key list
luncur ssh-key remove <id>

# Once per repo: add the luncur remote
git remote add luncur ssh://git@<ip>:30022/myproj/myapp.git

# Deploy
git push luncur main
```

The push streams the whole build into your `git push` output (as
`remote:` lines) and prints the app URL when it goes live. Only a push to
the app's configured branch (default `main`) triggers a deploy; the
receiver is push-only (`git pull`/`clone` from luncur is rejected) and no
repository is stored server-side — each push is archived straight into the
same build pipeline `luncur deploy` uses.

## Auto-deploy from a webhook

For git-source apps hosted elsewhere (GitHub/GitLab/Gitea), skip pushing to
luncur directly and let a webhook trigger the build instead:

```sh
luncur webhook enable myapp --project myproj
# paste the printed URL + secret into GitHub/GitLab/Gitea's webhook settings (push events)
```

A push to the app's configured branch (default `main`) triggers the same
build pipeline as `luncur deploy`; pushes to other refs are ignored. GitHub's
"ping" event (sent when a webhook is first saved) is answered without
deploying, so saving the hook doesn't kick off a spurious build.
`luncur webhook enable` re-run rotates the secret — update it at the
provider or deploys will stop authenticating. `luncur webhook show` and
`luncur webhook disable` round out the command.

## Monorepo builds (`--path`)

One git repo can back several apps by pointing each at a different
subdirectory as its build context/detection dir — e.g. a `dashboard/` React
app, a `backend/` FastAPI service, and an `ai/` FastAPI service all living
in the same repo. `dashboard` and `backend` are public; `ai` is only ever
called by `backend`, so it's created `--internal` — no public URL,
cluster-only:

```sh
luncur app create dashboard --project myproj --port 3000 \
  --git-url https://github.com/user/monorepo.git --path dashboard
luncur app create backend --project myproj --port 8000 \
  --git-url https://github.com/user/monorepo.git --path backend
luncur app create ai --project myproj --port 8001 --internal \
  --git-url https://github.com/user/monorepo.git --path ai
```

`backend` reaches `ai` at `http://ai.luncur-myproj:80` (in-cluster DNS —
see [Apps & projects](apps-and-projects.md) for internal web apps). A single
webhook push (or `luncur deploy`/`git push luncur`) to the shared repo
redeploys all three — each app's build only looks inside its own `--path`: a
`Dockerfile` there is used as-is, and nixpacks detection/output also run
inside that subdirectory instead of the repo root. `--path` is validated
(relative, no `..`, no leading `/`, `[a-zA-Z0-9._/-]` only) and is
**immutable after creation** — recreate the app to change it. Omitting
`--path` keeps the previous behavior: the whole repo root is the build
context.

## Scale and set resource limits

```sh
luncur scale myapp --project myproj --replicas 3
luncur scale myapp --project myproj --cpu 250m --memory 256Mi   # requests==limits
luncur scale myapp --project myproj --cpu "" --memory ""        # clear
```

Apps that set no CPU/memory still get small **requests** (default `50m` /
`64Mi`, no limits), so they're never the first pods evicted under node
pressure. Change or disable the defaults with the `default_cpu_request` /
`default_memory_request` settings (`0` = off). See [Insights](insights.md) for
recommended values based on real usage.

Multi-replica apps are spread across nodes (and zones) when the cluster has
more than one. On K8s 1.30+ (K3s's default), pods also pause 5 seconds before
shutting down, so the ingress stops routing to them first: no 502s during
rollouts.

## Add health checks

```sh
luncur health myapp --project myproj --path /healthz   # readiness+liveness probes
luncur health myapp --project myproj --off
```

Readiness gates rollouts and Service endpoints (zero-downtime deploys), while
liveness restarts a wedged container.

Without a health path, web apps still get a **TCP readiness probe** on their
port, so "Ready" means "accepting connections" rather than "process started".
Turn it off per app with `luncur app set myapp --project myproj --probe off`.

## Rollouts: when a deploy is "live"

A deploy is marked `live` only once its **new pods are serving**. Applying
the manifests is not enough: with zero-downtime rolling updates the old pods
keep serving while new ones start, so a broken image used to look live.
Now the deploy stays `deploying` while luncur's rollout gate watches the new
ReplicaSet:

- **Live:** every wanted replica is updated and available and no old pod is
  left.
- **Failed fast**, before the timeout:

  | What happened to a new pod | `fail_reason` |
  |---|---|
  | `CrashLoopBackOff`, or 3+ restarts | `crash-looping: <exit reason> (N restarts)` |
  | OOM-killed | `OOM-killed: <limit> limit hit` |
  | Missing Secret/ConfigMap key, invalid image name | `bad container config: …` |
  | Image pull failing for over 60s | `image pull failed: …` |
  | Not ready within the rollout timeout (default 5m) | `rollout timed out …` or `unschedulable: …` |

When a rollout fails, luncur **rolls back automatically** to the previous live
deploy. The new deploy row shows `(auto-rollback of #N)`, and a
`deploy_rolled_back` notification is sent. A rollback is never itself
auto-rolled back. The CLI waits for the verdict and prints the 3-line error:

```text
$ luncur deploy web --project shop --image shop/web:2.0
deployment #14 rolling out…
  ready 0/2
✗ deployment #14 failed: crash-looping: Error (4 restarts)
  why:  The new container exits shortly after starting — usually a missing env var, a bad command, or a failing migration.
  next: luncur logs web --project shop
```

`--no-wait` returns as soon as the manifests are applied. A pipeline `deploy:`
step finishes only when its deploy is live or failed.

```sh
luncur app set myapp --project myproj                       # show the policy
luncur app set myapp --project myproj --auto-rollback off   # keep a failed rollout in place
luncur app set myapp --project myproj --rollout-timeout 10m # slow-starting apps
```

The same settings are on the app's **Wire → Rollout** card.

## Canary and blue-green deploys

Web apps can roll out new images gradually:

```sh
luncur app set web --project shop --strategy canary --canary-steps 10,50 --canary-interval 2m
luncur app set web --project shop --strategy bluegreen --bluegreen-keep 10m
luncur app set web --project shop --strategy rolling       # back to the default
```

With `canary`, a deploy starts a second Deployment, `web-canary`, running the
new image while the current one keeps serving. Traffic then shifts through the
steps (10% → 50% → promote). Each step holds for the canary interval, and luncur
probes the canary every 10 seconds on the health path (or `/`). The canary is
**aborted** if:

- the probe success rate drops below `--canary-min-success` (default 99%); or
- a canary pod restarts.

Your live version never changes during a canary, so an abort needs no
rollback.

After the last step, the stable Deployment is rolled to the new image (gated
like any rollout), and the canary is removed.

**Blue-green** is the same machine with a full-size canary ("green") and a
single 100% switch, held for `--bluegreen-keep`. Aborting within that window
flips traffic straight back.

How traffic is split:

- **Weighted** (default on K3s): when Traefik's CRDs are installed, a
  `TraefikService` splits traffic by weight. It sits behind `IngressRoute`s that
  outrank the app's Ingress for the same hosts. Removing them hands routing
  back to the untouched Ingress.
- **Replica ratio** (fallback): without those CRDs, canary pods join the app's
  Service and are scaled to approximate each weight.

```sh
luncur rollout status web --project shop    # phase, weight, probe success
luncur rollout promote web --project shop   # skip the remaining steps
luncur rollout abort web --project shop     # traffic back to the current version
```

The app's **Ship** tab shows the rollout live with promote and abort buttons.
Canary and blue-green can't be used with:

- apps with volumes (ReadWriteOnce storage can't run two copies);
- GPU apps;
- internal apps.

A first deploy and every rollback always roll normally.

## Roll back a bad deploy

```sh
luncur rollback myapp --project myproj               # back to the previous live deploy
luncur rollback myapp --project myproj --deploy 12   # back to deploy #12 (as shown by `luncur status`/the web UI)
```

Rolling back redeploys an earlier deployment's image directly — no rebuild —
and records the new deployment row's lineage, shown in the web UI's Deploys
table as "(rollback of #N)" (N is the source deploy's seq, not its internal
id). The app page also has a `rollback` button on
every history row except the newest and any row with no image. Only images
hosted in luncur's embedded registry are HEAD-checked before rolling back
(a 409 naming the image if it's gone); externally-hosted image refs (e.g.
`docker.io/...`) are assumed present since luncur has no credentials to
verify them.

## Destroy an app

```sh
luncur destroy myapp --project myproj
```

## Set and edit environment variables

```sh
luncur env set myapp KEY=value --project myproj
luncur env unset myapp KEY --project myproj
luncur env list myapp --project myproj
luncur edit myapp Deployment --project myproj
```

## Bake env vars into the build

The same values set with `luncur env set` also reach the build itself, not
just the running container: each var is passed to the builder as a Docker
build-arg (Dockerfile path) or a nixpacks `--env` (nixpacks path). This
matters for frontend frameworks that bake env into the bundle at build time
instead of reading it at runtime — e.g. Vite:

```dockerfile
ARG VITE_API_URL=http://localhost:8000
ENV VITE_API_URL=$VITE_API_URL
RUN npm run build
```

`luncur env set myapp VITE_API_URL=https://api.myapp.example --project myproj`
then makes the next build inline the real URL instead of the Dockerfile's
localhost default. The Dockerfile must declare `ARG <KEY>` for a var to be
picked up; unreferenced vars are simply ignored by `docker build`.

!!! warning
    Build-args are not a safe place for high-grade secrets — they can leak
    into build cache metadata and image history. Keep secrets runtime-only
    (don't reference them from any `ARG`); use build-time env only for
    values that are fine to be visible in a built image.

## Check status

```sh
# List apps in a project (name + URL)
luncur status --project myproj

# Show one app's status (replicas, image, URL, live cpu/memory, deploy count)
luncur status myapp --project myproj
```

Live cpu/memory come from the cluster's `metrics-server` (K3s bundles it by
default); if it's not installed or unreachable, `status` prints `metrics:
unavailable` instead — the deploy count is always shown regardless.

**Related:** [Apps & projects](apps-and-projects.md) ·
[Domains & TLS](domains-and-tls.md) · [Volumes](volumes.md) ·
[Backups & restore](backups.md)
