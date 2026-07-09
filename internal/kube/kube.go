// Package kube applies luncur-rendered manifests to the cluster with
// server-side apply (fieldManager=luncur), so user edits made through
// luncur's override system merge cleanly with cluster state.
package kube

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"sort"
	"strings"
	"time"

	rbacv1 "k8s.io/api/rbac/v1"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/remotecommand"

	"github.com/sutantodadang/luncur/internal/addon"
	"github.com/sutantodadang/luncur/internal/render"
)

var gvrByKind = map[string]schema.GroupVersionResource{
	"Deployment":            {Group: "apps", Version: "v1", Resource: "deployments"},
	"Service":               {Group: "", Version: "v1", Resource: "services"},
	"Ingress":               {Group: "networking.k8s.io", Version: "v1", Resource: "ingresses"},
	"Secret":                {Group: "", Version: "v1", Resource: "secrets"},
	"Namespace":             {Group: "", Version: "v1", Resource: "namespaces"},
	"Job":                   {Group: "batch", Version: "v1", Resource: "jobs"},
	"CronJob":               {Group: "batch", Version: "v1", Resource: "cronjobs"},
	"DaemonSet":             {Group: "apps", Version: "v1", Resource: "daemonsets"},
	"RuntimeClass":          {Group: "node.k8s.io", Version: "v1", Resource: "runtimeclasses"},
	"PersistentVolumeClaim": {Group: "", Version: "v1", Resource: "persistentvolumeclaims"},
	"ServiceAccount":        {Group: "", Version: "v1", Resource: "serviceaccounts"},
	"ClusterRole":           {Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterroles"},
	"ClusterRoleBinding":    {Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterrolebindings"},
	"HelmChartConfig":       {Group: "helm.cattle.io", Version: "v1", Resource: "helmchartconfigs"},
	"ClusterIssuer":         {Group: "cert-manager.io", Version: "v1", Resource: "clusterissuers"},
	"StatefulSet":           {Group: "apps", Version: "v1", Resource: "statefulsets"},
	"PodMetrics":            {Group: "metrics.k8s.io", Version: "v1beta1", Resource: "pods"},
	"NodeMetrics":           {Group: "metrics.k8s.io", Version: "v1beta1", Resource: "nodes"},
	// Argo Workflows engine (PR-C3): the Workflow CR and the manifest kinds
	// its pinned install.yaml carries (CustomResourceDefinition, ConfigMap,
	// Role, RoleBinding, PriorityClass — Deployment/Service/ServiceAccount
	// etc. above already cover the rest).
	"Workflow":                 {Group: "argoproj.io", Version: "v1alpha1", Resource: "workflows"},
	"CustomResourceDefinition": {Group: "apiextensions.k8s.io", Version: "v1", Resource: "customresourcedefinitions"},
	"ConfigMap":                {Group: "", Version: "v1", Resource: "configmaps"},
	"Role":                     {Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "roles"},
	"RoleBinding":              {Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "rolebindings"},
	"PriorityClass":            {Group: "scheduling.k8s.io", Version: "v1", Resource: "priorityclasses"},
	"HorizontalPodAutoscaler":  {Group: "autoscaling", Version: "v2", Resource: "horizontalpodautoscalers"},
	"NetworkPolicy":            {Group: "networking.k8s.io", Version: "v1", Resource: "networkpolicies"},
	// Project quotas (D4): ResourceQuota/LimitRange also back the GPU quota
	// feature's ResourceQuota apply, which had no GVR entry until now (see
	// gpu.QuotaObject) — its cluster apply was failing with "no GVR for kind
	// \"ResourceQuota\"" since it shipped.
	"ResourceQuota":       {Group: "", Version: "v1", Resource: "resourcequotas"},
	"LimitRange":          {Group: "", Version: "v1", Resource: "limitranges"},
	"PodDisruptionBudget": {Group: "policy", Version: "v1", Resource: "poddisruptionbudgets"},
}

// clusterScoped marks kinds Apply must patch without a namespace.
var clusterScoped = map[string]bool{
	"Namespace":                true,
	"RuntimeClass":             true,
	"ClusterRole":              true,
	"ClusterRoleBinding":       true,
	"ClusterIssuer":            true,
	"CustomResourceDefinition": true,
	"PriorityClass":            true,
}

type Client struct {
	dyn dynamic.Interface
	cs  kubernetes.Interface
	cfg *rest.Config
}

// PodExecer runs a command inside a pod container. Faked in tests; the
// real implementation streams over SPDY.
type PodExecer interface {
	ExecPod(ctx context.Context, namespace, pod, container string, cmd []string, stdout, stderr io.Writer) error
}

// New builds a client from a kubeconfig path, or in-cluster config when
// path is empty.
func New(kubeconfig string) (*Client, error) {
	var cfg *rest.Config
	var err error
	if kubeconfig == "" {
		cfg, err = rest.InClusterConfig()
	} else {
		cfg, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
	}
	if err != nil {
		return nil, fmt.Errorf("kube config: %w", err)
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	return &Client{dyn: dyn, cs: cs, cfg: cfg}, nil
}

func NewFromDynamic(dyn dynamic.Interface) *Client { return &Client{dyn: dyn} }

// NewForTest wires both halves explicitly; either may be nil.
func NewForTest(dyn dynamic.Interface, cs kubernetes.Interface) *Client {
	return &Client{dyn: dyn, cs: cs}
}

func applyOpts() metav1.PatchOptions {
	force := true
	return metav1.PatchOptions{FieldManager: "luncur", Force: &force}
}

func nameOf(objJSON []byte) (string, error) {
	var m struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(objJSON, &m); err != nil {
		return "", err
	}
	if m.Metadata.Name == "" {
		return "", fmt.Errorf("object has no metadata.name")
	}
	return m.Metadata.Name, nil
}

// EnsureNamespace stamps a project/app namespace at the "baseline"
// PodSecurity level. Not "restricted": luncur renders app pods from
// arbitrary user images (nixpacks output, postgres, nginx) that routinely
// run as root and set no securityContext, and restricted rejects every
// such pod at admission — on a live cluster all app ReplicaSets sat at
// FailedCreate and the ingress served 503s. Baseline still blocks the
// escalation vectors (privileged, host namespaces, hostPath, added caps);
// the override denylist blocks them at the API layer too.
func (c *Client) EnsureNamespace(ctx context.Context, name string) error {
	return c.EnsureNamespaceWithPolicy(ctx, name, "baseline")
}

// EnsureNamespaceWithPolicy server-side-applies a namespace stamped with
// app.kubernetes.io/managed-by:luncur and pod-security.kubernetes.io/enforce
// set to policy. Callers that need a laxer profile than "restricted" (e.g.
// the system namespace hosting BuildKit) pass "baseline" here instead of
// going through EnsureNamespace.
func (c *Client) EnsureNamespaceWithPolicy(ctx context.Context, name, policy string) error {
	ns := fmt.Sprintf(
		`{"apiVersion":"v1","kind":"Namespace","metadata":{"name":%q,"labels":{"app.kubernetes.io/managed-by":"luncur","pod-security.kubernetes.io/enforce":%q}}}`,
		name, policy,
	)
	_, err := c.dyn.Resource(gvrByKind["Namespace"]).Patch(
		ctx, name, types.ApplyPatchType, []byte(ns), applyOpts(),
	)
	return err
}

// luncurIsolationPolicy is the project-isolation NetworkPolicy applied to
// every project namespace when the network_isolation setting is on: ingress
// only from pods in the same namespace, the ingress controller (kube-system
// on k3s), and luncur-system (the panel proxies addon UIs, e.g. mlflow).
// Egress is deliberately untouched.
const luncurIsolationPolicy = `{"apiVersion":"networking.k8s.io/v1","kind":"NetworkPolicy",
 "metadata":{"name":"luncur-isolation","labels":{"app.kubernetes.io/managed-by":"luncur"}},
 "spec":{"podSelector":{},"policyTypes":["Ingress"],
   "ingress":[{"from":[
     {"podSelector":{}},
     {"namespaceSelector":{"matchLabels":{"kubernetes.io/metadata.name":"kube-system"}}},
     {"namespaceSelector":{"matchLabels":{"kubernetes.io/metadata.name":"luncur-system"}}}
   ]}]}}`

// ApplyIsolation server-side-applies the project-isolation NetworkPolicy
// named "luncur-isolation" into namespace. See luncurIsolationPolicy for the
// policy shape.
func (c *Client) ApplyIsolation(ctx context.Context, namespace string) error {
	_, err := c.dyn.Resource(gvrByKind["NetworkPolicy"]).Namespace(namespace).Patch(
		ctx, "luncur-isolation", types.ApplyPatchType, []byte(luncurIsolationPolicy), applyOpts(),
	)
	return err
}

// RemoveIsolation deletes the project-isolation NetworkPolicy. NotFound is
// fine — toggling network_isolation off must be idempotent.
func (c *Client) RemoveIsolation(ctx context.Context, namespace string) error {
	return c.DeleteObject(ctx, namespace, "NetworkPolicy", "luncur-isolation")
}

// IsNotFound reports whether err is a Kubernetes "not found" API error —
// exposed so server-package callers can skip projects whose namespace
// hasn't been created yet without importing apierrors themselves.
func IsNotFound(err error) bool {
	return apierrors.IsNotFound(err)
}

func (c *Client) Apply(ctx context.Context, namespace string, objs []render.Object) error {
	for _, o := range objs {
		gvr, ok := gvrByKind[o.Kind]
		if !ok {
			return fmt.Errorf("no GVR for kind %q", o.Kind)
		}
		name, err := nameOf(o.JSON)
		if err != nil {
			return fmt.Errorf("%s: %w", o.Kind, err)
		}
		// Cluster-scoped kinds: never namespace the patch call itself.
		if clusterScoped[o.Kind] {
			_, err = c.dyn.Resource(gvr).Patch(
				ctx, name, types.ApplyPatchType, o.JSON, applyOpts(),
			)
		} else {
			_, err = c.dyn.Resource(gvr).Namespace(namespace).Patch(
				ctx, name, types.ApplyPatchType, o.JSON, applyOpts(),
			)
		}
		if err != nil {
			return fmt.Errorf("apply %s/%s: %w", o.Kind, name, err)
		}
	}
	return nil
}

// EnsureClusterRole makes the live "luncur" ClusterRole match cr, creating
// it if absent — the server's startup self-heal (internal/cli/serve.go)
// calls this on every boot so a release that adds RBAC rules (e.g. metrics
// nodes, PodDisruptionBudgets) doesn't require the operator to re-run
// `luncur up` before the new permission takes effect.
//
// Deliberately get-then-create/update rather than routing through Apply's
// server-side-apply patch: SSA apply-patches don't three-way-merge reliably
// against the fake dynamic client this package's tests use (see
// TestApplyClusterRoleBindingSkipsNamespace's comment on the same
// limitation), and a plain get/write pair is simple enough for an object
// that only changes across releases. Returns changed=true when the
// ClusterRole was created or its rules differed from cr's.
func (c *Client) EnsureClusterRole(ctx context.Context, cr *rbacv1.ClusterRole) (changed bool, err error) {
	gvr := gvrByKind["ClusterRole"]
	obj, err := clusterRoleToUnstructured(cr)
	if err != nil {
		return false, err
	}

	existing, getErr := c.dyn.Resource(gvr).Get(ctx, cr.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(getErr) {
		if _, err := c.dyn.Resource(gvr).Create(ctx, obj, metav1.CreateOptions{FieldManager: "luncur"}); err != nil {
			return false, fmt.Errorf("create ClusterRole/%s: %w", cr.Name, err)
		}
		return true, nil
	}
	if getErr != nil {
		return false, fmt.Errorf("get ClusterRole/%s: %w", cr.Name, getErr)
	}

	var current rbacv1.ClusterRole
	b, err := json.Marshal(existing.Object)
	if err != nil {
		return false, err
	}
	if err := json.Unmarshal(b, &current); err != nil {
		return false, err
	}
	if reflect.DeepEqual(current.Rules, cr.Rules) {
		return false, nil
	}

	obj.SetResourceVersion(existing.GetResourceVersion())
	if _, err := c.dyn.Resource(gvr).Update(ctx, obj, metav1.UpdateOptions{FieldManager: "luncur"}); err != nil {
		return false, fmt.Errorf("update ClusterRole/%s: %w", cr.Name, err)
	}
	return true, nil
}

// clusterRoleToUnstructured round-trips cr through JSON — the dynamic
// client's typed Create/Update calls need an *unstructured.Unstructured,
// and json.Marshal already respects cr's TypeMeta (apiVersion/kind) plus
// every field's json tag, so this is simpler than hand-building the map.
func clusterRoleToUnstructured(cr *rbacv1.ClusterRole) (*unstructured.Unstructured, error) {
	b, err := json.Marshal(cr)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return &unstructured.Unstructured{Object: m}, nil
}

// HasWorkflowCRD reports whether the Argo Workflows CRD is installed —
// the preflight `startPipelineRun` uses before compiling a run onto the
// argo engine (spec §Argo: friendly "run `luncur argo install`" error
// instead of a raw apply failure against a missing CRD).
func (c *Client) HasWorkflowCRD(ctx context.Context) (bool, error) {
	_, err := c.dyn.Resource(gvrByKind["CustomResourceDefinition"]).Get(
		ctx, "workflows.argoproj.io", metav1.GetOptions{},
	)
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// GetWorkflow returns a pipeline run's Workflow CR as unstructured content;
// (nil, false, nil) when it doesn't exist (the run's argo tick treats a
// vanished Workflow as a distinct case from a transient error).
func (c *Client) GetWorkflow(ctx context.Context, namespace, name string) (map[string]any, bool, error) {
	u, err := c.dyn.Resource(gvrByKind["Workflow"]).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if u == nil {
		return nil, false, nil
	}
	return u.Object, true, nil
}

// DeleteWorkflow removes a pipeline run's Workflow CR. NotFound is fine —
// stopPipelineRun's argo branch must be idempotent like every other
// teardown path in this file.
func (c *Client) DeleteWorkflow(ctx context.Context, namespace, name string) error {
	err := c.dyn.Resource(gvrByKind["Workflow"]).Namespace(namespace).Delete(ctx, name, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete Workflow/%s: %w", name, err)
	}
	return nil
}

// DeleteAppObjects removes everything Render produces for an app, across
// every kind (web/worker/cron): the target list is a superset, and
// NotFound (a kind's object was never rendered for this app) is fine —
// destroy must be idempotent.
func (c *Client) DeleteAppObjects(ctx context.Context, namespace, app string) error {
	targets := []struct{ kind, name string }{
		{"Deployment", app},
		{"Service", app},
		{"Ingress", app},
		{"CronJob", app},
		{"HorizontalPodAutoscaler", app},
		{"PodDisruptionBudget", app},
		{"Secret", render.SecretName(app)},
	}
	for _, t := range targets {
		err := c.dyn.Resource(gvrByKind[t.kind]).Namespace(namespace).Delete(
			ctx, t.name, metav1.DeleteOptions{},
		)
		if err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete %s/%s: %w", t.kind, t.name, err)
		}
	}
	// Per-run Jobs (kind=job apps) have per-run names; delete by the app
	// label instead.
	if err := c.dyn.Resource(gvrByKind["Job"]).Namespace(namespace).DeleteCollection(
		ctx, metav1.DeleteOptions{}, metav1.ListOptions{LabelSelector: "app.kubernetes.io/name=" + app},
	); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete jobs for %s: %w", app, err)
	}
	return nil
}

// JobPods lists the pods backing a Job, via the job-name label the Job
// controller stamps on them. No clientset wired -> (nil, nil).
func (c *Client) JobPods(ctx context.Context, namespace, jobName string) ([]string, error) {
	if c.cs == nil {
		return nil, nil
	}
	list, err := c.cs.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "job-name=" + jobName,
	})
	if err != nil {
		return nil, fmt.Errorf("list pods: %w", err)
	}
	names := make([]string, 0, len(list.Items))
	for _, p := range list.Items {
		names = append(names, p.Name)
	}
	return names, nil
}

// JobExitCode reports the terminated exit code of the newest pod backing a
// Job. ok=false when no pod (or no terminated container status) was found —
// callers record "exit code unknown", never an error.
func (c *Client) JobExitCode(ctx context.Context, namespace, jobName string) (int64, bool, error) {
	if c.cs == nil {
		return 0, false, nil
	}
	list, err := c.cs.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "job-name=" + jobName,
	})
	if err != nil {
		return 0, false, fmt.Errorf("list pods: %w", err)
	}
	if len(list.Items) == 0 {
		return 0, false, nil
	}
	newest := list.Items[0]
	for _, p := range list.Items[1:] {
		if p.CreationTimestamp.After(newest.CreationTimestamp.Time) {
			newest = p
		}
	}
	for _, cst := range newest.Status.ContainerStatuses {
		if cst.State.Terminated != nil {
			return int64(cst.State.Terminated.ExitCode), true, nil
		}
	}
	return 0, false, nil
}

