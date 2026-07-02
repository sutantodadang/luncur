package server

import (
	"encoding/json"
	"errors"
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
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	tok, err := s.st.CreateToken(u.ID, "login")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"token": tok})
}
