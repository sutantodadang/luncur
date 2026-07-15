package server

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"

	"github.com/sutantodadang/luncur/internal/store"
)

// environmentJSON is the shared list/create/set-default response shape for
// an environment.
func environmentJSON(e store.Environment) map[string]any {
	return map[string]any{
		"name":          e.Name,
		"namespace":     e.Namespace,
		"kind":          e.Kind,
		"is_default":    e.IsDefault,
		"base_branch":   e.BaseBranch,
		"source_branch": e.SourceBranch,
	}
}

// handleListEnvs lists every environment on a project, ordered by name
// (store.ListEnvironments' own ordering) — read-only, so any project member
// may call it.
func (s *server) handleListEnvs(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.requireProject(w, u, r.PathValue("project"))
	if !ok {
		return
	}
	list, err := s.st.ListEnvironments(p.ID)
	if err != nil {
		log.Printf("list environments: %v", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	out := make([]map[string]any, 0, len(list))
	for _, e := range list {
		out = append(out, environmentJSON(e))
	}
	writeJSON(w, http.StatusOK, out)
}

// handleCreateEnv creates a new standing environment on a project. name
// must be a unique DNS-1123 label within the project (400 on a duplicate or
// invalid name); base_branch is optional (drives future webhook-triggered
// deploys, unused here).
func (s *server) handleCreateEnv(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.requireProjectWrite(w, u, r.PathValue("project"))
	if !ok {
		return
	}
	var req struct {
		Name       string `json:"name"`
		BaseBranch string `json:"base_branch"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	env, err := s.st.CreateEnvironment(p.ID, req.Name, "standing", req.BaseBranch)
	if err != nil {
		var ve *store.ValidationError
		if errors.As(err, &ve) {
			writeError(w, http.StatusBadRequest, "bad_request", ve.Error())
			return
		}
		log.Printf("create environment: %v", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	writeJSON(w, http.StatusCreated, environmentJSON(env))
}

// deleteEnvironment is handleDeleteEnv's shared core: tear down the env's
// whole namespace (which reaps every app/addon kube object inside it in one
// shot — no need to walk them individually the way deleteProject does),
// then drop the now-orphaned app rows and the environment row itself.
// Guarded on s.kube exactly like deleteProject guards its own namespace
// delete: a nil kube client just skips the cluster-side teardown.
func (s *server) deleteEnvironment(ctx context.Context, env store.Environment, apps []store.App) error {
	if s.kube != nil {
		if err := s.kube.DeleteNamespace(ctx, env.Namespace); err != nil {
			log.Printf("delete environment: delete namespace %s: %v", env.Namespace, err)
		}
	}
	for _, a := range apps {
		if err := s.st.DeleteApp(a.ID); err != nil {
			return err
		}
	}
	return s.st.DeleteEnvironment(env.ID)
}

// handleDeleteEnv removes a standing environment. The project's default
// environment always refuses (409) — every legacy/env-less request resolves
// there, so deleting it would strand them. An environment with live apps
// also refuses (409) unless ?force=1, mirroring handleDeleteProject's own
// force semantics.
func (s *server) handleDeleteEnv(w http.ResponseWriter, r *http.Request, u store.User) {
	_, env, ok := s.requireEnvWrite(w, r, u, r.PathValue("project"), r.PathValue("env"))
	if !ok {
		return
	}
	if env.IsDefault {
		writeError(w, http.StatusConflict, "default_environment", "cannot delete the project's default environment")
		return
	}
	apps, err := s.st.ListAppsInEnv(env.ID)
	if err != nil {
		log.Printf("delete environment: list apps: %v", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	if len(apps) > 0 && r.URL.Query().Get("force") != "1" {
		writeError(w, http.StatusConflict, "environment_has_apps", "environment has apps; pass ?force=1 to remove anyway")
		return
	}
	if err := s.deleteEnvironment(r.Context(), env, apps); err != nil {
		log.Printf("delete environment: %v", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleSetDefaultEnv reassigns which environment legacy (env-less) routes
// and CLI calls resolve to: both the environments.is_default flag (via
// SetDefaultEnvironment, which clears any previous default in the same
// transaction) and projects.default_env (via SetDefaultEnv, requireEnv's
// actual read path) move together so the two never disagree.
func (s *server) handleSetDefaultEnv(w http.ResponseWriter, r *http.Request, u store.User) {
	p, env, ok := s.requireEnvWrite(w, r, u, r.PathValue("project"), r.PathValue("env"))
	if !ok {
		return
	}
	if err := s.st.SetDefaultEnvironment(p.ID, env.ID); err != nil {
		log.Printf("set default environment: %v", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	if err := s.st.SetDefaultEnv(p.ID, env.Name); err != nil {
		log.Printf("set default environment: %v", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	env.IsDefault = true
	writeJSON(w, http.StatusOK, environmentJSON(env))
}

// handleSetPreviewBase sets which environment new preview environments
// clone their app specs and addon data from (Task 13). The named
// environment must already exist (404 if not) — previews always need a
// live base to clone.
func (s *server) handleSetPreviewBase(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.requireProjectWrite(w, u, r.PathValue("project"))
	if !ok {
		return
	}
	var req struct {
		Env string `json:"env"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	if _, err := s.st.GetEnvironment(p.ID, req.Env); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "no such environment")
			return
		}
		log.Printf("set preview base: get environment: %v", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	if err := s.st.SetPreviewBaseEnv(p.ID, req.Env); err != nil {
		var ve *store.ValidationError
		if errors.As(err, &ve) {
			writeError(w, http.StatusBadRequest, "bad_request", ve.Error())
			return
		}
		log.Printf("set preview base: %v", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"project": p.Name, "preview_base_env": req.Env})
}
