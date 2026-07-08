package server

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"

	"github.com/sutantodadang/luncur/internal/kube"
	"github.com/sutantodadang/luncur/internal/secret"
	"github.com/sutantodadang/luncur/internal/store"
)

// sweepUIServer is kubeServer's twin for tests that GET the rendered app
// page: it registers podMetricsGVR's list kind (WithCustomListKinds) so
// s.kube.AppMetrics' PodMetrics List call — which renderAppDetail always
// makes, regardless of app kind — returns a valid empty list instead of
// panicking the fake dynamic client (kubeServer's plain
// NewSimpleDynamicClient has no such registration, which is exactly what
// bites a GET-the-app-page test). Non-list verbs (patch/delete Jobs from
// startSweep/stopSweep) are still recorded and swallowed, same as
// kubeServer/sweepAPIServer — only "list" is let through to the real
// tracker, which safely answers the registered-but-empty PodMetricsList.
func sweepUIServer(t *testing.T) (*httptestServer, *store.Store, *[]string) {
	t.Helper()
	st := newTestStore(t)
	scheme := runtime.NewScheme()
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme,
		map[schema.GroupVersionResource]string{podMetricsGVR: "PodMetricsList"},
	)
	var actions []string
	dyn.PrependReactor("*", "*", func(a ktesting.Action) (bool, runtime.Object, error) {
		actions = append(actions, a.GetVerb()+" "+a.GetResource().Resource)
		switch a.GetVerb() {
		case "get", "list", "watch":
			// Read verbs fall through to the real object tracker: Get
			// returns a proper NotFound (DeploymentStatus's status-line
			// lookup) instead of a nil object that panics on
			// unstructured.NestedInt64, and List answers with the
			// registered-but-empty PodMetricsList (the metrics card)
			// instead of panicking the fake dynamic client on an
			// unregistered list kind.
			return false, nil, nil
		default:
			// Write verbs (patch/create/update/delete) are faked as
			// always-succeeding, same as kubeServer/sweepAPIServer — the
			// fake tracker doesn't support create-on-apply (SSA) upsert
			// semantics, so a real Namespace/Deployment/Job patch here
			// would 404 on the not-yet-existent object.
			return true, nil, nil
		}
	})
	cs := k8sfake.NewSimpleClientset()
	cs.PrependReactor("*", "*", func(a ktesting.Action) (bool, runtime.Object, error) {
		actions = append(actions, a.GetVerb()+" "+a.GetResource().Resource)
		return false, nil, nil
	})
	sealer, err := secret.New(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	srv := newHTTPTest(t, Deps{Store: st, Kube: kube.NewForTest(dyn, cs), Sealer: sealer, ExternalIP: "1.2.3.4"})
	return srv, st, &actions
}

// sweepStartForm is the Sweeps card's start-form field set for a simple
// 2-value grid (a=[1,2]), matching sweepAPIServer's TestCreateSweepHappyPath
// fixture.
func sweepStartForm() url.Values {
	return url.Values{
		"params_yaml": {"a: [\"1\", \"2\"]"},
		"metric":      {"val_loss"},
		"direction":   {"min"},
		"max_trials":  {"10"},
		"parallel":    {"1"},
	}
}

