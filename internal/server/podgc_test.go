package server

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sfake "k8s.io/client-go/kubernetes/fake"

	"github.com/sutantodadang/luncur/internal/kube"
)

// TestGCFailedPods mirrors pods_test.go's fake-clientset harness: a
// ReplicaSet-owned Failed pod (an eviction corpse) should be deleted by the
// sweep, a Running pod should not.
func TestGCFailedPods(t *testing.T) {
	st := newTestStore(t)
	p, err := st.CreateProject("proj")
	if err != nil {
		t.Fatal(err)
	}
	p, _ = seedDefaultEnv(t, st, p)

	dead := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "web-dead", Namespace: p.Namespace,
			OwnerReferences: []metav1.OwnerReference{{Kind: "ReplicaSet", Name: "web-rs"}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodFailed},
	}
	alive := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "web-alive", Namespace: p.Namespace,
			OwnerReferences: []metav1.OwnerReference{{Kind: "ReplicaSet", Name: "web-rs"}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
	cs := k8sfake.NewSimpleClientset(dead, alive)

	s := newServer(Deps{Store: st, Kube: kube.NewForTest(nil, cs)})
	s.gcFailedPods(context.Background())

	list, err := cs.CoreV1().Pods(p.Namespace).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 1 || list.Items[0].Name != "web-alive" {
		t.Fatalf("pods after gc = %+v, want only web-alive", list.Items)
	}
}
