// Package render turns luncur's app model into Kubernetes manifests.
// Objects are rendered from the model, then per-kind user overrides
// (strategic merge patches) are applied — so user customizations survive
// every redeploy. Pure package: no cluster access.
package render

import (
	"encoding/json"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	netv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/yaml"
)

type Input struct {
	AppName   string
	Namespace string
	Image     string
	Host      string
	Port      int32
	Replicas  int32
	// Overrides maps Kind -> strategic-merge-patch JSON. Applied by Task 6.
	Overrides map[string]string
}

type Object struct {
	Kind string
	JSON []byte
}

type Rendered struct {
	Objects []Object
}

func SecretName(app string) string { return app + "-env" }

func labels(app string) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":       app,
		"app.kubernetes.io/managed-by": "luncur",
	}
}

func selector(app string) map[string]string {
	return map[string]string{"app.kubernetes.io/name": app}
}

func meta(in Input, name string) metav1.ObjectMeta {
	return metav1.ObjectMeta{Name: name, Namespace: in.Namespace, Labels: labels(in.AppName)}
}

// Render builds the manifest set for one app. env is plaintext (the caller
// unseals); empty env omits the Secret entirely.
func Render(in Input, env map[string]string) (Rendered, error) {
	if in.AppName == "" || in.Namespace == "" || in.Image == "" || in.Host == "" || in.Port < 1 {
		return Rendered{}, fmt.Errorf("render: AppName, Namespace, Image, Host, and Port are required")
	}

	var objs []Object
	add := func(kind string, v any) error {
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		objs = append(objs, Object{Kind: kind, JSON: b})
		return nil
	}

	if len(env) > 0 {
		sec := &corev1.Secret{
			TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
			ObjectMeta: meta(in, SecretName(in.AppName)),
			Type:       corev1.SecretTypeOpaque,
			StringData: env,
		}
		if err := add("Secret", sec); err != nil {
			return Rendered{}, err
		}
	}

	container := corev1.Container{
		Name:  "app",
		Image: in.Image,
		Ports: []corev1.ContainerPort{{ContainerPort: in.Port}},
	}
	if len(env) > 0 {
		container.EnvFrom = []corev1.EnvFromSource{{
			SecretRef: &corev1.SecretEnvSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: SecretName(in.AppName)},
			},
		}}
	}
	replicas := in.Replicas
	dep := &appsv1.Deployment{
		TypeMeta:   metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"},
		ObjectMeta: meta(in, in.AppName),
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: selector(in.AppName)},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels(in.AppName)},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{container}},
			},
		},
	}
	if err := add("Deployment", dep); err != nil {
		return Rendered{}, err
	}

	svc := &corev1.Service{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Service"},
		ObjectMeta: meta(in, in.AppName),
		Spec: corev1.ServiceSpec{
			Selector: selector(in.AppName),
			Ports: []corev1.ServicePort{{
				Port:       80,
				TargetPort: intstr.FromInt32(in.Port),
			}},
		},
	}
	if err := add("Service", svc); err != nil {
		return Rendered{}, err
	}

	pathType := netv1.PathTypePrefix
	ing := &netv1.Ingress{
		TypeMeta:   metav1.TypeMeta{APIVersion: "networking.k8s.io/v1", Kind: "Ingress"},
		ObjectMeta: meta(in, in.AppName),
		Spec: netv1.IngressSpec{
			Rules: []netv1.IngressRule{{
				Host: in.Host,
				IngressRuleValue: netv1.IngressRuleValue{
					HTTP: &netv1.HTTPIngressRuleValue{
						Paths: []netv1.HTTPIngressPath{{
							Path:     "/",
							PathType: &pathType,
							Backend: netv1.IngressBackend{
								Service: &netv1.IngressServiceBackend{
									Name: in.AppName,
									Port: netv1.ServiceBackendPort{Number: 80},
								},
							},
						}},
					},
				},
			}},
		},
	}
	if err := add("Ingress", ing); err != nil {
		return Rendered{}, err
	}

	return Rendered{Objects: objs}, nil
}

// YAML renders the object set as ----separated multi-doc YAML (for --raw).
func YAML(r Rendered) ([]byte, error) {
	var out []byte
	for i, o := range r.Objects {
		y, err := yaml.JSONToYAML(o.JSON)
		if err != nil {
			return nil, err
		}
		if i > 0 {
			out = append(out, []byte("---\n")...)
		}
		out = append(out, y...)
	}
	return out, nil
}
