package server

import (
	"context"
	"errors"
	"regexp"
	"strings"

	"github.com/sutantodadang/luncur/internal/render"
	"github.com/sutantodadang/luncur/internal/store"
	"github.com/sutantodadang/luncur/internal/up"
)

// panelTLSSecret is the TLS Secret luncur's own panel Ingress uses once its
// custom panel_domain has an issued builtin cert.
const panelTLSSecret = "luncur-panel-tls"

// panelHostnameRe mirrors store.AddDomain's hostname rule (see
// internal/store/domains.go's hostnameRe) minus wildcard support — the panel
// domain is a single custom host, never a wildcard.
var panelHostnameRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$`)

// validPanelDomain accepts "" (clears the custom panel domain) or a
// lowercase, non-wildcard hostname.
func validPanelDomain(v string) bool {
	if v == "" {
		return true
	}
	if v != strings.ToLower(v) || strings.Contains(v, "*") {
		return false
	}
	return panelHostnameRe.MatchString(v)
}

// applyPanelIngress (re)applies luncur's own panel Ingress: the base
// sslip.io host plus the configured panel_domain, with a TLS block once its
// builtin cert is issued. A nil kube client (tests, no cluster wired) is a
// no-op.
func (s *server) applyPanelIngress(ctx context.Context) error {
	if s.kube == nil {
		return nil
	}
	host, err := s.st.GetSetting("panel_domain")
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	tlsSecret := ""
	if host != "" {
		status, _ := s.st.GetSetting("panel_cert_status")
		if status == "issued" {
			tlsSecret = panelTLSSecret
		}
	}
	obj, err := up.PanelIngress(s.externalIP, host, tlsSecret)
	if err != nil {
		return err
	}
	return s.kube.Apply(ctx, s.systemNamespace, []render.Object{obj})
}

// panelDomainChanged runs after panel_domain is written via setSetting —
// both handleSetSetting (JSON API) and handleUISettingsSet (UI form) call it
// right after a successful write. It resets the panel's internal cert-state
// settings, re-applies the Ingress, and nudges the builtin cert manager (or
// marks the domain "external" when a different provider owns TLS).
func (s *server) panelDomainChanged(ctx context.Context) error {
	host, err := s.st.GetSetting("panel_domain")
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}

	switch {
	case host == "":
		for _, k := range []string{"panel_cert_status", "panel_cert_error", "panel_cert_expires_at"} {
			if err := s.st.SetSetting(k, ""); err != nil {
				return err
			}
		}
	case s.certProviderName() == "builtin":
		if err := s.st.SetSetting("panel_cert_status", "none"); err != nil {
			return err
		}
		if err := s.st.SetSetting("panel_cert_error", ""); err != nil {
			return err
		}
	default:
		// Traefik/cert-manager own TLS for this host; nothing for the
		// builtin manager to track.
		if err := s.st.SetSetting("panel_cert_status", "external"); err != nil {
			return err
		}
	}

	if err := s.applyPanelIngress(ctx); err != nil {
		return err
	}

	if host != "" && s.certProviderName() == "builtin" && s.certs != nil {
		s.certs.KickPanel(host)
	}
	return nil
}
