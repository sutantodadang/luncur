package kube

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
)

func drainPod(name string, mods ...func(*corev1.Pod)) *corev1.Pod {
	p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns"}, Spec: corev1.PodSpec{NodeName: "n1"},
		Status: corev1.PodStatus{Phase: corev1.PodRunning}}
	for _, m := range mods {
		m(p)
	}
	return p
}

func TestDrainEvictsInOrderSkipsDaemonSetsAndRetriesPDB(t *testing.T) {
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1"}}
	cs := k8sfake.NewSimpleClientset(node,
		drainPod("web-1"),
		drainPod("db-0"),
		drainPod("luncur-0"),
		drainPod("ds-1", func(p *corev1.Pod) { p.OwnerReferences = []metav1.OwnerReference{{Kind: "DaemonSet", Name: "x"}} }),
		drainPod("done", func(p *corev1.Pod) { p.Status.Phase = corev1.PodSucceeded }),
	)
	// The fake field selector isn't enforced; all pods above are on n1.
	var order []string
	blockedOnce := false
	cs.PrependReactor("create", "pods", func(a ktesting.Action) (bool, runtime.Object, error) {
		if a.GetSubresource() != "eviction" {
			return false, nil, nil
		}
		name := a.(ktesting.CreateAction).GetObject().(metav1.Object).GetName()
		if name == "db-0" && !blockedOnce {
			blockedOnce = true
			return true, nil, apierrors.NewTooManyRequests("Cannot evict pod as it would violate the pod's disruption budget.", 1)
		}
		order = append(order, name)
		return true, nil, nil
	})
	c := NewForTest(nil, cs)
	var last DrainProgress
	prog, err := c.Drain(context.Background(), "n1", DrainOptions{
		Retry:    time.Millisecond,
		IsLast:   func(p corev1.Pod) bool { return p.Name == "luncur-0" },
		Progress: func(p DrainProgress) { last = p },
	})
	if err != nil {
		t.Fatal(err)
	}
	if prog.Total != 3 || prog.Evicted != 3 || last.Evicted != 3 {
		t.Fatalf("progress = %+v", prog)
	}
	if len(order) != 3 || order[len(order)-1] != "luncur-0" {
		t.Fatalf("eviction order = %v, want luncur-0 last", order)
	}
	got, _ := cs.CoreV1().Nodes().Get(context.Background(), "n1", metav1.GetOptions{})
	if !got.Spec.Unschedulable {
		t.Fatal("node not cordoned")
	}
	if err := c.Cordon(context.Background(), "n1", false); err != nil {
		t.Fatal(err)
	}
	if got, _ := cs.CoreV1().Nodes().Get(context.Background(), "n1", metav1.GetOptions{}); got.Spec.Unschedulable {
		t.Fatal("uncordon failed")
	}
}

func TestDrainTimesOutOnPersistentPDB(t *testing.T) {
	cs := k8sfake.NewSimpleClientset(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1"}}, drainPod("db-0"))
	cs.PrependReactor("create", "pods", func(a ktesting.Action) (bool, runtime.Object, error) {
		if a.GetSubresource() != "eviction" {
			return false, nil, nil
		}
		return true, nil, apierrors.NewTooManyRequests("pdb", 1)
	})
	prog, err := NewForTest(nil, cs).Drain(context.Background(), "n1", DrainOptions{Timeout: 5 * time.Millisecond, Retry: time.Millisecond})
	if err == nil || len(prog.Blocked) != 1 {
		t.Fatalf("drain = %+v, %v; want timeout with 1 blocked", prog, err)
	}
}
