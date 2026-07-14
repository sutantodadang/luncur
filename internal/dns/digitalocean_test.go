package dns

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeDO implements just enough of the DigitalOcean v2 API: domain (zone)
// existence check, TXT record create/list/delete.
func fakeDO(t *testing.T) (*httptest.Server, *[]string) {
	t.Helper()
	var log []string
	mux := http.NewServeMux()
	mux.HandleFunc("GET /domains/example.com", func(w http.ResponseWriter, r *http.Request) {
		log = append(log, "domain example.com")
		if r.Header.Get("Authorization") != "Bearer tok123" {
			http.Error(w, `{}`, http.StatusUnauthorized)
			return
		}
		w.Write([]byte(`{"domain":{"name":"example.com"}}`))
	})
	mux.HandleFunc("GET /domains/www.example.com", func(w http.ResponseWriter, r *http.Request) {
		log = append(log, "domain www.example.com")
		http.Error(w, `{"id":"not_found"}`, http.StatusNotFound)
	})
	mux.HandleFunc("POST /domains/example.com/records", func(w http.ResponseWriter, r *http.Request) {
		var rec struct {
			Type string `json:"type"`
			Name string `json:"name"`
			Data string `json:"data"`
			TTL  int    `json:"ttl"`
		}
		json.NewDecoder(r.Body).Decode(&rec)
		log = append(log, "create "+rec.Type+" "+rec.Name+" "+rec.Data)
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"domain_record":{"id":1}}`))
	})
	mux.HandleFunc("GET /domains/example.com/records", func(w http.ResponseWriter, r *http.Request) {
		log = append(log, "list name="+r.URL.Query().Get("name"))
		w.Write([]byte(`{"domain_records":[{"id":1,"type":"TXT","data":"txtval"}]}`))
	})
	mux.HandleFunc("DELETE /domains/example.com/records/1", func(w http.ResponseWriter, r *http.Request) {
		log = append(log, "delete 1")
		w.WriteHeader(http.StatusNoContent)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &log
}

func TestDigitalOceanPresentCleanUp(t *testing.T) {
	srv, log := fakeDO(t)
	do := &DigitalOcean{Token: "tok123", BaseURL: srv.URL}

	if err := do.Present(context.Background(), "_acme-challenge.www.example.com", "txtval"); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(*log, "|")
	// Longest-suffix zone walk: tries www.example.com (404) then example.com.
	if !strings.Contains(joined, "domain www.example.com") || !strings.Contains(joined, "domain example.com") {
		t.Fatalf("zone walk missing: %s", joined)
	}
	// The create body's name is zone-relative.
	if !strings.Contains(joined, "create TXT _acme-challenge.www txtval") {
		t.Fatalf("create missing/wrong shape: %s", joined)
	}

	if err := do.CleanUp(context.Background(), "_acme-challenge.www.example.com", "txtval"); err != nil {
		t.Fatal(err)
	}
	joined = strings.Join(*log, "|")
	// The list filter's name is the fully-qualified name, not the relative one.
	if !strings.Contains(joined, "list name=_acme-challenge.www.example.com") {
		t.Fatalf("list filter missing/wrong shape: %s", joined)
	}
	if !strings.Contains(joined, "delete 1") {
		t.Fatalf("delete missing: %s", joined)
	}
}

func TestDigitalOceanNoZone(t *testing.T) {
	srv, _ := fakeDO(t)
	do := &DigitalOcean{Token: "tok123", BaseURL: srv.URL}
	if err := do.Present(context.Background(), "_acme-challenge.other.net", "v"); err == nil ||
		!strings.Contains(err.Error(), "no zone") {
		t.Fatalf("want no-zone error, got %v", err)
	}
}

// TestDigitalOceanApexName covers the root-domain case (cert for the zone
// itself) and the literal-apex mapping in digitaloceanName.
func TestDigitalOceanApexName(t *testing.T) {
	if got := digitaloceanName("_acme-challenge.example.com", "example.com"); got != "_acme-challenge" {
		t.Fatalf("root-domain name = %q, want _acme-challenge", got)
	}
	if got := digitaloceanName("example.com", "example.com"); got != "@" {
		t.Fatalf("literal apex name = %q, want @", got)
	}
}
