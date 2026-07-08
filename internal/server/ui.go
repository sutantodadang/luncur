package server

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/sutantodadang/luncur/internal/kube"
	"github.com/sutantodadang/luncur/internal/render"
	"github.com/sutantodadang/luncur/internal/store"
)

const sessionCookie = "luncur_session"
const csrfCookie = "luncur_csrf"

func (s *server) uiRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /ui/static/{file}", s.handleUIStatic)
	mux.HandleFunc("GET /ui/login", s.handleUILoginPage)
	mux.HandleFunc("POST /ui/login", s.handleUILogin)
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
	mux.HandleFunc("POST /ui/projects/{project}/apps", s.uiPage(s.handleUICreateApp))
	mux.HandleFunc("POST /ui/projects/{project}/gpu-quota", s.uiPage(s.handleUIGPUQuota))
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
	mux.HandleFunc("GET /ui/projects/{project}/apps/{app}/chip", s.uiPage(s.handleUIChip))
	mux.HandleFunc("GET /ui/projects/{project}/apps/{app}/chart", s.uiPage(s.handleUIAppChart))
	mux.HandleFunc("GET /ui/nodes/charts", s.uiPage(s.handleUINodeCharts))
	mux.HandleFunc("POST /ui/projects/{project}/apps/{app}/destroy", s.uiPage(s.handleUIAppDestroy))
	mux.HandleFunc("POST /ui/projects/{project}/apps/{app}/eject", s.uiPage(s.handleUIEject))
	mux.HandleFunc("POST /ui/projects/{project}/apps/{app}/domains/retry", s.uiPage(s.handleUIDomainRetry))
	mux.HandleFunc("POST /ui/projects/{project}/apps/{app}/scale", s.uiPage(s.handleUIScale))
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

