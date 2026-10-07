// Package aitools is the AI tool registry: each tool describes one existing
// luncur API endpoint (method, path, parameters, CLI equivalent). The same
// registry serves two executors — the server's built-in assistant, which
// dispatches tool calls in-process through the normal API stack as the
// calling user, and `luncur mcp`, which sends them over HTTP with the user's
// own token — so an AI can never do more than the user's role allows.
package aitools

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

// Param is one tool argument.
type Param struct {
	Name        string
	Type        string // "string", "integer", "boolean", "object"
	Description string
	Required    bool
	// In is where the argument goes: "path" ({name} in Tool.Path), "query",
	// "body" (a JSON body field), or "rawbody" (the argument itself is the
	// whole JSON body).
	In   string
	Enum []string
}

// Tool is one API endpoint exposed to an AI.
type Tool struct {
	Name        string
	Description string
	Method      string
	// Path is the project-default form, e.g.
	// "/v1/projects/{project}/apps/{app}/scale".
	Path string
	// EnvScoped tools take an optional "env" argument; when set, the
	// environment-qualified route (".../projects/{project}/envs/{env}/...")
	// is used instead.
	EnvScoped   bool
	Params      []Param
	Mutating    bool // changes state
	Destructive bool // deletes or detaches state (shown distinctly)
	Admin       bool // server admin role required
	// CLI is the equivalent luncur command, with {arg} placeholders; flags
	// whose argument is empty are dropped.
	CLI string
	// KeysOnly strips values from a {KEY: value} response (env vars): the
	// AI sees which variables exist, never their values.
	KeysOnly bool
	// SSE marks a server-sent-events response, flattened to plain lines.
	SSE bool
}

const projectPrefix = "/v1/projects/{project}/"

var (
	pProject = Param{Name: "project", Type: "string", Description: "project name", Required: true, In: "path"}
	pApp     = Param{Name: "app", Type: "string", Description: "app name", Required: true, In: "path"}
)

func pathP(name, desc string) Param {
	return Param{Name: name, Type: "string", Description: desc, Required: true, In: "path"}
}
func bodyS(name, desc string, req bool) Param {
	return Param{Name: name, Type: "string", Description: desc, Required: req, In: "body"}
}
func bodyI(name, desc string, req bool) Param {
	return Param{Name: name, Type: "integer", Description: desc, Required: req, In: "body"}
}
func bodyB(name, desc string) Param {
	return Param{Name: name, Type: "boolean", Description: desc, In: "body"}
}
func queryI(name, desc string) Param {
	return Param{Name: name, Type: "integer", Description: desc, In: "query"}
}
func queryB(name, desc string) Param {
	return Param{Name: name, Type: "boolean", Description: desc, In: "query"}
}

