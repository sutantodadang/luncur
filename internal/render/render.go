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
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	netv1 "k8s.io/api/networking/v1"
	policyv1 "k8s.io/api/policy/v1"
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
	// Suspended maps to CronJob.Spec.Suspend: true stops the schedule from
	// firing new runs without losing history/config. Cron kind only.
	Suspended bool
	// CPUMilli and MemoryMB are the container's requests==limits (Guaranteed
	// QoS); 0 means unset — no resources block is rendered for that resource.
	CPUMilli int64
	MemoryMB int64
	// HealthPath is the HTTP path probed for readiness/liveness; "" means
	// unset — no probes are rendered. Ignored for non-web kinds.
	HealthPath string
	// Internal marks a web app as cluster-only: the Service is still
	// rendered (ClusterIP), but the Ingress is omitted entirely — no public
	// URL, no domains. Ignored for non-web kinds (worker/cron already emit
	// no Service to make internal).
	Internal bool
	// GPU is the number of nvidia.com/gpu devices (requests==limits); 0
	// renders nothing. Any GPU>0 pod also gets runtimeClassName "nvidia"
	// and a nodeSelector pinning it to nodes labeled luncur.dev/gpu=true.
	GPU int64
	// RunName names the batch/v1 Job rendered for one triggered run of a
	// kind=job app ("<app>-run-<n>"). Empty (the deploy path) renders the
	// job app's Secret and PVCs only — the workload exists per run, not
	// per deploy. Ignored for other kinds.
	RunName string
	// Nodes is how many pods a kind=job run spans. 0 or 1 renders the
	// classic single-pod Job. >1 renders an Indexed Job (completions ==
	// parallelism == Nodes) plus a headless Service for pod-to-pod DNS, and
	// injects the LUNCUR_* rendezvous env contract. Ignored without RunName.
	Nodes int32
	// Framework optionally layers a framework's native rendezvous env vars
	// on top of the LUNCUR_* contract: "torchrun" (PET_*) or "torch"
	// (MASTER_ADDR/RANK/WORLD_SIZE). "" = contract only.
	Framework string
	// RunEnv are per-run plain env vars set directly on the job container
	// (not via the app Secret) — sweep params, trial ids. Job runs only.
	RunEnv map[string]string
	// ModelSource locates a kind=model app's weights: hf:<org>/<name>[/<file>]
	// or s3:<key-in-project-bucket>. Required for model apps.
	ModelSource string
	// Runtime selects the model serving runtime: auto (default), llamacpp,
	// vllm, or custom (user-supplied image; luncur wires the model volume).
	Runtime string
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
	// AutoMin, AutoMax, AutoCPU: AutoMin > 0 enables an autoscaling/v2 HPA
	// for web/worker Deployments; the Deployment then omits spec.replicas so
	// the HPA owns scale under server-side apply.
	AutoMin, AutoMax, AutoCPU int32
	// DeployStamp, when non-empty, is stamped as a pod-template annotation
	// (luncur.dev/deploy) carrying the current deployment id. A deploy or
	// redeploy mints a new id, changing the pod template and forcing a rolling
	// restart; plain re-syncs (env set/unset, scale, override edits) reuse the
	// same id and leave running pods alone. This is deliberate: env is injected
	// via a Secret EnvFrom (K8s does not hot-reload it), so a config change is
	// staged and only goes live on the next explicit redeploy. Empty leaves the
	// annotation off (deterministic default).
	DeployStamp string
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

func boolPtr(b bool) *bool { return &b }

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
	case "Job":
		return batchv1.Job{}, nil
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
	case "Job":
		v = &batchv1.Job{}
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return nil, err
	}
	return json.Marshal(v)
}

func SecretName(app string) string { return app + "-env" }

// GPU scheduling constants, shared with the gpu package (device plugin
// manifests) and kube (GPU node reporting).
const (
	GPUResource       = "nvidia.com/gpu"
	GPURuntimeClass   = "nvidia"
	GPUNodeLabelKey   = "luncur.dev/gpu"
	GPUNodeLabelValue = "true"
)

