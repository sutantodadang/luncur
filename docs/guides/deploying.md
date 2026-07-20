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

## Add health checks

```sh
luncur health myapp --project myproj --path /healthz   # readiness+liveness probes
luncur health myapp --project myproj --off
```

Readiness gates rollouts and Service endpoints (zero-downtime deploys), while
liveness restarts a wedged container.

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
