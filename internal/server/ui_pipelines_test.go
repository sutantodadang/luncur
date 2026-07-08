package server

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/sutantodadang/luncur/internal/store"
)

// pipelineYAML is a minimal single-step "app" pipeline referencing the
// fixture's "train" job app — kind=app leaves its step "running" (job_runs
// row created, never harvested in these tests) so the run itself stays
// "running" too, exactly what the polling-fragment tests need.
const pipelineYAML = "steps:\n  train:\n    app: train\n"

// pipelineCreateForm is the create-form field set for pipelineYAML.
func pipelineCreateForm(name string) url.Values {
	return url.Values{
		"name": {name},
		"yaml": {pipelineYAML},
		"cron": {""},
	}
}

// seedPipelineProject creates project "ml" with a deployed job app "train"
// (so an "app" kind step has something to launch), returning the admin
// session + csrf cookies ready for uiPost/getUIPage.
func seedPipelineProject(t *testing.T) (*httptestServer, *store.Store, *http.Client, *http.Cookie, *http.Cookie) {
	t.Helper()
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
	return srv, st, client, csrfCk, ck
}

// TestUIProjectPageShowsPipelinesCard: the project page lists the Pipelines
// card with its create-form CLI-echo even with zero pipelines yet.
func TestUIProjectPageShowsPipelinesCard(t *testing.T) {
	srv, _, client, _, ck := seedPipelineProject(t)

	status, body := getUIPage(t, client, srv.URL, "/ui/projects/ml", ck)
	if status != http.StatusOK {
		t.Fatalf("GET project page: want 200, got %d", status)
	}
	if !strings.Contains(body, "<h2>Pipelines</h2>") {
		t.Fatalf("project page missing Pipelines card, got:\n%s", body)
	}
	if !strings.Contains(body, "luncur pipeline create") {
		t.Fatalf("project page missing pipeline create CLI echo, got:\n%s", body)
	}
}

// TestUIPipelineCreateFormListsPipelineOnProjectPage exercises the create
// form end to end: submitting it creates a pipeline, and the reloaded
// project page lists it in the Pipelines card table.
func TestUIPipelineCreateFormListsPipelineOnProjectPage(t *testing.T) {
	srv, st, client, csrfCk, ck := seedPipelineProject(t)

	resp := uiPost(t, client, srv.URL+"/ui/projects/ml/pipelines", csrfCk, ck, pipelineCreateForm("build"))
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("create pipeline: want 303, got %d", resp.StatusCode)
	}
	loc := resp.Header.Get("Location")
	if loc != "/ui/projects/ml/pipelines/build" {
		t.Fatalf("create pipeline: want redirect to detail page, got %q", loc)
	}

	p, err := st.GetProject("ml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetPipeline(p.ID, "build"); err != nil {
		t.Fatalf("pipeline not created: %v", err)
	}

	status, body := getUIPage(t, client, srv.URL, "/ui/projects/ml", ck)
	if status != http.StatusOK {
		t.Fatalf("GET project page: want 200, got %d", status)
	}
	if !strings.Contains(body, `href="/ui/projects/ml/pipelines/build"`) {
		t.Fatalf("project page missing pipeline link, got:\n%s", body)
	}
}

// TestUIPipelineDetailPageRendersYAMLCronEngine: the detail page's editor
// textarea, cron field, and engine select reflect the stored pipeline.
func TestUIPipelineDetailPageRendersYAMLCronEngine(t *testing.T) {
	srv, st, client, csrfCk, ck := seedPipelineProject(t)

	form := pipelineCreateForm("build")
	form.Set("cron", "0 3 * * *")
	form.Set("engine", "native")
	uiPost(t, client, srv.URL+"/ui/projects/ml/pipelines", csrfCk, ck, form).Body.Close()

	p, err := st.GetProject("ml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetPipeline(p.ID, "build"); err != nil {
		t.Fatal(err)
	}

	status, body := getUIPage(t, client, srv.URL, "/ui/projects/ml/pipelines/build", ck)
	if status != http.StatusOK {
		t.Fatalf("GET pipeline detail: want 200, got %d", status)
	}
	if !strings.Contains(body, "app: train") {
		t.Fatalf("detail page missing yaml in editor, got:\n%s", body)
	}
	if !strings.Contains(body, `value="0 3 * * *"`) {
		t.Fatalf("detail page missing cron value, got:\n%s", body)
	}
	if !strings.Contains(body, `<option value="native" selected>native</option>`) {
		t.Fatalf("detail page missing selected native engine, got:\n%s", body)
	}
}

