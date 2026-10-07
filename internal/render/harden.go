package render

import (
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// defaultRequests fills requests-only CPU/memory defaults for resources the
// app didn't set (Burstable instead of BestEffort: BestEffort pods are
// evicted first and invisible to the scheduler's accounting).
func defaultRequests(in Input, c *corev1.Container) {
	if in.CPUMilli == 0 && in.DefaultCPUMilli > 0 {
		setRequest(c, corev1.ResourceCPU, *resource.NewMilliQuantity(in.DefaultCPUMilli, resource.DecimalSI))
	}
	if in.MemoryMB == 0 && in.DefaultMemoryMB > 0 {
		setRequest(c, corev1.ResourceMemory, *resource.NewQuantity(in.DefaultMemoryMB*1024*1024, resource.BinarySI))
	}
}

// setRequest sets one request without touching limits (Requests and Limits
// may share a map when both were set from the same app values).
func setRequest(c *corev1.Container, name corev1.ResourceName, q resource.Quantity) {
	req := corev1.ResourceList{}
	for k, v := range c.Resources.Requests {
		req[k] = v
	}
	req[name] = q
	c.Resources.Requests = req
}

// hardenDeployment applies the Deployment-level knobs, then hardenPod.
func hardenDeployment(in Input, kind string, dep *appsv1.Deployment) {
	if in.RevisionHistory > 0 {
		dep.Spec.RevisionHistoryLimit = int32Ptr(in.RevisionHistory)
	}
	if in.ProgressDeadline > 0 {
		dep.Spec.ProgressDeadlineSeconds = int32Ptr(in.ProgressDeadline)
	}
	spec := &dep.Spec.Template.Spec
	if in.PreStopSleep > 0 && (kind == "web" || kind == "model") && len(spec.Containers) > 0 {
		spec.Containers[0].Lifecycle = &corev1.Lifecycle{
			PreStop: &corev1.LifecycleHandler{Sleep: &corev1.SleepAction{Seconds: in.PreStopSleep}},
		}
	}
	if in.Spread && (in.Replicas >= 2 || in.AutoMax >= 2) {
		sel := &metav1.LabelSelector{MatchLabels: selector(in.AppName)}
		for _, key := range []string{"kubernetes.io/hostname", "topology.kubernetes.io/zone"} {
			spec.TopologySpreadConstraints = append(spec.TopologySpreadConstraints, corev1.TopologySpreadConstraint{
				MaxSkew: 1, TopologyKey: key, WhenUnsatisfiable: corev1.ScheduleAnyway, LabelSelector: sel,
			})
		}
	}
	hardenPod(in, spec)
}

// hardenPod applies the pod-level knobs shared by every workload kind:
// priority class and the security level.
func hardenPod(in Input, spec *corev1.PodSpec) {
	if in.PriorityClass != "" {
		spec.PriorityClassName = in.PriorityClass
	}
	applySecurity(in.Security, spec)
}

// applySecurity renders a pod security level:
//
//   - baseline: seccomp RuntimeDefault, no privilege escalation, NET_RAW
//     dropped (the rest of the runtime's default capabilities kept, so
//     entrypoints that chown/setuid still work), no service-account token.
//   - restricted: baseline + runAsNonRoot and every capability dropped
//     except NET_BIND_SERVICE.
//
// Any other value ("", "relaxed") renders nothing.
func applySecurity(level string, spec *corev1.PodSpec) {
	if level != "baseline" && level != "restricted" {
		return
	}
	spec.AutomountServiceAccountToken = boolPtr(false)
	if spec.SecurityContext == nil {
		spec.SecurityContext = &corev1.PodSecurityContext{}
	}
	spec.SecurityContext.SeccompProfile = &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}
	caps := &corev1.Capabilities{Drop: []corev1.Capability{"NET_RAW"}}
	if level == "restricted" {
		spec.SecurityContext.RunAsNonRoot = boolPtr(true)
		caps = &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}, Add: []corev1.Capability{"NET_BIND_SERVICE"}}
	}
	harden := func(c *corev1.Container) {
		if c.SecurityContext == nil {
			c.SecurityContext = &corev1.SecurityContext{}
		}
		c.SecurityContext.AllowPrivilegeEscalation = boolPtr(false)
		c.SecurityContext.Capabilities = caps
	}
	for i := range spec.Containers {
		harden(&spec.Containers[i])
	}
	for i := range spec.InitContainers {
		harden(&spec.InitContainers[i])
	}
}
