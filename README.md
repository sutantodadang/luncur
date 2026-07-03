# luncur

Tiny self-hosted PaaS on K3s. One Go binary, SQLite, deploys as simple as
Heroku — with an escape hatch to the real Kubernetes objects.

Status: Phase 1 complete (Plans A-D); Phase 2 in progress — git push deploys
shipped (Plan E). Working today:

## Install

On a fresh Linux VPS:

```sh
curl -sfLo luncur https://github.com/sutantodadang/luncur/releases/latest/download/luncur-linux-amd64
chmod +x luncur
sudo ./luncur up
```

`luncur up` is a one-command installer: it installs K3s (pinned version),
writes the containerd `registries.yaml` mirror for the in-cluster registry,
applies luncur's own system infrastructure (namespace, registry, PVCs) and
its own Deployment/Service/Ingress, waits for the rollout, and logs the CLI
in. On first run it prints the generated admin email + password — **this is
shown once, store it now**. Re-running `luncur up` is safe: every step is
skip-or-repair (server-side apply), so it's also how you upgrade or repair
an install.

Flags:
- `--ip` — public IP to advertise (default: detected from the node's
  `ExternalIP`, falling back to `InternalIP`)
- `--image` — luncur server image (default: `ghcr.io/sutantodadang/luncur:<version>`)
- `--kubeconfig` — point at an existing cluster instead of installing K3s
  (skips the K3s/registries host steps)

**Server (manual, e.g. for local dev):**
```sh
luncur serve --db luncur.db \
  --listen :8080 \
  --bootstrap-admin admin@example.com:password \
  --kubeconfig /path/to/kubeconfig \
  --external-ip 10.0.0.1 \
  --secret-key-file luncur.key \
  --data-dir /var/lib/luncur \
  --builder-image luncur/builder:latest \
  --registry-host registry.luncur-system:5000
```

**Auth:**
```sh
luncur login http://localhost:8080
luncur whoami
luncur user add teammate@example.com --password ... [--role admin|member]
```

**Projects & apps:**
```sh
luncur project create myproj
luncur project list
luncur project add-member myproj member@example.com

luncur app create myapp --project myproj --port 8080
luncur app list --project myproj
luncur app info myapp --project myproj
luncur app raw myapp --project myproj
```

**Deployment & scaling:**
```sh
luncur deploy myapp --project myproj --image my.registry/my/image:tag
luncur scale myapp --project myproj --replicas 3
luncur destroy myapp --project myproj
```

**Source build & deployment:**
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
```

**Deploy with git push:**
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

**Environment & editing:**
```sh
luncur env set myapp KEY=value --project myproj
luncur env unset myapp KEY --project myproj
luncur env list myapp --project myproj
luncur edit myapp Deployment --project myproj
```

**Status:**
```sh
# List apps in a project (name + URL)
luncur status --project myproj

# Show one app's status (replicas, image, URL)
luncur status myapp --project myproj
```

## Web UI

After `luncur up`, the panel is served at `http://panel.<ip>.sslip.io/ui/`
(login with the admin credentials printed by `luncur up`, or any user added
via `luncur user add`). From there you can browse projects and apps, scale
and edit env vars, trigger a deploy, and watch build/runtime logs stream
live via Server-Sent Events — no CLI required.

## Build Pipeline

When you run `luncur deploy` on a local source or git repository, the following happens: source is uploaded (tarball from local cwd or cloned from git URL) → a BuildKit Job runs in the `luncur-system` namespace, applying Nixpacks if no Dockerfile exists or using the Dockerfile if present → the resulting image is pushed to the in-cluster registry (default `registry.luncur-system:5000`) → app manifests are rendered and applied to Kubernetes → the app becomes live at `http://<app>.<ip>.sslip.io`. Build logs are streamed on demand via `luncur logs`.

**Serve flags for build infrastructure:**
- `--data-dir` — path where build sources and logs are persisted; becomes a Kubernetes PVC in production (default `./data`)
- `--builder-image` — OCI image for the build environment (default `luncur/builder:latest`); built by the release pipeline
- `--registry-host` — in-cluster registry address (default `registry.luncur-system:5000`); K3s requires an insecure-registry entry for this host, written to `/etc/rancher/k3s/registries.yaml` by `luncur up`

## Approved deviations from the design spec

- Web UI uses stdlib `html/template` + one vanilla-JS `EventSource` block instead of templ + HTMX. Zero codegen, zero vendored JS; same server-rendered pages + SSE behavior.
- In-cluster registry is reachable from containerd via a NodePort (30500) + `registries.yaml` mirror to `http://127.0.0.1:30500`, since containerd on the node cannot resolve cluster-DNS names like `registry.luncur-system`.
- Public-IP detection: node `ExternalIP` → node `InternalIP` → `--ip` flag. No outbound HTTP probe.
- API tokens expire after 90 days (enforcement only; `luncur token list/revoke` is a future addition).
- The git-push SSH host key persists as a file on the data PVC (beside the DB), not a K8s Secret — same durability, no kube dependency at SSH boot.
- Push progress streams via a `post-receive` hook, so the `git push` exit code cannot reflect a build failure (refs land before the hook runs). The client still sees the full build log and a final `BUILD FAILED`/`app live` line.

Design docs: `docs/superpowers/specs/`. Plans: `docs/superpowers/plans/`.
