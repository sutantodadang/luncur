package server

import (
	"context"
	"errors"
	"log"
	"net/http"

	"github.com/sutantodadang/luncur/internal/store"
)

// errNoDeployYet is redeploy's sentinel for "this app has never been
// deployed", so there is nothing to re-roll.
var errNoDeployYet = errors.New("app has not been deployed yet")

// redeploy re-rolls an app's current release: a git app rebuilds from its
// repo; any other app re-applies its latest image as a NEW deployment. The
// new deployment id changes the pod-template config hash (see podConfigHash),
// so the pods roll even when the image and env are unchanged — this is the
// "restart it" primitive. Shared by the JSON API and UI handlers.
func (s *server) redeploy(ctx context.Context, p store.Project, env store.Environment, a store.App, u store.User) (store.Deployment, error) {
	if a.Ejected {
		return store.Deployment{}, errAppEjected
	}
	if a.SourceType == "git" {
		return s.deployGitApp(p, env, a, u.ID)
	}
	latest, err := s.st.LatestDeployment(a.ID)
	if errors.Is(err, store.ErrNotFound) {
		return store.Deployment{}, errNoDeployYet
	}
	if err != nil {
		return store.Deployment{}, err
	}
	if latest.ImageRef == "" {
		// A build that never produced an image (e.g. a source deploy still
		// mid-build or failed) has nothing to re-apply.
		return store.Deployment{}, errNoDeployYet
	}
	d, err := s.st.CreateDeployment(a.ID, "deploying", latest.ImageRef, u.ID)
	if err != nil {
		return store.Deployment{}, err
	}
	if err := s.applyImageDeploy(ctx, p, env, a, d, latest.ImageRef); err != nil {
		return store.Deployment{}, err
	}
	return d, nil
}

// handleRedeploy re-rolls the app's current release. Git apps answer 202
// (async build); image apps answer 200 (synchronous apply), matching
// handleDeployApp's shapes.
func (s *server) handleRedeploy(w http.ResponseWriter, r *http.Request, u store.User) {
	p, env, ok := s.requireEnvWrite(w, r, u, r.PathValue("project"), r.PathValue("env"))
	if !ok {
		return
	}
	a, ok := s.requireApp(w, p, env, r.PathValue("app"))
	if !ok {
		return
	}
	if !s.requireKube(w) {
		return
	}

	d, err := s.redeploy(r.Context(), p, env, a, u)
	if err != nil {
		s.writeRedeployError(w, err)
		return
	}
	if a.SourceType == "git" {
		writeJSON(w, http.StatusAccepted, map[string]any{"deployment_id": d.ID, "seq": d.Seq, "status": "building"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"deployment_id": d.ID,
		"seq":           d.Seq,
		"status":        "live",
		"url":           s.appURLForEnv(a, env.Name, p.DefaultEnv),
	})
}

func (s *server) writeRedeployError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errAppEjected):
		writeError(w, http.StatusConflict, "app_ejected", errAppEjected.Error())
	case errors.Is(err, errNoDeployYet):
		writeError(w, http.StatusConflict, "no_deploy", errNoDeployYet.Error())
	case errors.Is(err, errKubeUnavailable):
		writeError(w, http.StatusServiceUnavailable, "kubernetes_unavailable", "kubernetes is not configured")
	case errors.Is(err, errBuildUnavailable):
		writeError(w, http.StatusServiceUnavailable, "build_unavailable", "server has no data directory configured")
	default:
		log.Printf("redeploy: %v", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
	}
}

// handleUIRedeploy is handleRedeploy's UI twin: same redeploy core, flash +
// redirect back to the app page instead of a JSON envelope.
func (s *server) handleUIRedeploy(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.uiProjectWrite(w, r, u)
	if !ok {
		return
	}
	a, ok := s.uiApp(w, r, p)
	if !ok {
		return
	}
	if s.kube == nil {
		http.Error(w, "kubernetes is not configured", http.StatusServiceUnavailable)
		return
	}
	env, err := s.st.GetEnvironmentByID(a.EnvironmentID)
	if err != nil {
		log.Printf("ui redeploy: get environment: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if _, err := s.redeploy(r.Context(), p, env, a, u); err != nil {
		switch {
		case errors.Is(err, errAppEjected):
			http.Error(w, err.Error(), http.StatusConflict)
		case errors.Is(err, errNoDeployYet):
			http.Error(w, err.Error(), http.StatusConflict)
		case errors.Is(err, errBuildUnavailable):
			http.Error(w, "server has no data directory configured", http.StatusServiceUnavailable)
		default:
			log.Printf("ui redeploy: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
		}
		return
	}
	flash(w, "ok", "redeploy started")
	uiRedirect(w, r, p, a)
}
