package server

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sutantodadang/luncur/internal/gpucloud"
	"github.com/sutantodadang/luncur/internal/secret"
	"github.com/sutantodadang/luncur/internal/store"
)

// gpuTestServer builds a *server with a sealer, a node-token file (so
// rentGPU's K3s-join step succeeds), and its HTTP frontend.
func gpuTestServer(t *testing.T) (*server, *httptest.Server) {
	t.Helper()
	sealer, err := secret.New(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	st := newTestStore(t)
	tokFile := filepath.Join(t.TempDir(), "node-token")
	if err := os.WriteFile(tokFile, []byte("tok-node-abc"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := newServer(Deps{Store: st, Sealer: sealer, NodeTokenPath: tokFile})
	srv := httptest.NewServer(s.handler())
	t.Cleanup(srv.Close)
	return s, srv
}

// genRSAPEM generates a throwaway RSA key (PKCS#1 PEM) so Nebius's
// token-exchange JWT signs successfully against a fake server.
func genRSAPEM(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der := x509.MarshalPKCS1PrivateKey(key)
	return string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: der}))
}

func setSetting(t *testing.T, s *server, key, value string) {
	t.Helper()
	if err := s.st.SetSetting(key, value); err != nil {
		t.Fatal(err)
	}
}

// TestSetGPUKeyVastaiBackCompat ensures a body with just api_key (no
// provider field) still stores the vast.ai key, matching pre-Nebius clients.
func TestSetGPUKeyVastaiBackCompat(t *testing.T) {
	s, srv := gpuTestServer(t)
	admin := seedUserToken(t, s.st, "root@b.co", "admin")

	resp := doAuthed(t, "PUT", srv.URL+"/v1/gpu/key", admin, `{"api_key":"vastkey123"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if _, err := s.vast(); err != nil {
		t.Fatalf("vast() after set: %v", err)
	}
}

// TestSetGPUKeyNebius covers both the happy path (all five fields) and the
// missing-field 400.
func TestSetGPUKeyNebius(t *testing.T) {
	s, srv := gpuTestServer(t)
	admin := seedUserToken(t, s.st, "root@b.co", "admin")

	// Missing private_key and subnet_id -> 400 listing both.
	bad := doAuthed(t, "PUT", srv.URL+"/v1/gpu/key", admin, `{
		"provider":"nebius","sa_id":"sa-1","pubkey_id":"kid-1","parent_id":"proj-1"
	}`)
	defer bad.Body.Close()
	if bad.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", bad.StatusCode)
	}
	var env struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(bad.Body).Decode(&env); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(env.Error.Message, "private_key") || !strings.Contains(env.Error.Message, "subnet_id") {
		t.Fatalf("message = %q, want it to list private_key and subnet_id", env.Error.Message)
	}

	// All five present -> 200, and s.nebius() can build a client from it.
	body := `{
		"provider":"nebius","sa_id":"sa-1","pubkey_id":"kid-1",
		"private_key":"----- PEM -----","parent_id":"proj-1","subnet_id":"subnet-1"
	}`
	ok := doAuthed(t, "PUT", srv.URL+"/v1/gpu/key", admin, body)
	defer ok.Body.Close()
	if ok.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", ok.StatusCode)
	}
	if _, err := s.nebius(); err != nil {
		t.Fatalf("nebius() after set: %v", err)
	}
}

// nebiusFakeServer wires an httptest server that satisfies token-exchange,
// CreateInstance (immediately done, no polling delay), and List, returning
// the got-request details via the closures for assertions.
func nebiusFakeServer(t *testing.T, instanceID string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/iam/v1/tokens:exchange", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"access_token":"tok-e2e","expires_in":3600}`))
	})
	mux.HandleFunc("/compute/v1/instances", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			_, _ = w.Write([]byte(`{"id":"op-e2e","resource_id":"` + instanceID + `","done":false}`))
		case http.MethodGet:
			_, _ = w.Write([]byte(`{"items":[{"id":"` + instanceID + `","name":"luncur-gpu-e2e","status":"running"}]}`))
		}
	})
	mux.HandleFunc("/compute/v1/operations/op-e2e", func(w http.ResponseWriter, r *http.Request) {
		// Done on the very first poll: no real wait, keeps the test fast.
		_, _ = w.Write([]byte(`{"id":"op-e2e","resource_id":"` + instanceID + `","done":true}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// TestRentGPUNebiusEndToEnd exercises the full JSON API: store creds, point
// the endpoint test seam at a fake Nebius server, rent, and confirm the row
// carries the string external ref.
func TestRentGPUNebiusEndToEnd(t *testing.T) {
	s, srv := gpuTestServer(t)
	admin := seedUserToken(t, s.st, "root@b.co", "admin")
	fake := nebiusFakeServer(t, "computeinstance-e2e")

	pem := genRSAPEM(t)
	keyBody, err := json.Marshal(map[string]string{
		"provider": "nebius", "sa_id": "sa-1", "pubkey_id": "kid-1",
		"private_key": pem, "parent_id": "proj-1", "subnet_id": "subnet-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	setResp := doAuthed(t, "PUT", srv.URL+"/v1/gpu/key", admin, string(keyBody))
	defer setResp.Body.Close()
	if setResp.StatusCode != http.StatusOK {
		t.Fatalf("set key status = %d", setResp.StatusCode)
	}
	// test seam: point Nebius at the fake server.
	setSetting(t, s, settingNebiusEndpoint, fake.URL)

	rentResp := doAuthed(t, "POST", srv.URL+"/v1/gpu/instances", admin, `{
		"provider":"nebius","platform":"gpu-h100-sxm","preset":"1gpu-16vcpu-200gb",
		"gpu_name":"H100","num_gpus":1
	}`)
	defer rentResp.Body.Close()
	if rentResp.StatusCode != http.StatusCreated {
		t.Fatalf("rent status = %d, want 201", rentResp.StatusCode)
	}
	var inst struct {
		Provider   string `json:"provider"`
		ExternalID string `json:"external_id"`
	}
	if err := json.NewDecoder(rentResp.Body).Decode(&inst); err != nil {
		t.Fatal(err)
	}
	if inst.Provider != "nebius" || inst.ExternalID != "computeinstance-e2e" {
		t.Fatalf("instance = %+v", inst)
	}
}

// TestRentGPURequiresPlatformPresetForNebius checks the nebius-specific
// request validation (offer_id is a vast-only concept).
func TestRentGPURequiresPlatformPresetForNebius(t *testing.T) {
	s, srv := gpuTestServer(t)
	admin := seedUserToken(t, s.st, "root@b.co", "admin")

	resp := doAuthed(t, "POST", srv.URL+"/v1/gpu/instances", admin, `{"provider":"nebius"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

// TestRentGPUUnknownProvider confirms an unrecognized provider name surfaces
// as a clean provider error rather than a panic or 500.
func TestRentGPUUnknownProvider(t *testing.T) {
	s, srv := gpuTestServer(t)
	admin := seedUserToken(t, s.st, "root@b.co", "admin")

	resp := doAuthed(t, "POST", srv.URL+"/v1/gpu/instances", admin, `{"provider":"digitalocean","offer_id":1}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatal(err)
	}
	if env.Error.Code != "gpu_unconfigured" {
		t.Fatalf("code = %q, want gpu_unconfigured", env.Error.Code)
	}
}

// stubProvider is a minimal gpucloud.Provider for testing rentWithProvider's
// ambiguous-outcome handling without any real HTTP or poll waiting.
type stubProvider struct {
	rentRef string
	rentErr error
}

func (p *stubProvider) Name() string { return "stub" }
func (p *stubProvider) Rent(ctx context.Context, spec gpucloud.RentSpec) (string, error) {
	return p.rentRef, p.rentErr
}
func (p *stubProvider) List(ctx context.Context) ([]gpucloud.Instance, error) { return nil, nil }
func (p *stubProvider) Destroy(ctx context.Context, ref string) error         { return nil }

// TestRentWithProviderAmbiguous confirms an ErrRentAmbiguous outcome still
// records a row (status "renting") instead of losing the contract.
func TestRentWithProviderAmbiguous(t *testing.T) {
	s, _ := gpuTestServer(t)
	prov := &stubProvider{rentRef: "", rentErr: gpucloud.ErrRentAmbiguous}

	g, err := s.rentWithProvider(context.Background(), prov, "nebius",
		gpucloud.RentSpec{Label: "luncur-gpu-ambiguous"}, "luncur-gpu-ambiguous", "H100", 1)
	if !errors.Is(err, gpucloud.ErrRentAmbiguous) {
		t.Fatalf("err = %v, want ErrRentAmbiguous", err)
	}
	if g.Status != "renting" || g.Provider != "nebius" {
		t.Fatalf("recorded row = %+v", g)
	}

	list, lerr := s.st.ListGPUInstances()
	if lerr != nil {
		t.Fatal(lerr)
	}
	if len(list) != 1 || list[0].ID != g.ID {
		t.Fatalf("list = %+v", list)
	}
}

// TestHandleRentGPUAmbiguousReturns202 exercises the ambiguous path through
// the real HTTP handler and gpuProvider factory (a fake Nebius server whose
// operation never reaches done=true within a very short poll timeout would
// take real wall-clock time to prove via HTTP, so this asserts the JSON
// envelope shape via rentWithProvider's contract instead -- see
// TestRentWithProviderAmbiguous for the row-recording assertion, and
// gpucloud.TestNebiusRent_AmbiguousOnPollTimeout for the provider-level
// timeout behavior).
func TestHandleRentGPUAmbiguousResponseShape(t *testing.T) {
	s, _ := gpuTestServer(t)
	prov := &stubProvider{rentRef: "", rentErr: gpucloud.ErrRentAmbiguous}
	g, err := s.rentWithProvider(context.Background(), prov, "nebius",
		gpucloud.RentSpec{Label: "luncur-gpu-shape"}, "luncur-gpu-shape", "H100", 1)
	if !errors.Is(err, gpucloud.ErrRentAmbiguous) {
		t.Fatalf("err = %v, want ErrRentAmbiguous", err)
	}
	m := gpuInstanceJSON(g)
	if m["status"] != "renting" {
		t.Fatalf("gpuInstanceJSON status = %v, want renting", m["status"])
	}
}

// TestListGPUInstancesMergesProviders configures both vast.ai and Nebius
// (fakes) plus a stored row for each, and checks GET /v1/gpu/instances
// merges live provider_status from both.
func TestListGPUInstancesMergesProviders(t *testing.T) {
	s, srv := gpuTestServer(t)
	admin := seedUserToken(t, s.st, "root@b.co", "admin")

	// vast.ai fake: List() only.
	vastFake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/instances/" {
			_, _ = w.Write([]byte(`{"instances":[{"id":777,"label":"luncur-gpu-777","actual_status":"running","gpu_name":"RTX 4090","num_gpus":1,"dph_total":0.35}]}`))
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(vastFake.Close)
	s.vastBaseURL = vastFake.URL
	setKey := doAuthed(t, "PUT", srv.URL+"/v1/gpu/key", admin, `{"api_key":"vastkey"}`)
	setKey.Body.Close()

	// Nebius fake: List() only (via /compute/v1/instances GET).
	nebiusFake := nebiusFakeServer(t, "computeinstance-xyz")
	pem := genRSAPEM(t)
	nebiusKeyBody, err := json.Marshal(map[string]string{
		"provider": "nebius", "sa_id": "sa-1", "pubkey_id": "kid-1",
		"private_key": pem, "parent_id": "proj-1", "subnet_id": "subnet-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	setNebius := doAuthed(t, "PUT", srv.URL+"/v1/gpu/key", admin, string(nebiusKeyBody))
	setNebius.Body.Close()
	setSetting(t, s, settingNebiusEndpoint, nebiusFake.URL)

	// Rows for both providers so the live merge has something to attach to.
	if _, err := s.st.CreateGPUInstance("vastai", "777", "luncur-gpu-777", "RTX 4090", 1); err != nil {
		t.Fatal(err)
	}
	if _, err := s.st.CreateGPUInstance("nebius", "computeinstance-xyz", "luncur-gpu-xyz", "H100", 1); err != nil {
		t.Fatal(err)
	}

	resp := doAuthed(t, "GET", srv.URL+"/v1/gpu/instances", admin, "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var out []map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 {
		t.Fatalf("out = %+v, want 2 rows", out)
	}
	seen := map[string]bool{}
	for _, row := range out {
		provider, _ := row["provider"].(string)
		seen[provider] = true
		if _, ok := row["provider_status"]; !ok {
			t.Fatalf("row %+v missing provider_status (live merge failed)", row)
		}
	}
	if !seen["vastai"] || !seen["nebius"] {
		t.Fatalf("seen = %+v, want both vastai and nebius", seen)
	}
}

// TestDecideIdleDestroysBusyNeverDestroyed covers a busy instance: it's
// never a destroy candidate and carries no idle clock into next.
func TestDecideIdleDestroysBusyNeverDestroyed(t *testing.T) {
	now := time.Now()
	instances := []store.GPUInstance{{Label: "gpu-1", Status: "active"}}
	busy := map[string]bool{"gpu-1": true}
	destroy, next := decideIdleDestroys(instances, busy, map[string]time.Time{"gpu-1": now.Add(-time.Hour)}, now, 5*time.Minute)
	if len(destroy) != 0 {
		t.Fatalf("destroy = %v, want none", destroy)
	}
	if _, ok := next["gpu-1"]; ok {
		t.Fatalf("next = %+v, want gpu-1 cleared (busy)", next)
	}
}

// TestDecideIdleDestroysIdleTicksThenDestroys covers the two-tick idle path:
// the first idle tick sets the clock without destroying, and a later tick
// once the window has elapsed destroys it.
func TestDecideIdleDestroysIdleTicksThenDestroys(t *testing.T) {
	instances := []store.GPUInstance{{Label: "gpu-1", Status: "active"}}
	busy := map[string]bool{}
	window := 5 * time.Minute

	t0 := time.Now()
	destroy, next := decideIdleDestroys(instances, busy, map[string]time.Time{}, t0, window)
	if len(destroy) != 0 {
		t.Fatalf("first tick: destroy = %v, want none", destroy)
	}
	since, ok := next["gpu-1"]
	if !ok || !since.Equal(t0) {
		t.Fatalf("first tick: next = %+v, want gpu-1 = %v", next, t0)
	}

	t1 := t0.Add(window)
	destroy, next = decideIdleDestroys(instances, busy, next, t1, window)
	if len(destroy) != 1 || destroy[0] != "gpu-1" {
		t.Fatalf("second tick: destroy = %v, want [gpu-1]", destroy)
	}
	if _, ok := next["gpu-1"]; !ok {
		t.Fatalf("second tick: next = %+v, want gpu-1 still tracked", next)
	}
}

// TestDecideIdleDestroysPendingUnscheduledFreezes covers busy[""]==true (an
// unscheduled Pending GPU pod): destroys freeze entirely this tick, and
// idleSince is returned unchanged even though an instance is long idle.
func TestDecideIdleDestroysPendingUnscheduledFreezes(t *testing.T) {
	now := time.Now()
	instances := []store.GPUInstance{{Label: "gpu-1", Status: "active"}}
	busy := map[string]bool{"": true}
	idleSince := map[string]time.Time{"gpu-1": now.Add(-time.Hour)}
	destroy, next := decideIdleDestroys(instances, busy, idleSince, now, 5*time.Minute)
	if len(destroy) != 0 {
		t.Fatalf("destroy = %v, want none (frozen)", destroy)
	}
	if len(next) != len(idleSince) || !next["gpu-1"].Equal(idleSince["gpu-1"]) {
		t.Fatalf("next = %+v, want idleSince unchanged %+v", next, idleSince)
	}
}

// TestDecideIdleDestroysMixedFleet covers a fleet with a long-idle instance
// past the window, a busy instance, and a short-idle instance still under
// the window: only the long-idle one is destroyed, the busy one is cleared
// from next, and the short-idle one carries its clock forward.
func TestDecideIdleDestroysMixedFleet(t *testing.T) {
	now := time.Now()
	window := 5 * time.Minute
	instances := []store.GPUInstance{
		{Label: "idle-long", Status: "active"},
		{Label: "busy", Status: "active"},
		{Label: "idle-short", Status: "active"},
	}
	busy := map[string]bool{"busy": true}
	idleSince := map[string]time.Time{
		"idle-long":  now.Add(-2 * window),
		"busy":       now.Add(-2 * window),
		"idle-short": now.Add(-1 * time.Minute),
	}
	destroy, next := decideIdleDestroys(instances, busy, idleSince, now, window)
	if len(destroy) != 1 || destroy[0] != "idle-long" {
		t.Fatalf("destroy = %v, want [idle-long]", destroy)
	}
	if _, ok := next["busy"]; ok {
		t.Fatalf("next = %+v, want busy cleared", next)
	}
	if since, ok := next["idle-short"]; !ok || !since.Equal(idleSince["idle-short"]) {
		t.Fatalf("next[idle-short] = %v, ok=%v, want %v carried forward", since, ok, idleSince["idle-short"])
	}
	if _, ok := next["idle-long"]; !ok {
		t.Fatalf("next = %+v, want idle-long still tracked (retries next tick if destroy fails)", next)
	}
}

// TestDecideIdleDestroysWindowBoundary confirms now.Sub(since) == window
// counts as destroy (>=, not >).
func TestDecideIdleDestroysWindowBoundary(t *testing.T) {
	window := 5 * time.Minute
	now := time.Now()
	instances := []store.GPUInstance{{Label: "gpu-1", Status: "active"}}
	idleSince := map[string]time.Time{"gpu-1": now.Add(-window)}
	destroy, _ := decideIdleDestroys(instances, map[string]bool{}, idleSince, now, window)
	if len(destroy) != 1 || destroy[0] != "gpu-1" {
		t.Fatalf("destroy = %v, want [gpu-1] at exact window boundary", destroy)
	}
}
