package dns

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// Hetzner manages TXT records via the Hetzner DNS v1 REST API
// (https://dns.hetzner.com/api-docs), authenticated with an API token in the
// Auth-API-Token header. Hetzner stores the TXT value raw (unquoted) — unlike
// deSEC, it does not expect DNS zone-file style quoting.
type Hetzner struct {
	Token      string
	BaseURL    string // default https://dns.hetzner.com/api/v1
	HTTPClient *http.Client
}

func (h *Hetzner) base() string {
	if h.BaseURL != "" {
		return strings.TrimRight(h.BaseURL, "/")
	}
	return "https://dns.hetzner.com/api/v1"
}

func (h *Hetzner) client() *http.Client {
	if h.HTTPClient != nil {
		return h.HTTPClient
	}
	return http.DefaultClient
}

// do performs one authenticated API call and decodes the response body (a
// bare object/array, not an envelope like Cloudflare's) into result (may be
// nil).
func (h *Hetzner) do(ctx context.Context, method, path string, body, result any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, h.base()+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Auth-API-Token", h.Token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := h.client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("hetzner %s %s: %d %s", method, path, resp.StatusCode, truncate(raw))
	}
	if result != nil && len(raw) > 0 {
		return json.Unmarshal(raw, result)
	}
	return nil
}

// resolveZone resolves the zone containing fqdn by longest-suffix match:
// walk the label suffixes from longest to shortest and take the first zone
// the API returns. Returns the zone's id (for record CRUD) and name (to
// compute the record name relative to the zone).
func (h *Hetzner) resolveZone(ctx context.Context, fqdn string) (id, name string, err error) {
	labels := strings.Split(strings.TrimSuffix(fqdn, "."), ".")
	for i := 0; i <= len(labels)-2; i++ {
		cand := strings.Join(labels[i:], ".")
		var out struct {
			Zones []struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"zones"`
		}
		if err := h.do(ctx, http.MethodGet, "/zones?name="+url.QueryEscape(cand), nil, &out); err != nil {
			return "", "", err
		}
		if len(out.Zones) > 0 {
			return out.Zones[0].ID, out.Zones[0].Name, nil
		}
	}
	return "", "", fmt.Errorf("hetzner: no zone found for %s", fqdn)
}

// hetznerName returns the record name relative to zone; the apex (fqdn ==
// zone, or the trimmed name is empty) is "@".
func hetznerName(fqdn, zone string) string {
	fqdn = strings.TrimSuffix(fqdn, ".")
	if fqdn == zone {
		return "@"
	}
	name := strings.TrimSuffix(fqdn, "."+zone)
	if name == "" {
		return "@"
	}
	return name
}

func (h *Hetzner) Present(ctx context.Context, fqdn, value string) error {
	zoneID, zoneName, err := h.resolveZone(ctx, fqdn)
	if err != nil {
		return err
	}
	rec := map[string]any{
		"zone_id": zoneID,
		"type":    "TXT",
		"name":    hetznerName(fqdn, zoneName),
		"value":   value,
		"ttl":     60,
	}
	return h.do(ctx, http.MethodPost, "/records", rec, nil)
}

func (h *Hetzner) CleanUp(ctx context.Context, fqdn, value string) error {
	zoneID, zoneName, err := h.resolveZone(ctx, fqdn)
	if err != nil {
		return err
	}
	name := hetznerName(fqdn, zoneName)

	var out struct {
		Records []struct {
			ID    string `json:"id"`
			Type  string `json:"type"`
			Name  string `json:"name"`
			Value string `json:"value"`
		} `json:"records"`
	}
	if err := h.do(ctx, http.MethodGet, "/records?zone_id="+url.QueryEscape(zoneID), nil, &out); err != nil {
		return err
	}
	for _, r := range out.Records {
		if r.Type != "TXT" || r.Name != name || r.Value != value {
			continue
		}
		if err := h.do(ctx, http.MethodDelete, "/records/"+r.ID, nil, nil); err != nil {
			return err
		}
	}
	return nil
}
