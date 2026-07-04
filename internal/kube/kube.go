// Package kube applies luncur-rendered manifests to the cluster with
// server-side apply (fieldManager=luncur), so user edits made through
// luncur's override system merge cleanly with cluster state.
package kube

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

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
	"PersistentVolumeClaim": {Group: "", Version: "v1", Resource: "persistentvolumeclaims"},
	"ServiceAccount":        {Group: "", Version: "v1", Resource: "serviceaccounts"},
	"ClusterRole":           {Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterroles"},
	"ClusterRoleBinding":    {Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterrolebindings"},
	"HelmChartConfig":       {Group: "helm.cattle.io", Version: "v1", Resource: "helmchartconfigs"},
	"ClusterIssuer":         {Group: "cert-manager.io", Version: "v1", Resource: "clusterissuers"},
	"StatefulSet":           {Group: "apps", Version: "v1", Resource: "statefulsets"},
	"PodMetrics":            {Group: "metrics.k8s.io", Version: "v1beta1", Resource: "pods"},
}

// clusterScoped marks kinds Apply must patch without a namespace.
var clusterScoped = map[string]bool{
	"Namespace":          true,
	"ClusterRole":        true,
	"ClusterRoleBinding": true,
	"ClusterIssuer":      true,
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

func (c *Client) EnsureNamespace(ctx context.Context, name string) error {
	ns := fmt.Sprintf(
		`{"apiVersion":"v1","kind":"Namespace","metadata":{"name":%q,"labels":{"app.kubernetes.io/managed-by":"luncur","pod-security.kubernetes.io/enforce":"restricted"}}}`,
		name,
	)
	_, err := c.dyn.Resource(gvrByKind["Namespace"]).Patch(
		ctx, name, types.ApplyPatchType, []byte(ns), applyOpts(),
	)
	return err
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

// PodLogStream streams a pod's logs, optionally following new output.
func (c *Client) PodLogStream(ctx context.Context, namespace, pod string, follow bool) (io.ReadCloser, error) {
	return c.cs.CoreV1().Pods(namespace).GetLogs(pod, &corev1.PodLogOptions{Follow: follow}).Stream(ctx)
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
