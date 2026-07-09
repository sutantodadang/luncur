package server

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestRateLimiterAllowsThenBlocks(t *testing.T) {
	now := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	l := newRateLimiter(func() time.Time { return now })
	for i := 0; i < loginLimit; i++ {
		if !l.allow("1.2.3.4") {
			t.Fatalf("attempt %d should be allowed", i+1)
		}
	}
	if l.allow("1.2.3.4") {
		t.Fatal("attempt over limit should be blocked")
	}
	if !l.allow("5.6.7.8") {
		t.Fatal("other IP must not be affected")
	}
	now = now.Add(loginWindow + time.Second)
	if !l.allow("1.2.3.4") {
		t.Fatal("window expiry must reset the counter")
	}
}

func TestLoginEndpointRateLimited(t *testing.T) {
	srv, _ := testServer(t)
	var last int
	for i := 0; i < loginLimit+1; i++ {
		resp, err := http.Post(srv.URL+"/v1/login", "application/json",
			strings.NewReader(`{"email":"no@such.user","password":"wrong-pw"}`))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		last = resp.StatusCode
	}
	if last != http.StatusTooManyRequests {
		t.Fatalf("attempt %d: got %d, want 429", loginLimit+1, last)
	}
}
