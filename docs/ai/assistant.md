# AI assistant

luncur has a built-in AI assistant that does four things:

1. **Explains failures.** It reads a failed deploy's build log, pod status and recent logs, and answers in three lines: what broke, the most likely cause, and the next command to run.
2. **Runs ops in plain language.** For example, "add a postgres to api and scale web to 3".
3. **Generates config.** It writes a `pipeline.yaml`, sweep `params.yaml`, YAML override or Dockerfile, and luncur's own compiler checks the file before you see it.
4. **Lets external agents operate luncur.** `luncur mcp` exposes luncur to Claude Code, Claude Desktop or any MCP client.

## How it acts: as you

Every assistant action is a call to luncur's own API, made in-process with **your** identity:

- What your role can't do, the assistant can't do either: a project viewer gets read-only tools, and a forbidden call fails with the same 403 the API gives.
- Every change is in the [audit log](../operations/audit.md) under your name, marked `(via ai)`.
- Each action is shown as the equivalent luncur command (CLI-echo), so a session doubles as a runbook.
- Destructive tools (delete an app, addon, volume or environment; eject) are available within your role. They're marked as destructive, and the assistant uses one only when you asked for exactly that.

## Turn it on

=== "Claude (recommended)"

    ```sh
    luncur config set ai_provider claude
    luncur config set ai_api_key sk-ant-…        # sealed at rest, write-only
    # optional: luncur config set ai_model claude-opus-5-5   (the default)
    ```

=== "Self-hosted model (nothing leaves the cluster)"

    Serve a model with a [model app](../ml/model-serving.md), then point the assistant at it:

    ```sh
    luncur app create chat --project ml --kind model --source hf:unsloth/gemma-3n-E4B-it-GGUF/gemma-3n-E4B-it-Q4_K_M.gguf
    luncur config set ai_provider openai
    luncur config set ai_base_url app:ml/chat     # resolves to the app's in-cluster URL
    luncur config set ai_model gemma
    ```

    Any OpenAI-compatible endpoint works (`ai_base_url https://…/v1`, plus `ai_api_key` if it needs one). Tool use depends on the model: small local models handle diagnosis well but may be unreliable at multi-step chat.

`luncur ai status` shows whether it's on. The Settings page has the same knobs under **AI assistant**.

## Explain a failure

```sh
luncur ai explain web --project shop                 # latest failed deploy
luncur ai explain web --project shop --deploy <id>
luncur ai explain trainer --project ml --run 42      # a job run
```

```
what broke   The build failed at npm install
likely why   package-lock.json pins a package that no longer exists ("npm ERR! 404 left-pad@0.0.1")
next         $ luncur redeploy web --project shop
confidence   high
```

In the web UI, failed deploys (Ship tab) and failed runs (Jobs tab) have an **explain** button.

Diagnosis is a single call with no tools, so text inside your logs can't make it change anything. App env values and attached addon credentials are scrubbed from everything sent to the provider.

With `luncur config set ai_notify on`, `deploy_failed` and `app_unhealthy` [notifications](../guides/backups.md) get one extra line with the likely cause and the next command.

## Ask it to do things

```sh
luncur ai ask "why is web restarting?" --project shop
luncur ai ask "add a postgres called db, attach it to api, and scale web to 3" --project shop --env staging
luncur ai ask --conversation <id> "now roll web back to the previous deploy"
```

```
[ok ] $ luncur addon create postgres --name db --app api --project shop --env staging
[ok ] $ luncur scale web --replicas 3 --project shop --env staging

Created postgres "db", attached it to api (DATABASE_URL is injected), and scaled web to 3 replicas.
```

The **Assistant** page (sidebar → You → Assistant) is the same conversation in the browser.

## Generate config

```sh
luncur ai gen pipeline "train with trainer, evaluate, notify on success" --project ml -o pipeline.yaml
luncur ai gen params "lr 1e-5..1e-2 log scale, batch size 16/32/64" -o params.yaml
luncur ai gen override "add a sidecar that ships logs to vector" --project shop --app web --kind Deployment
luncur ai gen dockerfile "production Node 22 app, pnpm, port 3000"
luncur ai gen pipeline "add a retry to evaluate" --project ml --from pipeline.yaml
```

The assistant loops until luncur's validator accepts the file: `pipeline.Compile` plus app references for pipelines, the sweep parser for params, and the override safety checks for patches. In the UI, the pipeline editor and the sweep form have a **generate with ai** disclosure that fills the editor. Nothing is saved until you press save.

## External agents (MCP)

```sh
claude mcp add luncur -- luncur mcp --project shop      # Claude Code
```

Claude Desktop (`claude_desktop_config.json`):

```json
{"mcpServers": {"luncur": {"command": "luncur", "args": ["mcp"]}}}
```

`luncur mcp` runs on your machine and calls the server with your login (`luncur login`), so the agent has exactly your permissions. Tools carry MCP hints (`readOnlyHint`, `destructiveHint`) so the client can ask before risky calls. No provider settings are needed on the server: the agent brings its own model.

## Cost and limits

| Setting | Default | |
|---|---|---|
| `ai_daily_token_budget` | `2000000` | Install-wide tokens per UTC day; `0` = unlimited. Requests stop once it's spent |
| `ai_max_steps` | `20` | Model calls per request (tool loop cap) |
| `ai_effort` | `medium` | Reasoning effort for chat and generation (diagnosis always uses `low`) |

`luncur ai usage` shows tokens per day, user and workflow: admins see everyone's, others see their own.

## What leaves your server

With `ai_provider claude`, the prompt sent to Anthropic contains your request, luncur state the assistant reads (app/deploy/pod listings, build and runtime log excerpts) and, for generation, the file being edited. Env var **values** are never sent (tools list names only), and known secret values are scrubbed from logs. To keep everything in the cluster, use `ai_provider openai` with a luncur model app.

Conversations are kept in memory for two hours and are lost on restart.
