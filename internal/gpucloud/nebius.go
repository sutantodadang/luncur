package gpucloud

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ErrRentAmbiguous means a Nebius CreateInstance operation was accepted but
// did not reach done=true before the poll deadline. Callers should still
// record a pending/renting row: the instance may finish provisioning after
// this call returns, so silently dropping it would orphan real spend.
var ErrRentAmbiguous = errors.New("nebius rent outcome unknown (operation still pending)")

const nebiusDefaultPollInterval = 5 * time.Second
const nebiusDefaultPollTimeout = 3 * time.Minute

// Nebius is a minimal Nebius AI Cloud compute client. It authenticates every
// call with a Bearer IAM token obtained (and cached) via nebiusTokenSource.
type Nebius struct {
	Cfg  NebiusConfig
	HTTP *http.Client

	ts *nebiusTokenSource

	pollInterval time.Duration // unexported; tests shrink this, default 5s
	pollTimeout  time.Duration // unexported; tests shrink this, default 3m
}

var _ Provider = (*Nebius)(nil)

// NewNebius wires a Nebius client and its token source from one config.
func NewNebius(cfg NebiusConfig) *Nebius {
	return &Nebius{
		Cfg: cfg,
		ts:  &nebiusTokenSource{cfg: cfg},
	}
}

// Name identifies this provider in stored records and logs.
func (n *Nebius) Name() string { return "nebius" }

func (n *Nebius) client() *http.Client {
	if n.HTTP != nil {
		return n.HTTP
	}
	return &http.Client{Timeout: 30 * time.Second}
}

func (n *Nebius) interval() time.Duration {
	if n.pollInterval > 0 {
		return n.pollInterval
	}
	return nebiusDefaultPollInterval
}

func (n *Nebius) timeout() time.Duration {
	if n.pollTimeout > 0 {
		return n.pollTimeout
	}
	return nebiusDefaultPollTimeout
}

// token fetches a valid IAM access token, routing the token source through
// the same HTTP client as compute calls (matters for tests pointed at one
// httptest.Server).
func (n *Nebius) token(ctx context.Context) (string, error) {
	n.ts.http = n.client()
	return n.ts.Token(ctx)
}

// do sends one authenticated JSON request against the Nebius compute API and
// decodes the response into out (skipped when out is nil).
func (n *Nebius) do(ctx context.Context, method, path string, body, out any) error {
	tok, err := n.token(ctx)
	if err != nil {
		return err
	}

	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}

	req, err := http.NewRequestWithContext(ctx, method, n.Cfg.endpoint()+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	res, err := n.client().Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return err
	}
	if res.StatusCode < 200 || res.StatusCode > 299 {
		return fmt.Errorf("nebius %s %s: %s: %s", method, path, res.Status, string(raw))
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(raw, out)
}

// nebiusResources selects the GPU shape: a platform (hardware generation)
// plus a preset (GPU count/vCPU/RAM bundle within that platform).
type nebiusResources struct {
	Platform string `json:"platform"`
	Preset   string `json:"preset"`
}

type nebiusSourceImageFamily struct {
	ImageFamily string `json:"image_family"`
}

type nebiusBootDisk struct {
	SizeGibibytes     int                     `json:"size_gibibytes"`
	SourceImageFamily nebiusSourceImageFamily `json:"source_image_family"`
}

type nebiusNetworkInterface struct {
	SubnetID string `json:"subnet_id"`
}

// nebiusCreateInstanceRequest is the CreateInstance request body.
type nebiusCreateInstanceRequest struct {
	ParentID          string                   `json:"parent_id"`
	Name              string                   `json:"name"`
	Resources         nebiusResources          `json:"resources"`
	BootDisk          nebiusBootDisk           `json:"boot_disk"`
	NetworkInterfaces []nebiusNetworkInterface `json:"network_interfaces"`
	CloudInitUserData string                   `json:"cloud_init_user_data"`
}

