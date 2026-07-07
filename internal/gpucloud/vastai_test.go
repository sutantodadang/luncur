package gpucloud

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestVastAIFlow(t *testing.T) {
	var gotAuth, gotSearchBody, gotRentPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/bundles/":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			b, _ := json.Marshal(body)
			gotSearchBody = string(b)
			_, _ = w.Write([]byte(`{"offers":[{"id":42,"gpu_name":"RTX 4090","num_gpus":1,"dph_total":0.35,"geolocation":"SE"}]}`))
		case r.Method == http.MethodPut && r.URL.Path == "/asks/42/":
			gotRentPath = r.URL.Path
			_, _ = w.Write([]byte(`{"success":true,"new_contract":777}`))
		case r.Method == http.MethodGet && r.URL.Path == "/instances/":
			_, _ = w.Write([]byte(`{"instances":[{"id":777,"label":"luncur-gpu-777","actual_status":"running","gpu_name":"RTX 4090","num_gpus":1,"dph_total":0.35}]}`))
		case r.Method == http.MethodDelete && r.URL.Path == "/instances/777/":
			_, _ = w.Write([]byte(`{"success":true}`))
		default:
			http.Error(w, `{"error":"not_found","msg":"nope"}`, http.StatusNotFound)
		}
	}))
	defer srv.Close()

	v := &VastAI{APIKey: "k123", BaseURL: srv.URL}
	ctx := context.Background()

	offers, err := v.SearchOffers(ctx, "RTX 4090", 1, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(offers) != 1 || offers[0].ID != 42 || offers[0].DPHTotal != 0.35 {
		t.Fatalf("offers = %+v", offers)
	}
	if gotAuth != "Bearer k123" {
		t.Fatalf("auth = %q", gotAuth)
	}
	for _, want := range []string{`"vms_enabled":{"eq":true}`, `"gpu_name":{"eq":"RTX 4090"}`, `"num_gpus":{"eq":1}`} {
		if !contains(gotSearchBody, want) {
			t.Fatalf("search body missing %s: %s", want, gotSearchBody)
		}
	}

	id, err := v.Rent(ctx, RentSpec{OfferID: 42, Image: "ubuntu:22.04", DiskGB: 40, Label: "x", Onstart: "#!/bin/sh"})
	if err != nil {
		t.Fatal(err)
	}
	if id != 777 || gotRentPath != "/asks/42/" {
		t.Fatalf("rent id=%d path=%s", id, gotRentPath)
	}

	list, err := v.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Status != "running" {
		t.Fatalf("list = %+v", list)
	}

	if err := v.Destroy(ctx, 777); err != nil {
		t.Fatal(err)
	}

	// Error body surfaces msg.
	if err := v.Destroy(ctx, 1); err == nil || !contains(err.Error(), "nope") {
		t.Fatalf("want vast error with msg, got %v", err)
	}
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }
