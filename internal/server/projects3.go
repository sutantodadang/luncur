package server

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"

	"github.com/sutantodadang/luncur/internal/store"
)

// handleSetProjectS3 stores a project's external S3 configuration; keys are
// sealed at rest. Apps opt in per app (POST .../apps/{app}/s3).
func (s *server) handleSetProjectS3(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.requireProjectWrite(w, u, r.PathValue("project"))
	if !ok {
		return
	}
	var req struct {
		Endpoint  string `json:"endpoint"`
		Region    string `json:"region"`
		Bucket    string `json:"bucket"`
		AccessKey string `json:"access_key"`
		SecretKey string `json:"secret_key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	if req.Endpoint == "" || req.Bucket == "" || req.AccessKey == "" || req.SecretKey == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "endpoint, bucket, access_key, and secret_key are required")
		return
	}
	if s.sealer == nil {
		writeError(w, http.StatusServiceUnavailable, "sealer_unavailable", "sealer is not configured")
		return
	}
	ak, err := s.sealer.Seal([]byte(req.AccessKey))
	if err == nil {
		var sk []byte
		if sk, err = s.sealer.Seal([]byte(req.SecretKey)); err == nil {
			err = s.st.SetProjectS3(store.ProjectS3{
				ProjectID: p.ID, Endpoint: req.Endpoint, Region: req.Region,
				Bucket: req.Bucket, AccessKeyEnc: ak, SecretKeyEnc: sk,
			})
		}
	}
	if err != nil {
		log.Printf("set project s3: %v", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	// Live apps that opted in pick up the change on their next sync; do it
	// opportunistically now. A project's apps may span several environments,
	// so each app's own environment (not necessarily the project's default)
	// is resolved before syncing it.
	if apps, err := s.st.ListApps(p.ID); err == nil {
		for _, a := range apps {
			if !a.InjectS3 {
				continue
			}
			if env, err := s.st.GetEnvironmentByID(a.EnvironmentID); err == nil {
				s.syncIfLive(r.Context(), p, env, a)
			} else {
				log.Printf("set project s3: get environment for %s: %v", a.Name, err)
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"endpoint": req.Endpoint, "region": req.Region, "bucket": req.Bucket,
	})
}

// handleGetProjectS3 reports the stored external S3 configuration without
// its secret key (the access key id is echoed for recognizability).
func (s *server) handleGetProjectS3(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.requireProject(w, u, r.PathValue("project"))
	if !ok {
		return
	}
	cfg, err := s.st.GetProjectS3(p.ID)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "no external S3 configured for this project")
		return
	}
	if err != nil {
		log.Printf("get project s3: %v", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	out := map[string]any{"endpoint": cfg.Endpoint, "region": cfg.Region, "bucket": cfg.Bucket}
	if s.sealer != nil {
		if ak, err := s.sealer.Open(cfg.AccessKeyEnc); err == nil {
			out["access_key"] = string(ak)
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *server) handleDeleteProjectS3(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.requireProjectWrite(w, u, r.PathValue("project"))
	if !ok {
		return
	}
	if err := s.st.DeleteProjectS3(p.ID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "no external S3 configured for this project")
			return
		}
		log.Printf("delete project s3: %v", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleAppS3Env toggles an app's LUNCUR_S3_* injection from the project's
// external S3 settings.
func (s *server) handleAppS3Env(w http.ResponseWriter, r *http.Request, u store.User) {
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
	var req struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	if err := s.st.SetInjectS3(a.ID, req.Enabled); err != nil {
		log.Printf("set inject_s3: %v", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	a.InjectS3 = req.Enabled
	s.syncIfLive(r.Context(), p, env, a)
	writeJSON(w, http.StatusOK, map[string]any{"s3_env": req.Enabled})
}
