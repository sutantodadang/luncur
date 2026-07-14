package server

import (
	"context"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	ktesting "k8s.io/client-go/testing"

	"github.com/sutantodadang/luncur/internal/kube"
	"github.com/sutantodadang/luncur/internal/secret"
)

// previewTestServer builds a *server (no HTTP wrapper — routeBranch is
// unexported, so tests call it directly, mirroring TestRequireEnv's own
// style) with a fake kube layer that never reports a build job as
// terminal, so a deploy routeBranch triggers deterministically stays
// "building" — same convention as webhook_test.go's webhookTestServer.
func previewTestServer(t *testing.T) (*server, *[]string) {
	t.Helper()
	st := newTestStore(t)
	scheme := runtime.NewScheme()
	dyn := dynamicfake.NewSimpleDynamicClient(scheme)
	var actions []string
	dyn.PrependReactor("*", "*", func(a ktesting.Action) (bool, runtime.Object, error) {
		actions = append(actions, a.GetVerb()+" "+a.GetResource().Resource)
		return true, nil, nil
	})
	dyn.PrependReactor("get", "jobs", func(a ktesting.Action) (bool, runtime.Object, error) {
		return true, &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "batch/v1", "kind": "Job",
			"metadata": map[string]any{"name": a.(ktesting.GetAction).GetName(), "namespace": "luncur-system"},
			"status":   map[string]any{},
		}}, nil
	})
	sealer, err := secret.New(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	srv := newServer(Deps{
		Store: st, Kube: kube.NewFromDynamic(dyn), Sealer: sealer,
		ExternalIP: "1.2.3.4", DataDir: t.TempDir(),
	})
	return srv, &actions
}

// TestRouteBranch covers routeBranch's dispatch: a standing environment's
// base_branch match deploys that environment's git apps and bumps
// LastActiveAt; an unmapped branch falls through to ensurePreview (stubbed
// until Task 13 — asserting the fall-through reached it, via its
// not-yet-implemented error, is the "stub/spy" coverage for this task); a
// delete/PR-close event with no matching preview environment is a no-op.
func TestRouteBranch(t *testing.T) {
	srv, _ := previewTestServer(t)
	st := srv.st

	p, err := st.CreateProject("proj")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SeedProjectEnvironments(p.ID); err != nil {
		t.Fatal(err)
	}
	p, err = st.GetProjectByID(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	prod, err := st.GetEnvironment(p.ID, "production") // base_branch "main"
	if err != nil {
		t.Fatal(err)
	}

	prodApp, err := st.CreateGitApp(p.ID, "api", 8080, "https://x/y.git", "main", "web", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetAppEnvironmentID(prodApp.ID, prod.ID); err != nil {
		t.Fatal(err)
	}

	// Standing branch match: deploys the production app and touches the
	// environment.
	if err := srv.routeBranch(context.Background(), p, "main", false); err != nil {
		t.Fatalf("routeBranch(main): %v", err)
	}
	if n, err := st.CountDeployments(prodApp.ID); err != nil || n != 1 {
		t.Fatalf("want 1 deployment after standing-branch route, got %d (%v)", n, err)
	}
	touched, err := st.GetEnvironmentByID(prod.ID)
	if err != nil || touched.LastActiveAt == "" {
		t.Fatalf("expected production LastActiveAt set: %+v %v", touched, err)
	}

	// Unmapped branch falls through to ensurePreview — stubbed (Task 13)
	// to return a "not yet implemented" error, so its being reached (and
	// propagated) is the observable proof of the fall-through.
	err = srv.routeBranch(context.Background(), p, "feature/x", false)
	if err == nil {
		t.Fatal("routeBranch(feature/x): want error from stubbed ensurePreview, got nil")
	}

	// Delete/PR-close routing to a branch with no matching preview: no-op,
	// no error.
	if err := srv.routeBranch(context.Background(), p, "no-such-branch", true); err != nil {
		t.Fatalf("routeBranch(delete, missing preview): %v", err)
	}
}
