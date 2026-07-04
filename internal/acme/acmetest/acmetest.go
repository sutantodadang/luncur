// Package acmetest provides a fake ACME (RFC 8555) directory server for
// tests that exercise internal/acme's Issuer without a real Let's Encrypt
// account. It is used by internal/acme's own tests and by internal/server's
// cert manager tests, which is why it lives in its own importable
// (non-test) package rather than a _test.go file.
package acmetest

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sutantodadang/luncur/internal/acme"
)

// Server implements just enough of RFC 8555 for x/crypto/acme's client:
// directory, nonce, account, order, authz, http-01 challenge (verified by
// really fetching the challenge URL), finalize (signs the CSR with a test
// CA), and cert download (PEM chain).
type Server struct {
	t         *testing.T
	mux       *http.ServeMux
	srv       *httptest.Server
	caKey     *ecdsa.PrivateKey
	caCert    *x509.Certificate
	chalHost  string // host:port serving the challenge (our Challenges store)
	authzOK   bool
	certDER   []byte
	orderDone bool

	txtLookup func(fqdn string) []string // non-nil => offer dns-01
	domain    string                     // identifier from the order request
}

// SetTXTLookup switches the fake to dns-01: authorizations offer a dns-01
// challenge, validated by checking lookup("_acme-challenge.<domain>")
// returns at least one TXT value.
func (f *Server) SetTXTLookup(lookup func(fqdn string) []string) { f.txtLookup = lookup }

// New starts a fake ACME directory server. chalHost is the host:port
// serving the HTTP-01 challenge response (an acme.Challenges-backed
// server) — the fake validates the challenge by actually fetching it.
func New(t *testing.T, chalHost string) *Server {
	f := &Server{t: t, mux: http.NewServeMux(), chalHost: chalHost, domain: "www.example.com"}
	f.srv = httptest.NewServer(f.withNonce(f.mux))
	t.Cleanup(f.srv.Close)

	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	f.caKey = caKey
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "fake ACME CA"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		IsCA:         true, KeyUsage: x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	f.caCert, _ = x509.ParseCertificate(der)

	u := f.srv.URL
	f.mux.HandleFunc("/dir", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{
			"newNonce": u + "/nonce", "newAccount": u + "/acct", "newOrder": u + "/order",
		})
	})
	f.mux.HandleFunc("/nonce", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	f.mux.HandleFunc("/acct", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", u+"/acct/1")
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{"status":"valid"}`)
	})
	f.mux.HandleFunc("/order", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Identifiers []struct {
				Value string `json:"value"`
			} `json:"identifiers"`
		}
		if b := jwsPayload(t, r); b != nil {
			_ = json.Unmarshal(b, &req)
		}
		if len(req.Identifiers) > 0 && req.Identifiers[0].Value != "" {
			f.domain = req.Identifiers[0].Value
		}
		w.Header().Set("Location", u+"/order/1")
		w.WriteHeader(http.StatusCreated)
		f.writeOrder(w)
	})
	f.mux.HandleFunc("/order/1", func(w http.ResponseWriter, r *http.Request) {
		f.writeOrder(w)
	})
	f.mux.HandleFunc("/authz/1", func(w http.ResponseWriter, r *http.Request) {
		status := "pending"
		if f.authzOK {
			status = "valid"
		}
		chalType := "http-01"
		if f.txtLookup != nil {
			chalType = "dns-01"
		}
		json.NewEncoder(w).Encode(map[string]any{
			"status":     status,
			"identifier": map[string]string{"type": "dns", "value": strings.TrimPrefix(f.domain, "*.")},
			"challenges": []map[string]string{{
				"type": chalType, "url": u + "/chal/1", "token": "tok-e2e", "status": status,
			}},
		})
	})
	f.mux.HandleFunc("/chal/1", func(w http.ResponseWriter, r *http.Request) {
		if f.txtLookup != nil {
			// DNS-01 mode: "validate" by checking a TXT record was
			// presented for the challenge name.
			fqdn := "_acme-challenge." + strings.TrimPrefix(f.domain, "*.")
			if len(f.txtLookup(fqdn)) == 0 {
				http.Error(w, `{"status":"invalid"}`, http.StatusOK)
				return
			}
			f.authzOK = true
			fmt.Fprint(w, `{"status":"valid"}`)
			return
		}
		// HTTP-01 mode: "validate" by fetching the token from the
		// challenge server.
		resp, err := http.Get("http://" + f.chalHost + acme.ChallengePath + "tok-e2e")
		if err != nil || resp.StatusCode != 200 {
			http.Error(w, `{"status":"invalid"}`, http.StatusOK)
			return
		}
		f.authzOK = true
		fmt.Fprint(w, `{"status":"valid"}`)
	})
	f.mux.HandleFunc("/finalize/1", func(w http.ResponseWriter, r *http.Request) {
		body := jwsPayload(t, r)
		var req struct {
			CSR string `json:"csr"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			t.Fatalf("finalize payload: %v", err)
		}
		der, _ := base64.RawURLEncoding.DecodeString(req.CSR)
		csr, err := x509.ParseCertificateRequest(der)
		if err != nil {
			t.Fatalf("parse csr: %v", err)
		}
		leaf := &x509.Certificate{
			SerialNumber: big.NewInt(2),
			Subject:      pkix.Name{CommonName: csr.Subject.CommonName},
			DNSNames:     csr.DNSNames,
			NotBefore:    time.Now().Add(-time.Hour),
			NotAfter:     time.Now().Add(90 * 24 * time.Hour),
			KeyUsage:     x509.KeyUsageDigitalSignature,
			ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		}
		f.certDER, err = x509.CreateCertificate(rand.Reader, leaf, f.caCert, csr.PublicKey, f.caKey)
		if err != nil {
			t.Fatal(err)
		}
		f.orderDone = true
		f.writeOrder(w)
	})
	f.mux.HandleFunc("/cert/1", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/pem-certificate-chain")
		pem.Encode(w, &pem.Block{Type: "CERTIFICATE", Bytes: f.certDER})
	})
	return f
}

// DirectoryURL is the ACME directory URL to pass as Issuer.DirectoryURL.
func (f *Server) DirectoryURL() string { return f.srv.URL + "/dir" }

func (f *Server) writeOrder(w http.ResponseWriter) {
	u := f.srv.URL
	status := "pending"
	if f.authzOK {
		status = "ready"
	}
	if f.orderDone {
		status = "valid"
	}
	json.NewEncoder(w).Encode(map[string]any{
		"status":         status,
		"authorizations": []string{u + "/authz/1"},
		"finalize":       u + "/finalize/1",
		"certificate":    u + "/cert/1",
	})
}

// withNonce stamps a Replay-Nonce on every response (the client demands it).
func (f *Server) withNonce(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Replay-Nonce", "nonce-"+fmt.Sprint(time.Now().UnixNano()))
		w.Header().Set("Content-Type", "application/json")
		next.ServeHTTP(w, r)
	})
}

// jwsPayload extracts the base64url payload from a JWS body, skipping
// signature verification entirely (this is a test double).
func jwsPayload(t *testing.T, r *http.Request) []byte {
	t.Helper()
	var env struct {
		Payload string `json:"payload"`
	}
	if err := json.NewDecoder(r.Body).Decode(&env); err != nil {
		t.Fatalf("jws decode: %v", err)
	}
	if env.Payload == "" {
		return nil // POST-as-GET
	}
	b, err := base64.RawURLEncoding.DecodeString(env.Payload)
	if err != nil {
		t.Fatalf("jws payload b64: %v", err)
	}
	return b
}
