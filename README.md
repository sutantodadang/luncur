# luncur

Tiny self-hosted PaaS on K3s. One Go binary, SQLite, deploys as simple as
Heroku — with an escape hatch to the real Kubernetes objects.

Status: Phase 1 done (Plan A); Phase 2 in progress (Plan B2). Working today:

**Server:**
```sh
luncur serve --db luncur.db \
  --listen :8080 \
  --bootstrap-admin admin@example.com:password \
  --kubeconfig /path/to/kubeconfig \
  --external-ip 10.0.0.1 \
  --secret-key-file luncur.key
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

**Environment & editing:**
```sh
luncur env set myapp KEY=value --project myproj
luncur env unset myapp KEY --project myproj
luncur env list myapp --project myproj
luncur edit myapp Deployment --project myproj
```

Design docs: `docs/superpowers/specs/`. Plans: `docs/superpowers/plans/`.