// TestUIPipelineRunCreatesRunAndRedirectsWithPollingFragment covers the run
// button (fixture kube launches the "app" step, leaving it — and the run —
// "running") plus the steps fragment's topo rows, chips, and 15s poll
// attribute while running.
func TestUIPipelineRunCreatesRunAndRedirectsWithPollingFragment(t *testing.T) {
	srv, st, client, csrfCk, ck := seedPipelineProject(t)
	uiPost(t, client, srv.URL+"/ui/projects/ml/pipelines", csrfCk, ck, pipelineCreateForm("build")).Body.Close()

	resp := uiPost(t, client, srv.URL+"/ui/projects/ml/pipelines/build/run", csrfCk, ck, url.Values{})
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("run pipeline: want 303, got %d", resp.StatusCode)
	}

	p, err := st.GetProject("ml")
	if err != nil {
		t.Fatal(err)
	}
	pl, err := st.GetPipeline(p.ID, "build")
	if err != nil {
		t.Fatal(err)
	}
	runs, err := st.ListPipelineRuns(pl.ID)
	if err != nil || len(runs) != 1 {
		t.Fatalf("runs = %+v, err %v, want 1", runs, err)
	}
	run := runs[0]
	if run.Status != "running" {
		t.Fatalf("run status = %q, want running", run.Status)
	}

	// Detail page embeds the current run's step table with the poll trigger.
	status, body := getUIPage(t, client, srv.URL, "/ui/projects/ml/pipelines/build", ck)
	if status != http.StatusOK {
		t.Fatalf("GET pipeline detail: want 200, got %d", status)
	}
	if !strings.Contains(body, `<td class="font-mono text-xs">train</td>`) {
		t.Fatalf("detail page missing step row for step \"train\", got:\n%s", body)
	}
	if !strings.Contains(body, `class="chip chip-warn">running`) {
		t.Fatalf("detail page missing running step chip, got:\n%s", body)
	}
	if !strings.Contains(body, `hx-get="/ui/projects/ml/pipelines/build/runs/`+run.ID+`/steps"`) {
		t.Fatalf("detail page missing 15s poll fragment for the running run, got:\n%s", body)
	}

	// The standalone fragment endpoint carries the same poll trigger...
	fragURL := "/ui/projects/ml/pipelines/build/runs/" + run.ID + "/steps"
	status, body = getUIPage(t, client, srv.URL, fragURL, ck)
	if status != http.StatusOK {
		t.Fatalf("GET steps fragment: want 200, got %d", status)
	}
	if !strings.Contains(body, `hx-trigger="every 15s"`) {
		t.Fatalf("running run fragment missing 15s poll trigger, got:\n%s", body)
	}

	// ...and drops it once the run finishes.
	if err := st.FinishPipelineRun(run.ID, "done"); err != nil {
		t.Fatal(err)
	}
	status, body = getUIPage(t, client, srv.URL, fragURL, ck)
	if status != http.StatusOK {
		t.Fatalf("GET steps fragment (done): want 200, got %d", status)
	}
	if strings.Contains(body, `hx-trigger="every 15s"`) {
		t.Fatalf("done run fragment must not keep polling, got:\n%s", body)
	}
}

