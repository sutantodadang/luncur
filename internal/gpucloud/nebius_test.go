package gpucloud

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// newTestNebiusConfig builds a NebiusConfig with a throwaway RSA key so the
// token-exchange JWT signs successfully against the test server.
func newTestNebiusConfig(t *testing.T, endpoint string) NebiusConfig {
	t.Helper()
	key := genRSAKey(t)
	return NebiusConfig{
		ServiceAccountID: "sa-1",
		PublicKeyID:      "kid-1",
		PrivateKeyPEM:    pkcs1PEM(key),
		ParentID:         "project-1",
		SubnetID:         "subnet-1",
		Endpoint:         endpoint,
	}
}

func TestNebiusFlow(t *testing.T) {
	var opPolls int
	var gotCreateAuth, gotListAuth, gotDestroyAuth string
	var gotCreateBody string

	mux := http.NewServeMux()
	mux.HandleFunc("/iam/v1/tokens:exchange", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"access_token":"tok-abc","expires_in":3600}`))
	})
	mux.HandleFunc("/compute/v1/instances", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			gotCreateAuth = r.Header.Get("Authorization")
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			b, _ := json.Marshal(body)
			gotCreateBody = string(b)
			_, _ = w.Write([]byte(`{"id":"op-1","resource_id":"computeinstance-xyz","done":false}`))
		case http.MethodGet:
			gotListAuth = r.Header.Get("Authorization")
			if r.URL.Query().Get("parent_id") != "project-1" {
				t.Errorf("list parent_id = %q, want project-1", r.URL.Query().Get("parent_id"))
			}
			_, _ = w.Write([]byte(`{"items":[{"id":"computeinstance-xyz","name":"luncur-gpu-1","status":"running","resources":{"platform":"gpu-h100-sxm","preset":"1gpu-16vcpu-200gb"}}]}`))
		default:
			http.Error(w, "unexpected method", http.StatusMethodNotAllowed)
		}
	})
	mux.HandleFunc("/compute/v1/operations/op-1", func(w http.ResponseWriter, r *http.Request) {
		opPolls++
		if opPolls == 1 {
			_, _ = w.Write([]byte(`{"id":"op-1","resource_id":"computeinstance-xyz","done":false}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"op-1","resource_id":"computeinstance-xyz","done":true}`))
	})
	mux.HandleFunc("/compute/v1/instances/computeinstance-xyz", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			http.Error(w, "unexpected method", http.StatusMethodNotAllowed)
			return
		}
		gotDestroyAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	n := NewNebius(newTestNebiusConfig(t, srv.URL))
	n.pollInterval = time.Millisecond

	ctx := context.Background()

	ref, err := n.Rent(ctx, RentSpec{
		Label:          "luncur-gpu-1",
		DiskGB:         100,
		Onstart:        "#!/bin/sh\necho join-cluster",
		NebiusPlatform: "gpu-h100-sxm",
		NebiusPreset:   "1gpu-16vcpu-200gb",
	})
	if err != nil {
		t.Fatalf("Rent: %v", err)
	}
	if ref != "computeinstance-xyz" {
		t.Fatalf("ref = %q, want computeinstance-xyz", ref)
	}
	if gotCreateAuth != "Bearer tok-abc" {
		t.Fatalf("create auth = %q, want Bearer tok-abc", gotCreateAuth)
	}
	if !strings.Contains(gotCreateBody, "echo join-cluster") {
		t.Fatalf("create body missing onstart script verbatim: %s", gotCreateBody)
	}
	if opPolls < 2 {
		t.Fatalf("opPolls = %d, want at least 2 (running then done)", opPolls)
	}

	list, err := n.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 || list[0].Ref != "computeinstance-xyz" || list[0].Label != "luncur-gpu-1" || list[0].Status != "running" {
		t.Fatalf("list = %+v", list)
	}
	if gotListAuth != "Bearer tok-abc" {
		t.Fatalf("list auth = %q, want Bearer tok-abc", gotListAuth)
	}

	if err := n.Destroy(ctx, "computeinstance-xyz"); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if gotDestroyAuth != "Bearer tok-abc" {
		t.Fatalf("destroy auth = %q, want Bearer tok-abc", gotDestroyAuth)
	}
}

func TestNebiusRent_AmbiguousOnPollTimeout(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/iam/v1/tokens:exchange", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"access_token":"tok-abc","expires_in":3600}`))
	})
	mux.HandleFunc("/compute/v1/instances", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"op-1","resource_id":"computeinstance-pending","done":false}`))
	})
	mux.HandleFunc("/compute/v1/operations/op-1", func(w http.ResponseWriter, r *http.Request) {
		// Never finishes.
		_, _ = w.Write([]byte(`{"id":"op-1","resource_id":"computeinstance-pending","done":false}`))
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	n := NewNebius(newTestNebiusConfig(t, srv.URL))
	n.pollInterval = time.Millisecond
	n.pollTimeout = 10 * time.Millisecond

	_, err := n.Rent(context.Background(), RentSpec{
		Label:          "luncur-gpu-pending",
		NebiusPlatform: "gpu-h100-sxm",
		NebiusPreset:   "1gpu-16vcpu-200gb",
	})
	if err == nil {
		t.Fatal("want error on poll timeout")
	}
	if !errors.Is(err, ErrRentAmbiguous) {
		t.Fatalf("err = %v, want ErrRentAmbiguous", err)
	}
}

func TestNebiusName(t *testing.T) {
	n := NewNebius(NebiusConfig{})
	if n.Name() != "nebius" {
		t.Fatalf("Name() = %q, want nebius", n.Name())
	}
}
