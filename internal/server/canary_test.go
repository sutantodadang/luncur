package server

import (
	"strings"
	"sync"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
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

// canaryFixture: a web app with a live deploy and a canary policy, whose
// fake clientset already reports both the stable and canary Deployments
// fully rolled out, and whose fake dynamic client records applies/deletes.
type canaryFixture struct {
	gateFixture
	mu      sync.Mutex
	applied map[string][]string // kind -> names (in order)
	patches map[string]string   // kind/name -> last patch body
	deleted []string            // kind/name
	probeOK bool
}

func doneDeployment(name string, uid types.UID, ns string) *appsv1.Deployment {
	one := int32(1)
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, UID: uid, Generation: 1},
		Spec:       appsv1.DeploymentSpec{Replicas: &one, Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app.kubernetes.io/name": name}}},
		Status:     appsv1.DeploymentStatus{ObservedGeneration: 1, Replicas: 1, UpdatedReplicas: 1, AvailableReplicas: 1},
	}
}

func newCanaryFixture(t *testing.T, strategy string, weighted bool, canaryPods ...*corev1.Pod) *canaryFixture {
	t.Helper()
	oldPoll, oldTick, oldScale := rolloutPoll, canaryTick, canaryTimeScale
	rolloutPoll, canaryTick, canaryTimeScale = 2*time.Millisecond, 2*time.Millisecond, 0.0001
	t.Cleanup(func() { rolloutPoll, canaryTick, canaryTimeScale = oldPoll, oldTick, oldScale })

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
	prev, _ := st.CreateDeployment(a.ID, "live", "nginx:1", 0)
	pol := store.DefaultAppPolicy(a.ID)
	pol.Strategy = strategy
	if err := st.SetAppPolicy(pol); err != nil {
		t.Fatal(err)
	}

	objs := []runtime.Object{doneDeployment("web", "stable-uid", env.Namespace), doneDeployment("web-canary", "canary-uid", env.Namespace)}
	for _, pod := range canaryPods {
		pod.Namespace = env.Namespace
		objs = append(objs, pod)
	}
	// The canary pods need a ReplicaSet for RolloutStatus to find them.
	objs = append(objs, &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{Name: "web-canary-x", Namespace: env.Namespace,
			Labels:          map[string]string{"app.kubernetes.io/name": "web-canary"},
			OwnerReferences: []metav1.OwnerReference{{Kind: "Deployment", Name: "web-canary", UID: "canary-uid"}}},
		Spec: appsv1.ReplicaSetSpec{Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"pod-template-hash": "x"}}},
	})
	cs := k8sfake.NewSimpleClientset(objs...)

	f := &canaryFixture{applied: map[string][]string{}, patches: map[string]string{}, probeOK: true}
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{podMetricsGVR: "PodMetricsList"})
	dyn.PrependReactor("*", "*", func(a ktesting.Action) (bool, runtime.Object, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch act := a.(type) {
		case ktesting.PatchAction:
			kind := a.GetResource().Resource
			f.applied[kind] = append(f.applied[kind], act.GetName())
			f.patches[kind+"/"+act.GetName()] = string(act.GetPatch())
		case ktesting.DeleteAction:
			f.deleted = append(f.deleted, a.GetResource().Resource+"/"+act.GetName())
		case ktesting.GetAction:
			if a.GetResource().Resource == "customresourcedefinitions" && weighted {
				return true, &unstructured.Unstructured{Object: map[string]any{"metadata": map[string]any{"name": act.GetName()}}}, nil
			}
		}
		return true, nil, nil
	})
	sealer, _ := secret.New(make([]byte, 32))
	s := newServer(Deps{Store: st, Sealer: sealer, Kube: kube.NewForTest(dyn, cs), ExternalIP: "1.2.3.4"})
	s.canaryProbeFn = func(string) bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.probeOK
	}
	f.gateFixture = gateFixture{s: s, cs: cs, p: p, env: env, app: a, prev: prev}
	return f
}

