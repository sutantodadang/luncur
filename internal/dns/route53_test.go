package dns

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func fakeR53(t *testing.T) (*httptest.Server, *[]string) {
	t.Helper()
	var log []string
	mux := http.NewServeMux()
	mux.HandleFunc("GET /2013-04-01/hostedzone", func(w http.ResponseWriter, r *http.Request) {
		name := r.URL.Query().Get("dnsname")
		log = append(log, "zones?"+name)
		if r.Header.Get("Authorization") == "" {
			http.Error(w, "unsigned", http.StatusForbidden)
			return
		}
		if name == "example.com" {
			w.Write([]byte(`<?xml version="1.0"?>
<ListHostedZonesByNameResponse><HostedZones><HostedZone>
  <Id>/hostedzone/Z123</Id><Name>example.com.</Name>
</HostedZone></HostedZones></ListHostedZonesByNameResponse>`))
			return
		}
		w.Write([]byte(`<?xml version="1.0"?><ListHostedZonesByNameResponse><HostedZones></HostedZones></ListHostedZonesByNameResponse>`))
	})
	mux.HandleFunc("POST /2013-04-01/hostedzone/Z123/rrset", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		log = append(log, "change "+string(body))
		w.Write([]byte(`<?xml version="1.0"?><ChangeResourceRecordSetsResponse/>`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &log
}

func TestRoute53PresentCleanUp(t *testing.T) {
	srv, log := fakeR53(t)
	r53 := &Route53{AccessKey: "AK", SecretKey: "SK", BaseURL: srv.URL}

	if err := r53.Present(context.Background(), "_acme-challenge.www.example.com", "txtval"); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(*log, "|")
	if !strings.Contains(joined, "zones?example.com") {
		t.Fatalf("zone walk missing: %s", joined)
	}
	if !strings.Contains(joined, "<Action>UPSERT</Action>") ||
		!strings.Contains(joined, "<Name>_acme-challenge.www.example.com.</Name>") ||
		!strings.Contains(joined, `<Value>&#34;txtval&#34;</Value>`) {
		t.Fatalf("upsert body wrong: %s", joined)
	}

	if err := r53.CleanUp(context.Background(), "_acme-challenge.www.example.com", "txtval"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(*log, "|"), "<Action>DELETE</Action>") {
		t.Fatalf("delete missing: %v", *log)
	}
}

func TestRoute53NoZone(t *testing.T) {
	srv, _ := fakeR53(t)
	r53 := &Route53{AccessKey: "AK", SecretKey: "SK", BaseURL: srv.URL}
	if err := r53.Present(context.Background(), "_acme-challenge.nope.net", "v"); err == nil ||
		!strings.Contains(err.Error(), "no hosted zone") {
		t.Fatalf("want no-zone error, got %v", err)
	}
}