var registry = []Tool{
	// ---- read ----
	{Name: "list_projects", Description: "List the projects the user can see.", Method: "GET", Path: "/v1/projects",
		CLI: "luncur project list"},
	{Name: "list_envs", Description: "List a project's environments (production, develop, staging, previews).", Method: "GET",
		Path: projectPrefix + "envs", Params: []Param{pProject}, CLI: "luncur envs list --project {project}"},
	{Name: "list_apps", Description: "List the apps in a project environment with their kind and status.", Method: "GET",
		Path: projectPrefix + "apps", EnvScoped: true, Params: []Param{pProject}, CLI: "luncur app list --project {project} --env {env}"},
	{Name: "get_app", Description: "Show one app: kind, image, replicas, URL, latest deploy status.", Method: "GET",
		Path: projectPrefix + "apps/{app}", EnvScoped: true, Params: []Param{pProject, pApp}, CLI: "luncur app info {app} --project {project} --env {env}"},
	{Name: "list_deploys", Description: "List an app's deploys, newest first, with status (building, deploying, live, failed) and, for failed rollouts, fail_reason.", Method: "GET",
		Path: projectPrefix + "apps/{app}/deploys", EnvScoped: true, Params: []Param{pProject, pApp}, CLI: "luncur status {app} --project {project} --env {env}"},
	{Name: "deploy_logs", Description: "Read the build/deploy log of one deploy (by deploy id from list_deploys).", Method: "GET",
		Path: projectPrefix + "apps/{app}/deploys/{id}/logs", EnvScoped: true, Params: []Param{pProject, pApp, pathP("id", "deploy id")},
		CLI: "luncur logs {app} --deploy {id} --project {project} --env {env}"},
	{Name: "app_logs", Description: "Read an app's recent runtime (pod) logs.", Method: "GET",
		Path: projectPrefix + "apps/{app}/logs", EnvScoped: true, SSE: true,
		Params: []Param{pProject, pApp, queryI("tail", "only the last N lines (recommended: 200)")},
		CLI:    "luncur logs {app} --tail {tail} --project {project} --env {env}"},
	{Name: "app_pods", Description: "List an app's pods with phase, restarts, exit reasons and resource usage.", Method: "GET",
		Path: projectPrefix + "apps/{app}/pods", EnvScoped: true, Params: []Param{pProject, pApp}, CLI: "luncur ps {app} --project {project} --env {env}"},
	{Name: "app_metrics", Description: "Show an app's current CPU and memory usage.", Method: "GET",
		Path: projectPrefix + "apps/{app}/metrics", EnvScoped: true, Params: []Param{pProject, pApp}, CLI: "luncur metrics {app} --project {project} --env {env}"},
	{Name: "list_env_keys", Description: "List the names of an app's environment variables (values are never shown).", Method: "GET",
		Path: projectPrefix + "apps/{app}/env", EnvScoped: true, KeysOnly: true, Params: []Param{pProject, pApp},
		CLI: "luncur env list {app} --project {project} --env {env}"},
	{Name: "list_domains", Description: "List an app's custom domains and their certificate status.", Method: "GET",
		Path: projectPrefix + "apps/{app}/domains", EnvScoped: true, Params: []Param{pProject, pApp}, CLI: "luncur domain list {app} --project {project} --env {env}"},
	{Name: "list_volumes", Description: "List an app's persistent volumes.", Method: "GET",
		Path: projectPrefix + "apps/{app}/volumes", EnvScoped: true, Params: []Param{pProject, pApp}, CLI: "luncur volume list {app} --project {project}"},
	{Name: "list_addons", Description: "List a project environment's addons (postgres, redis, minio, mlflow) and which apps they're attached to.", Method: "GET",
		Path: projectPrefix + "addons", EnvScoped: true, Params: []Param{pProject}, CLI: "luncur addon list --project {project} --env {env}"},
	{Name: "raw_manifest", Description: "Show the Kubernetes YAML luncur renders for an app.", Method: "GET",
		Path: projectPrefix + "apps/{app}/raw", Params: []Param{pProject, pApp}, CLI: "luncur app raw {app} --project {project}"},
	{Name: "list_runs", Description: "List a job app's training/batch runs.", Method: "GET",
		Path: projectPrefix + "apps/{app}/runs", EnvScoped: true, Params: []Param{pProject, pApp}, CLI: "luncur run ls {app} --project {project}"},
	{Name: "run_logs", Description: "Read the logs of one job run.", Method: "GET",
		Path: projectPrefix + "apps/{app}/runs/{id}/logs", EnvScoped: true, SSE: true,
		Params: []Param{pProject, pApp, pathP("id", "run id"), queryI("tail", "only the last N lines")},
		CLI:    "luncur run logs {app} --id {id} --project {project}"},
	{Name: "list_sweeps", Description: "List a job app's hyperparameter sweeps.", Method: "GET",
		Path: projectPrefix + "apps/{app}/sweeps", EnvScoped: true, Params: []Param{pProject, pApp}, CLI: "luncur sweep ls --app {app} --project {project}"},
	{Name: "get_sweep", Description: "Show one sweep with every trial's params, state and metric.", Method: "GET",
		Path: projectPrefix + "apps/{app}/sweeps/{id}", EnvScoped: true, Params: []Param{pProject, pApp, pathP("id", "sweep id")},
		CLI: "luncur sweep status {id} --app {app} --project {project}"},
	{Name: "cron_runs", Description: "List a cron app's recent runs and their status.", Method: "GET",
		Path: projectPrefix + "apps/{app}/cron-runs", EnvScoped: true, Params: []Param{pProject, pApp}, CLI: "luncur cron runs {app} --project {project}"},
	{Name: "list_pipelines", Description: "List a project's pipelines.", Method: "GET",
		Path: projectPrefix + "pipelines", Params: []Param{pProject}, CLI: "luncur pipeline ls --project {project}"},
	{Name: "get_pipeline", Description: "Show a pipeline and its pipeline.yaml.", Method: "GET",
		Path: projectPrefix + "pipelines/{name}", Params: []Param{pProject, pathP("name", "pipeline name")}},
	{Name: "list_pipeline_runs", Description: "List a pipeline's runs.", Method: "GET",
		Path: projectPrefix + "pipelines/{name}/runs", Params: []Param{pProject, pathP("name", "pipeline name")}},
	{Name: "get_pipeline_run", Description: "Show one pipeline run with every step's state.", Method: "GET",
		Path: projectPrefix + "pipelines/{name}/runs/{id}", Params: []Param{pProject, pathP("name", "pipeline name"), pathP("id", "run id")},
		CLI: "luncur pipeline status {id} --pipeline {name} --project {project}"},
	{Name: "doctor", Description: "Run luncur's health checks (database, kubernetes, registry, builds, certificates, backups).", Method: "GET",
		Path: "/v1/doctor", Admin: true, CLI: "luncur doctor"},
	{Name: "list_nodes", Description: "List cluster nodes with readiness, resource usage, cordon state and drain progress.", Method: "GET",
		Path: "/v1/nodes", Admin: true, CLI: "luncur node ls"},

	// ---- write ----
	{Name: "create_app", Description: "Create an app. kind: web (default, needs port), worker, cron (needs schedule), job, or model (needs model_source).", Method: "POST",
		Path: projectPrefix + "apps", EnvScoped: true, Mutating: true,
		Params: []Param{pProject, bodyS("name", "app name (lowercase letters, digits, dashes)", true), bodyI("port", "container port for web apps", false),
			{Name: "kind", Type: "string", In: "body", Enum: []string{"web", "worker", "cron", "job", "model"}, Description: "app kind"},
			bodyS("schedule", "cron schedule for kind=cron", false), bodyS("git_url", "git repository to build from", false),
			bodyS("git_branch", "git branch", false), bodyS("build_path", "subdirectory to build", false),
			bodyB("internal", "web app without a public URL"), bodyI("gpu", "GPUs per pod", false),
			bodyS("model_source", "model source for kind=model, e.g. hf:org/name/file.gguf", false), bodyS("runtime", "model runtime", false)},
		CLI: "luncur app create {name} --kind {kind} --port {port} --project {project} --env {env}"},
	{Name: "deploy_image", Description: "Deploy a pre-built container image to an app.", Method: "POST",
		Path: projectPrefix + "apps/{app}/deploy", EnvScoped: true, Mutating: true,
		Params: []Param{pProject, pApp, bodyS("image", "image reference, e.g. nginx:1.27", true)},
		CLI:    "luncur deploy {app} --image {image} --project {project} --env {env}"},
	{Name: "redeploy", Description: "Re-roll an app's current release (rebuild git apps, re-apply the latest image otherwise).", Method: "POST",
		Path: projectPrefix + "apps/{app}/redeploy", EnvScoped: true, Mutating: true, Params: []Param{pProject, pApp},
		CLI: "luncur redeploy {app} --project {project} --env {env}"},
	{Name: "rollback", Description: "Roll an app back to an earlier deploy (deploy_id from list_deploys; default: previous live).", Method: "POST",
		Path: projectPrefix + "apps/{app}/rollback", EnvScoped: true, Mutating: true,
		Params: []Param{pProject, pApp, bodyS("deploy_id", "deploy id to roll back to", false)},
		CLI:    "luncur rollback {app} --project {project}"},
	{Name: "rollout_status", Description: "Show an app's canary / blue-green rollout: phase, current traffic weight, steps and probe success rate.", Method: "GET",
		Path: projectPrefix + "apps/{app}/rollout", EnvScoped: true, Params: []Param{pProject, pApp},
		CLI: "luncur rollout status {app} --project {project} --env {env}"},
	{Name: "rollout_promote", Description: "Skip an app's remaining canary steps and promote the new image now.", Method: "POST",
		Path: projectPrefix + "apps/{app}/rollout/promote", EnvScoped: true, Mutating: true, Params: []Param{pProject, pApp},
		CLI: "luncur rollout promote {app} --project {project} --env {env}"},
	{Name: "rollout_abort", Description: "Abort an app's canary / blue-green rollout; traffic returns to the current version.", Method: "POST",
		Path: projectPrefix + "apps/{app}/rollout/abort", EnvScoped: true, Mutating: true, Params: []Param{pProject, pApp},
		CLI: "luncur rollout abort {app} --project {project} --env {env}"},
	{Name: "uptime_status", Description: "Show an app's uptime check: state (up/down), 24h/30d/90d availability, p95 latency, last error.", Method: "GET",
		Path: projectPrefix + "apps/{app}/uptime", EnvScoped: true, Params: []Param{pProject, pApp},
		CLI: "luncur uptime status {app} --project {project} --env {env}"},
	{Name: "list_incidents", Description: "List a project's incidents (outages opened by uptime checks, maintenance notices).", Method: "GET",
		Path: projectPrefix + "incidents", Params: []Param{pProject}, CLI: "luncur incident list --project {project}"},
	{Name: "open_incident", Description: "Open an incident on the project's status page (e.g. planned maintenance).", Method: "POST",
		Path: projectPrefix + "incidents", Mutating: true,
		Params: []Param{pProject, bodyS("title", "incident title", true), bodyS("app", "affected app (optional)", false), bodyS("body", "first update (optional)", false)},
		CLI:    "luncur incident open {title} --project {project}"},
	{Name: "cordon_node", Description: "Mark a node unschedulable (running pods stay).", Method: "POST",
		Path: "/v1/nodes/{name}/cordon", Admin: true, Mutating: true, Params: []Param{pathP("name", "node name")}, CLI: "luncur node cordon {name}"},
	{Name: "uncordon_node", Description: "Mark a node schedulable again.", Method: "POST",
		Path: "/v1/nodes/{name}/uncordon", Admin: true, Mutating: true, Params: []Param{pathP("name", "node name")}, CLI: "luncur node uncordon {name}"},
	{Name: "drain_node", Description: "Cordon a node and evict its pods for maintenance (disruption budgets respected); refuses the only schedulable node unless force.", Method: "POST",
		Path: "/v1/nodes/{name}/drain", Admin: true, Mutating: true,
		Params: []Param{pathP("name", "node name"), bodyB("force", "drain even the only schedulable node")}, CLI: "luncur node drain {name}"},
	{Name: "get_policy", Description: "Show an app's rollout policy: auto-rollback, rollout timeout, default probe, pod security level, deploy strategy (rolling/canary/bluegreen) and canary settings.", Method: "GET",
		Path: projectPrefix + "apps/{app}/policy", EnvScoped: true, Params: []Param{pProject, pApp},
		CLI: "luncur app set {app} --project {project} --env {env}"},
	{Name: "set_policy", Description: "Change an app's rollout policy; only the given fields change.", Method: "PUT",
		Path: projectPrefix + "apps/{app}/policy", EnvScoped: true, Mutating: true,
		Params: []Param{pProject, pApp, bodyB("auto_rollback", "roll back automatically when a rollout fails"),
			bodyI("rollout_timeout", "seconds new pods may take to become ready (30-3600)", false),
			{Name: "probe", Type: "string", In: "body", Enum: []string{"auto", "off"}, Description: "default TCP readiness probe when no health path is set"},
			{Name: "security", Type: "string", In: "body", Enum: []string{"baseline", "restricted", "relaxed"}, Description: "pod security level"},
			{Name: "strategy", Type: "string", In: "body", Enum: []string{"rolling", "canary", "bluegreen"}, Description: "deploy strategy"},
			bodyS("canary_steps", "canary traffic weights in percent, e.g. 10,50", false),
			bodyI("canary_interval", "seconds each canary step holds", false)},
		CLI: "luncur app set {app} --project {project} --env {env}"},
	{Name: "scale_app", Description: "Set an app's replica count and/or per-pod CPU, memory and GPU limits.", Method: "POST",
		Path: projectPrefix + "apps/{app}/scale", EnvScoped: true, Mutating: true,
		Params: []Param{pProject, pApp, bodyI("replicas", "replica count", false), bodyS("cpu", "CPU limit, e.g. 500m", false),
			bodyS("memory", "memory limit, e.g. 512Mi", false), bodyI("gpu", "GPUs per pod", false)},
		CLI: "luncur scale {app} --replicas {replicas} --cpu {cpu} --memory {memory} --project {project} --env {env}"},
	{Name: "autoscale_app", Description: "Configure CPU-based autoscaling (min/max replicas, target CPU %); min 0 turns it off.", Method: "PUT",
		Path: projectPrefix + "apps/{app}/autoscale", EnvScoped: true, Mutating: true,
		Params: []Param{pProject, pApp, bodyI("min", "minimum replicas", true), bodyI("max", "maximum replicas", false), bodyI("cpu", "target CPU utilization percent", false)},
		CLI:    "luncur autoscale {app} --min {min} --max {max} --cpu {cpu} --project {project}"},
	{Name: "set_health", Description: "Set an app's HTTP health-check path (empty turns it off).", Method: "POST",
		Path: projectPrefix + "apps/{app}/health", EnvScoped: true, Mutating: true,
		Params: []Param{pProject, pApp, bodyS("path", "health check path, e.g. /healthz", false)},
		CLI:    "luncur health {app} --path {path} --project {project}"},
	{Name: "set_env", Description: "Set one environment variable on an app (triggers a rolling restart if live).", Method: "PUT",
		Path: projectPrefix + "apps/{app}/env", EnvScoped: true, Mutating: true,
		Params: []Param{pProject, pApp, bodyS("key", "variable name", true), bodyS("value", "variable value", true)},
		CLI:    "luncur env set {app} {key}=<value> --project {project} --env {env}"},
	{Name: "unset_env", Description: "Remove an environment variable from an app.", Method: "DELETE",
		Path: projectPrefix + "apps/{app}/env/{key}", EnvScoped: true, Mutating: true, Destructive: true,
		Params: []Param{pProject, pApp, pathP("key", "variable name")},
		CLI:    "luncur env unset {app} {key} --project {project} --env {env}"},
	{Name: "add_domain", Description: "Add a custom domain to a web app (HTTPS is issued automatically).", Method: "POST",
		Path: projectPrefix + "apps/{app}/domains", EnvScoped: true, Mutating: true,
		Params: []Param{pProject, pApp, bodyS("hostname", "hostname, e.g. www.example.com", true)},
		CLI:    "luncur domain add {app} {hostname} --project {project} --env {env}"},
	{Name: "remove_domain", Description: "Remove a custom domain from an app.", Method: "DELETE",
		Path: projectPrefix + "apps/{app}/domains/{hostname}", EnvScoped: true, Mutating: true, Destructive: true,
		Params: []Param{pProject, pApp, pathP("hostname", "hostname")},
		CLI:    "luncur domain remove {app} {hostname} --project {project} --env {env}"},
	{Name: "retry_domain", Description: "Retry certificate issuance for a domain stuck pending or failed.", Method: "POST",
		Path: projectPrefix + "apps/{app}/domains/{hostname}/retry", EnvScoped: true, Mutating: true,
		Params: []Param{pProject, pApp, pathP("hostname", "hostname")},
		CLI:    "luncur domain retry {app} {hostname} --project {project} --env {env}"},
	{Name: "create_addon", Description: "Create a managed addon (postgres, redis, minio, mlflow), optionally attaching it to an app (injects DATABASE_URL etc.).", Method: "POST",
		Path: projectPrefix + "addons", EnvScoped: true, Mutating: true,
		Params: []Param{pProject, {Name: "type", Type: "string", In: "body", Required: true, Enum: []string{"postgres", "redis", "minio", "mlflow"}, Description: "addon type"},
			bodyS("name", "addon name (default: the type)", false), bodyS("version", "version", false), bodyI("size_gb", "storage size in GB", false),
			bodyS("app", "app to attach it to", false)},
		CLI: "luncur addon create {type} --name {name} --app {app} --project {project} --env {env}"},
	{Name: "attach_addon", Description: "Attach an existing addon to an app (injects its connection env vars).", Method: "POST",
		Path: projectPrefix + "addons/{name}/attach", EnvScoped: true, Mutating: true,
		Params: []Param{pProject, pathP("name", "addon name"), bodyS("app", "app name", true)},
		CLI:    "luncur addon attach {name} {app} --project {project} --env {env}"},
	{Name: "detach_addon", Description: "Detach an addon from an app.", Method: "POST",
		Path: projectPrefix + "addons/{name}/detach", EnvScoped: true, Mutating: true, Destructive: true,
		Params: []Param{pProject, pathP("name", "addon name"), bodyS("app", "app name", true)},
		CLI:    "luncur addon detach {name} {app} --project {project} --env {env}"},
	{Name: "upgrade_addon", Description: "Upgrade an addon in place to a newer version.", Method: "POST",
		Path: projectPrefix + "addons/{name}/upgrade", EnvScoped: true, Mutating: true,
		Params: []Param{pProject, pathP("name", "addon name"), bodyS("version", "target version", true)},
		CLI:    "luncur addon upgrade {name} --version {version} --project {project} --env {env}"},
	{Name: "delete_addon", Description: "Delete an addon. keep_data keeps its volume; force deletes even while attached.", Method: "DELETE",
		Path: projectPrefix + "addons/{name}", EnvScoped: true, Mutating: true, Destructive: true,
		Params: []Param{pProject, pathP("name", "addon name"), queryB("keep_data", "keep the data volume"), queryB("force", "delete even if attached")},
		CLI:    "luncur addon remove {name} --project {project} --env {env}"},
	{Name: "add_volume", Description: "Mount a persistent volume into a web or worker app.", Method: "POST",
		Path: projectPrefix + "apps/{app}/volumes", EnvScoped: true, Mutating: true,
		Params: []Param{pProject, pApp, bodyS("path", "mount path, e.g. /data", true), bodyS("name", "volume name", false), bodyI("size_gb", "size in GB", false)},
		CLI:    "luncur volume add {app} {path} --size {size_gb} --project {project}"},
	{Name: "delete_volume", Description: "Unmount a volume; purge also deletes its data.", Method: "DELETE",
		Path: projectPrefix + "apps/{app}/volumes/{name}", EnvScoped: true, Mutating: true, Destructive: true,
		Params: []Param{pProject, pApp, pathP("name", "volume name"), queryB("purge", "also delete the data")},
		CLI:    "luncur volume rm {app} {name} --project {project}"},
	{Name: "start_run", Description: "Start a run of a job app (optionally multi-node).", Method: "POST",
		Path: projectPrefix + "apps/{app}/runs", EnvScoped: true, Mutating: true,
		Params: []Param{pProject, pApp, bodyI("nodes", "node count", false), {Name: "framework", Type: "string", In: "body", Enum: []string{"torchrun", "torch"}, Description: "multi-node framework preset"}},
		CLI:    "luncur run {app} --nodes {nodes} --project {project}"},
	{Name: "create_sweep", Description: "Start a hyperparameter sweep over a job app.", Method: "POST",
		Path: projectPrefix + "apps/{app}/sweeps", EnvScoped: true, Mutating: true,
		Params: []Param{pProject, pApp, bodyS("params_yaml", "params.yaml search space", true), bodyS("metric", "metric name", true),
			{Name: "direction", Type: "string", In: "body", Required: true, Enum: []string{"min", "max"}, Description: "optimize direction"},
			bodyI("max_trials", "1..500", true), bodyI("parallel", "1..50", true), bodyB("early_stop", "median early stopping"),
			bodyI("nodes", "nodes per trial", false), bodyS("framework", "multi-node framework preset", false)},
		CLI: "luncur sweep create --app {app} --metric {metric} --direction {direction} --max-trials {max_trials} --parallel {parallel} --project {project}"},
	{Name: "stop_sweep", Description: "Stop a running sweep.", Method: "POST",
		Path: projectPrefix + "apps/{app}/sweeps/{id}/stop", EnvScoped: true, Mutating: true, Destructive: true,
		Params: []Param{pProject, pApp, pathP("id", "sweep id")}, CLI: "luncur sweep stop {id} --app {app} --project {project}"},
	{Name: "pause_cron", Description: "Pause a cron app's schedule.", Method: "POST",
		Path: projectPrefix + "apps/{app}/pause", EnvScoped: true, Mutating: true, Params: []Param{pProject, pApp}, CLI: "luncur cron pause {app} --project {project}"},
	{Name: "resume_cron", Description: "Resume a paused cron app.", Method: "POST",
		Path: projectPrefix + "apps/{app}/resume", EnvScoped: true, Mutating: true, Params: []Param{pProject, pApp}, CLI: "luncur cron resume {app} --project {project}"},
	{Name: "trigger_cron", Description: "Run a cron app now, outside its schedule.", Method: "POST",
		Path: projectPrefix + "apps/{app}/trigger", EnvScoped: true, Mutating: true, Params: []Param{pProject, pApp}, CLI: "luncur cron run-now {app} --project {project}"},
	{Name: "create_pipeline", Description: "Create a pipeline from pipeline.yaml (optionally on a cron schedule).", Method: "POST",
		Path: projectPrefix + "pipelines", Mutating: true,
		Params: []Param{pProject, bodyS("name", "pipeline name", true), bodyS("yaml", "pipeline.yaml contents", true),
			{Name: "engine", Type: "string", In: "body", Enum: []string{"native", "argo"}, Description: "engine"}, bodyS("cron", "5-field cron schedule", false)},
		CLI: "luncur pipeline create {name} --file pipeline.yaml --project {project}"},
	{Name: "update_pipeline", Description: "Update a pipeline's yaml, engine or cron.", Method: "PUT",
		Path: projectPrefix + "pipelines/{name}", Mutating: true,
		Params: []Param{pProject, pathP("name", "pipeline name"), bodyS("yaml", "new pipeline.yaml", false), bodyS("engine", "engine", false), bodyS("cron", "cron schedule (empty clears)", false)},
		CLI:    "luncur pipeline update {name} --project {project}"},
	{Name: "run_pipeline", Description: "Trigger a pipeline run now.", Method: "POST",
		Path: projectPrefix + "pipelines/{name}/runs", Mutating: true, Params: []Param{pProject, pathP("name", "pipeline name")},
		CLI: "luncur pipeline run {name} --project {project}"},
	{Name: "stop_pipeline_run", Description: "Stop a running pipeline run.", Method: "POST",
		Path: projectPrefix + "pipelines/{name}/runs/{id}/stop", Mutating: true, Destructive: true,
		Params: []Param{pProject, pathP("name", "pipeline name"), pathP("id", "run id")},
		CLI:    "luncur pipeline stop {id} --pipeline {name} --project {project}"},
	{Name: "delete_pipeline", Description: "Delete a pipeline and its run history.", Method: "DELETE",
		Path: projectPrefix + "pipelines/{name}", Mutating: true, Destructive: true, Params: []Param{pProject, pathP("name", "pipeline name")},
		CLI: "luncur pipeline rm {name} --project {project}"},
	{Name: "create_env", Description: "Create a standing environment in a project.", Method: "POST",
		Path: projectPrefix + "envs", Mutating: true,
		Params: []Param{pProject, bodyS("name", "environment name", true), bodyS("base_branch", "git branch it tracks", false)},
		CLI:    "luncur envs create {name} --project {project}"},
	{Name: "delete_env", Description: "Delete an environment and everything deployed in it.", Method: "DELETE",
		Path: projectPrefix + "envs/{env}", Mutating: true, Destructive: true, Params: []Param{pProject, pathP("env", "environment name")},
		CLI: "luncur envs rm {env} --project {project}"},
	{Name: "copy_env_setup", Description: "Copy apps, env vars and addons from one environment to another.", Method: "POST",
		Path: projectPrefix + "envs/copy", Mutating: true,
		Params: []Param{pProject, bodyS("source", "source environment", true), bodyS("target", "target environment", true)},
		CLI:    "luncur envs copy --from {source} --to {target} --project {project}"},
	{Name: "create_preview", Description: "Create a preview environment for a git branch.", Method: "POST",
		Path: projectPrefix + "previews", Mutating: true,
		Params: []Param{pProject, bodyS("branch", "git branch", true), bodyS("from", "base environment", false)},
		CLI:    "luncur preview create {branch} --project {project}"},
	{Name: "delete_preview", Description: "Delete a preview environment.", Method: "DELETE",
		Path: projectPrefix + "previews/{name}", Mutating: true, Destructive: true, Params: []Param{pProject, pathP("name", "preview name")},
		CLI: "luncur preview rm {name} --project {project}"},
	{Name: "set_override", Description: "Store a strategic-merge patch for one of an app's manifests (Deployment, Service, Ingress, CronJob) that survives redeploys.", Method: "PUT",
		Path: projectPrefix + "apps/{app}/overrides/{kind}", EnvScoped: true, Mutating: true,
		Params: []Param{pProject, pApp, {Name: "kind", Type: "string", In: "path", Required: true, Enum: []string{"Deployment", "Service", "Ingress", "CronJob"}, Description: "manifest kind"},
			{Name: "patch", Type: "object", In: "rawbody", Required: true, Description: "the strategic-merge patch, as a JSON object"}},
		CLI: "luncur edit {app} {kind} --project {project}"},
	{Name: "delete_override", Description: "Remove an app's override for one manifest kind.", Method: "DELETE",
		Path: projectPrefix + "apps/{app}/overrides/{kind}", EnvScoped: true, Mutating: true, Destructive: true,
		Params: []Param{pProject, pApp, pathP("kind", "manifest kind")}},
	{Name: "eject_app", Description: "Eject an app: luncur stops managing it and leaves its Kubernetes objects running.", Method: "POST",
		Path: projectPrefix + "apps/{app}/eject", EnvScoped: true, Mutating: true, Destructive: true, Params: []Param{pProject, pApp},
		CLI: "luncur eject {app} --project {project}"},
	{Name: "adopt_app", Description: "Adopt an ejected app back under luncur's management.", Method: "POST",
		Path: projectPrefix + "apps/{app}/adopt", EnvScoped: true, Mutating: true, Params: []Param{pProject, pApp},
		CLI: "luncur adopt {app} --project {project}"},
	{Name: "delete_app", Description: "Delete an app and its Kubernetes objects (volumes are kept).", Method: "DELETE",
		Path: projectPrefix + "apps/{app}", EnvScoped: true, Mutating: true, Destructive: true, Params: []Param{pProject, pApp},
		CLI: "luncur destroy {app} --project {project} --env {env}"},
	{Name: "delete_project", Description: "Delete a whole project, every environment and app in it.", Method: "DELETE",
		Path: "/v1/projects/{project}", Mutating: true, Destructive: true, Admin: true, Params: []Param{pProject},
		CLI: "luncur project rm {project}"},
}