// applyGPU pins a GPU pod to GPU-labeled nodes and selects the nvidia
// runtime; a no-op for gpu <= 0.
func applyGPU(spec *corev1.PodSpec, gpu int64) {
	if gpu <= 0 {
		return
	}
	rc := GPURuntimeClass
	spec.RuntimeClassName = &rc
	if spec.NodeSelector == nil {
		spec.NodeSelector = map[string]string{}
	}
	spec.NodeSelector[GPUNodeLabelKey] = GPUNodeLabelValue
}

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

// buildHPA renders the autoscaling/v2 HorizontalPodAutoscaler for a web or
// worker app with AutoMin > 0, targeting the app's Deployment on CPU
// utilization.
func buildHPA(in Input) *autoscalingv2.HorizontalPodAutoscaler {
	min := in.AutoMin
	cpu := in.AutoCPU
	return &autoscalingv2.HorizontalPodAutoscaler{
		TypeMeta:   metav1.TypeMeta{APIVersion: "autoscaling/v2", Kind: "HorizontalPodAutoscaler"},
		ObjectMeta: meta(in, in.AppName),
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
				APIVersion: "apps/v1", Kind: "Deployment", Name: in.AppName,
			},
			MinReplicas: &min,
			MaxReplicas: in.AutoMax,
			Metrics: []autoscalingv2.MetricSpec{{
				Type: autoscalingv2.ResourceMetricSourceType,
				Resource: &autoscalingv2.ResourceMetricSource{
					Name: corev1.ResourceCPU,
					Target: autoscalingv2.MetricTarget{
						Type:               autoscalingv2.UtilizationMetricType,
						AverageUtilization: &cpu,
					},
				},
			}},
		},
	}
}

// buildPDB caps voluntary disruption for multi-replica web/worker apps so a
// node drain always leaves at least one pod. Rendered only when the app's
// floor (autoscale min when autoscaling, replicas otherwise) is >= 2 — a
// maxUnavailable:1 PDB on a single-replica app would block drains forever.
func buildPDB(in Input) *policyv1.PodDisruptionBudget {
	maxUnavailable := intstr.FromInt32(1)
	return &policyv1.PodDisruptionBudget{
		TypeMeta:   metav1.TypeMeta{APIVersion: "policy/v1", Kind: "PodDisruptionBudget"},
		ObjectMeta: meta(in, in.AppName),
		Spec: policyv1.PodDisruptionBudgetSpec{
			MaxUnavailable: &maxUnavailable,
			Selector:       &metav1.LabelSelector{MatchLabels: selector(in.AppName)},
		},
	}
}

// pdbFloor is the replica floor buildPDB's threshold is judged against: the
// autoscale minimum when autoscaling is on, the static replica count
// otherwise.
func pdbFloor(in Input) int32 {
	if in.AutoMin > 0 {
		return in.AutoMin
	}
	return in.Replicas
}

