//go:build e2e

// Package e2e boots the real server against a real cluster (kind in CI) and
// drives the public API: login -> create project -> create app -> deploy a
// public image -> poll until the Deployment is ready -> clean up.
//
// Requires KUBECONFIG pointing at a reachable cluster; skipped otherwise
// (e.g. on developer machines without a local kind cluster). CI provisions
// kind via helm/kind-action and sets KUBECONFIG before running this suite.
package e2e

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sutantodadang/luncur/internal/client"
	"github.com/sutantodadang/luncur/internal/kube"
	"github.com/sutantodadang/luncur/internal/secret"
	"github.com/sutantodadang/luncur/internal/server"
	"github.com/sutantodadang/luncur/internal/store"
)

func TestDeployRoundTrip(t *testing.T) {
	kubeconfig := os.Getenv("KUBECONFIG")
	if kubeconfig == "" {
		t.Skip("KUBECONFIG not set")
	}

	// 1. Temp SQLite store + bootstrap admin (mirrors bootstrapAdmin in
	// internal/cli/serve.go, minus the CLI plumbing).
	st, err := store.Open(filepath.Join(t.TempDir(), "e2e.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	const email, password = "e2e@luncur.test", "pw123456789"
	if _, err := st.CreateUser(email, password, "admin"); err != nil {
		t.Fatalf("bootstrap admin: %v", err)
	}

	// Throwaway sealing key — same pattern internal/client's own test
	// helper (testAPI in client_test.go) uses; nothing here needs to
	// survive a restart.
	sealer, err := secret.New(make([]byte, 32))
	if err != nil {
		t.Fatalf("sealer: %v", err)
	}

	kubeClient, err := kube.New(kubeconfig)
	if err != nil {
		t.Fatalf("kube client from KUBECONFIG=%s: %v", kubeconfig, err)
	}

	// 2. Real server, real handler, behind httptest.
	srv := httptest.NewServer(server.New(server.Deps{
		Store:      st,
		Sealer:     sealer,
		Kube:       kubeClient,
		ExternalIP: "127.0.0.1",
	}))
	defer srv.Close()

	// 3. Drive the public API via internal/client.
	c := client.New(srv.URL, "")
	tok, err := c.Login(email, password)
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	c = client.New(srv.URL, tok)

	if _, err := c.CreateProject("e2e"); err != nil {
		t.Fatalf("create project: %v", err)
	}
	defer func() {
		// 5. Cleanup: delete project (namespace should go away).
		if err := c.DeleteProject("e2e"); err != nil {
			t.Logf("cleanup: delete project e2e: %v", err)
		}
	}()

	if _, err := c.CreateApp("e2e", "web", 80, "web", "", "", false, 0); err != nil {
		t.Fatalf("create app: %v", err)
	}
	if _, err := c.Deploy("e2e", "web", "nginx:alpine"); err != nil {
		t.Fatalf("deploy nginx:alpine: %v", err)
	}

	// 4. Poll app metrics until ReadyReplicas >= 1, deadline 3 min.
	deadline := time.Now().Add(3 * time.Minute)
	for {
		m, mErr := c.AppMetrics("e2e", "web")
		if mErr == nil && m.ReadyReplicas >= 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("app not ready after 3m: metrics=%+v err=%v", m, mErr)
		}
		time.Sleep(5 * time.Second)
	}
}