// DeleteObject removes a single object by kind/name, honoring the same
// cluster-scoped/namespaced rules Apply does (namespace is ignored for
// cluster-scoped kinds). NotFound is fine — teardown paths (e.g. `luncur
// down`) must be idempotent.
func (c *Client) DeleteObject(ctx context.Context, namespace, kind, name string) error {
	gvr, ok := gvrByKind[kind]
	if !ok {
		return fmt.Errorf("no GVR for kind %q", kind)
	}
	var err error
	if clusterScoped[kind] {
		err = c.dyn.Resource(gvr).Delete(ctx, name, metav1.DeleteOptions{})
	} else {
		err = c.dyn.Resource(gvr).Namespace(namespace).Delete(ctx, name, metav1.DeleteOptions{})
	}
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete %s/%s: %w", kind, name, err)
	}
	return nil
}

// ListNamespacesByLabel returns the names of namespaces matching a label
// selector — used by `luncur down` to enumerate every luncur-managed
// namespace (luncur-system + every project's "luncur-<name>") without
// needing offline access to the DB, which (unlike a bare-host dataDir)
// lives inside a PersistentVolumeClaim in the cluster.
func (c *Client) ListNamespacesByLabel(ctx context.Context, selector string) ([]string, error) {
	list, err := c.cs.CoreV1().Namespaces().List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return nil, fmt.Errorf("list namespaces: %w", err)
	}
	names := make([]string, 0, len(list.Items))
	for _, n := range list.Items {
		names = append(names, n.Name)
	}
	return names, nil
}

