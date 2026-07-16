package server

import (
	"errors"
	"log"
	"net/http"
	"net/url"

	"github.com/sutantodadang/luncur/internal/store"
)

// handleUIDomainAdd is handleAddDomain's UI twin: same shared addDomain
// core, but redirects back to the app page instead of returning JSON. A
// non-empty DNS warning rides along as a ?warn= query param so the page can
// show it once, on the redirect target, without persisting it anywhere.
func (s *server) handleUIDomainAdd(w http.ResponseWriter, r *http.Request, u store.User) {
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
	_, warning, err := s.addDomain(r.Context(), p, env, a, r.PostFormValue("hostname"))
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
	env, ok := s.uiAppEnv(w, a)
	if !ok {
		return
	}
	s.syncIfLive(r.Context(), p, env, a)
	flash(w, "ok", "domain removed")
	uiRedirect(w, r, p, a)
}

// handleUIDomainRetry is handleRetryDomain's UI twin: same retryDomain core,
// redirect back to the app page instead of a 202.
func (s *server) handleUIDomainRetry(w http.ResponseWriter, r *http.Request, u store.User) {
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
	if s.certProviderName() != "builtin" {
		http.Error(w, "cert retry only applies to the builtin provider", http.StatusConflict)
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
	if err := s.retryDomain(p, env, a, r.PostFormValue("hostname")); err != nil {
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
