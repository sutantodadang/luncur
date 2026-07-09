package server

import (
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"github.com/sutantodadang/luncur/internal/render"
	"github.com/sutantodadang/luncur/internal/store"
	"github.com/sutantodadang/luncur/internal/up"
)

const (
	fwdCookie     = "luncur_fwd"
	fwdAuthPath   = "/__luncur/auth"
	fwdHandoffTTL = 60 * time.Second
	fwdSessionTTL = 4 * time.Hour
)

// forwardParent is the DNS parent forward hosts hang under: the custom
// panel domain when set, else the sslip.io fallback.
func (s *server) forwardParent() string {
	host, err := s.st.GetSetting("panel_domain")
	if err == nil && host != "" {
		return host
	}
	return s.externalIP + ".sslip.io"
}

func (s *server) forwardHost(p store.Project, a store.App) string {
	return a.Name + "--" + p.Name + "." + s.forwardParent()
}

// forwardAppFromHost resolves a request Host to its project+app when the
// host is a forward host ({app}--{project}.<parent>). Leak-nothing: any
// mismatch is a plain false.
func (s *server) forwardAppFromHost(host string) (store.Project, store.App, bool) {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	label, found := strings.CutSuffix(host, "."+s.forwardParent())
	if !found || label == "" || strings.Contains(label, ".") {
		return store.Project{}, store.App{}, false
	}
	appName, projName, ok := strings.Cut(label, "--")
	if !ok || appName == "" || projName == "" || strings.Contains(projName, "--") {
		return store.Project{}, store.App{}, false
	}
	p, err := s.st.GetProject(projName)
	if err != nil {
		return store.Project{}, store.App{}, false
	}
	a, err := s.st.GetApp(p.ID, appName)
	if err != nil || (a.Kind != "" && a.Kind != "web") {
		return store.Project{}, store.App{}, false
	}
	return p, a, true
}

// fwdProxyTarget is split out so tests can point the proxy at an
// httptest.Server instead of cluster DNS (same pattern as sweepMLflowURLFn).
func fwdProxyTarget(p store.Project, a store.App) *url.URL {
	// Service port is always 80 (targetPort = the app's container port).
	return &url.URL{Scheme: "http", Host: fmt.Sprintf("%s.%s:80", a.Name, p.Namespace)}
}

// handleForwardHost serves every request whose Host is a forward host:
// the auth handoff sets the cookie; everything else needs the cookie and
// is reverse-proxied to the app's Service.
func (s *server) handleForwardHost(w http.ResponseWriter, r *http.Request, p store.Project, a store.App) {
	if r.URL.Path == fwdAuthPath {
		ip, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			ip = r.RemoteAddr
		}
		if !s.loginLimiter.allow(ip) {
			http.Error(w, "too many attempts, retry later", http.StatusTooManyRequests)
			return
		}
		appID, ok := verifyFwdToken(s.fwdKey, r.URL.Query().Get("t"), s.nowFn())
		if !ok || appID != a.ID {
			http.Error(w, "invalid or expired link — reopen from the luncur panel", http.StatusForbidden)
			return
		}
		http.SetCookie(w, &http.Cookie{
			Name:     fwdCookie,
			Value:    mintFwdToken(s.fwdKey, a.ID, s.nowFn().Add(fwdSessionTTL)),
			Path:     "/",
			HttpOnly: true,
			Secure:   r.TLS != nil,
			SameSite: http.SameSiteLaxMode,
			MaxAge:   int(fwdSessionTTL.Seconds()),
		})
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}

	ck, err := r.Cookie(fwdCookie)
	if err != nil {
		http.Error(w, "not signed in — use the open button in the luncur panel", http.StatusUnauthorized)
		return
	}
	appID, ok := verifyFwdToken(s.fwdKey, ck.Value, s.nowFn())
	if !ok || appID != a.ID {
		http.Error(w, "session expired — reopen from the luncur panel", http.StatusUnauthorized)
		return
	}
	httputil.NewSingleHostReverseProxy(s.fwdProxyTargetFn(p, a)).ServeHTTP(w, r)
}

// handleUIAppOpen is the panel-side half of the handoff: membership-checked
// on the panel host (via uiProject/uiApp, the same helpers handleUIApp
// uses), applies the forward Ingress (idempotent), then redirects to the
// forward host with a short-lived token.
func (s *server) handleUIAppOpen(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.uiProject(w, r, u)
	if !ok {
		return
	}
	a, ok := s.uiApp(w, r, p)
	if !ok {
		return
	}
	if a.Kind != "" && a.Kind != "web" {
		http.Error(w, "only web apps can be opened", http.StatusConflict)
		return
	}
	if strings.Contains(a.Name, "--") || strings.Contains(p.Name, "--") {
		http.Error(w, "names containing -- cannot use one-click open; use: luncur forward", http.StatusBadRequest)
		return
	}
	host := s.forwardHost(p, a)
	if s.kube != nil {
		obj, err := up.ForwardIngress(host, a.Name, p.Namespace)
		if err == nil {
			err = s.kube.Apply(r.Context(), s.systemNamespace, []render.Object{obj})
		}
		if err != nil {
			log.Printf("forward ingress apply: %v", err)
			http.Error(w, "could not publish forward route", http.StatusBadGateway)
			return
		}
	}
	tok := mintFwdToken(s.fwdKey, a.ID, s.nowFn().Add(fwdHandoffTTL))
	http.Redirect(w, r, "http://"+host+fwdAuthPath+"?t="+url.QueryEscape(tok), http.StatusSeeOther)
}
