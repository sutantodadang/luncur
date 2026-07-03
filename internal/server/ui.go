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
	"time"

	"github.com/sutantodadang/luncur/internal/store"
)

const sessionCookie = "luncur_session"
const csrfCookie = "luncur_csrf"

func (s *server) uiRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /ui/login", s.handleUILoginPage)
	mux.HandleFunc("POST /ui/login", s.handleUILogin)
	mux.HandleFunc("POST /ui/logout", s.handleUILogout)
	mux.HandleFunc("GET /ui/", s.uiPage(s.handleUIProjects))
	mux.HandleFunc("GET /ui/projects/{project}", s.uiPage(s.handleUIApps))
	mux.HandleFunc("GET /ui/projects/{project}/apps/{app}", s.uiPage(s.handleUIApp))
	mux.HandleFunc("POST /ui/projects/{project}/apps/{app}/scale", s.uiPage(s.handleUIScale))
	mux.HandleFunc("POST /ui/projects/{project}/apps/{app}/env", s.uiPage(s.handleUIEnvSet))
	mux.HandleFunc("POST /ui/projects/{project}/apps/{app}/env/delete", s.uiPage(s.handleUIEnvUnset))
	mux.HandleFunc("POST /ui/projects/{project}/apps/{app}/domains", s.uiPage(s.handleUIDomainAdd))
	mux.HandleFunc("POST /ui/projects/{project}/apps/{app}/domains/delete", s.uiPage(s.handleUIDomainDelete))
	mux.HandleFunc("POST /ui/projects/{project}/apps/{app}/deploy", s.uiPage(s.handleUIDeploy))
	mux.HandleFunc("POST /ui/projects/{project}/apps/{app}/rollback", s.uiPage(s.handleUIRollback))
}

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

func (s *server) handleUIProjects(w http.ResponseWriter, r *http.Request, u store.User) {
	list, err := s.visibleProjects(u)
	if err != nil {
		log.Printf("ui projects: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.renderPage(w, "projects.html", map[string]any{"User": u, "Projects": list, "CSRF": s.csrf(w, r)})
}

// uiAppRow is apps.html's per-row view model: the store.App plus its
// derived public URL.
type uiAppRow struct {
	Name     string
	Replicas int
	URL      string
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
		rows = append(rows, uiAppRow{Name: a.Name, Replicas: a.Replicas, URL: "http://" + hostFor(a.Name, s.externalIP)})
	}
	s.renderPage(w, "apps.html", map[string]any{"User": u, "Project": p, "Apps": rows, "CSRF": s.csrf(w, r)})
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

	s.renderPage(w, "app.html", map[string]any{
		"User": u, "Project": p, "App": a,
		"Status": status, "LatestID": latestID, "URL": "http://" + hostFor(a.Name, s.externalIP),
		"History": history, "EnvKeys": envKeys,
		"IsGit":   a.SourceType == "git",
		"Domains": domains, "DNSWarning": r.URL.Query().Get("warn"),
		"CSRF": s.csrf(w, r),
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
	replicas, err := strconv.Atoi(r.PostFormValue("replicas"))
	if err != nil {
		http.Error(w, "invalid replicas", http.StatusBadRequest)
		return
	}

	if _, err := s.scaleApp(r.Context(), p, a, replicas); err != nil {
		var re *scaleReplicasError
		switch {
		case errors.Is(err, errKubeUnavailable):
			http.Error(w, "kubernetes is not configured", http.StatusServiceUnavailable)
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
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, "no such env var", http.StatusNotFound)
			return
		}
		log.Printf("ui unset env: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
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
