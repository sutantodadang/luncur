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
// human warning ("" when all good). Never blocks domain creation.
func dnsWarning(ctx context.Context, hostname, wantIP string) string {
	rctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	addrs, err := (&net.Resolver{}).LookupHost(rctx, hostname)
	if err != nil {
		return fmt.Sprintf("DNS lookup failed for %s — point an A record at %s", hostname, wantIP)
	}
	for _, a := range addrs {
		if a == wantIP {
			return ""
		}
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

func (s *server) handleAddDomain(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.requireProject(w, u, r.PathValue("project"))
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
	d, err := s.st.AddDomain(a.ID, req.Hostname)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	// Non-builtin providers own issuance — mark the row so the UI/CLI show
	// "external" instead of a forever-"none".
	if s.certProviderName() != "builtin" {
		if err := s.st.SetDomainCert(d.ID, "external", "", ""); err == nil {
			d.CertStatus = "external"
		}
	}
	warning := dnsWarning(r.Context(), d.Hostname, s.externalIP)
	s.syncIfLive(r.Context(), p, a)
	s.kickCerts(p, a, d) // Task 6 wires this; stub in this task (see below)
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
	p, ok := s.requireProject(w, u, r.PathValue("project"))
	if !ok {
		return
	}
	a, ok := s.requireApp(w, p, r.PathValue("app"))
	if !ok {
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

func (s *server) handleRetryDomain(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.requireProject(w, u, r.PathValue("project"))
	if !ok {
		return
	}
	a, ok := s.requireApp(w, p, r.PathValue("app"))
	if !ok {
		return
	}
	if s.certProviderName() != "builtin" {
		writeError(w, http.StatusConflict, "wrong_provider", "cert retry only applies to the builtin provider")
		return
	}
	list, err := s.st.ListDomains(a.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	for _, d := range list {
		if d.Hostname == r.PathValue("hostname") {
			if err := s.st.SetDomainCert(d.ID, "none", "", ""); err != nil {
				writeError(w, http.StatusInternalServerError, "internal", "internal error")
				return
			}
			d.CertStatus, d.CertError = "none", ""
			s.kickCerts(p, a, d)
			w.WriteHeader(http.StatusAccepted)
			return
		}
	}
	writeError(w, http.StatusNotFound, "not_found", "no such domain")
}

// kickCerts nudges the cert manager about a domain. Wired by the builtin
// provider's manager; a nil manager (tests, non-builtin providers) is a
// no-op.
func (s *server) kickCerts(p store.Project, a store.App, d store.Domain) {
	if s.certs != nil {
		s.certs.Kick(p, a, d)
	}
}
