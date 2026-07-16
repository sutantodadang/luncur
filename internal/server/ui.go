package server

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"log"
	"net/http"
	"net/url"

	"github.com/sutantodadang/luncur/internal/kube"
	"github.com/sutantodadang/luncur/internal/store"
)

const sessionCookie = "luncur_session"
const csrfCookie = "luncur_csrf"

func (s *server) uiRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /ui/static/{file}", s.handleUIStatic)
	mux.HandleFunc("GET /ui/login", s.handleUILoginPage)
	mux.HandleFunc("POST /ui/login", s.rateLimited(s.handleUILogin))
	mux.HandleFunc("POST /ui/logout", s.handleUILogout)
	mux.HandleFunc("GET /ui/register", s.handleUIRegisterPage)
	mux.HandleFunc("POST /ui/register", s.handleUIRegister)
	mux.HandleFunc("GET /ui/users", s.uiPage(s.handleUIUsers))
	mux.HandleFunc("GET /ui/audit", s.uiPage(s.handleUIAudit))
	mux.HandleFunc("POST /ui/users/invite", s.uiPage(s.handleUIInviteCreate))
	mux.HandleFunc("POST /ui/users/invite/revoke", s.uiPage(s.handleUIInviteRevoke))
	mux.HandleFunc("POST /ui/users/delete", s.uiPage(s.handleUIUserDelete))
	mux.HandleFunc("POST /ui/users/password", s.uiPage(s.handleUIUserPassword))
	mux.HandleFunc("GET /ui/account", s.uiPage(s.handleUIAccount))
	mux.HandleFunc("POST /ui/account/password", s.uiPage(s.handleUIAccountPassword))
	mux.HandleFunc("POST /ui/account/email", s.uiPage(s.handleUIAccountEmail))
	mux.HandleFunc("GET /ui/tokens", s.uiPage(s.handleUITokens))
	mux.HandleFunc("POST /ui/tokens/revoke", s.uiPage(s.handleUITokenRevoke))
	mux.HandleFunc("GET /ui/sshkeys", s.uiPage(s.handleUISSHKeys))
	mux.HandleFunc("POST /ui/sshkeys", s.uiPage(s.handleUISSHKeyAdd))
	mux.HandleFunc("POST /ui/sshkeys/delete", s.uiPage(s.handleUISSHKeyDelete))
	mux.HandleFunc("GET /ui/backups", s.uiPage(s.handleUIBackups))
	mux.HandleFunc("POST /ui/backups", s.uiPage(s.handleUIBackupCreate))
	mux.HandleFunc("POST /ui/backups/prune", s.uiPage(s.handleUIBackupPrune))
	mux.HandleFunc("GET /ui/settings", s.uiPage(s.handleUISettings))
	mux.HandleFunc("POST /ui/settings", s.uiPage(s.handleUISettingsSet))
	mux.HandleFunc("POST /ui/settings/update", s.uiPage(s.handleUISettingsUpdate))
	mux.HandleFunc("POST /ui/registry-gc", s.uiPage(s.handleUIRegistryGC))
	mux.HandleFunc("GET /ui/doctor", s.uiPage(s.handleUIDoctor))
	mux.HandleFunc("GET /ui/nodes", s.uiPage(s.handleUINodes))
	mux.HandleFunc("POST /ui/gpu/key", s.uiPage(s.handleUIGPUKey))
	mux.HandleFunc("POST /ui/gpu/key/nebius", s.uiPage(s.handleUIGPUKeyNebius))
	mux.HandleFunc("POST /ui/gpu/rent", s.uiPage(s.handleUIGPURent))
	mux.HandleFunc("POST /ui/gpu/{id}/stop", s.uiPage(s.handleUIGPUStop))
	mux.HandleFunc("GET /ui/", s.uiPage(s.handleUIProjects))
	mux.HandleFunc("POST /ui/projects", s.uiPage(s.handleUIProjectCreate))
	mux.HandleFunc("POST /ui/projects/{project}/members", s.uiPage(s.handleUIAddMember))
	mux.HandleFunc("POST /ui/projects/{project}/members/remove", s.uiPage(s.handleUIMemberRemove))
	mux.HandleFunc("POST /ui/projects/{project}/rename", s.uiPage(s.handleUIProjectRename))
	mux.HandleFunc("POST /ui/projects/{project}/delete", s.uiPage(s.handleUIProjectDelete))
	mux.HandleFunc("GET /ui/projects/{project}", s.uiPage(s.handleUIApps))
	// Env-scoped twin of the project page: same handler, resolves env from
	// the path instead of defaulting (Task 11's environment selector).
	mux.HandleFunc("GET /ui/projects/{project}/envs/{env}", s.uiPage(s.handleUIApps))
	mux.HandleFunc("POST /ui/projects/{project}/apps", s.uiPage(s.handleUICreateApp))
	mux.HandleFunc("POST /ui/projects/{project}/gpu-quota", s.uiPage(s.handleUIGPUQuota))
	mux.HandleFunc("POST /ui/projects/{project}/quota", s.uiPage(s.handleUIQuota))
	mux.HandleFunc("POST /ui/projects/{project}/addons/upgrade", s.uiPage(s.handleUIAddonUpgrade))
	mux.HandleFunc("GET /ui/projects/{project}/addons/url", s.uiPage(s.handleUIAddonURL))
	mux.HandleFunc("GET /ui/projects/{project}/pipelines/{name}", s.uiPage(s.handleUIPipeline))
	mux.HandleFunc("POST /ui/projects/{project}/pipelines", s.uiPage(s.handleUIPipelineCreate))
	mux.HandleFunc("POST /ui/projects/{project}/pipelines/{name}", s.uiPage(s.handleUIPipelineUpdate))
	mux.HandleFunc("POST /ui/projects/{project}/pipelines/{name}/run", s.uiPage(s.handleUIPipelineRun))
	mux.HandleFunc("POST /ui/projects/{project}/pipelines/{name}/webhook-secret", s.uiPage(s.handleUIPipelineWebhookSecret))
	mux.HandleFunc("POST /ui/projects/{project}/pipelines/{name}/runs/{id}/stop", s.uiPage(s.handleUIPipelineRunStop))
	mux.HandleFunc("GET /ui/projects/{project}/pipelines/{name}/runs/{id}/steps", s.uiPage(s.handleUIPipelineRunSteps))
	mux.HandleFunc("GET /ui/projects/{project}/apps/{app}", s.uiPage(s.handleUIApp))
	// Env-scoped twin: disambiguates an app name that exists in more than
	// one of the project's environments (uiApp resolves via this path
	// value, defaulting to the project's default env when absent).
	mux.HandleFunc("GET /ui/projects/{project}/envs/{env}/apps/{app}", s.uiPage(s.handleUIApp))
	mux.HandleFunc("GET /ui/projects/{project}/apps/{app}/open", s.uiPage(s.handleUIAppOpen))
	mux.HandleFunc("GET /ui/projects/{project}/apps/{app}/chip", s.uiPage(s.handleUIChip))
	mux.HandleFunc("GET /ui/projects/{project}/apps/{app}/chart", s.uiPage(s.handleUIAppChart))
	mux.HandleFunc("GET /ui/nodes/charts", s.uiPage(s.handleUINodeCharts))
	mux.HandleFunc("POST /ui/projects/{project}/apps/{app}/destroy", s.uiPage(s.handleUIAppDestroy))
	mux.HandleFunc("POST /ui/projects/{project}/apps/{app}/eject", s.uiPage(s.handleUIEject))
	mux.HandleFunc("POST /ui/projects/{project}/apps/{app}/domains/retry", s.uiPage(s.handleUIDomainRetry))
	mux.HandleFunc("POST /ui/projects/{project}/apps/{app}/scale", s.uiPage(s.handleUIScale))
	mux.HandleFunc("POST /ui/projects/{project}/apps/{app}/autoscale", s.uiPage(s.handleUIAutoscale))
	mux.HandleFunc("POST /ui/projects/{project}/apps/{app}/runs", s.uiPage(s.handleUIRunCreate))
	mux.HandleFunc("POST /ui/projects/{project}/apps/{app}/training", s.uiPage(s.handleUITraining))
	mux.HandleFunc("POST /ui/projects/{project}/apps/{app}/sweeps", s.uiPage(s.handleUISweepCreate))
	mux.HandleFunc("POST /ui/projects/{project}/apps/{app}/sweeps/{id}/stop", s.uiPage(s.handleUISweepStop))
	mux.HandleFunc("GET /ui/projects/{project}/apps/{app}/sweeps/{id}/trials", s.uiPage(s.handleUISweepTrials))
	mux.HandleFunc("POST /ui/projects/{project}/apps/{app}/health", s.uiPage(s.handleUIHealth))
	mux.HandleFunc("POST /ui/projects/{project}/apps/{app}/webhook", s.uiPage(s.handleUIWebhookEnable))
	mux.HandleFunc("POST /ui/projects/{project}/apps/{app}/webhook/disable", s.uiPage(s.handleUIWebhookDisable))
	mux.HandleFunc("POST /ui/projects/{project}/apps/{app}/env", s.uiPage(s.handleUIEnvSet))
	mux.HandleFunc("POST /ui/projects/{project}/apps/{app}/env/bulk", s.uiPage(s.handleUIEnvBulk))
	mux.HandleFunc("POST /ui/projects/{project}/apps/{app}/env/delete", s.uiPage(s.handleUIEnvUnset))
	mux.HandleFunc("POST /ui/projects/{project}/apps/{app}/redeploy", s.uiPage(s.handleUIRedeploy))
	mux.HandleFunc("POST /ui/projects/{project}/apps/{app}/pause", s.uiPage(s.handleUIPauseCron))
	mux.HandleFunc("POST /ui/projects/{project}/apps/{app}/resume", s.uiPage(s.handleUIResumeCron))
	mux.HandleFunc("POST /ui/projects/{project}/apps/{app}/run-now", s.uiPage(s.handleUITriggerCron))
	mux.HandleFunc("POST /ui/projects/{project}/apps/{app}/git-token", s.uiPage(s.handleUIGitTokenSet))
	mux.HandleFunc("POST /ui/projects/{project}/apps/{app}/git-token/clear", s.uiPage(s.handleUIGitTokenClear))
	mux.HandleFunc("POST /ui/projects/{project}/apps/{app}/domains", s.uiPage(s.handleUIDomainAdd))
	mux.HandleFunc("POST /ui/projects/{project}/apps/{app}/domains/delete", s.uiPage(s.handleUIDomainDelete))
	mux.HandleFunc("POST /ui/projects/{project}/apps/{app}/volumes", s.uiPage(s.handleUIVolumeAdd))
	mux.HandleFunc("POST /ui/projects/{project}/apps/{app}/volumes/remove", s.uiPage(s.handleUIVolumeRemove))
	mux.HandleFunc("POST /ui/projects/{project}/addons", s.uiPage(s.handleUIAddonCreate))
	mux.HandleFunc("POST /ui/projects/{project}/addons/delete", s.uiPage(s.handleUIAddonDelete))
	mux.HandleFunc("POST /ui/projects/{project}/previews/delete", s.uiPage(s.handleUIPreviewDelete))
	mux.HandleFunc("POST /ui/projects/{project}/apps/{app}/addons/attach", s.uiPage(s.handleUIAddonAttach))
	mux.HandleFunc("POST /ui/projects/{project}/apps/{app}/addons/detach", s.uiPage(s.handleUIAddonDetach))
	mux.HandleFunc("POST /ui/projects/{project}/apps/{app}/deploy", s.uiPage(s.handleUIDeploy))
	mux.HandleFunc("POST /ui/projects/{project}/apps/{app}/rollback", s.uiPage(s.handleUIRollback))
	mux.HandleFunc("POST /ui/projects/{project}/apps/{app}/adopt", s.uiPage(s.handleUIAdopt))
	mux.HandleFunc("GET /ui/projects/{project}/apps/{app}/edit/{kind}", s.uiPage(s.handleUIEditGet))
	mux.HandleFunc("POST /ui/projects/{project}/apps/{app}/edit/{kind}", s.uiPage(s.handleUIEditPost))

	// mlflow UI proxy: GET serves its static UI assets, POST carries its
	// REST API calls (mlflow's own API is POST-only for mutations), session
	// auth only — mlflow requests carry no CSRF token; the SameSite=Strict
	// cookie covers cross-site POSTs. Registered per-method (rather than a
	// bare method-less pattern) because net/http's ServeMux rejects a
	// method-less specific-path pattern that overlaps "GET /ui/"'s
	// catch-all subtree as a static conflict.
	mlflowProxy := func(w http.ResponseWriter, r *http.Request) {
		u, ok := s.uiUser(r)
		if !ok {
			http.Redirect(w, r, "/ui/login", http.StatusSeeOther)
			return
		}
		s.handleUIMlflow(w, r, u)
	}
	mux.HandleFunc("GET /ui/mlflow/{ns}/{name}/{rest...}", mlflowProxy)
	mux.HandleFunc("POST /ui/mlflow/{ns}/{name}/{rest...}", mlflowProxy)
}