// DeleteNamespace removes a namespace, cascading to delete everything
// namespaced inside it. NotFound is fine — teardown must be idempotent.
func (c *Client) DeleteNamespace(ctx context.Context, name string) error {
	err := c.cs.CoreV1().Namespaces().Delete(ctx, name, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete namespace %s: %w", name, err)
	}
	return nil
}

// DeletePVC removes a single PersistentVolumeClaim (an app volume's purge
// path). NotFound is fine — the claim may never have been applied.
func (c *Client) DeletePVC(ctx context.Context, namespace, name string) error {
	err := c.dyn.Resource(gvrByKind["PersistentVolumeClaim"]).Namespace(namespace).Delete(
		ctx, name, metav1.DeleteOptions{},
	)
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete PersistentVolumeClaim/%s: %w", name, err)
	}
	return nil
}

// DeleteAddonObjects removes an addon instance's StatefulSet, headless
// Service, and credentials Secret, and (unless keepData) its PVC.
// NotFound is fine — deletion must be idempotent.
func (c *Client) DeleteAddonObjects(ctx context.Context, namespace, name string, keepData bool) error {
	svcName := addon.ServiceName(name)
	targets := []struct{ kind, name string }{
		{"StatefulSet", svcName},
		{"Service", svcName},
		{"Secret", addon.SecretName(name)},
	}
	if !keepData {
		targets = append(targets, struct{ kind, name string }{"PersistentVolumeClaim", "data-" + svcName + "-0"})
	}
	for _, t := range targets {
		err := c.dyn.Resource(gvrByKind[t.kind]).Namespace(namespace).Delete(
			ctx, t.name, metav1.DeleteOptions{},
		)
		if err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete %s/%s: %w", t.kind, t.name, err)
		}
	}
	return nil
}

