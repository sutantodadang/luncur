package kube

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	ktesting "k8s.io/client-go/testing"
)

// A multi-node run is an Indexed Job with completions=N: it isn't done when
// the first worker exits 0 while the others are still running.
func TestWaitJobWaitsForAllCompletions(t *testing.T) {
	job := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "batch/v1", "kind": "Job",
		"metadata": map[string]any{"name": "train-run-1", "namespace": "ns"},
		"spec":     map[string]any{"completions": int64(4), "parallelism": int64(4), "completionMode": "Indexed"},
		"status":   map[string]any{"succeeded": int64(1), "active": int64(3)},
	}}
	c := NewForTest(dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(), job), nil)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	ok, err := c.WaitJob(ctx, "ns", "train-run-1", 10*time.Millisecond)
	if ok && err == nil {
		t.Fatal("WaitJob reported success with 1/4 workers succeeded and 3 still active")
	}
	if done, _, _ := c.JobDone(context.Background(), "ns", "train-run-1"); done {
		t.Fatal("JobDone reported done with 1/4 workers succeeded and 3 still active")
	}
}

// Deleting an app's Jobs must cascade to their pods: batch/v1 Jobs orphan
// pods when the delete names no propagationPolicy.
func TestDeleteAppObjectsCascadesToJobPods(t *testing.T) {
	dyn := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())
	var got *metav1.DeletionPropagation
	seen := false
	dyn.PrependReactor("delete-collection", "jobs", func(a ktesting.Action) (bool, runtime.Object, error) {
		seen = true
		got = a.(ktesting.DeleteCollectionActionImpl).DeleteOptions.PropagationPolicy
		return true, nil, nil
	})
	c := NewForTest(dyn, nil)
	if err := c.DeleteAppObjects(context.Background(), "ns", "train"); err != nil {
		t.Fatal(err)
	}
	if !seen {
		t.Fatal("no Job delete-collection issued")
	}
	if got == nil || *got == metav1.DeletePropagationOrphan {
		t.Fatalf("Job delete-collection propagationPolicy = %v, want Background/Foreground (pods are orphaned otherwise)", got)
	}
}