// editableKinds are the manifest kinds the YAML editor accepts — the same
// set render.dataStructFor (via SetOverride) understands.
var editableKinds = map[string]bool{"Deployment": true, "Service": true, "Ingress": true, "CronJob": true}

// uiUser resolves the session cookie to a user.
func (s *server) uiUser(r *http.Request) (store.User, bool) {
	ck, err := r.Cookie(sessionCookie)
	if err != nil {
		return store.User{}, false
	}
	u, err := s.st.UserForToken(ck.Value)
	if err != nil {
		return store.User{}, false
	}
	return u, true
}

// uiPage is authed's HTML twin: unauthenticated browsers get redirected to
// the login form instead of a JSON 401.
func (s *server) uiPage(next func(http.ResponseWriter, *http.Request, store.User)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u, ok := s.uiUser(r)
		if !ok {
			http.Redirect(w, r, "/ui/login", http.StatusSeeOther)
			return
		}
		if r.Method == http.MethodPost && !s.checkCSRF(w, r) {
			return
		}
		if info := auditFrom(r.Context()); info != nil {
			info.Email = u.Email
			info.Pattern = r.Pattern
		}
		next(w, r, u)
	}
}

// csrf returns the request's CSRF token, minting the cookie on first use.
// Double-submit pattern: the value is only ever compared against the same
// browser's form field, so no server-side state is needed.
func (s *server) csrf(w http.ResponseWriter, r *http.Request) string {
	if ck, err := r.Cookie(csrfCookie); err == nil && ck.Value != "" {
		return ck.Value
	}
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		log.Printf("csrf rand: %v", err)
		return ""
	}
	v := hex.EncodeToString(raw)
	http.SetCookie(w, &http.Cookie{
		Name: csrfCookie, Value: v, Path: "/",
		HttpOnly: true, SameSite: http.SameSiteStrictMode,
	})
	return v
}