// All returns every tool, in registry order.
func All() []Tool { return append([]Tool(nil), registry...) }

// Find returns the named tool.
func Find(name string) (Tool, bool) {
	for _, t := range registry {
		if t.Name == name {
			return t, true
		}
	}
	return Tool{}, false
}

// Filter returns the tools available to a caller: admins get everything;
// everyone else loses Admin tools, and read-only callers (project viewers)
// also lose every Mutating tool. The API enforces the same rules on each
// call; filtering just keeps the model from planning around tools it can't
// use.
func Filter(admin, canWrite bool) []Tool {
	var out []Tool
	for _, t := range registry {
		if t.Admin && !admin {
			continue
		}
		if t.Mutating && !canWrite && !admin {
			continue
		}
		out = append(out, t)
	}
	return out
}

// Schema is the tool's JSON Schema input object.
func (t Tool) Schema() map[string]any {
	props := map[string]any{}
	required := []string{}
	for _, p := range t.params() {
		prop := map[string]any{"type": p.Type}
		if p.Description != "" {
			prop["description"] = p.Description
		}
		if len(p.Enum) > 0 {
			prop["enum"] = p.Enum
		}
		props[p.Name] = prop
		if p.Required {
			required = append(required, p.Name)
		}
	}
	return map[string]any{"type": "object", "properties": props, "required": required}
}

