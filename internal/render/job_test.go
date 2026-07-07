package render

import (
	"encoding/json"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
)

func TestRenderJobDeployPathRendersNoWorkload(t *testing.T) {
	r, err := Render(Input{
		AppName: "train", Namespace: "ns", Image: "img:1", Kind: "job",
	}, map[string]string{"K": "v"})
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range r.Objects {
		if o.Kind != "Secret" && o.Kind != "PersistentVolumeClaim" {
			t.Fatalf("deploy render must only emit Secret/PVC, got %s", o.Kind)
		}
	}
}

func TestRenderJobRun(t *testing.T) {
	r, err := Render(Input{
		AppName: "train", Namespace: "ns", Image: "img:1", Kind: "job",
		RunName: "train-run-7", GPU: 1,
	}, map[string]string{"K": "v"})
	if err != nil {
		t.Fatal(err)
	}
	var job *batchv1.Job
	for _, o := range r.Objects {
		if o.Kind == "Job" {
			job = &batchv1.Job{}
			if err := json.Unmarshal(o.JSON, job); err != nil {
				t.Fatal(err)
			}
		}
	}
	if job == nil {
		t.Fatal("no Job rendered")
	}
	if job.Name != "train-run-7" {
		t.Fatalf("job name = %s", job.Name)
	}
	if job.Spec.BackoffLimit == nil || *job.Spec.BackoffLimit != 0 {
		t.Fatalf("backoffLimit = %v, want 0", job.Spec.BackoffLimit)
	}
	if job.Spec.TTLSecondsAfterFinished == nil || *job.Spec.TTLSecondsAfterFinished != 86400 {
		t.Fatalf("ttl = %v, want 86400", job.Spec.TTLSecondsAfterFinished)
	}
	spec := job.Spec.Template.Spec
	if spec.RestartPolicy != corev1.RestartPolicyNever {
		t.Fatalf("restartPolicy = %s, want Never", spec.RestartPolicy)
	}
	if spec.RuntimeClassName == nil || *spec.RuntimeClassName != GPURuntimeClass {
		t.Fatalf("runtimeClassName = %v, want nvidia", spec.RuntimeClassName)
	}
	if q := spec.Containers[0].Resources.Limits[GPUResource]; q.Value() != 1 {
		t.Fatalf("gpu limit = %v, want 1", q.Value())
	}
	// Env flows in via the app Secret.
	if len(spec.Containers[0].EnvFrom) != 1 {
		t.Fatalf("envFrom = %+v", spec.Containers[0].EnvFrom)
	}
}

func TestRenderJobRejectsUnknownKindStill(t *testing.T) {
	if _, err := Render(Input{AppName: "x", Namespace: "n", Image: "i", Kind: "nope"}, nil); err == nil {
		t.Fatal("unknown kind must error")
	}
}
