package server

import (
	"errors"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/sutantodadang/luncur/internal/store"
)

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
	p, err := s.st.CreateProject(r.PostFormValue("name"))
	if err != nil {
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
	// See handleCreateProject: seeds the resolvable default environment
	// every project needs for requireEnv/uiEnv-style env-less resolution.
	if err := s.st.SeedProjectEnvironments(p.ID); err != nil {
		log.Printf("ui create project: seed environments: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
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
	role := r.PostFormValue("role")
	if role == "" {
		role = "member"
	}
	if err := s.st.AddMember(p.ID, member.ID, role); err != nil {
		var ve *store.ValidationError
		if errors.As(err, &ve) {
			http.Redirect(w, r, "/ui/projects/"+p.Name+"?err="+url.QueryEscape(ve.Error()), http.StatusSeeOther)
			return
		}
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
	if err := s.deleteProject(r.Context(), p); err != nil {
		log.Printf("ui delete project: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	flash(w, "ok", "project deleted")
	http.Redirect(w, r, "/ui/", http.StatusSeeOther)
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

// handleUIQuota is setProjectQuota's UI twin: same shared store+kube core as
// handleSetProjectQuota, form-POST instead of JSON, redirect back to the
// project page with ?err= on failure (mirrors handleUIGPUQuota). Blank or 0
// in either field means unlimited for that resource.
func (s *server) handleUIQuota(w http.ResponseWriter, r *http.Request, u store.User) {
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
	parseOr0 := func(v string) (int64, error) {
		if v == "" {
			return 0, nil
		}
		return strconv.ParseInt(v, 10, 64)
	}
	cpu, err := parseOr0(r.PostFormValue("cpu"))
	if err != nil {
		http.Redirect(w, r, "/ui/projects/"+p.Name+"?err="+url.QueryEscape("invalid cpu quota"), http.StatusSeeOther)
		return
	}
	mem, err := parseOr0(r.PostFormValue("memory"))
	if err != nil {
		http.Redirect(w, r, "/ui/projects/"+p.Name+"?err="+url.QueryEscape("invalid memory quota"), http.StatusSeeOther)
		return
	}
	if err := s.setProjectQuota(r.Context(), p, cpu, mem); err != nil {
		http.Redirect(w, r, "/ui/projects/"+p.Name+"?err="+url.QueryEscape(err.Error()), http.StatusSeeOther)
		return
	}
	flash(w, "ok", "quota saved")
	http.Redirect(w, r, "/ui/projects/"+p.Name, http.StatusSeeOther)
}

// handleUIPreviewDelete is handleDeletePreview's UI twin: same
// teardownPreview core, redirect instead of a 204. 404s (plain text) on an
// unknown name or a standing environment named by mistake, mirroring
// handleDeletePreview's own guard.
func (s *server) handleUIPreviewDelete(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.uiProjectWrite(w, r, u)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	name := r.PostFormValue("name")
	env, err := s.st.GetEnvironment(p.ID, name)
	if errors.Is(err, store.ErrNotFound) || (err == nil && env.Kind != "preview") {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if err != nil {
		log.Printf("ui delete preview: get environment: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if err := s.teardownPreview(r.Context(), p, env); err != nil {
		log.Printf("ui delete preview: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	flash(w, "ok", "preview deleted")
	http.Redirect(w, r, "/ui/projects/"+p.Name, http.StatusSeeOther)
}
