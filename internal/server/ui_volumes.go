package server

import (
	"errors"
	"log"
	"net/http"
	"strconv"

	"github.com/sutantodadang/luncur/internal/store"
)

// handleUIVolumeAdd is handleAddVolume's UI twin: same shared addVolume
// core, redirect instead of JSON.
func (s *server) handleUIVolumeAdd(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.uiProjectWrite(w, r, u)
	if !ok {
		return
	}
	a, ok := s.uiApp(w, r, p)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	size, err := strconv.Atoi(r.PostFormValue("size_gb"))
	if err != nil {
		http.Error(w, "invalid size", http.StatusBadRequest)
		return
	}

	env, ok := s.uiAppEnv(w, a)
	if !ok {
		return
	}
	if _, err := s.addVolume(r.Context(), p, env, a, r.PostFormValue("name"), r.PostFormValue("path"), size); err != nil {
		var rc *volumeReplicaConflictError
		var ke *kindMismatchError
		var ve *store.ValidationError
		switch {
		case errors.Is(err, errAppEjected):
			http.Error(w, err.Error(), http.StatusConflict)
		case errors.As(err, &rc):
			http.Error(w, rc.Error(), http.StatusConflict)
		case errors.As(err, &ke):
			http.Error(w, ke.Error(), http.StatusBadRequest)
		case errors.As(err, &ve):
			http.Error(w, ve.Error(), http.StatusBadRequest)
		default:
			log.Printf("ui add volume: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
		}
		return
	}
	flash(w, "ok", "volume added")
	uiRedirect(w, r, p, a)
}

// handleUIVolumeRemove is handleDeleteVolume's UI twin: same shared
// removeVolume core (purge via checkbox), redirect instead of JSON.
func (s *server) handleUIVolumeRemove(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.uiProjectWrite(w, r, u)
	if !ok {
		return
	}
	a, ok := s.uiApp(w, r, p)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}

	purge := r.PostFormValue("purge") != ""
	env, ok := s.uiAppEnv(w, a)
	if !ok {
		return
	}
	if err := s.removeVolume(r.Context(), p, env, a, r.PostFormValue("name"), purge); err != nil {
		switch {
		case errors.Is(err, errAppEjected):
			http.Error(w, err.Error(), http.StatusConflict)
		case errors.Is(err, errKubeUnavailable):
			http.Error(w, "kubernetes is not configured", http.StatusServiceUnavailable)
		case errors.Is(err, store.ErrNotFound):
			http.Error(w, "no such volume", http.StatusNotFound)
		default:
			log.Printf("ui remove volume: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
		}
		return
	}
	flash(w, "ok", "volume removed")
	uiRedirect(w, r, p, a)
}
