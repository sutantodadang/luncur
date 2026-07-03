package acme

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestChallengesServeHTTP(t *testing.T) {
	c := NewChallenges()
	c.Put("tok1", "tok1.keyauth")
	srv := httptest.NewServer(c)
	defer srv.Close()

	resp, err := http.Get(srv.URL + ChallengePath + "tok1")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || string(b) != "tok1.keyauth" {
		t.Fatalf("got %d %q", resp.StatusCode, b)
	}
	if resp, _ := http.Get(srv.URL + ChallengePath + "nope"); resp.StatusCode != 404 {
		t.Fatalf("unknown token: %d, want 404", resp.StatusCode)
	}

	c.Delete("tok1")
	if resp, _ := http.Get(srv.URL + ChallengePath + "tok1"); resp.StatusCode != 404 {
		t.Fatalf("deleted token still served")
	}
}

func TestNeedsRenewal(t *testing.T) {
	now := time.Date(2026, 7, 3, 0, 0, 0, 0, time.UTC)
	if NeedsRenewal(now.Add(60*24*time.Hour), now) {
		t.Fatal("60 days out should not renew")
	}
	if !NeedsRenewal(now.Add(10*24*time.Hour), now) {
		t.Fatal("10 days out should renew")
	}
}

// --- fake ACME directory ---------------------------------------------------

// fakeACME implements just enough of RFC 8555 for x/crypto/acme's client:
// directory, nonce, account, order, authz, http-01 challenge (verified by
// really fetching the challenge URL), finalize (signs the CSR with a test
// CA), and cert download (PEM chain).
type fakeACME struct {
	t         *testing.T
	mux       *http.ServeMux
	srv       *httptest.Server
	caKey     *ecdsa.PrivateKey
	caCert    *x509.Certificate
	chalHost  string // host:port serving the challenge (our Challenges store)
	authzOK   bool
	certDER   []byte
	orderDone bool
}

func newFakeACME(t *testing.T, chalHost string) *fakeACME {
	f := &fakeACME{t: t, mux: http.NewServeMux(), chalHost: chalHost}
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
		json.NewEncoder(w).Encode(map[string]any{
			"status":     status,
			"identifier": map[string]string{"type": "dns", "value": "www.example.com"},
			"challenges": []map[string]string{{
				"type": "http-01", "url": u + "/chal/1", "token": "tok-e2e", "status": status,
			}},
		})
	})
	f.mux.HandleFunc("/chal/1", func(w http.ResponseWriter, r *http.Request) {
		// "Validate" by fetching the token from the challenge server.
		resp, err := http.Get("http://" + f.chalHost + ChallengePath + "tok-e2e")
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

func (f *fakeACME) writeOrder(w http.ResponseWriter) {
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
func (f *fakeACME) withNonce(next http.Handler) http.Handler {
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

func TestIssueEndToEnd(t *testing.T) {
	ch := NewChallenges()
	chalSrv := httptest.NewServer(ch)
	defer chalSrv.Close()

	fake := newFakeACME(t, strings.TrimPrefix(chalSrv.URL, "http://"))

	key, err := GenerateAccountKey()
	if err != nil {
		t.Fatal(err)
	}
	iss := &Issuer{
		DirectoryURL: fake.srv.URL + "/dir",
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
	enc, err := EncodeAccountKey(key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeAccountKey(enc); err != nil {
		t.Fatal(err)
	}
}

func contextWithTimeout(t *testing.T) (context.Context, context.CancelFunc) {
	t.Helper()
	return context.WithTimeout(context.Background(), 30*time.Second)
}