// WaitJob polls a Job until it succeeds (true), fails (false), or ctx ends.
func (c *Client) WaitJob(ctx context.Context, namespace, name string, poll time.Duration) (bool, error) {
	for {
		u, err := c.dyn.Resource(gvrByKind["Job"]).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return false, fmt.Errorf("get job %s: %w", name, err)
		}
		if u == nil {
			// Fake clients with intercept-everything reactors return a nil
			// object with a nil error; treat it like a missing Job.
			return false, fmt.Errorf("get job %s: no object returned", name)
		}
		if n, _, _ := unstructured.NestedInt64(u.Object, "status", "succeeded"); n >= 1 {
			return true, nil
		}
		if n, _, _ := unstructured.NestedInt64(u.Object, "status", "failed"); n >= 1 {
			return false, nil
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-time.After(poll):
		}
	}
}

// JobExists reports whether the named Job still exists. Used by restart
// reconciliation to distinguish "the Job survived the restart, still worth
// waiting on" from "the Job is gone, mark the deployment failed".
func (c *Client) JobExists(ctx context.Context, namespace, name string) (bool, error) {
	_, err := c.dyn.Resource(gvrByKind["Job"]).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("get job %s: %w", name, err)
	}
	return true, nil
}

// JobDone reports whether a Job has reached a terminal state: done is true
// once status.succeeded or status.failed is >=1 (failed distinguishes
// which); both false means the Job is still running. Unlike WaitJob this
// is a single non-blocking read — the pipeline engine's 30s tick calls it
// once per running step rather than blocking the tick on one Job. A
// missing Job (NotFound) reports done=false, failed=false: callers that
// need to distinguish "still pending" from "vanished after a restart" use
// JobExists for that.
func (c *Client) JobDone(ctx context.Context, namespace, name string) (done, failed bool, err error) {
	u, getErr := c.dyn.Resource(gvrByKind["Job"]).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(getErr) {
		return false, false, nil
	}
	if getErr != nil {
		return false, false, fmt.Errorf("get job %s: %w", name, getErr)
	}
	if u == nil {
		// Fake clients with intercept-everything reactors return a nil
		// object with a nil error; treat it like a missing Job.
		return false, false, nil
	}
	if n, _, _ := unstructured.NestedInt64(u.Object, "status", "succeeded"); n >= 1 {
		return true, false, nil
	}
	if n, _, _ := unstructured.NestedInt64(u.Object, "status", "failed"); n >= 1 {
		return true, true, nil
	}
	return false, false, nil
}

