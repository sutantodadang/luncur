// Package server implements luncur's REST API.
package server

import (
	"context"
	"crypto/rand"
	"embed"
	"fmt"
	"html/template"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
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
	NodeTokenPath   string // K3s node token ("" = up.NodeTokenPath); tests override
	VastBaseURL     string // vast.ai API base ("" = production); tests override
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
	nodeTokenPath   string
	vastBaseURL     string

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

	// sweepMLflowURLFn resolves an app's attached mlflow addon to its
	// in-cluster tracking URL ("" when none); defaults to sweepMLflowURL.
	// Tests override it to point at an httptest.Server instead of a real
	// cluster-internal DNS name, mirroring mailer/dnsProvider above.
	sweepMLflowURLFn func(store.App, string) string

	certs *certManager

	// lastRegistryGC tracks the last completed weekly registry GC sweep,
	// in memory only — StartRegistryGC uses it to decide when to run again.
	lastRegistryGC time.Time

	// gpuIdleSince tracks, per GPU instance label, when its node was last
	// seen idle (no GPU pod scheduled on it). In-memory, loop-local state
	// for runGPUIdleLoop — written only by that single goroutine.
	gpuIdleSince map[string]time.Time

	// sweepMLflowDown tracks, per sweep id, whether its mlflow addon has
	// already been found unreachable this sweep's lifetime — once set, the
	// loop stops retrying mlflow and reads log-line metrics instead.
	// In-memory, loop-local state for startSweepLoop.
	sweepMLflowDown map[string]bool

	tmpl *template.Template

	// mon collects live app/node CPU/memory samples in memory; see monitor.go.
	mon *monitor

	// loginLimiter guards login endpoints against brute-force attempts;
	// see ratelimit.go.
	loginLimiter *rateLimiter

	// fwdKey signs port-forward handoff tokens and luncur_fwd cookies
	// (fwdtoken.go). Random per boot: a restart only forces re-opening.
	fwdKey []byte

	// fwdDial opens the tunnel's in-cluster TCP connection; tests point it
	// at a local listener.
	fwdDial func(ctx context.Context, network, addr string) (net.Conn, error)

	// fwdProxyTargetFn resolves a forward-host app to its in-cluster proxy
	// target (fwdproxy.go); defaults to fwdProxyTarget. Tests override it to
	// point at an httptest.Server instead of cluster-internal DNS, mirroring
	// sweepMLflowURLFn above.
	fwdProxyTargetFn func(store.Project, store.App) *url.URL
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
		nodeTokenPath:   d.NodeTokenPath,
		vastBaseURL:     d.VastBaseURL,
		dataDir:         d.DataDir,
		secretKeyPath:   d.SecretKeyPath,
		version:         d.Version,
		nowFn:           time.Now,
		httpClient:      &http.Client{Timeout: 5 * time.Second},
		mon:             newMonitor(),
		loginLimiter:    newRateLimiter(time.Now),
	}
	if d.Kube != nil {
		s.execer = d.Kube
	}
	s.mailer = s.smtpMailer
	s.dnsProvider = s.dnsProviderFromSettings
	s.sweepMLflowURLFn = s.sweepMLflowURL

	s.fwdKey = make([]byte, 32)
	if _, err := rand.Read(s.fwdKey); err != nil {
		panic(fmt.Sprintf("fwd key: %v", err)) // crypto/rand failure is unrecoverable
	}
	s.fwdDial = (&net.Dialer{Timeout: 10 * time.Second}).DialContext
	s.fwdProxyTargetFn = fwdProxyTarget

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

// envRoutePrefix is the path prefix every app/addon route lives under —
// routeEnv splices "envs/{env}/" in right after it to build each route's
// env-scoped twin.
const envRoutePrefix = "/v1/projects/{project}/"

