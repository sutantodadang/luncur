package gpucloud

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func genRSAKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	return key
}

func pkcs1PEM(key *rsa.PrivateKey) []byte {
	der := x509.MarshalPKCS1PrivateKey(key)
	return pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: der})
}

func pkcs8PEM(t *testing.T, key *rsa.PrivateKey) []byte {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshal pkcs8: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
}

func verifyNebiusJWT(t *testing.T, token string, key *rsa.PrivateKey, publicKeyID, serviceAccountID string, now time.Time) {
	t.Helper()
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("expected 3 dot-separated parts, got %d: %q", len(parts), token)
	}

	headerRaw, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		t.Fatalf("decode header: %v", err)
	}
	var header struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
		Typ string `json:"typ"`
	}
	if err := json.Unmarshal(headerRaw, &header); err != nil {
		t.Fatalf("unmarshal header: %v", err)
	}
	if header.Alg != "RS256" {
		t.Errorf("alg = %q, want RS256", header.Alg)
	}
	if header.Kid != publicKeyID {
		t.Errorf("kid = %q, want %q", header.Kid, publicKeyID)
	}
	if header.Typ != "JWT" {
		t.Errorf("typ = %q, want JWT", header.Typ)
	}

	claimsRaw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("decode claims: %v", err)
	}
	var claims struct {
		Iss string `json:"iss"`
		Sub string `json:"sub"`
		Iat int64  `json:"iat"`
		Exp int64  `json:"exp"`
	}
	if err := json.Unmarshal(claimsRaw, &claims); err != nil {
		t.Fatalf("unmarshal claims: %v", err)
	}
	if claims.Iss != serviceAccountID {
		t.Errorf("iss = %q, want %q", claims.Iss, serviceAccountID)
	}
	if claims.Sub != serviceAccountID {
		t.Errorf("sub = %q, want %q", claims.Sub, serviceAccountID)
	}
	if claims.Iat != now.Unix() {
		t.Errorf("iat = %d, want %d", claims.Iat, now.Unix())
	}
	if claims.Exp-claims.Iat != 3600 {
		t.Errorf("exp-iat = %d, want 3600", claims.Exp-claims.Iat)
	}

	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatalf("decode signature: %v", err)
	}
	signingInput := parts[0] + "." + parts[1]
	hash := sha256.Sum256([]byte(signingInput))
	if err := rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, hash[:], sig); err != nil {
		t.Errorf("signature verify failed: %v", err)
	}
}

func TestNebiusJWT_PKCS1(t *testing.T) {
	key := genRSAKey(t)
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	token, err := nebiusJWT(pkcs1PEM(key), "public-key-id", "service-account-id", now)
	if err != nil {
		t.Fatalf("nebiusJWT: %v", err)
	}
	verifyNebiusJWT(t, token, key, "public-key-id", "service-account-id", now)
}

func TestNebiusJWT_PKCS8(t *testing.T) {
	key := genRSAKey(t)
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	token, err := nebiusJWT(pkcs8PEM(t, key), "public-key-id-2", "service-account-id-2", now)
	if err != nil {
		t.Fatalf("nebiusJWT (pkcs8): %v", err)
	}
	verifyNebiusJWT(t, token, key, "public-key-id-2", "service-account-id-2", now)
}

func TestNebiusJWT_BadPEM(t *testing.T) {
	if _, err := nebiusJWT([]byte("not a pem"), "kid", "sa", time.Now()); err == nil {
		t.Fatal("expected error for invalid PEM, got nil")
	}
}

func TestNebiusTokenSource_CachesUntilExpiry(t *testing.T) {
	key := genRSAKey(t)
	var requests int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.URL.Path != "/iam/v1/tokens:exchange" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"tok-1","expires_in":3600}`))
	}))
	defer srv.Close()

	clock := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	ts := &nebiusTokenSource{
		cfg: NebiusConfig{
			ServiceAccountID: "sa-1",
			PublicKeyID:      "kid-1",
			PrivateKeyPEM:    pkcs1PEM(key),
			Endpoint:         srv.URL,
		},
		http: srv.Client(),
		now:  func() time.Time { return clock },
	}

	tok, err := ts.Token(context.Background())
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if tok != "tok-1" {
		t.Errorf("token = %q, want tok-1", tok)
	}
	if requests != 1 {
		t.Fatalf("requests = %d, want 1", requests)
	}

	// Same clock: cached token, no new request.
	tok, err = ts.Token(context.Background())
	if err != nil {
		t.Fatalf("Token (cached): %v", err)
	}
	if tok != "tok-1" {
		t.Errorf("cached token = %q, want tok-1", tok)
	}
	if requests != 1 {
		t.Fatalf("requests after cached call = %d, want 1", requests)
	}

	// Advance clock past expiry (3600s minus 5m skew buffer).
	clock = clock.Add(time.Hour + time.Minute)
	tok, err = ts.Token(context.Background())
	if err != nil {
		t.Fatalf("Token (after expiry): %v", err)
	}
	if tok != "tok-1" {
		t.Errorf("token after refresh = %q, want tok-1", tok)
	}
	if requests != 2 {
		t.Fatalf("requests after expiry = %d, want 2", requests)
	}
}

func TestNebiusTokenSource_ErrorOnNon2xx(t *testing.T) {
	key := genRSAKey(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"message":"invalid subject token"}`))
	}))
	defer srv.Close()

	ts := &nebiusTokenSource{
		cfg: NebiusConfig{
			ServiceAccountID: "sa-1",
			PublicKeyID:      "kid-1",
			PrivateKeyPEM:    pkcs1PEM(key),
			Endpoint:         srv.URL,
		},
		http: srv.Client(),
	}

	_, err := ts.Token(context.Background())
	if err == nil {
		t.Fatal("expected error for non-2xx response")
	}
	if !strings.Contains(err.Error(), "invalid subject token") {
		t.Errorf("error %q does not contain provider body", err.Error())
	}
}

func TestNebiusConfig_Endpoint(t *testing.T) {
	c := NebiusConfig{}
	if got := c.endpoint(); got != "https://api.nebius.cloud" {
		t.Errorf("default endpoint = %q, want https://api.nebius.cloud", got)
	}
	c.Endpoint = "https://custom.example"
	if got := c.endpoint(); got != "https://custom.example" {
		t.Errorf("custom endpoint = %q, want https://custom.example", got)
	}
}
