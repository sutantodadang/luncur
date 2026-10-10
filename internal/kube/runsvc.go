package kube

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
)

// OwnByJob makes the Job named jobName the owner of a namespaced object,
// so garbage collection deletes the object with the Job (a multi-node
// run's headless Service would otherwise outlive its Job's TTL forever).
func (c *Client) OwnByJob(ctx context.Context, namespace, kind, name, jobName string) error {
	if c.dyn == nil {
		return fmt.Errorf("no dynamic client")
	}
	job, err := c.dyn.Resource(gvrByKind["Job"]).Namespace(namespace).Get(ctx, jobName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get job %s: %w", jobName, err)
	}
	if job == nil || job.GetUID() == "" {
		return fmt.Errorf("job %s has no uid yet", jobName)
	}
	patch, err := json.Marshal(map[string]any{"metadata": map[string]any{"ownerReferences": []map[string]any{{
		"apiVersion": "batch/v1", "kind": "Job", "name": jobName, "uid": string(job.GetUID()),
	}}}})
	if err != nil {
		return err
	}
	gvr, ok := gvrByKind[kind]
	if !ok {
		return fmt.Errorf("no GVR for kind %q", kind)
	}
	_, err = c.dyn.Resource(gvr).Namespace(namespace).Patch(ctx, name, types.MergePatchType, patch, metav1.PatchOptions{})
	return err
}

// runServiceName matches the headless Service a multi-node run renders
// ("<app>-run-<n>", named after its Job).
var runServiceName = regexp.MustCompile(`-run-[0-9]+$`)

// DeleteOrphanRunServices deletes luncur's multi-node run Services whose
// Job is gone and that have no owner (Services created before OwnByJob, or
// whose owner patch failed). Returns how many it deleted.
func (c *Client) DeleteOrphanRunServices(ctx context.Context, namespace string) (int, error) {
	if c.dyn == nil {
		return 0, nil
	}
	list, err := c.dyn.Resource(gvrByKind["Service"]).Namespace(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "app.kubernetes.io/managed-by=luncur",
	})
	if err != nil {
		return 0, fmt.Errorf("list services: %w", err)
	}
	if list == nil {
		return 0, nil
	}
	n := 0
	for _, svc := range list.Items {
		if !orphanRunService(svc) {
			continue
		}
		_, err := c.dyn.Resource(gvrByKind["Job"]).Namespace(namespace).Get(ctx, svc.GetName(), metav1.GetOptions{})
		if !apierrors.IsNotFound(err) {
			continue // job still there (or unknown): leave it
		}
		if err := c.DeleteObject(ctx, namespace, "Service", svc.GetName()); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

func orphanRunService(svc unstructured.Unstructured) bool {
	if len(svc.GetOwnerReferences()) > 0 || !runServiceName.MatchString(svc.GetName()) {
		return false
	}
	ip, _, _ := unstructured.NestedString(svc.Object, "spec", "clusterIP")
	return ip == "None"
}
