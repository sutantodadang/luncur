package server

import (
	"errors"
	"fmt"

	"github.com/sutantodadang/luncur/internal/dns"
	"github.com/sutantodadang/luncur/internal/store"
)

// errNoDNS means dns_provider is none/unset — a valid steady state, not a
// configuration error.
var errNoDNS = errors.New("no dns provider configured")

// dnsProviderName reads the install-level dns_provider setting.
func (s *server) dnsProviderName() string {
	v, err := s.st.GetSetting("dns_provider")
	if err != nil || v == "" {
		return "none"
	}
	return v
}

// plainSetting reads a non-sealed setting, mapping ErrNotFound to a
// missing-key error mentioning the key.
func (s *server) plainSetting(key string) (string, error) {
	v, err := s.st.GetSetting(key)
	if errors.Is(err, store.ErrNotFound) {
		return "", fmt.Errorf("%s not set", key)
	}
	return v, err
}

// dnsProviderFromSettings is the default dnsProvider factory: build the
// configured provider from (sealed) settings. errNoDNS when none.
func (s *server) dnsProviderFromSettings() (dns.Provider, error) {
	switch s.dnsProviderName() {
	case "cloudflare":
		token, err := s.sealedSetting("dns_cloudflare_token")
		if err != nil {
			return nil, fmt.Errorf("dns_cloudflare_token: %w", err)
		}
		return &dns.Cloudflare{Token: token}, nil
	case "route53":
		access, err := s.plainSetting("dns_route53_access_key")
		if err != nil {
			return nil, err
		}
		secretKey, err := s.sealedSetting("dns_route53_secret_key")
		if err != nil {
			return nil, fmt.Errorf("dns_route53_secret_key: %w", err)
		}
		region, _ := s.st.GetSetting("dns_route53_region") // optional, default us-east-1
		return &dns.Route53{AccessKey: access, SecretKey: secretKey, Region: region}, nil
	case "rfc2136":
		server, err := s.plainSetting("dns_rfc2136_server")
		if err != nil {
			return nil, err
		}
		name, err := s.plainSetting("dns_rfc2136_tsig_name")
		if err != nil {
			return nil, err
		}
		secretVal, err := s.sealedSetting("dns_rfc2136_tsig_secret")
		if err != nil {
			return nil, fmt.Errorf("dns_rfc2136_tsig_secret: %w", err)
		}
		algo, _ := s.st.GetSetting("dns_rfc2136_tsig_algo") // optional, default hmac-sha256
		return &dns.RFC2136{Server: server, TSIGName: name, TSIGSecret: secretVal, TSIGAlgo: algo}, nil
	default:
		return nil, errNoDNS
	}
}
