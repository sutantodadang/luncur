package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"

	"github.com/sutantodadang/luncur/internal/render"
	"github.com/sutantodadang/luncur/internal/store"
)

// volumeWarning rides along on every successful volume add so callers learn
// the two operational consequences up front.
const volumeWarning = "deploys briefly stop the app (Recreate strategy); volume data is not included in luncur backup"

// volumeReplicaConflictError wraps the RWO constraint violation: an app
// can't combine a volume with more than one replica (node-local RWO storage
// can only attach to one pod). Callers map it to 409 volume_replica_conflict.
type volumeReplicaConflictError struct{ err error }

func (e *volumeReplicaConflictError) Error() string { return e.err.Error() }
func (e *volumeReplicaConflictError) Unwrap() error { return e.err }

func volumeJSON(v store.Volume) map[string]any {
	return map[string]any{
		"id": v.ID, "name": v.Name, "path": v.Path, "size_gb": v.SizeGB,
	}
}

// addVolume is the shared core of handleAddVolume and its UI twin: gate
// (ejected, cron kind, replica conflict), persist, opportunistically sync.
func (s *server) addVolume(ctx context.Context, p store.Project, a store.App, name, path string, sizeGB int) (store.Volume, error) {
	if a.Ejected {
		return store.Volume{}, errAppEjected
	}
	if a.Kind == "cron" {
		return store.Volume{}, &kindMismatchError{fmt.Errorf("volumes are not supported for cron apps")}
	}
	if a.Replicas > 1 {
		return store.Volume{}, &volumeReplicaConflictError{fmt.Errorf("app runs %d replicas; a volume is RWO node-local storage, max 1 replica — scale to 1 first", a.Replicas)}
	}
	v, err := s.st.AddVolume(a.ID, name, path, sizeGB)
	if err != nil {
		return store.Volume{}, err
	}
	s.syncIfLive(ctx, p, a)
	return v, nil
}

func (s *server) handleAddVolume(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.requireProjectWrite(w, u, r.PathValue("project"))
	if !ok {
		return
	}
	a, ok := s.requireApp(w, p, r.PathValue("app"))
	if !ok {
		return
	}

	var req struct {
		Name   string `json:"name"`
		Path   string `json:"path"`
		SizeGB int    `json:"size_gb"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}

	v, err := s.addVolume(r.Context(), p, a, req.Name, req.Path, req.SizeGB)
	if err != nil {
		var ke *kindMismatchError
		var rc *volumeReplicaConflictError
		var ve *store.ValidationError
		switch {
		case errors.Is(err, errAppEjected):
			writeError(w, http.StatusConflict, "app_ejected", errAppEjected.Error())
		case errors.As(err, &ke):
			writeError(w, http.StatusBadRequest, "kind_mismatch", ke.Error())
		case errors.As(err, &rc):
			writeError(w, http.StatusConflict, "volume_replica_conflict", rc.Error())
		case errors.As(err, &ve):
			writeError(w, http.StatusBadRequest, "bad_request", ve.Error())
		default:
			log.Printf("add volume: %v", err)
			writeError(w, http.StatusInternalServerError, "internal", "internal error")
		}
		return
	}

	out := volumeJSON(v)
	out["warning"] = volumeWarning
	writeJSON(w, http.StatusCreated, out)
}

func (s *server) handleListVolumes(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.requireProject(w, u, r.PathValue("project"))
	if !ok {
		return
	}
	a, ok := s.requireApp(w, p, r.PathValue("app"))
	if !ok {
		return
	}
	list, err := s.st.ListVolumes(a.ID)
	if err != nil {
		log.Printf("list volumes: %v", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	out := make([]map[string]any, 0, len(list))
	for _, v := range list {
		out = append(out, volumeJSON(v))
	}
	writeJSON(w, http.StatusOK, map[string]any{"volumes": out})
}

// removeVolume is the shared core of handleDeleteVolume and its UI twin:
// delete the DB row and, only with purge, the cluster PVC. A purge with no
// kube client fails with errKubeUnavailable BEFORE the row is touched so the
// volume never silently detaches while its data lingers unaddressed.
func (s *server) removeVolume(ctx context.Context, p store.Project, a store.App, name string, purge bool) error {
	if a.Ejected {
		return errAppEjected
	}
	if purge && s.kube == nil {
		return errKubeUnavailable
	}
	if err := s.st.DeleteVolume(a.ID, name); err != nil {
		return err
	}
	if purge {
		if err := s.kube.DeletePVC(ctx, p.Namespace, render.VolumeClaimName(a.Name, name)); err != nil {
			return err
		}
	}
	s.syncIfLive(ctx, p, a)
	return nil
}

func (s *server) handleDeleteVolume(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.requireProjectWrite(w, u, r.PathValue("project"))
	if !ok {
		return
	}
	a, ok := s.requireApp(w, p, r.PathValue("app"))
	if !ok {
		return
	}

	name := r.PathValue("name")
	q := r.URL.Query().Get("purge")
	purge := q == "true" || q == "1"

	if err := s.removeVolume(r.Context(), p, a, name, purge); err != nil {
		switch {
		case errors.Is(err, errAppEjected):
			writeError(w, http.StatusConflict, "app_ejected", errAppEjected.Error())
		case errors.Is(err, errKubeUnavailable):
			writeError(w, http.StatusServiceUnavailable, "kubernetes_unavailable", "kubernetes is not configured")
		case errors.Is(err, store.ErrNotFound):
			writeError(w, http.StatusNotFound, "not_found", "no such volume")
		default:
			log.Printf("remove volume: %v", err)
			writeError(w, http.StatusInternalServerError, "internal", "internal error")
		}
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"removed": name, "purged": purge})
}
