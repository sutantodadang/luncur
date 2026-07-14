package server

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/sutantodadang/luncur/internal/store"
)

// emailRe is a permissive email-shape check for notify_email: it guards
// against obviously malformed input, not deliverability.
var emailRe = regexp.MustCompile(`^[^\s@]+@[^\s@]+\.[^\s@]+$`)

// validEmailCSV reports whether v is a non-empty CSV of one or more
// addresses, each matching emailRe.
func validEmailCSV(v string) bool {
	if strings.TrimSpace(v) == "" {
		return false
	}
	for _, part := range strings.Split(v, ",") {
		addr := strings.TrimSpace(part)
		if addr == "" || !emailRe.MatchString(addr) {
			return false
		}
	}
	return true
}

// settableKeys guards the settings API: only install-level knobs luncur
// understands, with per-key validation.
var settableKeys = map[string]func(string) bool{
	"cert_provider": func(v string) bool {
		return v == "builtin" || v == "traefik" || v == "cert-manager"
	},
	"acme_email":           func(v string) bool { return true },
	"acme_directory":       func(v string) bool { return true },
	"panel_domain":         validPanelDomain,
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
	"smtp_host": func(v string) bool { return v != "" },
	"smtp_port": func(v string) bool {
		n, err := strconv.Atoi(v)
		return err == nil && n > 0 && n < 65536
	},
	"smtp_user": func(v string) bool { return v != "" },
	"smtp_pass": func(v string) bool { return v != "" },
	"smtp_from": func(v string) bool { return v != "" },
	"dns_provider": func(v string) bool {
		return v == "cloudflare" || v == "route53" || v == "rfc2136" ||
			v == "desec" || v == "hetzner" || v == "digitalocean" || v == "none"
	},
	"dns_cloudflare_token":    func(v string) bool { return v != "" },
	"dns_route53_access_key":  func(v string) bool { return v != "" },
	"dns_route53_secret_key":  func(v string) bool { return v != "" },
	"dns_route53_region":      func(v string) bool { return v != "" },
	"dns_rfc2136_server":      func(v string) bool { return v != "" },
	"dns_rfc2136_tsig_name":   func(v string) bool { return v != "" },
	"dns_rfc2136_tsig_secret": func(v string) bool { return v != "" },
	"dns_rfc2136_tsig_algo":   func(v string) bool { return v != "" },
	"dns_desec_token":         func(v string) bool { return v != "" },
	"dns_hetzner_token":       func(v string) bool { return v != "" },
	"dns_digitalocean_token":  func(v string) bool { return v != "" },
	"notify_url":              func(v string) bool { return v != "" },
	"notify_format":           func(v string) bool { return notifyFormats[v] },
	"notify_telegram_chat":    func(v string) bool { return v != "" },
	"notify_email":            validEmailCSV,
	"notify_events":           validNotifyEvents,
	"build_cache":             func(v string) bool { return v == "on" || v == "off" },
	"build_timeout_minutes": func(v string) bool {
		n, err := strconv.Atoi(v)
		return err == nil && n >= 1 && n <= 720
	},
	"audit_retention_days": func(v string) bool {
		n, err := strconv.Atoi(v)
		return err == nil && n >= 0
	},
	// train_gang_timeout_minutes: multi-node run gang-startup window (see
	// gangGuard in runs.go); 0 disables the guard. Same integer >= 0 shape as
	// audit_retention_days / gpu_idle_minutes.
	"train_gang_timeout_minutes": func(v string) bool {
		n, err := strconv.Atoi(v)
		return err == nil && n >= 0
	},
	// pipeline_engine: default orchestrator for pipelines that don't pin
	// their own engine (store.Pipeline.Engine == ""). "argo" is accepted here
	// (stored) but startPipelineRun rejects it with engine_unavailable until
	// C3 ships the Argo engine.
	"pipeline_engine": func(v string) bool {
		return v == "native" || v == "argo"
	},
	// network_isolation: global switch for per-project NetworkPolicy
	// isolation (see netpolicy.go). Default seeded per-install by
	// store.seedNetworkIsolation; toggling here fans out to every project
	// namespace via networkIsolationChanged.
	"network_isolation": func(v string) bool { return v == "on" || v == "off" },
	// metrics_token: bearer token gating GET /metrics/prometheus. Sealed at
	// rest like backup_s3_secret_key; unset means the endpoint 404s.
	"metrics_token": func(v string) bool { return v != "" },
}

