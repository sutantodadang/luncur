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

// rollbackTarget picks the deployment to roll back to: an explicit id (must
// be this app's, with an image), or the newest live deployment older than
// the latest one.
func (s *server) rollbackTarget(a store.App, deployID int64) (store.Deployment, error) {
	if deployID != 0 {
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
	for _, d := range history { // newest first
		if d.ID < latest.ID && d.Status == "live" && d.ImageRef != "" {
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
		DeployID int64 `json:"deploy_id"`
	}
	if r.Body != nil {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil && err.Error() != "EOF" {
			writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
			return
		}
	}

	target, err := s.rollbackTarget(a, req.DeployID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", "no such deployment for this app")
		return
	case errors.Is(err, errNoRollbackTarget):
		writeError(w, http.StatusConflict, "no_target", errNoRollbackTarget.Error())
		return
	case err != nil:
		log.Printf("rollback target: %v", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}

	present, err := s.imageInRegistry(r.Context(), target.ImageRef)
	if err != nil {
		log.Printf("registry check: %v", err)
		writeError(w, http.StatusBadGateway, "registry_error", "could not verify image in registry")
		return
	}
	if !present {
		writeError(w, http.StatusConflict, "image_missing",
			fmt.Sprintf("image %s is no longer in the registry", target.ImageRef))
		return
	}

	d, err := s.st.CreateRollbackDeployment(a.ID, target.ImageRef, u.ID, target.ID)
	if err != nil {
		log.Printf("create rollback deployment: %v", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	if err := s.applyImageDeploy(r.Context(), p, a, d, target.ImageRef); err != nil {
		log.Printf("rollback apply: %v", err)
		writeError(w, http.StatusInternalServerError, "internal", "rollback apply failed")
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"deployment_id": d.ID, "status": "live"})
}
