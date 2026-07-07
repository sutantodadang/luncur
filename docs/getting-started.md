# Getting started

## Install

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
  --builder-image ghcr.io/sutantodadang/luncur-builder:latest \
  --registry-host registry.luncur-system:5000
```

## ⚡ Quickstart

On a fresh Linux VPS:

```sh
curl -sfLo luncur https://github.com/sutantodadang/luncur/releases/latest/download/luncur-linux-amd64
chmod +x luncur
sudo ./luncur up        # installs K3s + luncur, prints admin credentials (once — save them!)
```

Deploy your first app from your laptop:

```sh
luncur login http://panel.<ip>.sslip.io
luncur project create demo
luncur app create web --project demo --port 8080

luncur ssh-key add                                        # once per machine
git remote add luncur ssh://git@<ip>:30022/demo/web.git   # once per repo
git push luncur main                                      # 🚀 build streams into your push output
```

Your app is live at `http://web.<ip>.sslip.io` — no DNS setup needed
(sslip.io resolves automatically). Add a real domain and HTTPS:

```sh
luncur domain add web www.example.com --project demo   # cert issues automatically
```

Re-running `sudo ./luncur up` is always safe: every step is
skip-or-repair, so it doubles as upgrade and self-heal.

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

Note: apps with volumes use K3s local-path storage, which pins a pod to the
node where its volume was first created.