// params is Params plus the implicit optional "env" of EnvScoped tools.
func (t Tool) params() []Param {
	ps := append([]Param(nil), t.Params...)
	if t.EnvScoped {
		ps = append(ps, Param{Name: "env", Type: "string", In: "env", Description: "environment name (default: the project's default environment)"})
	}
	return ps
}

// Request is a tool call resolved to an HTTP request.
type Request struct {
	Method string
	Path   string // including any query string
	Body   []byte // nil for no body
}

// BuildRequest resolves args against the tool's parameters.
func (t Tool) BuildRequest(args map[string]any) (Request, error) {
	path := t.Path
	if t.EnvScoped {
		// Resolve the route shape first: the env-qualified form inserts
		// envs/{env}/ after the project, before any placeholder is filled.
		if env := argString(args["env"]); args["env"] != nil && env != "" {
			path = strings.Replace(path, projectPrefix, projectPrefix+"envs/{env}/", 1)
		}
	}
	query := url.Values{}
	body := map[string]any{}
	var raw []byte
	for _, p := range t.params() {
		v, ok := args[p.Name]
		if !ok || v == nil {
			if p.Required {
				return Request{}, fmt.Errorf("%s: missing required argument %q", t.Name, p.Name)
			}
			continue
		}
		switch p.In {
		case "path":
			s := argString(v)
			if s == "" {
				return Request{}, fmt.Errorf("%s: argument %q must not be empty", t.Name, p.Name)
			}
			path = strings.ReplaceAll(path, "{"+p.Name+"}", url.PathEscape(s))
		case "env":
			if s := argString(v); s != "" {
				path = strings.ReplaceAll(path, "{env}", url.PathEscape(s))
			}
		case "query":
			if s := argString(v); s != "" && s != "false" {
				if p.Type == "boolean" {
					s = "1"
				}
				query.Set(p.Name, s)
			}
		case "rawbody":
			b, err := json.Marshal(v)
			if err != nil {
				return Request{}, fmt.Errorf("%s: argument %q: %w", t.Name, p.Name, err)
			}
			raw = b
		default: // body
			body[p.Name] = v
		}
	}
	if strings.Contains(path, "{") {
		return Request{}, fmt.Errorf("%s: unresolved path %s", t.Name, path)
	}
	req := Request{Method: t.Method, Path: path}
	if len(query) > 0 {
		req.Path += "?" + query.Encode()
	}
	switch {
	case raw != nil:
		req.Body = raw
	case t.Method != "GET" && (len(body) > 0 || t.Method == "POST" || t.Method == "PUT"):
		b, err := json.Marshal(body)
		if err != nil {
			return Request{}, err
		}
		req.Body = b
	}
	return req, nil
}

