package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/sutantodadang/luncur/internal/store"
)

// projectWebhookPath is the authenticated project's build webhook URL a git
// provider posts pushes to.
func projectWebhookPath(project string) string {
	return "/v1/projects/" + project + "/webhook"
}

// enableProjectWebhook is handleGenerateProjectWebhookSecret's shared core:
// generate a fresh 32-byte secret (hex-encoded, 64 chars — what the
// provider signs/compares with), seal it, and store it. Always
// regenerates: calling this on a project that already has a secret rotates
// it, invalidating the old one — same convention as enableWebhook
// (webhook.go, app deploy hooks) and generatePipelineWebhookSecret
// (pipelines.go, pipeline triggers). Returns the plaintext hex secret — the
// ONLY time it is ever available in plaintext; only the sealed form
// persists.
func (s *server) enableProjectWebhook(p store.Project) (string, error) {
	if s.sealer == nil {
		return "", errSealerUnavailable
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	secretHex := hex.EncodeToString(raw)
	sealed, err := s.sealer.Seal([]byte(secretHex))
	if err != nil {
		return "", err
	}
	if err := s.st.SetProjectWebhookSecret(p.ID, sealed); err != nil {
		return "", err
	}
	return secretHex, nil
}

// handleGenerateProjectWebhookSecret turns on (or rotates) a project's
// build webhook. The secret is returned in this response ONLY — it is
// never recoverable from the store afterward (only the sealed bytes
// persist), mirroring handleWebhookEnable/handleGeneratePipelineWebhookSecret.
func (s *server) handleGenerateProjectWebhookSecret(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.requireProjectWrite(w, u, r.PathValue("project"))
	if !ok {
		return
	}
	secretHex, err := s.enableProjectWebhook(p)
	if err != nil {
		if errors.Is(err, errSealerUnavailable) {
			writeError(w, http.StatusServiceUnavailable, "sealer_unavailable", errSealerUnavailable.Error())
			return
		}
		log.Printf("generate project webhook secret: %v", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"path": projectWebhookPath(p.Name), "secret": secretHex,
	})
}

// projectWebhookPayload is the shared push/PR-close payload shape used by
// GitHub and GitLab, extended (relative to webhook.go's app-level payload)
// with the fields a project-level branch router needs: a branch-delete
// push, and a merge/pull-request close so a preview environment can be torn
// down instead of (re)deployed.
type projectWebhookPayload struct {
	Ref        string `json:"ref"`
	ObjectKind string `json:"object_kind"`
	Deleted    bool   `json:"deleted"`
	// GitHub pull_request event: no top-level "ref"; the pushed branch is
	// under pull_request.head.ref, and a close is pull_request.action.
	PullRequest struct {
		Action string `json:"action"`
		Head   struct {
			Ref string `json:"ref"`
		} `json:"head"`
	} `json:"pull_request"`
	// GitLab merge_request event: object_kind=="merge_request", branch
	// under object_attributes.source_branch, close/merge under
	// object_attributes.action.
	ObjectAttributes struct {
		Action       string `json:"action"`
		SourceBranch string `json:"source_branch"`
	} `json:"object_attributes"`
}

// branch returns the pushed/PR branch name and whether this event means the
// branch is gone (a delete push, or a closed/merged PR/MR) — routeBranch's
// signal to tear a preview down instead of (re)deploying it.
func (p projectWebhookPayload) branch() (branch string, isDelete bool) {
	branch = strings.TrimPrefix(p.Ref, "refs/heads/")
	isDelete = p.Deleted
	if p.PullRequest.Action == "closed" {
		isDelete = true
		if branch == "" {
			branch = p.PullRequest.Head.Ref
		}
	} else if p.ObjectAttributes.Action == "close" || p.ObjectAttributes.Action == "merge" {
		isDelete = true
		if branch == "" {
			branch = p.ObjectAttributes.SourceBranch
		}
	}
	return branch, isDelete
}

// handleProjectWebhook is the public (unauthenticated at the HTTP-auth
// layer) endpoint a git provider posts pushes/PR events to for
// branch-to-environment routing. Auth is the HMAC/token check itself,
// reusing verifyWebhook (webhook.go) exactly like the per-app deploy hook:
// every failure up to and including a bad signature answers with the
// identical 401 body (webhookUnauthorized) — no existence oracle for a
// project name.
func (s *server) handleProjectWebhook(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, webhookMaxBody)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		webhookUnauthorized(w)
		return
	}

	p, err := s.st.GetProject(r.PathValue("project"))
	if err != nil {
		webhookUnauthorized(w)
		return
	}
	if p.WebhookSecret == nil || s.sealer == nil {
		webhookUnauthorized(w)
		return
	}
	plain, err := s.sealer.Open(p.WebhookSecret)
	if err != nil {
		webhookUnauthorized(w)
		return
	}
	if !verifyWebhook(r, body, string(plain)) {
		webhookUnauthorized(w)
		return
	}

	if info := auditFrom(r.Context()); info != nil {
		info.Email = "webhook"
		info.Pattern = r.Pattern
	}

	// Authenticated from here on — failures are ordinary status codes.
	if r.Header.Get("X-GitHub-Event") == "ping" {
		writeJSON(w, http.StatusOK, map[string]any{"pong": true})
		return
	}

	var payload projectWebhookPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	if payload.ObjectKind != "" && payload.ObjectKind != "push" && payload.ObjectKind != "merge_request" {
		writeJSON(w, http.StatusOK, map[string]any{"skipped": "event"})
		return
	}

	branch, isDelete := payload.branch()
	if branch == "" {
		writeJSON(w, http.StatusOK, map[string]any{"skipped": "no_branch"})
		return
	}

	if err := s.routeBranch(r.Context(), p, branch, isDelete); err != nil {
		log.Printf("project webhook: route branch %q: %v", branch, err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"branch": branch, "deleted": isDelete})
}