// JobPodStatus reports the phase (and, if a container is waiting, its
// reason — e.g. ImagePullBackOff) of the newest pod backing a Job. Used to
// surface build-in-progress milestones to the deploy log before the Job
// itself finishes. No pods yet, or no clientset half configured (some test
// clients only wire the dynamic half) -> ("", "", nil): callers poll.
func (c *Client) JobPodStatus(ctx context.Context, namespace, jobName string) (phase, reason string, err error) {
	if c.cs == nil {
		return "", "", nil
	}
	list, err := c.cs.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "job-name=" + jobName,
	})
	if err != nil {
		return "", "", fmt.Errorf("list pods: %w", err)
	}
	if len(list.Items) == 0 {
		return "", "", nil
	}
	newest := list.Items[0]
	for _, p := range list.Items[1:] {
		if p.CreationTimestamp.After(newest.CreationTimestamp.Time) {
			newest = p
		}
	}
	phase = string(newest.Status.Phase)
	reason = newest.Status.Reason
	for _, cst := range newest.Status.ContainerStatuses {
		if cst.State.Waiting != nil && cst.State.Waiting.Reason != "" {
			reason = cst.State.Waiting.Reason
			break
		}
	}
	return phase, reason, nil
}

// JobEvents returns up to the 5 most recent Kubernetes events recorded
// against the named Job (e.g. a Warning/FailedCreate from a PodSecurity or
// quota rejection that stopped the Job from ever creating a pod), formatted
// as "<Type> <Reason>: <Message>", oldest first. Used by watchBuildPod to
// explain a build that never produces a builder pod. No clientset wired
// (some test clients only wire the dynamic half) -> (nil, nil).
func (c *Client) JobEvents(ctx context.Context, namespace, jobName string) ([]string, error) {
	if c.cs == nil {
		return nil, nil
	}
	list, err := c.cs.CoreV1().Events(namespace).List(ctx, metav1.ListOptions{
		FieldSelector: "involvedObject.name=" + jobName + ",involvedObject.kind=Job",
	})
	if err != nil {
		return nil, fmt.Errorf("list events: %w", err)
	}
	items := list.Items
	sort.Slice(items, func(i, j int) bool {
		return items[i].LastTimestamp.Time.Before(items[j].LastTimestamp.Time)
	})
	if len(items) > 5 {
		items = items[len(items)-5:]
	}
	out := make([]string, 0, len(items))
	for _, e := range items {
		out = append(out, fmt.Sprintf("%s %s: %s", e.Type, e.Reason, e.Message))
	}
	return out, nil
}

// AppPods lists pod names carrying the app label Render stamps on
// every workload (app.kubernetes.io/name=<app>).
func (c *Client) AppPods(ctx context.Context, namespace, app string) ([]string, error) {
	list, err := c.cs.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "app.kubernetes.io/name=" + app,
	})
	if err != nil {
		return nil, fmt.Errorf("list pods: %w", err)
	}
	names := make([]string, 0, len(list.Items))
	for _, p := range list.Items {
		names = append(names, p.Name)
	}
	return names, nil
}

// PodInfo is one pod's live status plus its metrics.k8s.io usage for the
// pods API. CPU/Memory stay zero with MetricsOK=false when metrics-server
// is unavailable — never an error.
type PodInfo struct {
	Name      string `json:"name"`
	Phase     string `json:"phase"`
	Reason    string `json:"reason,omitempty"`
	Ready     bool   `json:"ready"`
	Restarts  int32  `json:"restarts"`
	Node      string `json:"node"`
	StartedAt string `json:"started_at,omitempty"`
	CPUMilli  int64  `json:"cpu_millicores"`
	MemoryMiB int64  `json:"memory_mib"`
	MetricsOK bool   `json:"metrics_available"`
}

// AppPodInfos lists an app's pods with status and (when metrics-server is
// reachable) per-pod CPU/memory usage. No clientset wired (some test clients
// only wire the dynamic half) -> (nil, nil).
func (c *Client) AppPodInfos(ctx context.Context, namespace, app string) ([]PodInfo, error) {
	if c.cs == nil {
		return nil, nil
	}
	selector := "app.kubernetes.io/name=" + app
	list, err := c.cs.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return nil, fmt.Errorf("list pods: %w", err)
	}
	out := make([]PodInfo, 0, len(list.Items))
	for _, p := range list.Items {
		phase := string(p.Status.Phase)
		if p.DeletionTimestamp != nil {
			phase = "Terminating"
		}
		ready := false
		for _, cond := range p.Status.Conditions {
			if cond.Type == corev1.PodReady && cond.Status == corev1.ConditionTrue {
				ready = true
				break
			}
		}
		var restarts int32
		var reason string
		for _, cst := range p.Status.ContainerStatuses {
			restarts += cst.RestartCount
			if reason == "" && cst.State.Waiting != nil && cst.State.Waiting.Reason != "" {
				reason = cst.State.Waiting.Reason
			}
		}
		startedAt := ""
		if p.Status.StartTime != nil {
			startedAt = p.Status.StartTime.Format(time.RFC3339)
		}
		out = append(out, PodInfo{
			Name: p.Name, Phase: phase, Reason: reason, Ready: ready,
			Restarts: restarts, Node: p.Spec.NodeName, StartedAt: startedAt,
		})
	}

	if c.dyn == nil {
		return out, nil
	}
	mlist, err := c.dyn.Resource(gvrByKind["PodMetrics"]).Namespace(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: selector,
	})
	if err != nil {
		return out, nil
	}
	usage := make(map[string]struct {
		cpuMilli, memMiB int64
	}, len(mlist.Items))
	for _, item := range mlist.Items {
		var cpuMilli, memMiB int64
		containers, _, _ := unstructured.NestedSlice(item.Object, "containers")
		for _, ci := range containers {
			cm, ok := ci.(map[string]any)
			if !ok {
				continue
			}
			u, _, _ := unstructured.NestedStringMap(cm, "usage")
			if q, err := resource.ParseQuantity(u["cpu"]); err == nil {
				cpuMilli += q.MilliValue()
			}
			if q, err := resource.ParseQuantity(u["memory"]); err == nil {
				memMiB += q.Value() / (1 << 20)
			}
		}
		usage[item.GetName()] = struct{ cpuMilli, memMiB int64 }{cpuMilli, memMiB}
	}
	for i := range out {
		if m, ok := usage[out[i].Name]; ok {
			out[i].CPUMilli = m.cpuMilli
			out[i].MemoryMiB = m.memMiB
			out[i].MetricsOK = true
		}
	}
	return out, nil
}

