package server

import (
	"errors"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"strconv"

	"github.com/sutantodadang/luncur/internal/store"
)

// handleUIAddonURL is an on-demand reveal fragment: the project page never
// renders an addon's connection URL by default, only on this explicit
// hx-get, so credentials don't sit in the page's initial HTML/history.
func (s *server) handleUIAddonURL(w http.ResponseWriter, r *http.Request, u store.User) {
	// Credentials in the reveal fragment: viewers are blocked like writes.
	p, ok := s.uiProjectWrite(w, r, u)
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

	env, err := s.st.GetEnvironment(p.ID, p.DefaultEnv)
	if err != nil {
		log.Printf("ui addon url %s: get environment: %v", ad.Name, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	key, url := addonKeyURL(ad.Type, ad.Name, env.Namespace, creds)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, `<input class="input w-full font-mono text-xs" readonly value="%s">`,
		template.HTMLEscapeString(key+"="+url))
}

// handleUIAddonCreate is handleCreateAddon's UI twin: same shared
// createAddon core (unattached — the project page's form has no app
// picker), redirect instead of a 201 body.
func (s *server) handleUIAddonCreate(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.uiProjectWrite(w, r, u)
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

	env, ok := s.uiDefaultEnv(w, p)
	if !ok {
		return
	}
	if _, err := s.createAddon(r.Context(), p, env, r.PostFormValue("type"), r.PostFormValue("name"), r.PostFormValue("version"), sizeGB, ""); err != nil {
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
	p, ok := s.uiProjectWrite(w, r, u)
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

	env, ok := s.uiDefaultEnv(w, p)
	if !ok {
		return
	}
	if err := s.removeAddon(r.Context(), p, env, ad, force, keepData); err != nil {
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
	ad, ok := s.uiAddon(w, p, r.PostFormValue("name"))
	if !ok {
		return
	}

	env, ok := s.uiAppEnv(w, a)
	if !ok {
		return
	}
	warning, err := s.attachAddon(r.Context(), p, env, ad, a.Name)
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
	env, ok := s.uiAppEnv(w, a)
	if !ok {
		return
	}
	s.syncIfLive(r.Context(), p, env, a)
	flash(w, "ok", "addon detached")
	uiRedirect(w, r, p, a)
}

// handleUIAddonUpgrade is handleUpgradeAddon's UI twin: same upgradeAddon
// core, redirect back to the project page instead of a 200 body.
func (s *server) handleUIAddonUpgrade(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.uiProjectWrite(w, r, u)
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
	env, ok := s.uiDefaultEnv(w, p)
	if !ok {
		return
	}
	version := r.PostFormValue("version")
	if version == "" {
		http.Error(w, "version is required", http.StatusBadRequest)
		return
	}
	if _, err := s.upgradeAddon(r.Context(), p, env, ad, version); err != nil {
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
