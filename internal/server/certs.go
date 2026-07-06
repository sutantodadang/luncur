package server

import (
	"context"
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/sutantodadang/luncur/internal/acme"
	"github.com/sutantodadang/luncur/internal/dns"
	"github.com/sutantodadang/luncur/internal/render"
	"github.com/sutantodadang/luncur/internal/store"
)

const acmeAccountSecret = "luncur-acme-account"
const challengeIngress = "luncur-acme"

type certJob struct {
	p store.Project
	a store.App
	d store.Domain

	// panelHost, when non-empty, diverts issue() to issuePanel: this job is
	// luncur's own panel custom domain, not an app's, so p/a/d are unset.
	panelHost string
}

// certManager drives builtin-provider cert issuance and renewal.
type certManager struct {
	s            *server
	directoryURL string
	challenges   *acme.Challenges

	jobs chan certJob

	// lookupTXT overrides the DNS-01 solver's propagation lookup; nil =
	// the solver's default authoritative-NS query. Tests inject an
	// instant one.
	lookupTXT func(ctx context.Context, fqdn string) ([]string, error)

	mu           sync.Mutex
	pendingHosts map[string]bool // hosts currently in the challenge Ingress
}

// newDNS01Solver builds the dns-01 solver for one issuance.
func (m *certManager) newDNS01Solver(prov dns.Provider) *acme.DNS01Solver {
	return &acme.DNS01Solver{Provider: prov, LookupTXT: m.lookupTXT}
}

func newCertManager(s *server, directoryURL string) *certManager {
	if directoryURL == "" {
		if v, err := s.st.GetSetting("acme_directory"); err == nil && v != "" {
			directoryURL = v
		} else {
			directoryURL = acme.LetsEncryptDirectory
		}
	}
	return &certManager{
		s: s, directoryURL: directoryURL,
		challenges:   acme.NewChallenges(),
		jobs:         make(chan certJob, 64),
		pendingHosts: map[string]bool{},
	}
}

func (m *certManager) Challenges() http.Handler { return m.challenges }

// Kick enqueues issuance for a domain; drops silently when the queue is
// full (the renewal sweep will pick it up again).
func (m *certManager) Kick(p store.Project, a store.App, d store.Domain) {
	select {
	case m.jobs <- certJob{p: p, a: a, d: d}:
	default:
	}
}

// KickPanel enqueues issuance for luncur's own panel custom domain; same
// drop-if-full semantics as Kick (the daily sweep will retry).
func (m *certManager) KickPanel(host string) {
	select {
	case m.jobs <- certJob{panelHost: host}:
	default:
	}
}

// Run processes issuance jobs and sweeps daily for renewals until ctx ends.
func (m *certManager) Run(ctx context.Context) {
	tick := time.NewTicker(24 * time.Hour)
	defer tick.Stop()
	m.sweep(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case j := <-m.jobs:
			m.issue(ctx, j)
		case <-tick.C:
			m.sweep(ctx)
		}
	}
}

// sweep re-enqueues unissued domains and soon-to-expire certs. For the
// cert-manager provider, issuance isn't luncur's job — instead it reads the
// expiry back from the TLS Secret cert-manager maintains.
func (m *certManager) sweep(ctx context.Context) {
	domains, err := m.s.st.AllDomains()
	if err != nil {
		log.Printf("cert sweep: %v", err)
		return
	}
	provider := m.s.certProviderName()
	for _, d := range domains {
		if provider == "cert-manager" && d.CertStatus == "external" {
			m.readbackExpiry(ctx, d)
			continue
		}
		renew := false
		switch d.CertStatus {
		case "none", "pending":
			renew = true
		case "issued":
			if exp, err := time.Parse(time.RFC3339, d.CertExpiresAt); err == nil {
				renew = acme.NeedsRenewal(exp, time.Now())
			}
		}
		if !renew {
			continue
		}
		p, a, err := m.s.projectAppForDomain(d)
		if err != nil {
			log.Printf("cert sweep domain %s: %v", d.Hostname, err)
			continue
		}
		m.Kick(p, a, d)
	}

	// Panel custom domain: same builtin-only lifecycle as an app domain
	// above (renew "none"/"pending", renew "issued" nearing expiry), but
	// state lives in panel_cert_* settings rather than a domains row.
	if provider == "builtin" {
		host, _ := m.s.st.GetSetting("panel_domain")
		if host != "" {
			status, _ := m.s.st.GetSetting("panel_cert_status")
			renew := false
			switch status {
			case "none", "pending":
				renew = true
			case "issued":
				if exp, err := m.s.st.GetSetting("panel_cert_expires_at"); err == nil {
					if t, err := time.Parse(time.RFC3339, exp); err == nil {
						renew = acme.NeedsRenewal(t, time.Now())
					}
				}
			}
			if renew {
				m.KickPanel(host)
			}
		}
	}
}

