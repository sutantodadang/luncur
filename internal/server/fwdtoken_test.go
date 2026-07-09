package server

import (
	"strings"
	"testing"
	"time"
)

func TestFwdTokenRoundTrip(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	now := time.Unix(1_700_000_000, 0)

	tok := mintFwdToken(key, 42, now.Add(60*time.Second))
	id, ok := verifyFwdToken(key, tok, now)
	if !ok || id != 42 {
		t.Fatalf("want (42,true), got (%d,%v)", id, ok)
	}

	// expired
	if _, ok := verifyFwdToken(key, tok, now.Add(61*time.Second)); ok {
		t.Fatal("expired token verified")
	}
	// wrong key
	if _, ok := verifyFwdToken([]byte("ffffffffffffffffffffffffffffffff"), tok, now); ok {
		t.Fatal("wrong-key token verified")
	}
	// tampered payload
	bad := strings.Replace(tok, "42.", "43.", 1)
	if _, ok := verifyFwdToken(key, bad, now); ok {
		t.Fatal("tampered token verified")
	}
	// garbage
	for _, g := range []string{"", "x", "1.2", "1.2.zz", "a.b.cc"} {
		if _, ok := verifyFwdToken(key, g, now); ok {
			t.Fatalf("garbage %q verified", g)
		}
	}
}
