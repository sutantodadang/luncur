package server

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"

	"github.com/sutantodadang/luncur/internal/store"
)

func (s *server) appJSON(a store.App) map[string]any {
	return map[string]any{
		"id":       a.ID,
		"name":     a.Name,
		"port":     a.Port,
		"replicas": a.Replicas,
		"url":      "http://" + hostFor(a.Name, s.externalIP),
	}
}

// requireApp loads an app within a project by name. Writes the error
// response and returns ok=false on failure.
func (s *server) requireApp(w http.ResponseWriter, p store.Project, name string) (store.App, bool) {
	a, err := s.st.GetApp(p.ID, name)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "no such app")
		return store.App{}, false
	}
	if err != nil {
		log.Printf("get app: %v", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return store.App{}, false
	}
	return a, true
}

func (s *server) handleCreateApp(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.requireProject(w, u, r.PathValue("project"))
	if !ok {
		return
	}
	var req struct {
		Name string `json:"name"`
		Port int    `json:"port"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	a, err := s.st.CreateApp(p.ID, req.Name, req.Port)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			writeError(w, http.StatusConflict, "app_exists", "app already exists")
			return
		}
		if strings.HasPrefix(err.Error(), "insert app:") {
			log.Printf("create app: %v", err)
			writeError(w, http.StatusInternalServerError, "internal", "internal error")
			return
		}
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, s.appJSON(a))
}

func (s *server) handleListApps(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.requireProject(w, u, r.PathValue("project"))
	if !ok {
		return
	}
	list, err := s.st.ListApps(p.ID)
	if err != nil {
		log.Printf("list apps: %v", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	out := make([]map[string]any, 0, len(list))
	for _, a := range list {
		out = append(out, s.appJSON(a))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *server) handleGetApp(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.requireProject(w, u, r.PathValue("project"))
	if !ok {
		return
	}
	a, ok := s.requireApp(w, p, r.PathValue("app"))
	if !ok {
		return
	}

	out := s.appJSON(a)
	d, err := s.st.LatestDeployment(a.ID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		out["status"] = "never_deployed"
		out["image"] = ""
	case err != nil:
		log.Printf("latest deployment: %v", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	default:
		out["status"] = d.Status
		out["image"] = d.ImageRef
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *server) handleDeleteApp(w http.ResponseWriter, r *http.Request, u store.User) {
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

	if err := s.kube.DeleteAppObjects(r.Context(), p.Namespace, a.Name); err != nil {
		log.Printf("delete app objects: %v", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	if err := s.st.DeleteApp(a.ID); err != nil {
		log.Printf("delete app: %v", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleDeployApp dispatches on the request shape: a multipart body carries
// a source tarball for an async build (Job -> wait -> apply); a JSON body
// with a non-empty "image" is the synchronous prebuilt-image path (kept
// byte-for-byte compatible with the pre-build-pipeline behavior); a git-source
// app (App.SourceType == "git") with neither triggers an async build cloning
// from its configured repo; anything else is a bad request.
func (s *server) handleDeployApp(w http.ResponseWriter, r *http.Request, u store.User) {
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

	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		if s.src == nil {
			writeError(w, http.StatusServiceUnavailable, "build_unavailable", "server has no data directory configured")
			return
		}

		if err := r.ParseMultipartForm(64 << 20); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "invalid multipart body")
			return
		}
		part, _, err := r.FormFile("source")
		if err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "missing source file")
			return
		}
		defer part.Close()

		d, err := s.st.CreateDeployment(a.ID, "building", "", u.ID)
		if err != nil {
			log.Printf("create deployment: %v", err)
			writeError(w, http.StatusInternalServerError, "internal", "internal error")
			return
		}
		if _, err := s.src.Save(d.ID, part); err != nil {
			log.Printf("save source tarball: %v", err)
			writeError(w, http.StatusInternalServerError, "internal", "internal error")
			return
		}

		s.startBuild(p, a, d)

		writeJSON(w, http.StatusAccepted, map[string]any{
			"deployment_id": d.ID,
			"status":        "building",
		})
		return
	}

	var req struct {
		Image string `json:"image"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}

	if req.Image != "" {
		s.deployImage(w, r, p, a, req.Image)
		return
	}

	if a.SourceType == "git" {
		if s.src == nil {
			writeError(w, http.StatusServiceUnavailable, "build_unavailable", "server has no data directory configured")
			return
		}
		d, err := s.st.CreateDeployment(a.ID, "building", "", u.ID)
		if err != nil {
			log.Printf("create deployment: %v", err)
			writeError(w, http.StatusInternalServerError, "internal", "internal error")
			return
		}
		s.startBuild(p, a, d)
		writeJSON(w, http.StatusAccepted, map[string]any{"deployment_id": d.ID, "status": "building"})
		return
	}
	writeError(w, http.StatusBadRequest, "bad_request", "provide a source tarball or an image")
}