// routeBranch is handleProjectWebhook's post-auth core: dispatch a pushed
// (or PR-closed/branch-deleted) branch to the right place.
//
//   - A standing environment whose base_branch matches deploys that
//     environment's git apps (deployEnvGitApps, reusing the same
//     deployGitApp core the per-app webhook and API/UI deploy paths share)
//     and bumps its LastActiveAt.
//   - Otherwise, a delete/PR-close event tears down the matching preview
//     environment (teardownPreview) if one exists (matched on SourceBranch,
//     not name — sanitizeBranch's naming scheme is Task 13's concern, not
//     this dispatch); no matching preview is a no-op.
//   - Otherwise, the branch is routed to its preview environment
//     (ensurePreview, Task 13), creating one on first push and reusing it
//     on every later push, and that environment's git apps are (re)deployed.
func (s *server) routeBranch(ctx context.Context, p store.Project, branch string, isDelete bool) error {
	envs, err := s.st.ListEnvironments(p.ID)
	if err != nil {
		return fmt.Errorf("list environments: %w", err)
	}
	for _, env := range envs {
		if env.Kind == "standing" && env.BaseBranch != "" && env.BaseBranch == branch {
			return s.deployEnvGitApps(ctx, p, env)
		}
	}

	if isDelete {
		for _, env := range envs {
			if env.Kind == "preview" && env.SourceBranch == branch {
				return s.teardownPreview(ctx, p, env)
			}
		}
		return nil // nothing to tear down
	}

	env, err := s.ensurePreview(ctx, p, branch)
	if err != nil {
		return fmt.Errorf("ensure preview: %w", err)
	}
	return s.deployEnvGitApps(ctx, p, env)
}

// deployEnvGitApps deploys every git-source, non-ejected app in env from
// its configured branch — the shared "a webhook fired for this
// environment" core for both a standing environment's base-branch match
// and a preview environment's repeat push. Always touches the
// environment's LastActiveAt (even if it has no git apps to deploy) so an
// active preview survives reapPreviews' idle-TTL sweep. A single app's deploy
// failure is logged and does not stop the others — best effort, mirroring
// backup's per-item resilience.
func (s *server) deployEnvGitApps(ctx context.Context, p store.Project, env store.Environment) error {
	apps, err := s.st.ListAppsInEnv(env.ID)
	if err != nil {
		return fmt.Errorf("list apps: %w", err)
	}
	for _, a := range apps {
		if a.SourceType != "git" || a.Ejected {
			continue
		}
		if _, err := s.deployGitApp(p, env, a, 0); err != nil {
			log.Printf("route branch: deploy %s/%s: %v", env.Name, a.Name, err)
		}
	}
	if err := s.st.TouchEnvironment(env.ID); err != nil {
		log.Printf("route branch: touch environment %s: %v", env.Name, err)
	}
	return nil
}

