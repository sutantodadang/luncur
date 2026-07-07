package gpucloud

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

// DefaultNebiusEndpoint is Nebius AI Cloud's public API root.
const DefaultNebiusEndpoint = "https://api.nebius.cloud"

// NebiusConfig carries the service-account credentials and placement
// defaults needed to authenticate against Nebius and rent instances.
type NebiusConfig struct {
	ServiceAccountID string
	PublicKeyID      string
	PrivateKeyPEM    []byte
	ParentID         string // project id, parent of instances
	SubnetID         string
	Endpoint         string // "" = https://api.nebius.cloud (VERIFY exact host)
}

// endpoint returns c.Endpoint, or DefaultNebiusEndpoint when unset.
func (c NebiusConfig) endpoint() string {
	if c.Endpoint != "" {
		return c.Endpoint
	}
	return DefaultNebiusEndpoint
}

// nebiusJWT builds and signs a short-lived (1h) RS256 JWT identifying a
// Nebius service account, per Nebius IAM's token-exchange requirements.
// privateKeyPEM must decode to an RSA private key, either PKCS#1
// ("RSA PRIVATE KEY") or PKCS#8 ("PRIVATE KEY").
func nebiusJWT(privateKeyPEM []byte, publicKeyID, serviceAccountID string, now time.Time) (string, error) {
	key, err := parseRSAPrivateKeyPEM(privateKeyPEM)
	if err != nil {
		return "", err
	}

	header := struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
		Typ string `json:"typ"`
	}{Alg: "RS256", Kid: publicKeyID, Typ: "JWT"}
	headerJSON, err := json.Marshal(header)
	if err != nil {
		return "", fmt.Errorf("marshal jwt header: %w", err)
	}

	claims := struct {
		Iss string `json:"iss"`
		Sub string `json:"sub"`
		Iat int64  `json:"iat"`
		Exp int64  `json:"exp"`
	}{
		Iss: serviceAccountID,
		Sub: serviceAccountID,
		Iat: now.Unix(),
		Exp: now.Add(time.Hour).Unix(),
	}
	claimsJSON, err := json.Marshal(claims)
	if err != nil {
		return "", fmt.Errorf("marshal jwt claims: %w", err)
	}

	signingInput := base64.RawURLEncoding.EncodeToString(headerJSON) + "." +
		base64.RawURLEncoding.EncodeToString(claimsJSON)

	hash := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, hash[:])
	if err != nil {
		return "", fmt.Errorf("sign jwt: %w", err)
	}

	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

// parseRSAPrivateKeyPEM decodes a PEM block and parses it as an RSA private
// key, trying PKCS#1 first and falling back to PKCS#8.
func parseRSAPrivateKeyPEM(privateKeyPEM []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(privateKeyPEM)
	if block == nil {
		return nil, fmt.Errorf("nebius: no PEM block found in private key")
	}

	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}

	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("nebius: private key is neither PKCS#1 nor PKCS#8: %w", err)
	}
	key, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("nebius: private key is not RSA (got %T)", parsed)
	}
	return key, nil
}

// nebiusTokenSource exchanges a service-account JWT for a short-lived IAM
// access token, caching it until shortly before expiry.
type nebiusTokenSource struct {
	cfg  NebiusConfig
	http *http.Client

	mu     sync.Mutex
	token  string
	expiry time.Time

	now func() time.Time // injectable clock for tests; default time.Now
}

func (ts *nebiusTokenSource) client() *http.Client {
	if ts.http != nil {
		return ts.http
	}
	return &http.Client{Timeout: 30 * time.Second}
}

func (ts *nebiusTokenSource) clock() time.Time {
	if ts.now != nil {
		return ts.now()
	}
	return time.Now()
}

// Token returns a valid Nebius IAM access token, refreshing it via the
// token-exchange endpoint when the cached one is missing or near expiry.
func (ts *nebiusTokenSource) Token(ctx context.Context) (string, error) {
	ts.mu.Lock()
	defer ts.mu.Unlock()

	now := ts.clock()
	if ts.token != "" && now.Before(ts.expiry.Add(-5*time.Minute)) {
		return ts.token, nil
	}

	jwt, err := nebiusJWT(ts.cfg.PrivateKeyPEM, ts.cfg.PublicKeyID, ts.cfg.ServiceAccountID, now)
	if err != nil {
		return "", err
	}

	reqBody := struct {
		GrantType          string `json:"grant_type"`
		RequestedTokenType string `json:"requested_token_type"`
		SubjectTokenType   string `json:"subject_token_type"`
		SubjectToken       string `json:"subject_token"`
	}{
		GrantType:          "urn:ietf:params:oauth:grant-type:token-exchange",
		RequestedTokenType: "urn:ietf:params:oauth:token-type:access_token",
		SubjectTokenType:   "urn:ietf:params:oauth:token-type:jwt",
		SubjectToken:       jwt,
	}
	bodyJSON, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("marshal token-exchange request: %w", err)
	}

	// VERIFY(nebius-smoke): token-exchange endpoint path
	url := ts.cfg.endpoint() + "/iam/v1/tokens:exchange"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(bodyJSON))
	if err != nil {
		return "", fmt.Errorf("build token-exchange request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	res, err := ts.client().Do(req)
	if err != nil {
		return "", fmt.Errorf("nebius token exchange: %w", err)
	}
	defer res.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("read token-exchange response: %w", err)
	}

	if res.StatusCode < 200 || res.StatusCode > 299 {
		return "", fmt.Errorf("nebius token exchange: %s: %s", res.Status, string(raw))
	}

	var out struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("decode token-exchange response: %w", err)
	}

	ts.token = out.AccessToken
	ts.expiry = now.Add(time.Duration(out.ExpiresIn) * time.Second)
	return ts.token, nil
}
