// Package up renders luncur's own self-deploy manifests: the ServiceAccount,
// ClusterRoleBinding, Deployment, Service, and Ingress that run luncur itself
// on the K3s cluster it manages.
package up

import (
	"encoding/json"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	netv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/sutantodadang/luncur/internal/render"
)

const systemNamespace = "luncur-system"

// BootstrapSecretName is the Secret luncur's Deployment reads the initial
// admin bootstrap token from (key "admin").
const BootstrapSecretName = "luncur-bootstrap"

// Params configures luncur's own self-deploy manifests.
type Params struct {
	Image        string // luncur server image
	ExternalIP   string
	BuilderImage string
}

func ptr[T any](v T) *T { return &v }

// PanelHost returns the sslip.io host luncur's own panel Ingress is served
// on, derived from the cluster's external IP.
func PanelHost(ip string) string {
	return "panel." + ip + ".sslip.io"
}

// LuncurObjects returns the manifests luncur applies to deploy itself:
// ServiceAccount, ClusterRoleBinding, Deployment, Service, and Ingress —
// all in the luncur-system namespace, labeled app.kubernetes.io/managed-by:
// luncur.
func LuncurObjects(p Params) ([]render.Object, error) {
	var objs []render.Object
	add := func(kind string, v any) error {
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		objs = append(objs, render.Object{Kind: kind, JSON: b})
		return nil
	}

	labels := map[string]string{
		"app.kubernetes.io/name":       "luncur",
		"app.kubernetes.io/managed-by": "luncur",
	}

	sa := &corev1.ServiceAccount{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "ServiceAccount"},
		ObjectMeta: metav1.ObjectMeta{Name: "luncur", Namespace: systemNamespace, Labels: labels},
	}
	if err := add("ServiceAccount", sa); err != nil {
		return nil, err
	}

	// ponytail: cluster-admin — luncur manages arbitrary namespaces/CRDs;
	// a scoped ClusterRole is the Phase 2 hardening path.
	crb := &rbacv1.ClusterRoleBinding{
		TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "ClusterRoleBinding"},
		ObjectMeta: metav1.ObjectMeta{Name: "luncur-admin", Labels: labels},
		RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: "cluster-admin"},
		Subjects:   []rbacv1.Subject{{Kind: "ServiceAccount", Name: "luncur", Namespace: systemNamespace}},
	}
	if err := add("ClusterRoleBinding", crb); err != nil {
		return nil, err
	}

	dep := &appsv1.Deployment{
		TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "luncur",
			Namespace: systemNamespace,
			Labels:    labels,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr(int32(1)),
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					ServiceAccountName: "luncur",
					Containers: []corev1.Container{{
						Name:  "luncur",
						Image: p.Image,
						Args: []string{
							"serve",
							"--listen", ":8080",
							"--db", "/var/lib/luncur/luncur.db",
							"--data-dir", "/var/lib/luncur/data",
							"--secret-key-file", "/var/lib/luncur/luncur.key",
							"--external-ip", p.ExternalIP,
							"--builder-image", p.BuilderImage,
							"--bootstrap-admin", "$(BOOTSTRAP_ADMIN)",
						},
						Env: []corev1.EnvVar{{
							Name: "BOOTSTRAP_ADMIN",
							ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
								LocalObjectReference: corev1.LocalObjectReference{Name: BootstrapSecretName},
								Key:                  "admin",
							}},
						}},
						Ports: []corev1.ContainerPort{{ContainerPort: 8080}},
						VolumeMounts: []corev1.VolumeMount{
							{Name: "luncur-data", MountPath: "/var/lib/luncur"},
						},
						ReadinessProbe: &corev1.Probe{
							ProbeHandler: corev1.ProbeHandler{
								HTTPGet: &corev1.HTTPGetAction{
									Path: "/v1/health",
									Port: intstr.FromInt32(8080),
								},
							},
						},
					}},
					Volumes: []corev1.Volume{{
						Name: "luncur-data",
						VolumeSource: corev1.VolumeSource{
							PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
								ClaimName: "luncur-data",
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
			Name:      "luncur",
			Namespace: systemNamespace,
			Labels:    labels,
		},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{"app.kubernetes.io/name": "luncur"},
			Ports: []corev1.ServicePort{{
				Port:       80,
				TargetPort: intstr.FromInt32(8080),
			}},
		},
	}
	if err := add("Service", svc); err != nil {
		return nil, err
	}

	pathType := netv1.PathTypePrefix
	ing := &netv1.Ingress{
		TypeMeta:   metav1.TypeMeta{APIVersion: "networking.k8s.io/v1", Kind: "Ingress"},
		ObjectMeta: metav1.ObjectMeta{Name: "luncur", Namespace: systemNamespace, Labels: labels},
		Spec: netv1.IngressSpec{Rules: []netv1.IngressRule{{
			Host: PanelHost(p.ExternalIP),
			IngressRuleValue: netv1.IngressRuleValue{HTTP: &netv1.HTTPIngressRuleValue{
				Paths: []netv1.HTTPIngressPath{{
					Path: "/", PathType: &pathType,
					Backend: netv1.IngressBackend{Service: &netv1.IngressServiceBackend{
						Name: "luncur", Port: netv1.ServiceBackendPort{Number: 80},
					}},
				}},
			}},
		}}},
	}
	if err := add("Ingress", ing); err != nil {
		return nil, err
	}

	return objs, nil
}