// teardownPreview tears down a preview environment: its namespace (cascades
// every pod/service/PVC inside — apps and addons alike) and every row
// scoped to it — addons, then apps, then the environment row itself. Shared
// core for routeBranch's PR-close/branch-delete path, reapPreviews' idle-TTL
// loop, and any future manual delete. Defensively refuses anything but a
// kind=='preview' environment: every caller already filters on kind before
// reaching here, but this guard means a bug in one of them can never nuke a
// standing environment's namespace and rows.
func (s *server) teardownPreview(ctx context.Context, p store.Project, env store.Environment) error {
	if env.Kind != "preview" {
		return fmt.Errorf("teardown preview: environment %q is kind %q, not preview — refusing", env.Name, env.Kind)
	}

	if s.kube != nil {
		// Namespace may already be gone (e.g. a prior partial run); log and
		// continue rather than fail the whole teardown on that alone —
		// mirrors deleteEnvironment/deleteProject's own namespace-delete
		// handling.
		if err := s.kube.DeleteNamespace(ctx, env.Namespace); err != nil {
			log.Printf("teardown preview %s/%s: delete namespace %s: %v", p.Name, env.Name, env.Namespace, err)
		}
	}

	addons, err := s.st.AddonsForEnv(env.ID)
	if err != nil {
		return fmt.Errorf("list addons: %w", err)
	}
	for _, ad := range addons {
		if err := s.st.DeleteAddon(ad.ID); err != nil && !errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("delete addon %s: %w", ad.Name, err)
		}
	}

	apps, err := s.st.ListAppsInEnv(env.ID)
	if err != nil {
		return fmt.Errorf("list apps: %w", err)
	}
	for _, a := range apps {
		if err := s.st.DeleteApp(a.ID); err != nil && !errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("delete app %s: %w", a.Name, err)
		}
	}

	return s.st.DeleteEnvironment(env.ID)
}

// defaultPreviewTTLDays is how long a preview environment survives with no
// deploy activity before reapPreviews tears it down, when the
// preview_ttl_days install setting is unset.
const defaultPreviewTTLDays = 7

// previewTTLDays reads the preview_ttl_days setting, falling back to
// defaultPreviewTTLDays when unset or invalid — mirrors pruneBackups'
// backup_keep fallback.
func (s *server) previewTTLDays() int {
	v, err := s.st.GetSetting("preview_ttl_days")
	if err != nil {
		return defaultPreviewTTLDays
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		return defaultPreviewTTLDays
	}
	return n
}

// previewLastActiveLayout matches the format SQLite's datetime('now') writes
// (TouchEnvironment, CreateEnvironment's last_active_at default) — the same
// layout runBackupSchedule already parses backups.CreatedAt with.
const previewLastActiveLayout = "2006-01-02 15:04:05"

// reapPreviews tears down every preview environment, across every project,
// whose LastActiveAt is older than previewTTLDays. Every deploy into an
// environment (deployEnvGitApps, applyImageDeploy, finishDeploy) touches
// LastActiveAt, so only a truly idle preview is ever reaped. Standing
// environments are never inspected — the kind=='preview' filter runs before
// any teardown call, and teardownPreview itself refuses non-preview
// environments as a second line of defense. A single project's or
// environment's failure is logged and does not stop the sweep.
func (s *server) reapPreviews(ctx context.Context) {
	ttl := time.Duration(s.previewTTLDays()) * 24 * time.Hour
	projects, err := s.st.ListProjects()
	if err != nil {
		log.Printf("reap previews: list projects: %v", err)
		return
	}
	for _, p := range projects {
		envs, err := s.st.ListEnvironments(p.ID)
		if err != nil {
			log.Printf("reap previews: list environments for %s: %v", p.Name, err)
			continue
		}
		for _, env := range envs {
			if env.Kind != "preview" {
				continue
			}
			last, err := time.Parse(previewLastActiveLayout, env.LastActiveAt)
			if err != nil {
				log.Printf("reap previews: parse last_active_at for %s/%s: %v", p.Name, env.Name, err)
				continue
			}
			if s.nowFn().UTC().Sub(last) < ttl {
				continue
			}
			if err := s.teardownPreview(ctx, p, env); err != nil {
				log.Printf("reap previews: teardown %s/%s: %v", p.Name, env.Name, err)
			}
		}
	}
}