// routeEnv registers pattern (a full "METHOD /path" mux pattern starting
// with envRoutePrefix) under both its legacy path and an explicit-env twin
// ".../envs/{env}/..." bound to the SAME handler. The handlers underneath
// (apps.go, addons.go, and everything env-scoped by Task 7) already resolve
// the environment from r.PathValue("env") via requireEnv/requireEnvWrite —
// "" (the legacy path never sets it) resolves to the project's default
// environment, so registering the twin is all that's needed to make the
// explicit-env form reachable.
func routeEnv(mux *http.ServeMux, pattern string, handler http.HandlerFunc) {
	mux.HandleFunc(pattern, handler)
	method, path, ok := strings.Cut(pattern, " ")
	if !ok || !strings.HasPrefix(path, envRoutePrefix) {
		panic("routeEnv: pattern must be \"METHOD " + envRoutePrefix + "...\": " + pattern)
	}
	envPath := envRoutePrefix + "envs/{env}/" + strings.TrimPrefix(path, envRoutePrefix)
	mux.HandleFunc(method+" "+envPath, handler)
}

// handler builds the full API mux from an already-wired server.
func (s *server) handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /v1/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /metrics/prometheus", s.handlePrometheus)
	mux.HandleFunc("POST /v1/login", s.rateLimited(s.handleLogin))
	mux.HandleFunc("GET /v1/me", s.authed(s.handleMe))
	mux.HandleFunc("PUT /v1/me/password", s.authed(s.handleChangePassword))
	mux.HandleFunc("PUT /v1/me/email", s.authed(s.handleChangeEmail))
	mux.HandleFunc("PUT /v1/users/{id}/password", s.adminOnly(s.handleAdminSetPassword))
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
	mux.HandleFunc("PUT /v1/projects/{project}", s.adminOnly(s.handleRenameProject))
	mux.HandleFunc("DELETE /v1/projects/{project}", s.adminOnly(s.handleDeleteProject))
	mux.HandleFunc("POST /v1/projects/{project}/members", s.adminOnly(s.handleAddMember))
	mux.HandleFunc("DELETE /v1/projects/{project}/members/{email}", s.adminOnly(s.handleRemoveMember))
	mux.HandleFunc("PUT /v1/projects/{project}/gpu-quota", s.adminOnly(s.handleSetGPUQuota))
	mux.HandleFunc("PUT /v1/projects/{project}/quota", s.adminOnly(s.handleSetProjectQuota))
	mux.HandleFunc("GET /v1/projects/{project}/envs", s.authed(s.handleListEnvs))
	mux.HandleFunc("POST /v1/projects/{project}/envs", s.authed(s.handleCreateEnv))
	mux.HandleFunc("DELETE /v1/projects/{project}/envs/{env}", s.authed(s.handleDeleteEnv))
	mux.HandleFunc("PUT /v1/projects/{project}/envs/{env}/default", s.authed(s.handleSetDefaultEnv))
	mux.HandleFunc("PUT /v1/projects/{project}/preview-base", s.authed(s.handleSetPreviewBase))
	mux.HandleFunc("GET /v1/projects/{project}/previews", s.authed(s.handleListPreviews))
	mux.HandleFunc("POST /v1/projects/{project}/previews", s.authed(s.handleCreatePreview))
	mux.HandleFunc("DELETE /v1/projects/{project}/previews/{name}", s.authed(s.handleDeletePreview))
	mux.HandleFunc("POST /v1/projects/{project}/webhook/secret", s.authed(s.handleGenerateProjectWebhookSecret))
	// Project build webhook: unauthenticated by design (HMAC/token
	// verification IS the auth), same convention as the per-app deploy
	// hook and pipeline trigger hooks below.
	mux.HandleFunc("POST /v1/projects/{project}/webhook", s.handleProjectWebhook)
	routeEnv(mux, "POST /v1/projects/{project}/apps", s.authed(s.handleCreateApp))
	routeEnv(mux, "GET /v1/projects/{project}/apps", s.authed(s.handleListApps))
	routeEnv(mux, "GET /v1/projects/{project}/apps/{app}", s.authed(s.handleGetApp))
	routeEnv(mux, "DELETE /v1/projects/{project}/apps/{app}", s.authed(s.handleDeleteApp))
	routeEnv(mux, "POST /v1/projects/{project}/apps/{app}/eject", s.authed(s.handleEjectApp))
	routeEnv(mux, "POST /v1/projects/{project}/apps/{app}/adopt", s.authed(s.handleAdoptApp))
	routeEnv(mux, "POST /v1/projects/{project}/apps/{app}/deploy", s.authed(s.handleDeployApp))
	routeEnv(mux, "GET /v1/projects/{project}/apps/{app}/deploys", s.authed(s.handleListDeploys))
	routeEnv(mux, "GET /v1/projects/{project}/apps/{app}/deploys/{id}", s.authed(s.handleGetDeploy))
	routeEnv(mux, "GET /v1/projects/{project}/apps/{app}/deploys/{id}/logs", s.authed(s.handleDeployLogs))
	routeEnv(mux, "PUT /v1/projects/{project}/apps/{app}/training", s.authed(s.handleSetTraining))
	routeEnv(mux, "POST /v1/projects/{project}/apps/{app}/runs", s.authed(s.handleCreateRun))
	routeEnv(mux, "GET /v1/projects/{project}/apps/{app}/runs", s.authed(s.handleListRuns))
	routeEnv(mux, "GET /v1/projects/{project}/apps/{app}/runs/{id}", s.authed(s.handleGetRun))
	routeEnv(mux, "GET /v1/projects/{project}/apps/{app}/runs/{id}/logs", s.authed(s.handleRunLogs))
	routeEnv(mux, "POST /v1/projects/{project}/apps/{app}/sweeps", s.authed(s.handleCreateSweep))
	routeEnv(mux, "GET /v1/projects/{project}/apps/{app}/sweeps", s.authed(s.handleListSweeps))
	routeEnv(mux, "GET /v1/projects/{project}/apps/{app}/sweeps/{id}", s.authed(s.handleGetSweep))
	routeEnv(mux, "POST /v1/projects/{project}/apps/{app}/sweeps/{id}/stop", s.authed(s.handleStopSweep))
	mux.HandleFunc("POST /v1/projects/{project}/pipelines", s.authed(s.handleCreatePipeline))
	mux.HandleFunc("GET /v1/projects/{project}/pipelines", s.authed(s.handleListPipelines))
	mux.HandleFunc("GET /v1/projects/{project}/pipelines/{name}", s.authed(s.handleGetPipeline))
	mux.HandleFunc("PUT /v1/projects/{project}/pipelines/{name}", s.authed(s.handleUpdatePipeline))
	mux.HandleFunc("DELETE /v1/projects/{project}/pipelines/{name}", s.authed(s.handleDeletePipeline))
	mux.HandleFunc("POST /v1/projects/{project}/pipelines/{name}/runs", s.authed(s.handleCreatePipelineRun))
	mux.HandleFunc("GET /v1/projects/{project}/pipelines/{name}/runs", s.authed(s.handleListPipelineRuns))
	mux.HandleFunc("GET /v1/projects/{project}/pipelines/{name}/runs/{id}", s.authed(s.handleGetPipelineRun))
	mux.HandleFunc("POST /v1/projects/{project}/pipelines/{name}/runs/{id}/stop", s.authed(s.handleStopPipelineRun))
	mux.HandleFunc("POST /v1/projects/{project}/pipelines/{name}/webhook-secret", s.authed(s.handleGeneratePipelineWebhookSecret))
	mux.HandleFunc("DELETE /v1/projects/{project}/pipelines/{name}/webhook-secret", s.authed(s.handleDeletePipelineWebhookSecret))
	routeEnv(mux, "POST /v1/projects/{project}/apps/{app}/scale", s.authed(s.handleScaleApp))
	routeEnv(mux, "PUT /v1/projects/{project}/apps/{app}/autoscale", s.authed(s.handleAutoscaleApp))
	routeEnv(mux, "POST /v1/projects/{project}/apps/{app}/health", s.authed(s.handleSetHealth))
	routeEnv(mux, "POST /v1/projects/{project}/apps/{app}/webhook", s.authed(s.handleWebhookEnable))
	routeEnv(mux, "GET /v1/projects/{project}/apps/{app}/webhook", s.authed(s.handleWebhookShow))
	routeEnv(mux, "DELETE /v1/projects/{project}/apps/{app}/webhook", s.authed(s.handleWebhookDisable))
	routeEnv(mux, "POST /v1/projects/{project}/apps/{app}/rollback", s.authed(s.handleRollback))
	routeEnv(mux, "GET /v1/projects/{project}/apps/{app}/env", s.authed(s.handleGetEnv))
	routeEnv(mux, "PUT /v1/projects/{project}/apps/{app}/env", s.authed(s.handleSetEnv))
	routeEnv(mux, "PUT /v1/projects/{project}/apps/{app}/env/bulk", s.authed(s.handleBulkSetEnv))
	routeEnv(mux, "DELETE /v1/projects/{project}/apps/{app}/env/{key}", s.authed(s.handleUnsetEnv))
	routeEnv(mux, "POST /v1/projects/{project}/apps/{app}/redeploy", s.authed(s.handleRedeploy))
	routeEnv(mux, "POST /v1/projects/{project}/apps/{app}/pause", s.authed(s.handlePauseCron))
	routeEnv(mux, "POST /v1/projects/{project}/apps/{app}/resume", s.authed(s.handleResumeCron))
	routeEnv(mux, "POST /v1/projects/{project}/apps/{app}/trigger", s.authed(s.handleTriggerCron))
	routeEnv(mux, "GET /v1/projects/{project}/apps/{app}/cron-runs", s.authed(s.handleCronRuns))
	routeEnv(mux, "PUT /v1/projects/{project}/apps/{app}/git-token", s.authed(s.handleSetGitToken))
	routeEnv(mux, "DELETE /v1/projects/{project}/apps/{app}/git-token", s.authed(s.handleDeleteGitToken))
	routeEnv(mux, "PUT /v1/projects/{project}/apps/{app}/overrides/{kind}", s.authed(s.handleSetOverride))
	routeEnv(mux, "DELETE /v1/projects/{project}/apps/{app}/overrides/{kind}", s.authed(s.handleDeleteOverride))
	routeEnv(mux, "GET /v1/projects/{project}/apps/{app}/raw", s.authed(s.handleRawManifest))
	routeEnv(mux, "GET /v1/projects/{project}/apps/{app}/logs", s.authed(s.handleRuntimeLogs))
	routeEnv(mux, "GET /v1/projects/{project}/apps/{app}/metrics", s.authed(s.handleAppMetrics))
	routeEnv(mux, "GET /v1/projects/{project}/apps/{app}/metrics/history", s.authed(s.handleAppMetricsHistory))
	routeEnv(mux, "GET /v1/projects/{project}/apps/{app}/pods", s.authed(s.handleAppPods))
	routeEnv(mux, "GET /v1/projects/{project}/apps/{app}/forward", s.authed(s.handleForwardApp))
	routeEnv(mux, "POST /v1/projects/{project}/apps/{app}/volumes", s.authed(s.handleAddVolume))
	routeEnv(mux, "GET /v1/projects/{project}/apps/{app}/volumes", s.authed(s.handleListVolumes))
	routeEnv(mux, "DELETE /v1/projects/{project}/apps/{app}/volumes/{name}", s.authed(s.handleDeleteVolume))
	routeEnv(mux, "POST /v1/projects/{project}/apps/{app}/domains", s.authed(s.handleAddDomain))
	routeEnv(mux, "GET /v1/projects/{project}/apps/{app}/domains", s.authed(s.handleListDomains))
	routeEnv(mux, "DELETE /v1/projects/{project}/apps/{app}/domains/{hostname}", s.authed(s.handleDeleteDomain))
	routeEnv(mux, "POST /v1/projects/{project}/apps/{app}/domains/{hostname}/retry", s.authed(s.handleRetryDomain))
	mux.HandleFunc("PUT /v1/projects/{project}/s3", s.authed(s.handleSetProjectS3))
	mux.HandleFunc("GET /v1/projects/{project}/s3", s.authed(s.handleGetProjectS3))
	mux.HandleFunc("DELETE /v1/projects/{project}/s3", s.authed(s.handleDeleteProjectS3))
	routeEnv(mux, "POST /v1/projects/{project}/apps/{app}/s3", s.authed(s.handleAppS3Env))
	routeEnv(mux, "POST /v1/projects/{project}/addons", s.authed(s.handleCreateAddon))
	routeEnv(mux, "GET /v1/projects/{project}/addons", s.authed(s.handleListAddons))
	routeEnv(mux, "POST /v1/projects/{project}/addons/{name}/attach", s.authed(s.handleAttachAddon))
	routeEnv(mux, "POST /v1/projects/{project}/addons/{name}/detach", s.authed(s.handleDetachAddon))
	routeEnv(mux, "DELETE /v1/projects/{project}/addons/{name}", s.authed(s.handleDeleteAddon))
	routeEnv(mux, "POST /v1/projects/{project}/addons/{name}/upgrade", s.authed(s.handleUpgradeAddon))
	routeEnv(mux, "POST /v1/projects/{project}/addons/{name}/restore", s.authed(s.handleRestoreAddon))
	routeEnv(mux, "GET /v1/projects/{project}/addons/{name}/url", s.authed(s.handleAddonURL))
	mux.HandleFunc("POST /v1/system/update", s.adminOnly(s.handleSystemUpdate))
	mux.HandleFunc("POST /v1/system/argo-install", s.adminOnly(s.handleArgoInstall))
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
	mux.HandleFunc("PUT /v1/gpu/key", s.adminOnly(s.handleSetGPUKey))
	mux.HandleFunc("GET /v1/gpu/offers", s.adminOnly(s.handleGPUOffers))
	mux.HandleFunc("POST /v1/gpu/instances", s.adminOnly(s.handleRentGPU))
	mux.HandleFunc("GET /v1/gpu/instances", s.adminOnly(s.handleListGPUInstances))
	mux.HandleFunc("DELETE /v1/gpu/instances/{id}", s.adminOnly(s.handleDestroyGPUInstance))

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
	// Pipeline webhook trigger: same unauthenticated-by-design convention as
	// the app deploy hook above — no additional rate-limit middleware wraps
	// either route (auditMiddleware, applied to the whole mux below, is it).
	mux.HandleFunc("POST /hooks/pipelines/{id}", s.handlePipelineWebhookTrigger)

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

	// Forward-host requests ({app}--{project}.<parent>) are app traffic, not
	// control-plane actions: they branch off before auditMiddleware/routing
	// so they skip both the audit log and the /ui, /v1 route tables
	// entirely. The /open click that mints their handoff token, on the
	// panel host, IS audited like any other UI GET/POST through the normal
	// chain below.
	h := s.auditMiddleware(mux)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if p, a, ok := s.forwardAppFromHost(r.Host); ok {
			s.handleForwardHost(w, r, p, a)
			return
		}
		h.ServeHTTP(w, r)
	})
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
// StartGPUIdleLoop launches the idle auto-destroy loop for rented GPU
// instances; call alongside StartCerts (serve/push).
func (s *server) StartGPUIdleLoop(ctx context.Context) {
	go s.runGPUIdleLoop(ctx)
}

func (s *server) StartCerts(ctx context.Context) {
	if s.certProviderName() != "builtin" || s.kube == nil {
		return
	}
	go s.certs.Run(ctx)
}