// checkCSRF verifies a POST's _csrf field against the cookie. Writes the
// 403 itself so callers can just return.
func (s *server) checkCSRF(w http.ResponseWriter, r *http.Request) bool {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return false
	}
	ck, err := r.Cookie(csrfCookie)
	if err != nil || ck.Value == "" ||
		subtle.ConstantTimeCompare([]byte(ck.Value), []byte(r.PostFormValue("_csrf"))) != 1 {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)
		return false
	}
	return true
}

func (s *server) renderPage(w http.ResponseWriter, page string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.ExecuteTemplate(w, page, data); err != nil {
		log.Printf("render %s: %v", page, err)
	}
}

// uiProject is requireProject's UI twin: it 404s with plain text instead of
// a JSON envelope (this is browser, not API, context). Enforces the exact
// same membership rule as requireProject — admins may access any project,
// members must be in project_members — so a member can never see, via
// either surface, a project they don't belong to.
func (s *server) uiProject(w http.ResponseWriter, r *http.Request, u store.User) (store.Project, bool) {
	p, err := s.st.GetProject(r.PathValue("project"))
	if errors.Is(err, store.ErrNotFound) {
		http.Error(w, "not found", http.StatusNotFound)
		return store.Project{}, false
	}
	if err != nil {
		log.Printf("ui get project: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return store.Project{}, false
	}
	if u.Role != "admin" {
		ok, err := s.st.IsMember(p.ID, u.ID)
		if err != nil || !ok {
			// Not-a-member answers identically to not-found: a 403 here
			// would leak the project's existence to non-members.
			http.Error(w, "not found", http.StatusNotFound)
			return store.Project{}, false
		}
	}
	return p, true
}

// uiProjectWrite is uiProject plus write authorization: global admins and
// role=member pass; role=viewer gets a plain-text 403, matching the CSRF
// check's precedent for how this UI surface rejects a blocked write (as
// opposed to uiProject/uiAdmin's leak-nothing 404, which is about hiding
// existence rather than declining an otherwise-visible action).
func (s *server) uiProjectWrite(w http.ResponseWriter, r *http.Request, u store.User) (store.Project, bool) {
	p, ok := s.uiProject(w, r, u)
	if !ok {
		return p, false
	}
	if u.Role == "admin" {
		return p, true
	}
	role, err := s.st.MemberRole(p.ID, u.ID)
	if err == nil && role == "viewer" {
		http.Error(w, "viewers cannot modify this project", http.StatusForbidden)
		return p, false
	}
	return p, true
}

// uiAdmin 404s non-admins (leak-nothing, same policy as uiProject).
func (s *server) uiAdmin(w http.ResponseWriter, u store.User) bool {
	if u.Role != "admin" {
		http.Error(w, "not found", http.StatusNotFound)
		return false
	}
	return true
}

// uiApp is requireApp's UI twin: 404s with plain text instead of a JSON
// envelope. Resolves the app within r.PathValue("env") (the project's
// default environment when absent, e.g. every legacy /apps/{app} route),
// mirroring requireApp's own env-scoped lookup — necessary since two apps
// may now share a name across environments in the same project (Task 11),
// so a plain project+name lookup would be ambiguous.
func (s *server) uiApp(w http.ResponseWriter, r *http.Request, p store.Project) (store.App, bool) {
	env, ok := s.uiEnv(w, r, p)
	if !ok {
		return store.App{}, false
	}
	a, err := s.st.GetAppInEnv(env.ID, r.PathValue("app"))
	if errors.Is(err, store.ErrNotFound) {
		http.Error(w, "not found", http.StatusNotFound)
		return store.App{}, false
	}
	if err != nil {
		log.Printf("ui get app: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return store.App{}, false
	}
	return a, true
}

// uiAppEnv resolves a's own environment (set at create — see
// handleCreateApp/handleUICreateApp) for UI handlers that call an
// env-scoped core (syncIfLive, scaleApp, addDomain, ...). 500s with plain
// text on failure, matching uiApp's error style.
func (s *server) uiAppEnv(w http.ResponseWriter, a store.App) (store.Environment, bool) {
	env, err := s.st.GetEnvironmentByID(a.EnvironmentID)
	if err != nil {
		log.Printf("ui get environment for app %s: %v", a.Name, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return store.Environment{}, false
	}
	return env, true
}

// uiDefaultEnv resolves p's default environment, for UI handlers that act on
// an addon (no app in scope to hang uiAppEnv off of) — today that's always
// the production environment.
func (s *server) uiDefaultEnv(w http.ResponseWriter, p store.Project) (store.Environment, bool) {
	env, err := s.st.GetEnvironment(p.ID, p.DefaultEnv)
	if err != nil {
		log.Printf("ui get default environment for project %s: %v", p.Name, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return store.Environment{}, false
	}
	return env, true
}

// uiEnv resolves r.PathValue("env") to one of p's environments, falling
// back to p.DefaultEnv when the path carries none (every legacy route, plus
// the plain project page) — the UI's twin of requireEnv's own "" ->
// default-env fallback. 404s with plain text on an unknown, explicitly
// requested env name.
//
// A project with no environments row at all (created directly via
// store.CreateProject, bypassing handleCreateProject's seeding — legacy
// fixtures, or any project older than the environments migration that
// backfillEnvironments hasn't reached) still resolves the *legacy*
// (env-less) request: apps created the same way default to
// environment_id=0, so a synthetic Environment{ID:0, Namespace:p.Namespace}
// reproduces exactly the pre-environments behavior every UI handler already
// assumed. An explicit /envs/{env} request on such a project still 404s —
// there is genuinely no such environment to view.
func (s *server) uiEnv(w http.ResponseWriter, r *http.Request, p store.Project) (store.Environment, bool) {
	name := r.PathValue("env")
	fellBackToDefault := name == ""
	if fellBackToDefault {
		name = p.DefaultEnv
	}
	env, err := s.st.GetEnvironment(p.ID, name)
	if errors.Is(err, store.ErrNotFound) {
		if fellBackToDefault {
			return store.Environment{Name: p.DefaultEnv, Namespace: p.Namespace, IsDefault: true}, true
		}
		http.Error(w, "not found", http.StatusNotFound)
		return store.Environment{}, false
	}
	if err != nil {
		log.Printf("ui get environment %s/%s: %v", p.Name, name, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return store.Environment{}, false
	}
	return env, true
}

// uiEnvChip is the environment selector's per-option view model — reused
// both for the current env (a single value, "Env" in the app/project page
// data) and the full list to switch between ("Envs"). Default envs get no
// special badge; Preview marks the chip a non-default env needs to look
// visually distinct (chip-warn vs chip-muted — see app.html/apps.html).
type uiEnvChip struct {
	Name    string
	Default bool
	Preview bool
}

// uiEnvChipFrom builds one uiEnvChip from a store.Environment.
func uiEnvChipFrom(e store.Environment) uiEnvChip {
	return uiEnvChip{Name: e.Name, Default: e.IsDefault, Preview: e.Kind == "preview"}
}

// uiEnvChips lists every environment on p, for the selector's option list.
func (s *server) uiEnvChips(p store.Project) ([]uiEnvChip, error) {
	envs, err := s.st.ListEnvironments(p.ID)
	if err != nil {
		return nil, err
	}
	out := make([]uiEnvChip, 0, len(envs))
	for _, e := range envs {
		out = append(out, uiEnvChipFrom(e))
	}
	return out, nil
}

// uiAddon is requireAddon's UI twin: 404s with plain text instead of a JSON
// envelope.
func (s *server) uiAddon(w http.ResponseWriter, p store.Project, name string) (store.Addon, bool) {
	a, err := s.st.GetAddon(p.ID, name)
	if errors.Is(err, store.ErrNotFound) {
		http.Error(w, "not found", http.StatusNotFound)
		return store.Addon{}, false
	}
	if err != nil {
		log.Printf("ui get addon: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return store.Addon{}, false
	}
	return a, true
}

// uiPreviewApp is one preview environment's cloned app, as shown in the
// Previews card's app-URL list.
type uiPreviewApp struct {
	Name string
	URL  string
}

// uiPreviewRow is a preview environment's project-page view model — its
// source branch, idle-activity timestamp (the clock reapPreviews' TTL
// sweep reads), and its cloned apps' URLs. Mirrors previewJSON
// (preview.go), the REST API's equivalent shape.
type uiPreviewRow struct {
	Name         string
	SourceBranch string
	LastActiveAt string
	Apps         []uiPreviewApp
}

// uiPreviewRows lists every preview environment on p (kind=='preview' only
// — its standing environments are the env selector's concern, uiEnvChips),
// for the project page's Previews card.
func (s *server) uiPreviewRows(p store.Project) ([]uiPreviewRow, error) {
	envs, err := s.st.ListEnvironments(p.ID)
	if err != nil {
		return nil, err
	}
	rows := make([]uiPreviewRow, 0)
	for _, e := range envs {
		if e.Kind != "preview" {
			continue
		}
		apps, err := s.st.ListAppsInEnv(e.ID)
		if err != nil {
			return nil, err
		}
		appRows := make([]uiPreviewApp, 0, len(apps))
		for _, a := range apps {
			u := s.appURLForEnv(a, e.Name, p.DefaultEnv)
			if a.Internal {
				u = internalURLFor(a.Name, e.Namespace)
			}
			appRows = append(appRows, uiPreviewApp{Name: a.Name, URL: u})
		}
		rows = append(rows, uiPreviewRow{
			Name: e.Name, SourceBranch: e.SourceBranch, LastActiveAt: e.LastActiveAt, Apps: appRows,
		})
	}
	return rows, nil
}

// uiProjectCard is projects.html's per-card view model: the project plus its
// app-count summary (derived the same way handleUIApps derives per-app
// status: LatestDeployment per app, bucketed into live/building/failed) and
// its member count.
type uiProjectCard struct {
	Name      string
	Namespace string
	Apps      int
	Live      int
	Building  int
	Failed    int
	Members   int
}

// uiAppRow is apps.html's per-row view model: the store.App plus its
// derived public URL and latest-deploy status (empty when the app has never
// been deployed — the template renders a "no deploys" chip for that case).
type uiAppRow struct {
	Name        string
	Kind        string
	Schedule    string
	Replicas    int
	URL         string
	Internal    bool
	InternalURL string
	Ejected     bool
	Status      string
}

// uiChipData is the "statuschip" fragment's view model: enough to render
// the chip itself plus, while the deploy is still in flight, the route the
// fragment polls to re-fetch its own next state.
type uiChipData struct {
	ProjectName string
	AppName     string
	Status      string
	Building    bool
}

// uiDeployRow is app.html's Deploys-card view model: store.Deployment plus
// the image tag (the part of ImageRef after its last ":", full ref kept for
// the row's title attribute) and an actor placeholder. No store surface maps
// a user id to an email cheaply yet, so every row shows "-" for actor rather
// than adding one just for this column.
type uiDeployRow struct {
	ID                string
	Seq               int64
	Status            string
	ImageRef          string
	ImageTag          string
	CreatedAt         string
	RolledBackFromSeq int64 // 0 = not a rollback (or source fell out of history)
	Actor             string
}

// uiDeployRows builds the Deploys card's view model from ListDeployments'
// newest-first history, capped at limit rows. RolledBackFrom is an opaque
// id now, never shown to a user — the seq lookup map is built from the
// full (unclipped, up to 50-row) history so a rollback's source deploy
// resolves to its human-facing seq even when it falls outside the limit
// rows actually rendered.
// uiRunRow is app.html's Runs-card view model: store.JobRun with its
// nullable fields flattened to plain strings ("" when unset) for simple
// template rendering.
type uiRunRow struct {
	ID         int64
	Status     string
	Nodes      int
	ExitCode   string
	StartedAt  string
	FinishedAt string
}

// uiCronRunRow is app.html's cron Runs-card view model: kube.CronRunInfo
// as-is, aliased so the template package doesn't need to import kube.
type uiCronRunRow = kube.CronRunInfo

// uiRedirect sends the browser back to the app detail page an action
// (scale/env/deploy) was posted from.
func uiRedirect(w http.ResponseWriter, r *http.Request, p store.Project, a store.App) {
	http.Redirect(w, r, "/ui/projects/"+p.Name+"/apps/"+a.Name, http.StatusSeeOther)
}

// flash queues a one-shot toast shown by base.html's foot script on the
// next page load: cookie value "<kind>|<msg>", read+cleared by JS.
func flash(w http.ResponseWriter, kind, msg string) {
	http.SetCookie(w, &http.Cookie{
		Name: "luncur_flash", Value: url.QueryEscape(kind + "|" + msg),
		Path: "/", MaxAge: 15, SameSite: http.SameSiteLaxMode,
	})
}

// firstNonEmpty returns the first non-empty string, or "" if all are empty.
// Used to fold the app page's "warn" and "err" query params into the single
// Warning banner slot — both render identically (see app.html's .err class).
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// uiInviteRow is users.html's per-invite view model: store.Invite plus
// whether it's been used (the template can't compare UsedBy to 0 itself).
type uiInviteRow struct {
	Token     string
	Role      string
	ExpiresAt string
	Used      bool
}