// PodLogStream streams a pod's logs, optionally following new output.
// tailLines > 0 limits output to the last N lines; sinceSeconds > 0 limits
// it to the trailing time window. Zero values mean unbounded (full history).
func (c *Client) PodLogStream(ctx context.Context, namespace, pod string, follow bool, tailLines, sinceSeconds int64) (io.ReadCloser, error) {
	opts := &corev1.PodLogOptions{Follow: follow}
	if tailLines > 0 {
		opts.TailLines = &tailLines
	}
	if sinceSeconds > 0 {
		opts.SinceSeconds = &sinceSeconds
	}
	return c.cs.CoreV1().Pods(namespace).GetLogs(pod, opts).Stream(ctx)
}

// ExecPod implements PodExecer via the pods/exec subresource.
func (c *Client) ExecPod(ctx context.Context, namespace, pod, container string, cmd []string, stdout, stderr io.Writer) error {
	if c.cfg == nil {
		return fmt.Errorf("exec unavailable: no rest config (test client?)")
	}
	req := c.cs.CoreV1().RESTClient().Post().
		Resource("pods").Namespace(namespace).Name(pod).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: container, Command: cmd,
			Stdout: true, Stderr: true,
		}, scheme.ParameterCodec)
	exec, err := remotecommand.NewSPDYExecutor(c.cfg, "POST", req.URL())
	if err != nil {
		return err
	}
	return exec.StreamWithContext(ctx, remotecommand.StreamOptions{Stdout: stdout, Stderr: stderr})
}

// WaitDeployment polls until the Deployment has at least one ready replica
// or ctx ends. Same shape as WaitJob.
func (c *Client) WaitDeployment(ctx context.Context, namespace, name string, poll time.Duration) error {
	for {
		u, err := c.dyn.Resource(gvrByKind["Deployment"]).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
		if err == nil {
			if n, _, _ := unstructured.NestedInt64(u.Object, "status", "readyReplicas"); n >= 1 {
				return nil
			}
		} else if !apierrors.IsNotFound(err) {
			return fmt.Errorf("get deployment %s: %w", name, err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(poll):
		}
	}
}

// SetDeploymentImage bumps one container's image via strategic-merge
// patch — containers merge by name, so nothing else in the pod spec is
// touched and the Deployment's rolling update takes it from there.
func (c *Client) SetDeploymentImage(ctx context.Context, namespace, name, container, image string) error {
	patch, err := json.Marshal(map[string]any{
		"spec": map[string]any{
			"template": map[string]any{
				"spec": map[string]any{
					"containers": []map[string]any{
						{"name": container, "image": image},
					},
				},
			},
		},
	})
	if err != nil {
		return err
	}
	_, err = c.dyn.Resource(gvrByKind["Deployment"]).Namespace(namespace).Patch(
		ctx, name, types.StrategicMergePatchType, patch, metav1.PatchOptions{},
	)
	return err
}

// NodeIP returns the first node's ExternalIP, falling back to InternalIP.
// Single-node K3s is the Phase 1 target, so "first node" is the node.
func (c *Client) NodeIP(ctx context.Context) (string, error) {
	nodes, err := c.cs.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return "", fmt.Errorf("list nodes: %w", err)
	}
	var internal string
	for _, n := range nodes.Items {
		for _, a := range n.Status.Addresses {
			switch a.Type {
			case corev1.NodeExternalIP:
				return a.Address, nil
			case corev1.NodeInternalIP:
				if internal == "" {
					internal = a.Address
				}
			}
		}
	}
	if internal != "" {
		return internal, nil
	}
	return "", fmt.Errorf("no node addresses found")
}

// NodesReady returns the cluster's total node count and the names of any
// nodes whose NodeReady condition isn't True.
func (c *Client) NodesReady(ctx context.Context) (total int, notReady []string, err error) {
	nodes, err := c.cs.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return 0, nil, fmt.Errorf("list nodes: %w", err)
	}
	total = len(nodes.Items)
	for _, n := range nodes.Items {
		ready := false
		for _, cond := range n.Status.Conditions {
			if cond.Type == corev1.NodeReady && cond.Status == corev1.ConditionTrue {
				ready = true
				break
			}
		}
		if !ready {
			notReady = append(notReady, n.Name)
		}
	}
	return total, notReady, nil
}

// NodeInfo is a summary of one cluster node for the nodes API.
type NodeInfo struct {
	Name        string `json:"name"`
	Role        string `json:"role"`
	Ready       bool   `json:"ready"`
	IP          string `json:"ip"`
	Version     string `json:"version"`
	CPUCapMilli int64  `json:"cpu_capacity_millicores"`
	CPUMilli    int64  `json:"cpu_used_millicores"`
	MemCapMiB   int64  `json:"memory_capacity_mib"`
	MemMiB      int64  `json:"memory_used_mib"`
	GPU         bool   `json:"gpu"`
	GPUCapacity int64  `json:"gpu_capacity"`
	MetricsOK   bool   `json:"metrics_available"`
}