// deployImage is the synchronous prebuilt-image deploy path: render, apply,
// mark live. Unchanged from the pre-build-pipeline behavior.
func (s *server) deployImage(w http.ResponseWriter, r *http.Request, p store.Project, a store.App, image string) {
	d, err := s.st.CreateDeployment(a.ID, "deploying", image, 0)
	if err != nil {
		log.Printf("create deployment: %v", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}

	rendered, err := s.renderApp(p, a, image, true)
	if err == nil {
		if err = s.kube.EnsureNamespace(r.Context(), p.Namespace); err == nil {
			err = s.kube.Apply(r.Context(), p.Namespace, rendered.Objects)
		}
	}
	if err != nil {
		if setErr := s.st.SetDeploymentStatus(d.ID, "failed"); setErr != nil {
			log.Printf("set deployment failed: %v", setErr)
		}
		writeError(w, http.StatusBadGateway, "deploy_failed", err.Error())
		return
	}

	if err := s.st.SetDeploymentStatus(d.ID, "live"); err != nil {
		log.Printf("set deployment live: %v", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"deployment_id": d.ID,
		"status":        "live",
		"url":           "http://" + hostFor(a.Name, s.externalIP),
	})
}

// requireDeploy loads a deployment by id and verifies it belongs to app a.
// Writes the error response and returns ok=false on failure.
func (s *server) requireDeploy(w http.ResponseWriter, a store.App, idStr string) (store.Deployment, bool) {
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid deploy id")
		return store.Deployment{}, false
	}
	d, err := s.st.GetDeployment(id)
	if errors.Is(err, store.ErrNotFound) || (err == nil && d.AppID != a.ID) {
		writeError(w, http.StatusNotFound, "not_found", "no such deploy")
		return store.Deployment{}, false
	}
	if err != nil {
		log.Printf("get deploy: %v", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return store.Deployment{}, false
	}
	return d, true
}

func (s *server) handleGetDeploy(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.requireProject(w, u, r.PathValue("project"))
	if !ok {
		return
	}
	a, ok := s.requireApp(w, p, r.PathValue("app"))
	if !ok {
		return
	}
	d, ok := s.requireDeploy(w, a, r.PathValue("id"))
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"deployment_id": d.ID,
		"status":        d.Status,
		"image":         d.ImageRef,
		"url":           "http://" + hostFor(a.Name, s.externalIP),
	})
}

func (s *server) handleDeployLogs(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.requireProject(w, u, r.PathValue("project"))
	if !ok {
		return
	}
	a, ok := s.requireApp(w, p, r.PathValue("app"))
	if !ok {
		return
	}
	d, ok := s.requireDeploy(w, a, r.PathValue("id"))
	if !ok {
		return
	}
	if s.src == nil {
		writeError(w, http.StatusServiceUnavailable, "build_unavailable", "no build logs available")
		return
	}
	logBytes, err := s.src.ReadLog(d.ID)
	if err != nil {
		log.Printf("read log: %v", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	w.Write(logBytes)
}

func (s *server) handleScaleApp(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.requireProject(w, u, r.PathValue("project"))
	if !ok {
		return
	}
	a, ok := s.requireApp(w, p, r.PathValue("app"))
	if !ok {
		return
	}

	var req struct {
		Replicas int `json:"replicas"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}

	// Check whether the app is live BEFORE persisting the replica change:
	// a live app with no kube client can't apply a scale, so it must not
	// record a DB state it can't honor.
	d, err := s.st.LatestDeployment(a.ID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		log.Printf("latest deployment: %v", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	live := err == nil && d.Status == "live"
	if live && !s.requireKube(w) {
		return
	}

	if err := s.st.SetReplicas(a.ID, req.Replicas); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	a.Replicas = req.Replicas

	if live {
		if err := s.syncApp(r.Context(), p, a); err != nil {
			log.Printf("sync app: %v", err)
			writeError(w, http.StatusInternalServerError, "internal", "internal error")
			return
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{"replicas": a.Replicas})
}
