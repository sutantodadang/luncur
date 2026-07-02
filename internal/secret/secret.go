// Package secret seals small values (env vars, git tokens) at rest with
// AES-256-GCM. The key lives in a file created at first boot.
package secret

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
)

type Sealer struct {
	aead cipher.AEAD
}

func New(key []byte) (*Sealer, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("key must be 32 bytes, got %d", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Sealer{aead: aead}, nil
}

// LoadOrCreate reads a 64-hex-char key file, generating it (0600) if absent.
func LoadOrCreate(path string) (*Sealer, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		key := make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return nil, err
		}
		if err := os.WriteFile(path, []byte(hex.EncodeToString(key)), 0o600); err != nil {
			return nil, err
		}
		return New(key)
	}
	if err != nil {
		return nil, err
	}
	key, err := hex.DecodeString(string(b))
	if err != nil {
		return nil, fmt.Errorf("key file %s: %w", path, err)
	}
	return New(key)
}

func (s *Sealer) Seal(plain []byte) ([]byte, error) {
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return s.aead.Seal(nonce, nonce, plain, nil), nil
}

func (s *Sealer) Open(box []byte) ([]byte, error) {
	ns := s.aead.NonceSize()
	if len(box) < ns {
		return nil, errors.New("sealed value too short")
	}
	return s.aead.Open(nil, box[:ns], box[ns:], nil)
}
