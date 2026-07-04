// Package render turns luncur's app model into Kubernetes manifests.
// Objects are rendered from the model, then per-kind user overrides
// (strategic merge patches) are applied — so user customizations survive
// every redeploy. Pure package: no cluster access.
package render

import (
	"bytes"
	"encoding/json"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	netv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/strategicpatch"
	"sigs.k8s.io/yaml"
)

type Input struct {
	AppName   string
	Namespace string
	Image     string
	Host      string
	Port      int32
	Replicas  int32
	// Kind is one of web|worker|cron; "" means web (back-compat default).
	Kind string
	// Schedule is a 5-field cron expression; cron kind only.
	Schedule string
	// CPUMilli and MemoryMB are the container's requests==limits (Guaranteed
	// QoS); 0 means unset — no resources block is rendered for that resource.
	CPUMilli int64
	MemoryMB int64
	// HealthPath is the HTTP path probed for readiness/liveness; "" means
	// unset — no probes are rendered. Ignored for non-web kinds.
	HealthPath string
	// Overrides maps Kind -> strategic-merge-patch JSON. Applied by Task 6.
	Overrides map[string]string
	// ExtraHosts adds Ingress rules (same backend) for custom domains.
	ExtraHosts []string
	// IngressAnnotations lands on the Ingress metadata (cert providers).
	IngressAnnotations map[string]string
	// TLS is set as spec.tls verbatim (secret refs per issued domain).
	TLS []netv1.IngressTLS
	// Volumes are the app's persistent volumes (web/worker only; cron
	// ignores this field). Each renders an RWO PersistentVolumeClaim plus a
	// pod volume + container mount; any non-empty Volumes forces the
	// Deployment's strategy to Recreate (RWO/node-local storage can't be
	// rolling-updated across nodes).
	Volumes []Volume
}

// Volume is a single per-app persistent volume: a name (becomes the PVC's
// name suffix and the pod volume name), the container mount path, and the
// requested size in GB.
type Volume struct {
	Name   string
	Path   string
	SizeGB int
}

// VolumeClaimName is the PersistentVolumeClaim name luncur renders for one
// of an app's volumes: deterministic so the server's purge path (which never
// has a rendered manifest at hand) can compute it independently.
func VolumeClaimName(appName, volumeName string) string {
	return appName + "-" + volumeName
}

func int32Ptr(n int32) *int32 { return &n }

type Object struct {
	Kind string
	JSON []byte
}

type Rendered struct {
	Objects []Object
}

// dataStructFor returns the typed zero value strategicpatch needs to
// understand list-merge keys (e.g. containers merged by name).
func dataStructFor(kind string) (any, error) {
	switch kind {
	case "Deployment":
		return appsv1.Deployment{}, nil
	case "Service":
		return corev1.Service{}, nil
	case "Ingress":
		return netv1.Ingress{}, nil
	case "CronJob":
		return batchv1.CronJob{}, nil
	default:
		return nil, fmt.Errorf("kind %q cannot be overridden", kind)
	}
}

func applyOverride(kind string, base []byte, patch string) ([]byte, error) {
	ds, err := dataStructFor(kind)
	if err != nil {
		return nil, err
	}
	merged, err := strategicpatch.StrategicMergePatch(base, []byte(patch), ds)
	if err != nil {
		return nil, fmt.Errorf("apply %s override: %w", kind, err)
	}
	// Round-trip through the typed struct so type mismatches fail loudly
	// at render time instead of at cluster apply time.
	typed, err := roundTrip(kind, merged)
	if err != nil {
		return nil, fmt.Errorf("%s override produces invalid object: %w", kind, err)
	}
	return typed, nil
}

