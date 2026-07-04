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

// Cloudflare manages TXT records via the v4 REST API with a bearer token.
type Cloudflare struct {
	Token      string
	BaseURL    string // default https://api.cloudflare.com/client/v4
	HTTPClient *http.Client
}

func (c *Cloudflare) base() string {
	if c.BaseURL != "" {
		return strings.TrimRight(c.BaseURL, "/")
	}
	return "https://api.cloudflare.com/client/v4"
}

func (c *Cloudflare) client() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	return http.DefaultClient
}

// do performs one authenticated API call and decodes the standard
// {"success":..,"result":..} envelope into result (may be nil).
func (c *Cloudflare) do(ctx context.Context, method, path string, body, result any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base()+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	var env struct {
		Success bool            `json:"success"`
		Result  json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(raw, &env); err != nil || !env.Success {
		return fmt.Errorf("cloudflare %s %s: %d %s", method, path, resp.StatusCode, truncate(raw))
	}
	if result != nil {
		return json.Unmarshal(env.Result, result)
	}
	return nil
}

func truncate(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 256 {
		return s[:256]
	}
	return s
}

// zoneID resolves the zone containing fqdn by longest-suffix match:
// walk the label suffixes from longest to shortest and take the first
// zone the API returns.
func (c *Cloudflare) zoneID(ctx context.Context, fqdn string) (string, error) {
	labels := strings.Split(strings.TrimSuffix(fqdn, "."), ".")
	for i := 0; i <= len(labels)-2; i++ {
		cand := strings.Join(labels[i:], ".")
		var zones []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		}
		if err := c.do(ctx, http.MethodGet, "/zones?name="+url.QueryEscape(cand), nil, &zones); err != nil {
			return "", err
		}
		if len(zones) > 0 {
			return zones[0].ID, nil
		}
	}
	return "", fmt.Errorf("cloudflare: no zone found for %s", fqdn)
}

func (c *Cloudflare) Present(ctx context.Context, fqdn, value string) error {
	zone, err := c.zoneID(ctx, fqdn)
	if err != nil {
		return err
	}
	rec := map[string]any{"type": "TXT", "name": fqdn, "content": value, "ttl": 60}
	return c.do(ctx, http.MethodPost, "/zones/"+zone+"/dns_records", rec, nil)
}

func (c *Cloudflare) CleanUp(ctx context.Context, fqdn, value string) error {
	zone, err := c.zoneID(ctx, fqdn)
	if err != nil {
		return err
	}
	var recs []struct {
		ID      string `json:"id"`
		Content string `json:"content"`
	}
	q := "/zones/" + zone + "/dns_records?type=TXT&name=" + url.QueryEscape(fqdn)
	if err := c.do(ctx, http.MethodGet, q, nil, &recs); err != nil {
		return err
	}
	for _, r := range recs {
		if r.Content != value {
			continue
		}
		if err := c.do(ctx, http.MethodDelete, "/zones/"+zone+"/dns_records/"+r.ID, nil, nil); err != nil {
			return err
		}
	}
	return nil
}
