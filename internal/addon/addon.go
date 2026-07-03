// Package addon renders managed Postgres/Redis instances: StatefulSet +
// headless Service + credentials Secret, all in the app project's
// namespace.
package addon

import (
	"encoding/json"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/sutantodadang/luncur/internal/render"
)

// Creds holds addon credentials; Redis only uses Password.
type Creds struct{ User, Password, DB string }

// Params configures one addon instance's manifests.
type Params struct {
	Namespace, Type, Name, Version string
	SizeGB                         int
	Creds                          Creds
}

func ServiceName(name string) string { return "addon-" + name }
func SecretName(name string) string  { return "addon-" + name + "-creds" }

func ptr[T any](v T) *T { return &v }

func labels(name string) map[string]string {
	return map[string]string{
		"app.kubernetes.io/managed-by": "luncur",
		"luncur.dev/addon":             name,
	}
}

// Render builds the manifest set for one addon instance: StatefulSet,
// headless Service, and credentials Secret.
func Render(p Params) ([]render.Object, error) {
	if p.Type != "postgres" && p.Type != "redis" {
		return nil, fmt.Errorf("unsupported addon type %q (postgres|redis)", p.Type)
	}

	var objs []render.Object
	add := func(kind string, v any) error {
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		objs = append(objs, render.Object{Kind: kind, JSON: b})
		return nil
	}

	lbls := labels(p.Name)
	meta := metav1.ObjectMeta{Name: ServiceName(p.Name), Namespace: p.Namespace, Labels: lbls}

	var (
		container  corev1.Container
		port       int32
		stringData map[string]string
	)

	switch p.Type {
	case "postgres":
		port = 5432
		container = corev1.Container{
			Name:  "postgres",
			Image: fmt.Sprintf("postgres:%s-alpine", p.Version),
			Env: []corev1.EnvVar{
				{Name: "PGDATA", Value: "/var/lib/postgresql/data/pgdata"},
			},
			EnvFrom: []corev1.EnvFromSource{{
				SecretRef: &corev1.SecretEnvSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: SecretName(p.Name)},
				},
			}},
			Ports: []corev1.ContainerPort{{ContainerPort: port}},
			VolumeMounts: []corev1.VolumeMount{
				{Name: "data", MountPath: "/var/lib/postgresql/data"},
			},
			ReadinessProbe: &corev1.Probe{
				ProbeHandler: corev1.ProbeHandler{
					Exec: &corev1.ExecAction{Command: []string{"pg_isready", "-U", "app"}},
				},
			},
		}
		stringData = map[string]string{
			"POSTGRES_USER":     p.Creds.User,
			"POSTGRES_PASSWORD": p.Creds.Password,
			"POSTGRES_DB":       p.Creds.DB,
		}
	case "redis":
		port = 6379
		container = corev1.Container{
			Name:  "redis",
			Image: fmt.Sprintf("redis:%s-alpine", p.Version),
			Args:  []string{"--requirepass", "$(REDIS_PASSWORD)"},
			Env: []corev1.EnvVar{{
				Name: "REDIS_PASSWORD",
				ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: SecretName(p.Name)},
					Key:                  "REDIS_PASSWORD",
				}},
			}},
			Ports: []corev1.ContainerPort{{ContainerPort: port}},
			VolumeMounts: []corev1.VolumeMount{
				{Name: "data", MountPath: "/data"},
			},
			ReadinessProbe: &corev1.Probe{
				ProbeHandler: corev1.ProbeHandler{
					TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt32(port)},
				},
			},
		}
		stringData = map[string]string{
			"REDIS_PASSWORD": p.Creds.Password,
		}
	}

	sts := &appsv1.StatefulSet{
		TypeMeta:   metav1.TypeMeta{APIVersion: "apps/v1", Kind: "StatefulSet"},
		ObjectMeta: meta,
		Spec: appsv1.StatefulSetSpec{
			Replicas:    ptr(int32(1)),
			ServiceName: ServiceName(p.Name),
			Selector:    &metav1.LabelSelector{MatchLabels: lbls},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: lbls},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{container}},
			},
			VolumeClaimTemplates: []corev1.PersistentVolumeClaim{{
				ObjectMeta: metav1.ObjectMeta{Name: "data", Labels: lbls},
				Spec: corev1.PersistentVolumeClaimSpec{
					AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
					Resources: corev1.VolumeResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceStorage: resource.MustParse(fmt.Sprintf("%dGi", p.SizeGB)),
						},
					},
				},
			}},
		},
	}
	if err := add("StatefulSet", sts); err != nil {
		return nil, err
	}

	svc := &corev1.Service{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Service"},
		ObjectMeta: meta,
		Spec: corev1.ServiceSpec{
			ClusterIP: "None",
			Selector:  lbls,
			Ports:     []corev1.ServicePort{{Port: port, TargetPort: intstr.FromInt32(port)}},
		},
	}
	if err := add("Service", svc); err != nil {
		return nil, err
	}

	sec := &corev1.Secret{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
		ObjectMeta: metav1.ObjectMeta{Name: SecretName(p.Name), Namespace: p.Namespace, Labels: lbls},
		Type:       corev1.SecretTypeOpaque,
		StringData: stringData,
	}
	if err := add("Secret", sec); err != nil {
		return nil, err
	}

	return objs, nil
}
