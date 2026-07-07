package render

import (
	"encoding/json"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
)

func renderTrain(t *testing.T, nodes int32, framework string) (batchv1.Job, corev1.Service, bool) {
	t.Helper()
	in := Input{
		AppName: "train", Namespace: "ns", Image: "img:1",
		Kind: "job", RunName: "train-run-7", GPU: 1,
		Nodes: nodes, Framework: framework,
	}
	out, err := Render(in, nil)
	if err != nil {
		t.Fatal(err)
	}
	var job batchv1.Job
	var svc corev1.Service
	var hasSvc bool
	for _, o := range out.Objects {
		switch o.Kind {
		case "Job":
			if err := json.Unmarshal(o.JSON, &job); err != nil {
				t.Fatal(err)
			}
		case "Service":
			if err := json.Unmarshal(o.JSON, &svc); err != nil {
				t.Fatal(err)
			}
			hasSvc = true
		}
	}
	return job, svc, hasSvc
}

func TestRenderSingleNodeRunUnchanged(t *testing.T) {
	job, _, hasSvc := renderTrain(t, 1, "")
	if hasSvc {
		t.Fatal("single-node run must not render a Service")
	}
	if job.Spec.CompletionMode != nil {
		t.Fatal("single-node run must not set completionMode")
	}
	if job.Spec.Completions != nil || job.Spec.Parallelism != nil {
		t.Fatal("single-node run must not set completions/parallelism")
	}
	for _, e := range job.Spec.Template.Spec.Containers[0].Env {
		if strings.HasPrefix(e.Name, "LUNCUR_NODE") || strings.HasPrefix(e.Name, "LUNCUR_MASTER") {
			t.Fatalf("single-node run leaked contract env %s", e.Name)
		}
	}
}

func TestRenderMultiNodeIndexedJob(t *testing.T) {
	job, svc, hasSvc := renderTrain(t, 3, "")
	if job.Spec.CompletionMode == nil || *job.Spec.CompletionMode != batchv1.IndexedCompletion {
		t.Fatal("want completionMode Indexed")
	}
	if job.Spec.Completions == nil || *job.Spec.Completions != 3 {
		t.Fatal("want completions=3")
	}
	if job.Spec.Parallelism == nil || *job.Spec.Parallelism != 3 {
		t.Fatal("want parallelism=3")
	}
	if job.Spec.BackoffLimit == nil || *job.Spec.BackoffLimit != 0 {
		t.Fatal("want backoffLimit=0")
	}
	if job.Spec.Template.Spec.Subdomain != "train-run-7" {
		t.Fatalf("want subdomain=train-run-7, got %q", job.Spec.Template.Spec.Subdomain)
	}
	if !hasSvc {
		t.Fatal("multi-node run must render the headless Service")
	}
	if svc.Name != "train-run-7" {
		t.Fatalf("svc name %q, want train-run-7", svc.Name)
	}
	if svc.Spec.ClusterIP != corev1.ClusterIPNone {
		t.Fatal("service must be headless (clusterIP None)")
	}
	if !svc.Spec.PublishNotReadyAddresses {
		t.Fatal("headless svc must publish not-ready addresses (rendezvous races readiness)")
	}
	if svc.Spec.Selector["batch.kubernetes.io/job-name"] != "train-run-7" {
		t.Fatalf("svc selector %v, want job-name=train-run-7", svc.Spec.Selector)
	}

	env := map[string]corev1.EnvVar{}
	for _, e := range job.Spec.Template.Spec.Containers[0].Env {
		env[e.Name] = e
	}
	if env["LUNCUR_NUM_NODES"].Value != "3" {
		t.Fatalf("LUNCUR_NUM_NODES=%q, want 3", env["LUNCUR_NUM_NODES"].Value)
	}
	wantMaster := "train-run-7-0.train-run-7.ns"
	if env["LUNCUR_MASTER_ADDR"].Value != wantMaster {
		t.Fatalf("LUNCUR_MASTER_ADDR=%q, want %s", env["LUNCUR_MASTER_ADDR"].Value, wantMaster)
	}
	if env["LUNCUR_MASTER_PORT"].Value != "29500" {
		t.Fatal("LUNCUR_MASTER_PORT missing")
	}
	rank := env["LUNCUR_NODE_RANK"]
	if rank.ValueFrom == nil || rank.ValueFrom.FieldRef == nil ||
		rank.ValueFrom.FieldRef.FieldPath != "metadata.annotations['batch.kubernetes.io/job-completion-index']" {
		t.Fatalf("LUNCUR_NODE_RANK must fieldRef the completion-index annotation, got %+v", rank)
	}
}

func TestRenderFrameworkPresets(t *testing.T) {
	job, _, _ := renderTrain(t, 2, "torchrun")
	env := map[string]corev1.EnvVar{}
	for _, e := range job.Spec.Template.Spec.Containers[0].Env {
		env[e.Name] = e
	}
	if env["PET_NNODES"].Value != "2" || env["PET_RDZV_BACKEND"].Value != "c10d" {
		t.Fatalf("torchrun preset env wrong: %+v", env)
	}
	if env["PET_RDZV_ENDPOINT"].Value != "train-run-7-0.train-run-7.ns:29500" {
		t.Fatalf("PET_RDZV_ENDPOINT=%q", env["PET_RDZV_ENDPOINT"].Value)
	}
	if env["PET_NODE_RANK"].ValueFrom == nil {
		t.Fatal("PET_NODE_RANK must be a fieldRef")
	}

	job, _, _ = renderTrain(t, 2, "torch")
	env = map[string]corev1.EnvVar{}
	for _, e := range job.Spec.Template.Spec.Containers[0].Env {
		env[e.Name] = e
	}
	if env["MASTER_ADDR"].Value == "" || env["MASTER_PORT"].Value != "29500" ||
		env["WORLD_SIZE"].Value != "2" || env["NNODES"].Value != "2" {
		t.Fatalf("torch preset env wrong: %+v", env)
	}
	if env["RANK"].ValueFrom == nil || env["NODE_RANK"].ValueFrom == nil {
		t.Fatal("RANK/NODE_RANK must be fieldRefs")
	}
}

func TestRenderUnknownFramework(t *testing.T) {
	in := Input{AppName: "t", Namespace: "ns", Image: "i", Kind: "job",
		RunName: "t-run-1", Nodes: 2, Framework: "mpi"}
	if _, err := Render(in, nil); err == nil {
		t.Fatal("unknown framework accepted")
	}
}

func TestRenderRunEnv(t *testing.T) {
	in := Input{AppName: "t", Namespace: "ns", Image: "i", Kind: "job",
		RunName: "t-run-1", Nodes: 1,
		RunEnv: map[string]string{"LUNCUR_PARAM_LR": "0.01", "LUNCUR_TRIAL_ID": "abc"}}
	out, err := Render(in, nil)
	if err != nil {
		t.Fatal(err)
	}
	var job batchv1.Job
	for _, o := range out.Objects {
		if o.Kind == "Job" {
			json.Unmarshal(o.JSON, &job)
		}
	}
	env := job.Spec.Template.Spec.Containers[0].Env
	// sorted by key: LUNCUR_PARAM_LR then LUNCUR_TRIAL_ID
	if len(env) != 2 || env[0].Name != "LUNCUR_PARAM_LR" || env[1].Name != "LUNCUR_TRIAL_ID" {
		t.Fatalf("RunEnv not rendered sorted: %+v", env)
	}
}