func (s *server) handleUIProjects(w http.ResponseWriter, r *http.Request, u store.User) {
	list, err := s.visibleProjects(u)
	if err != nil {
		log.Printf("ui projects: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	cards := make([]uiProjectCard, 0, len(list))
	for _, p := range list {
		apps, err := s.st.ListApps(p.ID)
		if err != nil {
			log.Printf("ui projects: list apps: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		var live, building, failed int
		for _, a := range apps {
			status := ""
			if d, err := s.st.LatestDeployment(a.ID); err == nil {
				status = d.Status
			} else if !errors.Is(err, store.ErrNotFound) {
				log.Printf("ui projects: latest deployment: %v", err)
				http.Error(w, "internal error", http.StatusInternalServerError)
				return
			}
			switch status {
			case "live":
				live++
			case "building", "deploying", "pending":
				building++
			case "failed":
				failed++
			}
		}
		members, err := s.st.ListMembers(p.ID)
		if err != nil {
			log.Printf("ui projects: list members: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		cards = append(cards, uiProjectCard{
			Name: p.Name, Namespace: p.Namespace,
			Apps: len(apps), Live: live, Building: building, Failed: failed,
			Members: len(members),
		})
	}
	var banner string
	if e := r.URL.Query().Get("err"); e != "" {
		banner = "error: " + e
	}
	s.renderPage(w, "projects.html", map[string]any{
		"User": u, "Projects": cards, "Banner": banner,
		"CSRF": s.csrf(w, r), "IsAdmin": u.Role == "admin",
	})
}

// handleUIProjectCreate is handleCreateProject's UI twin: same store
// CreateProject core, admin-gated, redirect instead of a 201 body.
func (s *server) handleUIProjectCreate(w http.ResponseWriter, r *http.Request, u store.User) {
	if !s.uiAdmin(w, u) {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	if _, err := s.st.CreateProject(r.PostFormValue("name")); err != nil {
		msg := "internal error"
		var ve *store.ValidationError
		switch {
		case strings.Contains(err.Error(), "UNIQUE constraint failed: projects."):
			msg = "project already exists"
		case errors.As(err, &ve):
			msg = ve.Error()
		default:
			log.Printf("ui create project: %v", err)
		}
		http.Redirect(w, r, "/ui/?err="+url.QueryEscape(msg), http.StatusSeeOther)
		return
	}
	flash(w, "ok", "project created")
	http.Redirect(w, r, "/ui/", http.StatusSeeOther)
}

// handleUIAddMember is handleAddMember's UI twin: same GetUserByEmail+
// AddMember core, admin-gated, redirect back to the project page instead of
// a 204.
func (s *server) handleUIAddMember(w http.ResponseWriter, r *http.Request, u store.User) {
	if !s.uiAdmin(w, u) {
		return
	}
	p, ok := s.uiProject(w, r, u)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	member, err := s.st.GetUserByEmail(r.PostFormValue("email"))
	if errors.Is(err, store.ErrNotFound) {
		http.Redirect(w, r, "/ui/projects/"+p.Name+"?err="+url.QueryEscape("no such user"), http.StatusSeeOther)
		return
	}
	if err != nil {
		log.Printf("ui add member: get user: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if err := s.st.AddMember(p.ID, member.ID); err != nil {
		log.Printf("ui add member: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	flash(w, "ok", "member added")
	http.Redirect(w, r, "/ui/projects/"+p.Name, http.StatusSeeOther)
}

// handleUIMemberRemove is handleRemoveMember's UI twin: same GetUserByEmail+
// RemoveMember core, admin-gated, redirect back to the project page. A
// non-member or unknown email redirects back the same as success — nothing
// for the admin to fix, the end state is already what they wanted.
func (s *server) handleUIMemberRemove(w http.ResponseWriter, r *http.Request, u store.User) {
	if !s.uiAdmin(w, u) {
		return
	}
	p, ok := s.uiProject(w, r, u)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	member, err := s.st.GetUserByEmail(r.PostFormValue("email"))
	if err == nil {
		if err := s.st.RemoveMember(p.ID, member.ID); err != nil && !errors.Is(err, store.ErrNotFound) {
			log.Printf("ui remove member: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
	} else if !errors.Is(err, store.ErrNotFound) {
		log.Printf("ui remove member: get user: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	flash(w, "ok", "member removed")
	http.Redirect(w, r, "/ui/projects/"+p.Name, http.StatusSeeOther)
}

// handleUIProjectRename is handleRenameProject's UI twin: same RenameProject
// core, admin-gated, redirect to the renamed project's new page on success
// or back to the old page with a fixed perr code on failure.
func (s *server) handleUIProjectRename(w http.ResponseWriter, r *http.Request, u store.User) {
	if !s.uiAdmin(w, u) {
		return
	}
	p, ok := s.uiProject(w, r, u)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	newName := r.PostFormValue("name")
	if err := s.st.RenameProject(p.ID, newName); err != nil {
		var ve *store.ValidationError
		switch {
		case strings.Contains(err.Error(), "UNIQUE constraint failed: projects."):
			http.Redirect(w, r, "/ui/projects/"+p.Name+"?perr=taken", http.StatusSeeOther)
		case errors.As(err, &ve):
			http.Redirect(w, r, "/ui/projects/"+p.Name+"?perr=invalid", http.StatusSeeOther)
		default:
			log.Printf("ui rename project: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
		}
		return
	}
	flash(w, "ok", "project renamed")
	http.Redirect(w, r, "/ui/projects/"+newName, http.StatusSeeOther)
}

// handleUIProjectDelete is handleDeleteProject's UI twin: same
// list-apps/list-addons + deleteProject core, admin-gated, redirect to the
// project list on success or back to the project page with perr=nokube when
// kube is required but unconfigured.
func (s *server) handleUIProjectDelete(w http.ResponseWriter, r *http.Request, u store.User) {
	if !s.uiAdmin(w, u) {
		return
	}
	p, ok := s.uiProject(w, r, u)
	if !ok {
		return
	}
	apps, err := s.st.ListApps(p.ID)
	if err != nil {
		log.Printf("ui delete project: list apps: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	addons, err := s.st.ListAddons(p.ID)
	if err != nil {
		log.Printf("ui delete project: list addons: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if projectNeedsKube(apps, addons) && s.kube == nil {
		http.Redirect(w, r, "/ui/projects/"+p.Name+"?perr=nokube", http.StatusSeeOther)
		return
	}
	if err := s.deleteProject(r.Context(), p, apps, addons); err != nil {
		log.Printf("ui delete project: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	flash(w, "ok", "project deleted")
	http.Redirect(w, r, "/ui/", http.StatusSeeOther)
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
		url := s.appURL(a)
		internalURL := ""
		if a.Kind != "web" {
			url = ""
		} else if a.Internal {
			url = ""
			internalURL = internalURLFor(a.Name, p.Namespace)
		}
		status := ""
		if d, err := s.st.LatestDeployment(a.ID); err == nil {
			status = d.Status
		} else if !errors.Is(err, store.ErrNotFound) {
			log.Printf("ui apps latest deployment: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		rows = append(rows, uiAppRow{
			Name: a.Name, Kind: a.Kind, Schedule: a.Schedule,
			Replicas: a.Replicas, URL: url, Internal: a.Internal, InternalURL: internalURL,
			Ejected: a.Ejected, Status: status,
		})
	}
	addons, err := s.addonRows(r.Context(), p)
	if err != nil {
		log.Printf("ui addons: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	members, err := s.st.ListMembers(p.ID)
	if err != nil {
		log.Printf("ui members: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	pipelines, err := s.uiPipelineCardRows(p)
	if err != nil {
		log.Printf("ui pipelines: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	var banner string
	if e := r.URL.Query().Get("err"); e != "" {
		banner = "error: " + e
	}
	// perr carries handleUIProjectRename/handleUIProjectDelete's outcome
	// back to this page — fixed strings only, same idiom as users.html's
	// "mail" notice, never the raw error or user input.
	var perrNote string
	switch r.URL.Query().Get("perr") {
	case "invalid":
		perrNote = "invalid project name"
	case "taken":
		perrNote = "name already in use"
	case "nokube":
		perrNote = "kubernetes unavailable — cannot destroy apps"
	}
	s.renderPage(w, "apps.html", map[string]any{
		"User": u, "Project": p, "Apps": rows, "Addons": addons, "Members": members, "Banner": banner,
		"CSRF": s.csrf(w, r), "IsAdmin": u.Role == "admin", "PErrNote": perrNote,
		"GPUQuota": p.GPUQuota, "Pipelines": pipelines,
	})
}

// handleUIGPUQuota is setGPUQuota's UI twin: same shared store+kube core as
// handleSetGPUQuota, form-POST instead of JSON, redirect back to the
// project page with ?err= on failure (mirrors handleUIProjectRename).
func (s *server) handleUIGPUQuota(w http.ResponseWriter, r *http.Request, u store.User) {
	if !s.uiAdmin(w, u) {
		return
	}
	p, ok := s.uiProject(w, r, u)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	n, err := strconv.ParseInt(r.PostFormValue("quota"), 10, 64)
	if err != nil {
		http.Redirect(w, r, "/ui/projects/"+p.Name+"?err="+url.QueryEscape("invalid gpu quota"), http.StatusSeeOther)
		return
	}
	if err := s.setGPUQuota(r.Context(), p, n); err != nil {
		http.Redirect(w, r, "/ui/projects/"+p.Name+"?err="+url.QueryEscape(err.Error()), http.StatusSeeOther)
		return
	}
	flash(w, "ok", "gpu quota updated")
	http.Redirect(w, r, "/ui/projects/"+p.Name, http.StatusSeeOther)
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
	image := strings.TrimSpace(r.PostFormValue("image"))

	buildPath, err := validBuildPath(r.PostFormValue("build_path"))
	if err != nil {
		http.Error(w, "build_path: "+err.Error(), http.StatusBadRequest)
		return
	}
	internal := r.PostFormValue("internal") != ""
	if err := validateInternalKind(internal, kind); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	var gpu int64
	if v := r.PostFormValue("gpu"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			http.Error(w, "invalid gpu count", http.StatusBadRequest)
			return
		}
		gpu = n
	}
	modelSource := strings.TrimSpace(r.PostFormValue("model_source"))
	modelRuntime := r.PostFormValue("runtime")
	var modelRT render.ModelRuntimeInfo
	if kind == "model" {
		if gitURL != "" {
			http.Error(w, "model apps do not take a git url", http.StatusBadRequest)
			return
		}
		// Resolve now so a bad source/runtime combination fails before the
		// app row exists — same order as the JSON API's create.
		modelRT, err = render.ResolveModelRuntime(modelSource, modelRuntime, gpu)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}

	var a store.App
	switch {
	case kind == "model":
		a, err = s.st.CreateModelApp(p.ID, name, modelSource, modelRuntime)
	case gitURL != "":
		a, err = s.st.CreateGitApp(p.ID, name, port, gitURL, r.PostFormValue("git_branch"), kind, schedule)
	default:
		a, err = s.st.CreateApp(p.ID, name, port, kind, schedule)
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if buildPath != "" {
		if err := s.st.SetBuildPath(a.ID, buildPath); err != nil {
			log.Printf("ui create app: set build path: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		a.BuildPath = buildPath
	}
	if internal {
		if err := s.st.SetInternal(a.ID, true); err != nil {
			log.Printf("ui create app: set internal: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		a.Internal = true
	}
	if gpu != 0 {
		if err := s.st.SetGPU(a.ID, gpu); err != nil {
			http.Error(w, "gpu: "+err.Error(), http.StatusBadRequest)
			return
		}
		a.GPUCount = gpu
	}

	// Built-in runtime model apps deploy themselves at create: the runtime
	// image is known, so reuse the one-click image-deploy tail below.
	if a.Kind == "model" && modelRT.Name != "custom" {
		image = modelRT.Image
	}

	if image == "" {
		flash(w, "ok", "app created")
		http.Redirect(w, r, "/ui/projects/"+p.Name, http.StatusSeeOther)
		return
	}

	// One-click deploy from a prebuilt image: same applyImageDeploy core
	// deployImage (API) and rollback use. Any failure past this point leaves
	// the app created — only the deploy itself failed — so we redirect to
	// the app page with ?err= instead of erroring the whole create.
	if s.kube == nil {
		http.Redirect(w, r, "/ui/projects/"+p.Name+"/apps/"+a.Name+"?err="+url.QueryEscape("deploy failed: kubernetes is not configured"), http.StatusSeeOther)
		return
	}
	d, err := s.st.CreateDeployment(a.ID, "deploying", image, 0)
	if err != nil {
		log.Printf("ui create app: create deployment: %v", err)
		http.Redirect(w, r, "/ui/projects/"+p.Name+"/apps/"+a.Name+"?err="+url.QueryEscape("deploy failed: internal error"), http.StatusSeeOther)
		return
	}
	if err := s.applyImageDeploy(r.Context(), p, a, d, image); err != nil {
		http.Redirect(w, r, "/ui/projects/"+p.Name+"/apps/"+a.Name+"?err="+url.QueryEscape("deploy failed: "+err.Error()), http.StatusSeeOther)
		return
	}
	flash(w, "ok", "app created")
	uiRedirect(w, r, p, a)
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
	s.renderAppDetail(w, r, u, p, a, nil)
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

// chipData classifies a latest-deploy status into the chip's view model.
// Shared by renderAppDetail (initial render) and handleUIChip (the polling
// fragment) so "what counts as still building" lives in exactly one place.
func chipData(projectName, appName, status string) uiChipData {
	return uiChipData{
		ProjectName: projectName, AppName: appName, Status: status,
		Building: status == "building" || status == "deploying",
	}
}

// handleUIChip is the polling fragment app.html's "statuschip" block
// re-fetches every 3s while a deploy is building/deploying. It renders only
// that one template block, not the full page.
func (s *server) handleUIChip(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.uiProject(w, r, u)
	if !ok {
		return
	}
	a, ok := s.uiApp(w, r, p)
	if !ok {
		return
	}
	status := "never_deployed"
	if d, err := s.st.LatestDeployment(a.ID); err == nil {
		status = d.Status
	} else if !errors.Is(err, store.ErrNotFound) {
		log.Printf("ui chip: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.ExecuteTemplate(w, "statuschip", chipData(p.Name, a.Name, status)); err != nil {
		log.Printf("render statuschip: %v", err)
	}
}

// handleUIAddonURL is an on-demand reveal fragment: the project page never
// renders an addon's connection URL by default, only on this explicit
// hx-get, so credentials don't sit in the page's initial HTML/history.
func (s *server) handleUIAddonURL(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.uiProject(w, r, u)
	if !ok {
		return
	}
	ad, ok := s.uiAddon(w, p, r.URL.Query().Get("name"))
	if !ok {
		return
	}

	creds, err := s.unsealCreds(ad)
	if err != nil {
		if errors.Is(err, errSealerUnavailable) {
			http.Error(w, "sealer is not configured", http.StatusServiceUnavailable)
			return
		}
		log.Printf("ui addon url %s: %v", ad.Name, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	key, url := addonKeyURL(ad.Type, ad.Name, p.Namespace, creds)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, `<input class="input w-full font-mono text-xs" readonly value="%s">`,
		template.HTMLEscapeString(key+"="+url))
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

// uiRunRows builds the Runs card's view model from ListJobRuns' newest-first
// history.
func uiRunRows(runs []store.JobRun) []uiRunRow {
	rows := make([]uiRunRow, 0, len(runs))
	for _, run := range runs {
		exit := ""
		if run.ExitCode.Valid {
			exit = strconv.FormatInt(run.ExitCode.Int64, 10)
		}
		finished := ""
		if run.FinishedAt.Valid {
			finished = run.FinishedAt.String
		}
		rows = append(rows, uiRunRow{
			ID: run.ID, Status: run.Status, Nodes: run.Nodes, ExitCode: exit,
			StartedAt: run.StartedAt, FinishedAt: finished,
		})
	}
	return rows
}

func uiDeployRows(history []store.Deployment, limit int) []uiDeployRow {
	seqByID := make(map[string]int64, len(history))
	for _, d := range history {
		seqByID[d.ID] = d.Seq
	}
	if len(history) > limit {
		history = history[:limit]
	}
	rows := make([]uiDeployRow, 0, len(history))
	for _, d := range history {
		tag := d.ImageRef
		if idx := strings.LastIndex(d.ImageRef, ":"); idx >= 0 {
			tag = d.ImageRef[idx+1:]
		}
		rows = append(rows, uiDeployRow{
			ID: d.ID, Seq: d.Seq, Status: d.Status, ImageRef: d.ImageRef, ImageTag: tag,
			CreatedAt: d.CreatedAt, RolledBackFromSeq: seqByID[d.RolledBackFrom], Actor: "-",
		})
	}
	return rows
}

// renderAppDetail assembles app.html's full view model and renders it.
// extra is merged in last (overriding nothing app.html itself sets) — its
// only current use is handleUIWebhookEnable riding the freshly generated
// secret along on the same response, instead of a redirect (a redirect
// would have to carry the secret in the URL, which must never happen).
func (s *server) renderAppDetail(w http.ResponseWriter, r *http.Request, u store.User, p store.Project, a store.App, extra map[string]any) {
	status := "never_deployed"
	latestID := ""
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

	var pods []kube.PodInfo
	if s.kube != nil {
		if list, err := s.kube.AppPodInfos(r.Context(), p.Namespace, a.Name); err == nil {
			pods = list
		}
	}

	// Runs card is only meaningful for kind=job apps; nil for every other
	// kind (app.html gates the whole card on .App.Kind).
	var runRows []uiRunRow
	if a.Kind == "job" {
		runs, err := s.st.ListJobRuns(a.ID)
		if err != nil {
			log.Printf("ui app runs: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		runRows = uiRunRows(runs)
	}

	// Sweeps card, likewise job-only: sweepRows is the history table (newest
	// first); sweep is the most recent sweep's live detail (nil when the app
	// has none yet) — the card only ever shows one sweep's trial table, not
	// every past sweep's.
	var sweepRows []uiSweepRow
	var sweep *uiSweepData
	if a.Kind == "job" {
		sweeps, err := s.st.ListSweeps(a.ID)
		if err != nil {
			log.Printf("ui app sweeps: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		sweepRows = make([]uiSweepRow, 0, len(sweeps))
		for _, sw := range sweeps {
			trials, err := s.st.ListTrials(sw.ID)
			if err != nil {
				log.Printf("ui app sweep %s trials: %v", sw.ID, err)
				http.Error(w, "internal error", http.StatusInternalServerError)
				return
			}
			sweepRows = append(sweepRows, uiSweepRowFrom(sw, trials))
			if sweep == nil {
				d := uiSweepDataFrom(sw, trials)
				sweep = &d
			}
		}
	}

	url := s.appURL(a)
	internalURL := ""
	if a.Internal {
		internalURL = internalURLFor(a.Name, p.Namespace)
	}

	chip := chipData(p.Name, a.Name, status)
	csrf := s.csrf(w, r)
	if sweep != nil {
		sweep.ProjectName, sweep.AppName, sweep.CSRF = p.Name, a.Name, csrf
	}
	data := map[string]any{
		"User": u, "Project": p, "App": a,
		"Status": status, "LatestID": latestID, "URL": url, "InternalURL": internalURL,
		"Chip": chip, "Building": chip.Building,
		"Deploys": uiDeployRows(history, 10), "EnvKeys": envKeys,
		"IsGit":          a.SourceType == "git",
		"WebhookEnabled": a.WebhookSecret != nil,
		"WebhookURL":     "http://" + r.Host + webhookPath(p.Name, a.Name),
		"Domains": domains, "Volumes": volumes, "Warning": firstNonEmpty(r.URL.Query().Get("warn"), r.URL.Query().Get("err")),
		"Addons": attached, "ProjectAddons": projectAddons, "Metrics": metrics, "Pods": pods,
		"Runs": runRows, "TrainFrameworks": render.TrainFrameworks,
		"Sweeps": sweepRows, "Sweep": sweep,
		"CSRF": csrf, "IsAdmin": u.Role == "admin",
	}
	for k, v := range extra {
		data[k] = v
	}
	s.renderPage(w, "app.html", data)
}

// handleUIWebhookEnable is enableWebhook's UI twin: same core, but renders
// the app page directly (never redirects) so the freshly generated secret
// rides along on this one response — it must never be persisted in
// plaintext or appear in a URL/query string.
func (s *server) handleUIWebhookEnable(w http.ResponseWriter, r *http.Request, u store.User) {
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
	if err := s.st.SetWebhookSecret(a.ID, nil); err != nil {
		log.Printf("ui disable webhook: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	flash(w, "ok", "webhook disabled")
	uiRedirect(w, r, p, a)
}

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
	flash(w, "ok", "scaled")
	uiRedirect(w, r, p, a)
}

// handleUIRunCreate is startRun's UI twin: same shared core, redirect
// instead of a 202 JSON body. A missing live deployment, a bad nodes/
// framework override, or an over-budget nodes bump redirects back to the
// app page with ?err= (matches handleUICreateApp's deploy-failure idiom)
// instead of erroring the whole request.
func (s *server) handleUIRunCreate(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.uiProject(w, r, u)
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

	if _, err := s.startRun(r.Context(), p, a, opts); err != nil {
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
	p, ok := s.uiProject(w, r, u)
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
	flash(w, "ok", "health check saved")
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
	flash(w, "ok", "env saved")
	uiRedirect(w, r, p, a)
}

// handleUIEnvBulk is handleBulkSetEnv's UI twin: paste-in-only bulk upsert
// from a raw .env textarea, redirecting back to the app page on success.
func (s *server) handleUIEnvBulk(w http.ResponseWriter, r *http.Request, u store.User) {
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

	vars, err := parseDotenv(r.PostFormValue("dotenv"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if len(vars) == 0 {
		http.Error(w, "no KEY=VALUE pairs found", http.StatusBadRequest)
		return
	}

	if err := s.setAppEnvBulk(r.Context(), p, a, vars); err != nil {
		var ve *store.ValidationError
		switch {
		case errors.Is(err, errAppEjected):
			http.Error(w, err.Error(), http.StatusConflict)
		case errors.Is(err, errSealerUnavailable):
			http.Error(w, "sealer is not configured", http.StatusServiceUnavailable)
		case errors.As(err, &ve):
			http.Error(w, ve.Error(), http.StatusBadRequest)
		default:
			log.Printf("ui bulk set env: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
		}
		return
	}
	flash(w, "ok", "env vars saved")
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
	flash(w, "ok", "env var removed")
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
		flash(w, "ok", "domain added")
		http.Redirect(w, r, "/ui/projects/"+p.Name+"/apps/"+a.Name+"?warn="+url.QueryEscape(warning), http.StatusSeeOther)
		return
	}
	flash(w, "ok", "domain added")
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
	flash(w, "ok", "domain removed")
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
	flash(w, "ok", "volume added")
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
	flash(w, "ok", "volume removed")
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
	flash(w, "ok", "addon created")
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
	flash(w, "ok", "addon deleted")
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
		flash(w, "ok", "addon attached")
		http.Redirect(w, r, "/ui/projects/"+p.Name+"/apps/"+a.Name+"?warn="+url.QueryEscape(warning), http.StatusSeeOther)
		return
	}
	flash(w, "ok", "addon attached")
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
	flash(w, "ok", "addon detached")
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
	flash(w, "ok", "deploy started")
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
	// Unlike the JSON API, the UI form's hidden deploy_id field is always
	// populated by app.html — an empty or malformed value here is a bad
	// request, same as before ids became opaque nanoids (ParseInt of "" or
	// garbage failed the same way).
	deployID := r.PostFormValue("deploy_id")
	if !validDeployID(deployID) {
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
	flash(w, "ok", "rollback started")
	uiRedirect(w, r, p, a)
}

// handleUIAppDestroy is handleDeleteApp's UI twin: same destroyApp core,
// redirect back to the project page instead of a 204.
func (s *server) handleUIAppDestroy(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.uiProject(w, r, u)
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
	if err := s.destroyApp(r.Context(), p, a); err != nil {
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
	p, ok := s.uiProject(w, r, u)
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
	if _, _, err := s.ejectApp(p, a); err != nil {
		log.Printf("ui eject %s: %v", a.Name, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	flash(w, "ok", "app ejected")
	uiRedirect(w, r, p, a)
}

// handleUIDomainRetry is handleRetryDomain's UI twin: same retryDomain core,
// redirect back to the app page instead of a 202.
func (s *server) handleUIDomainRetry(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.uiProject(w, r, u)
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
	if s.certProviderName() != "builtin" {
		http.Error(w, "cert retry only applies to the builtin provider", http.StatusConflict)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	if err := s.retryDomain(p, a, r.PostFormValue("hostname")); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, "no such domain", http.StatusNotFound)
			return
		}
		log.Printf("ui domain retry: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	flash(w, "ok", "domain retry started")
	uiRedirect(w, r, p, a)
}

// handleUIAddonUpgrade is handleUpgradeAddon's UI twin: same upgradeAddon
// core, redirect back to the project page instead of a 200 body.
func (s *server) handleUIAddonUpgrade(w http.ResponseWriter, r *http.Request, u store.User) {
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
	version := r.PostFormValue("version")
	if version == "" {
		http.Error(w, "version is required", http.StatusBadRequest)
		return
	}
	if _, err := s.upgradeAddon(r.Context(), p, ad, version); err != nil {
		if errors.Is(err, errSealerUnavailable) {
			http.Error(w, "sealer is not configured", http.StatusServiceUnavailable)
			return
		}
		log.Printf("ui upgrade addon %s: %v", ad.Name, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	flash(w, "ok", "addon upgrade started")
	http.Redirect(w, r, "/ui/projects/"+p.Name, http.StatusSeeOther)
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
	var pwNote string
	switch r.URL.Query().Get("pw") {
	case "ok":
		pwNote = "password updated"
	case "invalid":
		pwNote = "password must be at least 8 characters"
	case "missing":
		pwNote = "no such user"
	}
	s.renderPage(w, "users.html", map[string]any{
		"User": u, "Users": users, "Invites": rows, "Self": u.ID,
		"CSRF": s.csrf(w, r), "IsAdmin": u.Role == "admin",
		"MailNote": mailNote, "PwNote": pwNote,
	})
}

// handleUIUserPassword is admin-only password reset for any user — no old
// password required (the admin isn't the account owner).
func (s *server) handleUIUserPassword(w http.ResponseWriter, r *http.Request, u store.User) {
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
	if err := s.st.UpdatePassword(id, r.PostFormValue("password")); err != nil {
		var ve *store.ValidationError
		switch {
		case errors.Is(err, store.ErrNotFound):
			http.Redirect(w, r, "/ui/users?pw=missing", http.StatusSeeOther)
		case errors.As(err, &ve):
			http.Redirect(w, r, "/ui/users?pw=invalid", http.StatusSeeOther)
		default:
			log.Printf("ui admin set password: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
		}
		return
	}
	flash(w, "ok", "password reset")
	http.Redirect(w, r, "/ui/users?pw=ok", http.StatusSeeOther)
}

// accountNote maps the account page's fixed ok/err query params to display
// strings — never the submitted value itself, so nothing user-supplied ever
// gets echoed back into the page.
func accountNote(r *http.Request) (note, errMsg string) {
	switch r.URL.Query().Get("ok") {
	case "password":
		note = "password changed"
	case "email":
		note = "email changed — use it on your next login"
	}
	switch r.URL.Query().Get("err") {
	case "wrong":
		errMsg = "current password is incorrect"
	case "invalid":
		errMsg = "invalid input (password min 8 chars, email required)"
	case "taken":
		errMsg = "that email is already in use"
	}
	return note, errMsg
}

// handleUIAccount is the self-service account page: change own password or
// email, both gated on the current password (checked by the POST handlers,
// not here).
func (s *server) handleUIAccount(w http.ResponseWriter, r *http.Request, u store.User) {
	note, errMsg := accountNote(r)
	s.renderPage(w, "account.html", map[string]any{
		"User": u, "CSRF": s.csrf(w, r), "IsAdmin": u.Role == "admin",
		"Note": note, "Error": errMsg,
	})
}

// handleUIAccountPassword is handleChangePassword's UI twin: same
// Authenticate-then-UpdatePassword core, redirect with a fixed ?ok=/?err=
// note instead of a JSON envelope.
func (s *server) handleUIAccountPassword(w http.ResponseWriter, r *http.Request, u store.User) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	if _, err := s.st.Authenticate(u.Email, r.PostFormValue("old")); errors.Is(err, store.ErrAuthFailed) {
		http.Redirect(w, r, "/ui/account?err=wrong", http.StatusSeeOther)
		return
	} else if err != nil {
		log.Printf("ui change password: authenticate: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if err := s.st.UpdatePassword(u.ID, r.PostFormValue("new")); err != nil {
		var ve *store.ValidationError
		if errors.As(err, &ve) {
			http.Redirect(w, r, "/ui/account?err=invalid", http.StatusSeeOther)
			return
		}
		log.Printf("ui change password: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	flash(w, "ok", "password changed")
	http.Redirect(w, r, "/ui/account?ok=password", http.StatusSeeOther)
}

// handleUIAccountEmail is handleChangeEmail's UI twin: same
// Authenticate-then-UpdateEmail core, redirect with a fixed ?ok=/?err= note
// instead of a JSON envelope.
func (s *server) handleUIAccountEmail(w http.ResponseWriter, r *http.Request, u store.User) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	if _, err := s.st.Authenticate(u.Email, r.PostFormValue("password")); errors.Is(err, store.ErrAuthFailed) {
		http.Redirect(w, r, "/ui/account?err=wrong", http.StatusSeeOther)
		return
	} else if err != nil {
		log.Printf("ui change email: authenticate: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if err := s.st.UpdateEmail(u.ID, r.PostFormValue("email")); err != nil {
		var ve *store.ValidationError
		switch {
		case strings.Contains(err.Error(), "UNIQUE constraint failed: users.email"):
			http.Redirect(w, r, "/ui/account?err=taken", http.StatusSeeOther)
		case errors.As(err, &ve):
			http.Redirect(w, r, "/ui/account?err=invalid", http.StatusSeeOther)
		default:
			log.Printf("ui change email: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
		}
		return
	}
	flash(w, "ok", "email changed")
	http.Redirect(w, r, "/ui/account?ok=email", http.StatusSeeOther)
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
	flash(w, "ok", "token revoked")
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
	flash(w, "ok", "invite created")
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
	flash(w, "ok", "app adopted")
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
	flash(w, "ok", "invite revoked")
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
	flash(w, "ok", "user deleted")
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
	flash(w, "ok", "override saved")
	uiRedirect(w, r, p, a)
}
