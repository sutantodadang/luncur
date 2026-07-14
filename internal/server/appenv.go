package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"

	"github.com/sutantodadang/luncur/internal/render"
	"github.com/sutantodadang/luncur/internal/store"
)

// errSealerUnavailable is setAppEnv's sentinel for "no sealer is
// configured". Callers (the JSON API and the UI) each translate it into
// their own response shape.
var errSealerUnavailable = errors.New("sealer is not configured")

// setAppEnv is the shared core of handleSetEnv and its UI twin
// (handleUIEnvSet): seal the value, upsert it, then opportunistically sync
// if the app is live. A *store.ValidationError means the key/value were
// rejected by the store; any other error is an internal failure.
func (s *server) setAppEnv(ctx context.Context, p store.Project, env store.Environment, a store.App, key, value string) error {
	if a.Ejected {
		return errAppEjected
	}
	if s.sealer == nil {
		return errSealerUnavailable
	}

	sealed, err := s.sealer.Seal([]byte(value))
	if err != nil {
		return fmt.Errorf("seal env %q: %w", key, err)
	}

	if err := s.st.SetEnv(a.ID, key, sealed); err != nil {
		return err
	}

	s.syncIfLive(ctx, p, env, a)
	return nil
}

// setAppEnvBulk upserts many env vars in one pass: parse errors and the
// ejected/sealer guards reject the whole batch up front, then vars are
// sealed and stored one by one (store validation can still fail a key
// mid-batch — earlier keys stay set, same as re-pasting would) and the
// app is synced once at the end instead of per key.
func (s *server) setAppEnvBulk(ctx context.Context, p store.Project, env store.Environment, a store.App, vars map[string]string) error {
	if a.Ejected {
		return errAppEjected
	}
	if s.sealer == nil {
		return errSealerUnavailable
	}
	keys := make([]string, 0, len(vars))
	for k := range vars {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		sealed, err := s.sealer.Seal([]byte(vars[k]))
		if err != nil {
			return fmt.Errorf("seal env %q: %w", k, err)
		}
		if err := s.st.SetEnv(a.ID, k, sealed); err != nil {
			return err
		}
	}
	s.syncIfLive(ctx, p, env, a)
	return nil
}

// unsetAppEnv is the shared core of handleUnsetEnv and its UI twin
// (handleUIEnvUnset): delete the var, then opportunistically sync if the
// app is live. Returns store.ErrNotFound when the key doesn't exist.
func (s *server) unsetAppEnv(ctx context.Context, p store.Project, env store.Environment, a store.App, key string) error {
	if a.Ejected {
		return errAppEjected
	}
	if err := s.st.UnsetEnv(a.ID, key); err != nil {
		return err
	}
	s.syncIfLive(ctx, p, env, a)
	return nil
}

// handleGetEnv returns an app's env vars, unsealed to plaintext.
func (s *server) handleGetEnv(w http.ResponseWriter, r *http.Request, u store.User) {
	p, env, ok := s.requireEnv(w, r, u, r.PathValue("project"), r.PathValue("env"))
	if !ok {
		return
	}
	a, ok := s.requireApp(w, p, env, r.PathValue("app"))
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

	plain := make(map[string]string, len(sealed))
	for k, v := range sealed {
		val, err := s.sealer.Open(v)
		if err != nil {
			log.Printf("unseal env %q: %v", k, err)
			writeError(w, http.StatusInternalServerError, "internal", "internal error")
			return
		}
		plain[k] = string(val)
	}
	writeJSON(w, http.StatusOK, plain)
}

