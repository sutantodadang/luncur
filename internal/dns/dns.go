// Package dns abstracts DNS record management for ACME DNS-01 challenges.
// fqdn is always the full challenge name (_acme-challenge.<domain>);
// value is the TXT record contents.
package dns

import "context"

// Provider creates and removes one TXT record.
type Provider interface {
	Present(ctx context.Context, fqdn, value string) error
	CleanUp(ctx context.Context, fqdn, value string) error
}
