// Package server implements luncur's REST API.
package server

import (
	"net/http"

	"github.com/sutantodadang/luncur/internal/kube"
	"github.com/sutantodadang/luncur/internal/secret"
	"github.com/sutantodadang/luncur/internal/store"
)

// Deps bundles server dependencies. Sealer and Kube may be nil (e.g. in
// tests, or when kube is unavailable at boot); handlers that need them must
// check via requireKube.
type Deps struct {
	Store      *store.Store
	Sealer     *secret.Sealer
	Kube       *kube.Client
	ExternalIP string
}

type server struct {
	st         *store.Store
	sealer     *secret.Sealer
	kube       *kube.Client
	externalIP string
}

// New builds the full API handler. Later plans add their routes here.
func New(d Deps) http.Handler {
	externalIP := d.ExternalIP
	if externalIP == "" {
		externalIP = "127.0.0.1"
	}
	s := &server{st: d.Store, sealer: d.Sealer, kube: d.Kube, externalIP: externalIP}
	mux := http.NewServeMux()

	mux.HandleFunc("GET /v1/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("POST /v1/login", s.handleLogin)
	mux.HandleFunc("GET /v1/me", s.authed(s.handleMe))
	mux.HandleFunc("POST /v1/users", s.adminOnly(s.handleCreateUser))
	mux.HandleFunc("POST /v1/projects", s.adminOnly(s.handleCreateProject))
	mux.HandleFunc("GET /v1/projects", s.authed(s.handleListProjects))
	mux.HandleFunc("POST /v1/projects/{project}/members", s.adminOnly(s.handleAddMember))
	mux.HandleFunc("POST /v1/projects/{project}/apps", s.authed(s.handleCreateApp))
	mux.HandleFunc("GET /v1/projects/{project}/apps", s.authed(s.handleListApps))
	mux.HandleFunc("GET /v1/projects/{project}/apps/{app}", s.authed(s.handleGetApp))
	mux.HandleFunc("DELETE /v1/projects/{project}/apps/{app}", s.authed(s.handleDeleteApp))
	mux.HandleFunc("POST /v1/projects/{project}/apps/{app}/deploy", s.authed(s.handleDeployApp))
	mux.HandleFunc("POST /v1/projects/{project}/apps/{app}/scale", s.authed(s.handleScaleApp))
	mux.HandleFunc("GET /v1/projects/{project}/apps/{app}/env", s.authed(s.handleGetEnv))
	mux.HandleFunc("PUT /v1/projects/{project}/apps/{app}/env", s.authed(s.handleSetEnv))
	mux.HandleFunc("DELETE /v1/projects/{project}/apps/{app}/env/{key}", s.authed(s.handleUnsetEnv))
	mux.HandleFunc("PUT /v1/projects/{project}/apps/{app}/overrides/{kind}", s.authed(s.handleSetOverride))
	mux.HandleFunc("DELETE /v1/projects/{project}/apps/{app}/overrides/{kind}", s.authed(s.handleDeleteOverride))
	mux.HandleFunc("GET /v1/projects/{project}/apps/{app}/raw", s.authed(s.handleRawManifest))

	// Fallback for unmatched paths keeps every response envelope-compliant
	// instead of falling through to the stdlib's plain-text 404.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "not_found", "no such endpoint")
	})

	return mux
}

// requireKube writes a 503 and returns false when no kube client is
// configured, so handlers can bail out early.
func (s *server) requireKube(w http.ResponseWriter) bool {
	if s.kube == nil {
		writeError(w, http.StatusServiceUnavailable, "kubernetes_unavailable", "kubernetes is not configured")
		return false
	}
	return true
}
