package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"

	"github.com/sutantodadang/luncur/internal/store"
)

var errNoRollbackTarget = errors.New("no previous live deployment to roll back to")

// errImageMissing indicates the target image is no longer present in the
// embedded registry; it carries the ref so both handlers can name the image
// in their response.
type errImageMissing struct{ ref string }

func (e *errImageMissing) Error() string {
	return fmt.Sprintf("image %s is no longer in the registry", e.ref)
}

// errRegistryCheck wraps a failure while probing the embedded registry so
// callers can tell it apart from other internal errors and answer 502/503
// instead of 500.
type errRegistryCheck struct{ err error }

func (e *errRegistryCheck) Error() string { return e.err.Error() }
func (e *errRegistryCheck) Unwrap() error { return e.err }

// rollbackTarget picks the deployment to roll back to: an explicit id (must
// be this app's, with an image), or the newest live deployment older than
// the latest one.
func (s *server) rollbackTarget(a store.App, deployID string) (store.Deployment, error) {
	if deployID != "" {
		d, err := s.st.GetDeployment(deployID)
		if err != nil || d.AppID != a.ID {
			return store.Deployment{}, store.ErrNotFound
		}
		if d.ImageRef == "" {
			return store.Deployment{}, errNoRollbackTarget
		}
		return d, nil
	}
	latest, err := s.st.LatestDeployment(a.ID)
	if err != nil {
		return store.Deployment{}, errNoRollbackTarget
	}
	history, err := s.st.ListDeployments(a.ID)
	if err != nil {
		return store.Deployment{}, err
	}
	// history is newest-first (see ListDeployments), so skipping the row
	// that IS latest and taking the first remaining live one is
	// "the newest live deployment older than the latest" — ids are opaque
	// nanoids now, with no ordering of their own, so this can no longer
	// compare them with `<` the way sequential integer ids allowed.
	for _, d := range history {
		if d.ID != latest.ID && d.Status == "live" && d.ImageRef != "" {
			return d, nil
		}
	}
	return store.Deployment{}, errNoRollbackTarget
}

// imageInRegistry HEAD-checks the embedded registry for ref's manifest.
// External refs (different host prefix) are assumed present — luncur has no
// credentials to verify them.
func (s *server) imageInRegistry(ctx context.Context, ref string) (bool, error) {
	rest, ok := strings.CutPrefix(ref, s.registryHost+"/")
	if !ok {
		return true, nil
	}
	name, tag, ok := strings.Cut(rest, ":")
	if !ok {
		return true, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodHead,
		"http://"+s.registryHost+"/v2/"+name+"/manifests/"+tag, nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("Accept",
		"application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.v2+json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false, err
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK, nil
}

// rollback is the shared core for the JSON API and UI rollback handlers:
// pick the target deployment, verify its image is still in the registry,
// record the new deployment row, and apply it. It does not check that kube
// is configured — callers do that first, since applying would otherwise
// panic on a nil client. Returned errors are sentinels/wrapped types
// (store.ErrNotFound, errNoRollbackTarget, *errImageMissing,
// *errRegistryCheck) that each caller maps to its own response shape; any
// other error is an unexpected internal failure.
func (s *server) rollback(ctx context.Context, p store.Project, a store.App, u store.User, deployID string) (store.Deployment, error) {
	if a.Ejected {
		return store.Deployment{}, errAppEjected
	}
	target, err := s.rollbackTarget(a, deployID)
	if err != nil {
		return store.Deployment{}, err
	}

	present, err := s.imageInRegistry(ctx, target.ImageRef)
	if err != nil {
		return store.Deployment{}, &errRegistryCheck{err}
	}
	if !present {
		return store.Deployment{}, &errImageMissing{ref: target.ImageRef}
	}

	d, err := s.st.CreateRollbackDeployment(a.ID, target.ImageRef, u.ID, target.ID)
	if err != nil {
		return store.Deployment{}, err
	}
	if err := s.applyImageDeploy(ctx, p, a, d, target.ImageRef); err != nil {
		return store.Deployment{}, err
	}
	return d, nil
}

func (s *server) handleRollback(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.requireProject(w, u, r.PathValue("project"))
	if !ok {
		return
	}
	a, ok := s.requireApp(w, p, r.PathValue("app"))
	if !ok {
		return
	}
	if !s.requireKube(w) {
		return
	}
	var req struct {
		DeployID string `json:"deploy_id"`
	}
	if r.Body != nil {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil && err.Error() != "EOF" {
			writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
			return
		}
	}
	if req.DeployID != "" && !validDeployID(req.DeployID) {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid deploy id")
		return
	}

	d, err := s.rollback(r.Context(), p, a, u, req.DeployID)
	if err != nil {
		var missing *errImageMissing
		var regErr *errRegistryCheck
		switch {
		case errors.Is(err, errAppEjected):
			writeError(w, http.StatusConflict, "app_ejected", errAppEjected.Error())
		case errors.Is(err, store.ErrNotFound):
			writeError(w, http.StatusNotFound, "not_found", "no such deployment for this app")
		case errors.Is(err, errNoRollbackTarget):
			writeError(w, http.StatusConflict, "no_target", errNoRollbackTarget.Error())
		case errors.As(err, &missing):
			writeError(w, http.StatusConflict, "image_missing", missing.Error())
		case errors.As(err, &regErr):
			writeError(w, http.StatusBadGateway, "registry_error", "could not verify image in registry")
		default:
			log.Printf("rollback: %v", err)
			writeError(w, http.StatusInternalServerError, "internal", "internal error")
		}
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"deployment_id": d.ID, "seq": d.Seq, "status": "live"})
}
