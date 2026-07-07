// Package gpu renders the cluster objects luncur needs to schedule GPU
// workloads: the "nvidia" RuntimeClass and the NVIDIA device plugin
// DaemonSet, pinned to nodes joined with `luncur join --gpu` (which labels
// them luncur.dev/gpu=true).
package gpu

import (
	"encoding/json"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	nodev1 "k8s.io/api/node/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/sutantodadang/luncur/internal/render"
)

// DaemonSetName is the device plugin DaemonSet's name in the system
// namespace.
const DaemonSetName = "nvidia-device-plugin"

// devicePluginImage is pinned; bumped deliberately, never floated.
const devicePluginImage = "nvcr.io/nvidia/k8s-device-plugin:v0.17.1"

func ptr[T any](v T) *T { return &v }

// Objects returns the manifests the server applies when the first GPU node
// appears: the nvidia RuntimeClass (cluster-scoped) and the device plugin
// DaemonSet (namespaced, normally luncur-system). Server-side apply makes
// re-applying them a no-op.
func Objects(namespace string) ([]render.Object, error) {
	labels := map[string]string{
		"app.kubernetes.io/name":       DaemonSetName,
		"app.kubernetes.io/managed-by": "luncur",
	}

	rc := &nodev1.RuntimeClass{
		TypeMeta:   metav1.TypeMeta{APIVersion: "node.k8s.io/v1", Kind: "RuntimeClass"},
		ObjectMeta: metav1.ObjectMeta{Name: render.GPURuntimeClass, Labels: labels},
		Handler:    "nvidia",
	}

	rcName := render.GPURuntimeClass
	ds := &appsv1.DaemonSet{
		TypeMeta:   metav1.TypeMeta{APIVersion: "apps/v1", Kind: "DaemonSet"},
		ObjectMeta: metav1.ObjectMeta{Name: DaemonSetName, Namespace: namespace, Labels: labels},
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app.kubernetes.io/name": DaemonSetName}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					RuntimeClassName:  &rcName,
					NodeSelector:      map[string]string{render.GPUNodeLabelKey: render.GPUNodeLabelValue},
					PriorityClassName: "system-node-critical",
					Tolerations: []corev1.Toleration{{
						Key: render.GPUResource, Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule,
					}},
					Containers: []corev1.Container{{
						Name:  DaemonSetName,
						Image: devicePluginImage,
						Env: []corev1.EnvVar{{
							// Keep the plugin alive on nodes where the driver
							// isn't ready yet instead of crash-looping.
							Name: "FAIL_ON_INIT_ERROR", Value: "false",
						}},
						SecurityContext: &corev1.SecurityContext{
							AllowPrivilegeEscalation: ptr(false),
							Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
						},
						VolumeMounts: []corev1.VolumeMount{{
							Name: "device-plugin", MountPath: "/var/lib/kubelet/device-plugins",
						}},
					}},
					Volumes: []corev1.Volume{{
						Name: "device-plugin",
						VolumeSource: corev1.VolumeSource{
							HostPath: &corev1.HostPathVolumeSource{Path: "/var/lib/kubelet/device-plugins"},
						},
					}},
				},
			},
		},
	}

	var objs []render.Object
	for _, o := range []struct {
		kind string
		v    any
	}{{"RuntimeClass", rc}, {"DaemonSet", ds}} {
		b, err := json.Marshal(o.v)
		if err != nil {
			return nil, err
		}
		objs = append(objs, render.Object{Kind: o.kind, JSON: b})
	}
	return objs, nil
}

// QuotaObjectName is the ResourceQuota luncur manages in each project
// namespace when a GPU quota is set.
const QuotaObjectName = "luncur-gpu"

// QuotaObject renders the project-namespace ResourceQuota that caps total
// requested nvidia.com/gpu devices at n. K8s enforces it at pod admission;
// luncur's static validation only exists for friendlier errors.
func QuotaObject(namespace string, n int64) (render.Object, error) {
	rq := corev1.ResourceQuota{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "ResourceQuota"},
		ObjectMeta: metav1.ObjectMeta{Name: QuotaObjectName, Namespace: namespace},
		Spec: corev1.ResourceQuotaSpec{
			Hard: corev1.ResourceList{
				corev1.ResourceName("requests." + render.GPUResource): *resource.NewQuantity(n, resource.DecimalSI),
			},
		},
	}
	b, err := json.Marshal(rq)
	if err != nil {
		return render.Object{}, err
	}
	return render.Object{Kind: "ResourceQuota", JSON: b}, nil
}