// StartPreviewReaper runs the idle-preview reap loop: every hour, delegate
// to reapPreviews. Mirrors StartBackups' lifecycle.
func (s *server) StartPreviewReaper(ctx context.Context) {
	tick := time.NewTicker(time.Hour)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			s.reapPreviews(ctx)
		}
	}
}

// maxSanitizedBranch caps sanitizeBranch's output length: short enough that
// "<app>-<env>" (hostForEnv's non-default-environment host label) still
// fits comfortably under validName's 40-char DNS-1123 limit alongside a
// realistic app name.
const maxSanitizedBranch = 30

// sanitizeBranch turns an arbitrary git branch name into a DNS-1123-safe
// environment name: lowercase, every character outside [a-z0-9] (including
// "/" and any other punctuation) becomes "-", repeated dashes collapse to
// one, and leading/trailing dashes are trimmed. The result is truncated to
// maxSanitizedBranch chars (trimming any dash truncation lands on) so it
// always passes store.validName — non-empty, DNS-1123 label shaped.
func sanitizeBranch(b string) string {
	b = strings.ToLower(b)
	var sb strings.Builder
	for _, r := range b {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			sb.WriteRune(r)
		} else {
			sb.WriteByte('-')
		}
	}
	s := sb.String()
	for strings.Contains(s, "--") {
		s = strings.ReplaceAll(s, "--", "-")
	}
	s = strings.Trim(s, "-")
	if len(s) > maxSanitizedBranch {
		s = strings.Trim(s[:maxSanitizedBranch], "-")
	}
	if s == "" {
		s = "branch"
	}
	return s
}

// ensurePreview returns the preview environment for branch on project p,
// creating one if it doesn't already exist. A second call for the same
// branch is a no-op that returns the existing environment unchanged — the
// caller (routeBranch) redeploys into it either way, so ensurePreview never
// needs to distinguish a fresh preview from a repeat push.
//
// A freshly created preview is cloned from p.PreviewBaseEnv (falling back
// to "develop" when unset, matching the SQL-level default): every app in
// the base environment gets a same-shaped counterpart (CreateAppInEnv plus
// the scalar setters — kind/port came from CreateAppInEnv itself) with
// replicas capped low, its env vars copied, and — for a git-source app —
// the same repo with git_branch overridden to the pushed branch. Addon data
// (postgres/redis via dump->restore, minio/mlflow created empty) is seeded
// separately by clonePreviewAddons, below.
//
// ensurePreview always resolves its base the automatic way; a caller that
// needs to override which environment a fresh preview clones from (the
// manual `POST .../previews` endpoint's `from` field, Task 16) calls
// ensurePreviewFromBase directly.
func (s *server) ensurePreview(ctx context.Context, p store.Project, branch string) (store.Environment, error) {
	return s.ensurePreviewFromBase(ctx, p, branch, "")
}

