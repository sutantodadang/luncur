package store

import (
	"crypto/rand"
	"math/big"
)

// idAlphabet is deliberately restricted to lowercase ASCII letters and
// digits: deployment ids flow unescaped into Kubernetes Job names
// ("build-<id>", a DNS-1123 subdomain component) and into log/tarball
// filenames on disk, so every character must be safe in both contexts
// without quoting or percent-encoding.
const idAlphabet = "0123456789abcdefghijklmnopqrstuvwxyz"

// idLength is 12 base-36 characters (~4.7e18 possible ids) — enough that
// birthday-bound collisions across a single deployment's history are not a
// practical concern; CreateDeployment/CreateRollbackDeployment still retry
// once on a UNIQUE constraint failure as a belt-and-suspenders measure.
const idLength = 12

// NewID returns a random 12-character lowercase base-36 id (opaque
// deployment identifier). Each character is drawn via crypto/rand.Int,
// which samples uniformly over [0, len(idAlphabet)) with no modulo bias
// (it internally rejects and re-draws out-of-range bytes rather than
// reducing them mod n).
func NewID() string {
	max := big.NewInt(int64(len(idAlphabet)))
	b := make([]byte, idLength)
	for i := range b {
		n, err := rand.Int(rand.Reader, max)
		if err != nil {
			// The platform CSPRNG is broken/exhausted — there is no sensible
			// fallback that stays safe, so fail loudly instead of handing
			// back a predictable or weak id.
			panic("store: crypto/rand unavailable: " + err.Error())
		}
		b[i] = idAlphabet[n.Int64()]
	}
	return string(b)
}