// handleSetEnv seals and upserts one env var, then opportunistically syncs.
func (s *server) handleSetEnv(w http.ResponseWriter, r *http.Request, u store.User) {
	p, env, ok := s.requireEnvWrite(w, r, u, r.PathValue("project"), r.PathValue("env"))
	if !ok {
		return
	}
	a, ok := s.requireApp(w, p, env, r.PathValue("app"))
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

	if err := s.setAppEnv(r.Context(), p, env, a, req.Key, req.Value); err != nil {
		var ve *store.ValidationError
		switch {
		case errors.Is(err, errAppEjected):
			writeError(w, http.StatusConflict, "app_ejected", errAppEjected.Error())
		case errors.Is(err, errSealerUnavailable):
			writeError(w, http.StatusServiceUnavailable, "sealer_unavailable", "sealer is not configured")
		case errors.As(err, &ve):
			writeError(w, http.StatusBadRequest, "bad_request", ve.Error())
		default:
			log.Printf("set env: %v", err)
			writeError(w, http.StatusInternalServerError, "internal", "internal error")
		}
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// handleBulkSetEnv accepts raw .env text and upserts every pair in it.
func (s *server) handleBulkSetEnv(w http.ResponseWriter, r *http.Request, u store.User) {
	p, env, ok := s.requireEnvWrite(w, r, u, r.PathValue("project"), r.PathValue("env"))
	if !ok {
		return
	}
	a, ok := s.requireApp(w, p, env, r.PathValue("app"))
	if !ok {
		return
	}

	var req struct {
		Dotenv string `json:"dotenv"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}

	vars, err := parseDotenv(req.Dotenv)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_dotenv", err.Error())
		return
	}
	if len(vars) == 0 {
		writeError(w, http.StatusBadRequest, "bad_dotenv", "no KEY=VALUE pairs found")
		return
	}

	if err := s.setAppEnvBulk(r.Context(), p, env, a, vars); err != nil {
		var ve *store.ValidationError
		switch {
		case errors.Is(err, errAppEjected):
			writeError(w, http.StatusConflict, "app_ejected", errAppEjected.Error())
		case errors.Is(err, errSealerUnavailable):
			writeError(w, http.StatusServiceUnavailable, "sealer_unavailable", "sealer is not configured")
		case errors.As(err, &ve):
			writeError(w, http.StatusBadRequest, "bad_request", ve.Error())
		default:
			log.Printf("bulk set env: %v", err)
			writeError(w, http.StatusInternalServerError, "internal", "internal error")
		}
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"set": len(vars)})
}

// handleUnsetEnv deletes one env var, then opportunistically syncs.
func (s *server) handleUnsetEnv(w http.ResponseWriter, r *http.Request, u store.User) {
	p, env, ok := s.requireEnvWrite(w, r, u, r.PathValue("project"), r.PathValue("env"))
	if !ok {
		return
	}
	a, ok := s.requireApp(w, p, env, r.PathValue("app"))
	if !ok {
		return
	}

	key := r.PathValue("key")
	if err := s.unsetAppEnv(r.Context(), p, env, a, key); err != nil {
		switch {
		case errors.Is(err, errAppEjected):
			writeError(w, http.StatusConflict, "app_ejected", errAppEjected.Error())
		case errors.Is(err, store.ErrNotFound):
			writeError(w, http.StatusNotFound, "not_found", "no such env var")
		default:
			log.Printf("unset env: %v", err)
			writeError(w, http.StatusInternalServerError, "internal", "internal error")
		}
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// setOverride is the shared core of handleSetOverride and the UI YAML
// editor: store the patch, then opportunistically sync if the app is live.
// A *store.ValidationError means the patch was rejected by the store; any
// other error is an internal failure.
func (s *server) setOverride(ctx context.Context, p store.Project, env store.Environment, a store.App, kind, patch string) error {
	if a.Ejected {
		return errAppEjected
	}
	if err := s.st.SetOverride(a.ID, kind, patch); err != nil {
		return err
	}
	s.syncIfLive(ctx, p, env, a)
	return nil
}

// handleSetOverride stores a raw strategic-merge-patch for one manifest
// kind, then opportunistically syncs. The request body IS the patch.
func (s *server) handleSetOverride(w http.ResponseWriter, r *http.Request, u store.User) {
	p, env, ok := s.requireEnvWrite(w, r, u, r.PathValue("project"), r.PathValue("env"))
	if !ok {
		return
	}
	a, ok := s.requireApp(w, p, env, r.PathValue("app"))
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

	if err := s.setOverride(r.Context(), p, env, a, kind, string(body)); err != nil {
		var ve *store.ValidationError
		switch {
		case errors.Is(err, errAppEjected):
			writeError(w, http.StatusConflict, "app_ejected", errAppEjected.Error())
		case errors.As(err, &ve):
			writeError(w, http.StatusBadRequest, "bad_request", ve.Error())
		default:
			log.Printf("set override: %v", err)
			writeError(w, http.StatusInternalServerError, "internal", "internal error")
		}
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// handleDeleteOverride removes one manifest kind's override, then
// opportunistically syncs.
func (s *server) handleDeleteOverride(w http.ResponseWriter, r *http.Request, u store.User) {
	p, env, ok := s.requireEnvWrite(w, r, u, r.PathValue("project"), r.PathValue("env"))
	if !ok {
		return
	}
	a, ok := s.requireApp(w, p, env, r.PathValue("app"))
	if !ok {
		return
	}
	if s.refuseEjected(w, a) {
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

	s.syncIfLive(r.Context(), p, env, a)
	w.WriteHeader(http.StatusNoContent)
}

// appImage returns the image to render an app with: its latest deployment's
// image, or a placeholder if it has never been deployed.
func (s *server) appImage(a store.App) (string, error) {
	d, err := s.st.LatestDeployment(a.ID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return "<pending-first-deploy>", nil
	case err != nil:
		return "", err
	default:
		return d.ImageRef, nil
	}
}

// handleRawManifest renders the app's current manifest set as multi-doc
// YAML. Uses the latest deployment's image, or a placeholder if the app
// has never been deployed. ?base=1 renders without overrides applied.
func (s *server) handleRawManifest(w http.ResponseWriter, r *http.Request, u store.User) {
	p, env, ok := s.requireEnv(w, r, u, r.PathValue("project"), r.PathValue("env"))
	if !ok {
		return
	}
	a, ok := s.requireApp(w, p, env, r.PathValue("app"))
	if !ok {
		return
	}

	image, err := s.appImage(a)
	if err != nil {
		log.Printf("latest deployment: %v", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}

	withOverrides := r.URL.Query().Get("base") != "1"
	rendered, err := s.renderApp(p, env, a, image, withOverrides)
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
