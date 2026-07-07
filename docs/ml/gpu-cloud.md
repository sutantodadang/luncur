# GPU cloud

## GPU cloud rental

```sh
# vast.ai — marketplace: browse offers, rent by offer id
luncur gpu key <vastai-api-key>
luncur gpu offers --gpu "RTX 4090" --count 1 --limit 10       # OFFER, GPU, COUNT, $/HR, DISK, WHERE
luncur gpu rent 123456 --disk 40                                # rent offer 123456

# Nebius — managed: rent by platform + preset, prices checked in the console
luncur gpu key --provider nebius --sa-id $SA_ID --pubkey-id $PUBKEY_ID \
  --private-key-file sa-key.pem --parent-id $PROJECT_ID --subnet-id $SUBNET_ID
luncur gpu rent --provider nebius --platform gpu-h100-sxm --preset 1gpu-16vcpu-200gb --disk 100

luncur gpu ls                                                   # both providers, one table
luncur gpu stop <id>                                            # destroy; billing stops, data gone
```

Both providers rent a VM that boots a K3s agent and auto-joins the cluster —
`luncur node ls` shows it once it's up. vast.ai is a marketplace: search
offers (GPU model, count, price/hr) and rent by offer id. Nebius is a managed
cloud: pick a platform (hardware generation) and preset (GPU/vCPU/RAM
bundle) directly — there's no in-luncur offer catalog, so check current
pricing in the Nebius console before renting.

Idle scale-to-zero is **per-instance**: each rented GPU node is destroyed
independently after `gpu_idle_minutes` (a setting; `0`/unset disables it) of
no GPU pod scheduled on it, so an always-on inference node survives while a
burst training node on the same account gets reaped on its own schedule.

Nebius support is docs-derived (API shapes read from docs.nebius.com, not yet
confirmed against a live account) — see
[`nebius-smoke-test.md`](../nebius-smoke-test.md) for the pending
verification checklist.

## Per-project GPU quota

```sh
luncur project gpu-quota myproj 4     # cap the project's apps at 4 GPUs total
luncur project gpu-quota myproj 0     # clear the cap (unlimited)
```

Caps the total `nvidia.com/gpu` devices a project's apps may request.
Enforcement is a Kubernetes `ResourceQuota` in the project namespace, so it
is exact at scheduling time; luncur additionally rejects obvious over-budget
creates and scale-ups with a friendly error before they reach the cluster.
The budget counts `gpu × replicas` for `web`/`worker` apps, `gpu × 1` for
`cron`, and `gpu × nodes` for `job` (a job app's planned footprint is its
multi-node run shape, not its largely-unused replicas column — see
[Multi-node training runs](training.md#multi-node-training-runs)). `0` means unlimited
(the default). Lowering the quota below current usage does not evict running
pods — new pods are rejected until usage drops, which is standard Kubernetes
`ResourceQuota` behavior. The project page in the web UI has a matching GPU
quota control (admin only).
