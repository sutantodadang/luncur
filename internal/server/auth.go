package server

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"

	"github.com/sutantodadang/luncur/internal/store"
)

func (s *server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	u, err := s.st.Authenticate(req.Email, req.Password)
	if errors.Is(err, store.ErrAuthFailed) {
		writeError(w, http.StatusUnauthorized, "auth_failed", "wrong email or password")
		return
	}
	if err != nil {
		log.Printf("login authenticate: %v", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	tok, err := s.st.CreateToken(u.ID, "login")
	if err != nil {
		log.Printf("login create token: %v", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"token": tok})
}

// authed wraps a handler with bearer-token authentication.
func (s *server) authed(next func(http.ResponseWriter, *http.Request, store.User)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		const prefix = "Bearer "
		h := r.Header.Get("Authorization")
		if len(h) <= len(prefix) || h[:len(prefix)] != prefix {
			writeError(w, http.StatusUnauthorized, "unauthorized", "missing bearer token")
			return
		}
		u, err := s.st.UserForToken(h[len(prefix):])
		if err != nil {
			writeError(w, http.StatusUnauthorized, "unauthorized", "invalid token")
			return
		}
		next(w, r, u)
	}
}

func (s *server) adminOnly(next func(http.ResponseWriter, *http.Request, store.User)) http.HandlerFunc {
	return s.authed(func(w http.ResponseWriter, r *http.Request, u store.User) {
		if u.Role != "admin" {
			writeError(w, http.StatusForbidden, "forbidden", "admin role required")
			return
		}
		next(w, r, u)
	})
}
