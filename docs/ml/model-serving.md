# Model serving

Serve an LLM behind an OpenAI-compatible HTTP API. Point the `model` app kind
at a set of weights, luncur picks (or you pick) a serving runtime, downloads
the model into the pod, and wires up the Service/Ingress like any web app —
`POST /v1/chat/completions` works out of the box.

## Deploy a model

```sh
# CPU inference with llama.cpp (GGUF file from Hugging Face)
luncur app create chat --project ml --kind model \
  --source hf:unsloth/gemma-3n-E4B-it-GGUF/gemma-3n-E4B-it-Q4_K_M.gguf \
  --cpu 4 --memory 8Gi

# GPU inference with vLLM (full-weights repo, needs a GPU node)
luncur app create chat --project ml --kind model \
  --source hf:meta-llama/Llama-3.1-8B-Instruct --runtime vllm --gpu 1
```

Model apps deploy at create for the built-in runtimes — no `git push` or
image needed. The endpoint URL is printed on create and shown on the app
page.

## Model sources

| Source | Meaning |
|---|---|
| `hf:<org>/<name>` | Hugging Face repo (vLLM serves it directly) |
| `hf:<org>/<name>/<file>` | one file from a HF repo — e.g. a `.gguf` for llama.cpp |
| `s3:<key>` | object in the project's configured S3 bucket (see [Addons](../guides/addons.md) for project S3) |

Downloaded files land in `/models` inside the pod via an init container.
Source components are validated against a strict character allowlist —
they flow into container commands and URLs.

## Runtimes

| `--runtime` | Image | When |
|---|---|---|
| `auto` (default) | — | `.gguf` source → `llamacpp` (CPU or GPU); anything else with `--gpu` → `vllm`; non-GGUF without GPU → friendly error |
| `llamacpp` | `ghcr.io/ggml-org/llama.cpp:server` | GGUF files, CPU or GPU |
| `vllm` | `vllm/vllm-openai:v0.8.5` | full-weights repos, GPU |
| `custom` | your deployed image | luncur still downloads the model to `/models` and wires the Service; your server binds the app port |

Built-in runtime images are pinned in luncur and bumped deliberately.

## Notes

- A model app is a `web`-shaped deployment: it gets a Service + Ingress
  (sslip.io URL, custom [domains & TLS](../guides/domains-and-tls.md) work),
  scaling and CPU/memory limits via `luncur scale`.
- `--gpu N` schedules onto GPU nodes and adds `nvidia.com/gpu` requests —
  rent capacity with the [GPU cloud](gpu-cloud.md) commands. GPU model apps
  use the `Recreate` strategy (the replacement pod can't schedule while the
  old one holds the node's GPU).
- Track experiments and versions with the MLflow addon
  (see [Addons](../guides/addons.md)).

**Related:** [GPU cloud](gpu-cloud.md) · [Training](training.md) · [Pipelines](pipelines.md)
