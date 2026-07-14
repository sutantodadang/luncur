package server

import (
	"encoding/json"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestCronPauseResumeKindMismatch covers the four cron-controls endpoints
// rejecting non-cron apps with kind_mismatch, and the pause/resume happy
// path: the flag persists in the store and (once the app has a live
// deployment) re-applying re-patches the CronJob.
func TestCronPauseResumeKindMismatch(t *testing.T) {
	srv, st, actions := kubeServer(t)
	admin := seedUserToken(t, st, "root@b.co", "admin")
	doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"web"}`).Body.Close()
	doAuthed(t, "POST", srv.URL+"/v1/projects/web/apps", admin, `{"name":"nightly","kind":"cron","schedule":"0 3 * * *"}`).Body.Close()
	doAuthed(t, "POST", srv.URL+"/v1/projects/web/apps", admin, `{"name":"api","port":3000}`).Body.Close()

	// Non-cron app -> kind_mismatch on every cron-controls endpoint.
	for _, action := range []string{"pause", "resume", "trigger"} {
		resp := doAuthed(t, "POST", srv.URL+"/v1/projects/web/apps/api/"+action, admin, "")
		body := mustReadBody(t, resp)
		if resp.StatusCode != 400 || errCode(t, body) != "kind_mismatch" {
			t.Fatalf("%s on non-cron app: want 400 kind_mismatch, got %d (%s)", action, resp.StatusCode, body)
		}
	}
	resp := doAuthed(t, "GET", srv.URL+"/v1/projects/web/apps/api/cron-runs", admin, "")
	body := mustReadBody(t, resp)
	if resp.StatusCode != 400 || errCode(t, body) != "kind_mismatch" {
		t.Fatalf("cron-runs on non-cron app: want 400 kind_mismatch, got %d (%s)", resp.StatusCode, body)
	}

	p, err := st.GetProject("web")
	if err != nil {
		t.Fatal(err)
	}

	// Pause before any deploy: persists, nothing live to re-apply against.
	resp = doAuthed(t, "POST", srv.URL+"/v1/projects/web/apps/nightly/pause", admin, "")
	body = mustReadBody(t, resp)
	if resp.StatusCode != 200 {
		t.Fatalf("pause: want 200, got %d (%s)", resp.StatusCode, body)
	}
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatal(err)
	}
	if out["suspended"] != true {
		t.Fatalf("pause response: %+v", out)
	}
	a, err := st.GetApp(p.ID, "nightly")
	if err != nil || !a.Suspended {
		t.Fatalf("app after pause: %+v %v", a, err)
	}

	// Deploy: applies the CronJob with Suspend=true (Suspended plumbed
	// through render.Input).
	resp = doAuthed(t, "POST", srv.URL+"/v1/projects/web/apps/nightly/deploy", admin, `{"image":"registry/nightly:1"}`)
	if resp.StatusCode != 200 {
		t.Fatalf("deploy: want 200, got %d (%s)", resp.StatusCode, mustReadBody(t, resp))
	}
	resp.Body.Close()
	if !strings.Contains(strings.Join(*actions, ","), "patch cronjobs") {
		t.Fatalf("deploy did not apply cronjob: %v", *actions)
	}
	*actions = nil

	// Resume: persists, and re-applies now that the app is deployed.
	resp = doAuthed(t, "POST", srv.URL+"/v1/projects/web/apps/nightly/resume", admin, "")
	body = mustReadBody(t, resp)
	if resp.StatusCode != 200 {
		t.Fatalf("resume: want 200, got %d (%s)", resp.StatusCode, body)
	}
	out = nil
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatal(err)
	}
	if out["suspended"] != false {
		t.Fatalf("resume response: %+v", out)
	}
	if !strings.Contains(strings.Join(*actions, ","), "patch cronjobs") {
		t.Fatalf("resume did not re-apply cronjob: %v", *actions)
	}
	a, err = st.GetApp(p.ID, "nightly")
	if err != nil || a.Suspended {
		t.Fatalf("app after resume: %+v %v", a, err)
	}
}

// TestCronTriggerNotDeployed covers trigger's 409 not_deployed guard: no
// live deployment yet, so there is no CronJob to build a manual run from.
func TestCronTriggerNotDeployed(t *testing.T) {
	srv, st, _ := kubeServer(t)
	admin := seedUserToken(t, st, "root@b.co", "admin")
	doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"web"}`).Body.Close()
	doAuthed(t, "POST", srv.URL+"/v1/projects/web/apps", admin, `{"name":"nightly","kind":"cron","schedule":"0 3 * * *"}`).Body.Close()

	resp := doAuthed(t, "POST", srv.URL+"/v1/projects/web/apps/nightly/trigger", admin, "")
	body := mustReadBody(t, resp)
	if resp.StatusCode != 409 || errCode(t, body) != "not_deployed" {
		t.Fatalf("trigger before deploy: want 409 not_deployed, got %d (%s)", resp.StatusCode, body)
	}
}