// TestUISweepCreateFormStartsSweepAndShowsOnPage exercises the Sweeps card's
// start form end to end: submitting it creates a sweep with pending trials,
// and the reloaded app page shows the sweep in the history table plus the
// current sweep's trial table (state chips, compact params).
func TestUISweepCreateFormStartsSweepAndShowsOnPage(t *testing.T) {
	srv, st, _ := sweepUIServer(t)
	admin := seedUserToken(t, st, "root@b.co", "admin")
	doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"ml"}`).Body.Close()
	doAuthed(t, "POST", srv.URL+"/v1/projects/ml/apps", admin, `{"name":"train","kind":"job"}`).Body.Close()
	doAuthed(t, "POST", srv.URL+"/v1/projects/ml/apps/train/deploy", admin, `{"image":"trainer:1"}`).Body.Close()

	u, err := st.GetUserByEmail("root@b.co")
	if err != nil {
		t.Fatal(err)
	}
	ck := uiSessionCookie(t, st, u.ID)
	client := noRedirectClient()
	csrfCk := uiCSRF(t, client, srv.URL)

	resp := uiPost(t, client, srv.URL+"/ui/projects/ml/apps/train/sweeps", csrfCk, ck, sweepStartForm())
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("start sweep: want 303, got %d", resp.StatusCode)
	}

	a, err := st.GetApp(mustProjectID(t, st, "ml"), "train")
	if err != nil {
		t.Fatal(err)
	}
	sweeps, err := st.ListSweeps(a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(sweeps) != 1 || sweeps[0].Status != "running" {
		t.Fatalf("sweeps = %+v, want 1 running", sweeps)
	}

	status, body := getUIPage(t, client, srv.URL, "/ui/projects/ml/apps/train", ck)
	if status != http.StatusOK {
		t.Fatalf("GET app page: want 200, got %d", status)
	}
	if !strings.Contains(body, sweeps[0].ID) {
		t.Fatalf("app page missing sweep id %s in history table, got:\n%s", sweeps[0].ID, body)
	}
	if !strings.Contains(body, `class="chip chip-warn">pending`) {
		t.Fatalf("app page missing pending trial chip, got:\n%s", body)
	}
	if !strings.Contains(body, "a=1") && !strings.Contains(body, "a=2") {
		t.Fatalf("app page missing compact trial params, got:\n%s", body)
	}
	if !strings.Contains(body, "luncur sweep start") {
		t.Fatalf("app page missing sweep start CLI echo, got:\n%s", body)
	}
	if !strings.Contains(body, `hx-get="/ui/projects/ml/apps/train/sweeps/`+sweeps[0].ID+`/trials"`) {
		t.Fatalf("app page missing 15s poll fragment for the running sweep, got:\n%s", body)
	}
}

// TestUISweepCreateBadParamsRedirectsWithErr mirrors
// TestUIRunCreateBadFramework: a malformed params.yaml redirects back to the
// app page with ?err= (error banner) instead of a hard error, and no sweep
// is created.
func TestUISweepCreateBadParamsRedirectsWithErr(t *testing.T) {
	srv, st, _ := sweepUIServer(t)
	admin := seedUserToken(t, st, "root@b.co", "admin")
	doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"ml"}`).Body.Close()
	doAuthed(t, "POST", srv.URL+"/v1/projects/ml/apps", admin, `{"name":"train","kind":"job"}`).Body.Close()
	doAuthed(t, "POST", srv.URL+"/v1/projects/ml/apps/train/deploy", admin, `{"image":"trainer:1"}`).Body.Close()

	u, err := st.GetUserByEmail("root@b.co")
	if err != nil {
		t.Fatal(err)
	}
	ck := uiSessionCookie(t, st, u.ID)
	client := noRedirectClient()
	csrfCk := uiCSRF(t, client, srv.URL)

	form := sweepStartForm()
	form.Set("params_yaml", "lr: 0.1") // scalar, not a list or {min,max} — ParseParams rejects it
	resp := uiPost(t, client, srv.URL+"/ui/projects/ml/apps/train/sweeps", csrfCk, ck, form)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("bad params sweep: want 303, got %d", resp.StatusCode)
	}
	loc := resp.Header.Get("Location")
	if !strings.Contains(loc, "err=") {
		t.Fatalf("want ?err= redirect, got Location %q", loc)
	}

	a, err := st.GetApp(mustProjectID(t, st, "ml"), "train")
	if err != nil {
		t.Fatal(err)
	}
	sweeps, err := st.ListSweeps(a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(sweeps) != 0 {
		t.Fatalf("bad params must not create a sweep: %+v", sweeps)
	}
}