// issue runs one domain's issuance end to end.
func (m *certManager) issue(ctx context.Context, j certJob) {
	if m.s.kube == nil {
		return
	}
	if j.panelHost != "" {
		m.issuePanel(ctx, j.panelHost)
		return
	}
	st := m.s.st
	fail := func(err error) {
		log.Printf("cert %s: %v", j.d.Hostname, err)
		if e := st.SetDomainCert(j.d.ID, "failed", err.Error(), ""); e != nil {
			log.Printf("mark cert failed: %v", e)
		}
		m.s.notify(notifyEvent{Event: "cert_failed", Project: j.p.Name, App: j.a.Name, URL: j.d.Hostname, Err: err.Error()})
	}
	if err := st.SetDomainCert(j.d.ID, "pending", "", j.d.CertExpiresAt); err != nil {
		fail(err)
		return
	}

	key, err := m.accountKey(ctx)
	if err != nil {
		fail(fmt.Errorf("acme account key: %w", err))
		return
	}

	// DNS-01 when the hostname is a wildcard (HTTP-01 can't validate it)
	// or a dns provider is configured for this install; HTTP-01 with the
	// challenge Ingress otherwise.
	useDNS := strings.HasPrefix(j.d.Hostname, "*.") || m.s.dnsProviderName() != "none"
	var solver acme.Solver
	if useDNS {
		prov, err := m.s.dnsProvider()
		if err != nil {
			fail(fmt.Errorf("dns provider: %w", err))
			return
		}
		solver = m.newDNS01Solver(prov)
	} else {
		if err := m.setChallengeHost(ctx, j.d.Hostname, true); err != nil {
			fail(fmt.Errorf("challenge ingress: %w", err))
			return
		}
		defer func() {
			if err := m.setChallengeHost(ctx, j.d.Hostname, false); err != nil {
				log.Printf("remove challenge host %s: %v", j.d.Hostname, err)
			}
		}()
	}

	email, _ := st.GetSetting("acme_email")
	iss := &acme.Issuer{
		DirectoryURL: m.directoryURL, AccountKey: key,
		Email: email, Challenges: m.challenges, Solver: solver,
	}
	ictx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	certPEM, keyPEM, notAfter, err := iss.Issue(ictx, j.d.Hostname)
	if err != nil {
		fail(err)
		return
	}

	secJSON, err := json.Marshal(map[string]any{
		"apiVersion": "v1", "kind": "Secret",
		"metadata": map[string]any{
			"name": certSecretName(j.a.Name, j.d.Hostname), "namespace": j.p.Namespace,
			"labels": map[string]string{"app.kubernetes.io/managed-by": "luncur"},
		},
		"type": "kubernetes.io/tls",
		"stringData": map[string]string{
			"tls.crt": string(certPEM), "tls.key": string(keyPEM),
		},
	})
	if err != nil {
		fail(err)
		return
	}
	if err := m.s.kube.Apply(ctx, j.p.Namespace, []render.Object{{Kind: "Secret", JSON: secJSON}}); err != nil {
		fail(fmt.Errorf("apply tls secret: %w", err))
		return
	}
	if err := st.SetDomainCert(j.d.ID, "issued", "", notAfter.UTC().Format(time.RFC3339)); err != nil {
		fail(err)
		return
	}
	if err := m.s.syncApp(ctx, j.p, j.a); err != nil {
		log.Printf("sync after cert %s: %v", j.d.Hostname, err)
	}
	log.Printf("cert issued for %s (expires %s)", j.d.Hostname, notAfter.Format(time.RFC3339))
	m.s.notify(notifyEvent{Event: "cert_issued", Project: j.p.Name, App: j.a.Name, URL: j.d.Hostname})
}