// nebiusOperation is Nebius's long-running-operation envelope: CreateInstance
// returns one immediately with done=false, and the poll endpoint returns the
// same shape until done=true.
type nebiusOperation struct {
	ID         string `json:"id"`
	ResourceID string `json:"resource_id"`
	Done       bool   `json:"done"`
}

// cloudInit wraps a shell script (vast.ai's onstart convention) as a
// cloud-config runcmd block so the same join script runs on both providers.
func cloudInit(onstart string) string {
	return "#cloud-config\nruncmd:\n  - |\n" + indentLines(onstart, 4)
}

// indentLines prefixes every line of s with n spaces, preserving content
// verbatim so the wrapped script still runs correctly.
func indentLines(s string, n int) string {
	prefix := strings.Repeat(" ", n)
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = prefix + l
	}
	return strings.Join(lines, "\n")
}

// Rent creates a Nebius compute instance from spec.NebiusPlatform/Preset and
// polls the resulting operation until it completes, returning the new
// instance's id. If the operation is still pending at the poll deadline, it
// returns ErrRentAmbiguous so the caller can still record a pending row.
func (n *Nebius) Rent(ctx context.Context, spec RentSpec) (string, error) {
	body := nebiusCreateInstanceRequest{
		ParentID: n.Cfg.ParentID,
		Name:     spec.Label,
		Resources: nebiusResources{
			Platform: spec.NebiusPlatform,
			Preset:   spec.NebiusPreset,
		},
		BootDisk: nebiusBootDisk{
			SizeGibibytes: spec.DiskGB,
			SourceImageFamily: nebiusSourceImageFamily{
				ImageFamily: "ubuntu22.04-driverless",
			},
		},
		NetworkInterfaces: []nebiusNetworkInterface{{SubnetID: n.Cfg.SubnetID}},
		// VERIFY(nebius-smoke): CreateInstance JSON body shape
		CloudInitUserData: cloudInit(spec.Onstart),
	}

	var op nebiusOperation
	// VERIFY(nebius-smoke): CreateInstance REST path
	if err := n.do(ctx, http.MethodPost, "/compute/v1/instances", body, &op); err != nil {
		return "", err
	}

	return n.pollOperation(ctx, op.ID, spec.Label)
}

// pollOperation polls a Nebius operation until done=true or pollTimeout
// elapses, returning the operation's resource_id (the new instance's id).
func (n *Nebius) pollOperation(ctx context.Context, opID, label string) (string, error) {
	deadline := time.Now().Add(n.timeout())
	for {
		var op nebiusOperation
		// VERIFY(nebius-smoke): operation poll path
		// VERIFY(nebius-smoke): operation response shape
		if err := n.do(ctx, http.MethodGet, "/compute/v1/operations/"+opID, nil, &op); err != nil {
			return "", err
		}
		if op.Done {
			return op.ResourceID, nil
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("nebius rent %s: %w", label, ErrRentAmbiguous)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(n.interval()):
		}
	}
}

// List returns the account's instances under Cfg.ParentID. GPUName/NumGPUs
// are left zero-value: the preset already encodes the GPU shape and Nebius's
// list response doesn't break it back out per-GPU.
func (n *Nebius) List(ctx context.Context) ([]Instance, error) {
	var out struct {
		Items []struct {
			ID     string `json:"id"`
			Name   string `json:"name"`
			Status string `json:"status"`
		} `json:"items"`
	}
	// VERIFY(nebius-smoke): List instances path
	path := "/compute/v1/instances?parent_id=" + url.QueryEscape(n.Cfg.ParentID)
	if err := n.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	instances := make([]Instance, 0, len(out.Items))
	for _, it := range out.Items {
		instances = append(instances, Instance{
			Ref:    it.ID,
			Label:  it.Name,
			Status: it.Status,
		})
	}
	return instances, nil
}

// Destroy permanently deletes an instance (billing stops; data is gone).
func (n *Nebius) Destroy(ctx context.Context, ref string) error {
	// VERIFY(nebius-smoke): Destroy instance path
	return n.do(ctx, http.MethodDelete, "/compute/v1/instances/"+ref, nil, nil)
}