// TestUISweepCreateOnEjectedAppConflict: an ejected app refuses new sweeps,
// same as it refuses new runs (handleUIRunCreate's errAppEjected path).
func TestUISweepCreateOnEjectedAppConflict(t *testing.T) {
	srv, st, _ := sweepUIServer(t)
	admin := seedUserToken(t, st, "root@b.co", "admin")
	doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"ml"}`).Body.Close()
	doAuthed(t, "POST", srv.URL+"/v1/projects/ml/apps", admin, `{"name":"train","kind":"job"}`).Body.Close()
	doAuthed(t, "POST", srv.URL+"/v1/projects/ml/apps/train/deploy", admin, `{"image":"trainer:1"}`).Body.Close()
	doAuthed(t, "POST", srv.URL+"/v1/projects/ml/apps/train/eject", admin, `{}`).Body.Close()

	u, err := st.GetUserByEmail("root@b.co")
	if err != nil {
		t.Fatal(err)
	}
	ck := uiSessionCookie(t, st, u.ID)
	client := noRedirectClient()
	csrfCk := uiCSRF(t, client, srv.URL)

	resp := uiPost(t, client, srv.URL+"/ui/projects/ml/apps/train/sweeps", csrfCk, ck, sweepStartForm())
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("sweep on ejected app: want 409, got %d", resp.StatusCode)
	}
}

// TestUISweepStopIdempotentAndHighlightsBest drives the full stop path
// through the UI: stopping a running sweep kills its running trial (marked
// pruned, not failed — sweepPruneTrial's pod-level/sweep-level split) and
// flips the sweep to "stopped"; a second stop is a no-op that still
// redirects cleanly. A finished (done, with metric) trial is also seeded so
// the reloaded page's best-trial highlight can be asserted in the same pass.
func TestUISweepStopIdempotentAndHighlightsBest(t *testing.T) {
	srv, st, _ := sweepUIServer(t)
	admin := seedUserToken(t, st, "root@b.co", "admin")
	doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"ml"}`).Body.Close()
	doAuthed(t, "POST", srv.URL+"/v1/projects/ml/apps", admin, `{"name":"train","kind":"job"}`).Body.Close()
	doAuthed(t, "POST", srv.URL+"/v1/projects/ml/apps/train/deploy", admin, `{"image":"trainer:1"}`).Body.Close()

	u, err := st.GetUserByEmail("root@b.co")
	if err != nil {
		t.Fatal(err)
	}
	ck := uiSessionCookie(t, st, u.ID)
	client := noRedirectClient()
	csrfCk := uiCSRF(t, client, srv.URL)

	resp := uiPost(t, client, srv.URL+"/ui/projects/ml/apps/train/sweeps", csrfCk, ck, sweepStartForm())
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("start sweep: want 303, got %d", resp.StatusCode)
	}

	a, err := st.GetApp(mustProjectID(t, st, "ml"), "train")
	if err != nil {
		t.Fatal(err)
	}
	sweeps, err := st.ListSweeps(a.ID)
	if err != nil {
		t.Fatal(err)
	}
	sw := sweeps[0]
	trials, err := st.ListTrials(sw.ID)
	if err != nil || len(trials) != 2 {
		t.Fatalf("trials = %+v, err %v, want 2", trials, err)
	}

	// Simulate the orchestrator having launched one trial and finished it
	// with a metric, so the reloaded page has a best trial to highlight.
	run1, err := st.CreateJobRun(a.ID, 1, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.MarkTrialLaunched(trials[0].ID, run1.ID); err != nil {
		t.Fatal(err)
	}
	val := 0.5
	if err := st.FinishTrial(trials[0].ID, "done", &val, nil); err != nil {
		t.Fatal(err)
	}
	// The other trial is still running when stop is pressed.
	run2, err := st.CreateJobRun(a.ID, 1, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.MarkTrialLaunched(trials[1].ID, run2.ID); err != nil {
		t.Fatal(err)
	}

	stopURL := srv.URL + "/ui/projects/ml/apps/train/sweeps/" + sw.ID + "/stop"
	resp = uiPost(t, client, stopURL, csrfCk, ck, url.Values{})
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("stop sweep: want 303, got %d", resp.StatusCode)
	}

	got, err := st.GetSweep(sw.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "stopped" {
		t.Fatalf("sweep status after stop = %q, want stopped", got.Status)
	}
	trials, err = st.ListTrials(sw.ID)
	if err != nil {
		t.Fatal(err)
	}
	var runningTrial store.SweepTrial
	for _, tr := range trials {
		if tr.ID == trials[1].ID {
			runningTrial = tr
		}
	}
	if runningTrial.State != "pruned" {
		t.Fatalf("stopped sweep's running trial state = %q, want pruned", runningTrial.State)
	}

	// Idempotent: stopping an already-stopped sweep is a clean no-op.
	resp = uiPost(t, client, stopURL, csrfCk, ck, url.Values{})
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("second stop: want 303, got %d", resp.StatusCode)
	}

	status, body := getUIPage(t, client, srv.URL, "/ui/projects/ml/apps/train", ck)
	if status != http.StatusOK {
		t.Fatalf("GET app page: want 200, got %d", status)
	}
	if !strings.Contains(body, `<span class="chip chip-ok">best</span>`) {
		t.Fatalf("app page missing best-trial highlight, got:\n%s", body)
	}
	if strings.Contains(body, `hx-get="/ui/projects/ml/apps/train/sweeps/`+sw.ID+`/trials"`) {
		t.Fatalf("stopped sweep's fragment must not keep polling, got:\n%s", body)
	}
}