// Render builds the manifest set for one app. env is plaintext (the caller
// unseals); empty env omits the Secret entirely. Kind selects the object
// set: web (default, "") gets Deployment+Service+Ingress; an internal web
// app gets Deployment+Service but no Ingress (cluster-only, no public URL);
// worker gets a Deployment only (no ports); cron gets a batch/v1 CronJob.
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
	if kind == "model" && (in.Host == "" || in.ModelSource == "") {
		return Rendered{}, fmt.Errorf("render: Host and ModelSource are required for model apps")
	}
	if kind != "web" && kind != "worker" && kind != "cron" && kind != "job" && kind != "model" {
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
	if in.CPUMilli > 0 || in.MemoryMB > 0 || in.GPU > 0 {
		res := corev1.ResourceList{}
		if in.CPUMilli > 0 {
			res[corev1.ResourceCPU] = *resource.NewMilliQuantity(in.CPUMilli, resource.DecimalSI)
		}
		if in.MemoryMB > 0 {
			res[corev1.ResourceMemory] = *resource.NewQuantity(in.MemoryMB*1024*1024, resource.BinarySI)
		}
		if in.GPU > 0 {
			res[corev1.ResourceName(GPUResource)] = *resource.NewQuantity(in.GPU, resource.DecimalSI)
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
		s := probe()
		// 60s boot budget (2s x 30); liveness only starts after startup succeeds.
		s.PeriodSeconds, s.FailureThreshold = 2, 30
		container.StartupProbe = s
	}
	// Model apps rewire the container for their serving runtime; svcPort is
	// what the Service targets (the runtime's port for models, in.Port
	// otherwise).
	svcPort := in.Port
	var modelInits []corev1.Container
	var modelVols []corev1.Volume
	if kind == "model" {
		inits, vols, port, err := applyModel(in, &container, len(env) > 0)
		if err != nil {
			return Rendered{}, err
		}
		modelInits, modelVols, svcPort = inits, vols, port
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

	podVolumes = append(podVolumes, modelVols...)

	replicas := in.Replicas
	dep := &appsv1.Deployment{
		TypeMeta:   metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"},
		ObjectMeta: meta(in, in.AppName),
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: selector(in.AppName)},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels(in.AppName)},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{container}, Volumes: podVolumes},
			},
		},
	}
	// A new deployment id forces a rolling restart on deploy/redeploy while
	// leaving plain re-syncs alone (see Input.DeployStamp).
	if in.DeployStamp != "" {
		dep.Spec.Template.Annotations = map[string]string{"luncur.dev/deploy": in.DeployStamp}
	}
	// Autoscale on for web/worker: omit spec.replicas entirely so
	// server-side apply releases the field to the HPA controller.
	if !(in.AutoMin > 0 && (kind == "web" || kind == "worker")) {
		dep.Spec.Replicas = &replicas
	}
	// Explicit (== the K8s default) so the contract is visible in ejected YAML.
	grace := int64(30)
	dep.Spec.Template.Spec.TerminationGracePeriodSeconds = &grace
	dep.Spec.Template.Spec.InitContainers = modelInits
	applyGPU(&dep.Spec.Template.Spec, in.GPU)
	hasPVC := kind != "cron" && len(in.Volumes) > 0
	if hasPVC || (kind == "model" && in.GPU > 0) {
		// RWO node-local volumes can't be attached by the old and new pod
		// at once, and a GPU model's replacement pod can't schedule while
		// the old pod holds the node's GPU — Recreate instead of rolling.
		dep.Spec.Strategy = appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType}
	} else {
		// Zero-downtime rolling update: the replacement pod must be Ready
		// (readiness probe, when set) before the old one is terminated.
		zero := intstr.FromInt32(0)
		one := intstr.FromInt32(1)
		dep.Spec.Strategy = appsv1.DeploymentStrategy{
			Type: appsv1.RollingUpdateDeploymentStrategyType,
			RollingUpdate: &appsv1.RollingUpdateDeployment{
				MaxUnavailable: &zero,
				MaxSurge:       &one,
			},
		}
	}

	switch kind {
	case "web", "model":
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
					TargetPort: intstr.FromInt32(svcPort),
				}},
			},
		}
		if err := add("Service", svc); err != nil {
			return Rendered{}, err
		}

		// Internal web apps stop here: ClusterIP Service only, no Ingress —
		// no public URL, unreachable outside the cluster.
		if !in.Internal {
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
		}

		if kind == "web" && in.AutoMin > 0 {
			if err := add("HorizontalPodAutoscaler", buildHPA(in)); err != nil {
				return Rendered{}, err
			}
		}

		if kind == "web" && pdbFloor(in) >= 2 {
			if err := add("PodDisruptionBudget", buildPDB(in)); err != nil {
				return Rendered{}, err
			}
		}

	case "worker":
		if err := add("Deployment", dep); err != nil {
			return Rendered{}, err
		}

		if in.AutoMin > 0 {
			if err := add("HorizontalPodAutoscaler", buildHPA(in)); err != nil {
				return Rendered{}, err
			}
		}

		if pdbFloor(in) >= 2 {
			if err := add("PodDisruptionBudget", buildPDB(in)); err != nil {
				return Rendered{}, err
			}
		}

	case "job":
		// Deploy path (no RunName): the workload is rendered per run, so a
		// deploy only (re)applies the Secret and PVCs added above.
		if in.RunName != "" {
			jobContainer := container
			jobContainer.Env = append(jobContainer.Env, runEnvVars(in.RunEnv)...)
			multiNode := in.Nodes > 1
			if multiNode {
				tenv, err := trainEnv(in.RunName, in.Namespace, in.Nodes, in.Framework)
				if err != nil {
					return Rendered{}, err
				}
				jobContainer.Env = append(jobContainer.Env, tenv...)
			} else if in.Framework != "" {
				// Validate even when single-node so a bad value fails loudly.
				if _, err := trainEnv(in.RunName, in.Namespace, 2, in.Framework); err != nil {
					return Rendered{}, err
				}
			}

			job := &batchv1.Job{
				TypeMeta:   metav1.TypeMeta{APIVersion: "batch/v1", Kind: "Job"},
				ObjectMeta: meta(in, in.RunName),
				Spec: batchv1.JobSpec{
					// One attempt, no retries: a training/batch run that
					// failed must surface as failed, not silently rerun.
					BackoffLimit:            int32Ptr(0),
					TTLSecondsAfterFinished: int32Ptr(86400),
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{Labels: labels(in.AppName)},
						Spec: corev1.PodSpec{
							RestartPolicy: corev1.RestartPolicyNever,
							Containers:    []corev1.Container{jobContainer},
							Volumes:       podVolumes,
						},
					},
				},
			}
			if multiNode {
				mode := batchv1.IndexedCompletion
				job.Spec.CompletionMode = &mode
				job.Spec.Completions = int32Ptr(in.Nodes)
				job.Spec.Parallelism = int32Ptr(in.Nodes)
				job.Spec.Template.Spec.Subdomain = in.RunName
			}
			applyGPU(&job.Spec.Template.Spec, in.GPU)
			if err := add("Job", job); err != nil {
				return Rendered{}, err
			}
			if multiNode {
				svc := &corev1.Service{
					TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Service"},
					ObjectMeta: meta(in, in.RunName),
					Spec: corev1.ServiceSpec{
						ClusterIP:                corev1.ClusterIPNone,
						PublishNotReadyAddresses: true,
						Selector:                 map[string]string{"batch.kubernetes.io/job-name": in.RunName},
						Ports:                    []corev1.ServicePort{{Port: 29500, TargetPort: intstr.FromInt32(29500)}},
					},
				}
				if err := add("Service", svc); err != nil {
					return Rendered{}, err
				}
			}
		}

	case "cron":
		cj := &batchv1.CronJob{
			TypeMeta:   metav1.TypeMeta{APIVersion: "batch/v1", Kind: "CronJob"},
			ObjectMeta: meta(in, in.AppName),
			Spec: batchv1.CronJobSpec{
				Schedule:                   in.Schedule,
				Suspend:                    boolPtr(in.Suspended),
				ConcurrencyPolicy:          batchv1.ForbidConcurrent,
				SuccessfulJobsHistoryLimit: int32Ptr(3),
				FailedJobsHistoryLimit:     int32Ptr(3),
				JobTemplate: batchv1.JobTemplateSpec{
					// Labeled so the Jobs it spawns (and manual "run now"
					// Jobs built from this same JobTemplate) carry the app
					// label — CronRuns finds them via
					// app.kubernetes.io/name=<app>.
					ObjectMeta: metav1.ObjectMeta{Labels: labels(in.AppName)},
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
		applyGPU(&cj.Spec.JobTemplate.Spec.Template.Spec, in.GPU)
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
