package server

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/sutantodadang/luncur/internal/render"
	"github.com/sutantodadang/luncur/internal/store"
)

const sessionCookie = "luncur_session"
const csrfCookie = "luncur_csrf"

func (s *server) uiRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /ui/login", s.handleUILoginPage)
	mux.HandleFunc("POST /ui/login", s.handleUILogin)
	mux.HandleFunc("POST /ui/logout", s.handleUILogout)
	mux.HandleFunc("GET /ui/register", s.handleUIRegisterPage)
	mux.HandleFunc("POST /ui/register", s.handleUIRegister)
	mux.HandleFunc("GET /ui/users", s.uiPage(s.handleUIUsers))
	mux.HandleFunc("POST /ui/users/invite", s.uiPage(s.handleUIInviteCreate))
	mux.HandleFunc("POST /ui/users/invite/revoke", s.uiPage(s.handleUIInviteRevoke))
	mux.HandleFunc("POST /ui/users/delete", s.uiPage(s.handleUIUserDelete))
	mux.HandleFunc("GET /ui/tokens", s.uiPage(s.handleUITokens))
	mux.HandleFunc("POST /ui/tokens/revoke", s.uiPage(s.handleUITokenRevoke))
	mux.HandleFunc("GET /ui/", s.uiPage(s.handleUIProjects))
	mux.HandleFunc("GET /ui/projects/{project}", s.uiPage(s.handleUIApps))
	mux.HandleFunc("POST /ui/projects/{project}/apps", s.uiPage(s.handleUICreateApp))
	mux.HandleFunc("GET /ui/projects/{project}/apps/{app}", s.uiPage(s.handleUIApp))
	mux.HandleFunc("POST /ui/projects/{project}/apps/{app}/scale", s.uiPage(s.handleUIScale))
	mux.HandleFunc("POST /ui/projects/{project}/apps/{app}/health", s.uiPage(s.handleUIHealth))
	mux.HandleFunc("POST /ui/projects/{project}/apps/{app}/env", s.uiPage(s.handleUIEnvSet))
	mux.HandleFunc("POST /ui/projects/{project}/apps/{app}/env/delete", s.uiPage(s.handleUIEnvUnset))
	mux.HandleFunc("POST /ui/projects/{project}/apps/{app}/domains", s.uiPage(s.handleUIDomainAdd))
	mux.HandleFunc("POST /ui/projects/{project}/apps/{app}/domains/delete", s.uiPage(s.handleUIDomainDelete))
	mux.HandleFunc("POST /ui/projects/{project}/apps/{app}/volumes", s.uiPage(s.handleUIVolumeAdd))
	mux.HandleFunc("POST /ui/projects/{project}/apps/{app}/volumes/remove", s.uiPage(s.handleUIVolumeRemove))
	mux.HandleFunc("POST /ui/projects/{project}/addons", s.uiPage(s.handleUIAddonCreate))
	mux.HandleFunc("POST /ui/projects/{project}/addons/delete", s.uiPage(s.handleUIAddonDelete))
	mux.HandleFunc("POST /ui/projects/{project}/apps/{app}/addons/attach", s.uiPage(s.handleUIAddonAttach))
	mux.HandleFunc("POST /ui/projects/{project}/apps/{app}/addons/detach", s.uiPage(s.handleUIAddonDetach))
	mux.HandleFunc("POST /ui/projects/{project}/apps/{app}/deploy", s.uiPage(s.handleUIDeploy))
	mux.HandleFunc("POST /ui/projects/{project}/apps/{app}/rollback", s.uiPage(s.handleUIRollback))
	mux.HandleFunc("POST /ui/projects/{project}/apps/{app}/adopt", s.uiPage(s.handleUIAdopt))
	mux.HandleFunc("GET /ui/projects/{project}/apps/{app}/edit/{kind}", s.uiPage(s.handleUIEditGet))
	mux.HandleFunc("POST /ui/projects/{project}/apps/{app}/edit/{kind}", s.uiPage(s.handleUIEditPost))
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