// ensurePreviewFromBase is ensurePreview's actual implementation, with an
// optional explicit base-environment override: baseOverride=="" reproduces
// ensurePreview's original behavior exactly (p.PreviewBaseEnv, falling back
// to "develop"); a non-empty baseOverride clones from that environment
// instead. The caller is responsible for validating baseOverride names a
// real (and, for handleCreatePreview, standing) environment before calling
// this — ensurePreviewFromBase itself just looks it up and 404s via the
// ordinary GetEnvironment error path if it doesn't exist.
func (s *server) ensurePreviewFromBase(ctx context.Context, p store.Project, branch, baseOverride string) (store.Environment, error) {
	name := sanitizeBranch(branch)
	if existing, err := s.st.GetEnvironment(p.ID, name); err == nil {
		return existing, nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return store.Environment{}, fmt.Errorf("get environment: %w", err)
	}

	baseName := baseOverride
	if baseName == "" {
		baseName = p.PreviewBaseEnv
	}
	if baseName == "" {
		baseName = "develop"
	}
	base, err := s.st.GetEnvironment(p.ID, baseName)
	if err != nil {
		return store.Environment{}, fmt.Errorf("get base environment %q: %w", baseName, err)
	}

	env, err := s.st.CreateEnvironment(p.ID, name, "preview", "")
	if err != nil {
		return store.Environment{}, fmt.Errorf("create preview environment: %w", err)
	}
	if err := s.st.SetEnvironmentSourceBranch(env.ID, branch); err != nil {
		return store.Environment{}, fmt.Errorf("set source branch: %w", err)
	}
	env.SourceBranch = branch

	if s.kube != nil {
		if err := s.ensureEnvNamespace(ctx, env); err != nil {
			return store.Environment{}, fmt.Errorf("ensure namespace: %w", err)
		}
	}

	baseApps, err := s.st.ListAppsInEnv(base.ID)
	if err != nil {
		return store.Environment{}, fmt.Errorf("list base apps: %w", err)
	}
	for _, a := range baseApps {
		if err := s.clonePreviewApp(env, a, branch); err != nil {
			log.Printf("ensure preview: clone app %s: %v", a.Name, err)
		}
	}

	if warnings := s.clonePreviewAddons(ctx, base, env); len(warnings) > 0 {
		log.Printf("ensure preview: addon clone warnings: %v", warnings)
	}

	return env, nil
}

// clonePreviewApp creates one preview-environment counterpart of a base-env
// app: same kind/port (via CreateAppInEnv), then resources/health/internal/
// gpu/inject_s3 copied and replicas capped low (1) so a preview doesn't
// reproduce its base's full footprint. A git-source app gets the same repo
// with git_branch overridden to the pushed branch. Env vars are cloned
// separately (clonePreviewEnvVars) since they're sealed bytes, not scalar
// columns.
func (s *server) clonePreviewApp(env store.Environment, base store.App, branch string) error {
	a, err := s.st.CreateAppInEnv(env.ID, base.Name, base.Port, base.Kind, base.Schedule)
	if err != nil {
		return fmt.Errorf("create app: %w", err)
	}

	replicas := base.Replicas
	if replicas > 1 {
		replicas = 1
	}
	if err := s.st.SetReplicas(a.ID, replicas); err != nil {
		return fmt.Errorf("set replicas: %w", err)
	}
	if base.CPUMilli > 0 || base.MemoryMB > 0 {
		if err := s.st.SetResources(a.ID, base.CPUMilli, base.MemoryMB); err != nil {
			return fmt.Errorf("set resources: %w", err)
		}
	}
	if base.HealthPath != "" {
		if err := s.st.SetHealthPath(a.ID, base.HealthPath); err != nil {
			return fmt.Errorf("set health path: %w", err)
		}
	}
	if base.Internal {
		if err := s.st.SetInternal(a.ID, true); err != nil {
			return fmt.Errorf("set internal: %w", err)
		}
	}
	if base.GPUCount > 0 {
		if err := s.st.SetGPU(a.ID, base.GPUCount); err != nil {
			return fmt.Errorf("set gpu: %w", err)
		}
	}
	if base.InjectS3 {
		if err := s.st.SetInjectS3(a.ID, true); err != nil {
			return fmt.Errorf("set inject s3: %w", err)
		}
	}
	if base.SourceType == "git" {
		if err := s.st.SetAppGitSource(a.ID, base.GitURL, branch); err != nil {
			return fmt.Errorf("set git source: %w", err)
		}
	}
	if err := s.clonePreviewEnvVars(base.ID, a.ID); err != nil {
		return fmt.Errorf("clone env vars: %w", err)
	}
	return nil
}

// clonePreviewEnvVars copies every env var from a base-env app to its
// preview-env counterpart. Values arrive already sealed (ListEnv/SetEnv
// never see plaintext), so this is a pure sealed-bytes copy — no sealer
// round trip needed.
func (s *server) clonePreviewEnvVars(baseAppID, previewAppID int64) error {
	vars, err := s.st.ListEnv(baseAppID)
	if err != nil {
		return err
	}
	for k, v := range vars {
		if err := s.st.SetEnv(previewAppID, k, v); err != nil {
			return err
		}
	}
	return nil
}

