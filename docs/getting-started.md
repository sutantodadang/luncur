# Getting started

This walks you from a bare Linux VPS to a live app on your own domain. You'll
install luncur, log in, create a project and app, and deploy it with
`git push`. Budget about five minutes.

**You'll need:** a fresh 64-bit Linux VPS with root (or sudo) and a public IP,
and a machine with `git` and an SSH key. No Kubernetes knowledge required —
luncur installs and drives it for you.

## Step 1: Install on the VPS

Download the binary and run `luncur up`:

```sh
curl -sfLo luncur https://github.com/sutantodadang/luncur/releases/latest/download/luncur-linux-amd64
chmod +x luncur
sudo ./luncur up        # installs K3s + luncur, prints admin credentials (once — save them!)
```

`luncur up` installs K3s (a pinned version), writes the containerd
`registries.yaml` mirror for the in-cluster registry, applies luncur's own
system infrastructure (namespace, registry, PVCs) and its Deployment /
Service / Ingress, waits for the rollout, and logs the CLI in. On first run it
prints a generated admin email + password.

!!! warning "The admin password is shown once"
    Store the printed credentials immediately — they are not shown again.

On a **Raspberry Pi** or other 64-bit ARM board, swap the download for
`luncur-linux-arm64`; the container images are multi-arch, so everything else
is identical. See [Raspberry Pi & ARM](operations/raspberry-pi.md).

!!! tip "Re-running is safe"
    `sudo ./luncur up` is skip-or-repair at every step (server-side apply), so
    it doubles as your upgrade and self-heal command.

### Install flags

- `--ip` — public IP to advertise (default: detected from the node's
  `ExternalIP`, falling back to `InternalIP`)
- `--image` — luncur server image (default:
  `ghcr.io/sutantodadang/luncur:<version>`)
- `--kubeconfig` — point at an existing cluster instead of installing K3s
  (skips the K3s/registries host steps)

## Step 2: Log in from your laptop

Point the CLI at your panel. The URL uses your VPS IP; sslip.io resolves it
automatically, so there's no DNS to set up yet:

```sh
luncur login http://panel.<ip>.sslip.io
```

## Step 3: Create a project and an app

```sh
luncur project create demo
luncur app create web --project demo --port 8080
```

A **project** is a namespace with members; an **app** is a workload inside it.
`--port` is the port your web app listens on.

## Step 4: Deploy with `git push`

Register your SSH key once per machine, add the luncur remote once per repo,
then push:

```sh
luncur ssh-key add                                        # once per machine
git remote add luncur ssh://git@<ip>:30022/demo/web.git   # once per repo
git push luncur main                                      # 🚀 build streams into your push output
```

The build streams into your `git push` output as `remote:` lines and prints the
app URL when it goes live.

## Step 5: See it live

Your app is now at `http://web.<ip>.sslip.io` — open it in a browser. No DNS
setup needed (sslip.io resolves automatically).

## Step 6: Add a real domain and HTTPS

Point your domain's DNS at the VPS IP, then:

```sh
luncur domain add web www.example.com --project demo   # cert issues automatically
```

luncur requests a Let's Encrypt certificate automatically. For wildcard certs
and DNS-01 providers, see [Domains & TLS](guides/domains-and-tls.md).

## What you built

A self-hosted PaaS on one VPS: a project, a `web` app that redeploys on every
`git push`, and automatic HTTPS on your own domain — all backed by one Go
binary and a SQLite file. Everything luncur created is a normal Kubernetes
object; `kubectl` sees exactly what you see.

**Next steps:**

- [Deploying](guides/deploying.md) — scaling, rollbacks, health checks, env vars
- [Addons](guides/addons.md) — managed Postgres and Redis
- [Backups & restore](guides/backups.md) — snapshot to any S3 bucket
- [Doctor](operations/doctor.md) — one-shot health check if something looks off

## Server (manual, e.g. for local dev)

To run the server by hand against an existing cluster:

```sh
luncur serve --db luncur.db \
  --listen :8080 \
  --bootstrap-admin admin@example.com:password \
  --kubeconfig /path/to/kubeconfig \
  --external-ip 10.0.0.1 \
  --secret-key-file luncur.key \
  --data-dir /var/lib/luncur \
  --builder-image ghcr.io/sutantodadang/luncur-builder:latest \
  --registry-host registry.luncur-system:5000
```

## Multi-node

Add another VPS as a worker node:

```sh
# on the existing server
luncur node join-command

# on the new VPS (as root) — paste the printed command
luncur join https://<server-ip>:6443 --token <node-token>

# verify
luncur node ls
```

!!! note "Volumes pin a pod to its node"
    Apps with volumes use K3s local-path storage, which pins a pod to the node
    where its volume was first created.

## Related

- [Deploying](guides/deploying.md) · [Apps & projects](guides/apps-and-projects.md)
- [Support luncur](support.md) — if this saved you a PaaS bill