func (s *server) handleUILoginPage(w http.ResponseWriter, r *http.Request) {
	s.renderPage(w, "login.html", map[string]any{"CSRF": s.csrf(w, r)})
}

func (s *server) handleUILogin(w http.ResponseWriter, r *http.Request) {
	if !s.checkCSRF(w, r) {
		return
	}
	if err := r.ParseForm(); err != nil {
		s.renderPage(w, "login.html", map[string]any{"Error": "invalid form", "CSRF": s.csrf(w, r)})
		return
	}
	u, err := s.st.Authenticate(r.PostFormValue("email"), r.PostFormValue("password"))
	if errors.Is(err, store.ErrAuthFailed) {
		s.renderPage(w, "login.html", map[string]any{"Error": "wrong email or password", "CSRF": s.csrf(w, r)})
		return
	}
	if err != nil {
		log.Printf("ui login: %v", err)
		s.renderPage(w, "login.html", map[string]any{"Error": "internal error", "CSRF": s.csrf(w, r)})
		return
	}
	tok, err := s.st.CreateSessionToken(u.ID, "session")
	if err != nil {
		log.Printf("ui session token: %v", err)
		s.renderPage(w, "login.html", map[string]any{"Error": "internal error", "CSRF": s.csrf(w, r)})
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: tok, Path: "/",
		HttpOnly: true, SameSite: http.SameSiteStrictMode,
		Expires: time.Now().Add(7 * 24 * time.Hour),
	})
	http.Redirect(w, r, "/ui/", http.StatusSeeOther)
}

func (s *server) handleUILogout(w http.ResponseWriter, r *http.Request) {
	if !s.checkCSRF(w, r) {
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: "", Path: "/", HttpOnly: true, MaxAge: -1,
	})
	http.Redirect(w, r, "/ui/login", http.StatusSeeOther)
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

// uiAdmin 404s non-admins (leak-nothing, same policy as uiProject).
func (s *server) uiAdmin(w http.ResponseWriter, u store.User) bool {
	if u.Role != "admin" {
		http.Error(w, "not found", http.StatusNotFound)
		return false
	}
	return true
}

