// Package render: project-namespace resource quotas (D4). A ResourceQuota
// covering cpu/memory makes admission reject any pod without explicit
// resource limits, so a LimitRange rides along to inject defaults — apps
// with unset resources keep scheduling instead of failing at admission.
package render

import (
	"encoding/json"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ProjectQuotaName is the ResourceQuota luncur manages in each project
// namespace when a CPU/memory quota is set.
const ProjectQuotaName = "luncur-quota"

// LimitRangeName is the LimitRange luncur manages alongside the project
// quota, supplying default requests/limits for containers that set none.
const LimitRangeName = "luncur-defaults"

// ProjectQuotaObjects renders the project-namespace ResourceQuota and
// LimitRange that back `luncur project quota`. cpuMilli and memMB are each
// independently optional (0 = unlimited for that resource) — the
// ResourceQuota's Hard map only gets the entries for the ones set. The
// LimitRange is always rendered whenever this function is called (i.e.
// whenever either quota is > 0): a ResourceQuota covering limits.cpu/
// limits.memory makes admission reject any pod without a limit for that
// resource, so the LimitRange's container defaults keep apps that never set
// their own resources schedulable.
func ProjectQuotaObjects(namespace string, cpuMilli, memMB int64) ([]Object, error) {
	hard := corev1.ResourceList{}
	if cpuMilli > 0 {
		hard[corev1.ResourceName("limits.cpu")] = *resource.NewMilliQuantity(cpuMilli, resource.DecimalSI)
	}
	if memMB > 0 {
		hard[corev1.ResourceName("limits.memory")] = *resource.NewQuantity(memMB*1024*1024, resource.BinarySI)
	}
	rq := corev1.ResourceQuota{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "ResourceQuota"},
		ObjectMeta: metav1.ObjectMeta{Name: ProjectQuotaName, Namespace: namespace},
		Spec:       corev1.ResourceQuotaSpec{Hard: hard},
	}

	defaults := corev1.ResourceList{
		corev1.ResourceCPU:    *resource.NewMilliQuantity(500, resource.DecimalSI),
		corev1.ResourceMemory: *resource.NewQuantity(512*1024*1024, resource.BinarySI),
	}
	lr := corev1.LimitRange{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "LimitRange"},
		ObjectMeta: metav1.ObjectMeta{Name: LimitRangeName, Namespace: namespace},
		Spec: corev1.LimitRangeSpec{
			Limits: []corev1.LimitRangeItem{{
				Type:           corev1.LimitTypeContainer,
				Default:        defaults,
				DefaultRequest: defaults,
			}},
		},
	}

	var objs []Object
	for _, o := range []struct {
		kind string
		v    any
	}{{"ResourceQuota", &rq}, {"LimitRange", &lr}} {
		b, err := json.Marshal(o.v)
		if err != nil {
			return nil, err
		}
		objs = append(objs, Object{Kind: o.kind, JSON: b})
	}
	return objs, nil
}
