package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// gpuCloudFake is a fake luncur server exposing just the GPU-cloud endpoints,
// capturing the raw decoded request bodies so tests can assert on the exact
// JSON shape the client sends.
type gpuCloudFake struct {
	srv       *httptest.Server
	keyBody   map[string]any
	rentBody  map[string]any
	instances []map[string]any
}

func newGPUCloudFake(t *testing.T) *gpuCloudFake {
	t.Helper()
	f := &gpuCloudFake{}
	mux := http.NewServeMux()
	mux.HandleFunc("PUT /v1/gpu/key", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		f.keyBody = body
		json.NewEncoder(w).Encode(map[string]any{"provider": body["provider"], "set": true})
	})
	mux.HandleFunc("POST /v1/gpu/instances", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		f.rentBody = body
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]any{
			"id": 1, "provider": body["provider"], "external_id": "ext-1",
			"label": "luncur-gpu-1", "gpu_name": body["gpu_name"], "num_gpus": 0,
			"status": "active", "created_at": "2026-01-01T00:00:00Z",
		})
	})
	mux.HandleFunc("GET /v1/gpu/instances", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(f.instances)
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func TestGPUKeyVastUnchanged(t *testing.T) {
	f := newGPUCloudFake(t)
	setCLIConfig(t, f.srv.URL)

	out, err := run(t, "gpu", "key", "vast-key-123")
	if err != nil {
		t.Fatalf("gpu key: %v (%s)", err, out)
	}
	if f.keyBody["provider"] != "vastai" || f.keyBody["api_key"] != "vast-key-123" {
		t.Fatalf("want vastai key body, got %v", f.keyBody)
	}
	if _, ok := f.keyBody["sa_id"]; ok {
		t.Fatalf("vast key request should not carry nebius fields, got %v", f.keyBody)
	}
	if !strings.Contains(out, "stored vastai API key") {
		t.Fatalf("want stored message, got %q", out)
	}
	if !strings.Contains(out, "$ luncur gpu key <api-key>") {
		t.Fatalf("want CLI-echo line, got %q", out)
	}
}

func TestGPUKeyNebius(t *testing.T) {
	f := newGPUCloudFake(t)
	setCLIConfig(t, f.srv.URL)

	pemPath := filepath.Join(t.TempDir(), "key.pem")
	pemContents := "-----BEGIN PRIVATE KEY-----\nSUPERSECRETPEMBYTES\n-----END PRIVATE KEY-----"
	if err := os.WriteFile(pemPath, []byte(pemContents), 0o600); err != nil {
		t.Fatal(err)
	}

	out, err := run(t, "gpu", "key", "--provider", "nebius",
		"--sa-id", "sa1", "--pubkey-id", "pub1",
		"--private-key-file", pemPath,
		"--parent-id", "parent1", "--subnet-id", "subnet1")
	if err != nil {
		t.Fatalf("gpu key nebius: %v (%s)", err, out)
	}

	want := map[string]string{
		"provider": "nebius", "sa_id": "sa1", "pubkey_id": "pub1",
		"private_key": pemContents, "parent_id": "parent1", "subnet_id": "subnet1",
	}
	for k, v := range want {
		if f.keyBody[k] != v {
			t.Fatalf("key body[%q] = %v, want %q (full body: %v)", k, f.keyBody[k], v, f.keyBody)
		}
	}

	if strings.Contains(out, "SUPERSECRETPEMBYTES") {
		t.Fatalf("CLI output must never echo the raw PEM contents, got %q", out)
	}
	wantEcho := "$ luncur gpu key --provider nebius --sa-id sa1 --pubkey-id pub1 --private-key-file " +
		pemPath + " --parent-id parent1 --subnet-id subnet1"
	if !strings.Contains(out, wantEcho) {
		t.Fatalf("want reconstructed CLI-echo command, got %q", out)
	}
}

func TestGPUKeyNebiusMissingFlags(t *testing.T) {
	f := newGPUCloudFake(t)
	setCLIConfig(t, f.srv.URL)

	_, err := run(t, "gpu", "key", "--provider", "nebius", "--sa-id", "sa1")
	if err == nil {
		t.Fatal("want error for missing nebius flags")
	}
	for _, want := range []string{"--pubkey-id", "--private-key-file", "--parent-id", "--subnet-id"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("want missing-flag error to name %q, got %v", want, err)
		}
	}
}

