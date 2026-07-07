// Package gpucloud rents GPU virtual machines from cloud marketplaces and
// wires them into the cluster as K3s agents. vast.ai is the first provider;
// the exported shapes (Offer, Instance, RentSpec) are provider-neutral so a
// Nebius client can slot in later without touching callers.
//
// ponytail: single concrete VastAI type, no Provider interface yet — add the
// interface when the second provider actually lands.
package gpucloud

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// DefaultVastBaseURL is vast.ai's public REST API root.
const DefaultVastBaseURL = "https://console.vast.ai/api/v0"

// VastAI is a minimal vast.ai REST client. All endpoints authenticate with
// `Authorization: Bearer <api key>`.
type VastAI struct {
	APIKey  string
	BaseURL string // DefaultVastBaseURL when ""
	HTTP    *http.Client
}

// Offer is one rentable machine from search: enough to pick by price and
// show the operator what they are about to pay for.
type Offer struct {
	ID          int64   `json:"id"`
	GPUName     string  `json:"gpu_name"`
	NumGPUs     int     `json:"num_gpus"`
	GPURamMB    float64 `json:"gpu_ram"`
	CPUCores    float64 `json:"cpu_cores"`
	DiskSpace   float64 `json:"disk_space"`
	DPHTotal    float64 `json:"dph_total"`
	Geolocation string  `json:"geolocation"`
	Reliability float64 `json:"reliability"`
}

// Instance is one rented contract as vast.ai reports it.
type Instance struct {
	ID       int64   `json:"id"`
	Label    string  `json:"label"`
	Status   string  `json:"actual_status"`
	GPUName  string  `json:"gpu_name"`
	NumGPUs  int     `json:"num_gpus"`
	DPHTotal float64 `json:"dph_total"`
}

// RentSpec configures one instance create (accepting an "ask" contract).
// Onstart runs at boot inside the VM — the caller injects the K3s agent
// join script there.
type RentSpec struct {
	OfferID int64
	Image   string
	DiskGB  int
	Label   string
	Onstart string
}

func (v *VastAI) base() string {
	if v.BaseURL != "" {
		return v.BaseURL
	}
	return DefaultVastBaseURL
}

func (v *VastAI) client() *http.Client {
	if v.HTTP != nil {
		return v.HTTP
	}
	return &http.Client{Timeout: 30 * time.Second}
}

// do sends one authenticated JSON request and decodes the response into out
// (skipped when out is nil). Non-2xx responses surface vast.ai's error body.
func (v *VastAI) do(ctx context.Context, method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, v.base()+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+v.APIKey)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := v.client().Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return err
	}
	if res.StatusCode < 200 || res.StatusCode > 299 {
		var e struct {
			Error string `json:"error"`
			Msg   string `json:"msg"`
		}
		_ = json.Unmarshal(raw, &e)
		if e.Msg != "" {
			return fmt.Errorf("vast.ai %s %s: %s (%s)", method, path, e.Msg, res.Status)
		}
		return fmt.Errorf("vast.ai %s %s: %s", method, path, res.Status)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(raw, out)
}

// op wraps one search-filter comparison, e.g. {"eq": true}.
type op map[string]any

// SearchOffers lists rentable VM-capable offers, cheapest first. gpuName ""
// matches any GPU; numGPUs 0 matches any count.
func (v *VastAI) SearchOffers(ctx context.Context, gpuName string, numGPUs, limit int) ([]Offer, error) {
	if limit <= 0 {
		limit = 10
	}
	q := map[string]any{
		"verified":    op{"eq": true},
		"rentable":    op{"eq": true},
		"rented":      op{"eq": false},
		"vms_enabled": op{"eq": true}, // K3s agent needs a real VM, not a container
		"order":       [][]string{{"dph_total", "asc"}},
		"limit":       limit,
	}
	if gpuName != "" {
		q["gpu_name"] = op{"eq": gpuName}
	}
	if numGPUs > 0 {
		q["num_gpus"] = op{"eq": numGPUs}
	}
	var out struct {
		Offers []Offer `json:"offers"`
	}
	if err := v.do(ctx, http.MethodPost, "/bundles/", q, &out); err != nil {
		return nil, err
	}
	return out.Offers, nil
}

// Rent accepts offer OfferID as a VM instance and returns the new contract
// (instance) id.
func (v *VastAI) Rent(ctx context.Context, spec RentSpec) (int64, error) {
	body := map[string]any{
		"image":   spec.Image,
		"disk":    spec.DiskGB,
		"label":   spec.Label,
		"vm":      true,
		"onstart": spec.Onstart,
	}
	var out struct {
		Success     bool   `json:"success"`
		NewContract int64  `json:"new_contract"`
		Error       string `json:"error"`
		Msg         string `json:"msg"`
	}
	if err := v.do(ctx, http.MethodPut, fmt.Sprintf("/asks/%d/", spec.OfferID), body, &out); err != nil {
		return 0, err
	}
	if !out.Success {
		return 0, fmt.Errorf("vast.ai rent: %s", firstNonEmpty(out.Msg, out.Error, "unknown error"))
	}
	return out.NewContract, nil
}

// List returns the account's rented instances.
func (v *VastAI) List(ctx context.Context) ([]Instance, error) {
	var out struct {
		Instances []Instance `json:"instances"`
	}
	if err := v.do(ctx, http.MethodGet, "/instances/", nil, &out); err != nil {
		return nil, err
	}
	return out.Instances, nil
}

// Destroy permanently deletes an instance (billing stops; data is gone).
func (v *VastAI) Destroy(ctx context.Context, id int64) error {
	var out struct {
		Success bool   `json:"success"`
		Error   string `json:"error"`
		Msg     string `json:"msg"`
	}
	if err := v.do(ctx, http.MethodDelete, fmt.Sprintf("/instances/%d/", id), nil, &out); err != nil {
		return err
	}
	if !out.Success {
		return fmt.Errorf("vast.ai destroy: %s", firstNonEmpty(out.Msg, out.Error, "unknown error"))
	}
	return nil
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}
