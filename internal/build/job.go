package build

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/sutantodadang/luncur/internal/render"
)

// DefaultBuilderImage is the builder image ref used when --builder-image is
// left unset. Published by the release pipeline (.github/workflows/release.yml)
// to ghcr.io alongside the server image — luncur/builder:latest pointed at a
// Docker Hub org nobody publishes to and every build failed on pull.
const DefaultBuilderImage = "ghcr.io/sutantodadang/luncur-builder:latest"

type BuildParams struct {
	Namespace    string
	Name         string
	BuilderImage string
	DataPVC      string
	ImageRef     string
	RegistryHost string
	SourceType   string
	GitURL       string
	GitBranch    string
	DeployID     int64
	CacheRef     string
	BuildPath    string
	// BuildEnv is the app's decrypted env vars, forwarded to the builder as
	// LUNCUR_BUILDARG_<KEY> so the entrypoint can pass them through to the
	// image build (Docker build-args / nixpacks --env) — the only way a
	// build-time-only value (e.g. Vite's ARG VITE_API_URL) sees a real
	// value instead of the Dockerfile's baked-in default. Runtime env still
	// flows separately via the app's Secret; this is additive, not a
	// replacement.
	BuildEnv map[string]string
}

func ptr[T any](v T) *T { return &v }

func ImageRef(registryHost, project, app string, deployID int64) string {
	return fmt.Sprintf("%s/%s-%s:%d", registryHost, project, app, deployID)
}

// CacheRef returns the per-app BuildKit cache image ref, stored under a
// nested luncur-cache/ repo so it never collides with the app's own image
// repo (project-app never contains a slash).
func CacheRef(registryHost, project, app string) string {
	return fmt.Sprintf("%s/luncur-cache/%s-%s:buildcache", registryHost, project, app)
}

func RenderBuildJob(p BuildParams) (render.Object, error) {
	backoffLimit := int32(0)
	restartPolicy := corev1.RestartPolicyNever

	env := []corev1.EnvVar{
		{Name: "LUNCUR_DEPLOY_ID", Value: strconv.FormatInt(p.DeployID, 10)},
		{Name: "LUNCUR_IMAGE_REF", Value: p.ImageRef},
		{Name: "LUNCUR_REGISTRY_HOST", Value: p.RegistryHost},
		{Name: "LUNCUR_SOURCE_TYPE", Value: p.SourceType},
		{Name: "LUNCUR_GIT_URL", Value: p.GitURL},
		{Name: "LUNCUR_GIT_BRANCH", Value: p.GitBranch},
	}
	if p.CacheRef != "" {
		env = append(env, corev1.EnvVar{Name: "LUNCUR_CACHE_REF", Value: p.CacheRef})
	}
	if p.BuildPath != "" {
		env = append(env, corev1.EnvVar{Name: "LUNCUR_BUILD_PATH", Value: p.BuildPath})
	}
	if len(p.BuildEnv) > 0 {
		keys := make([]string, 0, len(p.BuildEnv))
		for k := range p.BuildEnv {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			env = append(env, corev1.EnvVar{Name: "LUNCUR_BUILDARG_" + k, Value: p.BuildEnv[k]})
		}
	}

	container := corev1.Container{
		Name:  "builder",
		Image: p.BuilderImage,
		// Rootless BuildKit's rootlesskit must create user+mount namespaces
		// and change mount propagation inside them. The runtime's default
		// seccomp and AppArmor profiles block those syscalls (observed on
		// Ubuntu hosts as "failed to share mount point: /: permission
		// denied"), so both are unconfined — the buildkit-documented
		// requirement for running rootless in Kubernetes. This is why the
		// system namespace enforces PodSecurity "privileged": the
		// unconfined profiles violate "baseline". Builds still run as the
		// unprivileged uid 1000 user.
		SecurityContext: &corev1.SecurityContext{
			RunAsUser:  ptr(int64(1000)),
			RunAsGroup: ptr(int64(1000)),
			SeccompProfile: &corev1.SeccompProfile{
				Type: corev1.SeccompProfileTypeUnconfined,
			},
		},
		Env: env,
		VolumeMounts: []corev1.VolumeMount{
			// SubPath "data": the server mounts this PVC at /var/lib/luncur
			// with --data-dir /var/lib/luncur/data (the layout `luncur up`
			// provisions — db and key live at the PVC root, OUTSIDE what the
			// builder can see). Without the subPath the builder's /data is
			// the PVC root, so its /data/logs/<id>.log lands in a directory
			// the server never tails and the UI log pane stays blind to all
			// builder output.
			{Name: "data", MountPath: "/data", SubPath: "data"},
		},
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("100m"),
				corev1.ResourceMemory: resource.MustParse("256Mi"),
			},
			Limits: corev1.ResourceList{
				corev1.ResourceMemory: resource.MustParse("2Gi"),
			},
		},
	}

	job := &batchv1.Job{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "batch/v1",
			Kind:       "Job",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      p.Name,
			Namespace: p.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "luncur",
			},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            &backoffLimit,
			TTLSecondsAfterFinished: ptr(int32(3600)),
			ActiveDeadlineSeconds:   ptr(int64(900)),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					// AppArmor must be unconfined for rootlesskit's mount
					// operations (see the container SecurityContext comment).
					// The annotation form works on every k8s version luncur
					// targets, including ones without the appArmorProfile
					// field.
					Annotations: map[string]string{
						"container.apparmor.security.beta.kubernetes.io/builder": "unconfined",
					},
				},
				Spec: corev1.PodSpec{
					RestartPolicy: restartPolicy,
					Containers:    []corev1.Container{container},
					Volumes: []corev1.Volume{
						{
							Name: "data",
							VolumeSource: corev1.VolumeSource{
								PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
									ClaimName: p.DataPVC,
								},
							},
						},
					},
				},
			},
		},
	}

	b, err := json.Marshal(job)
	if err != nil {
		return render.Object{}, err
	}

	return render.Object{Kind: "Job", JSON: b}, nil
}
