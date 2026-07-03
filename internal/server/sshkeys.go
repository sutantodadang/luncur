package server

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"

	"github.com/sutantodadang/luncur/internal/store"
)

func (s *server) handleAddSSHKey(w http.ResponseWriter, r *http.Request, u store.User) {
	var req struct {
		Name      string `json:"name"`
		PublicKey string `json:"public_key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	if req.Name == "" {
		req.Name = "key"
	}
	k, err := s.st.AddSSHKey(u.ID, req.Name, req.PublicKey)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"id": k.ID, "name": k.Name, "fingerprint": k.Fingerprint,
	})
}

func (s *server) handleListSSHKeys(w http.ResponseWriter, r *http.Request, u store.User) {
	list, err := s.st.ListSSHKeys(u.ID)
	if err != nil {
		log.Printf("list ssh keys: %v", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	out := make([]map[string]any, 0, len(list))
	for _, k := range list {
		out = append(out, map[string]any{
			"id": k.ID, "name": k.Name, "fingerprint": k.Fingerprint, "created_at": k.CreatedAt,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *server) handleDeleteSSHKey(w http.ResponseWriter, r *http.Request, u store.User) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid key id")
		return
	}
	if err := s.st.DeleteSSHKey(u.ID, id); errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "no such key")
		return
	} else if err != nil {
		log.Printf("delete ssh key: %v", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
