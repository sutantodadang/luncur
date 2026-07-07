package render

import (
	"encoding/json"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
)

func gpuInput(kind string) Input {
	in := Input{
		AppName: "ml", Namespace: "ns", Image: "img:1", Kind: kind,
		Replicas: 1, GPU: 2,
	}
	if kind == "web" {
		in.Host, in.Port = "ml.example.com", 8080
	}
	if kind == "cron" {
		in.Schedule = "0 3 * * *"
	}
	return in
}

func findDeployment(t *testing.T, r Rendered) appsv1.Deployment {
	t.Helper()
	for _, o := range r.Objects {
		if o.Kind == "Deployment" {
			var d appsv1.Deployment
			if err := json.Unmarshal(o.JSON, &d); err != nil {
				t.Fatal(err)
			}
			return d
		}
	}
	t.Fatal("no Deployment rendered")
	return appsv1.Deployment{}
}

func TestRenderGPUDeployment(t *testing.T) {
	for _, kind := range []string{"web", "worker"} {
		r, err := Render(gpuInput(kind), nil)
		if err != nil {
			t.Fatalf("%s: %v", kind, err)
		}
		d := findDeployment(t, r)
		spec := d.Spec.Template.Spec
		if spec.RuntimeClassName == nil || *spec.RuntimeClassName != GPURuntimeClass {
			t.Fatalf("%s: runtimeClassName = %v, want nvidia", kind, spec.RuntimeClassName)
		}
		if spec.NodeSelector[GPUNodeLabelKey] != GPUNodeLabelValue {
			t.Fatalf("%s: nodeSelector = %v", kind, spec.NodeSelector)
		}
		res := spec.Containers[0].Resources
		q, ok := res.Limits[GPUResource]
		if !ok || q.Value() != 2 {
			t.Fatalf("%s: gpu limit = %v (present %v), want 2", kind, q.Value(), ok)
		}
		if q := res.Requests[GPUResource]; q.Value() != 2 {
			t.Fatalf("%s: gpu request = %v, want 2", kind, q.Value())
		}
	}
}

func TestRenderGPUCron(t *testing.T) {
	r, err := Render(gpuInput("cron"), nil)
	if err != nil {
		t.Fatal(err)
	}
	var cj batchv1.CronJob
	for _, o := range r.Objects {
		if o.Kind == "CronJob" {
			if err := json.Unmarshal(o.JSON, &cj); err != nil {
				t.Fatal(err)
			}
		}
	}
	spec := cj.Spec.JobTemplate.Spec.Template.Spec
	if spec.RuntimeClassName == nil || *spec.RuntimeClassName != GPURuntimeClass {
		t.Fatalf("cron runtimeClassName = %v, want nvidia", spec.RuntimeClassName)
	}
	if spec.NodeSelector[GPUNodeLabelKey] != GPUNodeLabelValue {
		t.Fatalf("cron nodeSelector = %v", spec.NodeSelector)
	}
	if q := spec.Containers[0].Resources.Limits[GPUResource]; q.Value() != 2 {
		t.Fatalf("cron gpu limit = %v, want 2", q.Value())
	}
}

func TestRenderNoGPUByDefault(t *testing.T) {
	in := gpuInput("web")
	in.GPU = 0
	r, err := Render(in, nil)
	if err != nil {
		t.Fatal(err)
	}
	d := findDeployment(t, r)
	spec := d.Spec.Template.Spec
	if spec.RuntimeClassName != nil {
		t.Fatalf("runtimeClassName should be unset, got %v", *spec.RuntimeClassName)
	}
	if len(spec.NodeSelector) != 0 {
		t.Fatalf("nodeSelector should be empty, got %v", spec.NodeSelector)
	}
	if _, ok := spec.Containers[0].Resources.Limits[GPUResource]; ok {
		t.Fatal("gpu limit should be absent")
	}
}
