package kube

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
)

// Rollout is a Deployment's rollout progress as the rollout gate sees it:
// the counts `kubectl rollout status` uses, plus the newest ReplicaSet's
// pods so a failing rollout can be classified without waiting out its
// progress deadline.
type Rollout struct {
	// Want is the live spec.replicas (HPA-managed apps included).
	Want      int32
	Updated   int32
	Available int32
	// Replicas counts every pod of every ReplicaSet (old ones included).
	Replicas int32
	// Observed reports status.observedGeneration >= metadata.generation.
	Observed bool
	// DeadlineExceeded is the Progressing condition's
	// ProgressDeadlineExceeded reason.
	DeadlineExceeded bool
	// Pods are the newest ReplicaSet's pods.
	Pods []RolloutPod
}

// RolloutPod is one new-ReplicaSet pod's health signals.
type RolloutPod struct {
	Name     string
	Ready    bool
	Restarts int32
	// Waiting is the first container's waiting reason (CrashLoopBackOff,
	// ImagePullBackOff, ...) and WaitingMessage its message.
	Waiting        string
	WaitingMessage string
	// LastTerminated is the most recent termination reason (OOMKilled,
	// Error, ...), current or previous.
	LastTerminated string
	// Unschedulable is the scheduler's message while the pod can't be placed.
	Unschedulable string
	// MemoryLimit is the container's memory limit, for OOM messages.
	MemoryLimit string
	Created     time.Time
}

// Done reports whether the rollout finished: every wanted replica is
// updated and available, and no old-ReplicaSet pod remains.
func (r Rollout) Done() bool {
	return r.Observed && r.Updated >= r.Want && r.Available >= r.Want && r.Replicas <= r.Updated
}

// ErrNoDeployment is returned by RolloutStatus when the Deployment is gone.
var ErrNoDeployment = errors.New("deployment not found")

// RolloutStatus reads a Deployment's rollout progress.
func (c *Client) RolloutStatus(ctx context.Context, namespace, name string) (Rollout, error) {
	if c.cs == nil {
		return Rollout{}, fmt.Errorf("rollout status: no clientset")
	}
	dep, err := c.cs.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return Rollout{}, ErrNoDeployment
	}
	if err != nil {
		return Rollout{}, fmt.Errorf("get deployment %s: %w", name, err)
	}
	r := Rollout{
		Want:      1,
		Updated:   dep.Status.UpdatedReplicas,
		Available: dep.Status.AvailableReplicas,
		Replicas:  dep.Status.Replicas,
		Observed:  dep.Status.ObservedGeneration >= dep.Generation,
	}
	if dep.Spec.Replicas != nil {
		r.Want = *dep.Spec.Replicas
	}
	for _, cond := range dep.Status.Conditions {
		if cond.Type == appsv1.DeploymentProgressing && cond.Reason == "ProgressDeadlineExceeded" {
			r.DeadlineExceeded = true
		}
	}
	rs, err := c.newestReplicaSet(ctx, dep)
	if err != nil || rs == nil {
		return r, err
	}
	sel := labels.SelectorFromSet(rs.Spec.Selector.MatchLabels).String()
	pods, err := c.cs.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: sel})
	if err != nil {
		return r, fmt.Errorf("list pods: %w", err)
	}
	for _, p := range pods.Items {
		if p.DeletionTimestamp != nil {
			continue
		}
		r.Pods = append(r.Pods, rolloutPod(p))
	}
	return r, nil
}

// newestReplicaSet finds the ReplicaSet owned by dep whose revision matches
// the Deployment's current revision.
func (c *Client) newestReplicaSet(ctx context.Context, dep *appsv1.Deployment) (*appsv1.ReplicaSet, error) {
	if dep.Spec.Selector == nil {
		return nil, nil
	}
	sel := labels.SelectorFromSet(dep.Spec.Selector.MatchLabels).String()
	list, err := c.cs.AppsV1().ReplicaSets(dep.Namespace).List(ctx, metav1.ListOptions{LabelSelector: sel})
	if err != nil {
		return nil, fmt.Errorf("list replicasets: %w", err)
	}
	rev := dep.Annotations["deployment.kubernetes.io/revision"]
	var newest *appsv1.ReplicaSet
	for i := range list.Items {
		rs := &list.Items[i]
		if !ownedBy(rs.OwnerReferences, dep.UID) {
			continue
		}
		if rev != "" && rs.Annotations["deployment.kubernetes.io/revision"] == rev {
			return rs, nil
		}
		if newest == nil || rs.CreationTimestamp.After(newest.CreationTimestamp.Time) {
			newest = rs
		}
	}
	return newest, nil
}

func ownedBy(refs []metav1.OwnerReference, uid types.UID) bool {
	for _, ref := range refs {
		if ref.Kind == "Deployment" && ref.UID == uid {
			return true
		}
	}
	return false
}

