package server

import (
	"context"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"

	"github.com/sutantodadang/luncur/internal/kube"
	"github.com/sutantodadang/luncur/internal/secret"
	"github.com/sutantodadang/luncur/internal/store"
)

// gateFixture is a server whose fake clientset already holds the app's
// Deployment, newest ReplicaSet and one new pod in the given shape, so the
// rollout gate has something real to judge (the fake dynamic client only
// accepts applies).
type gateFixture struct {
	s    *server
	cs   *k8sfake.Clientset
	p    store.Project
	env  store.Environment
	app  store.App
	prev store.Deployment
}

func newGateFixture(t *testing.T, done bool, pod corev1.Pod) gateFixture {
	t.Helper()
	old := rolloutPoll
	rolloutPoll = 5 * time.Millisecond
	t.Cleanup(func() { rolloutPoll = old })

	st := newTestStore(t)
	p, err := st.CreateProject("shop")
	if err != nil {
		t.Fatal(err)
	}
	p, env := seedDefaultEnv(t, st, p)
	a, err := st.CreateAppInEnv(env.ID, "web", 8080, "web", "")
	if err != nil {
		t.Fatal(err)
	}
	prev, err := st.CreateDeployment(a.ID, "live", "nginx:1", 0)
	if err != nil {
		t.Fatal(err)
	}

	ns := env.Namespace
	one := int32(1)
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: ns, UID: types.UID("dep-uid"), Generation: 2,
			Annotations: map[string]string{"deployment.kubernetes.io/revision": "2"}},
		Spec:   appsv1.DeploymentSpec{Replicas: &one, Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app.kubernetes.io/name": "web"}}},
		Status: appsv1.DeploymentStatus{ObservedGeneration: 2, Replicas: 2, UpdatedReplicas: 1, AvailableReplicas: 1},
	}
	if done {
		dep.Status.Replicas = 1
	}
	rs := &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{Name: "web-abc", Namespace: ns,
			Labels:          map[string]string{"app.kubernetes.io/name": "web"},
			Annotations:     map[string]string{"deployment.kubernetes.io/revision": "2"},
			OwnerReferences: []metav1.OwnerReference{{Kind: "Deployment", Name: "web", UID: types.UID("dep-uid")}}},
		Spec: appsv1.ReplicaSetSpec{Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app.kubernetes.io/name": "web", "pod-template-hash": "abc"}}},
	}
	pod.Namespace = ns
	pod.Labels = map[string]string{"app.kubernetes.io/name": "web", "pod-template-hash": "abc"}
	cs := k8sfake.NewSimpleClientset(dep, rs, &pod)

	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{podMetricsGVR: "PodMetricsList"})
	dyn.PrependReactor("*", "*", func(ktesting.Action) (bool, runtime.Object, error) { return true, nil, nil })
	sealer, err := secret.New(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	s := newServer(Deps{Store: st, Sealer: sealer, Kube: kube.NewForTest(dyn, cs), ExternalIP: "1.2.3.4"})
	return gateFixture{s: s, cs: cs, p: p, env: env, app: a, prev: prev}
}

func (f gateFixture) deploy(t *testing.T, image string) store.Deployment {
	t.Helper()
	d, err := f.s.st.CreateDeployment(f.app.ID, "deploying", image, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.s.applyImageDeploy(context.Background(), f.p, f.env, f.app, d, image); err != nil {
		t.Fatal(err)
	}
	return d
}

// waitDeploys polls the app's deploy history until cond holds.
func (f gateFixture) waitDeploys(t *testing.T, cond func([]store.Deployment) bool) []store.Deployment {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		ds, err := f.s.st.ListDeployments(f.app.ID)
		if err != nil {
			t.Fatal(err)
		}
		if cond(ds) {
			return ds
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out; deploys: %+v", ds)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func gateCrashPod() corev1.Pod {
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "web-abc-1", CreationTimestamp: metav1.Now()},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
			RestartCount:         4,
			State:                corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
			LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "Error", ExitCode: 1}},
		}}},
	}
}

