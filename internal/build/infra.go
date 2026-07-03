package build

import (
	"encoding/json"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/sutantodadang/luncur/internal/render"
)

const systemNamespace = "luncur-system"

// SystemObjects returns the manifests luncur applies once at boot: the
// luncur-system namespace, the registry Deployment+Service, and the data
// and registry PVCs.
func SystemObjects(dataPVC, registryPVC, registryImage string) ([]render.Object, error) {
	var objs []render.Object
	add := func(kind string, v any) error {
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		objs = append(objs, render.Object{Kind: kind, JSON: b})
		return nil
	}

	// No pod-security.kubernetes.io/enforce: restricted here — the BuildKit
	// builder needs latitude the restricted profile denies, and this
	// namespace holds only luncur-operated workloads, not tenant apps.
	ns := &corev1.Namespace{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Namespace"},
		ObjectMeta: metav1.ObjectMeta{
			Name:   systemNamespace,
			Labels: map[string]string{"app.kubernetes.io/managed-by": "luncur"},
		},
	}
	if err := add("Namespace", ns); err != nil {
		return nil, err
	}

	if err := add("PersistentVolumeClaim", pvc(dataPVC, "2Gi")); err != nil {
		return nil, err
	}
	if err := add("PersistentVolumeClaim", pvc(registryPVC, "10Gi")); err != nil {
		return nil, err
	}

	registryLabels := map[string]string{"app.kubernetes.io/name": "registry"}
	dep := &appsv1.Deployment{
		TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "registry",
			Namespace: systemNamespace,
			Labels:    registryLabels,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr(int32(1)),
			Selector: &metav1.LabelSelector{MatchLabels: registryLabels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: registryLabels},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:  "registry",
						Image: registryImage,
						Ports: []corev1.ContainerPort{{ContainerPort: 5000}},
						VolumeMounts: []corev1.VolumeMount{
							{Name: "registry-data", MountPath: "/var/lib/registry"},
						},
					}},
					Volumes: []corev1.Volume{{
						Name: "registry-data",
						VolumeSource: corev1.VolumeSource{
							PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
								ClaimName: registryPVC,
							},
						},
					}},
				},
			},
		},
	}
	if err := add("Deployment", dep); err != nil {
		return nil, err
	}

	svc := &corev1.Service{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Service"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "registry",
			Namespace: systemNamespace,
		},
		Spec: corev1.ServiceSpec{
			Selector: registryLabels,
			Ports: []corev1.ServicePort{{
				Port:       5000,
				TargetPort: intstr.FromInt32(5000),
			}},
		},
	}
	if err := add("Service", svc); err != nil {
		return nil, err
	}

	return objs, nil
}

// ponytail: ReadWriteOnce assumes single-node K3s (Phase 1 target). If
// luncur ever runs multi-node, the upgrade path is RWX access mode or
// moving registry/data storage to an object store.
func pvc(name, size string) *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "PersistentVolumeClaim"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: systemNamespace,
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: resource.MustParse(size),
				},
			},
		},
	}
}
