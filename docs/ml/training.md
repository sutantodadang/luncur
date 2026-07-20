# Training

Run a training job across multiple GPU nodes as one Kubernetes job — luncur
wires up rendezvous env vars so your script (or `torchrun`) finds the other
nodes without you hand-rolling the networking. Reach for this once a single
GPU isn't enough, or you're bursting a run across rented capacity.

## Multi-node training runs

A `job` app's run can span N GPU nodes as one Kubernetes Indexed Job, with a
framework-agnostic env rendezvous contract always present and optional
framework presets layered on top.

### Start a run

```sh
luncur app training train --project ml --nodes 4 --framework torchrun  # set defaults
luncur run train --project ml            # 4-node torchrun DDP run (uses the app defaults)
luncur run train --project ml --nodes 8  # burst: this run only, needs quota + schedulable nodes
```

`--nodes`/`--framework` on `luncur run` (and the app page's run-now form)
override the app's stored defaults for that one run; omitted, they fall back
to `luncur app training`'s values. The app page's Runs card has matching
controls for both — the training-defaults form and the run-now form's
nodes/framework fields.

### Rendezvous env vars

Every multi-node run gets the `LUNCUR_*` contract regardless of framework —
write your training script against these and it works with any launcher:

| Env var             | Value                                                          |
|----------------------|-----------------------------------------------------------------|
| `LUNCUR_NODE_RANK`   | this pod's index (`0`..`N-1`, via the Indexed Job completion index) |
| `LUNCUR_NUM_NODES`   | `N`, the run's node count                                       |
| `LUNCUR_MASTER_ADDR` | rank-0's stable pod DNS name (`<run>-0.<run>.<namespace>`)       |
| `LUNCUR_MASTER_PORT` | `29500`                                                          |

### Framework presets

`--framework` additionally layers that launcher's native env vars on top —
optional, and only meaningful with `--nodes` > 1:

| Framework  | Vars set                                                                 |
|------------|---------------------------------------------------------------------------|
| (none)     | `LUNCUR_*` contract only — bring your own rendezvous (MPI, Ray, raw sockets, etc). |
| `torchrun` | `PET_NNODES`, `PET_NODE_RANK`, `PET_RDZV_BACKEND=c10d`, `PET_RDZV_ENDPOINT` — run your entrypoint as a plain `torchrun train.py`, no flags needed. |
| `torch`    | `MASTER_ADDR`, `MASTER_PORT`, `RANK`, `NODE_RANK`, `WORLD_SIZE`, `NNODES` — the `torch.distributed` env:// contract; also what `deepspeed`/`accelerate` read. |

!!! note "Node-level, not process-level"
    These are **node-level** values — `RANK`/`WORLD_SIZE` count nodes, not
    per-GPU processes. A multi-GPU node still needs its own in-container
    launcher (`torchrun --nproc_per_node`, `accelerate launch`) to fan out
    across that node's GPUs.

### Quota and scheduling

A run above the app's stored `nodes` needs GPU headroom *now* (its own
`gpu × nodes` is already counted against the [project GPU
quota](../ml/gpu-cloud.md#per-project-gpu-quota)) — `luncur run --nodes 8` on
a 1-GPU app asks for 7 more GPUs than the app's own footprint, and is
rejected with a budget error if the project doesn't have them free.

A run's pods aren't guaranteed to schedule together — if the cluster can't
fit all N before `train_gang_timeout_minutes` (a setting, default `10`; `0`
disables the guard) elapses, luncur tears the Job down and marks the run
failed rather than leaving it half-scheduled indefinitely, squatting GPUs no
training is using. Set it from the Settings page (`/ui/settings`, admin).

**Related:** [GPU cloud](gpu-cloud.md) · [Sweeps](sweeps.md) · [Pipelines](pipelines.md)
