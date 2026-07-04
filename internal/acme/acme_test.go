package acme_test

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sutantodadang/luncur/internal/acme"
	"github.com/sutantodadang/luncur/internal/acme/acmetest"
)

func TestChallengesServeHTTP(t *testing.T) {
	c := acme.NewChallenges()
	c.Put("tok1", "tok1.keyauth")
	srv := httptest.NewServer(c)
	defer srv.Close()

	resp, err := http.Get(srv.URL + acme.ChallengePath + "tok1")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || string(b) != "tok1.keyauth" {
		t.Fatalf("got %d %q", resp.StatusCode, b)
	}
	if resp, _ := http.Get(srv.URL + acme.ChallengePath + "nope"); resp.StatusCode != 404 {
		t.Fatalf("unknown token: %d, want 404", resp.StatusCode)
	}

	c.Delete("tok1")
	if resp, _ := http.Get(srv.URL + acme.ChallengePath + "tok1"); resp.StatusCode != 404 {
		t.Fatalf("deleted token still served")
	}
}

func TestNeedsRenewal(t *testing.T) {
	now := time.Date(2026, 7, 3, 0, 0, 0, 0, time.UTC)
	if acme.NeedsRenewal(now.Add(60*24*time.Hour), now) {
		t.Fatal("60 days out should not renew")
	}
	if !acme.NeedsRenewal(now.Add(10*24*time.Hour), now) {
		t.Fatal("10 days out should renew")
	}
}

func TestIssueEndToEnd(t *testing.T) {
	ch := acme.NewChallenges()
	chalSrv := httptest.NewServer(ch)
	defer chalSrv.Close()

	fake := acmetest.New(t, strings.TrimPrefix(chalSrv.URL, "http://"))

	key, err := acme.GenerateAccountKey()
	if err != nil {
		t.Fatal(err)
	}
	iss := &acme.Issuer{
		DirectoryURL: fake.DirectoryURL(),
		AccountKey:   key,
		Email:        "admin@example.com",
		Challenges:   ch,
	}
	ctx, cancel := contextWithTimeout(t)
	defer cancel()
	certPEM, keyPEM, notAfter, err := iss.Issue(ctx, "www.example.com")
	if err != nil {
		t.Fatal(err)
	}
	blk, _ := pem.Decode(certPEM)
	if blk == nil || blk.Type != "CERTIFICATE" {
		t.Fatal("no certificate PEM")
	}
	cert, err := x509.ParseCertificate(blk.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if len(cert.DNSNames) == 0 || cert.DNSNames[0] != "www.example.com" {
		t.Fatalf("dns names = %v", cert.DNSNames)
	}
	if kb, _ := pem.Decode(keyPEM); kb == nil || kb.Type != "EC PRIVATE KEY" {
		t.Fatal("no key PEM")
	}
	if notAfter.Before(time.Now().Add(24 * time.Hour)) {
		t.Fatalf("notAfter too soon: %v", notAfter)
	}

	// Account key round-trips through PEM (for the K8s Secret).
	enc, err := acme.EncodeAccountKey(key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := acme.DecodeAccountKey(enc); err != nil {
		t.Fatal(err)
	}
}

func contextWithTimeout(t *testing.T) (context.Context, context.CancelFunc) {
	t.Helper()
	return context.WithTimeout(context.Background(), 30*time.Second)
}

// recordingProvider is a dns.Provider that records Present/CleanUp calls.
type recordingProvider struct {
	mu       sync.Mutex
	presents map[string]string // fqdn -> value
	cleaned  []string
}

func (p *recordingProvider) Present(ctx context.Context, fqdn, value string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.presents == nil {
		p.presents = map[string]string{}
	}
	p.presents[fqdn] = value
	return nil
}

func (p *recordingProvider) CleanUp(ctx context.Context, fqdn, value string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cleaned = append(p.cleaned, fqdn)
	return nil
}

func (p *recordingProvider) txt(fqdn string) []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if v, ok := p.presents[fqdn]; ok {
		return []string{v}
	}
	return nil
}

// TestIssueDNS01Wildcard drives a full dns-01 order for *.example.com
// against the fake directory: the fake "validates" by looking up the TXT
// the solver presented via the recording provider.
func TestIssueDNS01Wildcard(t *testing.T) {
	prov := &recordingProvider{}
	fake := acmetest.New(t, "")
	fake.SetTXTLookup(prov.txt)

	key, err := acme.GenerateAccountKey()
	if err != nil {
		t.Fatal(err)
	}
	solver := &acme.DNS01Solver{
		Provider: prov,
		LookupTXT: func(ctx context.Context, fqdn string) ([]string, error) {
			return prov.txt(fqdn), nil // instant "propagation"
		},
	}
	iss := &acme.Issuer{
		DirectoryURL: fake.DirectoryURL(), AccountKey: key,
		Email: "a@b.co", Solver: solver,
	}
	ctx, cancel := contextWithTimeout(t)
	defer cancel()
	certPEM, keyPEM, notAfter, err := iss.Issue(ctx, "*.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if len(certPEM) == 0 || len(keyPEM) == 0 || notAfter.IsZero() {
		t.Fatal("empty issuance result")
	}
	// The challenge TXT went to the base domain, not *.example.com.
	if v := prov.txt("_acme-challenge.example.com"); len(v) == 0 {
		t.Fatalf("no TXT presented; presents = %v", prov.presents)
	}
	if len(prov.cleaned) == 0 {
		t.Fatal("TXT record never cleaned up")
	}
}

// TestDNS01PropagationTimeout: the solver gives up when the TXT never
// appears, and cleans up after itself.
func TestDNS01PropagationTimeout(t *testing.T) {
	prov := &recordingProvider{}
	solver := &acme.DNS01Solver{
		Provider: prov,
		LookupTXT: func(ctx context.Context, fqdn string) ([]string, error) {
			return nil, nil // never propagates
		},
		Timeout:  50 * time.Millisecond,
		Interval: 10 * time.Millisecond,
	}
	_, err := solver.Setup(context.Background(), "example.com", "tok", "tok.thumb")
	if err == nil || !strings.Contains(err.Error(), "propagat") {
		t.Fatalf("want propagation timeout, got %v", err)
	}
	if len(prov.cleaned) == 0 {
		t.Fatal("failed setup must clean up its record")
	}
}
