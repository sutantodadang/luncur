// Package server implements luncur's REST API.
package server

import (
	"embed"
	"html/template"
	"log"
	"net/http"

	"github.com/sutantodadang/luncur/internal/build"
	"github.com/sutantodadang/luncur/internal/kube"
	"github.com/sutantodadang/luncur/internal/secret"
	"github.com/sutantodadang/luncur/internal/store"
)

//go:embed templates/*.html
var templateFS embed.FS

// Deps bundles server dependencies. Sealer and Kube may be nil (e.g. in
// tests, or when kube is unavailable at boot); handlers that need them must
// check via requireKube. DataDir enables source-build deploys: when set, a
// build.Source is constructed and multipart tarball uploads are accepted.
type Deps struct {
	Store      *store.Store
	Sealer     *secret.Sealer
	Kube       *kube.Client
	ExternalIP string

	DataDir         string
	BuilderImage    string
	RegistryHost    string
	SystemNamespace string
	DataPVC         string
}

type server struct {
	st         *store.Store
	sealer     *secret.Sealer
	kube       *kube.Client
	externalIP string

	src             *build.Source
	builderImage    string
	registryHost    string
	systemNamespace string
	dataPVC         string

	certs *certManager

	tmpl *template.Template
}

// newServer wires all server fields (including build config defaults) but
// does not build the route table — call handler() for that. Kept separate
// so tests can obtain a *server and call unexported methods (e.g. runBuild)
// directly.
func newServer(d Deps) *server {
	externalIP := d.ExternalIP
	if externalIP == "" {
		externalIP = "127.0.0.1"
	}
	systemNamespace := d.SystemNamespace
	if systemNamespace == "" {
		systemNamespace = "luncur-system"
	}
	registryHost := d.RegistryHost
	if registryHost == "" {
		registryHost = "registry.luncur-system:5000"
	}
	builderImage := d.BuilderImage
	if builderImage == "" {
		builderImage = "luncur/builder:latest"
	}
	dataPVC := d.DataPVC
	if dataPVC == "" {
		dataPVC = "luncur-data"
	}

	s := &server{
		st:              d.Store,
		sealer:          d.Sealer,
		kube:            d.Kube,
		externalIP:      externalIP,
		builderImage:    builderImage,
		registryHost:    registryHost,
		systemNamespace: systemNamespace,
		dataPVC:         dataPVC,
	}

	if d.DataDir != "" {
		src, err := build.NewSource(d.DataDir)
		if err != nil {
			log.Printf("build source init: %v", err)
		} else {
			s.src = src
		}
	}

	s.tmpl = template.Must(template.ParseFS(templateFS, "templates/*.html"))

	return s
}

// handler builds the full API mux from an already-wired server.
func (s *server) handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /v1/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("POST /v1/login", s.handleLogin)
	mux.HandleFunc("GET /v1/me", s.authed(s.handleMe))
	mux.HandleFunc("POST /v1/ssh-keys", s.authed(s.handleAddSSHKey))
	mux.HandleFunc("GET /v1/ssh-keys", s.authed(s.handleListSSHKeys))
	mux.HandleFunc("DELETE /v1/ssh-keys/{id}", s.authed(s.handleDeleteSSHKey))
	mux.HandleFunc("POST /v1/users", s.adminOnly(s.handleCreateUser))
	mux.HandleFunc("POST /v1/projects", s.adminOnly(s.handleCreateProject))
	mux.HandleFunc("GET /v1/projects", s.authed(s.handleListProjects))
	mux.HandleFunc("POST /v1/projects/{project}/members", s.adminOnly(s.handleAddMember))
	mux.HandleFunc("POST /v1/projects/{project}/apps", s.authed(s.handleCreateApp))
	mux.HandleFunc("GET /v1/projects/{project}/apps", s.authed(s.handleListApps))
	mux.HandleFunc("GET /v1/projects/{project}/apps/{app}", s.authed(s.handleGetApp))
	mux.HandleFunc("DELETE /v1/projects/{project}/apps/{app}", s.authed(s.handleDeleteApp))
	mux.HandleFunc("POST /v1/projects/{project}/apps/{app}/deploy", s.authed(s.handleDeployApp))
	mux.HandleFunc("GET /v1/projects/{project}/apps/{app}/deploys/{id}", s.authed(s.handleGetDeploy))
	mux.HandleFunc("GET /v1/projects/{project}/apps/{app}/deploys/{id}/logs", s.authed(s.handleDeployLogs))
	mux.HandleFunc("POST /v1/projects/{project}/apps/{app}/scale", s.authed(s.handleScaleApp))
	mux.HandleFunc("GET /v1/projects/{project}/apps/{app}/env", s.authed(s.handleGetEnv))
	mux.HandleFunc("PUT /v1/projects/{project}/apps/{app}/env", s.authed(s.handleSetEnv))
	mux.HandleFunc("DELETE /v1/projects/{project}/apps/{app}/env/{key}", s.authed(s.handleUnsetEnv))
	mux.HandleFunc("PUT /v1/projects/{project}/apps/{app}/overrides/{kind}", s.authed(s.handleSetOverride))
	mux.HandleFunc("DELETE /v1/projects/{project}/apps/{app}/overrides/{kind}", s.authed(s.handleDeleteOverride))
	mux.HandleFunc("GET /v1/projects/{project}/apps/{app}/raw", s.authed(s.handleRawManifest))
	mux.HandleFunc("GET /v1/projects/{project}/apps/{app}/logs", s.authed(s.handleRuntimeLogs))
	mux.HandleFunc("POST /v1/projects/{project}/apps/{app}/domains", s.authed(s.handleAddDomain))
	mux.HandleFunc("GET /v1/projects/{project}/apps/{app}/domains", s.authed(s.handleListDomains))
	mux.HandleFunc("DELETE /v1/projects/{project}/apps/{app}/domains/{hostname}", s.authed(s.handleDeleteDomain))
	mux.HandleFunc("POST /v1/projects/{project}/apps/{app}/domains/{hostname}/retry", s.authed(s.handleRetryDomain))

	s.uiRoutes(mux)

	// Fallback for unmatched paths keeps every response envelope-compliant
	// instead of falling through to the stdlib's plain-text 404, except for
	// the root path which redirects into the web UI.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			http.Redirect(w, r, "/ui/", http.StatusSeeOther)
			return
		}
		writeError(w, http.StatusNotFound, "not_found", "no such endpoint")
	})

	return mux
}

// New builds the full API handler. Later plans add their routes here.
func New(d Deps) http.Handler {
	h, _ := NewWithBackend(d)
	return h
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
