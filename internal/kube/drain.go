package kube

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// Cordon marks a node (un)schedulable.
func (c *Client) Cordon(ctx context.Context, node string, on bool) error {
	if c.cs == nil {
		return fmt.Errorf("kubernetes client not configured")
	}
	patch, _ := json.Marshal(map[string]any{"spec": map[string]any{"unschedulable": on}})
	if _, err := c.cs.CoreV1().Nodes().Patch(ctx, node, types.StrategicMergePatchType, patch, metav1.PatchOptions{}); err != nil {
		return fmt.Errorf("cordon %s: %w", node, err)
	}
	return nil
}

// DrainProgress is a drain's running tally, reported after every pass.
type DrainProgress struct {
	Total   int `json:"total"`
	Evicted int `json:"evicted"`
	// Blocked lists pods whose eviction a PodDisruptionBudget refused on
	// the last pass ("namespace/name: reason").
	Blocked []string `json:"blocked,omitempty"`
}

// DrainOptions tunes Drain. IsLast marks pods to evict only after every
// other pod is gone (luncur's own server pod: draining its node must not
// kill the process doing the drain before the rest is done).
type DrainOptions struct {
	Timeout  time.Duration
	Retry    time.Duration
	IsLast   func(corev1.Pod) bool
	Progress func(DrainProgress)
}

// drainable reports whether Drain should evict a pod: DaemonSet pods are
// recreated on the node anyway, mirror (static) pods can't be evicted, and
// finished pods hold nothing.
func drainable(p corev1.Pod) bool {
	if p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed || p.DeletionTimestamp != nil {
		return false
	}
	if _, mirror := p.Annotations[corev1.MirrorPodAnnotationKey]; mirror {
		return false
	}
	for _, ref := range p.OwnerReferences {
		if ref.Kind == "DaemonSet" {
			return false
		}
	}
	return true
}

// Drain cordons a node, then evicts its pods through the Eviction API —
// which honors PodDisruptionBudgets: a PDB-blocked eviction (429) is
// retried every opts.Retry until opts.Timeout, and the pods still blocked
// then are reported in the returned progress and error.
func (c *Client) Drain(ctx context.Context, node string, opts DrainOptions) (DrainProgress, error) {
	if c.cs == nil {
		return DrainProgress{}, fmt.Errorf("kubernetes client not configured")
	}
	if opts.Timeout == 0 {
		opts.Timeout = 5 * time.Minute
	}
	if opts.Retry == 0 {
		opts.Retry = 5 * time.Second
	}
	if err := c.Cordon(ctx, node, true); err != nil {
		return DrainProgress{}, err
	}
	list, err := c.cs.CoreV1().Pods(metav1.NamespaceAll).List(ctx, metav1.ListOptions{FieldSelector: "spec.nodeName=" + node})
	if err != nil {
		return DrainProgress{}, fmt.Errorf("list pods on %s: %w", node, err)
	}
	var first, last []corev1.Pod
	for _, p := range list.Items {
		if !drainable(p) {
			continue
		}
		if opts.IsLast != nil && opts.IsLast(p) {
			last = append(last, p)
		} else {
			first = append(first, p)
		}
	}
	prog := DrainProgress{Total: len(first) + len(last)}
	deadline := time.Now().Add(opts.Timeout)
	for _, batch := range [][]corev1.Pod{first, last} {
		pending := batch
		for len(pending) > 0 {
			prog.Blocked = nil
			var retry []corev1.Pod
			for _, p := range pending {
				err := c.cs.PolicyV1().Evictions(p.Namespace).Evict(ctx, &policyv1.Eviction{
					ObjectMeta: metav1.ObjectMeta{Name: p.Name, Namespace: p.Namespace},
				})
				switch {
				case err == nil || apierrors.IsNotFound(err):
					prog.Evicted++
				case apierrors.IsTooManyRequests(err):
					retry = append(retry, p)
					prog.Blocked = append(prog.Blocked, p.Namespace+"/"+p.Name+": "+firstLine(err.Error(), "disruption budget"))
				default:
					return prog, fmt.Errorf("evict %s/%s: %w", p.Namespace, p.Name, err)
				}
			}
			if opts.Progress != nil {
				opts.Progress(prog)
			}
			pending = retry
			if len(pending) == 0 {
				break
			}
			if time.Now().After(deadline) {
				return prog, fmt.Errorf("drain %s timed out after %s with %d pod(s) blocked by disruption budgets", node, opts.Timeout, len(pending))
			}
			select {
			case <-ctx.Done():
				return prog, ctx.Err()
			case <-time.After(opts.Retry):
			}
		}
	}
	return prog, nil
}
