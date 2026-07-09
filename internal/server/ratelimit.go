package server

import (
	"net"
	"net/http"
	"sync"
	"time"
)

// loginLimit/loginWindow bound POST /v1/login to loginLimit attempts per
// IP per fixed window — brute-force guard, not a fairness scheduler.
const (
	loginLimit  = 10
	loginWindow = time.Minute
)

type rateLimiter struct {
	mu    sync.Mutex
	hits  map[string]int
	reset time.Time
	now   func() time.Time
}

func newRateLimiter(now func() time.Time) *rateLimiter {
	return &rateLimiter{hits: map[string]int{}, now: now}
}

// allow counts a hit for ip and reports whether it is within the limit.
// The whole map resets when the window rolls over (fixed window: cheap,
// worst case 2x burst at the boundary — fine for a login guard).
func (l *rateLimiter) allow(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := l.now()
	if n.After(l.reset) {
		l.hits = map[string]int{}
		l.reset = n.Add(loginWindow)
	}
	l.hits[ip]++
	return l.hits[ip] <= loginLimit
}

// rateLimited wraps a handler with the per-IP login limiter.
func (s *server) rateLimited(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ip, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			ip = r.RemoteAddr
		}
		if !s.loginLimiter.allow(ip) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte(`{"error":"rate_limited","message":"too many attempts, retry later"}`))
			return
		}
		next(w, r)
	}
}
