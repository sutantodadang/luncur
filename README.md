# luncur

Tiny self-hosted PaaS on K3s. One Go binary, SQLite, deploys as simple as
Heroku — with an escape hatch to the real Kubernetes objects.

Status: Phase 1 done (Plan A); Phase 2 done (Plan B2); Phase 3 in progress (Plan C). Working today:

**Server:**
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

# View build logs for a specific deploy
luncur logs myapp --project myproj --deploy 1
```

**Environment & editing:**
```sh
luncur env set myapp KEY=value --project myproj
luncur env unset myapp KEY --project myproj
luncur env list myapp --project myproj
luncur edit myapp Deployment --project myproj
```

## Build Pipeline

When you run `luncur deploy` on a local source or git repository, the following happens: source is uploaded (tarball from local cwd or cloned from git URL) → a BuildKit Job runs in the `luncur-system` namespace, applying Nixpacks if no Dockerfile exists or using the Dockerfile if present → the resulting image is pushed to the in-cluster registry (default `registry.luncur-system:5000`) → app manifests are rendered and applied to Kubernetes → the app becomes live at `http://<app>.<ip>.sslip.io`. Build logs are streamed on demand via `luncur logs`.

**Serve flags for build infrastructure:**
- `--data-dir` — path where build sources and logs are persisted; becomes a Kubernetes PVC in production (default `./luncur-data`)
- `--builder-image` — OCI image for the build environment (default `luncur/builder:latest`); built by the release pipeline
- `--registry-host` — in-cluster registry address (default `registry.luncur-system:5000`); K3s requires an insecure-registry entry for this host (configured by `luncur up` in Plan D)

Design docs: `docs/superpowers/specs/`. Plans: `docs/superpowers/plans/`.