func gateReadyPod() corev1.Pod {
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "web-abc-1", CreationTimestamp: metav1.Now()},
		Status:     corev1.PodStatus{Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}},
	}
}

// A crash-looping image is never recorded live: the deploy fails with the
// classified reason and auto-rollback restores the previous live image —
// and the rollback, which crash-loops on the same fake, fails without
// rolling back again.
func TestRolloutGateFailsCrashLoopAndRollsBack(t *testing.T) {
	f := newGateFixture(t, false, gateCrashPod())
	d := f.deploy(t, "nginx:2")
	if got := f.s.deployStatusWord(d); got != "deploying" && got != "failed" {
		t.Fatalf("status right after apply = %q, want deploying (gated)", got)
	}
	ds := f.waitDeploys(t, func(ds []store.Deployment) bool {
		return len(ds) == 3 && ds[0].Status == "failed" && ds[1].Status == "failed"
	})
	bad, rb := ds[1], ds[0]
	if bad.ID != d.ID || !strings.HasPrefix(bad.FailReason, "crash-looping: Error") {
		t.Fatalf("failed deploy = %+v, want crash-looping reason", bad)
	}
	if rb.RolledBackFrom != f.prev.ID || rb.ImageRef != "nginx:1" {
		t.Fatalf("rollback deploy = %+v, want restore of %s", rb, f.prev.ID)
	}
	time.Sleep(50 * time.Millisecond)
	if n, _ := f.s.st.CountDeployments(f.app.ID); n != 3 {
		t.Fatalf("deploy count = %d, want 3 (a rollback never auto-rolls back)", n)
	}
	audit, _ := f.s.st.ListAudit(10, 0, "", "")
	if len(audit) == 0 || audit[0].UserEmail != autoRollbackActor {
		t.Fatalf("auto-rollback not audited: %+v", audit)
	}
}

func TestRolloutGateNoRollbackWhenPolicyOff(t *testing.T) {
	f := newGateFixture(t, false, gateCrashPod())
	pol := store.DefaultAppPolicy(f.app.ID)
	pol.AutoRollback = false
	if err := f.s.st.SetAppPolicy(pol); err != nil {
		t.Fatal(err)
	}
	f.deploy(t, "nginx:2")
	f.waitDeploys(t, func(ds []store.Deployment) bool { return ds[0].Status == "failed" })
	time.Sleep(50 * time.Millisecond)
	if n, _ := f.s.st.CountDeployments(f.app.ID); n != 2 {
		t.Fatalf("deploy count = %d, want 2 (no rollback)", n)
	}
}

func TestRolloutGateMarksLiveWhenRolloutDone(t *testing.T) {
	f := newGateFixture(t, true, gateReadyPod())
	d := f.deploy(t, "nginx:2")
	ds := f.waitDeploys(t, func(ds []store.Deployment) bool { return ds[0].Status == "live" })
	if ds[0].ID != d.ID || ds[0].ReadyAt == "" {
		t.Fatalf("live deploy = %+v, want ready_at set", ds[0])
	}
	if o, _ := f.s.st.GetDeployOutcome(d.ID); o.Want != 1 || o.Ready != 1 {
		t.Fatalf("progress = %+v, want 1/1", o)
	}
}

// A newer deploy supersedes one still rolling out.
func TestRolloutGateSuperseded(t *testing.T) {
	f := newGateFixture(t, false, corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "web-abc-1", CreationTimestamp: metav1.Now()}})
	d := f.deploy(t, "nginx:2")
	if _, err := f.s.st.CreateDeployment(f.app.ID, "building", "", 0); err != nil {
		t.Fatal(err)
	}
	f.waitDeploys(t, func(ds []store.Deployment) bool { return ds[1].ID == d.ID && ds[1].Status == "failed" })
	got, _ := f.s.st.GetDeployment(d.ID)
	if !strings.HasPrefix(got.FailReason, "superseded by deploy #3") {
		t.Fatalf("fail reason = %q", got.FailReason)
	}
}