func (f *canaryFixture) waitRollout(t *testing.T, deployID string, phase string) store.Rollout {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		ro, err := f.s.st.GetRollout(deployID)
		if err == nil && ro.Phase == phase {
			return ro
		}
		if time.Now().After(deadline) {
			t.Fatalf("rollout never reached %q: %+v (%v)", phase, ro, err)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func (f *canaryFixture) wasApplied(kind, name string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, n := range f.applied[kind] {
		if n == name {
			return true
		}
	}
	return false
}

func (f *canaryFixture) wasDeleted(what string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, d := range f.deleted {
		if d == what {
			return true
		}
	}
	return false
}

// Ratio-mode canary: the canary track rolls out, walks the steps, promotes
// the stable Deployment and is cleaned up; the deploy goes live.
func TestCanaryRatioModePromotes(t *testing.T) {
	f := newCanaryFixture(t, "canary", false)
	d := f.deploy(t, "nginx:2")
	f.waitRollout(t, d.ID, "done")
	f.waitDeploys(t, func(ds []store.Deployment) bool { return ds[0].ID == d.ID && ds[0].Status == "live" })
	if !f.wasApplied("deployments", "web-canary") || !f.wasApplied("services", "web-canary") {
		t.Fatalf("canary track not applied: %v", f.applied)
	}
	// Promotion re-applies the stable Deployment with the new image.
	f.mu.Lock()
	stablePatch := f.patches["deployments/web"]
	f.mu.Unlock()
	if !strings.Contains(stablePatch, "nginx:2") {
		t.Fatalf("stable not promoted to nginx:2: %s", stablePatch)
	}
	if !f.wasDeleted("deployments/web-canary") || !f.wasDeleted("services/web-canary") {
		t.Fatalf("canary not cleaned up: %v", f.deleted)
	}
	// Ratio mode: canary pods share the app's name label.
	f.mu.Lock()
	canaryPatch := f.patches["deployments/web-canary"]
	f.mu.Unlock()
	if !strings.Contains(canaryPatch, `"luncur.dev/track":"canary"`) || !strings.Contains(canaryPatch, `"app.kubernetes.io/name":"web"`) {
		t.Fatalf("ratio canary labels wrong: %s", canaryPatch)
	}
}

// Weighted mode routes through a TraefikService + IngressRoute, removed on
// promotion.
func TestCanaryWeightedRoutesThroughTraefik(t *testing.T) {
	f := newCanaryFixture(t, "canary", true)
	d := f.deploy(t, "nginx:2")
	f.waitRollout(t, d.ID, "done")
	if !f.wasApplied("traefikservices", "web-split") || !f.wasApplied("ingressroutes", "web-split-web") {
		t.Fatalf("weighted routing not applied: %v", f.applied)
	}
	f.mu.Lock()
	split := f.patches["traefikservices/web-split"]
	f.mu.Unlock()
	if !strings.Contains(split, `"name":"web-canary","port":80,"weight":50`) {
		t.Fatalf("last split = %s, want 50%% on the canary", split)
	}
	if !f.wasDeleted("ingressroutes/web-split-web") || !f.wasDeleted("traefikservices/web-split") {
		t.Fatalf("routing not cleaned up: %v", f.deleted)
	}
}

// A failing canary aborts: stable is never touched, no rollback deploy is
// made, and the deploy fails with the reason.
func TestCanaryAbortsOnFailingProbes(t *testing.T) {
	f := newCanaryFixture(t, "canary", false)
	f.probeOK = false
	d := f.deploy(t, "nginx:2")
	ro := f.waitRollout(t, d.ID, "aborted")
	if !strings.Contains(ro.Note, "success rate 0%") {
		t.Fatalf("abort note = %q", ro.Note)
	}
	ds := f.waitDeploys(t, func(ds []store.Deployment) bool { return ds[0].Status == "failed" })
	if !strings.HasPrefix(ds[0].FailReason, "canary aborted:") || len(ds) != 2 {
		t.Fatalf("deploys = %+v", ds)
	}
	f.mu.Lock()
	_, stableTouched := f.patches["deployments/web"]
	f.mu.Unlock()
	if stableTouched {
		t.Fatal("stable Deployment changed by an aborted canary")
	}
}

// A restarting canary pod aborts the rollout.
func TestCanaryAbortsOnRestartingPod(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "web-canary-x-1", Labels: map[string]string{"pod-template-hash": "x"}},
		Status: corev1.PodStatus{
			Conditions:        []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
			ContainerStatuses: []corev1.ContainerStatus{{RestartCount: 1, LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "Error"}}}},
		},
	}
	f := newCanaryFixture(t, "canary", false, pod)
	d := f.deploy(t, "nginx:2")
	ro := f.waitRollout(t, d.ID, "aborted")
	if !strings.Contains(ro.Note, "restarted (Error)") {
		t.Fatalf("abort note = %q", ro.Note)
	}
}

// Rollbacks and first deploys always roll; the rollout API answers promote
// and abort requests only while a rollout runs.
func TestCanaryStrategySelectionAndAPI(t *testing.T) {
	f := newCanaryFixture(t, "bluegreen", false)
	rb, _ := f.s.st.CreateRollbackDeployment(f.app.ID, "nginx:1", 0, f.prev.ID)
	if got := f.s.deployStrategy(f.app, rb); got != "rolling" {
		t.Fatalf("rollback strategy = %q", got)
	}
	d, _ := f.s.st.CreateDeployment(f.app.ID, "deploying", "nginx:2", 0)
	if got := f.s.deployStrategy(f.app, d); got != "bluegreen" {
		t.Fatalf("strategy = %q", got)
	}
	if err := f.s.requestRollout(f.app, "promote"); err == nil {
		t.Fatal("promote with no rollout accepted")
	}
	if _, err := f.s.st.CreateRollout(d.ID, f.app.ID, "bluegreen"); err != nil {
		t.Fatal(err)
	}
	if err := f.s.requestRollout(f.app, "abort"); err != nil {
		t.Fatal(err)
	}
	if ro, _ := f.s.st.GetRollout(d.ID); ro.Phase != "abort-requested" {
		t.Fatalf("phase = %q", ro.Phase)
	}
	// Blue-green runs a full-size canary and a single 100% step.
	if got := canarySteps(store.DefaultAppPolicy(1), "bluegreen"); len(got) != 1 || got[0] != 100 {
		t.Fatalf("bluegreen steps = %v", got)
	}
}
