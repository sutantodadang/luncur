package server

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/sutantodadang/luncur/internal/render"
	"github.com/sutantodadang/luncur/internal/store"
)

// handleUIWebhookEnable is enableWebhook's UI twin: same core, but renders
// the app page directly (never redirects) so the freshly generated secret
// rides along on this one response — it must never be persisted in
// plaintext or appear in a URL/query string.
func (s *server) handleUIWebhookEnable(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.uiProjectWrite(w, r, u)
	if !ok {
		return
	}
	a, ok := s.uiApp(w, r, p)
	if !ok {
		return
	}
	if a.Ejected {
		http.Error(w, errAppEjected.Error(), http.StatusConflict)
		return
	}

	secretHex, err := s.enableWebhook(a)
	if err != nil {
		switch {
		case errors.Is(err, errNotGitApp):
			http.Error(w, errNotGitApp.Error(), http.StatusBadRequest)
		case errors.Is(err, errSealerUnavailable):
			http.Error(w, errSealerUnavailable.Error(), http.StatusServiceUnavailable)
		default:
			log.Printf("ui enable webhook: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
		}
		return
	}
	a.WebhookSecret = []byte("x") // renderAppDetail only checks non-nil-ness

	s.renderAppDetail(w, r, u, p, a, map[string]any{"WebhookSecretOnce": secretHex})
}

// handleUIWebhookDisable is disableWebhook's UI twin: clear the secret,
// then redirect back — nothing sensitive to show, so a normal redirect
// (unlike enable) is fine here.
func (s *server) handleUIWebhookDisable(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.uiProjectWrite(w, r, u)
	if !ok {
		return
	}
	a, ok := s.uiApp(w, r, p)
	if !ok {
		return
	}
	if a.Ejected {
		http.Error(w, errAppEjected.Error(), http.StatusConflict)
		return
	}
	if err := s.st.SetWebhookSecret(a.ID, nil); err != nil {
		log.Printf("ui disable webhook: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	flash(w, "ok", "webhook disabled")
	uiRedirect(w, r, p, a)
}

func (s *server) handleUIScale(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.uiProjectWrite(w, r, u)
	if !ok {
		return
	}
	a, ok := s.uiApp(w, r, p)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	// cron apps don't scale replicas (the form omits the input for them);
	// only parse it for kinds that take it.
	var replicasPtr *int
	if a.Kind != "cron" {
		replicas, err := strconv.Atoi(r.PostFormValue("replicas"))
		if err != nil {
			http.Error(w, "invalid replicas", http.StatusBadRequest)
			return
		}
		replicasPtr = &replicas
	}
	cpu, err := parseCPUMilli(r.PostFormValue("cpu"))
	if err != nil {
		http.Error(w, "cpu: "+err.Error(), http.StatusBadRequest)
		return
	}
	mem, err := parseMemoryMB(r.PostFormValue("memory"))
	if err != nil {
		http.Error(w, "memory: "+err.Error(), http.StatusBadRequest)
		return
	}

	env, ok := s.uiAppEnv(w, a)
	if !ok {
		return
	}
	if _, err := s.scaleApp(r.Context(), p, env, a, scaleChange{Replicas: replicasPtr, CPUMilli: &cpu, MemoryMB: &mem}); err != nil {
		var re *scaleReplicasError
		var ke *kindMismatchError
		switch {
		case errors.Is(err, errAppEjected):
			http.Error(w, err.Error(), http.StatusConflict)
		case errors.Is(err, errKubeUnavailable):
			http.Error(w, "kubernetes is not configured", http.StatusServiceUnavailable)
		case errors.As(err, &ke):
			http.Error(w, ke.Error(), http.StatusBadRequest)
		case errors.As(err, &re):
			http.Error(w, re.Error(), http.StatusBadRequest)
		default:
			log.Printf("ui scale app: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
		}
		return
	}
	flash(w, "ok", "scaled")
	uiRedirect(w, r, p, a)
}

// handleUIAutoscale is autoscaleApp's UI twin: an "off=1" field disables
// (0/0/0); otherwise min/max/cpu configure the HPA. Same shared core and
// error-mapping shape as handleUIScale.
func (s *server) handleUIAutoscale(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.uiProjectWrite(w, r, u)
	if !ok {
		return
	}
	a, ok := s.uiApp(w, r, p)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}

	var min, max, cpu int
	var err error
	if r.PostFormValue("off") == "" {
		min, err = strconv.Atoi(r.PostFormValue("min"))
		if err != nil {
			http.Error(w, "invalid min", http.StatusBadRequest)
			return
		}
		max, err = strconv.Atoi(r.PostFormValue("max"))
		if err != nil {
			http.Error(w, "invalid max", http.StatusBadRequest)
			return
		}
		cpu, err = strconv.Atoi(r.PostFormValue("cpu"))
		if err != nil {
			http.Error(w, "invalid cpu", http.StatusBadRequest)
			return
		}
	}

	env, ok := s.uiAppEnv(w, a)
	if !ok {
		return
	}
	if _, err := s.autoscaleApp(r.Context(), p, env, a, min, max, cpu); err != nil {
		var re *scaleReplicasError
		var ke *kindMismatchError
		var rc *volumeReplicaConflictError
		switch {
		case errors.Is(err, errAppEjected):
			http.Error(w, err.Error(), http.StatusConflict)
		case errors.Is(err, errKubeUnavailable):
			http.Error(w, "kubernetes is not configured", http.StatusServiceUnavailable)
		case errors.As(err, &ke):
			http.Error(w, ke.Error(), http.StatusBadRequest)
		case errors.As(err, &rc):
			http.Error(w, rc.Error(), http.StatusConflict)
		case errors.As(err, &re):
			http.Error(w, re.Error(), http.StatusBadRequest)
		default:
			log.Printf("ui autoscale app: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
		}
		return
	}
	if min > 0 {
		flash(w, "ok", "autoscale saved")
	} else {
		flash(w, "ok", "autoscale off")
	}
	uiRedirect(w, r, p, a)
}

// handleUIRunCreate is startRun's UI twin: same shared core, redirect
// instead of a 202 JSON body. A missing live deployment, a bad nodes/
// framework override, or an over-budget nodes bump redirects back to the
// app page with ?err= (matches handleUICreateApp's deploy-failure idiom)
// instead of erroring the whole request.
func (s *server) handleUIRunCreate(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.uiProjectWrite(w, r, u)
	if !ok {
		return
	}
	a, ok := s.uiApp(w, r, p)
	if !ok {
		return
	}
	if a.Kind != "job" {
		http.Error(w, "runs are only valid for job apps", http.StatusBadRequest)
		return
	}
	if a.Ejected {
		http.Error(w, errAppEjected.Error(), http.StatusConflict)
		return
	}
	if s.kube == nil {
		http.Error(w, "kubernetes is not configured", http.StatusServiceUnavailable)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	opts, err := parseUIRunOpts(r)
	if err != nil {
		http.Redirect(w, r, "/ui/projects/"+p.Name+"/apps/"+a.Name+"?err="+url.QueryEscape(err.Error()), http.StatusSeeOther)
		return
	}

	env, ok := s.uiAppEnv(w, a)
	if !ok {
		return
	}
	if _, err := s.startRun(r.Context(), p, env, a, opts); err != nil {
		if errors.Is(err, errNotDeployed) || errors.Is(err, errRunOverBudget) {
			http.Redirect(w, r, "/ui/projects/"+p.Name+"/apps/"+a.Name+"?err="+url.QueryEscape(err.Error()), http.StatusSeeOther)
			return
		}
		log.Printf("ui start run: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	flash(w, "ok", "run started")
	uiRedirect(w, r, p, a)
}

// parseUIRunOpts reads the run-now form's optional nodes/framework
// overrides — both left blank means the zero-value runOpts (app defaults).
// framework is validated against render.TrainFrameworks here so a bad value
// fails before startRun's gpu-budget check runs, same as handleCreateRun's
// JSON-body validation.
func parseUIRunOpts(r *http.Request) (runOpts, error) {
	var opts runOpts
	if v := r.PostFormValue("nodes"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return runOpts{}, errors.New("invalid nodes")
		}
		opts.Nodes = n
	}
	framework := r.PostFormValue("framework")
	if framework != "" && !slices.Contains(render.TrainFrameworks, framework) {
		return runOpts{}, fmt.Errorf("unknown framework %q (valid: %s)", framework, strings.Join(render.TrainFrameworks, ", "))
	}
	opts.Framework = framework
	return opts, nil
}

// handleUITraining is setAppTraining's UI twin: same shared store+budget
// core as handleSetTraining, form-POST instead of JSON, redirect back to
// the app page with ?err= on failure (mirrors handleUIGPUQuota).
func (s *server) handleUITraining(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.uiProjectWrite(w, r, u)
	if !ok {
		return
	}
	a, ok := s.uiApp(w, r, p)
	if !ok {
		return
	}
	if a.Kind != "job" {
		http.Error(w, "training defaults are only valid for job apps", http.StatusBadRequest)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	nodes, err := strconv.Atoi(r.PostFormValue("nodes"))
	if err != nil {
		http.Redirect(w, r, "/ui/projects/"+p.Name+"/apps/"+a.Name+"?err="+url.QueryEscape("invalid nodes"), http.StatusSeeOther)
		return
	}
	framework := r.PostFormValue("framework")
	if err := s.setAppTraining(p, a, nodes, framework); err != nil {
		http.Redirect(w, r, "/ui/projects/"+p.Name+"/apps/"+a.Name+"?err="+url.QueryEscape(err.Error()), http.StatusSeeOther)
		return
	}
	flash(w, "ok", "training defaults saved")
	uiRedirect(w, r, p, a)
}

func (s *server) handleUIHealth(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.uiProjectWrite(w, r, u)
	if !ok {
		return
	}
	a, ok := s.uiApp(w, r, p)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}

	env, ok := s.uiAppEnv(w, a)
	if !ok {
		return
	}
	if err := s.setAppHealth(r.Context(), p, env, a, r.PostFormValue("health_path")); err != nil {
		switch {
		case errors.Is(err, errAppEjected):
			http.Error(w, err.Error(), http.StatusConflict)
		default:
			http.Error(w, err.Error(), http.StatusBadRequest)
		}
		return
	}
	flash(w, "ok", "health check saved")
	uiRedirect(w, r, p, a)
}

// handleUIDeploy starts a git build for git-source apps via the same
// deployGitApp core handleDeployApp's git branch uses. A non-git app is a
// no-op redirect (the template hides the "Deploy from git" button, so this
// only guards against a direct POST); missing kube/build-source surface as
// 503 exactly like the API path, never as a silent redirect.
func (s *server) handleUIDeploy(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.uiProjectWrite(w, r, u)
	if !ok {
		return
	}
	a, ok := s.uiApp(w, r, p)
	if !ok {
		return
	}

	if a.SourceType != "git" {
		uiRedirect(w, r, p, a)
		return
	}

	env, ok := s.uiAppEnv(w, a)
	if !ok {
		return
	}
	if _, err := s.deployGitApp(p, env, a, u.ID); err != nil {
		switch {
		case errors.Is(err, errKubeUnavailable):
			http.Error(w, "kubernetes is not configured", http.StatusServiceUnavailable)
		case errors.Is(err, errBuildUnavailable):
			http.Error(w, "server has no data directory configured", http.StatusServiceUnavailable)
		default:
			log.Printf("ui deploy: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
		}
		return
	}
	flash(w, "ok", "deploy started")
	uiRedirect(w, r, p, a)
}

// handleUIRollback is handleRollback's UI twin: same shared s.rollback
// core, plain-text statuses instead of a JSON envelope, redirect instead of
// a 202 body. Guards on s.kube itself (rather than relying on a sentinel
// out of s.rollback) because applyImageDeploy would otherwise panic on a
// nil client — mirroring handleRollback's requireKube check.
func (s *server) handleUIRollback(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.uiProjectWrite(w, r, u)
	if !ok {
		return
	}
	a, ok := s.uiApp(w, r, p)
	if !ok {
		return
	}
	if s.kube == nil {
		http.Error(w, "kubernetes is not configured", http.StatusServiceUnavailable)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	// Unlike the JSON API, the UI form's hidden deploy_id field is always
	// populated by app.html — an empty or malformed value here is a bad
	// request, same as before ids became opaque nanoids (ParseInt of "" or
	// garbage failed the same way).
	deployID := r.PostFormValue("deploy_id")
	if !validDeployID(deployID) {
		http.Error(w, "invalid deploy_id", http.StatusBadRequest)
		return
	}

	env, ok := s.uiAppEnv(w, a)
	if !ok {
		return
	}
	if _, err := s.rollback(r.Context(), p, env, a, u, deployID); err != nil {
		var missing *errImageMissing
		var regErr *errRegistryCheck
		switch {
		case errors.Is(err, errAppEjected):
			http.Error(w, err.Error(), http.StatusConflict)
		case errors.Is(err, store.ErrNotFound):
			http.Error(w, "no such deployment for this app", http.StatusNotFound)
		case errors.Is(err, errNoRollbackTarget):
			http.Error(w, errNoRollbackTarget.Error(), http.StatusConflict)
		case errors.As(err, &missing):
			http.Error(w, missing.Error(), http.StatusConflict)
		case errors.As(err, &regErr):
			http.Error(w, "could not verify image in registry", http.StatusServiceUnavailable)
		default:
			log.Printf("ui rollback: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
		}
		return
	}
	flash(w, "ok", "rollback started")
	uiRedirect(w, r, p, a)
}

// handleUIAppDestroy is handleDeleteApp's UI twin: same destroyApp core,
// redirect back to the project page instead of a 204.
func (s *server) handleUIAppDestroy(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.uiProjectWrite(w, r, u)
	if !ok {
		return
	}
	a, ok := s.uiApp(w, r, p)
	if !ok {
		return
	}
	if !a.Ejected && s.kube == nil {
		http.Error(w, "kubernetes is not configured", http.StatusServiceUnavailable)
		return
	}
	env, ok := s.uiAppEnv(w, a)
	if !ok {
		return
	}
	if err := s.destroyApp(r.Context(), p, env, a); err != nil {
		log.Printf("ui destroy app: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	flash(w, "ok", "app destroyed")
	http.Redirect(w, r, "/ui/projects/"+p.Name, http.StatusSeeOther)
}

// handleUIEject is handleEjectApp's UI twin: same ejectApp core, redirect
// back to the app page (whose ejected banner then tells the story) instead
// of returning the rendered YAML in the body.
func (s *server) handleUIEject(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.uiProjectWrite(w, r, u)
	if !ok {
		return
	}
	a, ok := s.uiApp(w, r, p)
	if !ok {
		return
	}
	if s.refuseEjected(w, a) {
		return
	}
	env, ok := s.uiAppEnv(w, a)
	if !ok {
		return
	}
	if _, _, err := s.ejectApp(p, env, a); err != nil {
		log.Printf("ui eject %s: %v", a.Name, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	flash(w, "ok", "app ejected")
	uiRedirect(w, r, p, a)
}
