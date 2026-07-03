package server

import (
	"errors"
	"log"
	"net/http"
	"strconv"

	"github.com/sutantodadang/luncur/internal/store"
)

func (s *server) handleListTokens(w http.ResponseWriter, r *http.Request, u store.User) {
	list, err := s.st.ListTokens(u.ID)
	if err != nil {
		log.Printf("list tokens: %v", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	out := make([]map[string]any, 0, len(list))
	for _, t := range list {
		out = append(out, map[string]any{
			"id": t.ID, "name": t.Name, "created_at": t.CreatedAt,
			"last_used_at": t.LastUsedAt, "expires_at": t.ExpiresAt,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *server) handleRevokeToken(w http.ResponseWriter, r *http.Request, u store.User) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid token id")
		return
	}
	if err := s.st.RevokeToken(u.ID, id); errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "no such token")
		return
	} else if err != nil {
		log.Printf("revoke token: %v", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
