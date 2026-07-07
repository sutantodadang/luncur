package gpucloud

import "context"

// Provider is one GPU VM cloud. External refs are provider-native ids as
// strings (vast.ai contract ints, Nebius "computeinstance-…" ids).
type Provider interface {
	Name() string
	Rent(ctx context.Context, spec RentSpec) (externalRef string, err error)
	List(ctx context.Context) ([]Instance, error)
	Destroy(ctx context.Context, externalRef string) error
}

// RentSpec carries common fields plus per-provider selectors; each provider
// reads only its own.
type RentSpec struct {
	Label   string
	Image   string // vast.ai VM template; Nebius ignores (image family internal)
	DiskGB  int
	Onstart string // shell script; vast.ai onstart, Nebius wraps in cloud-init runcmd

	VastOfferID int64

	NebiusPlatform string
	NebiusPreset   string
}

var _ Provider = (*VastAI)(nil)
