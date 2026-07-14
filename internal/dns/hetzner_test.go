package dns

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// fakeHetzner implements just enough of the Hetzner DNS v1 API: zone lookup
// by name, TXT record create/list/delete.
func fakeHetzner(t *testing.T) (*httptest.Server, *[]string) {
	t.Helper()
	var log []string
	mux := http.NewServeMux()
	mux.HandleFunc("GET /zones", func(w http.ResponseWriter, r *http.Request) {
		name := r.URL.Query().Get("name")
		log = append(log, "zones?"+name)
		if r.Header.Get("Auth-API-Token") != "tok123" {
			http.Error(w, `{}`, http.StatusUnauthorized)
			return
		}
		if name == "example.com" {
			w.Write([]byte(`{"zones":[{"id":"z1","name":"example.com"}]}`))
			return
		}
		w.Write([]byte(`{"zones":[]}`))
	})
	mux.HandleFunc("POST /records", func(w http.ResponseWriter, r *http.Request) {
		var rec struct {
			ZoneID string `json:"zone_id"`
			Type   string `json:"type"`
			Name   string `json:"name"`
			Value  string `json:"value"`
			TTL    int    `json:"ttl"`
		}
		json.NewDecoder(r.Body).Decode(&rec)
		log = append(log, "create "+rec.Type+" "+rec.Name+" "+rec.Value+" ttl="+strconv.Itoa(rec.TTL))
		w.Write([]byte(`{"record":{"id":"r1"}}`))
	})
	mux.HandleFunc("GET /records", func(w http.ResponseWriter, r *http.Request) {
		log = append(log, "list zone_id="+r.URL.Query().Get("zone_id"))
		w.Write([]byte(`{"records":[{"id":"r1","type":"TXT","name":"_acme-challenge.www","value":"txtval"}]}`))
	})
	mux.HandleFunc("DELETE /records/r1", func(w http.ResponseWriter, r *http.Request) {
		log = append(log, "delete r1")
		w.Write([]byte(`{}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &log
}

func TestHetznerPresentCleanUp(t *testing.T) {
	srv, log := fakeHetzner(t)
	h := &Hetzner{Token: "tok123", BaseURL: srv.URL}

	if err := h.Present(context.Background(), "_acme-challenge.www.example.com", "txtval"); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(*log, "|")
	if !strings.Contains(joined, "zones?example.com") {
		t.Fatalf("zone walk missing: %s", joined)
	}
	// Hetzner stores the value raw (unquoted), unlike deSEC.
	if !strings.Contains(joined, "create TXT _acme-challenge.www txtval ttl=60") {
		t.Fatalf("create missing/wrong shape: %s", joined)
	}

	if err := h.CleanUp(context.Background(), "_acme-challenge.www.example.com", "txtval"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(*log, "|"), "delete r1") {
		t.Fatalf("delete missing: %v", *log)
	}
}

func TestHetznerNoZone(t *testing.T) {
	srv, _ := fakeHetzner(t)
	h := &Hetzner{Token: "tok123", BaseURL: srv.URL}
	if err := h.Present(context.Background(), "_acme-challenge.other.net", "v"); err == nil ||
		!strings.Contains(err.Error(), "no zone") {
		t.Fatalf("want no-zone error, got %v", err)
	}
}

// TestHetznerApexName covers the root-domain case (cert for the zone itself,
// so the record name is a single label with no further subdomain nesting)
// and the literal-apex mapping in hetznerName.
func TestHetznerApexName(t *testing.T) {
	if got := hetznerName("_acme-challenge.example.com", "example.com"); got != "_acme-challenge" {
		t.Fatalf("root-domain name = %q, want _acme-challenge", got)
	}
	if got := hetznerName("example.com", "example.com"); got != "@" {
		t.Fatalf("literal apex name = %q, want @", got)
	}
}
