# Node maintenance

What this documents: taking a node out of service (kernel upgrades, disk
swaps, decommissioning) without dropping traffic.

```sh
luncur node cordon worker-2     # no new pods land here; running pods stay
luncur node drain worker-2      # cordon, then evict every pod (follows progress)
luncur node uncordon worker-2   # back in service
luncur node ls                  # STATUS shows Ready,SchedulingDisabled while cordoned
```

`drain` evicts pods through the Kubernetes **Eviction API**, which honors
PodDisruptionBudgets. luncur creates one for every app with a minimum of 2+
replicas, so a multi-replica app never loses all its pods at once:

- An eviction a budget refuses is retried every 5 seconds until `--timeout`
  (default 5m). The pods still blocked are then listed.
- DaemonSet pods, static (mirror) pods and finished pods are skipped.
- luncur's own pod is evicted **last**.

Draining the **only** schedulable node is refused: it would evict every app
and luncur itself. Pass `--force` if that's really what you want.

The Nodes page has cordon, uncordon and drain buttons on every row, and shows
drain progress (`evicting 3/7`, `blocked: shop/web-…`).

## Priority under pressure

luncur installs three PriorityClasses. When a node runs short of memory, the
kubelet evicts pods from the lowest class first:

| Class | Value | Used by |
|---|---|---|
| `luncur-system` | 1000000 | luncur, addon databases |
| `luncur-app` | 10000 | web, worker and model apps |
| `luncur-batch` | 100 (never preempts) | builds, job runs, sweep trials, pipeline steps, cron |

**Related:** [Doctor](doctor.md) · [GPU cloud](../ml/gpu-cloud.md)
