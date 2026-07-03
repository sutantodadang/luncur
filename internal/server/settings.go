package server

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/sutantodadang/luncur/internal/store"
)

// settableKeys guards the settings API: only install-level knobs luncur
// understands, with per-key validation.
var settableKeys = map[string]func(string) bool{
	"cert_provider": func(v string) bool {
		return v == "builtin" || v == "traefik" || v == "cert-manager"
	},
	"acme_email":     func(v string) bool { return true },
	"acme_directory": func(v string) bool { return true },
}

func (s *server) handleGetSetting(w http.ResponseWriter, r *http.Request, _ store.User) {
	key := r.PathValue("key")
	if _, ok := settableKeys[key]; !ok {
		writeError(w, http.StatusBadRequest, "bad_request", "unknown setting")
		return
	}
	v, err := s.st.GetSetting(key)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "setting not set")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"key": key, "value": v})
}

func (s *server) handleSetSetting(w http.ResponseWriter, r *http.Request, _ store.User) {
	key := r.PathValue("key")
	valid, ok := settableKeys[key]
	if !ok {
		writeError(w, http.StatusBadRequest, "bad_request", "unknown setting")
		return
	}
	var req struct {
		Value string `json:"value"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || !valid(req.Value) {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid value")
		return
	}
	if err := s.st.SetSetting(key, req.Value); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
