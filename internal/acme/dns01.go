package acme

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/sutantodadang/luncur/internal/dns"
)

// DNS01Solver answers dns-01 challenges through a dns.Provider: publish
// TXT _acme-challenge.<domain> = base64url(sha256(keyAuth)), wait until
// the record is visible, hand back a cleanup that removes it.
type DNS01Solver struct {
	Provider dns.Provider

	// LookupTXT polls for propagation; default queries the domain's
	// authoritative nameservers directly (recursive caches would hold a
	// stale NXDOMAIN for the freshly created record).
	LookupTXT func(ctx context.Context, fqdn string) ([]string, error)

	Timeout  time.Duration // total propagation wait, default 2m
	Interval time.Duration // poll interval, default 2s
}

func (s *DNS01Solver) Type() string { return "dns-01" }

func (s *DNS01Solver) Setup(ctx context.Context, domain, token, keyAuth string) (func(), error) {
	fqdn := "_acme-challenge." + strings.TrimPrefix(domain, "*.")
	sum := sha256.Sum256([]byte(keyAuth))
	value := base64.RawURLEncoding.EncodeToString(sum[:])

	if err := s.Provider.Present(ctx, fqdn, value); err != nil {
		return nil, fmt.Errorf("dns present %s: %w", fqdn, err)
	}
	cleanup := func() {
		cctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		// Best-effort: a leftover TXT record is harmless.
		_ = s.Provider.CleanUp(cctx, fqdn, value)
	}

	if err := s.waitPropagation(ctx, fqdn, value); err != nil {
		cleanup()
		return nil, err
	}
	return cleanup, nil
}

func (s *DNS01Solver) waitPropagation(ctx context.Context, fqdn, value string) error {
	timeout := s.Timeout
	if timeout == 0 {
		timeout = 2 * time.Minute
	}
	interval := s.Interval
	if interval == 0 {
		interval = 2 * time.Second
	}
	lookup := s.LookupTXT
	if lookup == nil {
		lookup = authoritativeTXT
	}

	wctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for {
		vals, err := lookup(wctx, fqdn)
		if err == nil {
			for _, v := range vals {
				if v == value {
					return nil
				}
			}
		}
		select {
		case <-wctx.Done():
			return fmt.Errorf("dns-01: TXT %s did not propagate within %s", fqdn, timeout)
		case <-time.After(interval):
		}
	}
}

// authoritativeTXT resolves fqdn's TXT records against the zone's own
// authoritative nameserver: walk parent labels until an NS record set is
// found, then query the first NS directly.
func authoritativeTXT(ctx context.Context, fqdn string) ([]string, error) {
	labels := strings.Split(strings.TrimSuffix(fqdn, "."), ".")
	var nss []*net.NS
	for i := 1; i < len(labels)-1; i++ {
		zone := strings.Join(labels[i:], ".")
		if found, err := net.DefaultResolver.LookupNS(ctx, zone); err == nil && len(found) > 0 {
			nss = found
			break
		}
	}
	if len(nss) == 0 {
		// Fall back to the system resolver.
		return net.DefaultResolver.LookupTXT(ctx, fqdn)
	}
	r := &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, net.JoinHostPort(strings.TrimSuffix(nss[0].Host, "."), "53"))
		},
	}
	return r.LookupTXT(ctx, fqdn)
}
