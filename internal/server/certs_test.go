package server

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"

	"github.com/sutantodadang/luncur/internal/acme/acmetest"
	"github.com/sutantodadang/luncur/internal/dns"
	"github.com/sutantodadang/luncur/internal/kube"
	"github.com/sutantodadang/luncur/internal/secret"
	"github.com/sutantodadang/luncur/internal/store"
)

// recordedPatch captures one server-side-apply patch the cert manager sent
// through the fake dynamic client, so tests can inspect the object it
// applied (not just which verb/resource was hit).
type recordedPatch struct {
	resource  string
	namespace string
	name      string
	raw       []byte
}

// patchRecorder collects recordedPatches behind a mutex: the reactor runs on
// the cert manager's goroutine while the test goroutine reads assertions.
type patchRecorder struct {
	mu      sync.Mutex
	patches []recordedPatch
}

func (r *patchRecorder) add(p recordedPatch) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.patches = append(r.patches, p)
}

func (r *patchRecorder) snapshot() []recordedPatch {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]recordedPatch(nil), r.patches...)
}

// certTestServer builds a *server wired with a fake dynamic client (patches
// recorded) and a fake typed clientset (so GetSecretData/accountKey work),
// following the same fixture shape as apps_test.go's kubeServer and
// build_test.go's buildServer.
func certTestServer(t *testing.T) (*server, *store.Store, *patchRecorder, *k8sfake.Clientset) {
	t.Helper()
	st := newTestStore(t)
	scheme := runtime.NewScheme()
	dyn := dynamicfake.NewSimpleDynamicClient(scheme)
	cs := k8sfake.NewSimpleClientset()
	rec := &patchRecorder{}
	dyn.PrependReactor("*", "*", func(a ktesting.Action) (bool, runtime.Object, error) {
		if pa, ok := a.(ktesting.PatchAction); ok {
			rec.add(recordedPatch{
				resource:  a.GetResource().Resource,
				namespace: a.GetNamespace(),
				name:      pa.GetName(),
				raw:       pa.GetPatch(),
			})
		}
		return true, nil, nil
	})
	sealer, err := secret.New(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	s := newServer(Deps{
		Store: st, Sealer: sealer, Kube: kube.NewForTest(dyn, cs), ExternalIP: "1.2.3.4",
	})
	return s, st, rec, cs
}

func seedDomain(t *testing.T, st *store.Store, hostname string) (store.Project, store.Environment, store.App, store.Domain) {
	t.Helper()
	p, err := st.CreateProject("proj")
	if err != nil {
		t.Fatal(err)
	}
	p, env := seedDefaultEnv(t, st, p)
	a, err := st.CreateApp(p.ID, "web", 8080, "web", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetAppEnvironmentID(a.ID, env.ID); err != nil {
		t.Fatal(err)
	}
	d, err := st.AddDomain(a.ID, hostname)
	if err != nil {
		t.Fatal(err)
	}
	return p, env, a, d
}

func hasPatch(patches []recordedPatch, resource, namespace, name string, contains ...string) bool {
	for _, p := range patches {
		if p.resource != resource || p.namespace != namespace || p.name != name {
			continue
		}
		ok := true
		for _, c := range contains {
			if !strings.Contains(string(p.raw), c) {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

// pollDomain polls until the app's single domain reaches one of the given
// terminal statuses, or fails the test after deadline.
func pollDomain(t *testing.T, st *store.Store, appID int64, deadline time.Duration, terminal ...string) store.Domain {
	t.Helper()
	end := time.Now().Add(deadline)
	var got store.Domain
	for {
		list, err := st.ListDomains(appID)
		if err != nil {
			t.Fatal(err)
		}
		if len(list) == 1 {
			got = list[0]
			for _, want := range terminal {
				if got.CertStatus == want {
					return got
				}
			}
		}
		if time.Now().After(end) {
			t.Fatalf("domain did not reach a terminal status in time, stuck at %+v", got)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestCertManagerIssuesAndRenews(t *testing.T) {
	srv, st, patches, _ := certTestServer(t)
	p, env, a, d := seedDomain(t, st, "www.example.com")

	// Mount srv's own mux (it routes the challenge path through
	// srv.certs.Challenges()) behind an httptest server, and point the fake
	// ACME directory's challenge validation at it.
	mux := httptest.NewServer(srv.handler())
	t.Cleanup(mux.Close)
	fakeDir := acmetest.New(t, strings.TrimPrefix(mux.URL, "http://"))
	srv.certs.directoryURL = fakeDir.DirectoryURL()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	go srv.certs.Run(ctx)

	srv.certs.Kick(p, env, a, d)

	got := pollDomain(t, st, a.ID, 15*time.Second, "issued", "failed")
	if got.CertStatus != "issued" {
		t.Fatalf("cert_status = %q (error %q), want issued", got.CertStatus, got.CertError)
	}
	if got.CertExpiresAt == "" {
		t.Fatal("cert_expires_at not set")
	}

	secretName := certSecretName(a.Name, d.Hostname)
	applied := patches.snapshot()
	if !hasPatch(applied, "secrets", p.Namespace, secretName,
		`"type":"kubernetes.io/tls"`, `"namespace":"`+p.Namespace+`"`) {
		t.Fatalf("no applied TLS secret %s/%s of type kubernetes.io/tls found in patches: %+v", p.Namespace, secretName, applied)
	}

	if !hasPatch(applied, "ingresses", "luncur-system", challengeIngress, `"host":"`+d.Hostname+`"`) {
		t.Fatalf("no applied challenge Ingress %s with host %s found in patches: %+v", challengeIngress, d.Hostname, applied)
	}
}

func TestCertManagerFailureMarksDomain(t *testing.T) {
	srv, st, _, _ := certTestServer(t)
	p, env, a, d := seedDomain(t, st, "www.example.com")

	// Point the fake ACME's challenge validation at an unreachable host so
	// HTTP-01 validation never succeeds — the authorization stays pending
	// forever and issuance fails once its context deadline elapses.
	fakeDir := acmetest.New(t, "127.0.0.1:1")
	srv.certs.directoryURL = fakeDir.DirectoryURL()

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	go srv.certs.Run(ctx)

	srv.certs.Kick(p, env, a, d)

	got := pollDomain(t, st, a.ID, 15*time.Second, "issued", "failed")
	if got.CertStatus != "failed" {
		t.Fatalf("cert_status = %q, want failed", got.CertStatus)
	}
	if got.CertError == "" {
		t.Fatal("cert_error not set on failure")
	}
}

// selfSignedCertPEM generates a minimal self-signed leaf certificate PEM for
// hostname, expiring at notAfter — standalone rather than reusing
// acmetest's fake CA since this test never talks to an ACME server.
func selfSignedCertPEM(t *testing.T, hostname string, notAfter time.Time) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: hostname},
		DNSNames:     []string{hostname},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// TestCertSweepReadsBackCertManagerExpiry covers the cert-manager provider
// path: sweep doesn't issue anything itself, it just reads the leaf cert
// cert-manager already put in the TLS Secret and records its expiry.
func TestCertSweepReadsBackCertManagerExpiry(t *testing.T) {
	srv, st, _, cs := certTestServer(t)
	p, _, a, d := seedDomain(t, st, "www.example.com")
	if err := st.SetSetting("cert_provider", "cert-manager"); err != nil {
		t.Fatal(err)
	}
	if err := st.SetDomainCert(d.ID, "external", "", ""); err != nil {
		t.Fatal(err)
	}

	notAfter := time.Now().Add(90 * 24 * time.Hour)
	certPEM := selfSignedCertPEM(t, d.Hostname, notAfter)
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: certSecretName(a.Name, d.Hostname), Namespace: p.Namespace},
		Type:       corev1.SecretTypeTLS,
		Data:       map[string][]byte{"tls.crt": certPEM, "tls.key": []byte("fake-key")},
	}
	if _, err := cs.CoreV1().Secrets(p.Namespace).Create(context.Background(), sec, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}

	srv.certs.sweep(context.Background())

	list, err := st.ListDomains(a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("want 1 domain, got %d", len(list))
	}
	got := list[0]
	if got.CertStatus != "external" {
		t.Fatalf("cert_status = %q, want external", got.CertStatus)
	}
	gotExp, err := time.Parse(time.RFC3339, got.CertExpiresAt)
	if err != nil {
		t.Fatalf("cert_expires_at %q did not parse as RFC3339: %v", got.CertExpiresAt, err)
	}
	if want := notAfter.UTC().Truncate(time.Second); !gotExp.Equal(want) {
		t.Fatalf("cert_expires_at = %v, want %v", gotExp, want)
	}
}

// recordingServerProvider mirrors internal/acme's test recorder for use in
// package server tests.
type recordingServerProvider struct {
	mu       sync.Mutex
	presents map[string]string
}

func (p *recordingServerProvider) Present(ctx context.Context, fqdn, value string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.presents == nil {
		p.presents = map[string]string{}
	}
	p.presents[fqdn] = value
	return nil
}

func (p *recordingServerProvider) CleanUp(ctx context.Context, fqdn, value string) error {
	return nil
}

func (p *recordingServerProvider) txt(fqdn string) []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if v, ok := p.presents[fqdn]; ok {
		return []string{v}
	}
	return nil
}

// TestIssueDNS01PickedForWildcard: with a dns provider configured, the
// cert manager issues a wildcard via dns-01 — no challenge-Ingress writes,
// TXT presented for the base domain, TLS secret stored.
func TestIssueDNS01PickedForWildcard(t *testing.T) {
	srv, st, patches, _ := certTestServer(t)
	if err := st.SetSetting("dns_provider", "cloudflare"); err != nil {
		t.Fatal(err)
	}
	p, env, a, d := seedDomain(t, st, "*.example.com")

	prov := &recordingServerProvider{}
	srv.dnsProvider = func() (dns.Provider, error) { return prov, nil }
	srv.certs.lookupTXT = func(ctx context.Context, fqdn string) ([]string, error) {
		return prov.txt(fqdn), nil // instant propagation
	}

	// chalHost 127.0.0.1:1 — any HTTP-01 fetch would fail; dns-01 mode
	// validates via the recorded TXT instead.
	fakeDir := acmetest.New(t, "127.0.0.1:1")
	fakeDir.SetTXTLookup(prov.txt)
	srv.certs.directoryURL = fakeDir.DirectoryURL()

	srv.certs.issue(context.Background(), certJob{p: p, env: env, a: a, d: d})

	list, err := st.ListDomains(a.ID)
	if err != nil || len(list) != 1 {
		t.Fatalf("list = %+v err=%v", list, err)
	}
	got := list[0]
	if got.CertStatus != "issued" {
		t.Fatalf("cert_status = %q (error %q), want issued", got.CertStatus, got.CertError)
	}

	if v := prov.txt("_acme-challenge.example.com"); len(v) == 0 {
		t.Fatalf("no TXT presented for the base domain; presents = %v", prov.presents)
	}

	recorded := patches.snapshot()
	for _, pt := range recorded {
		if pt.resource == "ingresses" && pt.name == challengeIngress {
			t.Fatalf("dns-01 issuance must not touch the challenge Ingress: %+v", pt)
		}
	}
	if !hasPatch(recorded, "secrets", p.Namespace, certSecretName(a.Name, d.Hostname),
		`"type":"kubernetes.io/tls"`) {
		t.Fatalf("no applied TLS secret found in patches: %+v", recorded)
	}
}

// notifyCaptureServer starts an httptest server that captures each POSTed
// body onto a channel.
func notifyCaptureServer(t *testing.T) (*httptest.Server, chan []byte) {
	t.Helper()
	ch := make(chan []byte, 4)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		ch <- body
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(ts.Close)
	return ts, ch
}

// TestCertIssueNotifiesOnSuccess extends TestCertManagerIssuesAndRenews with
// a notify_url receiver: a successful issuance must deliver a cert_issued
// notification carrying the domain's hostname.
func TestCertIssueNotifiesOnSuccess(t *testing.T) {
	srv, st, _, _ := certTestServer(t)
	p, env, a, d := seedDomain(t, st, "www.example.com")

	mux := httptest.NewServer(srv.handler())
	t.Cleanup(mux.Close)
	fakeDir := acmetest.New(t, strings.TrimPrefix(mux.URL, "http://"))
	srv.certs.directoryURL = fakeDir.DirectoryURL()

	ts, ch := notifyCaptureServer(t)
	sealNotifyURL(t, srv, ts.URL)
	if err := st.SetSetting("notify_events", "cert_issued"); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	srv.certs.issue(ctx, certJob{p: p, env: env, a: a, d: d})

	got, err := st.ListDomains(a.ID)
	if err != nil || len(got) != 1 || got[0].CertStatus != "issued" {
		t.Fatalf("domain not issued: %+v err=%v", got, err)
	}

	select {
	case body := <-ch:
		var out struct {
			Event string `json:"event"`
			URL   string `json:"url"`
		}
		if err := json.Unmarshal(body, &out); err != nil {
			t.Fatal(err)
		}
		if out.Event != "cert_issued" || out.URL != d.Hostname {
			t.Fatalf("got %+v", out)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for cert_issued notification")
	}
}

// TestCertIssueNotifiesOnFailure extends TestCertManagerFailureMarksDomain
// with a notify_url receiver: a failed issuance must deliver a cert_failed
// notification carrying the hostname and a non-empty error.
func TestCertIssueNotifiesOnFailure(t *testing.T) {
	srv, st, _, _ := certTestServer(t)
	p, env, a, d := seedDomain(t, st, "www.example.com")

	fakeDir := acmetest.New(t, "127.0.0.1:1")
	srv.certs.directoryURL = fakeDir.DirectoryURL()

	ts, ch := notifyCaptureServer(t)
	sealNotifyURL(t, srv, ts.URL)
	if err := st.SetSetting("notify_events", "cert_failed"); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	srv.certs.issue(ctx, certJob{p: p, env: env, a: a, d: d})

	got, err := st.ListDomains(a.ID)
	if err != nil || len(got) != 1 || got[0].CertStatus != "failed" {
		t.Fatalf("domain not failed: %+v err=%v", got, err)
	}

	select {
	case body := <-ch:
		var out struct {
			Event string `json:"event"`
			Error string `json:"error"`
		}
		if err := json.Unmarshal(body, &out); err != nil {
			t.Fatal(err)
		}
		if out.Event != "cert_failed" || out.Error == "" {
			t.Fatalf("got %+v", out)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for cert_failed notification")
	}
}