// sealedKeys are write-only secrets: sealed at rest with the install
// sealer, and GET returns "(set)" instead of the value.
var sealedKeys = map[string]bool{
	"backup_s3_secret_key":    true,
	"smtp_pass":               true,
	"dns_cloudflare_token":    true,
	"dns_route53_secret_key":  true,
	"dns_rfc2136_tsig_secret": true,
	"dns_desec_token":         true,
	"dns_hetzner_token":       true,
	"dns_digitalocean_token":  true,
	"notify_url":              true,
	"metrics_token":           true,
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
	if sealedKeys[key] {
		v = "(set)"
	}
	writeJSON(w, http.StatusOK, map[string]string{"key": key, "value": v})
}

// errUnknownSetting and errInvalidSettingValue are setSetting's sentinel
// errors — handleSetSetting and the UI settings handler both map them to the
// same 400 responses/banners so the two surfaces behave identically.
var errUnknownSetting = errors.New("unknown setting")
var errInvalidSettingValue = errors.New("invalid value")

// setSetting is the settings API's write core: validate the key is settable,
// validate the value against its per-key rule, seal it if the key is
// write-only, then persist. Shared by handleSetSetting (JSON API) and the
// UI settings page so both surfaces apply byte-identical validation/sealing.
func (s *server) setSetting(key, value string) error {
	valid, ok := settableKeys[key]
	if !ok {
		return errUnknownSetting
	}
	if !valid(value) {
		return errInvalidSettingValue
	}

	if sealedKeys[key] {
		if s.sealer == nil {
			return errSealerUnavailable
		}
		sealed, err := s.sealer.Seal([]byte(value))
		if err != nil {
			return err
		}
		value = "sealed:" + hex.EncodeToString(sealed)
	}

	return s.st.SetSetting(key, value)
}

func (s *server) handleSetSetting(w http.ResponseWriter, r *http.Request, _ store.User) {
	key := r.PathValue("key")
	var req struct {
		Value string `json:"value"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid value")
		return
	}

	if err := s.setSetting(key, req.Value); err != nil {
		switch {
		case errors.Is(err, errUnknownSetting):
			writeError(w, http.StatusBadRequest, "bad_request", "unknown setting")
		case errors.Is(err, errInvalidSettingValue):
			writeError(w, http.StatusBadRequest, "bad_request", "invalid value")
		case errors.Is(err, errSealerUnavailable):
			writeError(w, http.StatusServiceUnavailable, "sealer_unavailable", "sealer is not configured")
		default:
			writeError(w, http.StatusInternalServerError, "internal", "internal error")
		}
		return
	}
	if key == "panel_domain" {
		if err := s.panelDomainChanged(r.Context()); err != nil {
			log.Printf("panel domain changed: %v", err)
		}
	}
	if key == "network_isolation" {
		if err := s.networkIsolationChanged(r.Context()); err != nil {
			log.Printf("network isolation changed: %v", err)
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// sealedSetting unseals a write-only sealed setting (see sealedKeys).
func (s *server) sealedSetting(key string) (string, error) {
	v, err := s.st.GetSetting(key)
	if err != nil {
		return "", err
	}
	raw, ok := strings.CutPrefix(v, "sealed:")
	if !ok {
		return "", fmt.Errorf("%s is not sealed", key)
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

// s3SecretKey unseals the write-only backup_s3_secret_key setting.
func (s *server) s3SecretKey() (string, error) {
	return s.sealedSetting("backup_s3_secret_key")
}
