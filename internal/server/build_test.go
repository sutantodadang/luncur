package server

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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
	a, err := st.CreateApp(p.ID, "api", 8080, "web", "")
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

// captureBuildServer mirrors buildServer, but additionally captures the
// raw JSON patch body of the applied Build Job so tests can assert on its
// env vars (e.g. LUNCUR_CACHE_REF).
func captureBuildServer(t *testing.T) (*server, *store.Store, *[]byte) {
	t.Helper()
	st := newTestStore(t)
	scheme := runtime.NewScheme()
	dyn := dynamicfake.NewSimpleDynamicClient(scheme)
	var jobJSON []byte

	dyn.PrependReactor("*", "*", func(a ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, nil
	})
	dyn.PrependReactor("patch", "jobs", func(a ktesting.Action) (bool, runtime.Object, error) {
		jobJSON = append([]byte(nil), a.(ktesting.PatchAction).GetPatch()...)
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
	return srv, st, &jobJSON
}

// jobJSONEnv decodes a captured build Job's env vars into a name->value map.
func jobJSONEnv(t *testing.T, jobJSON []byte) map[string]string {
	t.Helper()
	var j struct {
		Spec struct {
			Template struct {
				Spec struct {
					Containers []struct {
						Env []struct{ Name, Value string } `json:"env"`
					} `json:"containers"`
				} `json:"spec"`
			} `json:"template"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(jobJSON, &j); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{}
	for _, e := range j.Spec.Template.Spec.Containers[0].Env {
		env[e.Name] = e.Value
	}
	return env
}

func TestRunBuildIncludesCacheRefByDefault(t *testing.T) {
	srv, st, jobJSON := captureBuildServer(t)
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

	if err := srv.runBuild(context.Background(), p, a, d); err != nil {
		t.Fatalf("runBuild: %v", err)
	}

	env := jobJSONEnv(t, *jobJSON)
	want := "registry.luncur-system:5000/luncur-cache/web-api:buildcache"
	if env["LUNCUR_CACHE_REF"] != want {
		t.Fatalf("LUNCUR_CACHE_REF=%q, want %q", env["LUNCUR_CACHE_REF"], want)
	}
}

func TestRunBuildOmitsCacheRefWhenDisabled(t *testing.T) {
	srv, st, jobJSON := captureBuildServer(t)
	if err := st.SetSetting("build_cache", "off"); err != nil {
		t.Fatal(err)
	}
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

	if err := srv.runBuild(context.Background(), p, a, d); err != nil {
		t.Fatalf("runBuild: %v", err)
	}

	env := jobJSONEnv(t, *jobJSON)
	if _, ok := env["LUNCUR_CACHE_REF"]; ok {
		t.Fatalf("LUNCUR_CACHE_REF present, want absent: %+v", env)
	}
}

// buildServerFailingJob mirrors buildServer, but the Build Job reports
// "failed" rather than "succeeded", so runBuild's fail() path fires.
func buildServerFailingJob(t *testing.T) (*server, *store.Store) {
	t.Helper()
	st := newTestStore(t)
	scheme := runtime.NewScheme()
	dyn := dynamicfake.NewSimpleDynamicClient(scheme)
	dyn.PrependReactor("*", "*", func(a ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, nil
	})
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

// setSealedNotifyURLForTest seals url into the notify_url setting directly
// against a *server's own sealer/store (build_test.go's server fixtures
// aren't wrapped in an HTTP mux).
func setSealedNotifyURLForTest(t *testing.T, srv *server, url string) {
	t.Helper()
	sealed, err := srv.sealer.Seal([]byte(url))
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.st.SetSetting("notify_url", "sealed:"+hex.EncodeToString(sealed)); err != nil {
		t.Fatal(err)
	}
}

func TestRunBuildNotifiesOnSuccess(t *testing.T) {
	srv, st, _ := buildServer(t)
	ch := make(chan []byte, 4)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body []byte
		body, _ = io.ReadAll(r.Body)
		ch <- body
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(ts.Close)
	setSealedNotifyURLForTest(t, srv, ts.URL)
	if err := st.SetSetting("notify_events", "deploy_success"); err != nil {
		t.Fatal(err)
	}

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

	if err := srv.runBuild(context.Background(), p, a, d); err != nil {
		t.Fatalf("runBuild: %v", err)
	}

	select {
	case body := <-ch:
		var out struct {
			Event string `json:"event"`
			URL   string `json:"url"`
		}
		if err := json.Unmarshal(body, &out); err != nil {
			t.Fatal(err)
		}
		if out.Event != "deploy_success" || out.URL != "http://api.1-2-3-4.sslip.io" {
			t.Fatalf("got %+v", out)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for deploy_success notification")
	}
}

func TestRunBuildNotifiesOnFailure(t *testing.T) {
	srv, st := buildServerFailingJob(t)
	ch := make(chan []byte, 4)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		ch <- body
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(ts.Close)
	setSealedNotifyURLForTest(t, srv, ts.URL)
	if err := st.SetSetting("notify_events", "deploy_failed"); err != nil {
		t.Fatal(err)
	}

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

	if err := srv.runBuild(context.Background(), p, a, d); err == nil {
		t.Fatal("runBuild: want error for a failed build Job")
	}

	select {
	case body := <-ch:
		var out struct {
			Event string `json:"event"`
			Error string `json:"error"`
		}
		if err := json.Unmarshal(body, &out); err != nil {
			t.Fatal(err)
		}
		if out.Event != "deploy_failed" || out.Error == "" {
			t.Fatalf("got %+v", out)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for deploy_failed notification")
	}
}
