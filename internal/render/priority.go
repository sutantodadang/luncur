package render

import (
	"encoding/json"

	corev1 "k8s.io/api/core/v1"
	schedulingv1 "k8s.io/api/scheduling/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Priority classes luncur manages. System outranks apps outranks batch, so
// under node pressure the kubelet evicts batch work first and luncur and
// its databases last.
const (
	PriorityClassSystem = "luncur-system"
	PriorityClassApp    = "luncur-app"
	PriorityClassBatch  = "luncur-batch"
)

// PriorityClasses renders the cluster-scoped PriorityClasses (applied by
// `luncur up` before the luncur Deployment that references luncur-system,
// and re-asserted by the server at boot). Batch never preempts.
func PriorityClasses() []Object {
	never := corev1.PreemptNever
	classes := []struct {
		name string
		pc   schedulingv1.PriorityClass
	}{
		{PriorityClassSystem, schedulingv1.PriorityClass{Value: 1000000, Description: "luncur control plane and managed databases"}},
		{PriorityClassApp, schedulingv1.PriorityClass{Value: 10000, Description: "luncur apps (web, worker, model)"}},
		{PriorityClassBatch, schedulingv1.PriorityClass{Value: 100, Description: "luncur batch work (builds, job runs, sweeps, pipelines)", PreemptionPolicy: &never}},
	}
	out := make([]Object, 0, len(classes))
	for _, c := range classes {
		pc := c.pc
		pc.TypeMeta = metav1.TypeMeta{APIVersion: "scheduling.k8s.io/v1", Kind: "PriorityClass"}
		pc.ObjectMeta = metav1.ObjectMeta{Name: c.name, Labels: map[string]string{"app.kubernetes.io/managed-by": "luncur"}}
		b, _ := json.Marshal(pc) // static struct: cannot fail
		out = append(out, Object{Kind: "PriorityClass", JSON: b})
	}
	return out
}
