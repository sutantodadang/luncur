// Package acme issues TLS certificates via RFC 8555 (Let's Encrypt) using
// HTTP-01 challenges served by luncur itself.
package acme

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	xacme "golang.org/x/crypto/acme"
)

// LetsEncryptDirectory is the production directory URL.
const LetsEncryptDirectory = "https://acme-v02.api.letsencrypt.org/directory"

const ChallengePath = "/.well-known/acme-challenge/"

// Challenges is a concurrency-safe token → keyAuthorization store that
// doubles as the HTTP handler for the well-known challenge path.
type Challenges struct {
	mu sync.Mutex
	m  map[string]string
}

func NewChallenges() *Challenges { return &Challenges{m: map[string]string{}} }

func (c *Challenges) Put(token, keyAuth string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m[token] = keyAuth
}

func (c *Challenges) Delete(token string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.m, token)
}

func (c *Challenges) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	token := strings.TrimPrefix(r.URL.Path, ChallengePath)
	if token == "" || token == r.URL.Path {
		http.NotFound(w, r)
		return
	}
	c.mu.Lock()
	keyAuth, ok := c.m[token]
	c.mu.Unlock()
	if !ok {
		http.NotFound(w, r)
		return
	}
	fmt.Fprint(w, keyAuth)
}

// Issuer drives one ACME account.
type Issuer struct {
	DirectoryURL string
	AccountKey   *ecdsa.PrivateKey
	Email        string
	Challenges   *Challenges
}

func GenerateAccountKey() (*ecdsa.PrivateKey, error) {
	return ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
}

func EncodeAccountKey(k *ecdsa.PrivateKey) ([]byte, error) {
	der, err := x509.MarshalECPrivateKey(k)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}), nil
}

func DecodeAccountKey(b []byte) (*ecdsa.PrivateKey, error) {
	blk, _ := pem.Decode(b)
	if blk == nil {
		return nil, fmt.Errorf("no PEM block in account key")
	}
	return x509.ParseECPrivateKey(blk.Bytes)
}

// NeedsRenewal reports whether a cert expiring at notAfter should be
// re-issued now (within 30 days of expiry).
func NeedsRenewal(notAfter, now time.Time) bool {
	return now.Add(30 * 24 * time.Hour).After(notAfter)
}

// Issue runs one HTTP-01 order end to end and returns the PEM-encoded cert
// chain + private key and the leaf's NotAfter.
func (i *Issuer) Issue(ctx context.Context, domain string) (certPEM, keyPEM []byte, notAfter time.Time, err error) {
	cl := &xacme.Client{Key: i.AccountKey, DirectoryURL: i.DirectoryURL}

	// Idempotent registration: AlreadyRegistered is fine.
	_, err = cl.Register(ctx, &xacme.Account{Contact: []string{"mailto:" + i.Email}},
		xacme.AcceptTOS)
	if err != nil && err != xacme.ErrAccountAlreadyExists {
		return nil, nil, time.Time{}, fmt.Errorf("acme register: %w", err)
	}

	order, err := cl.AuthorizeOrder(ctx, xacme.DomainIDs(domain))
	if err != nil {
		return nil, nil, time.Time{}, fmt.Errorf("acme new order: %w", err)
	}

	for _, zurl := range order.AuthzURLs {
		z, err := cl.GetAuthorization(ctx, zurl)
		if err != nil {
			return nil, nil, time.Time{}, fmt.Errorf("acme authz: %w", err)
		}
		if z.Status == xacme.StatusValid {
			continue
		}
		var chal *xacme.Challenge
		for _, c := range z.Challenges {
			if c.Type == "http-01" {
				chal = c
				break
			}
		}
		if chal == nil {
			return nil, nil, time.Time{}, fmt.Errorf("no http-01 challenge offered for %s", domain)
		}
		keyAuth, err := cl.HTTP01ChallengeResponse(chal.Token)
		if err != nil {
			return nil, nil, time.Time{}, err
		}
		i.Challenges.Put(chal.Token, keyAuth)
		defer i.Challenges.Delete(chal.Token)

		if _, err := cl.Accept(ctx, chal); err != nil {
			return nil, nil, time.Time{}, fmt.Errorf("acme accept: %w", err)
		}
		if _, err := cl.WaitAuthorization(ctx, z.URI); err != nil {
			return nil, nil, time.Time{}, fmt.Errorf("acme authorization failed: %w", err)
		}
	}

	if _, err := cl.WaitOrder(ctx, order.URI); err != nil {
		return nil, nil, time.Time{}, fmt.Errorf("acme order: %w", err)
	}

	certKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, time.Time{}, err
	}
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject:  pkix.Name{CommonName: domain},
		DNSNames: []string{domain},
	}, certKey)
	if err != nil {
		return nil, nil, time.Time{}, err
	}

	der, _, err := cl.CreateOrderCert(ctx, order.FinalizeURL, csr, true)
	if err != nil {
		return nil, nil, time.Time{}, fmt.Errorf("acme finalize: %w", err)
	}

	for _, b := range der {
		certPEM = append(certPEM, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: b})...)
	}
	leaf, err := x509.ParseCertificate(der[0])
	if err != nil {
		return nil, nil, time.Time{}, err
	}
	keyDER, err := x509.MarshalECPrivateKey(certKey)
	if err != nil {
		return nil, nil, time.Time{}, err
	}
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM, leaf.NotAfter, nil
}
