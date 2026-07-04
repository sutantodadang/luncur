package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/sutantodadang/luncur/internal/store"
)

// setAppHealth is the shared core of handleSetHealth: validate + persist the
// health check path, then opportunistically sync if the app is live. Mirrors
// setAppEnv's gate/persist/sync pattern. A validation failure from
// SetHealthPath is surfaced as-is (mapped to bad_request by the caller).
func (s *server) setAppHealth(ctx context.Context, p store.Project, a store.App, path string) error {
	if a.Ejected {
		return errAppEjected
	}
	if err := s.st.SetHealthPath(a.ID, path); err != nil {
		return err
	}
	a.HealthPath = path
	s.syncIfLive(ctx, p, a)
	return nil
}

// handleSetHealth sets (or, with an empty path, clears) the app's HTTP
// health check path, then opportunistically syncs.
func (s *server) handleSetHealth(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.requireProject(w, u, r.PathValue("project"))
	if !ok {
		return
	}
	a, ok := s.requireApp(w, p, r.PathValue("app"))
	if !ok {
		return
	}

	var req struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}

	if err := s.setAppHealth(r.Context(), p, a, req.Path); err != nil {
		switch {
		case errors.Is(err, errAppEjected):
			writeError(w, http.StatusConflict, "app_ejected", errAppEjected.Error())
		default:
			writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		}
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"health_path": req.Path})
}