// clonePreviewAddons seeds a preview environment's addon data from its base
// environment: for every addon actually provisioned into base
// (AddonsForEnv, not the whole-project ListAddons), create a same-typed
// addon in preview via createAddon — the same core handleCreateAddon uses,
// so the preview's creds/secret get minted and applied identically. name is
// left "" so createAddon mints its own project-wide-unique name (addons.go's
// UNIQUE(project_id, name) means the preview's clone is a genuinely separate
// provisioned instance, never the base's own row). postgres/redis then get
// their data seeded via the same dump->restore path backup/restore use;
// minio/mlflow have no logical dump, so the clone is left freshly
// provisioned and empty. Any single addon's failure (create, dump, or
// restore) degrades to a warning rather than aborting the rest — a partial
// preview beats none, mirroring createBackup's per-addon resilience.
// Returns the warnings for the caller (ensurePreview) to log.
func (s *server) clonePreviewAddons(ctx context.Context, base, preview store.Environment) []string {
	addons, err := s.st.AddonsForEnv(base.ID)
	if err != nil {
		return []string{fmt.Sprintf("list base addons: %v", err)}
	}
	if len(addons) == 0 {
		return nil
	}
	if s.kube == nil {
		return []string{"kubernetes unavailable, addons not cloned"}
	}
	p, err := s.st.GetProjectByID(base.ProjectID)
	if err != nil {
		return []string{fmt.Sprintf("get project: %v", err)}
	}

	var warnings []string
	for _, ad := range addons {
		newAddon, err := s.createAddon(ctx, p, preview, ad.Type, "", ad.Version, ad.SizeGB, "")
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("create %s addon %s: %v", ad.Type, ad.Name, err))
			continue
		}

		switch ad.Type {
		case "postgres", "redis":
			if s.execer == nil {
				warnings = append(warnings, fmt.Sprintf("addon %s: exec unavailable, addon created empty", ad.Name))
				continue
			}
			dump, _, err := s.dumpAddon(ctx, ad)
			if err != nil {
				warnings = append(warnings, fmt.Sprintf("dump addon %s: %v", ad.Name, err))
				continue
			}
			if err := s.restoreAddon(ctx, newAddon, dump); err != nil {
				warnings = append(warnings, fmt.Sprintf("restore addon %s data into %s: %v", ad.Name, newAddon.Name, err))
			}
		default:
			warnings = append(warnings, fmt.Sprintf("addon %s (%s): created empty, no data clone supported", ad.Name, ad.Type))
		}

		// Re-point the preview apps' addon attachments the same way base's
		// were wired: any base app attached to ad gets its already-cloned
		// preview counterpart (clonePreviewApp always runs before this, in
		// ensurePreview) attached to newAddon instead.
		for _, w := range s.clonePreviewAddonAttachments(preview, ad, newAddon) {
			warnings = append(warnings, w)
		}
	}
	return warnings
}

// clonePreviewAddonAttachments replicates base's app-addon attachments onto
// preview: for every app in base attached to ad, the same-named app in
// preview gets newAddon attached via the store directly (not the
// name-keyed attachAddon helper — apps can share a name across
// environments, e.g. base and preview both have "api", and attachAddon's
// project+name lookup can't tell them apart; GetAppInEnv can). A base app
// clonePreviewApp failed to clone has no preview counterpart and is
// skipped, not an error — it simply has nothing to attach to.
func (s *server) clonePreviewAddonAttachments(preview store.Environment, base, newAddon store.Addon) []string {
	baseApps, err := s.st.AppsForAddon(base.ID)
	if err != nil {
		return []string{fmt.Sprintf("list apps attached to %s: %v", base.Name, err)}
	}
	var warnings []string
	for _, ba := range baseApps {
		previewApp, err := s.st.GetAppInEnv(preview.ID, ba.Name)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				continue
			}
			warnings = append(warnings, fmt.Sprintf("look up preview app %s: %v", ba.Name, err))
			continue
		}
		if err := s.st.AttachAddon(newAddon.ID, previewApp.ID); err != nil {
			warnings = append(warnings, fmt.Sprintf("attach %s to %s: %v", newAddon.Name, ba.Name, err))
		}
	}
	return warnings
}

