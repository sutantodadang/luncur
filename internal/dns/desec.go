package dns

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// DeSEC manages TXT records via the deSEC DNS REST API
// (https://desec.readthedocs.io/en/latest/dns/rrsets.html), authenticated
// with a token in the Authorization header. deSEC enforces a 3600s (1h)
// minimum TTL, so DNS-01 propagation waits are longer here than with the
// other providers (which default to 60s) — that's a deSEC constraint, not a
// bug. TXT record contents must carry literal surrounding double quotes in
// the records array (DNS TXT presentation format); deSEC does not add them.
type DeSEC struct {
	Token      string
	BaseURL    string // default https://desec.io/api/v1
	HTTPClient *http.Client
}

// desecTTL is deSEC's minimum allowed TTL.
const desecTTL = 3600

// desecApexPath is the URL path placeholder for the zone apex — deSEC's
// per-rrset endpoints can't have an empty path segment. The "subname" field
// in request/response bodies uses "" for the apex instead.
const desecApexPath = "@"

// errDeSECNotFound signals a 404 from a singular-resource lookup (domain or
// rrset), distinct from other errors so callers can treat "doesn't exist
// yet" as a normal case instead of a hard failure.
var errDeSECNotFound = errors.New("desec: not found")

func (d *DeSEC) base() string {
	if d.BaseURL != "" {
		return strings.TrimRight(d.BaseURL, "/")
	}
	return "https://desec.io/api/v1"
}

func (d *DeSEC) client() *http.Client {
	if d.HTTPClient != nil {
		return d.HTTPClient
	}
	return http.DefaultClient
}

// do performs one authenticated API call and decodes the response body (a
// bare object/array, not an envelope like Cloudflare's) into result (may be
// nil).
func (d *DeSEC) do(ctx context.Context, method, path string, body, result any) error {
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
	req.Header.Set("Authorization", "Token "+d.Token)
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
		return errDeSECNotFound
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("desec %s %s: %d %s", method, path, resp.StatusCode, truncate(raw))
	}
	if result != nil && len(raw) > 0 {
		return json.Unmarshal(raw, result)
	}
	return nil
}

// zone resolves the registered domain containing fqdn by longest-suffix
// match: walk the label suffixes from longest to shortest and take the
// first one the API confirms as a registered domain (GET /domains/{name}/
// is a singular-resource lookup, so a 404 means "not this one").
func (d *DeSEC) zone(ctx context.Context, fqdn string) (string, error) {
	labels := strings.Split(strings.TrimSuffix(fqdn, "."), ".")
	for i := 0; i <= len(labels)-2; i++ {
		cand := strings.Join(labels[i:], ".")
		var dom struct {
			Name string `json:"name"`
		}
		err := d.do(ctx, http.MethodGet, "/domains/"+cand+"/", nil, &dom)
		if err == nil {
			return dom.Name, nil
		}
		if !errors.Is(err, errDeSECNotFound) {
			return "", err
		}
	}
	return "", fmt.Errorf("desec: no zone found for %s", fqdn)
}

// desecSubname returns the rrset subname for request/response bodies ("" at
// the apex) and the URL path segment to address that single rrset ("@" at
// the apex, since deSEC's REST paths can't have an empty segment). The apex
// is fqdn == zone, or a trimmed name that comes out empty.
func desecSubname(fqdn, zone string) (subname, urlSubname string) {
	fqdn = strings.TrimSuffix(fqdn, ".")
	if fqdn == zone {
		return "", desecApexPath
	}
	subname = strings.TrimSuffix(fqdn, "."+zone)
	if subname == "" {
		return "", desecApexPath
	}
	return subname, subname
}

// desecRRSet is the bulk rrsets request/response shape: PUT-ing an array of
// these to /domains/{zone}/rrsets/ upserts (or, with an empty Records slice,
// deletes) each named rrset.
type desecRRSet struct {
	SubName string   `json:"subname"`
	Type    string   `json:"type"`
	Records []string `json:"records"`
	TTL     int      `json:"ttl,omitempty"`
}

func (d *DeSEC) Present(ctx context.Context, fqdn, value string) error {
	zone, err := d.zone(ctx, fqdn)
	if err != nil {
		return err
	}
	subname, urlSubname := desecSubname(fqdn, zone)
	quoted := fmt.Sprintf("%q", value)

	var existing desecRRSet
	err = d.do(ctx, http.MethodGet, "/domains/"+zone+"/rrsets/"+urlSubname+"/TXT/", nil, &existing)
	if err != nil && !errors.Is(err, errDeSECNotFound) {
		return err
	}
	records := append(existing.Records, quoted)

	body := []desecRRSet{{SubName: subname, Type: "TXT", Records: records, TTL: desecTTL}}
	return d.do(ctx, http.MethodPut, "/domains/"+zone+"/rrsets/", body, nil)
}

func (d *DeSEC) CleanUp(ctx context.Context, fqdn, value string) error {
	zone, err := d.zone(ctx, fqdn)
	if err != nil {
		return err
	}
	subname, urlSubname := desecSubname(fqdn, zone)
	quoted := fmt.Sprintf("%q", value)

	var existing desecRRSet
	err = d.do(ctx, http.MethodGet, "/domains/"+zone+"/rrsets/"+urlSubname+"/TXT/", nil, &existing)
	if errors.Is(err, errDeSECNotFound) {
		return nil
	}
	if err != nil {
		return err
	}

	records := make([]string, 0, len(existing.Records))
	for _, r := range existing.Records {
		if r != quoted {
			records = append(records, r)
		}
	}

	body := []desecRRSet{{SubName: subname, Type: "TXT", Records: records, TTL: desecTTL}}
	return d.do(ctx, http.MethodPut, "/domains/"+zone+"/rrsets/", body, nil)
}