func TestGPURentVastUnchanged(t *testing.T) {
	f := newGPUCloudFake(t)
	setCLIConfig(t, f.srv.URL)

	out, err := run(t, "gpu", "rent", "42", "--disk", "60", "--gpu", "RTX 4090", "--count", "2")
	if err != nil {
		t.Fatalf("gpu rent: %v (%s)", err, out)
	}
	if f.rentBody["provider"] != "vastai" {
		t.Fatalf("want provider vastai, got %v", f.rentBody)
	}
	if f.rentBody["offer_id"] != float64(42) {
		t.Fatalf("want offer_id 42, got %v", f.rentBody["offer_id"])
	}
	if f.rentBody["disk_gb"] != float64(60) {
		t.Fatalf("want disk_gb 60, got %v", f.rentBody["disk_gb"])
	}
	if f.rentBody["platform"] != "" || f.rentBody["preset"] != "" {
		t.Fatalf("vast rent should not carry nebius fields, got %v", f.rentBody)
	}
	if !strings.Contains(out, "rented: luncur-gpu-1 (contract ext-1)") {
		t.Fatalf("want rent confirmation, got %q", out)
	}
	if !strings.Contains(out, "$ luncur gpu rent 42 --disk 60") {
		t.Fatalf("want CLI-echo line, got %q", out)
	}
}

func TestGPURentNebius(t *testing.T) {
	f := newGPUCloudFake(t)
	setCLIConfig(t, f.srv.URL)

	out, err := run(t, "gpu", "rent", "--provider", "nebius",
		"--platform", "gpu-h100-sxm", "--preset", "1gpu-16vcpu-200gb", "--disk", "80")
	if err != nil {
		t.Fatalf("gpu rent nebius: %v (%s)", err, out)
	}
	if f.rentBody["provider"] != "nebius" {
		t.Fatalf("want provider nebius, got %v", f.rentBody)
	}
	if f.rentBody["platform"] != "gpu-h100-sxm" || f.rentBody["preset"] != "1gpu-16vcpu-200gb" {
		t.Fatalf("want platform/preset, got %v", f.rentBody)
	}
	if f.rentBody["disk_gb"] != float64(80) {
		t.Fatalf("want disk_gb 80, got %v", f.rentBody["disk_gb"])
	}
	if !strings.Contains(out, "$ luncur gpu rent --provider nebius --platform gpu-h100-sxm --preset 1gpu-16vcpu-200gb") {
		t.Fatalf("want CLI-echo line, got %q", out)
	}
}

func TestGPURentNebiusRequiresPlatformPreset(t *testing.T) {
	f := newGPUCloudFake(t)
	setCLIConfig(t, f.srv.URL)

	if _, err := run(t, "gpu", "rent", "--provider", "nebius"); err == nil {
		t.Fatal("want error when platform/preset are missing")
	}
}

func TestGPULsShowsProviderColumn(t *testing.T) {
	f := newGPUCloudFake(t)
	f.instances = []map[string]any{
		{"id": 1, "provider": "vastai", "external_id": "123", "label": "l1", "gpu_name": "RTX 4090",
			"num_gpus": 1, "status": "active", "provider_status": "running", "dph_total": 0.5},
		{"id": 2, "provider": "nebius", "external_id": "computeinstance-abc", "label": "l2", "gpu_name": "H100",
			"num_gpus": 1, "status": "active", "provider_status": "RUNNING", "dph_total": 2.1},
	}
	setCLIConfig(t, f.srv.URL)

	out, err := run(t, "gpu", "ls")
	if err != nil {
		t.Fatalf("gpu ls: %v (%s)", err, out)
	}
	for _, want := range []string{"PROVIDER", "vastai", "nebius", "l1", "l2"} {
		if !strings.Contains(out, want) {
			t.Fatalf("want %q in gpu ls output, got %q", want, out)
		}
	}
}
