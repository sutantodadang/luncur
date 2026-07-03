package server

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"

	"github.com/sutantodadang/luncur/internal/render"
	"github.com/sutantodadang/luncur/internal/store"
)

// handleGetEnv returns an app's env vars, unsealed to plaintext.
func (s *server) handleGetEnv(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.requireProject(w, u, r.PathValue("project"))
	if !ok {
		return
	}
	a, ok := s.requireApp(w, p, r.PathValue("app"))
	if !ok {
		return
	}

	sealed, err := s.st.ListEnv(a.ID)
	if err != nil {
		log.Printf("list env: %v", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	if len(sealed) > 0 && s.sealer == nil {
		writeError(w, http.StatusServiceUnavailable, "sealer_unavailable", "sealer is not configured")
		return
	}

	env := make(map[string]string, len(sealed))
	for k, v := range sealed {
		plain, err := s.sealer.Open(v)
		if err != nil {
			log.Printf("unseal env %q: %v", k, err)
			writeError(w, http.StatusInternalServerError, "internal", "internal error")
			return
		}
		env[k] = string(plain)
	}
	writeJSON(w, http.StatusOK, env)
}

// handleSetEnv seals and upserts one env var, then opportunistically syncs.
func (s *server) handleSetEnv(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.requireProject(w, u, r.PathValue("project"))
	if !ok {
		return
	}
	a, ok := s.requireApp(w, p, r.PathValue("app"))
	if !ok {
		return
	}

	var req struct {
		Key   string `json:"key"`
		Value string `json:"value"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}

	if s.sealer == nil {
		writeError(w, http.StatusServiceUnavailable, "sealer_unavailable", "sealer is not configured")
		return
	}

	sealed, err := s.sealer.Seal([]byte(req.Value))
	if err != nil {
		log.Printf("seal env %q: %v", req.Key, err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}

	if err := s.st.SetEnv(a.ID, req.Key, sealed); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}

	s.syncIfLive(r.Context(), p, a)
	w.WriteHeader(http.StatusNoContent)
}

// handleUnsetEnv deletes one env var, then opportunistically syncs.
func (s *server) handleUnsetEnv(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.requireProject(w, u, r.PathValue("project"))
	if !ok {
		return
	}
	a, ok := s.requireApp(w, p, r.PathValue("app"))
	if !ok {
		return
	}

	key := r.PathValue("key")
	if err := s.st.UnsetEnv(a.ID, key); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "no such env var")
			return
		}
		log.Printf("unset env: %v", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}

	s.syncIfLive(r.Context(), p, a)
	w.WriteHeader(http.StatusNoContent)
}

// handleSetOverride stores a raw strategic-merge-patch for one manifest
// kind, then opportunistically syncs. The request body IS the patch.
func (s *server) handleSetOverride(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.requireProject(w, u, r.PathValue("project"))
	if !ok {
		return
	}
	a, ok := s.requireApp(w, p, r.PathValue("app"))
	if !ok {
		return
	}

	kind := r.PathValue("kind")
	body, err := io.ReadAll(r.Body)
	if err != nil {
		log.Printf("read override body: %v", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}

	if err := s.st.SetOverride(a.ID, kind, string(body)); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}

	s.syncIfLive(r.Context(), p, a)
	w.WriteHeader(http.StatusNoContent)
}

// handleDeleteOverride removes one manifest kind's override, then
// opportunistically syncs.
func (s *server) handleDeleteOverride(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.requireProject(w, u, r.PathValue("project"))
	if !ok {
		return
	}
	a, ok := s.requireApp(w, p, r.PathValue("app"))
	if !ok {
		return
	}

	kind := r.PathValue("kind")
	if err := s.st.DeleteOverride(a.ID, kind); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "no such override")
			return
		}
		log.Printf("delete override: %v", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}

	s.syncIfLive(r.Context(), p, a)
	w.WriteHeader(http.StatusNoContent)
}

// handleRawManifest renders the app's current manifest set as multi-doc
// YAML. Uses the latest deployment's image, or a placeholder if the app
// has never been deployed. ?base=1 renders without overrides applied.
func (s *server) handleRawManifest(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.requireProject(w, u, r.PathValue("project"))
	if !ok {
		return
	}
	a, ok := s.requireApp(w, p, r.PathValue("app"))
	if !ok {
		return
	}

	image := "<pending-first-deploy>"
	d, err := s.st.LatestDeployment(a.ID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		// keep placeholder
	case err != nil:
		log.Printf("latest deployment: %v", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	default:
		image = d.ImageRef
	}

	withOverrides := r.URL.Query().Get("base") != "1"
	rendered, err := s.renderApp(p, a, image, withOverrides)
	if err != nil {
		log.Printf("render app: %v", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}

	yamlBytes, err := render.YAML(rendered)
	if err != nil {
		log.Printf("render yaml: %v", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}

	w.Header().Set("Content-Type", "text/yaml")
	w.WriteHeader(http.StatusOK)
	w.Write(yamlBytes)
}
