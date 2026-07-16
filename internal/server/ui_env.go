package server

import (
	"errors"
	"log"
	"net/http"
	"strings"

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
	uiRedirect(w, r, p, a)
}

// handleUIGitTokenSet is handleSetGitToken's UI twin: seal+store a
// private-repo clone token from the app page, redirecting back on success.
func (s *server) handleUIGitTokenSet(w http.ResponseWriter, r *http.Request, u store.User) {
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
	token := strings.TrimSpace(r.PostFormValue("token"))
	if token == "" {
		http.Error(w, "token is required", http.StatusBadRequest)
		return
	}
	if err := s.setGitToken(r.Context(), a, token); err != nil {
		s.uiGitTokenError(w, err)
		return
	}
	flash(w, "ok", "git token saved")
	uiRedirect(w, r, p, a)
}

// handleUIGitTokenClear is handleDeleteGitToken's UI twin.
func (s *server) handleUIGitTokenClear(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.uiProjectWrite(w, r, u)
	if !ok {
		return
	}
	a, ok := s.uiApp(w, r, p)
	if !ok {
		return
	}
	if err := s.clearGitToken(a); err != nil {
		s.uiGitTokenError(w, err)
		return
	}
	flash(w, "ok", "git token cleared")
	uiRedirect(w, r, p, a)
}

func (s *server) uiGitTokenError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errAppEjected):
		http.Error(w, err.Error(), http.StatusConflict)
	case errors.Is(err, errGitTokenNeedsGit):
		http.Error(w, err.Error(), http.StatusBadRequest)
	case errors.Is(err, errSealerUnavailable):
		http.Error(w, "sealer is not configured", http.StatusServiceUnavailable)
	default:
		log.Printf("ui git token: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
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
	uiRedirect(w, r, p, a)
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
	uiRedirect(w, r, p, a)
}
