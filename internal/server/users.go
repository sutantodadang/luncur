package server

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"

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
	u, err := s.st.CreateUser(req.Email, req.Password, req.Role)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed: users.email") {
			writeError(w, http.StatusConflict, "email_taken", "email already exists")
			return
		}
		var ve *store.ValidationError
		if errors.As(err, &ve) {
			writeError(w, http.StatusBadRequest, "bad_request", ve.Error())
			return
		}
		log.Printf("create user: %v", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	writeJSON(w, http.StatusCreated, userJSON(u))
}

func (s *server) handleListUsers(w http.ResponseWriter, r *http.Request, _ store.User) {
	list, err := s.st.ListUsers()
	if err != nil {
		log.Printf("list users: %v", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	out := make([]map[string]any, 0, len(list))
	for _, u := range list {
		out = append(out, map[string]any{
			"id": u.ID, "email": u.Email, "role": u.Role,
			"created_at": u.CreatedAt, "token_count": u.TokenCount,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *server) handleDeleteUser(w http.ResponseWriter, r *http.Request, u store.User) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid user id")
		return
	}
	if id == u.ID {
		writeError(w, http.StatusBadRequest, "bad_request", "cannot delete yourself")
		return
	}
	if err := s.st.DeleteUser(id); errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "no such user")
		return
	} else if err != nil {
		log.Printf("delete user: %v", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
