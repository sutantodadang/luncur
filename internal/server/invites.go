package server

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"

	"github.com/sutantodadang/luncur/internal/store"
)

func inviteJSON(i store.Invite) map[string]any {
	return map[string]any{
		"token": i.Token, "role": i.Role, "expires_at": i.ExpiresAt,
		"path": "/ui/register?token=" + i.Token,
		"used": i.UsedBy != 0,
	}
}

func (s *server) handleCreateInvite(w http.ResponseWriter, r *http.Request, u store.User) {
	var req struct {
		Role string `json:"role"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	if req.Role == "" {
		req.Role = "member"
	}
	inv, err := s.st.CreateInvite(req.Role, u.ID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, inviteJSON(inv))
}

func (s *server) handleListInvites(w http.ResponseWriter, r *http.Request, _ store.User) {
	list, err := s.st.ListInvites()
	if err != nil {
		log.Printf("list invites: %v", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	out := make([]map[string]any, 0, len(list))
	for _, i := range list {
		out = append(out, inviteJSON(i))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *server) handleRevokeInvite(w http.ResponseWriter, r *http.Request, _ store.User) {
	if err := s.st.RevokeInvite(r.PathValue("token")); errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "no such invite")
		return
	} else if err != nil {
		log.Printf("revoke invite: %v", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
