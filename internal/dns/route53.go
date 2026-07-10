package dns

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/sutantodadang/luncur/internal/awssig"
)

// Route53 manages TXT records via ChangeResourceRecordSets, signed with
// the shared SigV4 signer (XML request/response, stdlib only).
type Route53 struct {
	AccessKey  string
	SecretKey  string
	Region     string // signing region, default us-east-1 (Route53 is global)
	BaseURL    string // default https://route53.amazonaws.com
	HTTPClient *http.Client
	Now        func() time.Time
}

func (r *Route53) base() string {
	if r.BaseURL != "" {
		return strings.TrimRight(r.BaseURL, "/")
	}
	return "https://route53.amazonaws.com"
}

func (r *Route53) region() string {
	if r.Region != "" {
		return r.Region
	}
	return "us-east-1"
}

func (r *Route53) client() *http.Client {
	if r.HTTPClient != nil {
		return r.HTTPClient
	}
	return http.DefaultClient
}

func (r *Route53) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *Route53) do(ctx context.Context, method, path string, body []byte, out any) error {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, r.base()+path, rd)
	if err != nil {
		return err
	}
	awssig.Sign(req, r.AccessKey, r.SecretKey, r.region(), "route53", awssig.HashPayload(body), r.now().UTC())
	resp, err := r.client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		return fmt.Errorf("route53 %s %s: %d %s", method, path, resp.StatusCode, truncate(raw))
	}
	if out != nil {
		return xml.Unmarshal(raw, out)
	}
	return nil
}

// zoneID resolves the hosted zone containing fqdn by longest-suffix match
// via ListHostedZonesByName.
func (r *Route53) zoneID(ctx context.Context, fqdn string) (string, error) {
	labels := strings.Split(strings.TrimSuffix(fqdn, "."), ".")
	for i := 0; i <= len(labels)-2; i++ {
		cand := strings.Join(labels[i:], ".")
		var out struct {
			HostedZones struct {
				HostedZone []struct {
					ID   string `xml:"Id"`
					Name string `xml:"Name"`
				} `xml:"HostedZone"`
			} `xml:"HostedZones"`
		}
		path := "/2013-04-01/hostedzone?dnsname=" + url.QueryEscape(cand) + "&maxitems=1"
		if err := r.do(ctx, http.MethodGet, path, nil, &out); err != nil {
			return "", err
		}
		for _, z := range out.HostedZones.HostedZone {
			if strings.TrimSuffix(z.Name, ".") == cand {
				return strings.TrimPrefix(z.ID, "/hostedzone/"), nil
			}
		}
	}
	return "", fmt.Errorf("route53: no hosted zone found for %s", fqdn)
}

type r53Record struct {
	Value string `xml:"Value"`
}

type r53RRSet struct {
	Name    string      `xml:"Name"`
	Type    string      `xml:"Type"`
	TTL     int         `xml:"TTL"`
	Records []r53Record `xml:"ResourceRecords>ResourceRecord"`
}

type r53ChangeReq struct {
	XMLName xml.Name         `xml:"https://route53.amazonaws.com/doc/2013-04-01/ ChangeResourceRecordSetsRequest"`
	Changes []r53ChangeEntry `xml:"ChangeBatch>Changes>Change"`
}

type r53ChangeEntry struct {
	Action string   `xml:"Action"`
	RRSet  r53RRSet `xml:"ResourceRecordSet"`
}

func (r *Route53) change(ctx context.Context, action, fqdn, value string) error {
	zone, err := r.zoneID(ctx, fqdn)
	if err != nil {
		return err
	}
	req := r53ChangeReq{Changes: []r53ChangeEntry{{
		Action: action,
		RRSet: r53RRSet{
			Name:    strings.TrimSuffix(fqdn, ".") + ".",
			Type:    "TXT",
			TTL:     60,
			Records: []r53Record{{Value: `"` + value + `"`}},
		},
	}}}
	body, err := xml.Marshal(req)
	if err != nil {
		return err
	}
	return r.do(ctx, http.MethodPost, "/2013-04-01/hostedzone/"+zone+"/rrset", body, nil)
}

func (r *Route53) Present(ctx context.Context, fqdn, value string) error {
	return r.change(ctx, "UPSERT", fqdn, value)
}

func (r *Route53) CleanUp(ctx context.Context, fqdn, value string) error {
	return r.change(ctx, "DELETE", fqdn, value)
}
