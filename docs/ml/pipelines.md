# Pipelines

A pipeline is a DAG of steps — job-app runs, inline container images, and
deploy/scale/notify actions — driven by luncur's own orchestrator (or,
opt-in, by Argo Workflows). No DAG graphic here; the operator UI shows the
same information as a topo-ordered table instead.

## pipeline.yaml

```yaml
steps:
  train:
    app: trainer
    outputs: [model]
  evaluate:
    app: evaluator
    needs: [train]
    inputs: [train/model]
    retries: 2
  notify-done:
    needs: [evaluate]
    notify: "training pipeline finished"
```

```sh
luncur pipeline create nightly --project ml --file pipeline.yaml
```

## Step kinds

| Kind | Fields | Runs as |
|---|---|---|
| `app` | `app`, `env`, `retries`, `inputs`, `outputs` | a run of an existing `kind=job` app (B1's `luncur run`) |
| `image` | `image`, `command`, `env`, `gpu`, `retries`, `inputs`, `outputs` | an inline, ad hoc Kubernetes Job — no app needed |
| `deploy` | `deploy` (target app name) | redeploys the target app's current live image |
| `scale` | `scale: {app, replicas}` | sets an app's replica count |
| `notify` | `notify` (message, ≤500 chars) | fires a webhook notification (`notify_url` setting) |

Every step also takes `needs: [other-step, ...]` to declare its upstream
dependencies — the DAG edges.

!!! warning "Quote yes/no-ish choice values"
    Bare YAML 1.1 treats `yes`/`no`/`y`/`n` (and `on`/`off`) as booleans, not
    strings — the same gotcha as `params.yaml` in [Sweeps](sweeps.md). Quote
    any string value that looks like one of these.

## Artifacts

`app`/`image` steps declare `outputs: [name, ...]` and `inputs:
["step/name", ...]` (the source step must be a transitive upstream
dependency). Each step's env is seeded with:

- `LUNCUR_PIPELINE_ID`, `LUNCUR_PIPELINE_RUN_ID`, `LUNCUR_ARTIFACT_PREFIX`
  (`pipelines/<pipeline>/<run-id>/`) — always injected.
- `LUNCUR_OUTPUT_<NAME>` — the S3 key a step should write its named output
  to.
- `LUNCUR_INPUT_<NAME>` — the S3 key an input references (its producing
  step's output).

luncur doesn't move the bytes — steps read/write these keys themselves
against the project's own `LUNCUR_S3_*` env. A step's own `env:` overrides
these convention values if it sets the same key.

!!! warning "Step env is plaintext"
    `env` on an `app`/`image` step is stored (and shown in the UI's yaml
    editor) in plaintext — put secrets in the target app's env instead, not
    in pipeline.yaml.

## Triggers

- **Manual** — `luncur pipeline run <name> --project <p>`, or the detail
  page's "run now" button.
- **Cron** — `--cron "0 3 * * *"` at create/update time (minute-granularity,
  5-field). A cron-triggered pipeline is `Forbid`-concurrency: a still-running
  previous run skips that minute's fire rather than stacking up.
- **Webhook** — `luncur pipeline webhook <name> --project <p>` prints the
  trigger URL and a secret **once** — store it now; re-running rotates it and
  invalidates the old one. The external system signs its POST body with
  either GitHub/Gitea's `X-Hub-Signature-256: sha256=<hmac-sha256-hex>` or
  GitLab's plain `X-Gitlab-Token: <secret>` header. Unlike cron, a webhook
  fire is never concurrency-gated — every valid signed request starts a
  fresh run.

## Engines

- **native** (default) — luncur's own orchestrator loop, zero extra install,
  works air-gapped.
- **argo** — opt-in, backed by Argo Workflows:

  ```sh
  luncur argo install
  luncur config set pipeline_engine argo   # or --engine argo per pipeline/run
  ```

  Under the argo engine, **actions must be terminal**: no `app`/`image`
  (compute) step may depend on a `deploy`/`scale`/`notify` (action) step —
  Argo owns the compute DAG's lifecycle end to end, so an action can only sit
  downstream of everything it might need to react to, never upstream of more
  compute. A run keeps the engine it launched with even if the pipeline's own
  `engine`/`pipeline_engine` setting changes later.

## Failure handling

Fail-fast: a failed step (past its `retries` budget) skips every step
downstream of it; siblings on other branches of the DAG still run to
completion. The run itself finishes `done` only if every step finished
`done` — any `failed` step finishes the run `failed`.

## CLI reference

```sh
luncur pipeline create <name> --project <p> --file pipeline.yaml [--cron "0 3 * * *"] [--engine native|argo]
luncur pipeline update <name> --project <p> [--file pipeline.yaml] [--cron ""] [--engine argo]
luncur pipeline ls --project <p>
luncur pipeline run <name> --project <p>
luncur pipeline status <run-id> --pipeline <name> --project <p>
luncur pipeline stop <run-id> --pipeline <name> --project <p>
luncur pipeline webhook <name> --project <p> [--disable]
luncur pipeline rm <name> --project <p>

luncur argo install                        # opt-in engine
```

The project page's Pipelines card mirrors this: a create form, a table of
pipelines (engine, cron, last-run status) with a per-row run button, and a
detail page per pipeline — the yaml editor, run history, and the current
run's live step table (state chips, attempt, detail, duration), polling
every 15s while a run is in progress.
