package server

import (
	"errors"
	"log"
	"net/http"

	"github.com/sutantodadang/luncur/internal/store"
)

func (s *server) handleUIEnvSet(w http.ResponseWriter, r *http.Request, u store.User) {
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

	env, ok := s.uiAppEnv(w, a)
	if !ok {
		return
	}
	if err := s.setAppEnv(r.Context(), p, env, a, r.PostFormValue("key"), r.PostFormValue("value")); err != nil {
		var ve *store.ValidationError
		switch {
		case errors.Is(err, errAppEjected):
			http.Error(w, err.Error(), http.StatusConflict)
		case errors.Is(err, errSealerUnavailable):
			http.Error(w, "sealer is not configured", http.StatusServiceUnavailable)
		case errors.As(err, &ve):
			http.Error(w, ve.Error(), http.StatusBadRequest)
		default:
			log.Printf("ui set env: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
		}
		return
	}
	flash(w, "ok", "env saved")
	uiRedirect(w, r, p, a, tabWire)
}

// handleUIEnvBulk is handleBulkSetEnv's UI twin: paste-in-only bulk upsert
// from a raw .env textarea, redirecting back to the app page on success.
func (s *server) handleUIEnvBulk(w http.ResponseWriter, r *http.Request, u store.User) {
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

	vars, err := parseDotenv(r.PostFormValue("dotenv"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if len(vars) == 0 {
		http.Error(w, "no KEY=VALUE pairs found", http.StatusBadRequest)
		return
	}

	env, ok := s.uiAppEnv(w, a)
	if !ok {
		return
	}
	if err := s.setAppEnvBulk(r.Context(), p, env, a, vars); err != nil {
		var ve *store.ValidationError
		switch {
		case errors.Is(err, errAppEjected):
			http.Error(w, err.Error(), http.StatusConflict)
		case errors.Is(err, errSealerUnavailable):
			http.Error(w, "sealer is not configured", http.StatusServiceUnavailable)
		case errors.As(err, &ve):
			http.Error(w, ve.Error(), http.StatusBadRequest)
		default:
			log.Printf("ui bulk set env: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
		}
		return
	}
	flash(w, "ok", "env vars saved")
	uiRedirect(w, r, p, a, tabWire)
}

func (s *server) handleUIEnvUnset(w http.ResponseWriter, r *http.Request, u store.User) {
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

	env, ok := s.uiAppEnv(w, a)
	if !ok {
		return
	}
	if err := s.unsetAppEnv(r.Context(), p, env, a, r.PostFormValue("key")); err != nil {
		switch {
		case errors.Is(err, errAppEjected):
			http.Error(w, err.Error(), http.StatusConflict)
		case errors.Is(err, store.ErrNotFound):
			http.Error(w, "no such env var", http.StatusNotFound)
		default:
			log.Printf("ui unset env: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
		}
		return
	}
	flash(w, "ok", "env var removed")
	uiRedirect(w, r, p, a, tabWire)
}
