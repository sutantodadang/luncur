package server

import (
	"errors"
	"log"
	"net/http"
	"net/url"
	"strconv"

	"github.com/sutantodadang/luncur/internal/store"
)

// settingField describes one settings.html input: Options non-nil renders a
// <select> with those choices; Sealed renders a password input that never
// echoes the stored value, only whether one is set.
type settingField struct {
	Key     string
	Options []string
	Sealed  bool
}

// settingGroup is one settings.html card: a heading plus its fields, in the
// exact order the plan lists them.
type settingGroup struct {
	Title  string
	Fields []settingField
}

var settingGroups = []settingGroup{
	{Title: "Certificates", Fields: []settingField{
		{Key: "cert_provider", Options: []string{"builtin", "traefik", "cert-manager"}},
		{Key: "acme_email"},
		{Key: "acme_directory"},
		{Key: "panel_domain"},
	}},
	{Title: "DNS", Fields: []settingField{
		{Key: "dns_provider", Options: []string{"cloudflare", "route53", "rfc2136", "none"}},
		{Key: "dns_cloudflare_token", Sealed: true},
		{Key: "dns_route53_access_key"},
		{Key: "dns_route53_secret_key", Sealed: true},
		{Key: "dns_route53_region"},
		{Key: "dns_rfc2136_server"},
		{Key: "dns_rfc2136_tsig_name"},
		{Key: "dns_rfc2136_tsig_secret", Sealed: true},
		{Key: "dns_rfc2136_tsig_algo"},
	}},
	{Title: "SMTP", Fields: []settingField{
		{Key: "smtp_host"},
		{Key: "smtp_port"},
		{Key: "smtp_user"},
		{Key: "smtp_pass", Sealed: true},
		{Key: "smtp_from"},
	}},
	{Title: "Notifications", Fields: []settingField{
		{Key: "notify_url", Sealed: true},
		{Key: "notify_format", Options: []string{"generic", "discord", "slack", "telegram"}},
		{Key: "notify_telegram_chat"},
		{Key: "notify_events"},
	}},
	{Title: "Backups", Fields: []settingField{
		{Key: "backup_s3_endpoint"},
		{Key: "backup_s3_bucket"},
		{Key: "backup_s3_prefix"},
		{Key: "backup_s3_access_key"},
		{Key: "backup_s3_secret_key", Sealed: true},
		{Key: "backup_schedule", Options: []string{"daily", "off"}},
		{Key: "backup_keep"},
	}},
	{Title: "Registry & builds", Fields: []settingField{
		{Key: "registry_keep"},
		{Key: "build_cache", Options: []string{"on", "off"}},
		{Key: "build_timeout_minutes"},
	}},
	{Title: "Audit", Fields: []settingField{
		{Key: "audit_retention_days"},
	}},
}

// settingRow is one field's rendered view: Value carries the current
// non-sealed value (sealed fields never echo their plaintext — Placeholder
// says "(set)"/"not set" instead).
type settingRow struct {
	Key         string
	Options     []string
	Sealed      bool
	Value       string
	Placeholder string
}

// settingGroupView is settingGroup with its fields resolved to settingRow.
type settingGroupView struct {
	Title string
	Rows  []settingRow
}