func roundTrip(kind string, raw []byte) ([]byte, error) {
	var v any
	switch kind {
	case "Deployment":
		v = &appsv1.Deployment{}
	case "Service":
		v = &corev1.Service{}
	case "Ingress":
		v = &netv1.Ingress{}
	case "CronJob":
		v = &batchv1.CronJob{}
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return nil, err
	}
	return json.Marshal(v)
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
// unseals); empty env omits the Secret entirely. Kind selects the object
// set: web (default, "") gets Deployment+Service+Ingress; worker gets a
// Deployment only (no ports); cron gets a batch/v1 CronJob.
func Render(in Input, env map[string]string) (Rendered, error) {
	kind := in.Kind
	if kind == "" {
		kind = "web"
	}
	if in.AppName == "" || in.Namespace == "" || in.Image == "" {
		return Rendered{}, fmt.Errorf("render: AppName, Namespace, and Image are required")
	}
	if kind == "web" && (in.Host == "" || in.Port < 1) {
		return Rendered{}, fmt.Errorf("render: Host and Port are required for web apps")
	}
	if kind != "web" && kind != "worker" && kind != "cron" {
		return Rendered{}, fmt.Errorf("render: unknown kind %q", kind)
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
	}
	if kind == "web" {
		container.Ports = []corev1.ContainerPort{{ContainerPort: in.Port}}
	}
	if len(env) > 0 {
		container.EnvFrom = []corev1.EnvFromSource{{
			SecretRef: &corev1.SecretEnvSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: SecretName(in.AppName)},
			},
		}}
	}
	if in.CPUMilli > 0 || in.MemoryMB > 0 {
		res := corev1.ResourceList{}
		if in.CPUMilli > 0 {
			res[corev1.ResourceCPU] = *resource.NewMilliQuantity(in.CPUMilli, resource.DecimalSI)
		}
		if in.MemoryMB > 0 {
			res[corev1.ResourceMemory] = *resource.NewQuantity(in.MemoryMB*1024*1024, resource.BinarySI)
		}
		container.Resources = corev1.ResourceRequirements{Requests: res, Limits: res}
	}
	if in.HealthPath != "" && kind == "web" {
		probe := func() *corev1.Probe {
			return &corev1.Probe{ProbeHandler: corev1.ProbeHandler{
				HTTPGet: &corev1.HTTPGetAction{Path: in.HealthPath, Port: intstr.FromInt32(in.Port)},
			}}
		}
		r := probe()
		r.PeriodSeconds, r.FailureThreshold = 5, 3
		l := probe()
		l.InitialDelaySeconds, l.PeriodSeconds, l.FailureThreshold = 15, 20, 3
		container.ReadinessProbe, container.LivenessProbe = r, l
	}
	// Volumes: one RWO PVC per volume, mounted into the app container. Cron
	// jobs ignore them (no PVCs emitted, no mounts).
	var podVolumes []corev1.Volume
	if kind != "cron" {
		for _, v := range in.Volumes {
			claim := VolumeClaimName(in.AppName, v.Name)
			pvc := &corev1.PersistentVolumeClaim{
				TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "PersistentVolumeClaim"},
				ObjectMeta: meta(in, claim),
				Spec: corev1.PersistentVolumeClaimSpec{
					AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
					Resources: corev1.VolumeResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceStorage: *resource.NewQuantity(int64(v.SizeGB)*1024*1024*1024, resource.BinarySI),
						},
					},
				},
			}
			if err := add("PersistentVolumeClaim", pvc); err != nil {
				return Rendered{}, err
			}
			podVolumes = append(podVolumes, corev1.Volume{
				Name: v.Name,
				VolumeSource: corev1.VolumeSource{
					PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: claim},
				},
			})
			container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{
				Name: v.Name, MountPath: v.Path,
			})
		}
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
				Spec:       corev1.PodSpec{Containers: []corev1.Container{container}, Volumes: podVolumes},
			},
		},
	}
	if len(podVolumes) > 0 {
		// RWO node-local volumes can't be attached by the old and new pod at
		// once, so a rolling update would deadlock — Recreate instead.
		dep.Spec.Strategy = appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType}
	}

	switch kind {
	case "web":
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
		rule := func(host string) netv1.IngressRule {
			return netv1.IngressRule{
				Host: host,
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
			}
		}
		rules := []netv1.IngressRule{rule(in.Host)}
		for _, h := range in.ExtraHosts {
			rules = append(rules, rule(h))
		}
		ingMeta := meta(in, in.AppName)
		if len(in.IngressAnnotations) > 0 {
			ingMeta.Annotations = in.IngressAnnotations
		}
		ing := &netv1.Ingress{
			TypeMeta:   metav1.TypeMeta{APIVersion: "networking.k8s.io/v1", Kind: "Ingress"},
			ObjectMeta: ingMeta,
			Spec: netv1.IngressSpec{
				Rules: rules,
				TLS:   in.TLS,
			},
		}
		if err := add("Ingress", ing); err != nil {
			return Rendered{}, err
		}

	case "worker":
		if err := add("Deployment", dep); err != nil {
			return Rendered{}, err
		}

	case "cron":
		cj := &batchv1.CronJob{
			TypeMeta:   metav1.TypeMeta{APIVersion: "batch/v1", Kind: "CronJob"},
			ObjectMeta: meta(in, in.AppName),
			Spec: batchv1.CronJobSpec{
				Schedule:                   in.Schedule,
				ConcurrencyPolicy:          batchv1.ForbidConcurrent,
				SuccessfulJobsHistoryLimit: int32Ptr(3),
				FailedJobsHistoryLimit:     int32Ptr(3),
				JobTemplate: batchv1.JobTemplateSpec{
					Spec: batchv1.JobSpec{
						BackoffLimit: int32Ptr(2),
						Template: corev1.PodTemplateSpec{
							ObjectMeta: metav1.ObjectMeta{Labels: labels(in.AppName)},
							Spec: corev1.PodSpec{
								RestartPolicy: corev1.RestartPolicyOnFailure,
								Containers:    []corev1.Container{container},
							},
						},
					},
				},
			},
		}
		if err := add("CronJob", cj); err != nil {
			return Rendered{}, err
		}
	}

	for overrideKind := range in.Overrides {
		if _, err := dataStructFor(overrideKind); err != nil {
			return Rendered{}, err
		}
	}
	for i, o := range objs {
		patch, ok := in.Overrides[o.Kind]
		if !ok {
			continue
		}
		merged, err := applyOverride(o.Kind, o.JSON, patch)
		if err != nil {
			return Rendered{}, err
		}
		objs[i].JSON = merged
	}

	return Rendered{Objects: objs}, nil
}

