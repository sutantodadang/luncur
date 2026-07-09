package server

import (
	"encoding/hex"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/sutantodadang/luncur/internal/secret"
)

// TestPrometheusEndpoint: the endpoint is invisible (404) until
// metrics_token is set; wrong/missing bearer token 401s; the right token
// yields Prometheus exposition text with the app metrics.
func TestPrometheusEndpoint(t *testing.T) {
	srv, _ := testServer(t) // no metrics_token set -> 404

	resp, err := http.Get(srv.URL + "/metrics/prometheus")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unset token: got %d, want 404", resp.StatusCode)
	}

	// Set up a server with a sealer and a sealed metrics_token, mirroring
	// how notify_test.go/backup_test.go seed other sealed settings directly
	// on the store (bypassing the settings API).
	sealer, err := secret.New(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	st2 := newTestStore(t)
	sealed, err := sealer.Seal([]byte("s3cret"))
	if err != nil {
		t.Fatal(err)
	}
	if err := st2.SetSetting("metrics_token", "sealed:"+hex.EncodeToString(sealed)); err != nil {
		t.Fatal(err)
	}
	srv2 := newHTTPTest(t, Deps{Store: st2, Sealer: sealer})

	// wrong token -> 401
	req, _ := http.NewRequest("GET", srv2.URL+"/metrics/prometheus", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong token: got %d, want 401", resp2.StatusCode)
	}

	// right token -> exposition text
	req.Header.Set("Authorization", "Bearer s3cret")
	resp3, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp3.Body.Close()
	body, _ := io.ReadAll(resp3.Body)
	if resp3.StatusCode != 200 || !strings.Contains(string(body), "# TYPE luncur_app_deploys_total counter") {
		t.Fatalf("got %d\n%s", resp3.StatusCode, body)
	}
}