// ListNodes summarizes every cluster node: role (control-plane label or
// agent), readiness, preferred address, kubelet version, and (allocatable
// capacity plus, when metrics-server is reachable, used) CPU/memory.
func (c *Client) ListNodes(ctx context.Context) ([]NodeInfo, error) {
	nodes, err := c.cs.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list nodes: %w", err)
	}

	usage := map[string]struct{ cpuMilli, memMiB int64 }{}
	if c.dyn != nil {
		if mlist, err := c.dyn.Resource(gvrByKind["NodeMetrics"]).List(ctx, metav1.ListOptions{}); err == nil {
			for _, item := range mlist.Items {
				u, _, _ := unstructured.NestedStringMap(item.Object, "usage")
				var cpuMilli, memMiB int64
				if q, err := resource.ParseQuantity(u["cpu"]); err == nil {
					cpuMilli = q.MilliValue()
				}
				if q, err := resource.ParseQuantity(u["memory"]); err == nil {
					memMiB = q.Value() / (1 << 20)
				}
				usage[item.GetName()] = struct{ cpuMilli, memMiB int64 }{cpuMilli, memMiB}
			}
		}
	}

	out := make([]NodeInfo, 0, len(nodes.Items))
	for _, n := range nodes.Items {
		role := "agent"
		if _, ok := n.Labels["node-role.kubernetes.io/control-plane"]; ok {
			role = "control-plane"
		}
		ready := false
		for _, cond := range n.Status.Conditions {
			if cond.Type == corev1.NodeReady && cond.Status == corev1.ConditionTrue {
				ready = true
				break
			}
		}
		var ip, internal string
		for _, a := range n.Status.Addresses {
			switch a.Type {
			case corev1.NodeExternalIP:
				ip = a.Address
			case corev1.NodeInternalIP:
				if internal == "" {
					internal = a.Address
				}
			}
		}
		if ip == "" {
			ip = internal
		}
		info := NodeInfo{
			Name: n.Name, Role: role, Ready: ready, IP: ip,
			Version:     n.Status.NodeInfo.KubeletVersion,
			CPUCapMilli: n.Status.Allocatable.Cpu().MilliValue(),
			MemCapMiB:   n.Status.Allocatable.Memory().Value() / (1 << 20),
		}
		if n.Labels[render.GPUNodeLabelKey] == render.GPUNodeLabelValue {
			info.GPU = true
		}
		if q, ok := n.Status.Allocatable[corev1.ResourceName(render.GPUResource)]; ok {
			info.GPUCapacity = q.Value()
		}
		if m, ok := usage[n.Name]; ok {
			info.CPUMilli = m.cpuMilli
			info.MemMiB = m.memMiB
			info.MetricsOK = true
		}
		out = append(out, info)
	}
	return out, nil
}

// ReadyPods counts how many pods matching selector in namespace have a True
// PodReady condition, alongside the total matched.
func (c *Client) ReadyPods(ctx context.Context, namespace, selector string) (ready, total int, err error) {
	list, err := c.cs.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return 0, 0, fmt.Errorf("list pods: %w", err)
	}
	total = len(list.Items)
	for _, p := range list.Items {
		for _, cond := range p.Status.Conditions {
			if cond.Type == corev1.PodReady && cond.Status == corev1.ConditionTrue {
				ready++
				break
			}
		}
	}
	return ready, total, nil
}

// GetSecretData reads a Secret's decoded data; nil map when it doesn't exist.
func (c *Client) GetSecretData(ctx context.Context, namespace, name string) (map[string][]byte, error) {
	sec, err := c.cs.CoreV1().Secrets(namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return sec.Data, nil
}

// StatefulSetReady reports whether a StatefulSet has at least one ready
// replica. Absent → (false, nil): callers poll during provisioning.
func (c *Client) StatefulSetReady(ctx context.Context, namespace, name string) (bool, error) {
	u, err := c.dyn.Resource(gvrByKind["StatefulSet"]).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	n, _, _ := unstructured.NestedInt64(u.Object, "status", "readyReplicas")
	return n >= 1, nil
}

// HasGroupVersion reports whether the cluster serves the given
// group/version (e.g. "cert-manager.io/v1") — used to detect optional
// provider CRDs before selecting them.
func (c *Client) HasGroupVersion(ctx context.Context, gv string) (bool, error) {
	_, err := c.cs.Discovery().ServerResourcesForGroupVersion(gv)
	if err != nil {
		if apierrors.IsNotFound(err) || strings.Contains(err.Error(), "not found") {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// AppMetrics sums CPU/memory usage across an app's pods via the
// metrics.k8s.io API. ok=false when metrics-server isn't available —
// callers render "metrics unavailable", never an error.
type AppMetrics struct {
	CPUMilli  int64
	MemoryMiB int64
	Pods      int
}

func (c *Client) AppMetrics(ctx context.Context, namespace, app string) (AppMetrics, bool) {
	list, err := c.dyn.Resource(gvrByKind["PodMetrics"]).Namespace(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "app.kubernetes.io/name=" + app,
	})
	if err != nil {
		return AppMetrics{}, false
	}
	var out AppMetrics
	for _, item := range list.Items {
		out.Pods++
		containers, _, _ := unstructured.NestedSlice(item.Object, "containers")
		for _, ci := range containers {
			cm, ok := ci.(map[string]any)
			if !ok {
				continue
			}
			usage, _, _ := unstructured.NestedStringMap(cm, "usage")
			if q, err := resource.ParseQuantity(usage["cpu"]); err == nil {
				out.CPUMilli += q.MilliValue()
			}
			if q, err := resource.ParseQuantity(usage["memory"]); err == nil {
				out.MemoryMiB += q.Value() / (1 << 20)
			}
		}
	}
	return out, true
}

// RestartCount sums container restarts across an app's pods via the
// app.kubernetes.io/name label — used by the monitor's crash-loop
// detector. No clientset wired -> (0, nil).
func (c *Client) RestartCount(ctx context.Context, namespace, app string) (int, error) {
	if c.cs == nil {
		return 0, nil
	}
	list, err := c.cs.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "app.kubernetes.io/name=" + app,
	})
	if err != nil {
		return 0, fmt.Errorf("list pods: %w", err)
	}
	total := 0
	for _, p := range list.Items {
		for _, cs := range p.Status.ContainerStatuses {
			total += int(cs.RestartCount)
		}
	}
	return total, nil
}

// ClusterPodUsage sums CPU/memory usage per luncur app across ALL
// namespaces in one metrics.k8s.io list — the monitor samples every app on
// one API call instead of one per app. Keys are "<namespace>/<app>" using
// the app.kubernetes.io/name label; unlabeled pods are skipped.
// ok=false when metrics-server (or the dynamic client) is unavailable.
func (c *Client) ClusterPodUsage(ctx context.Context) (map[string]AppMetrics, bool) {
	if c.dyn == nil {
		return nil, false
	}
	list, err := c.dyn.Resource(gvrByKind["PodMetrics"]).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, false
	}
	out := make(map[string]AppMetrics, len(list.Items))
	for _, item := range list.Items {
		app := item.GetLabels()["app.kubernetes.io/name"]
		if app == "" {
			continue
		}
		key := item.GetNamespace() + "/" + app
		m := out[key]
		m.Pods++
		containers, _, _ := unstructured.NestedSlice(item.Object, "containers")
		for _, ci := range containers {
			cm, ok := ci.(map[string]any)
			if !ok {
				continue
			}
			usage, _, _ := unstructured.NestedStringMap(cm, "usage")
			if q, err := resource.ParseQuantity(usage["cpu"]); err == nil {
				m.CPUMilli += q.MilliValue()
			}
			if q, err := resource.ParseQuantity(usage["memory"]); err == nil {
				m.MemoryMiB += q.Value() / (1 << 20)
			}
		}
		out[key] = m
	}
	return out, true
}

