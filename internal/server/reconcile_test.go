package server

import (
	"context"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"

	"github.com/sutantodadang/luncur/internal/kube"
	"github.com/sutantodadang/luncur/internal/secret"
	"github.com/sutantodadang/luncur/internal/store"
)

// waitTerminal polls a deployment until it reaches live/failed, mirroring
// the poll loop apps_test.go uses for async builds — reconcileUnfinished's
// per-deployment work runs in its own goroutine, so tests must wait for it
// rather than asserting immediately.
func waitTerminal(t *testing.T, st *store.Store, id string) store.Deployment {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		got, err := st.GetDeployment(id)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status == "failed" || got.Status == "live" {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("deployment did not reach terminal status, stuck at %q", got.Status)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestReconcileBuildingNoJobMarksFailed covers a deployment orphaned in
// 'building' whose Build Job didn't survive the restart either: with an
// empty fake dynamic client (no job seeded), JobExists reports false and
// reconcileBuilding must mark the deployment failed synchronously, with no
// goroutine to wait for.
func TestReconcileBuildingNoJobMarksFailed(t *testing.T) {
	st := newTestStore(t)
	sealer, err := secret.New(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	scheme := runtime.NewScheme()
	dyn := dynamicfake.NewSimpleDynamicClient(scheme)
	srv := newServer(Deps{
		Store: st, Sealer: sealer, Kube: kube.NewFromDynamic(dyn),
		ExternalIP: "1.2.3.4", DataDir: t.TempDir(),
	})

	p, err := st.CreateProject("web")
	if err != nil {
		t.Fatal(err)
	}
	a, err := st.CreateApp(p.ID, "api", 8080, "web", "")
	if err != nil {
		t.Fatal(err)
	}
	d, err := st.CreateDeployment(a.ID, "building", "", 0)
	if err != nil {
		t.Fatal(err)
	}

	srv.reconcileUnfinished(context.Background())

	got, err := st.GetDeployment(d.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "failed" {
		t.Fatalf("status = %q, want failed", got.Status)
	}
}

// TestReconcileBuildingJobStillRunningEndsLive covers a deployment orphaned
// in 'building' whose Build Job survived the restart (buildServer's fake
// dynamic client reports the Job as already succeeded on Get): reconcile
// re-attaches, waits, and finishes the deploy exactly like a fresh build
// would.
func TestReconcileBuildingJobStillRunningEndsLive(t *testing.T) {
	srv, st, _ := buildServer(t)
	p, err := st.CreateProject("web")
	if err != nil {
		t.Fatal(err)
	}
	a, err := st.CreateApp(p.ID, "api", 8080, "web", "")
	if err != nil {
		t.Fatal(err)
	}
	d, err := st.CreateDeployment(a.ID, "building", "", 0)
	if err != nil {
		t.Fatal(err)
	}

	srv.reconcileUnfinished(context.Background())

	got := waitTerminal(t, st, d.ID)
	if got.Status != "live" {
		t.Fatalf("status = %q, want live", got.Status)
	}
	if got.ImageRef == "" {
		t.Fatalf("image ref not set")
	}
}

// TestReconcileDeployingResumesLive covers a deployment orphaned in
// 'deploying' — the build already succeeded and image_ref is set, so
// reconcile only needs to re-run finishDeploy's apply-and-mark-live tail.
func TestReconcileDeployingResumesLive(t *testing.T) {
	srv, st, _ := buildServer(t)
	p, err := st.CreateProject("web")
	if err != nil {
		t.Fatal(err)
	}
	a, err := st.CreateApp(p.ID, "api", 8080, "web", "")
	if err != nil {
		t.Fatal(err)
	}
	d, err := st.CreateDeployment(a.ID, "deploying", "registry.luncur-system:5000/web-api:7", 0)
	if err != nil {
		t.Fatal(err)
	}

	srv.reconcileUnfinished(context.Background())

	got := waitTerminal(t, st, d.ID)
	if got.Status != "live" {
		t.Fatalf("status = %q, want live", got.Status)
	}
	if got.ImageRef != "registry.luncur-system:5000/web-api:7" {
		t.Fatalf("image ref = %q, want unchanged", got.ImageRef)
	}
}

// TestReconcileSkipsWithoutKube guards the s.kube == nil early return: no
// panic, no deployment mutated.
func TestReconcileSkipsWithoutKube(t *testing.T) {
	st := newTestStore(t)
	srv := newServer(Deps{Store: st, DataDir: t.TempDir()})

	p, err := st.CreateProject("web")
	if err != nil {
		t.Fatal(err)
	}
	a, err := st.CreateApp(p.ID, "api", 8080, "web", "")
	if err != nil {
		t.Fatal(err)
	}
	d, err := st.CreateDeployment(a.ID, "building", "", 0)
	if err != nil {
		t.Fatal(err)
	}

	srv.reconcileUnfinished(context.Background())

	got, err := st.GetDeployment(d.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "building" {
		t.Fatalf("status = %q, want unchanged building", got.Status)
	}
}
