// Package kube applies luncur-rendered manifests to the cluster with
// server-side apply (fieldManager=luncur), so user edits made through
// luncur's override system merge cleanly with cluster state.
package kube

import (
	"context"
	"encoding/json"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/sutantodadang/luncur/internal/render"
)

var gvrByKind = map[string]schema.GroupVersionResource{
	"Deployment": {Group: "apps", Version: "v1", Resource: "deployments"},
	"Service":    {Group: "", Version: "v1", Resource: "services"},
	"Ingress":    {Group: "networking.k8s.io", Version: "v1", Resource: "ingresses"},
	"Secret":     {Group: "", Version: "v1", Resource: "secrets"},
	"Namespace":  {Group: "", Version: "v1", Resource: "namespaces"},
}

type Client struct {
	dyn dynamic.Interface
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
	return &Client{dyn: dyn}, nil
}

func NewFromDynamic(dyn dynamic.Interface) *Client { return &Client{dyn: dyn} }

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
		_, err = c.dyn.Resource(gvr).Namespace(namespace).Patch(
			ctx, name, types.ApplyPatchType, o.JSON, applyOpts(),
		)
		if err != nil {
			return fmt.Errorf("apply %s/%s: %w", o.Kind, name, err)
		}
	}
	return nil
}

// DeleteAppObjects removes everything Render produces for an app.
// NotFound is fine — destroy must be idempotent.
func (c *Client) DeleteAppObjects(ctx context.Context, namespace, app string) error {
	targets := []struct{ kind, name string }{
		{"Deployment", app},
		{"Service", app},
		{"Ingress", app},
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
