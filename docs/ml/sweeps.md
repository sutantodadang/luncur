# Hyperparameter sweeps

## params.yaml

A sweep's parameter space is a YAML mapping, one axis per key. Two forms:

```yaml
lr: {min: 1e-5, max: 1e-2, log: true}   # continuous — random samples in log10 space
batch_size: [16, 32]                     # discrete — rendered verbatim
```

Any axis with a continuous `{min, max[, log]}` range makes the whole sweep
random-sampled (`max_trials` draws); when every axis is a discrete list, the
sweep instead expands the full grid (all combinations, oldest-key-first),
truncated to `max_trials` if the grid is larger.

!!! warning "Quote yes/no-ish choice values"
    Bare YAML 1.1 treats `yes`/`no`/`y`/`n` (and `on`/`off`) as booleans, not
    strings — `["yes", "no"]` parses as `[true, false]`, not the choice
    strings you meant. Quote them: `["'yes'", "'no'"]`.

Each trial's params reach its run as `LUNCUR_PARAM_<KEY>` env vars (key
upper-cased), alongside `LUNCUR_SWEEP_ID` and `LUNCUR_TRIAL_ID`.

## Metric contract

A trial reports its metric one of two ways:

- **MLflow** — when the app has an `mlflow` addon attached, luncur sets
  `MLFLOW_TRACKING_URI`/`MLFLOW_RUN_NAME` and polls the tracking server for
  the named metric's latest value.
- **Log lines** — otherwise (or if MLflow becomes unreachable mid-sweep,
  which degrades the whole sweep to this source and sets a warning), luncur
  scans the trial's pod logs for the last line matching:

  ```
  luncur-metric: val_loss=0.42 step=100
  ```

  `step` is optional; malformed lines are skipped, never fatal.

## Early stopping

With `--early-stop`, once at least 3 trials have finished, any running trial
whose latest metric is worse than the median of finished trials' values (at a
comparably-advanced step) is pruned — its job is killed and it's marked
`pruned`, not `failed`.

## Quota pacing

A sweep launches up to `--parallel` trials at a time; if the project doesn't
have GPU budget free for the next trial this tick, it's left `pending` and
retried on a later tick as other trials finish and free up quota — the sweep
never hard-fails on a transient budget squeeze.

## Example

```sh
cat > params.yaml <<'EOF'
lr: {min: 1e-5, max: 1e-2, log: true}
batch_size: [16, 32]
EOF
luncur sweep start train --project ml --params params.yaml \
  --metric val_loss --max-trials 12 --parallel 3 --early-stop
luncur sweep status <id> --app train --project ml
```

The app page's Sweeps card mirrors this: a start form, the sweep's trial
table (state, params, metric, best-trial highlight), and a stop button.