// issuePanel issues (or renews) the builtin cert for luncur's own panel
// custom domain. Same ACME mechanics as issue, but: no wildcard case, status
// lives in the panel_cert_* settings instead of a domains row, the TLS
// Secret is panelTLSSecret in the system namespace, and success re-applies
// the panel Ingress (via applyPanelIngress) instead of syncing an app.
func (m *certManager) issuePanel(ctx context.Context, host string) {
	st := m.s.st
	fail := func(err error) {
		log.Printf("panel cert %s: %v", host, err)
		if e := st.SetSetting("panel_cert_status", "failed"); e != nil {
			log.Printf("mark panel cert failed: %v", e)
		}
		if e := st.SetSetting("panel_cert_error", err.Error()); e != nil {
			log.Printf("set panel cert error: %v", e)
		}
		m.s.notify(notifyEvent{Event: "cert_failed", Project: "system", App: "panel", URL: host, Err: err.Error()})
	}
	if err := st.SetSetting("panel_cert_status", "pending"); err != nil {
		fail(err)
		return
	}

	key, err := m.accountKey(ctx)
	if err != nil {
		fail(fmt.Errorf("acme account key: %w", err))
		return
	}

	// DNS-01 when a dns provider is configured for this install (no
	// wildcard case here — the panel domain is never a wildcard); HTTP-01
	// with the shared challenge Ingress otherwise.
	var solver acme.Solver
	if m.s.dnsProviderName() != "none" {
		prov, err := m.s.dnsProvider()
		if err != nil {
			fail(fmt.Errorf("dns provider: %w", err))
			return
		}
		solver = m.newDNS01Solver(prov)
	} else {
		if err := m.setChallengeHost(ctx, host, true); err != nil {
			fail(fmt.Errorf("challenge ingress: %w", err))
			return
		}
		defer func() {
			if err := m.setChallengeHost(ctx, host, false); err != nil {
				log.Printf("remove challenge host %s: %v", host, err)
			}
		}()
	}

	email, _ := st.GetSetting("acme_email")
	iss := &acme.Issuer{
		DirectoryURL: m.directoryURL, AccountKey: key,
		Email: email, Challenges: m.challenges, Solver: solver,
	}
	ictx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	certPEM, keyPEM, notAfter, err := iss.Issue(ictx, host)
	if err != nil {
		fail(err)
		return
	}

	secJSON, err := json.Marshal(map[string]any{
		"apiVersion": "v1", "kind": "Secret",
		"metadata": map[string]any{
			"name": panelTLSSecret, "namespace": m.s.systemNamespace,
			"labels": map[string]string{"app.kubernetes.io/managed-by": "luncur"},
		},
		"type": "kubernetes.io/tls",
		"stringData": map[string]string{
			"tls.crt": string(certPEM), "tls.key": string(keyPEM),
		},
	})
	if err != nil {
		fail(err)
		return
	}
	if err := m.s.kube.Apply(ctx, m.s.systemNamespace, []render.Object{{Kind: "Secret", JSON: secJSON}}); err != nil {
		fail(fmt.Errorf("apply tls secret: %w", err))
		return
	}
	if err := st.SetSetting("panel_cert_status", "issued"); err != nil {
		fail(err)
		return
	}
	if err := st.SetSetting("panel_cert_error", ""); err != nil {
		log.Printf("clear panel cert error: %v", err)
	}
	if err := st.SetSetting("panel_cert_expires_at", notAfter.UTC().Format(time.RFC3339)); err != nil {
		log.Printf("set panel cert expiry: %v", err)
	}
	if err := m.s.applyPanelIngress(ctx); err != nil {
		log.Printf("apply panel ingress after cert %s: %v", host, err)
	}
	log.Printf("panel cert issued for %s (expires %s)", host, notAfter.Format(time.RFC3339))
	m.s.notify(notifyEvent{Event: "cert_issued", Project: "system", App: "panel", URL: host})
}

