package server

import (
	"errors"
	"log"
	"net/http"

	"github.com/sutantodadang/luncur/internal/render"
	"github.com/sutantodadang/luncur/internal/store"
)

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
func (s *server) editDoc(p store.Project, env store.Environment, a store.App, kind string, withOverrides bool) ([]byte, error) {
	image, err := s.appImage(a)
	if err != nil {
		return nil, err
	}
	rendered, err := s.renderApp(p, env, a, image, withOverrides)
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

	env, ok := s.uiAppEnv(w, a)
	if !ok {
		return
	}
	doc, err := s.editDoc(p, env, a, kind, true)
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
	p, ok := s.uiProjectWrite(w, r, u)
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

	env, ok := s.uiAppEnv(w, a)
	if !ok {
		return
	}
	baseDoc, err := s.editDoc(p, env, a, kind, false)
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

	if err := s.setOverride(r.Context(), p, env, a, kind, patch); err != nil {
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