// TestUISweepTrialsFragmentPollsOnlyWhileRunning asserts the standalone
// fragment endpoint carries the 15s hx-get re-fetch attribute while the
// sweep is running, and drops it once stopped — htmx's self-terminating
// poll idiom (same as "statuschip").
func TestUISweepTrialsFragmentPollsOnlyWhileRunning(t *testing.T) {
	srv, st, _ := sweepUIServer(t)
	admin := seedUserToken(t, st, "root@b.co", "admin")
	doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"ml"}`).Body.Close()
	doAuthed(t, "POST", srv.URL+"/v1/projects/ml/apps", admin, `{"name":"train","kind":"job"}`).Body.Close()
	doAuthed(t, "POST", srv.URL+"/v1/projects/ml/apps/train/deploy", admin, `{"image":"trainer:1"}`).Body.Close()

	u, err := st.GetUserByEmail("root@b.co")
	if err != nil {
		t.Fatal(err)
	}
	ck := uiSessionCookie(t, st, u.ID)
	client := noRedirectClient()
	csrfCk := uiCSRF(t, client, srv.URL)

	resp := uiPost(t, client, srv.URL+"/ui/projects/ml/apps/train/sweeps", csrfCk, ck, sweepStartForm())
	resp.Body.Close()

	a, err := st.GetApp(mustProjectID(t, st, "ml"), "train")
	if err != nil {
		t.Fatal(err)
	}
	sweeps, err := st.ListSweeps(a.ID)
	if err != nil {
		t.Fatal(err)
	}
	sw := sweeps[0]
	fragURL := "/ui/projects/ml/apps/train/sweeps/" + sw.ID + "/trials"

	status, body := getUIPage(t, client, srv.URL, fragURL, ck)
	if status != http.StatusOK {
		t.Fatalf("GET trials fragment: want 200, got %d", status)
	}
	if !strings.Contains(body, `hx-trigger="every 15s"`) {
		t.Fatalf("running sweep fragment missing 15s poll trigger, got:\n%s", body)
	}

	if err := st.FinishSweep(sw.ID, "stopped"); err != nil {
		t.Fatal(err)
	}
	status, body = getUIPage(t, client, srv.URL, fragURL, ck)
	if status != http.StatusOK {
		t.Fatalf("GET trials fragment (stopped): want 200, got %d", status)
	}
	if strings.Contains(body, `hx-trigger="every 15s"`) {
		t.Fatalf("stopped sweep fragment must not carry the poll trigger, got:\n%s", body)
	}
}

// TestUISweepCardHiddenForNonJobApps: the Sweeps card is job-only, same
// gating as the Runs card.
func TestUISweepCardHiddenForNonJobApps(t *testing.T) {
	srv, st, _ := sweepUIServer(t)
	admin := seedUserToken(t, st, "root@b.co", "admin")
	doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"web"}`).Body.Close()
	doAuthed(t, "POST", srv.URL+"/v1/projects/web/apps", admin, `{"name":"api","port":3000}`).Body.Close()

	u, err := st.GetUserByEmail("root@b.co")
	if err != nil {
		t.Fatal(err)
	}
	ck := uiSessionCookie(t, st, u.ID)
	client := noRedirectClient()

	status, body := getUIPage(t, client, srv.URL, "/ui/projects/web/apps/api", ck)
	if status != http.StatusOK {
		t.Fatalf("GET app page: want 200, got %d", status)
	}
	if strings.Contains(body, "<h2>Sweeps</h2>") {
		t.Fatalf("web app page should not show the Sweeps card, got:\n%s", body)
	}
}