// readbackExpiry fills cert_expires_at for cert-manager-managed domains by
// parsing the leaf cert out of the TLS Secret cert-manager maintains.
func (m *certManager) readbackExpiry(ctx context.Context, d store.Domain) {
	if m.s.kube == nil {
		return
	}
	p, a, err := m.s.projectAppForDomain(d)
	if err != nil {
		return
	}
	data, err := m.s.kube.GetSecretData(ctx, p.Namespace, certSecretName(a.Name, d.Hostname))
	if err != nil || data == nil {
		return // not issued yet
	}
	blk, _ := pem.Decode(data["tls.crt"])
	if blk == nil {
		return
	}
	cert, err := x509.ParseCertificate(blk.Bytes)
	if err != nil {
		return
	}
	exp := cert.NotAfter.UTC().Format(time.RFC3339)
	if exp != d.CertExpiresAt {
		if err := m.s.st.SetDomainCert(d.ID, "external", "", exp); err != nil {
			log.Printf("cert expiry readback %s: %v", d.Hostname, err)
		}
	}
}

// accountKey loads the ACME account key from the luncur-acme-account
// Secret in the system namespace, generating and persisting one if absent.
func (m *certManager) accountKey(ctx context.Context) (*ecdsa.PrivateKey, error) {
	data, err := m.s.kube.GetSecretData(ctx, m.s.systemNamespace, acmeAccountSecret)
	if err != nil {
		return nil, err
	}
	if pemBytes, ok := data["key.pem"]; ok {
		return acme.DecodeAccountKey(pemBytes)
	}
	key, err := acme.GenerateAccountKey()
	if err != nil {
		return nil, err
	}
	pemBytes, err := acme.EncodeAccountKey(key)
	if err != nil {
		return nil, err
	}
	secJSON, err := json.Marshal(map[string]any{
		"apiVersion": "v1", "kind": "Secret",
		"metadata": map[string]any{
			"name": acmeAccountSecret, "namespace": m.s.systemNamespace,
			"labels": map[string]string{"app.kubernetes.io/managed-by": "luncur"},
		},
		"type":       "Opaque",
		"stringData": map[string]string{"key.pem": string(pemBytes)},
	})
	if err != nil {
		return nil, err
	}
	if err := m.s.kube.Apply(ctx, m.s.systemNamespace, []render.Object{{Kind: "Secret", JSON: secJSON}}); err != nil {
		return nil, err
	}
	return key, nil
}

// setChallengeHost adds/removes a host on the luncur-acme Ingress in
// luncur-system, which routes ONLY the ACME challenge path to luncur.
// Traefik merges same-host rules across namespaces; the longer challenge
// path wins over the app's "/" rule during validation.
func (m *certManager) setChallengeHost(ctx context.Context, host string, present bool) error {
	m.mu.Lock()
	if present {
		m.pendingHosts[host] = true
	} else {
		delete(m.pendingHosts, host)
	}
	hosts := make([]string, 0, len(m.pendingHosts))
	for h := range m.pendingHosts {
		hosts = append(hosts, h)
	}
	m.mu.Unlock()
	sort.Strings(hosts)

	rules := make([]map[string]any, 0, len(hosts))
	for _, h := range hosts {
		rules = append(rules, map[string]any{
			"host": h,
			"http": map[string]any{
				"paths": []map[string]any{{
					"path": acme.ChallengePath, "pathType": "Prefix",
					"backend": map[string]any{"service": map[string]any{
						"name": "luncur", "port": map[string]any{"number": int64(80)},
					}},
				}},
			},
		})
	}
	ing := map[string]any{
		"apiVersion": "networking.k8s.io/v1", "kind": "Ingress",
		"metadata": map[string]any{
			"name": challengeIngress, "namespace": m.s.systemNamespace,
			"labels": map[string]string{"app.kubernetes.io/managed-by": "luncur"},
		},
		"spec": map[string]any{"rules": rules},
	}
	b, err := json.Marshal(ing)
	if err != nil {
		return err
	}
	return m.s.kube.Apply(ctx, m.s.systemNamespace, []render.Object{{Kind: "Ingress", JSON: b}})
}

// projectAppForDomain resolves the app + project a domain row belongs to.
func (s *server) projectAppForDomain(d store.Domain) (store.Project, store.App, error) {
	a, err := s.st.GetAppByID(d.AppID)
	if err != nil {
		return store.Project{}, store.App{}, err
	}
	p, err := s.st.GetProjectByID(a.ProjectID)
	if err != nil {
		return store.Project{}, store.App{}, err
	}
	return p, a, nil
}