// argString renders a decoded JSON argument as a string (integers without a
// decimal point).
func argString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(x)
	case json.Number:
		return x.String()
	default:
		b, _ := json.Marshal(x)
		return string(b)
	}
}

// CLIEcho renders the luncur command equivalent to a call (DESIGN.md
// CLI-echo). Flags whose placeholder has no value are dropped; tools with
// no CLI template echo the raw API call.
func (t Tool) CLIEcho(args map[string]any) string {
	if t.CLI == "" {
		r, err := t.BuildRequest(args)
		if err != nil {
			return ""
		}
		return "# API: " + r.Method + " " + r.Path
	}
	fields := strings.Fields(t.CLI)
	var out []string
	for i := 0; i < len(fields); i++ {
		f := fields[i]
		if strings.HasPrefix(f, "--") && i+1 < len(fields) && isPlaceholder(fields[i+1]) {
			if v := placeholderValue(fields[i+1], args); v != "" {
				out = append(out, f, shellQuote(v))
			}
			i++
			continue
		}
		if strings.Contains(f, "{") {
			out = append(out, shellQuote(fillPlaceholders(f, args)))
			continue
		}
		out = append(out, f)
	}
	return strings.Join(out, " ")
}

func isPlaceholder(s string) bool { return strings.HasPrefix(s, "{") && strings.HasSuffix(s, "}") }

