package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"

	"github.com/sutantodadang/luncur/internal/store"
)

// errGitTokenNeedsGit is setGitToken's sentinel for "this app has no git
// source", so a token would never be used.
var errGitTokenNeedsGit = errors.New("git token requires a git-source app")

// setGitToken is the shared core of handleSetGitToken and its UI twin: seal
// the token and store it. The token is used only to clone a private repo at
// build time; it is not synced to any live workload, so there is no
// syncIfLive here.
func (s *server) setGitToken(_ context.Context, a store.App, token string) error {
	if a.Ejected {
		return errAppEjected
	}
	if a.SourceType != "git" {
		return errGitTokenNeedsGit
	}
	if s.sealer == nil {
		return errSealerUnavailable
	}
	sealed, err := s.sealer.Seal([]byte(token))
	if err != nil {
		return fmt.Errorf("seal git token: %w", err)
	}
	return s.st.SetGitToken(a.ID, sealed)
}

// clearGitToken removes an app's stored git token.
func (s *server) clearGitToken(a store.App) error {
	if a.Ejected {
		return errAppEjected
	}
	return s.st.SetGitToken(a.ID, nil)
}

// handleSetGitToken seals and stores an app's private-repo clone token. The
// token is write-only — there is no GET, matching how it's sealed at rest and
// never returned to the client.
func (s *server) handleSetGitToken(w http.ResponseWriter, r *http.Request, u store.User) {
	p, env, ok := s.requireEnvWrite(w, r, u, r.PathValue("project"), r.PathValue("env"))
	if !ok {
		return
	}
	a, ok := s.requireApp(w, p, env, r.PathValue("app"))
	if !ok {
		return
	}

	var req struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	if req.Token == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "token is required")
		return
	}

	if err := s.setGitToken(r.Context(), a, req.Token); err != nil {
		s.writeGitTokenError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleDeleteGitToken clears an app's stored git token.
func (s *server) handleDeleteGitToken(w http.ResponseWriter, r *http.Request, u store.User) {
	p, env, ok := s.requireEnvWrite(w, r, u, r.PathValue("project"), r.PathValue("env"))
	if !ok {
		return
	}
	a, ok := s.requireApp(w, p, env, r.PathValue("app"))
	if !ok {
		return
	}
	if err := s.clearGitToken(a); err != nil {
		s.writeGitTokenError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// writeGitTokenError maps the shared setGitToken/clearGitToken errors to a
// JSON envelope, matching handleSetEnv's error shape.
func (s *server) writeGitTokenError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errAppEjected):
		writeError(w, http.StatusConflict, "app_ejected", errAppEjected.Error())
	case errors.Is(err, errGitTokenNeedsGit):
		writeError(w, http.StatusBadRequest, "bad_request", errGitTokenNeedsGit.Error())
	case errors.Is(err, errSealerUnavailable):
		writeError(w, http.StatusServiceUnavailable, "sealer_unavailable", "sealer is not configured")
	default:
		log.Printf("git token: %v", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
	}
}
