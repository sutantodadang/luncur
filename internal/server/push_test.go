package server

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"os"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	ktesting "k8s.io/client-go/testing"

	"github.com/sutantodadang/luncur/internal/kube"
	"github.com/sutantodadang/luncur/internal/secret"
	"github.com/sutantodadang/luncur/internal/store"
)

// buildFailingServer mirrors buildServer (build_test.go) but reports the
// Build Job as failed on Get, so runBuild's WaitJob returns ok=false and the
// deployment ends "failed".
func buildFailingServer(t *testing.T) (*server, *store.Store) {
	t.Helper()
	st := newTestStore(t)
	scheme := runtime.NewScheme()
	dyn := dynamicfake.NewSimpleDynamicClient(scheme)
	dyn.PrependReactor("get", "jobs", func(a ktesting.Action) (bool, runtime.Object, error) {
		return true, &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "batch/v1", "kind": "Job",
			"metadata": map[string]any{"name": a.(ktesting.GetAction).GetName(), "namespace": "luncur-system"},
			"status":   map[string]any{"failed": int64(1)},
		}}, nil
	})

	sealer, err := secret.New(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	srv := newServer(Deps{
		Store:      st,
		Sealer:     sealer,
		Kube:       kube.NewFromDynamic(dyn),
		ExternalIP: "1.2.3.4",
		DataDir:    t.TempDir(),
	})
	return srv, st
}

// pushTestPubKey generates a fresh ed25519 key in authorized_keys format,
// mirroring internal/store/sshkeys_test.go's testPubKey helper.
func pushTestPubKey(t *testing.T) string {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return string(ssh.MarshalAuthorizedKey(signer.PublicKey()))
}

func TestPushBackendHappyPath(t *testing.T) {
	srv, st, _ := buildServer(t)

	p, err := st.CreateProject("web")
	if err != nil {
		t.Fatal(err)
	}
	a, err := st.CreateApp(p.ID, "api", 8080)
	if err != nil {
		t.Fatal(err)
	}

	u, err := st.CreateUser("dev@example.com", "password123", "member")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.AddMember(p.ID, u.ID); err != nil {
		t.Fatal(err)
	}
	outsider, err := st.CreateUser("outsider@example.com", "password123", "member")
	if err != nil {
		t.Fatal(err)
	}

	pub := pushTestPubKey(t)
	key, err := st.AddSSHKey(u.ID, "laptop", pub)
	if err != nil {
		t.Fatal(err)
	}

	backend := &PushBackend{s: srv}

	// Authorize.
	got, err := backend.Authorize(key.Fingerprint)
	if err != nil {
		t.Fatalf("Authorize: %v", err)
	}
	if got.ID != u.ID {
		t.Fatalf("Authorize user = %d, want %d", got.ID, u.ID)
	}
	if _, err := backend.Authorize("SHA256:nope"); err == nil {
		t.Fatal("Authorize accepted unknown fingerprint")
	}

	// Branch.
	branch, err := backend.Branch(u, "web", "api")
	if err != nil {
		t.Fatalf("Branch: %v", err)
	}
	if branch != "main" {
		t.Fatalf("Branch = %q, want main", branch)
	}
	if _, err := backend.Branch(outsider, "web", "api"); err == nil || !strings.Contains(err.Error(), "not a member") {
		t.Fatalf("Branch for non-member = %v, want error containing \"not a member\"", err)
	}
	if _, err := backend.Branch(u, "web", "nope"); err == nil || !strings.Contains(err.Error(), "no such app") {
		t.Fatalf("Branch for unknown app = %v, want error containing \"no such app\"", err)
	}

	// Push.
	tarballBytes := []byte("fake-tarball-bytes")
	var progressBuf bytes.Buffer
	if err := backend.Push(context.Background(), u, "web", "api", bytes.NewReader(tarballBytes), &progressBuf); err != nil {
		t.Fatalf("Push: %v", err)
	}

	d, err := st.LatestDeployment(a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if d.Status != "live" {
		t.Fatalf("deployment status = %q, want live", d.Status)
	}

	saved, err := os.ReadFile(srv.src.TarballPath(d.ID))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(saved, tarballBytes) {
		t.Fatalf("saved tarball = %q, want %q", saved, tarballBytes)
	}

	if !strings.Contains(progressBuf.String(), "live") {
		t.Fatalf("progress = %q, want it to contain \"live\"", progressBuf.String())
	}
}

func TestPushBackendBuildFailure(t *testing.T) {
	srv, st := buildFailingServer(t)

	p, err := st.CreateProject("web")
	if err != nil {
		t.Fatal(err)
	}
	a, err := st.CreateApp(p.ID, "api", 8080)
	if err != nil {
		t.Fatal(err)
	}

	u, err := st.CreateUser("dev@example.com", "password123", "member")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.AddMember(p.ID, u.ID); err != nil {
		t.Fatal(err)
	}

	backend := &PushBackend{s: srv}

	var progressBuf bytes.Buffer
	err = backend.Push(context.Background(), u, "web", "api", bytes.NewReader([]byte("fake-tarball-bytes")), &progressBuf)
	if err == nil {
		t.Fatal("Push succeeded, want error on build failure")
	}

	d, err := st.LatestDeployment(a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if d.Status != "failed" {
		t.Fatalf("deployment status = %q, want failed", d.Status)
	}
}
