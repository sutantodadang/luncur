package store

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

func ed25519GenerateKey() (ed25519.PublicKey, ed25519.PrivateKey, error) {
	return ed25519.GenerateKey(rand.Reader)
}

// testPubKey generates a fresh ed25519 key in authorized_keys format.
func testPubKey(t *testing.T) string {
	t.Helper()
	_, priv, err := ed25519GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return string(ssh.MarshalAuthorizedKey(signer.PublicKey()))
}

func TestSSHKeyRoundTrip(t *testing.T) {
	s := openTest(t)
	u, err := s.CreateUser("dev@example.com", "password123", "member")
	if err != nil {
		t.Fatal(err)
	}
	pub := testPubKey(t)

	k, err := s.AddSSHKey(u.ID, "laptop", pub)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(k.Fingerprint, "SHA256:") {
		t.Fatalf("fingerprint = %q, want SHA256:...", k.Fingerprint)
	}

	got, err := s.UserForSSHFingerprint(k.Fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != u.ID {
		t.Fatalf("user = %d, want %d", got.ID, u.ID)
	}

	list, err := s.ListSSHKeys(u.ID)
	if err != nil || len(list) != 1 || list[0].Name != "laptop" {
		t.Fatalf("list = %+v err=%v", list, err)
	}

	// Duplicate key rejected.
	if _, err := s.AddSSHKey(u.ID, "again", pub); err == nil {
		t.Fatal("duplicate public key accepted")
	}
	// Garbage rejected.
	if _, err := s.AddSSHKey(u.ID, "bad", "not a key"); err == nil {
		t.Fatal("garbage public key accepted")
	}

	if err := s.DeleteSSHKey(u.ID, k.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UserForSSHFingerprint(k.Fingerprint); !errors.Is(err, ErrAuthFailed) {
		t.Fatalf("deleted key: got %v, want ErrAuthFailed", err)
	}
	// Deleting someone else's key id → ErrNotFound.
	if err := s.DeleteSSHKey(u.ID+1, k.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign delete: got %v, want ErrNotFound", err)
	}
}