// uiApp is requireApp's UI twin: 404s with plain text instead of a JSON
// envelope.
func (s *server) uiApp(w http.ResponseWriter, r *http.Request, p store.Project) (store.App, bool) {
	a, err := s.st.GetApp(p.ID, r.PathValue("app"))
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

func (s *server) handleUIProjects(w http.ResponseWriter, r *http.Request, u store.User) {
	list, err := s.visibleProjects(u)
	if err != nil {
		log.Printf("ui projects: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.renderPage(w, "projects.html", map[string]any{"User": u, "Projects": list, "CSRF": s.csrf(w, r), "IsAdmin": u.Role == "admin"})
}

// uiAppRow is apps.html's per-row view model: the store.App plus its
// derived public URL.
type uiAppRow struct {
	Name     string
	Kind     string
	Schedule string
	Replicas int
	URL      string
	Ejected  bool
}

func (s *server) handleUIApps(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.uiProject(w, r, u)
	if !ok {
		return
	}
	list, err := s.st.ListApps(p.ID)
	if err != nil {
		log.Printf("ui apps: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	rows := make([]uiAppRow, 0, len(list))
	for _, a := range list {
		url := "http://" + hostFor(a.Name, s.externalIP)
		if a.Kind != "web" {
			url = ""
		}
		rows = append(rows, uiAppRow{
			Name: a.Name, Kind: a.Kind, Schedule: a.Schedule,
			Replicas: a.Replicas, URL: url, Ejected: a.Ejected,
		})
	}
	addons, err := s.addonRows(r.Context(), p)
	if err != nil {
		log.Printf("ui addons: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.renderPage(w, "apps.html", map[string]any{
		"User": u, "Project": p, "Apps": rows, "Addons": addons,
		"CSRF": s.csrf(w, r), "IsAdmin": u.Role == "admin",
	})
}

// handleUICreateApp is handleCreateApp's UI twin: same store CreateApp/
// CreateGitApp core, plain-text 400 + redirect back to the create form
// instead of a JSON envelope.
func (s *server) handleUICreateApp(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.uiProject(w, r, u)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	port := 0
	if v := r.PostFormValue("port"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			http.Error(w, "invalid port", http.StatusBadRequest)
			return
		}
		port = n
	}
	name := r.PostFormValue("name")
	kind := r.PostFormValue("kind")
	schedule := r.PostFormValue("schedule")
	gitURL := r.PostFormValue("git_url")

	var err error
	if gitURL != "" {
		_, err = s.st.CreateGitApp(p.ID, name, port, gitURL, r.PostFormValue("git_branch"), kind, schedule)
	} else {
		_, err = s.st.CreateApp(p.ID, name, port, kind, schedule)
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	http.Redirect(w, r, "/ui/projects/"+p.Name, http.StatusSeeOther)
}

func (s *server) handleUIApp(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.uiProject(w, r, u)
	if !ok {
		return
	}
	a, ok := s.uiApp(w, r, p)
	if !ok {
		return
	}

	status := "never_deployed"
	latestID := int64(0)
	if d, err := s.st.LatestDeployment(a.ID); err == nil {
		status = d.Status
		latestID = d.ID
	} else if !errors.Is(err, store.ErrNotFound) {
		log.Printf("ui app latest deployment: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	history, err := s.st.ListDeployments(a.ID)
	if err != nil {
		log.Printf("ui app history: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// Env values stay sealed — the UI only ever shows keys, never plaintext.
	sealed, err := s.st.ListEnv(a.ID)
	if err != nil {
		log.Printf("ui app env: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	envKeys := make([]string, 0, len(sealed))
	for k := range sealed {
		envKeys = append(envKeys, k)
	}
	sort.Strings(envKeys)

	domains, err := s.st.ListDomains(a.ID)
	if err != nil {
		log.Printf("ui app domains: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	volumes, err := s.st.ListVolumes(a.ID)
	if err != nil {
		log.Printf("ui app volumes: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	attached, err := s.st.AddonsForApp(a.ID)
	if err != nil {
		log.Printf("ui app addons: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	projectAddons, err := s.st.ListAddons(p.ID)
	if err != nil {
		log.Printf("ui project addons: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	metrics, err := s.appMetricsData(r.Context(), p, a)
	if err != nil {
		log.Printf("ui app metrics: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	s.renderPage(w, "app.html", map[string]any{
		"User": u, "Project": p, "App": a,
		"Status": status, "LatestID": latestID, "URL": "http://" + hostFor(a.Name, s.externalIP),
		"History": history, "EnvKeys": envKeys,
		"IsGit":   a.SourceType == "git",
		"Domains": domains, "Volumes": volumes, "Warning": r.URL.Query().Get("warn"),
		"Addons": attached, "ProjectAddons": projectAddons, "Metrics": metrics,
		"CSRF": s.csrf(w, r), "IsAdmin": u.Role == "admin",
	})
}

// uiRedirect sends the browser back to the app detail page an action
// (scale/env/deploy) was posted from.
func uiRedirect(w http.ResponseWriter, r *http.Request, p store.Project, a store.App) {
	http.Redirect(w, r, "/ui/projects/"+p.Name+"/apps/"+a.Name, http.StatusSeeOther)
}

func (s *server) handleUIScale(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.uiProject(w, r, u)
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

	if _, err := s.scaleApp(r.Context(), p, a, scaleChange{Replicas: replicasPtr, CPUMilli: &cpu, MemoryMB: &mem}); err != nil {
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
	uiRedirect(w, r, p, a)
}

func (s *server) handleUIHealth(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.uiProject(w, r, u)
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

	if err := s.setAppHealth(r.Context(), p, a, r.PostFormValue("health_path")); err != nil {
		switch {
		case errors.Is(err, errAppEjected):
			http.Error(w, err.Error(), http.StatusConflict)
		default:
			http.Error(w, err.Error(), http.StatusBadRequest)
		}
		return
	}
	uiRedirect(w, r, p, a)
}

func (s *server) handleUIEnvSet(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.uiProject(w, r, u)
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

	if err := s.setAppEnv(r.Context(), p, a, r.PostFormValue("key"), r.PostFormValue("value")); err != nil {
		var ve *store.ValidationError
		switch {
		case errors.Is(err, errAppEjected):
			http.Error(w, err.Error(), http.StatusConflict)
		case errors.Is(err, errSealerUnavailable):
			http.Error(w, "sealer is not configured", http.StatusServiceUnavailable)
		case errors.As(err, &ve):
			http.Error(w, ve.Error(), http.StatusBadRequest)
		default:
			log.Printf("ui set env: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
		}
		return
	}
	uiRedirect(w, r, p, a)
}

func (s *server) handleUIEnvUnset(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.uiProject(w, r, u)
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

	if err := s.unsetAppEnv(r.Context(), p, a, r.PostFormValue("key")); err != nil {
		switch {
		case errors.Is(err, errAppEjected):
			http.Error(w, err.Error(), http.StatusConflict)
		case errors.Is(err, store.ErrNotFound):
			http.Error(w, "no such env var", http.StatusNotFound)
		default:
			log.Printf("ui unset env: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
		}
		return
	}
	uiRedirect(w, r, p, a)
}

// handleUIDomainAdd is handleAddDomain's UI twin: same shared addDomain
// core, but redirects back to the app page instead of returning JSON. A
// non-empty DNS warning rides along as a ?warn= query param so the page can
// show it once, on the redirect target, without persisting it anywhere.
func (s *server) handleUIDomainAdd(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.uiProject(w, r, u)
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

	_, warning, err := s.addDomain(r.Context(), p, a, r.PostFormValue("hostname"))
	if err != nil {
		if errors.Is(err, errAppEjected) {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if warning != "" {
		http.Redirect(w, r, "/ui/projects/"+p.Name+"/apps/"+a.Name+"?warn="+url.QueryEscape(warning), http.StatusSeeOther)
		return
	}
	uiRedirect(w, r, p, a)
}

// handleUIDomainDelete is handleDeleteDomain's UI twin: same store+sync
// calls, redirect instead of a 204.
func (s *server) handleUIDomainDelete(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.uiProject(w, r, u)
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
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}

	if err := s.st.DeleteDomain(a.ID, r.PostFormValue("hostname")); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, "no such domain", http.StatusNotFound)
			return
		}
		log.Printf("ui delete domain: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.syncIfLive(r.Context(), p, a)
	uiRedirect(w, r, p, a)
}

// handleUIVolumeAdd is handleAddVolume's UI twin: same shared addVolume
// core, redirect instead of JSON.
func (s *server) handleUIVolumeAdd(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.uiProject(w, r, u)
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
	size, err := strconv.Atoi(r.PostFormValue("size_gb"))
	if err != nil {
		http.Error(w, "invalid size", http.StatusBadRequest)
		return
	}

	if _, err := s.addVolume(r.Context(), p, a, r.PostFormValue("name"), r.PostFormValue("path"), size); err != nil {
		var rc *volumeReplicaConflictError
		var ke *kindMismatchError
		var ve *store.ValidationError
		switch {
		case errors.Is(err, errAppEjected):
			http.Error(w, err.Error(), http.StatusConflict)
		case errors.As(err, &rc):
			http.Error(w, rc.Error(), http.StatusConflict)
		case errors.As(err, &ke):
			http.Error(w, ke.Error(), http.StatusBadRequest)
		case errors.As(err, &ve):
			http.Error(w, ve.Error(), http.StatusBadRequest)
		default:
			log.Printf("ui add volume: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
		}
		return
	}
	uiRedirect(w, r, p, a)
}

// handleUIVolumeRemove is handleDeleteVolume's UI twin: same shared
// removeVolume core (purge via checkbox), redirect instead of JSON.
func (s *server) handleUIVolumeRemove(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.uiProject(w, r, u)
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

	purge := r.PostFormValue("purge") != ""
	if err := s.removeVolume(r.Context(), p, a, r.PostFormValue("name"), purge); err != nil {
		switch {
		case errors.Is(err, errAppEjected):
			http.Error(w, err.Error(), http.StatusConflict)
		case errors.Is(err, errKubeUnavailable):
			http.Error(w, "kubernetes is not configured", http.StatusServiceUnavailable)
		case errors.Is(err, store.ErrNotFound):
			http.Error(w, "no such volume", http.StatusNotFound)
		default:
			log.Printf("ui remove volume: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
		}
		return
	}
	uiRedirect(w, r, p, a)
}

// handleUIAddonCreate is handleCreateAddon's UI twin: same shared
// createAddon core (unattached — the project page's form has no app
// picker), redirect instead of a 201 body.
func (s *server) handleUIAddonCreate(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.uiProject(w, r, u)
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
	sizeGB := 1
	if v := r.PostFormValue("size_gb"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			http.Error(w, "invalid size_gb", http.StatusBadRequest)
			return
		}
		sizeGB = n
	}

	if _, err := s.createAddon(r.Context(), p, r.PostFormValue("type"), r.PostFormValue("name"), r.PostFormValue("version"), sizeGB, ""); err != nil {
		switch {
		case errors.Is(err, errSealerUnavailable):
			http.Error(w, "sealer is not configured", http.StatusServiceUnavailable)
		default:
			http.Error(w, err.Error(), http.StatusBadRequest)
		}
		return
	}
	http.Redirect(w, r, "/ui/projects/"+p.Name, http.StatusSeeOther)
}

// handleUIAddonDelete is handleDeleteAddon's UI twin: same shared
// removeAddon core, redirect instead of a 204. force/keep_data ride form
// checkboxes instead of query params.
func (s *server) handleUIAddonDelete(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.uiProject(w, r, u)
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
	ad, ok := s.uiAddon(w, p, r.PostFormValue("name"))
	if !ok {
		return
	}
	force := r.PostFormValue("force") == "1"
	keepData := r.PostFormValue("keep_data") == "1"

	if err := s.removeAddon(r.Context(), p, ad, force, keepData); err != nil {
		if errors.Is(err, errAddonAttached) {
			http.Error(w, "addon is attached to one or more apps; check force to remove anyway", http.StatusConflict)
			return
		}
		log.Printf("ui delete addon: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/ui/projects/"+p.Name, http.StatusSeeOther)
}

// handleUIAddonAttach is handleAttachAddon's UI twin: same shared
// attachAddon core. A non-empty collision warning rides the ?warn= query
// param, same mechanism handleUIDomainAdd uses.
func (s *server) handleUIAddonAttach(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.uiProject(w, r, u)
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
	ad, ok := s.uiAddon(w, p, r.PostFormValue("name"))
	if !ok {
		return
	}

	warning, err := s.attachAddon(r.Context(), p, ad, a.Name)
	if err != nil {
		if errors.Is(err, errAppEjected) {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if warning != "" {
		http.Redirect(w, r, "/ui/projects/"+p.Name+"/apps/"+a.Name+"?warn="+url.QueryEscape(warning), http.StatusSeeOther)
		return
	}
	uiRedirect(w, r, p, a)
}

// handleUIAddonDetach is handleDetachAddon's UI twin: same store+sync
// calls, redirect instead of a 204.
func (s *server) handleUIAddonDetach(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.uiProject(w, r, u)
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
	ad, ok := s.uiAddon(w, p, r.PostFormValue("name"))
	if !ok {
		return
	}

	if err := s.st.DetachAddon(ad.ID, a.ID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, "addon is not attached to this app", http.StatusNotFound)
			return
		}
		log.Printf("ui detach addon: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.syncIfLive(r.Context(), p, a)
	uiRedirect(w, r, p, a)
}

// handleUIDeploy starts a git build for git-source apps via the same
// deployGitApp core handleDeployApp's git branch uses. A non-git app is a
// no-op redirect (the template hides the "Deploy from git" button, so this
// only guards against a direct POST); missing kube/build-source surface as
// 503 exactly like the API path, never as a silent redirect.
func (s *server) handleUIDeploy(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.uiProject(w, r, u)
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

	if _, err := s.deployGitApp(p, a, u.ID); err != nil {
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
	uiRedirect(w, r, p, a)
}

// handleUIRollback is handleRollback's UI twin: same shared s.rollback
// core, plain-text statuses instead of a JSON envelope, redirect instead of
// a 202 body. Guards on s.kube itself (rather than relying on a sentinel
// out of s.rollback) because applyImageDeploy would otherwise panic on a
// nil client — mirroring handleRollback's requireKube check.
func (s *server) handleUIRollback(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.uiProject(w, r, u)
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
	deployID, err := strconv.ParseInt(r.PostFormValue("deploy_id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid deploy_id", http.StatusBadRequest)
		return
	}

	if _, err := s.rollback(r.Context(), p, a, u, deployID); err != nil {
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
	uiRedirect(w, r, p, a)
}

// registerPageData builds register.html's view-model for a given token,
// looking the invite up fresh so GET and the POST error paths render
// identically.
func (s *server) registerPageData(w http.ResponseWriter, r *http.Request, token string, extra map[string]any) map[string]any {
	inv, err := s.st.GetValidInvite(token)
	data := map[string]any{"CSRF": s.csrf(w, r), "Token": token, "Valid": err == nil, "Role": inv.Role}
	for k, v := range extra {
		data[k] = v
	}
	return data
}

// handleUIRegisterPage is session-less: anyone with a valid invite link can
// reach it without being logged in.
func (s *server) handleUIRegisterPage(w http.ResponseWriter, r *http.Request) {
	s.renderPage(w, "register.html", s.registerPageData(w, r, r.URL.Query().Get("token"), nil))
}

// handleUIRegister is session-less, like handleUIRegisterPage: checkCSRF
// first, then re-validate the token (it may have been burned or expired
// since the form was loaded), create the user with the INVITE's role (never
// a client-supplied one), burn the invite, and log the new user straight in
// with the exact session-cookie shape handleUILogin uses.
func (s *server) handleUIRegister(w http.ResponseWriter, r *http.Request) {
	if !s.checkCSRF(w, r) {
		return
	}
	token := r.PostFormValue("token")
	inv, err := s.st.GetValidInvite(token)
	if err != nil {
		s.renderPage(w, "register.html", s.registerPageData(w, r, token, nil))
		return
	}

	u, err := s.st.CreateUser(r.PostFormValue("email"), r.PostFormValue("password"), inv.Role)
	if err != nil {
		errMsg := "internal error"
		var ve *store.ValidationError
		switch {
		case strings.Contains(err.Error(), "UNIQUE constraint failed: users.email"):
			errMsg = "an account with that email already exists"
		case errors.As(err, &ve):
			errMsg = ve.Error()
		default:
			log.Printf("ui register create user: %v", err)
		}
		// Invite stays unburned: re-render the same form the user just
		// filled in, with an error, so the token is still usable.
		s.renderPage(w, "register.html", s.registerPageData(w, r, token, map[string]any{"Error": errMsg}))
		return
	}

	if err := s.st.MarkInviteUsed(token, u.ID); err != nil {
		// The user account already exists at this point; a failure here
		// just means the invite could be replayed, not that registration
		// failed, so we log and continue rather than error out.
		log.Printf("ui register mark invite used: %v", err)
	}

	tok, err := s.st.CreateSessionToken(u.ID, "session")
	if err != nil {
		log.Printf("ui register session token: %v", err)
		s.renderPage(w, "register.html", s.registerPageData(w, r, token, map[string]any{"Error": "internal error"}))
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: tok, Path: "/",
		HttpOnly: true, SameSite: http.SameSiteStrictMode,
		Expires: time.Now().Add(7 * 24 * time.Hour),
	})
	http.Redirect(w, r, "/ui/", http.StatusSeeOther)
}

// uiInviteRow is users.html's per-invite view model: store.Invite plus
// whether it's been used (the template can't compare UsedBy to 0 itself).
type uiInviteRow struct {
	Token     string
	Role      string
	ExpiresAt string
	Used      bool
}

func (s *server) handleUIUsers(w http.ResponseWriter, r *http.Request, u store.User) {
	if !s.uiAdmin(w, u) {
		return
	}
	users, err := s.st.ListUsers()
	if err != nil {
		log.Printf("ui users: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	invites, err := s.st.ListInvites()
	if err != nil {
		log.Printf("ui invites: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	rows := make([]uiInviteRow, 0, len(invites))
	for _, i := range invites {
		rows = append(rows, uiInviteRow{Token: i.Token, Role: i.Role, ExpiresAt: i.ExpiresAt, Used: i.UsedBy != 0})
	}
	var mailNote string
	switch r.URL.Query().Get("mail") {
	case "sent":
		mailNote = "invite emailed"
	case "failed":
		mailNote = "invite created, but the email failed — copy the link below"
	}
	s.renderPage(w, "users.html", map[string]any{
		"User": u, "Users": users, "Invites": rows, "Self": u.ID,
		"CSRF": s.csrf(w, r), "IsAdmin": u.Role == "admin",
		"MailNote": mailNote,
	})
}

// handleUITokens lists the caller's own tokens — the UI twin of
// GET /v1/tokens. The web session rides the same table (name "session").
func (s *server) handleUITokens(w http.ResponseWriter, r *http.Request, u store.User) {
	tokens, err := s.st.ListTokens(u.ID)
	if err != nil {
		log.Printf("ui tokens: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.renderPage(w, "tokens.html", map[string]any{
		"User": u, "Tokens": tokens,
		"CSRF": s.csrf(w, r), "IsAdmin": u.Role == "admin",
	})
}

// handleUITokenRevoke revokes one of the caller's tokens. Revoking the
// current session's token logs the browser out on the next request.
func (s *server) handleUITokenRevoke(w http.ResponseWriter, r *http.Request, u store.User) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	id, err := strconv.ParseInt(r.PostFormValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid token id", http.StatusBadRequest)
		return
	}
	if err := s.st.RevokeToken(u.ID, id); err != nil && !errors.Is(err, store.ErrNotFound) {
		log.Printf("ui revoke token: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/ui/tokens", http.StatusSeeOther)
}

func (s *server) handleUIInviteCreate(w http.ResponseWriter, r *http.Request, u store.User) {
	if !s.uiAdmin(w, u) {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	role := r.PostFormValue("role")
	if role == "" {
		role = "member"
	}
	inv, err := s.st.CreateInvite(role, u.ID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	dest := "/ui/users"
	if email := strings.TrimSpace(r.PostFormValue("email")); email != "" {
		if err := s.emailInvite(r, email, inv); err != nil {
			log.Printf("ui invite email to %s: %v", email, err)
			dest += "?mail=failed"
		} else {
			dest += "?mail=sent"
		}
	}
	http.Redirect(w, r, dest, http.StatusSeeOther)
}

// handleUIAdopt is handleAdoptApp's UI twin: clear the ejected flag,
// best-effort re-sync, redirect back to the app page.
func (s *server) handleUIAdopt(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.uiProject(w, r, u)
	if !ok {
		return
	}
	a, ok := s.uiApp(w, r, p)
	if !ok {
		return
	}
	if !a.Ejected {
		http.Error(w, "app is not ejected", http.StatusConflict)
		return
	}
	if err := s.st.SetAppAdopted(a.ID); err != nil {
		log.Printf("ui adopt: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	a.Ejected = false
	s.syncIfLive(r.Context(), p, a)
	uiRedirect(w, r, p, a)
}

func (s *server) handleUIInviteRevoke(w http.ResponseWriter, r *http.Request, u store.User) {
	if !s.uiAdmin(w, u) {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	if err := s.st.RevokeInvite(r.PostFormValue("token")); err != nil && !errors.Is(err, store.ErrNotFound) {
		log.Printf("ui revoke invite: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/ui/users", http.StatusSeeOther)
}

func (s *server) handleUIUserDelete(w http.ResponseWriter, r *http.Request, u store.User) {
	if !s.uiAdmin(w, u) {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	id, err := strconv.ParseInt(r.PostFormValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid user id", http.StatusBadRequest)
		return
	}
	if id == u.ID {
		http.Error(w, "cannot delete yourself", http.StatusBadRequest)
		return
	}
	if err := s.st.DeleteUser(id); err != nil && !errors.Is(err, store.ErrNotFound) {
		log.Printf("ui delete user: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/ui/users", http.StatusSeeOther)
}

// renderEditPage renders edit.html for both the GET view and any POST error
// path, so a rejected submission re-shows the user's own text rather than
// reloading the stored doc.
func (s *server) renderEditPage(w http.ResponseWriter, r *http.Request, u store.User, p store.Project, a store.App, kind, yamlText, errMsg string) {
	data := map[string]any{
		"User": u, "Project": p, "App": a, "Kind": kind, "YAML": yamlText,
		"CSRF": s.csrf(w, r), "IsAdmin": u.Role == "admin",
	}
	if errMsg != "" {
		data["Error"] = errMsg
	}
	s.renderPage(w, "edit.html", data)
}

// editDoc renders the app (base or with overrides, per withOverrides) and
// extracts the single document for kind — the shared render-then-split step
// both the editor GET and POST need.
func (s *server) editDoc(p store.Project, a store.App, kind string, withOverrides bool) ([]byte, error) {
	image, err := s.appImage(a)
	if err != nil {
		return nil, err
	}
	rendered, err := s.renderApp(p, a, image, withOverrides)
	if err != nil {
		return nil, err
	}
	yamlBytes, err := render.YAML(rendered)
	if err != nil {
		return nil, err
	}
	return render.ExtractDoc(yamlBytes, kind)
}

// handleUIEditGet shows the app's current rendered doc (overrides applied)
// for one editable kind in a textarea.
func (s *server) handleUIEditGet(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.uiProject(w, r, u)
	if !ok {
		return
	}
	a, ok := s.uiApp(w, r, p)
	if !ok {
		return
	}
	kind := r.PathValue("kind")
	if !editableKinds[kind] {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	doc, err := s.editDoc(p, a, kind, true)
	if err != nil {
		log.Printf("ui edit render: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.renderEditPage(w, r, u, p, a, kind, string(doc), "")
}

// handleUIEditPost diffs the submitted YAML against a fresh base render (no
// overrides) and stores the resulting strategic-merge patch through the same
// s.setOverride path the JSON API uses. A no-op edit redirects without
// writing anything; any error — bad YAML, an invalid patch, a rejected
// override — re-renders the editor with the user's own text so nothing is
// lost.
func (s *server) handleUIEditPost(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.uiProject(w, r, u)
	if !ok {
		return
	}
	a, ok := s.uiApp(w, r, p)
	if !ok {
		return
	}
	kind := r.PathValue("kind")
	if !editableKinds[kind] {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	submitted := r.PostFormValue("yaml")

	baseDoc, err := s.editDoc(p, a, kind, false)
	if err != nil {
		log.Printf("ui edit base render: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	patch, err := render.ComputeOverride(kind, baseDoc, []byte(submitted))
	if err != nil {
		s.renderEditPage(w, r, u, p, a, kind, submitted, err.Error())
		return
	}
	if patch == "{}" {
		uiRedirect(w, r, p, a)
		return
	}

	if err := s.setOverride(r.Context(), p, a, kind, patch); err != nil {
		if errors.Is(err, errAppEjected) {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		var ve *store.ValidationError
		msg := "internal error"
		if errors.As(err, &ve) {
			msg = ve.Error()
		} else {
			log.Printf("ui edit set override: %v", err)
		}
		s.renderEditPage(w, r, u, p, a, kind, submitted, msg)
		return
	}
	uiRedirect(w, r, p, a)
}