// TestUIPipelineRunStopIdempotent drives the stop path: stopping a running
// run finishes it "stopped", and a second stop on the same run is a clean
// no-op redirect (B2 stopSweep convention).
func TestUIPipelineRunStopIdempotent(t *testing.T) {
	srv, st, client, csrfCk, ck := seedPipelineProject(t)
	uiPost(t, client, srv.URL+"/ui/projects/ml/pipelines", csrfCk, ck, pipelineCreateForm("build")).Body.Close()
	uiPost(t, client, srv.URL+"/ui/projects/ml/pipelines/build/run", csrfCk, ck, url.Values{}).Body.Close()

	p, err := st.GetProject("ml")
	if err != nil {
		t.Fatal(err)
	}
	pl, err := st.GetPipeline(p.ID, "build")
	if err != nil {
		t.Fatal(err)
	}
	runs, err := st.ListPipelineRuns(pl.ID)
	if err != nil || len(runs) != 1 {
		t.Fatalf("runs = %+v, err %v, want 1", runs, err)
	}
	run := runs[0]

	stopURL := srv.URL + "/ui/projects/ml/pipelines/build/runs/" + run.ID + "/stop"
	resp := uiPost(t, client, stopURL, csrfCk, ck, url.Values{})
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("stop run: want 303, got %d", resp.StatusCode)
	}
	got, err := st.GetPipelineRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "stopped" {
		t.Fatalf("run status after stop = %q, want stopped", got.Status)
	}

	// Idempotent: stopping an already-stopped run is a clean no-op.
	resp = uiPost(t, client, stopURL, csrfCk, ck, url.Values{})
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("second stop: want 303, got %d", resp.StatusCode)
	}
}

// TestUIPipelineWebhookRotateShowsSecretOnceOnly: the rotate fragment
// carries the secret in its own response, but the reloaded detail page never
// re-renders it.
func TestUIPipelineWebhookRotateShowsSecretOnceOnly(t *testing.T) {
	srv, _, client, csrfCk, ck := seedPipelineProject(t)
	uiPost(t, client, srv.URL+"/ui/projects/ml/pipelines", csrfCk, ck, pipelineCreateForm("build")).Body.Close()

	resp := uiPost(t, client, srv.URL+"/ui/projects/ml/pipelines/build/webhook-secret", csrfCk, ck, url.Values{})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("rotate webhook: want 200, got %d", resp.StatusCode)
	}
	buf := make([]byte, 4096)
	n, _ := resp.Body.Read(buf)
	fragment := string(buf[:n])
	if !strings.Contains(fragment, "/hooks/pipelines/") {
		t.Fatalf("rotate fragment missing webhook URL, got:\n%s", fragment)
	}

	status, body := getUIPage(t, client, srv.URL, "/ui/projects/ml/pipelines/build", ck)
	if status != http.StatusOK {
		t.Fatalf("GET pipeline detail: want 200, got %d", status)
	}
	if strings.Contains(body, "secret shown once") {
		t.Fatalf("detail page must not re-render the once-only secret fragment, got:\n%s", body)
	}
}

// TestUIPipelineDetailNonMemberNotFound: a non-member gets the same 404 as a
// nonexistent project (uiProject's leak-nothing policy), same as every other
// project-scoped UI page.
func TestUIPipelineDetailNonMemberNotFound(t *testing.T) {
	srv, st, client, csrfCk, ck := seedPipelineProject(t)
	uiPost(t, client, srv.URL+"/ui/projects/ml/pipelines", csrfCk, ck, pipelineCreateForm("build")).Body.Close()

	outsider := seedUserToken(t, st, "outsider@b.co", "member")
	u, err := st.GetUserByEmail("outsider@b.co")
	if err != nil {
		t.Fatal(err)
	}
	outsiderCk := uiSessionCookie(t, st, u.ID)
	_ = outsider

	status, _ := getUIPage(t, client, srv.URL, "/ui/projects/ml/pipelines/build", outsiderCk)
	if status != http.StatusNotFound {
		t.Fatalf("non-member GET pipeline detail: want 404, got %d", status)
	}
}
