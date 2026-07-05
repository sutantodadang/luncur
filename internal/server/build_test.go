package server

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8sfake "k8s.io/client-go/kubernetes/fake"
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

// TestRunBuildIncludesAppEnvAsBuildArgs checks an app's env vars (set the
// same way `luncur env set` does, via setAppEnv/s.sealer) reach the
// rendered Build Job as LUNCUR_BUILDARG_<KEY> — the plumbing that lets a
// Dockerfile's `ARG VITE_API_URL` see a real value instead of its baked-in
// default.
func TestRunBuildIncludesAppEnvAsBuildArgs(t *testing.T) {
	srv, st, jobJSON := captureBuildServer(t)
	p, err := st.CreateProject("web")
	if err != nil {
		t.Fatal(err)
	}
	a, err := st.CreateApp(p.ID, "api", 8080, "web", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.setAppEnv(context.Background(), p, a, "VITE_API_URL", "https://api.example.com"); err != nil {
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
	if env["LUNCUR_BUILDARG_VITE_API_URL"] != "https://api.example.com" {
		t.Fatalf("LUNCUR_BUILDARG_VITE_API_URL=%q, want https://api.example.com", env["LUNCUR_BUILDARG_VITE_API_URL"])
	}
}

// TestRunBuildOmitsBuildArgsWithNoEnv checks an app with no env vars renders
// a Build Job with no LUNCUR_BUILDARG_* vars at all — build jobs for
// env-less apps stay byte-identical to before this feature existed.
func TestRunBuildOmitsBuildArgsWithNoEnv(t *testing.T) {
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
	for k := range env {
		if strings.HasPrefix(k, "LUNCUR_BUILDARG_") {
			t.Fatalf("unexpected build-arg env var %q present: %+v", k, env)
		}
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

// TestRunBuildWritesMilestoneLog checks that a successful build leaves
// server-written [luncur] milestone lines in the deploy log, so the UI
// isn't blind before the builder pod exists — the actual builder pod
// entrypoint output is appended by a real cluster, not this fake-kube test.
func TestRunBuildWritesMilestoneLog(t *testing.T) {
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

	if err := srv.runBuild(context.Background(), p, a, d); err != nil {
		t.Fatalf("runBuild: %v", err)
	}

	b, err := os.ReadFile(srv.src.LogPath(d.ID))
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	log := string(b)
	for _, want := range []string{"rendering build job", "applying build job to cluster", "waiting for builder pod"} {
		if !strings.Contains(log, want) {
			t.Fatalf("log missing %q, got:\n%s", want, log)
		}
	}
}

// TestRunBuildFailureLogsMilestone checks fail()'s "build failed: <err>"
// milestone lands in the deploy log before the deployment flips to failed.
func TestRunBuildFailureLogsMilestone(t *testing.T) {
	srv, st := buildServerFailingJob(t)
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

	b, err := os.ReadFile(srv.src.LogPath(d.ID))
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if !strings.Contains(string(b), "build failed:") {
		t.Fatalf("log missing failure milestone, got:\n%s", string(b))
	}
}

// TestBuildLogfNoopWithoutSource guards buildLogf's nil-src early return:
// no data dir configured (s.src == nil) must never panic or create files.
func TestBuildLogfNoopWithoutSource(t *testing.T) {
	st := newTestStore(t)
	srv := newServer(Deps{Store: st})
	srv.buildLogf(store.Deployment{ID: "1"}, "hello %s", "world")
}

// watcherTestServer builds a *server wired only with a fake Kubernetes
// clientset (no dynamic client — watchBuildPod only ever calls
// JobPodStatus/JobEvents, both of which go through the clientset half), for
// exercising watchBuildPod directly without a full runBuild.
func watcherTestServer(t *testing.T, cs *k8sfake.Clientset) (*server, store.Deployment) {
	t.Helper()
	st := newTestStore(t)
	sealer, err := secret.New(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	srv := newServer(Deps{
		Store:   st,
		Sealer:  sealer,
		Kube:    kube.NewForTest(nil, cs),
		DataDir: t.TempDir(),
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
	return srv, d
}

// TestWatchBuildPodLogsJobEventsWhenNoPod drives the no-pod-ever-created
// path: a Job with zero pods but a seeded PodSecurity-rejection event.
// emptyPodChecksBeforeJobEvents is lowered to 1 so the watcher's immediate
// first check already crosses the threshold, no ticker wait needed.
func TestWatchBuildPodLogsJobEventsWhenNoPod(t *testing.T) {
	origChecks, origInterval := emptyPodChecksBeforeJobEvents, jobEventsReemitInterval
	emptyPodChecksBeforeJobEvents = 1
	jobEventsReemitInterval = time.Millisecond
	t.Cleanup(func() {
		emptyPodChecksBeforeJobEvents = origChecks
		jobEventsReemitInterval = origInterval
	})

	ev := &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Name: "build-1.1", Namespace: "luncur-system"},
		InvolvedObject: corev1.ObjectReference{Kind: "Job", Name: "build-1"},
		Type:           "Warning",
		Reason:         "FailedCreate",
		Message:        `pods "build-1-" is forbidden: violates PodSecurity "restricted:latest"`,
		LastTimestamp:  metav1.Now(),
	}
	cs := k8sfake.NewSimpleClientset(ev)
	srv, d := watcherTestServer(t, cs)

	done := make(chan struct{})
	go srv.watchBuildPod(context.Background(), d, "build-1", done)
	time.Sleep(150 * time.Millisecond)
	close(done)

	b, err := os.ReadFile(srv.src.LogPath(d.ID))
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	log := string(b)
	for _, want := range []string{
		"no builder pod created yet",
		`Warning FailedCreate: pods "build-1-" is forbidden: violates PodSecurity "restricted:latest"`,
	} {
		if !strings.Contains(log, want) {
			t.Fatalf("log missing %q, got:\n%s", want, log)
		}
	}
}

// TestWatchBuildPodLogsWatcherErrorOnce drives a persistent pod-listing
// error (e.g. missing RBAC) across several fast polls and checks it's
// logged exactly once, not once per poll.
func TestWatchBuildPodLogsWatcherErrorOnce(t *testing.T) {
	origPoll := watchBuildPollInterval
	watchBuildPollInterval = 10 * time.Millisecond
	t.Cleanup(func() { watchBuildPollInterval = origPoll })

	cs := k8sfake.NewSimpleClientset()
	cs.PrependReactor("list", "pods", func(a ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, fmt.Errorf("pods is forbidden: User cannot list resource")
	})
	srv, d := watcherTestServer(t, cs)

	done := make(chan struct{})
	go srv.watchBuildPod(context.Background(), d, "build-1", done)
	time.Sleep(150 * time.Millisecond) // several polls at the 10ms interval
	close(done)

	b, err := os.ReadFile(srv.src.LogPath(d.ID))
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	log := string(b)
	if got := strings.Count(log, "pod watcher error:"); got != 1 {
		t.Fatalf("want exactly 1 \"pod watcher error:\" line, got %d in:\n%s", got, log)
	}
}

// TestBuildTimeoutDefaultAndSetting checks buildTimeout falls back to 15
// minutes when build_timeout_minutes is unset, and honors it once set.
func TestBuildTimeoutDefaultAndSetting(t *testing.T) {
	st := newTestStore(t)
	srv := newServer(Deps{Store: st})

	if got := srv.buildTimeout(); got != 15*time.Minute {
		t.Fatalf("default buildTimeout = %v, want 15m", got)
	}

	if err := st.SetSetting("build_timeout_minutes", "30"); err != nil {
		t.Fatal(err)
	}
	if got := srv.buildTimeout(); got != 30*time.Minute {
		t.Fatalf("buildTimeout with setting = %v, want 30m", got)
	}
}