func rolloutPod(p corev1.Pod) RolloutPod {
	out := RolloutPod{Name: p.Name, Created: p.CreationTimestamp.Time}
	for _, cond := range p.Status.Conditions {
		switch {
		case cond.Type == corev1.PodReady && cond.Status == corev1.ConditionTrue:
			out.Ready = true
		case cond.Type == corev1.PodScheduled && cond.Status == corev1.ConditionFalse && cond.Reason == "Unschedulable":
			out.Unschedulable = cond.Message
		}
	}
	if len(p.Spec.Containers) > 0 {
		if q, ok := p.Spec.Containers[0].Resources.Limits[corev1.ResourceMemory]; ok {
			out.MemoryLimit = q.String()
		}
	}
	for _, cs := range p.Status.ContainerStatuses {
		out.Restarts += cs.RestartCount
		if out.Waiting == "" && cs.State.Waiting != nil {
			out.Waiting, out.WaitingMessage = cs.State.Waiting.Reason, cs.State.Waiting.Message
		}
		if out.LastTerminated == "" {
			if t := cs.State.Terminated; t != nil {
				out.LastTerminated = t.Reason
			} else if t := cs.LastTerminationState.Terminated; t != nil {
				out.LastTerminated = t.Reason
			}
		}
	}
	return out
}

// RolloutVerdict is the gate's reading of one RolloutStatus sample.
type RolloutVerdict struct {
	Done   bool
	Failed bool
	// Reason is the one-line "what broke" for a failed rollout.
	Reason string
}

// imagePullGrace is how long an image pull may back off before the gate
// calls it failed (registries hiccup; a typo never recovers).
const imagePullGrace = 60 * time.Second

// Judge classifies a rollout sample. started is when the deploy began
// rolling out, timeout the app's rollout timeout. Fast-fail signals fail
// the rollout before its deadline: crash loops, OOM kills and bad container
// config immediately, image pulls after a one-minute grace.
func Judge(r Rollout, now, started time.Time, timeout time.Duration) RolloutVerdict {
	if r.Done() {
		return RolloutVerdict{Done: true}
	}
	for _, p := range r.Pods {
		if p.Ready {
			continue
		}
		switch {
		case p.LastTerminated == "OOMKilled":
			limit := p.MemoryLimit
			if limit == "" {
				limit = "memory"
			}
			return failed("OOM-killed: %s limit hit", limit)
		case p.Waiting == "CrashLoopBackOff" || p.Restarts >= 3:
			why := p.LastTerminated
			if why == "" {
				why = "container exited"
			}
			return failed("crash-looping: %s (%d restarts)", why, p.Restarts)
		case p.Waiting == "CreateContainerConfigError" || p.Waiting == "InvalidImageName":
			return failed("bad container config: %s", firstLine(p.WaitingMessage, p.Waiting))
		case (p.Waiting == "ImagePullBackOff" || p.Waiting == "ErrImagePull") && now.Sub(p.Created) >= imagePullGrace:
			return failed("image pull failed: %s", firstLine(p.WaitingMessage, p.Waiting))
		}
	}
	if r.DeadlineExceeded || now.Sub(started) >= timeout {
		for _, p := range r.Pods {
			if p.Unschedulable != "" {
				return failed("unschedulable: %s", firstLine(p.Unschedulable, "no node fits"))
			}
		}
		return failed("rollout timed out after %s (%d/%d new pods ready)", timeout.Round(time.Second), r.Available, r.Want)
	}
	return RolloutVerdict{}
}

func failed(format string, args ...any) RolloutVerdict {
	return RolloutVerdict{Failed: true, Reason: fmt.Sprintf(format, args...)}
}

func firstLine(s, fallback string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	if s == "" {
		return fallback
	}
	return s
}

// CanWatchRollouts reports whether RolloutStatus can work (it needs the
// typed clientset, which dynamic-only test clients lack).
func (c *Client) CanWatchRollouts() bool { return c != nil && c.cs != nil }

// ServerVersionAtLeast reports whether the API server is at least
// major.minor. Unknown versions (fakes, discovery errors) report false, so
// version-gated features stay off when in doubt.
func (c *Client) ServerVersionAtLeast(major, minor int) bool {
	if c == nil || c.cs == nil {
		return false
	}
	v, err := c.cs.Discovery().ServerVersion()
	if err != nil {
		return false
	}
	maj, err1 := strconv.Atoi(strings.TrimRight(v.Major, "+"))
	min, err2 := strconv.Atoi(strings.TrimRight(v.Minor, "+"))
	if err1 != nil || err2 != nil {
		return false
	}
	return maj > major || (maj == major && min >= minor)
}
