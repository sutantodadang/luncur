package server

import (
	"encoding/json"
	"net/http"

	"github.com/sutantodadang/luncur/internal/store"
)

func userJSON(u store.User) map[string]any {
	return map[string]any{"id": u.ID, "email": u.Email, "role": u.Role}
}

func (s *server) handleMe(w http.ResponseWriter, r *http.Request, u store.User) {
	writeJSON(w, http.StatusOK, userJSON(u))
}

func (s *server) handleCreateUser(w http.ResponseWriter, r *http.Request, _ store.User) {
	var req struct {
		Email    string `json:"email"`
		Password string `json:"password"`
		Role     string `json:"role"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	if len(req.Password) < 8 {
		writeError(w, http.StatusBadRequest, "bad_request", "password must be at least 8 characters")
		return
	}
	u, err := s.st.CreateUser(req.Email, req.Password, req.Role)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, userJSON(u))
}