// previewJSON is the shared list/create response shape for a preview
// environment: its name, source branch, idle-activity timestamp (reapPreviews'
// TTL clock), and each of its cloned apps' public/internal URL — the same
// URL-resolution rule appJSON uses (appURLForEnv for a public app,
// internalURLFor for an internal one). Shared by handleListPreviews,
// handleCreatePreview, and the UI's Previews card (ui.go).
func (s *server) previewJSON(p store.Project, env store.Environment) map[string]any {
	apps, _ := s.st.ListAppsInEnv(env.ID)
	appsOut := make([]map[string]any, 0, len(apps))
	for _, a := range apps {
		u := s.appURLForEnv(a, env.Name, p.DefaultEnv)
		if a.Internal {
			u = internalURLFor(a.Name, env.Namespace)
		}
		appsOut = append(appsOut, map[string]any{"name": a.Name, "url": u})
	}
	return map[string]any{
		"name":           env.Name,
		"source_branch":  env.SourceBranch,
		"last_active_at": env.LastActiveAt,
		"apps":           appsOut,
	}
}

// handleListPreviews lists a project's preview environments (kind=='preview'
// only — its standing environments have their own listing, handleListEnvs).
// Read-only, so any project member may call it, mirroring handleListEnvs.
func (s *server) handleListPreviews(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.requireProject(w, u, r.PathValue("project"))
	if !ok {
		return
	}
	envs, err := s.st.ListEnvironments(p.ID)
	if err != nil {
		log.Printf("list previews: %v", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	out := make([]map[string]any, 0, len(envs))
	for _, e := range envs {
		if e.Kind != "preview" {
			continue
		}
		out = append(out, s.previewJSON(p, e))
	}
	writeJSON(w, http.StatusOK, out)
}

// handleCreatePreview manually creates (or, for an already-pushed branch,
// re-resolves — ensurePreview/ensurePreviewFromBase are both idempotent on
// name) a preview environment for a branch, the same core the project
// webhook's branch router uses. from, when non-empty, overrides which
// standing environment the preview clones from instead of the project's
// configured PreviewBaseEnv; it must name a real standing environment (400
// on an unknown name or on naming a preview environment as a base — a
// preview cloning a preview is not a supported shape).
func (s *server) handleCreatePreview(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.requireProjectWrite(w, u, r.PathValue("project"))
	if !ok {
		return
	}
	if !s.requireKube(w) {
		return
	}

	var req struct {
		Branch string `json:"branch"`
		From   string `json:"from"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	if req.Branch == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "branch is required")
		return
	}

	if req.From != "" {
		base, err := s.st.GetEnvironment(p.ID, req.From)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				writeError(w, http.StatusBadRequest, "bad_request", "no such base environment")
				return
			}
			log.Printf("create preview: get base environment: %v", err)
			writeError(w, http.StatusInternalServerError, "internal", "internal error")
			return
		}
		if base.Kind != "standing" {
			writeError(w, http.StatusBadRequest, "bad_request", "base environment must be a standing environment")
			return
		}
	}

	env, err := s.ensurePreviewFromBase(r.Context(), p, req.Branch, req.From)
	if err != nil {
		log.Printf("create preview: %v", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	writeJSON(w, http.StatusCreated, s.previewJSON(p, env))
}

// handleDeletePreview manually tears down a named preview environment
// (teardownPreview — the same core the idle-TTL reaper and PR-close webhook
// path use). 404s on an unknown environment name and, distinctly, on a
// standing environment: this route only ever operates on kind=='preview'
// rows, so naming a standing environment here reports the same "no such
// preview environment" 404 rather than tearing it down.
func (s *server) handleDeletePreview(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.requireProjectWrite(w, u, r.PathValue("project"))
	if !ok {
		return
	}
	env, err := s.st.GetEnvironment(p.ID, r.PathValue("name"))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "no such preview environment")
			return
		}
		log.Printf("delete preview: get environment: %v", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	if env.Kind != "preview" {
		writeError(w, http.StatusNotFound, "not_found", "no such preview environment")
		return
	}
	if err := s.teardownPreview(r.Context(), p, env); err != nil {
		log.Printf("delete preview: %v", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
