package server

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"

	"github.com/sutantodadang/luncur/internal/kube"
	"github.com/sutantodadang/luncur/internal/secret"
	"github.com/sutantodadang/luncur/internal/store"
)

func TestRunsAPI(t *testing.T) {
	runWatchPoll = 10 * time.Millisecond
	t.Cleanup(func() { runWatchPoll = 5 * time.Second })

	srv, st, actions := kubeServer(t)
	admin := seedUserToken(t, st, "root@b.co", "admin")
	doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"ml"}`).Body.Close()
	doAuthed(t, "POST", srv.URL+"/v1/projects/ml/apps", admin, `{"name":"train","kind":"job"}`).Body.Close()

	// Runs on a non-job app -> kind_mismatch.
	doAuthed(t, "POST", srv.URL+"/v1/projects/ml/apps", admin, `{"name":"api","port":3000}`).Body.Close()
	resp := doAuthed(t, "POST", srv.URL+"/v1/projects/ml/apps/api/runs", admin, `{}`)
	if resp.StatusCode != 400 || errCode(t, mustReadBody(t, resp)) != "kind_mismatch" {
		t.Fatalf("non-job run: want 400 kind_mismatch, got %d", resp.StatusCode)
	}

	// Run before any deploy -> not_deployed.
	resp = doAuthed(t, "POST", srv.URL+"/v1/projects/ml/apps/train/runs", admin, `{}`)
	if resp.StatusCode != 409 || errCode(t, mustReadBody(t, resp)) != "not_deployed" {
		t.Fatalf("undeployed run: want 409 not_deployed, got %d", resp.StatusCode)
	}

	// Deploy an image (job kind: applies Secret/PVCs only, marks live).
	resp = doAuthed(t, "POST", srv.URL+"/v1/projects/ml/apps/train/deploy", admin, `{"image":"trainer:1"}`)
	if resp.StatusCode != 200 {
		t.Fatalf("deploy: want 200, got %d (%s)", resp.StatusCode, mustReadBody(t, resp))
	}
	resp.Body.Close()

	// Trigger a run.
	resp = doAuthed(t, "POST", srv.URL+"/v1/projects/ml/apps/train/runs", admin, `{}`)
	if resp.StatusCode != 202 {
		t.Fatalf("run: want 202, got %d (%s)", resp.StatusCode, mustReadBody(t, resp))
	}
	var run map[string]any
	json.NewDecoder(resp.Body).Decode(&run)
	resp.Body.Close()
	if run["job"] != "train-run-1" || run["status"] != "running" {
		t.Fatalf("run: %+v", run)
	}
	if !strings.Contains(strings.Join(*actions, ","), "patch jobs") {
		t.Fatalf("no Job applied: %v", *actions)
	}

	// History lists the run (its status may already be failed: the fake
	// dynamic client errors the watcher's Job poll).
	resp = doAuthed(t, "GET", srv.URL+"/v1/projects/ml/apps/train/runs", admin, "")
	var runs []map[string]any
	json.NewDecoder(resp.Body).Decode(&runs)
	resp.Body.Close()
	if len(runs) != 1 {
		t.Fatalf("runs: %+v", runs)
	}

	// Single-run fetch.
	resp = doAuthed(t, "GET", srv.URL+"/v1/projects/ml/apps/train/runs/1", admin, "")
	if resp.StatusCode != 200 {
		t.Fatalf("get run: want 200, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Unknown run id.
	resp = doAuthed(t, "GET", srv.URL+"/v1/projects/ml/apps/train/runs/99", admin, "")
	if resp.StatusCode != 404 {
		t.Fatalf("missing run: want 404, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Malformed run id.
	resp = doAuthed(t, "GET", srv.URL+"/v1/projects/ml/apps/train/runs/zzz", admin, "")
	if resp.StatusCode != 400 {
		t.Fatalf("bad run id: want 400, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

// runRecord is one dynamic/typed-clientset action captured by
// kubeServerWithPods's reactors — verb+resource for a quick membership
// check, plus the raw patch body when the caller needs to inspect the
// object (e.g. a Job's spec.completions).
type runRecord struct {
	verb, resource string
	patch          []byte
}

// kubeServerWithPods is kubeServer's twin with the typed clientset half also
// wired (seeded with pods), so kube.RunningJobPods/DeleteJob — the
// multi-node gang guard's dependencies — work against it instead of always
// hitting the "no clientset" nil branch.
func kubeServerWithPods(t *testing.T, pods ...runtime.Object) (*httptestServer, *store.Store, *[]runRecord) {
	t.Helper()
	st := newTestStore(t)
	scheme := runtime.NewScheme()
	dyn := dynamicfake.NewSimpleDynamicClient(scheme)
	var recs []runRecord
	dyn.PrependReactor("*", "*", func(a ktesting.Action) (bool, runtime.Object, error) {
		rec := runRecord{verb: a.GetVerb(), resource: a.GetResource().Resource}
		if pa, ok := a.(ktesting.PatchAction); ok {
			rec.patch = pa.GetPatch()
		}
		recs = append(recs, rec)
		return true, nil, nil
	})
	cs := k8sfake.NewSimpleClientset(pods...)
	// Record, don't intercept: RunningJobPods/DeleteJob (the typed-clientset
	// half) need the fake's real List/Delete behavior against its object
	// tracker, unlike the dynamic half above which is recorded-only.
	cs.PrependReactor("*", "*", func(a ktesting.Action) (bool, runtime.Object, error) {
		recs = append(recs, runRecord{verb: a.GetVerb(), resource: a.GetResource().Resource})
		return false, nil, nil
	})
	sealer, err := secret.New(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	srv := newHTTPTest(t, Deps{Store: st, Kube: kube.NewForTest(dyn, cs), Sealer: sealer, ExternalIP: "1.2.3.4"})
	return srv, st, &recs
}

// runningJobPod builds a pod already carrying the label the Job controller
// stamps on every pod it creates, in phase Running — the shape gangGuard
// looks for via kube.RunningJobPods.
func runningJobPod(name, namespace, jobName string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: namespace,
			Labels: map[string]string{"batch.kubernetes.io/job-name": jobName},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
}

// TestCreateRunMultiNode covers a run's per-request nodes/framework
// overrides end to end: the API accepts them, the run row records them, and
// the rendered objects applied to the cluster are the multi-node shape (an
// Indexed Job with completions=3 plus the headless Service).
func TestCreateRunMultiNode(t *testing.T) {
	runWatchPoll = 10 * time.Millisecond
	gangTimeoutUnit = 10 * time.Millisecond
	t.Cleanup(func() {
		runWatchPoll = 5 * time.Second
		gangTimeoutUnit = time.Minute
	})

	// Namespace is "luncur-ml" (Store.CreateProject's derivation) and the
	// first run of "train" is named "train-run-1" (jobRunName). Seeding all
	// 3 pods already Running lets the gang guard's background goroutine
	// resolve immediately instead of idling for its timeout window.
	srv, st, recs := kubeServerWithPods(t,
		runningJobPod("train-run-1-0", "luncur-ml", "train-run-1"),
		runningJobPod("train-run-1-1", "luncur-ml", "train-run-1"),
		runningJobPod("train-run-1-2", "luncur-ml", "train-run-1"),
	)
	admin := seedUserToken(t, st, "root@b.co", "admin")
	doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"ml"}`).Body.Close()
	doAuthed(t, "POST", srv.URL+"/v1/projects/ml/apps", admin, `{"name":"train","kind":"job"}`).Body.Close()
	doAuthed(t, "POST", srv.URL+"/v1/projects/ml/apps/train/deploy", admin, `{"image":"trainer:1"}`).Body.Close()

	resp := doAuthed(t, "POST", srv.URL+"/v1/projects/ml/apps/train/runs", admin, `{"nodes":3,"framework":"torch"}`)
	if resp.StatusCode != 202 {
		t.Fatalf("run: want 202, got %d (%s)", resp.StatusCode, mustReadBody(t, resp))
	}
	var run map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&run); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if run["nodes"] != float64(3) || run["framework"] != "torch" {
		t.Fatalf("run response: %+v", run)
	}

	got, err := st.GetJobRun(int64(run["id"].(float64)))
	if err != nil {
		t.Fatal(err)
	}
	if got.Nodes != 3 || got.Framework != "torch" {
		t.Fatalf("run row: nodes=%d framework=%q, want 3 torch", got.Nodes, got.Framework)
	}

	var jobPatch []byte
	var sawService bool
	for _, rec := range *recs {
		if rec.verb != "patch" {
			continue
		}
		switch rec.resource {
		case "jobs":
			jobPatch = rec.patch
		case "services":
			sawService = true
		}
	}
	if jobPatch == nil {
		t.Fatalf("no Job applied: %+v", *recs)
	}
	if !sawService {
		t.Fatalf("no Service applied for multi-node run: %+v", *recs)
	}
	var job struct {
		Spec struct {
			Completions *int32 `json:"completions"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(jobPatch, &job); err != nil {
		t.Fatal(err)
	}
	if job.Spec.Completions == nil || *job.Spec.Completions != 3 {
		t.Fatalf("job completions = %v, want 3", job.Spec.Completions)
	}
}

// TestCreateRunBadFramework covers handleCreateRun rejecting an unknown
// framework before ever touching the cluster or the run table.
func TestCreateRunBadFramework(t *testing.T) {
	srv, st, _ := kubeServer(t)
	admin := seedUserToken(t, st, "root@b.co", "admin")
	doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"ml"}`).Body.Close()
	doAuthed(t, "POST", srv.URL+"/v1/projects/ml/apps", admin, `{"name":"train","kind":"job"}`).Body.Close()

	resp := doAuthed(t, "POST", srv.URL+"/v1/projects/ml/apps/train/runs", admin, `{"framework":"mpi"}`)
	body := mustReadBody(t, resp)
	if resp.StatusCode != 400 || errCode(t, body) != "bad_request" {
		t.Fatalf("bad framework: want 400 bad_request, got %d (%s)", resp.StatusCode, body)
	}
}