// DeploymentStatus reports ready/desired replicas; absent → zeros.
func (c *Client) DeploymentStatus(ctx context.Context, namespace, name string) (ready, desired int64, err error) {
	u, err := c.dyn.Resource(gvrByKind["Deployment"]).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return 0, 0, nil
	}
	if err != nil {
		return 0, 0, err
	}
	ready, _, _ = unstructured.NestedInt64(u.Object, "status", "readyReplicas")
	desired, _, _ = unstructured.NestedInt64(u.Object, "spec", "replicas")
	return ready, desired, nil
}

// podRequestsGPU reports whether any container in pod requests or limits
// nvidia.com/gpu devices.
func podRequestsGPU(p *corev1.Pod) bool {
	gpu := corev1.ResourceName(render.GPUResource)
	for _, ctr := range p.Spec.Containers {
		if q, ok := ctr.Resources.Requests[gpu]; ok && !q.IsZero() {
			return true
		}
		if q, ok := ctr.Resources.Limits[gpu]; ok && !q.IsZero() {
			return true
		}
	}
	return false
}

// GPUPodsRequested reports whether any non-terminal pod in the cluster
// requests nvidia.com/gpu devices. The gpucloud idle loop uses this: zero
// GPU pods for long enough means rented instances are burning money for
// nothing.
func (c *Client) GPUPodsRequested(ctx context.Context) (bool, error) {
	if c.cs == nil {
		return false, fmt.Errorf("kubernetes client not configured")
	}
	pods, err := c.cs.CoreV1().Pods("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return false, err
	}
	for _, p := range pods.Items {
		if p.Status.Phase != corev1.PodPending && p.Status.Phase != corev1.PodRunning {
			continue
		}
		if podRequestsGPU(&p) {
			return true, nil
		}
	}
	return false, nil
}

// RunningJobPods counts a Job's pods in phase Running vs all its pods, via
// the batch.kubernetes.io/job-name label the Job controller stamps on every
// pod it creates. Used by the multi-node gang guard: a run whose pods can't
// all reach Running within the window is failed instead of squatting GPUs
// half-scheduled. No clientset wired -> (0, 0, nil).
func (c *Client) RunningJobPods(ctx context.Context, ns, jobName string) (running, total int, err error) {
	if c.cs == nil {
		return 0, 0, nil
	}
	pods, err := c.cs.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{
		LabelSelector: "batch.kubernetes.io/job-name=" + jobName,
	})
	if err != nil {
		return 0, 0, fmt.Errorf("list pods: %w", err)
	}
	for _, p := range pods.Items {
		if p.Status.Phase == corev1.PodRunning {
			running++
		}
	}
	return running, len(pods.Items), nil
}

// DeleteJob removes a Job with foreground propagation, so its pods are torn
// down with it instead of orphaned. Missing job is success — callers use
// this to tear down, not to assert existence.
func (c *Client) DeleteJob(ctx context.Context, ns, name string) error {
	fg := metav1.DeletePropagationForeground
	err := c.cs.BatchV1().Jobs(ns).Delete(ctx, name, metav1.DeleteOptions{PropagationPolicy: &fg})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete job %s: %w", name, err)
	}
	return nil
}

// GPUBusyNodes reports which nodes currently have a GPU-requesting pod
// scheduled on them, for the per-instance idle loop: an instance whose node
// label isn't in this set has nothing running on it and is a destroy
// candidate. Pending pods with no NodeName yet (unscheduled) can't be
// attributed to a node, so they're recorded under the "" key instead —
// callers treat that as "freeze all destroys," since the scheduler may still
// place the pod on any node this tick.
func (c *Client) GPUBusyNodes(ctx context.Context) (map[string]bool, error) {
	if c.cs == nil {
		return nil, fmt.Errorf("kubernetes client not configured")
	}
	pods, err := c.cs.CoreV1().Pods(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	busy := map[string]bool{}
	for _, p := range pods.Items {
		if p.Status.Phase != corev1.PodPending && p.Status.Phase != corev1.PodRunning {
			continue
		}
		if !podRequestsGPU(&p) {
			continue
		}
		busy[p.Spec.NodeName] = true
	}
	return busy, nil
}