// nightlyCronJob builds the CronJob object render+kube.Apply would have
// produced for a deployed "nightly" cron app, for pre-seeding the fake typed
// clientset TriggerCronJob/CronRuns read from (the dynamic-client deploy
// path above and the typed-clientset fake here are separate fake trackers,
// so the object has to be seeded directly — same pattern as
// kubeServerWithPods pre-seeding pods for RunningJobPods/DeleteJob).
func nightlyCronJob(namespace string) *batchv1.CronJob {
	labels := map[string]string{"app.kubernetes.io/name": "nightly", "app.kubernetes.io/managed-by": "luncur"}
	return &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{Name: "nightly", Namespace: namespace, UID: "nightly-uid"},
		Spec: batchv1.CronJobSpec{
			Schedule: "0 3 * * *",
			JobTemplate: batchv1.JobTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: batchv1.JobSpec{
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{Labels: labels},
						Spec: corev1.PodSpec{
							RestartPolicy: corev1.RestartPolicyOnFailure,
							Containers:    []corev1.Container{{Name: "app", Image: "registry/nightly:1"}},
						},
					},
				},
			},
		},
	}
}

// TestCronTriggerAndRuns covers the manual "run now" + run-history endpoints
// end to end against a typed-clientset fake pre-seeded with the live
// CronJob: trigger creates a Job owned by the CronJob, and cron-runs lists
// it back.
func TestCronTriggerAndRuns(t *testing.T) {
	srv, st, _ := kubeServerWithPods(t, nightlyCronJob("luncur-ml"))
	admin := seedUserToken(t, st, "root@b.co", "admin")
	doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"ml"}`).Body.Close()
	doAuthed(t, "POST", srv.URL+"/v1/projects/ml/apps", admin, `{"name":"nightly","kind":"cron","schedule":"0 3 * * *"}`).Body.Close()
	doAuthed(t, "POST", srv.URL+"/v1/projects/ml/apps/nightly/deploy", admin, `{"image":"registry/nightly:1"}`).Body.Close()

	// No runs yet.
	resp := doAuthed(t, "GET", srv.URL+"/v1/projects/ml/apps/nightly/cron-runs", admin, "")
	var runs []map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&runs); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if len(runs) != 0 {
		t.Fatalf("cron-runs before trigger: %+v", runs)
	}

	// Trigger a manual run.
	resp = doAuthed(t, "POST", srv.URL+"/v1/projects/ml/apps/nightly/trigger", admin, "")
	body := mustReadBody(t, resp)
	if resp.StatusCode != 202 {
		t.Fatalf("trigger: want 202, got %d (%s)", resp.StatusCode, body)
	}
	var triggered map[string]any
	if err := json.Unmarshal(body, &triggered); err != nil {
		t.Fatal(err)
	}
	jobName, _ := triggered["job"].(string)
	if !strings.HasPrefix(jobName, "nightly-manual-") {
		t.Fatalf("trigger response: %+v", triggered)
	}

	// cron-runs now lists it.
	resp = doAuthed(t, "GET", srv.URL+"/v1/projects/ml/apps/nightly/cron-runs", admin, "")
	if err := json.NewDecoder(resp.Body).Decode(&runs); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if len(runs) != 1 || runs[0]["name"] != jobName || runs[0]["status"] != "active" {
		t.Fatalf("cron-runs after trigger: %+v", runs)
	}
}
