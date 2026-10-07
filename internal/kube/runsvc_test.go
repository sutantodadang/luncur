package kube

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

func svcObj(name, clusterIP string, owned bool) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "Service",
		"metadata": map[string]any{"name": name, "namespace": "ns",
			"labels": map[string]any{"app.kubernetes.io/managed-by": "luncur"}},
		"spec": map[string]any{"clusterIP": clusterIP},
	}}
	if owned {
		u.SetOwnerReferences([]metav1.OwnerReference{{APIVersion: "batch/v1", Kind: "Job", Name: name, UID: "u"}})
	}
	return u
}

func jobObj(name string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "batch/v1", "kind": "Job",
		"metadata": map[string]any{"name": name, "namespace": "ns", "uid": "job-uid"},
	}}
}

// S2: leaked run Services (no owner, Job gone) are deleted; live runs,
// owned Services and ordinary Services are kept.
func TestDeleteOrphanRunServicesAndOwnByJob(t *testing.T) {
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		gvrByKind["Service"]: "ServiceList", gvrByKind["Job"]: "JobList",
	},
		svcObj("train-run-1", "None", false), // leaked
		svcObj("train-run-2", "None", false), jobObj("train-run-2"), // live run
		svcObj("train-run-3", "None", true), // owned: GC handles it
		svcObj("web", "10.0.0.1", false),    // ordinary app Service
	)
	c := NewFromDynamic(dyn)
	n, err := c.DeleteOrphanRunServices(context.Background(), "ns")
	if err != nil || n != 1 {
		t.Fatalf("deleted %d (%v), want 1", n, err)
	}
	if _, err := dyn.Resource(gvrByKind["Service"]).Namespace("ns").Get(context.Background(), "train-run-1", metav1.GetOptions{}); err == nil {
		t.Fatal("leaked service still there")
	}

	if err := c.OwnByJob(context.Background(), "ns", "Service", "train-run-2", "train-run-2"); err != nil {
		t.Fatal(err)
	}
	got, _ := dyn.Resource(gvrByKind["Service"]).Namespace("ns").Get(context.Background(), "train-run-2", metav1.GetOptions{})
	if refs := got.GetOwnerReferences(); len(refs) != 1 || refs[0].Kind != "Job" || refs[0].UID != "job-uid" {
		t.Fatalf("owner refs = %+v", refs)
	}
}