func placeholderValue(s string, args map[string]any) string {
	v, ok := args[strings.Trim(s, "{}")]
	if !ok || v == nil {
		return ""
	}
	if b, isBool := v.(bool); isBool && !b {
		return ""
	}
	return argString(v)
}

func fillPlaceholders(f string, args map[string]any) string {
	for {
		i := strings.Index(f, "{")
		j := strings.Index(f, "}")
		if i < 0 || j < i {
			return f
		}
		f = f[:i] + placeholderValue(f[i:j+1], args) + f[j+1:]
	}
}

func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	safe := true
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("-_./:=@%+,<>", r)) {
			safe = false
			break
		}
	}
	if safe {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// ShapeResponse turns an API response into the text handed back to the
// model: SSE flattened to lines, env values stripped, error envelopes
// summarized. isError reports a non-2xx status.
func (t Tool) ShapeResponse(status int, body []byte) (string, bool) {
	if status >= 300 {
		var env struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(body, &env) == nil && env.Error.Code != "" {
			return fmt.Sprintf("HTTP %d %s: %s", status, env.Error.Code, env.Error.Message), true
		}
		return fmt.Sprintf("HTTP %d: %s", status, strings.TrimSpace(string(body))), true
	}
	if t.KeysOnly {
		var m map[string]any
		if err := json.Unmarshal(body, &m); err == nil {
			keys := make([]string, 0, len(m))
			for k := range m {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			b, _ := json.Marshal(map[string]any{"keys": keys, "note": "values are hidden"})
			return string(b), false
		}
		return `{"keys":[],"note":"values are hidden"}`, false
	}
	if t.SSE {
		var lines []string
		sc := bufio.NewScanner(bytes.NewReader(body))
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		ending := false
		for sc.Scan() {
			line := sc.Text()
			switch {
			case line == "event: end":
				ending = true
			case strings.HasPrefix(line, "data: ") && !ending:
				lines = append(lines, strings.TrimPrefix(line, "data: "))
			}
		}
		if len(lines) == 0 {
			return "(no log lines)", false
		}
		return strings.Join(lines, "\n"), false
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return fmt.Sprintf("OK (HTTP %d)", status), false
	}
	return string(body), false
}