// ExtractDoc splits a multi-document YAML stream on "---" boundaries and
// returns the document whose top-level `kind:` matches, erroring if none do.
func ExtractDoc(yamlMulti []byte, kind string) ([]byte, error) {
	docs := bytes.Split(yamlMulti, []byte("\n---"))
	for _, doc := range docs {
		doc = bytes.TrimSpace(doc)
		if len(doc) == 0 {
			continue
		}
		var meta struct {
			Kind string `json:"kind"`
		}
		if err := yaml.Unmarshal(doc, &meta); err != nil {
			continue
		}
		if meta.Kind == kind {
			return doc, nil
		}
	}
	return nil, fmt.Errorf("no document with kind %q found", kind)
}

// ComputeOverride diffs baseYAML against editedYAML and returns a strategic
// merge patch JSON string ("{}" if there is no difference). Pure function.
func ComputeOverride(kind string, baseYAML, editedYAML []byte) (string, error) {
	ds, err := dataStructFor(kind)
	if err != nil {
		return "", err
	}
	baseJSON, err := yaml.YAMLToJSON(baseYAML)
	if err != nil {
		return "", fmt.Errorf("base yaml: %w", err)
	}
	editedJSON, err := yaml.YAMLToJSON(editedYAML)
	if err != nil {
		return "", fmt.Errorf("edited yaml: %w", err)
	}
	patch, err := strategicpatch.CreateTwoWayMergePatch(baseJSON, editedJSON, ds)
	if err != nil {
		return "", fmt.Errorf("compute patch: %w", err)
	}
	return string(patch), nil
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
