package server

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/sutantodadang/luncur/internal/store"
)

// settableKeys guards the settings API: only install-level knobs luncur
// understands, with per-key validation.
var settableKeys = map[string]func(string) bool{
	"cert_provider": func(v string) bool {
		return v == "builtin" || v == "traefik" || v == "cert-manager"
	},
	"acme_email":           func(v string) bool { return true },
	"acme_directory":       func(v string) bool { return true },
	"backup_s3_endpoint":   func(v string) bool { return v != "" },
	"backup_s3_bucket":     func(v string) bool { return v != "" },
	"backup_s3_prefix":     func(v string) bool { return v != "" },
	"backup_s3_access_key": func(v string) bool { return v != "" },
	"backup_s3_secret_key": func(v string) bool { return v != "" },
	"backup_schedule": func(v string) bool {
		return v == "daily" || v == "off"
	},
	"backup_keep": func(v string) bool {
		n, err := strconv.Atoi(v)
		return err == nil && n > 0
	},
	"registry_keep": func(v string) bool {
		n, err := strconv.Atoi(v)
		return err == nil && n > 0
	},
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
	if key == "backup_s3_secret_key" {
		v = "(set)"
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

	value := req.Value
	if key == "backup_s3_secret_key" {
		if s.sealer == nil {
			writeError(w, http.StatusServiceUnavailable, "sealer_unavailable", "sealer is not configured")
			return
		}
		sealed, err := s.sealer.Seal([]byte(req.Value))
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal", "internal error")
			return
		}
		value = "sealed:" + hex.EncodeToString(sealed)
	}

	if err := s.st.SetSetting(key, value); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// s3SecretKey unseals the write-only backup_s3_secret_key setting.
func (s *server) s3SecretKey() (string, error) {
	v, err := s.st.GetSetting("backup_s3_secret_key")
	if err != nil {
		return "", err
	}
	raw, ok := strings.CutPrefix(v, "sealed:")
	if !ok {
		return "", fmt.Errorf("backup_s3_secret_key is not sealed")
	}
	b, err := hex.DecodeString(raw)
	if err != nil {
		return "", err
	}
	if s.sealer == nil {
		return "", errSealerUnavailable
	}
	plain, err := s.sealer.Open(b)
	if err != nil {
		return "", err
	}
	return string(plain), nil
}
