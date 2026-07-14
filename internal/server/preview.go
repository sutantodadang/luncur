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
	"strings"

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
//     environment (teardownPreview — stubbed until Task 15) if one exists
//     (matched on SourceBranch, not name — sanitizeBranch's naming scheme
//     is Task 13's concern, not this dispatch); no matching preview is a
//     no-op.
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
// active preview survives Task 15's idle-TTL reaper. A single app's deploy
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

// teardownPreview tears down a preview environment (its namespace and every
// row scoped to it). Stubbed here — Task 15 implements the real teardown
// (PR-close/branch-delete, idle-TTL reaper, and manual delete all share
// this core); for now it only logs, so routeBranch's delete path compiles
// and has somewhere to call.
func (s *server) teardownPreview(ctx context.Context, p store.Project, env store.Environment) error {
	log.Printf("teardown preview %s/%s: not yet implemented (Task 15)", p.Name, env.Name)
	return nil
}

// ensurePreview will create (or return an existing) preview environment for
// branch, cloning app specs and addon data from the project's preview base
// environment (p.PreviewBaseEnv). Implemented in Task 13 (sanitizeBranch +
// the create-and-clone core); stubbed here so routeBranch compiles and has
// somewhere to call.
func (s *server) ensurePreview(ctx context.Context, p store.Project, branch string) (store.Environment, error) {
	return store.Environment{}, fmt.Errorf("ensurePreview: not yet implemented (Task 13)")
}
