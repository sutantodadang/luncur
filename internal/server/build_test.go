package server

import (
	"context"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	ktesting "k8s.io/client-go/testing"

	"github.com/sutantodadang/luncur/internal/kube"
	"github.com/sutantodadang/luncur/internal/secret"
	"github.com/sutantodadang/luncur/internal/store"
)

// buildServer builds a *server (not wrapped in an HTTP mux, so unexported
// methods like runBuild are directly callable) wired with build config and
// a fake dynamic client that records every action and reports the Build
// Job as succeeded on Get, so WaitJob returns immediately.
func buildServer(t *testing.T) (*server, *store.Store, *[]string) {
	t.Helper()
	st := newTestStore(t)
	scheme := runtime.NewScheme()
	dyn := dynamicfake.NewSimpleDynamicClient(scheme)
	var actions []string

	// Catch-all recorder first, then the "get jobs" reactor prepended
	// after it — PrependReactor inserts at the front of the chain each
	// time, so the reactor added last (get jobs) is tried first and wins
	// over the catch-all for that specific verb/resource.
	dyn.PrependReactor("*", "*", func(a ktesting.Action) (bool, runtime.Object, error) {
		actions = append(actions, a.GetVerb()+" "+a.GetResource().Resource)
		return true, nil, nil
	})
	dyn.PrependReactor("get", "jobs", func(a ktesting.Action) (bool, runtime.Object, error) {
		return true, &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "batch/v1", "kind": "Job",
			"metadata": map[string]any{"name": a.(ktesting.GetAction).GetName(), "namespace": "luncur-system"},
			"status":   map[string]any{"succeeded": int64(1)},
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
	return srv, st, &actions
}

func TestRunBuildSuccess(t *testing.T) {
	srv, st, actions := buildServer(t)
	p, err := st.CreateProject("web")
	if err != nil {
		t.Fatal(err)
	}
	a, err := st.CreateApp(p.ID, "api", 8080)
	if err != nil {
		t.Fatal(err)
	}
	d, err := st.CreateDeployment(a.ID, "building", "", 0)
	if err != nil {
		t.Fatal(err)
	}

	if err := srv.runBuild(context.Background(), p, a, d); err != nil {
		t.Fatalf("runBuild: %v", err)
	}

	got, err := st.GetDeployment(d.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "live" {
		t.Fatalf("status=%q want live", got.Status)
	}
	if got.ImageRef == "" {
		t.Fatalf("image ref not set")
	}

	joined := strings.Join(*actions, ",")
	if !strings.Contains(joined, "patch jobs") {
		t.Fatalf("build Job not applied; actions=%v", *actions)
	}
	if !strings.Contains(joined, "patch deployments") {
		t.Fatalf("app Deployment not applied; actions=%v", *actions)
	}
}
