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

// fakeDeSEC implements just enough of the deSEC API: domain (zone) existence
// check, and a bulk rrsets endpoint that upserts one TXT rrset so Present's
// read-modify-write round trip can be observed.
func fakeDeSEC(t *testing.T) (*httptest.Server, *[]string) {
	t.Helper()
	var log []string
	var rrsetRecords []string // current TXT records for _acme-challenge.www
	mux := http.NewServeMux()
	mux.HandleFunc("GET /domains/example.com/", func(w http.ResponseWriter, r *http.Request) {
		log = append(log, "domain example.com")
		if r.Header.Get("Authorization") != "Token tok123" {
			http.Error(w, `{}`, http.StatusUnauthorized)
			return
		}
		w.Write([]byte(`{"name":"example.com"}`))
	})
	mux.HandleFunc("GET /domains/www.example.com/", func(w http.ResponseWriter, r *http.Request) {
		log = append(log, "domain www.example.com")
		http.Error(w, `{"detail":"Not found."}`, http.StatusNotFound)
	})
	mux.HandleFunc("GET /domains/example.com/rrsets/_acme-challenge.www/TXT/", func(w http.ResponseWriter, r *http.Request) {
		log = append(log, "get rrset _acme-challenge.www")
		if len(rrsetRecords) == 0 {
			http.Error(w, `{"detail":"Not found."}`, http.StatusNotFound)
			return
		}
		b, _ := json.Marshal(map[string]any{"subname": "_acme-challenge.www", "type": "TXT", "records": rrsetRecords, "ttl": 3600})
		w.Write(b)
	})
	mux.HandleFunc("PUT /domains/example.com/rrsets/", func(w http.ResponseWriter, r *http.Request) {
		var body []desecRRSet
		json.NewDecoder(r.Body).Decode(&body)
		if len(body) != 1 {
			http.Error(w, "bad bulk body", http.StatusBadRequest)
			return
		}
		rrsetRecords = body[0].Records
		log = append(log, "put rrsets subname="+body[0].SubName+" ttl="+strconv.Itoa(body[0].TTL)+" records="+strings.Join(body[0].Records, ","))
		w.Write([]byte(`[]`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &log
}

func TestDeSECPresentCleanUp(t *testing.T) {
	srv, log := fakeDeSEC(t)
	d := &DeSEC{Token: "tok123", BaseURL: srv.URL}

	if err := d.Present(context.Background(), "_acme-challenge.www.example.com", "txtval"); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(*log, "|")
	if !strings.Contains(joined, "domain www.example.com") || !strings.Contains(joined, "domain example.com") {
		t.Fatalf("zone walk missing: %s", joined)
	}
	if !strings.Contains(joined, `put rrsets subname=_acme-challenge.www ttl=3600 records="txtval"`) {
		t.Fatalf("create missing/wrong shape (value must be quoted, ttl floor 3600): %s", joined)
	}

	if err := d.CleanUp(context.Background(), "_acme-challenge.www.example.com", "txtval"); err != nil {
		t.Fatal(err)
	}
	logTail := (*log)[len(*log)-1]
	// After removing the only value, the PUT carries an empty records list
	// (deSEC deletes the rrset when records is empty).
	if logTail != "put rrsets subname=_acme-challenge.www ttl=3600 records=" {
		t.Fatalf("cleanup put = %q, want empty records list", logTail)
	}
}

func TestDeSECNoZone(t *testing.T) {
	srv, _ := fakeDeSEC(t)
	d := &DeSEC{Token: "tok123", BaseURL: srv.URL}
	if err := d.Present(context.Background(), "_acme-challenge.other.net", "v"); err == nil ||
		!strings.Contains(err.Error(), "no zone") {
		t.Fatalf("want no-zone error, got %v", err)
	}
}

// TestDeSECApexSubname covers the root-domain case (cert for the zone
// itself) and the literal-apex mapping in desecSubname: the body subname is
// "" but the URL path segment is "@" (deSEC paths can't have an empty
// segment).
func TestDeSECApexSubname(t *testing.T) {
	sub, urlSub := desecSubname("_acme-challenge.example.com", "example.com")
	if sub != "_acme-challenge" || urlSub != "_acme-challenge" {
		t.Fatalf("root-domain subname = (%q,%q), want (_acme-challenge,_acme-challenge)", sub, urlSub)
	}
	sub, urlSub = desecSubname("example.com", "example.com")
	if sub != "" || urlSub != "@" {
		t.Fatalf("literal apex subname = (%q,%q), want (\"\",@)", sub, urlSub)
	}
}
