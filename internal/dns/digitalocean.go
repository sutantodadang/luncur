package dns

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// DigitalOcean manages TXT records via the v2 REST API with a bearer token.
// DigitalOcean stores the TXT value raw (unquoted) and enforces a minimum
// TTL of 30 seconds.
type DigitalOcean struct {
	Token      string
	BaseURL    string // default https://api.digitalocean.com/v2
	HTTPClient *http.Client
}

// errDONotFound signals a 404 from a singular-resource lookup (GET
// /domains/{name}), distinct from other errors so the zone-resolve walk can
// keep trying shorter suffixes instead of aborting.
var errDONotFound = errors.New("digitalocean: not found")

func (d *DigitalOcean) base() string {
	if d.BaseURL != "" {
		return strings.TrimRight(d.BaseURL, "/")
	}
	return "https://api.digitalocean.com/v2"
}

func (d *DigitalOcean) client() *http.Client {
	if d.HTTPClient != nil {
		return d.HTTPClient
	}
	return http.DefaultClient
}

// do performs one authenticated API call and decodes the response body (a
// bare object/array, not an envelope like Cloudflare's) into result (may be
// nil).
func (d *DigitalOcean) do(ctx context.Context, method, path string, body, result any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, d.base()+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+d.Token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := d.client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode == http.StatusNotFound {
		return errDONotFound
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("digitalocean %s %s: %d %s", method, path, resp.StatusCode, truncate(raw))
	}
	if result != nil && len(raw) > 0 {
		return json.Unmarshal(raw, result)
	}
	return nil
}

// zone resolves the registered domain containing fqdn by longest-suffix
// match: walk the label suffixes from longest to shortest and take the
// first one the API confirms as a registered domain (GET /domains/{name}
// is a singular-resource lookup, so a 404 means "not this one" rather than
// a hard failure).
func (d *DigitalOcean) zone(ctx context.Context, fqdn string) (string, error) {
	labels := strings.Split(strings.TrimSuffix(fqdn, "."), ".")
	for i := 0; i <= len(labels)-2; i++ {
		cand := strings.Join(labels[i:], ".")
		var dom struct {
			Domain struct {
				Name string `json:"name"`
			} `json:"domain"`
		}
		err := d.do(ctx, http.MethodGet, "/domains/"+cand, nil, &dom)
		if err == nil {
			return dom.Domain.Name, nil
		}
		if !errors.Is(err, errDONotFound) {
			return "", err
		}
	}
	return "", fmt.Errorf("digitalocean: no zone found for %s", fqdn)
}

// digitaloceanName returns the record name relative to zone (as required by
// the record create/update body); the apex (fqdn == zone, or the trimmed
// name is empty) is "@".
func digitaloceanName(fqdn, zone string) string {
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

func (d *DigitalOcean) Present(ctx context.Context, fqdn, value string) error {
	zone, err := d.zone(ctx, fqdn)
	if err != nil {
		return err
	}
	rec := map[string]any{
		"type": "TXT",
		"name": digitaloceanName(fqdn, zone),
		"data": value,
		"ttl":  30,
	}
	return d.do(ctx, http.MethodPost, "/domains/"+zone+"/records", rec, nil)
}

// CleanUp lists records by the fully-qualified name (DigitalOcean's list
// filter requires the FQDN, unlike the name field on the record object
// itself, which is zone-relative) and deletes any TXT record whose data
// matches value.
func (d *DigitalOcean) CleanUp(ctx context.Context, fqdn, value string) error {
	zone, err := d.zone(ctx, fqdn)
	if err != nil {
		return err
	}
	fqdnNoDot := strings.TrimSuffix(fqdn, ".")

	var out struct {
		DomainRecords []struct {
			ID   int    `json:"id"`
			Type string `json:"type"`
			Data string `json:"data"`
		} `json:"domain_records"`
	}
	q := "/domains/" + zone + "/records?type=TXT&name=" + url.QueryEscape(fqdnNoDot) + "&per_page=200"
	if err := d.do(ctx, http.MethodGet, q, nil, &out); err != nil {
		return err
	}
	for _, r := range out.DomainRecords {
		if r.Type != "TXT" || r.Data != value {
			continue
		}
		id := strconv.Itoa(r.ID)
		if err := d.do(ctx, http.MethodDelete, "/domains/"+zone+"/records/"+id, nil, nil); err != nil {
			return err
		}
	}
	return nil
}
