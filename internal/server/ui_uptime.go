package server

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/sutantodadang/luncur/internal/store"
)

// uiUptime is the Observe tab's uptime card data (web apps only).
func (s *server) uiUptime(p store.Project, env store.Environment, a store.App, tab uiTab) *uptimeView {
	if tab != tabObserve || (a.Kind != "" && a.Kind != "web") {
		return nil
	}
	v, err := s.appUptimeView(p, env, a, 30)
	if err != nil {
		return nil
	}
	return &v
}

// uiStatusPageView is the project page's status page card.
type uiStatusPageView struct {
	store.StatusPage
	Published bool
	AppsCSV   string
}

func (s *server) uiStatusPage(p store.Project) uiStatusPageView {
	sp, err := s.st.GetStatusPage(p.ID)
	if err != nil {
		return uiStatusPageView{StatusPage: store.StatusPage{Slug: p.Name}}
	}
	return uiStatusPageView{StatusPage: sp, Published: sp.Enabled, AppsCSV: strings.Join(sp.Apps, ",")}
}

func (s *server) uiIncidents(p store.Project) []store.Incident {
	list, err := s.st.ListIncidents(p.ID, 10)
	if err != nil {
		return nil
	}
	return list
}

// handleUIUptime is handlePutUptime's UI twin (Observe tab).
func (s *server) handleUIUptime(w http.ResponseWriter, r *http.Request, u store.User) {
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
	enabled := r.PostFormValue("enabled") != ""
	external := r.PostFormValue("external") != ""
	path := r.PostFormValue("path")
	if err := s.setUptime(a, &enabled, &external, &path); err != nil {
		flash(w, "err", err.Error())
	} else {
		flash(w, "ok", "uptime check saved")
	}
	uiRedirect(w, r, p, a, tabObserve)
}

// handleUIStatusPage publishes/unpublishes the project's status page.
func (s *server) handleUIStatusPage(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.uiProjectWrite(w, r, u)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	var err error
	if r.PostFormValue("unpublish") != "" {
		err = s.st.DeleteStatusPage(p.ID)
		s.invalidateStatus()
	} else {
		_, err = s.setStatusPage(p, strings.TrimSpace(r.PostFormValue("slug")), r.PostFormValue("title"), strings.Split(r.PostFormValue("apps"), ","), true)
	}
	if err != nil {
		flash(w, "err", err.Error())
	} else {
		flash(w, "ok", "status page saved")
	}
	http.Redirect(w, r, "/ui/projects/"+p.Name, http.StatusSeeOther)
}

// handleUIIncident opens, updates or resolves an incident.
func (s *server) handleUIIncident(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.uiProjectWrite(w, r, u)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	var err error
	switch r.PostFormValue("action") {
	case "open":
		_, err = s.openIncident(p, r.PostFormValue("title"), r.PostFormValue("app"), r.PostFormValue("body"))
	case "note", "resolve":
		id, perr := strconv.ParseInt(r.PostFormValue("id"), 10, 64)
		in, gerr := s.st.GetIncident(id)
		switch {
		case perr != nil || gerr != nil || in.ProjectID != p.ID:
			err = store.ErrNotFound
		case r.PostFormValue("action") == "note":
			err = s.st.AddIncidentUpdate(id, r.PostFormValue("body"))
		default:
			err = s.st.ResolveIncident(id)
		}
	default:
		http.Error(w, "unknown action", http.StatusBadRequest)
		return
	}
	if err != nil {
		flash(w, "err", err.Error())
	} else {
		flash(w, "ok", "incident saved")
		s.invalidateStatus()
	}
	http.Redirect(w, r, "/ui/projects/"+p.Name, http.StatusSeeOther)
}
