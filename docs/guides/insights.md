# Cost and right-sizing insights

What this documents: how luncur compares what your apps reserve with what they
actually use, and estimates what they cost.

```sh
luncur insights
luncur insights --project shop
```

The web panel has an **Insights** page (sidebar) and a **Right-size** card on
each app's Observe tab.

## How it works

luncur's metrics monitor already samples every app's CPU and memory from
metrics-server (bundled with K3s). It also folds those samples into hourly
**per-pod** aggregates, kept for 30 days. Insights compare each app's requests
with the last 7 days of usage. Apps with no explicit requests are compared
using the workload defaults.

- Recommended CPU = highest hourly p95 × 1.2, rounded up to 5m.
- Recommended memory = peak × 1.3, rounded up to 16Mi.
- Each app is flagged:
  - **over-provisioned** when a request is more than twice the
    recommendation;
  - **at risk** when the memory peak exceeds 90% of the app's memory limit;
  - **right-sized** otherwise.
- Every flagged app comes with the exact command to apply, e.g.
  `luncur scale web --cpu 120m --memory 272Mi --project shop`.

Recommendations appear after 6 hours of data. Without metrics-server, the page
says so.

## Costs

Costs are estimates from unit prices you enter:

```sh
luncur config set cost_currency '$'
luncur config set cost_cpu_core_month 20     # per vCPU-month
luncur config set cost_mem_gb_month 5        # per GiB-month
luncur config set cost_gpu_month 400         # per GPU-month
```

The monthly cost of an app is its requests (CPU, memory, GPU) × prices ×
replicas. Run-to-completion kinds (cron, job) have no standing cost.
**Potential savings** sum the difference for over-provisioned apps. Members see
their own projects; admins see everything.

With the AI assistant configured, ask it "where am I wasting money?". The
`usage_insights` tool gives it the same report.

**Related:** [Deploying](deploying.md) · [AI assistant](../ai/assistant.md)
