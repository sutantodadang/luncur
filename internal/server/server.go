// Package server implements luncur's REST API.
package server

import (
	"context"
	"embed"
	"html/template"
	"log"
	"net/http"
	"time"

	"github.com/sutantodadang/luncur/internal/acme"
	"github.com/sutantodadang/luncur/internal/build"
	"github.com/sutantodadang/luncur/internal/dns"
	"github.com/sutantodadang/luncur/internal/kube"
	"github.com/sutantodadang/luncur/internal/mail"
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
	ACMEDirectory   string // override ACME directory URL ("" = setting/Let's Encrypt)
	SecretKeyPath   string // sealer key file, included in backups when set
	Version         string // server build version, reported by doctor
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
	dataDir         string
	secretKeyPath   string
	version         string

	// httpClient is used by the deploy/cert notifier (notify.go); tests may
	// swap it to point at an httptest.Server or a short-timeout client.
	httpClient *http.Client

	// execer runs commands in pods (addon dumps); s.kube in production,
	// a fake in tests. nowFn is injectable for deterministic archive names.
	execer kube.PodExecer
	nowFn  func() time.Time

	// mailer builds the invite Mailer from settings; tests override it.
	mailer func() (mail.Mailer, error)

	// dnsProvider builds the DNS-01 provider from settings; tests override.
	dnsProvider func() (dns.Provider, error)

	certs *certManager

	// lastRegistryGC tracks the last completed weekly registry GC sweep,
	// in memory only — StartRegistryGC uses it to decide when to run again.
	lastRegistryGC time.Time

	tmpl *template.Template

	// mon collects live app/node CPU/memory samples in memory; see monitor.go.
	mon *monitor
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
		builderImage = build.DefaultBuilderImage
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
		dataDir:         d.DataDir,
		secretKeyPath:   d.SecretKeyPath,
		version:         d.Version,
		nowFn:           time.Now,
		httpClient:      &http.Client{Timeout: 5 * time.Second},
		mon:             newMonitor(),
	}
	if d.Kube != nil {
		s.execer = d.Kube
	}
	s.mailer = s.smtpMailer
	s.dnsProvider = s.dnsProviderFromSettings

	if d.DataDir != "" {
		src, err := build.NewSource(d.DataDir)
		if err != nil {
			log.Printf("build source init: %v", err)
		} else {
			s.src = src
		}
	}

	s.tmpl = template.Must(template.New("").Funcs(template.FuncMap{
		"v": func() string { return staticHash() },
	}).ParseFS(templateFS, "templates/*.html"))

	if d.Store != nil {
		s.certs = newCertManager(s, d.ACMEDirectory)
	}

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
	mux.HandleFunc("GET /v1/users", s.adminOnly(s.handleListUsers))
	mux.HandleFunc("DELETE /v1/users/{id}", s.adminOnly(s.handleDeleteUser))
	mux.HandleFunc("POST /v1/invites", s.adminOnly(s.handleCreateInvite))
	mux.HandleFunc("GET /v1/invites", s.adminOnly(s.handleListInvites))
	mux.HandleFunc("DELETE /v1/invites/{token}", s.adminOnly(s.handleRevokeInvite))
	mux.HandleFunc("POST /v1/projects", s.adminOnly(s.handleCreateProject))
	mux.HandleFunc("GET /v1/projects", s.authed(s.handleListProjects))
	mux.HandleFunc("POST /v1/projects/{project}/members", s.adminOnly(s.handleAddMember))
	mux.HandleFunc("POST /v1/projects/{project}/apps", s.authed(s.handleCreateApp))
	mux.HandleFunc("GET /v1/projects/{project}/apps", s.authed(s.handleListApps))
	mux.HandleFunc("GET /v1/projects/{project}/apps/{app}", s.authed(s.handleGetApp))
	mux.HandleFunc("DELETE /v1/projects/{project}/apps/{app}", s.authed(s.handleDeleteApp))
	mux.HandleFunc("POST /v1/projects/{project}/apps/{app}/eject", s.authed(s.handleEjectApp))
	mux.HandleFunc("POST /v1/projects/{project}/apps/{app}/adopt", s.authed(s.handleAdoptApp))
	mux.HandleFunc("POST /v1/projects/{project}/apps/{app}/deploy", s.authed(s.handleDeployApp))
	mux.HandleFunc("GET /v1/projects/{project}/apps/{app}/deploys", s.authed(s.handleListDeploys))
	mux.HandleFunc("GET /v1/projects/{project}/apps/{app}/deploys/{id}", s.authed(s.handleGetDeploy))
	mux.HandleFunc("GET /v1/projects/{project}/apps/{app}/deploys/{id}/logs", s.authed(s.handleDeployLogs))
	mux.HandleFunc("POST /v1/projects/{project}/apps/{app}/scale", s.authed(s.handleScaleApp))
	mux.HandleFunc("POST /v1/projects/{project}/apps/{app}/health", s.authed(s.handleSetHealth))
	mux.HandleFunc("POST /v1/projects/{project}/apps/{app}/webhook", s.authed(s.handleWebhookEnable))
	mux.HandleFunc("GET /v1/projects/{project}/apps/{app}/webhook", s.authed(s.handleWebhookShow))
	mux.HandleFunc("DELETE /v1/projects/{project}/apps/{app}/webhook", s.authed(s.handleWebhookDisable))
	mux.HandleFunc("POST /v1/projects/{project}/apps/{app}/rollback", s.authed(s.handleRollback))
	mux.HandleFunc("GET /v1/projects/{project}/apps/{app}/env", s.authed(s.handleGetEnv))
	mux.HandleFunc("PUT /v1/projects/{project}/apps/{app}/env", s.authed(s.handleSetEnv))
	mux.HandleFunc("PUT /v1/projects/{project}/apps/{app}/env/bulk", s.authed(s.handleBulkSetEnv))
	mux.HandleFunc("DELETE /v1/projects/{project}/apps/{app}/env/{key}", s.authed(s.handleUnsetEnv))
	mux.HandleFunc("PUT /v1/projects/{project}/apps/{app}/overrides/{kind}", s.authed(s.handleSetOverride))
	mux.HandleFunc("DELETE /v1/projects/{project}/apps/{app}/overrides/{kind}", s.authed(s.handleDeleteOverride))
	mux.HandleFunc("GET /v1/projects/{project}/apps/{app}/raw", s.authed(s.handleRawManifest))
	mux.HandleFunc("GET /v1/projects/{project}/apps/{app}/logs", s.authed(s.handleRuntimeLogs))
	mux.HandleFunc("GET /v1/projects/{project}/apps/{app}/metrics", s.authed(s.handleAppMetrics))
	mux.HandleFunc("GET /v1/projects/{project}/apps/{app}/metrics/history", s.authed(s.handleAppMetricsHistory))
	mux.HandleFunc("GET /v1/projects/{project}/apps/{app}/pods", s.authed(s.handleAppPods))
	mux.HandleFunc("POST /v1/projects/{project}/apps/{app}/volumes", s.authed(s.handleAddVolume))
	mux.HandleFunc("GET /v1/projects/{project}/apps/{app}/volumes", s.authed(s.handleListVolumes))
	mux.HandleFunc("DELETE /v1/projects/{project}/apps/{app}/volumes/{name}", s.authed(s.handleDeleteVolume))
	mux.HandleFunc("POST /v1/projects/{project}/apps/{app}/domains", s.authed(s.handleAddDomain))
	mux.HandleFunc("GET /v1/projects/{project}/apps/{app}/domains", s.authed(s.handleListDomains))
	mux.HandleFunc("DELETE /v1/projects/{project}/apps/{app}/domains/{hostname}", s.authed(s.handleDeleteDomain))
	mux.HandleFunc("POST /v1/projects/{project}/apps/{app}/domains/{hostname}/retry", s.authed(s.handleRetryDomain))
	mux.HandleFunc("POST /v1/projects/{project}/addons", s.authed(s.handleCreateAddon))
	mux.HandleFunc("GET /v1/projects/{project}/addons", s.authed(s.handleListAddons))
	mux.HandleFunc("POST /v1/projects/{project}/addons/{name}/attach", s.authed(s.handleAttachAddon))
	mux.HandleFunc("POST /v1/projects/{project}/addons/{name}/detach", s.authed(s.handleDetachAddon))
	mux.HandleFunc("DELETE /v1/projects/{project}/addons/{name}", s.authed(s.handleDeleteAddon))
	mux.HandleFunc("POST /v1/projects/{project}/addons/{name}/upgrade", s.authed(s.handleUpgradeAddon))
	mux.HandleFunc("GET /v1/projects/{project}/addons/{name}/url", s.authed(s.handleAddonURL))
	mux.HandleFunc("POST /v1/system/update", s.adminOnly(s.handleSystemUpdate))
	mux.HandleFunc("POST /v1/backups", s.adminOnly(s.handleCreateBackup))
	mux.HandleFunc("GET /v1/backups", s.adminOnly(s.handleListBackups))
	mux.HandleFunc("POST /v1/backups/prune", s.adminOnly(s.handlePruneBackups))
	mux.HandleFunc("GET /v1/settings/{key}", s.adminOnly(s.handleGetSetting))
	mux.HandleFunc("PUT /v1/settings/{key}", s.adminOnly(s.handleSetSetting))
	mux.HandleFunc("POST /v1/registry/gc", s.adminOnly(s.handleRegistryGC))
	mux.HandleFunc("GET /v1/tokens", s.authed(s.handleListTokens))
	mux.HandleFunc("DELETE /v1/tokens/{id}", s.authed(s.handleRevokeToken))
	mux.HandleFunc("GET /v1/audit", s.adminOnly(s.handleListAudit))
	mux.HandleFunc("GET /v1/doctor", s.adminOnly(s.handleDoctor))
	mux.HandleFunc("GET /v1/nodes", s.adminOnly(s.handleListNodes))

	// ACME HTTP-01 challenge path: served by luncur itself, no auth (the
	// ACME CA fetches it directly). Nil-guarded: tests may build a server
	// without a store/manager.
	if s.certs != nil {
		mux.Handle("GET "+acme.ChallengePath+"{token}", s.certs.Challenges())
	}

	// Webhook trigger: unauthenticated by design (HMAC/token verification
	// IS the auth) — a git provider hits this directly, so it must not sit
	// behind s.authed's bearer-token check.
	mux.HandleFunc("POST /hooks/apps/{project}/{app}", s.handleWebhookTrigger)

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

	return s.auditMiddleware(mux)
}

// New builds the full API handler. Later plans add their routes here.
func New(d Deps) http.Handler {
	h, _, _ := NewWithBackend(d)
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

// StartCerts launches the builtin cert manager loop when the provider is
// builtin; call in a goroutine-managing context (serve.go).
func (s *server) StartCerts(ctx context.Context) {
	if s.certProviderName() != "builtin" || s.kube == nil {
		return
	}
	go s.certs.Run(ctx)
}
