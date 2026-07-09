package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/sutantodadang/luncur/internal/store"
)

// certSecretName is the TLS Secret an app's domain cert is stored in —
// deterministic so render and issuance agree without coordination.
func certSecretName(app, hostname string) string {
	sum := sha256.Sum256([]byte(hostname))
	return "tls-" + app + "-" + hex.EncodeToString(sum[:])[:8]
}

// certProviderName reads the install-level provider setting.
func (s *server) certProviderName() string {
	v, err := s.st.GetSetting("cert_provider")
	if err != nil || v == "" {
		return "builtin"
	}
	return v
}

// dnsWarning checks that hostname resolves to the advertised IP. Returns a
// human warning ("" when all good). Never blocks domain creation. dns01
// softens the mismatch message: issuance validates over DNS-01, so a
// proxied domain (e.g. Cloudflare) resolving to the proxy's IPs is
// expected — claiming "issuance will fail" there is simply wrong.
func dnsWarning(ctx context.Context, hostname, wantIP string, dns01 bool) string {
	rctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	addrs, err := (&net.Resolver{}).LookupHost(rctx, hostname)
	if err != nil {
		return fmt.Sprintf("DNS lookup failed for %s — point an A record at %s", hostname, wantIP)
	}
	if slices.Contains(addrs, wantIP) {
		return ""
	}
	if dns01 {
		return fmt.Sprintf("%s resolves to %v, not %s — fine if the domain is proxied (e.g. Cloudflare); certs are validated over DNS-01", hostname, addrs, wantIP)
	}
	return fmt.Sprintf("%s resolves to %v, not %s — TLS issuance will fail until DNS points here", hostname, addrs, wantIP)
}

func domainJSON(d store.Domain, warning string) map[string]any {
	out := map[string]any{
		"hostname": d.Hostname, "cert_status": d.CertStatus,
		"cert_error": d.CertError, "cert_expires_at": d.CertExpiresAt,
	}
	if warning != "" {
		out["dns_warning"] = warning
	}
	return out
}

// addDomain is the shared core of handleAddDomain and its UI twin
// (handleUIDomainAdd): register the hostname, mark non-builtin providers as
// externally-issued, check DNS, opportunistically sync, and kick the cert
// manager. Returns the DNS warning ("" when all good); a non-nil error means
// the store rejected the hostname (invalid or duplicate).
// errWildcardNeedsDNS gates wildcard hostnames: they can only be validated
// over DNS-01, which needs a configured provider.
var errWildcardNeedsDNS = errors.New("wildcard domains require a configured dns_provider (settings: dns_provider)")

func (s *server) addDomain(ctx context.Context, p store.Project, a store.App, hostname string) (store.Domain, string, error) {
	if a.Ejected {
		return store.Domain{}, "", errAppEjected
	}
	if a.Kind != "web" {
		return store.Domain{}, "", &kindMismatchError{fmt.Errorf("domains are only supported for web apps")}
	}
	if a.Internal {
		return store.Domain{}, "", &kindMismatchError{fmt.Errorf("internal apps cannot have public domains")}
	}
	isWildcard := strings.HasPrefix(strings.TrimSpace(hostname), "*.")
	if isWildcard && s.dnsProviderName() == "none" {
		return store.Domain{}, "", errWildcardNeedsDNS
	}
	d, err := s.st.AddDomain(a.ID, hostname)
	if err != nil {
		return store.Domain{}, "", err
	}
	// Non-builtin providers own issuance — mark the row so the UI/CLI show
	// "external" instead of a forever-"none".
	if s.certProviderName() != "builtin" {
		if err := s.st.SetDomainCert(d.ID, "external", "", ""); err == nil {
			d.CertStatus = "external"
		}
	}
	warning := ""
	if !isWildcard {
		// A wildcard can't be resolved directly; skip the A-record check.
		warning = dnsWarning(ctx, d.Hostname, s.externalIP, s.dnsProviderName() != "none")
	}
	s.syncIfLive(ctx, p, a)
	s.kickCerts(p, a, d)
	return d, warning, nil
}

func (s *server) handleAddDomain(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.requireProjectWrite(w, u, r.PathValue("project"))
	if !ok {
		return
	}
	a, ok := s.requireApp(w, p, r.PathValue("app"))
	if !ok {
		return
	}
	var req struct {
		Hostname string `json:"hostname"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	d, warning, err := s.addDomain(r.Context(), p, a, req.Hostname)
	if err != nil {
		var ke *kindMismatchError
		switch {
		case errors.Is(err, errAppEjected):
			writeError(w, http.StatusConflict, "app_ejected", errAppEjected.Error())
		case errors.As(err, &ke):
			writeError(w, http.StatusBadRequest, "kind_mismatch", ke.Error())
		default:
			writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		}
		return
	}
	writeJSON(w, http.StatusCreated, domainJSON(d, warning))
}

func (s *server) handleListDomains(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.requireProject(w, u, r.PathValue("project"))
	if !ok {
		return
	}
	a, ok := s.requireApp(w, p, r.PathValue("app"))
	if !ok {
		return
	}
	list, err := s.st.ListDomains(a.ID)
	if err != nil {
		log.Printf("list domains: %v", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	out := make([]map[string]any, 0, len(list))
	for _, d := range list {
		out = append(out, domainJSON(d, ""))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *server) handleDeleteDomain(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.requireProjectWrite(w, u, r.PathValue("project"))
	if !ok {
		return
	}
	a, ok := s.requireApp(w, p, r.PathValue("app"))
	if !ok {
		return
	}
	if s.refuseEjected(w, a) {
		return
	}
	if err := s.st.DeleteDomain(a.ID, r.PathValue("hostname")); errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "no such domain")
		return
	} else if err != nil {
		log.Printf("delete domain: %v", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	s.syncIfLive(r.Context(), p, a)
	w.WriteHeader(http.StatusNoContent)
}

// retryDomain is handleRetryDomain's and handleUIDomainRetry's shared core:
// reset hostname's cert status to "none" and nudge the cert manager. Returns
// store.ErrNotFound if hostname isn't one of the app's domains. The caller
// must have already checked the app isn't ejected and the provider is
// "builtin".
func (s *server) retryDomain(p store.Project, a store.App, hostname string) error {
	list, err := s.st.ListDomains(a.ID)
	if err != nil {
		return err
	}
	for _, d := range list {
		if d.Hostname == hostname {
			if err := s.st.SetDomainCert(d.ID, "none", "", ""); err != nil {
				return err
			}
			d.CertStatus, d.CertError = "none", ""
			s.kickCerts(p, a, d)
			return nil
		}
	}
	return store.ErrNotFound
}

func (s *server) handleRetryDomain(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.requireProjectWrite(w, u, r.PathValue("project"))
	if !ok {
		return
	}
	a, ok := s.requireApp(w, p, r.PathValue("app"))
	if !ok {
		return
	}
	if s.refuseEjected(w, a) {
		return
	}
	if s.certProviderName() != "builtin" {
		writeError(w, http.StatusConflict, "wrong_provider", "cert retry only applies to the builtin provider")
		return
	}
	if err := s.retryDomain(p, a, r.PathValue("hostname")); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "no such domain")
			return
		}
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

// kickCerts nudges the cert manager about a domain. Wired by the builtin
// provider's manager; a nil manager (tests, non-builtin providers) is a
// no-op.
func (s *server) kickCerts(p store.Project, a store.App, d store.Domain) {
	if s.certs != nil {
		s.certs.Kick(p, a, d)
	}
}
