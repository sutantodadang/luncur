package server

import (
	"errors"
	"net/http/httptest"
	"testing"

	"github.com/sutantodadang/luncur/internal/dns"
	"github.com/sutantodadang/luncur/internal/secret"
)

// dnsSettingsServer builds a *server with a sealer plus its HTTP frontend
// so tests can write sealed settings through the API, then call the
// factory directly.
func dnsSettingsServer(t *testing.T) (*server, *httptest.Server) {
	t.Helper()
	sealer, err := secret.New(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	st := newTestStore(t)
	s := newServer(Deps{Store: st, Sealer: sealer})
	srv := httptest.NewServer(s.handler())
	t.Cleanup(srv.Close)
	return s, srv
}

func TestDNSProviderFactory(t *testing.T) {
	s, srv := dnsSettingsServer(t)
	admin := seedUserToken(t, s.st, "root@b.co", "admin")

	// Unset -> errNoDNS.
	if _, err := s.dnsProviderFromSettings(); !errors.Is(err, errNoDNS) {
		t.Fatalf("unset: %v, want errNoDNS", err)
	}

	// none -> errNoDNS.
	doAuthed(t, "PUT", srv.URL+"/v1/settings/dns_provider", admin, `{"value":"none"}`).Body.Close()
	if _, err := s.dnsProviderFromSettings(); !errors.Is(err, errNoDNS) {
		t.Fatalf("none: %v, want errNoDNS", err)
	}

	// cloudflare without token -> hard error (not errNoDNS).
	doAuthed(t, "PUT", srv.URL+"/v1/settings/dns_provider", admin, `{"value":"cloudflare"}`).Body.Close()
	if _, err := s.dnsProviderFromSettings(); err == nil || errors.Is(err, errNoDNS) {
		t.Fatalf("missing token: %v", err)
	}

	// cloudflare with token -> *dns.Cloudflare carrying the unsealed token.
	doAuthed(t, "PUT", srv.URL+"/v1/settings/dns_cloudflare_token", admin, `{"value":"tok123"}`).Body.Close()
	p, err := s.dnsProviderFromSettings()
	if err != nil {
		t.Fatal(err)
	}
	cf, ok := p.(*dns.Cloudflare)
	if !ok || cf.Token != "tok123" {
		t.Fatalf("provider = %#v", p)
	}

	// route53.
	doAuthed(t, "PUT", srv.URL+"/v1/settings/dns_provider", admin, `{"value":"route53"}`).Body.Close()
	doAuthed(t, "PUT", srv.URL+"/v1/settings/dns_route53_access_key", admin, `{"value":"AK"}`).Body.Close()
	doAuthed(t, "PUT", srv.URL+"/v1/settings/dns_route53_secret_key", admin, `{"value":"SK"}`).Body.Close()
	doAuthed(t, "PUT", srv.URL+"/v1/settings/dns_route53_region", admin, `{"value":"eu-west-1"}`).Body.Close()
	p, err = s.dnsProviderFromSettings()
	if err != nil {
		t.Fatal(err)
	}
	r53, ok := p.(*dns.Route53)
	if !ok || r53.AccessKey != "AK" || r53.SecretKey != "SK" || r53.Region != "eu-west-1" {
		t.Fatalf("provider = %#v", p)
	}

	// rfc2136.
	doAuthed(t, "PUT", srv.URL+"/v1/settings/dns_provider", admin, `{"value":"rfc2136"}`).Body.Close()
	doAuthed(t, "PUT", srv.URL+"/v1/settings/dns_rfc2136_server", admin, `{"value":"ns1.example.com"}`).Body.Close()
	doAuthed(t, "PUT", srv.URL+"/v1/settings/dns_rfc2136_tsig_name", admin, `{"value":"luncur"}`).Body.Close()
	doAuthed(t, "PUT", srv.URL+"/v1/settings/dns_rfc2136_tsig_secret", admin, `{"value":"c2Vj"}`).Body.Close()
	p, err = s.dnsProviderFromSettings()
	if err != nil {
		t.Fatal(err)
	}
	rp, ok := p.(*dns.RFC2136)
	if !ok || rp.Server != "ns1.example.com" || rp.TSIGSecret != "c2Vj" {
		t.Fatalf("provider = %#v", p)
	}
}
