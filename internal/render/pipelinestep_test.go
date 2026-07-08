package render

import (
	"encoding/json"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
)

func decodeJob(t *testing.T, objs []Object) *batchv1.Job {
	t.Helper()
	for _, o := range objs {
		if o.Kind == "Job" {
			job := &batchv1.Job{}
			if err := json.Unmarshal(o.JSON, job); err != nil {
				t.Fatal(err)
			}
			return job
		}
	}
	t.Fatal("no Job rendered")
	return nil
}

func TestPipelineStepJobNameNamespaceLabels(t *testing.T) {
	objs := PipelineStepJob("ns", "abc123defghi", "train", 1, "img:1", nil, nil, 0)
	job := decodeJob(t, objs)
	if want := "pl-abc123defghi-train-a1"; job.Name != want {
		t.Fatalf("job name = %s, want %s", job.Name, want)
	}
	if job.Namespace != "ns" {
		t.Fatalf("namespace = %s, want ns", job.Namespace)
	}
	if job.Labels["app.kubernetes.io/managed-by"] != "luncur" {
		t.Fatalf("labels missing managed-by luncur: %+v", job.Labels)
	}
	if job.Labels["luncur.dev/pipeline-run"] != "abc123defghi" {
		t.Fatalf("labels missing pipeline-run label: %+v", job.Labels)
	}
	if job.Spec.Template.Labels["luncur.dev/pipeline-run"] != "abc123defghi" {
		t.Fatalf("pod template labels missing pipeline-run label: %+v", job.Spec.Template.Labels)
	}
}

func TestPipelineStepJobHardening(t *testing.T) {
	objs := PipelineStepJob("ns", "run1", "train", 1, "img:1", nil, nil, 0)
	job := decodeJob(t, objs)
	if job.Spec.BackoffLimit == nil || *job.Spec.BackoffLimit != 0 {
		t.Fatalf("backoffLimit = %v, want 0", job.Spec.BackoffLimit)
	}
	if job.Spec.Template.Spec.RestartPolicy != corev1.RestartPolicyNever {
		t.Fatalf("restartPolicy = %s, want Never", job.Spec.Template.Spec.RestartPolicy)
	}
}

func TestPipelineStepJobEnvSortedComplete(t *testing.T) {
	env := map[string]string{"B": "2", "A": "1", "C": "3"}
	objs := PipelineStepJob("ns", "run1", "train", 1, "img:1", nil, env, 0)
	job := decodeJob(t, objs)
	got := job.Spec.Template.Spec.Containers[0].Env
	if len(got) != 3 {
		t.Fatalf("env len = %d, want 3", len(got))
	}
	wantOrder := []string{"A", "B", "C"}
	for i, k := range wantOrder {
		if got[i].Name != k {
			t.Fatalf("env[%d].Name = %s, want %s (env must be sorted)", i, got[i].Name, k)
		}
	}
	wantVal := map[string]string{"A": "1", "B": "2", "C": "3"}
	for _, e := range got {
		if e.Value != wantVal[e.Name] {
			t.Fatalf("env %s = %s, want %s", e.Name, e.Value, wantVal[e.Name])
		}
	}
}

func TestPipelineStepJobCommand(t *testing.T) {
	objs := PipelineStepJob("ns", "run1", "train", 1, "img:1", []string{"python", "train.py"}, nil, 0)
	job := decodeJob(t, objs)
	got := job.Spec.Template.Spec.Containers[0].Command
	if len(got) != 2 || got[0] != "python" || got[1] != "train.py" {
		t.Fatalf("command = %v, want [python train.py]", got)
	}
}

func TestPipelineStepJobNoCommandRunsEntrypoint(t *testing.T) {
	objs := PipelineStepJob("ns", "run1", "train", 1, "img:1", nil, nil, 0)
	job := decodeJob(t, objs)
	got := job.Spec.Template.Spec.Containers[0].Command
	if len(got) != 0 {
		t.Fatalf("command = %v, want empty (image entrypoint)", got)
	}
}

func TestPipelineStepJobGPU(t *testing.T) {
	objs := PipelineStepJob("ns", "run1", "train", 1, "img:1", nil, nil, 2)
	job := decodeJob(t, objs)
	spec := job.Spec.Template.Spec
	if spec.RuntimeClassName == nil || *spec.RuntimeClassName != GPURuntimeClass {
		t.Fatalf("runtimeClassName = %v, want %s", spec.RuntimeClassName, GPURuntimeClass)
	}
	if spec.NodeSelector[GPUNodeLabelKey] != GPUNodeLabelValue {
		t.Fatalf("nodeSelector = %v, want %s=%s", spec.NodeSelector, GPUNodeLabelKey, GPUNodeLabelValue)
	}
	if q := spec.Containers[0].Resources.Requests[GPUResource]; q.Value() != 2 {
		t.Fatalf("gpu requests = %v, want 2", q.Value())
	}
	if q := spec.Containers[0].Resources.Limits[GPUResource]; q.Value() != 2 {
		t.Fatalf("gpu limits = %v, want 2", q.Value())
	}
}

func TestPipelineStepJobAttemptInName(t *testing.T) {
	objs := PipelineStepJob("ns", "run1", "train", 3, "img:1", nil, nil, 0)
	job := decodeJob(t, objs)
	if want := "pl-run1-train-a3"; job.Name != want {
		t.Fatalf("job name = %s, want %s", job.Name, want)
	}
}