func (s *server) handleUISettings(w http.ResponseWriter, r *http.Request, u store.User) {
	if !s.uiAdmin(w, u) {
		return
	}
	groups := make([]settingGroupView, 0, len(settingGroups))
	for _, g := range settingGroups {
		rows := make([]settingRow, 0, len(g.Fields))
		for _, f := range g.Fields {
			row := settingRow{Key: f.Key, Options: f.Options, Sealed: f.Sealed}
			v, err := s.st.GetSetting(f.Key)
			if err != nil && !errors.Is(err, store.ErrNotFound) {
				log.Printf("ui settings get %s: %v", f.Key, err)
				http.Error(w, "internal error", http.StatusInternalServerError)
				return
			}
			has := err == nil && v != ""
			if f.Sealed {
				if has {
					row.Placeholder = "(set)"
				} else {
					row.Placeholder = "not set"
				}
			} else {
				row.Value = v
			}
			rows = append(rows, row)
		}
		groups = append(groups, settingGroupView{Title: g.Title, Rows: rows})
	}

	var banner string
	q := r.URL.Query()
	switch {
	case q.Get("saved") != "":
		banner = "saved " + q.Get("saved")
	case q.Get("err") != "":
		banner = "error: " + q.Get("err")
	case q.Get("gc") != "":
		banner = "registry GC: " + q.Get("gc") + " manifest(s) deleted"
	}

	// Panel custom-domain status: a read-only line below the Certificates
	// group, not one of the settingRow fields (panel_cert_* aren't
	// settableKeys — they're written internally by the cert manager).
	panelDomain, _ := s.st.GetSetting("panel_domain")
	var panelStatus, panelCertErr, panelExpiresAt string
	if panelDomain != "" {
		panelStatus, _ = s.st.GetSetting("panel_cert_status")
		panelCertErr, _ = s.st.GetSetting("panel_cert_error")
		panelExpiresAt, _ = s.st.GetSetting("panel_cert_expires_at")
	}

	s.renderPage(w, "settings.html", map[string]any{
		"User": u, "Groups": groups, "Banner": banner,
		"CSRF": s.csrf(w, r), "IsAdmin": true, "Version": s.version,
		"PanelDomain": panelDomain, "PanelCertStatus": panelStatus,
		"PanelCertError": panelCertErr, "PanelCertExpiresAt": panelExpiresAt,
		"ExternalIP": s.externalIP,
	})
}

// handleUISettingsSet is the settings page's write path: same setSetting
// core the JSON API's handleSetSetting uses, redirect-with-banner instead of
// a status code/body.
func (s *server) handleUISettingsSet(w http.ResponseWriter, r *http.Request, u store.User) {
	if !s.uiAdmin(w, u) {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	key := r.PostFormValue("key")
	value := r.PostFormValue("value")

	if err := s.setSetting(key, value); err != nil {
		switch {
		case errors.Is(err, errUnknownSetting):
			http.Error(w, "unknown setting", http.StatusBadRequest)
		case errors.Is(err, errInvalidSettingValue):
			http.Redirect(w, r, "/ui/settings?err="+url.QueryEscape("invalid value for "+key), http.StatusSeeOther)
		case errors.Is(err, errSealerUnavailable):
			http.Redirect(w, r, "/ui/settings?err="+url.QueryEscape("sealer is not configured"), http.StatusSeeOther)
		default:
			log.Printf("ui set setting: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
		}
		return
	}
	if key == "panel_domain" {
		if err := s.panelDomainChanged(r.Context()); err != nil {
			log.Printf("panel domain changed: %v", err)
		}
	}
	http.Redirect(w, r, "/ui/settings?saved="+url.QueryEscape(key), http.StatusSeeOther)
}

// handleUISettingsUpdate is the settings page's self-update form: same
// updateServerImage core the JSON API's handleSystemUpdate uses, redirect
// back to the settings page instead of a status code/body.
func (s *server) handleUISettingsUpdate(w http.ResponseWriter, r *http.Request, u store.User) {
	if !s.uiAdmin(w, u) {
		return
	}
	if s.kube == nil {
		http.Error(w, "kubernetes is not configured", http.StatusServiceUnavailable)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}

	if _, err := s.updateServerImage(r.Context(), r.PostFormValue("version"), ""); err != nil {
		if errors.Is(err, errInvalidUpdateRequest) {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		log.Printf("ui system update: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/ui/settings?saved=update", http.StatusSeeOther)
}

// handleUIRegistryGC runs the same core the /v1/registry/gc API uses,
// redirecting back to the settings page with a result banner instead of a
// JSON body.
func (s *server) handleUIRegistryGC(w http.ResponseWriter, r *http.Request, u store.User) {
	if !s.uiAdmin(w, u) {
		return
	}
	report, err := s.runRegistryGC(r.Context())
	if err != nil {
		log.Printf("ui registry gc: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/ui/settings?gc="+strconv.Itoa(report.DeletedManifests), http.StatusSeeOther)
}
