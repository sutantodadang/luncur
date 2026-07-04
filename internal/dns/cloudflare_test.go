package dns

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeCF implements just enough of the Cloudflare v4 API: zone lookup by
// name, TXT record create/list/delete.
func fakeCF(t *testing.T) (*httptest.Server, *[]string) {
	t.Helper()
	var log []string
	mux := http.NewServeMux()
	mux.HandleFunc("GET /zones", func(w http.ResponseWriter, r *http.Request) {
		name := r.URL.Query().Get("name")
		log = append(log, "zones?"+name)
		if r.Header.Get("Authorization") != "Bearer tok123" {
			http.Error(w, `{"success":false}`, http.StatusForbidden)
			return
		}
		if name == "example.com" {
			w.Write([]byte(`{"success":true,"result":[{"id":"z1","name":"example.com"}]}`))
			return
		}
		w.Write([]byte(`{"success":true,"result":[]}`))
	})
	mux.HandleFunc("POST /zones/z1/dns_records", func(w http.ResponseWriter, r *http.Request) {
		var rec struct {
			Type, Name, Content string
		}
		json.NewDecoder(r.Body).Decode(&rec)
		log = append(log, "create "+rec.Type+" "+rec.Name+" "+rec.Content)
		w.Write([]byte(`{"success":true,"result":{"id":"r1"}}`))
	})
	mux.HandleFunc("GET /zones/z1/dns_records", func(w http.ResponseWriter, r *http.Request) {
		log = append(log, "list "+r.URL.Query().Get("name"))
		w.Write([]byte(`{"success":true,"result":[{"id":"r1","type":"TXT","name":"_acme-challenge.www.example.com","content":"txtval"}]}`))
	})
	mux.HandleFunc("DELETE /zones/z1/dns_records/r1", func(w http.ResponseWriter, r *http.Request) {
		log = append(log, "delete r1")
		w.Write([]byte(`{"success":true}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &log
}

func TestCloudflarePresentCleanUp(t *testing.T) {
	srv, log := fakeCF(t)
	cf := &Cloudflare{Token: "tok123", BaseURL: srv.URL}

	if err := cf.Present(context.Background(), "_acme-challenge.www.example.com", "txtval"); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(*log, "|")
	// Longest-suffix zone walk: tries the full name first, lands on example.com.
	if !strings.Contains(joined, "zones?example.com") {
		t.Fatalf("zone walk missing: %s", joined)
	}
	if !strings.Contains(joined, "create TXT _acme-challenge.www.example.com txtval") {
		t.Fatalf("create missing: %s", joined)
	}

	if err := cf.CleanUp(context.Background(), "_acme-challenge.www.example.com", "txtval"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(*log, "|"), "delete r1") {
		t.Fatalf("delete missing: %v", *log)
	}
}

func TestCloudflareNoZone(t *testing.T) {
	srv, _ := fakeCF(t)
	cf := &Cloudflare{Token: "tok123", BaseURL: srv.URL}
	if err := cf.Present(context.Background(), "_acme-challenge.other.net", "v"); err == nil ||
		!strings.Contains(err.Error(), "no zone") {
		t.Fatalf("want no-zone error, got %v", err)
	}
}