// TestCreateRunOverBudget covers startRun's GPU budget delta check: a
// gpu=1 app defaulting to nodes=1 asking for nodes=3 needs 2 more GPUs
// (1×3 − 1×1); a quota of 2 with the app's own footprint (1) already
// counted leaves only 1 free, so the request is rejected before any run
// row or cluster object is created.
func TestCreateRunOverBudget(t *testing.T) {
	srv, st, _ := kubeServer(t)
	admin := seedUserToken(t, st, "root@b.co", "admin")
	doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"ml"}`).Body.Close()
	doAuthed(t, "PUT", srv.URL+"/v1/projects/ml/gpu-quota", admin, `{"quota":2}`).Body.Close()
	doAuthed(t, "POST", srv.URL+"/v1/projects/ml/apps", admin, `{"name":"train","kind":"job","gpu":1}`).Body.Close()
	doAuthed(t, "POST", srv.URL+"/v1/projects/ml/apps/train/deploy", admin, `{"image":"trainer:1"}`).Body.Close()

	resp := doAuthed(t, "POST", srv.URL+"/v1/projects/ml/apps/train/runs", admin, `{"nodes":3}`)
	body := mustReadBody(t, resp)
	if resp.StatusCode != 400 || errCode(t, body) != "over_budget" {
		t.Fatalf("over budget: want 400 over_budget, got %d (%s)", resp.StatusCode, body)
	}

	runs, err := st.ListJobRuns(appID(t, st, "ml", "train"))
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 0 {
		t.Fatalf("over-budget request must not create a run row: %+v", runs)
	}
}

// TestGangGuardTimeout covers the multi-node gang guard's failure path: a
// 2-node run whose Job only ever schedules 1 Running pod never reaches
// quorum, so once train_gang_timeout_minutes closes, watchRun must fail the
// run and tear down the Job (not leave it half-scheduled, burning GPU
// budget).
func TestGangGuardTimeout(t *testing.T) {
	runWatchPoll = 5 * time.Millisecond
	gangTimeoutUnit = 5 * time.Millisecond
	t.Cleanup(func() {
		runWatchPoll = 5 * time.Second
		gangTimeoutUnit = time.Minute
	})

	// Only 1 of the 2 wanted pods ever shows up as Running.
	srv, st, recs := kubeServerWithPods(t,
		runningJobPod("train-run-1-0", "luncur-ml", "train-run-1"),
	)
	admin := seedUserToken(t, st, "root@b.co", "admin")
	doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"ml"}`).Body.Close()
	if err := st.SetSetting(settingTrainGangTimeout, "1"); err != nil {
		t.Fatal(err)
	}
	doAuthed(t, "POST", srv.URL+"/v1/projects/ml/apps", admin, `{"name":"train","kind":"job"}`).Body.Close()
	doAuthed(t, "POST", srv.URL+"/v1/projects/ml/apps/train/deploy", admin, `{"image":"trainer:1"}`).Body.Close()

	resp := doAuthed(t, "POST", srv.URL+"/v1/projects/ml/apps/train/runs", admin, `{"nodes":2}`)
	if resp.StatusCode != 202 {
		t.Fatalf("run: want 202, got %d (%s)", resp.StatusCode, mustReadBody(t, resp))
	}
	var run map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&run); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	runID := int64(run["id"].(float64))

	// gangTimeoutUnit/runWatchPoll are both in the low milliseconds, so the
	// guard's deadline (1 * gangTimeoutUnit) closes well within this window.
	deadline := time.Now().Add(2 * time.Second)
	var got store.JobRun
	for time.Now().Before(deadline) {
		var err error
		got, err = st.GetJobRun(runID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status == "failed" {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got.Status != "failed" {
		t.Fatalf("run status = %q, want failed (gang timeout)", got.Status)
	}

	var sawJobDelete bool
	for _, rec := range *recs {
		if rec.verb == "delete" && rec.resource == "jobs" {
			sawJobDelete = true
		}
	}
	if !sawJobDelete {
		t.Fatalf("gang timeout must delete the Job: %+v", *recs)
	}
}
