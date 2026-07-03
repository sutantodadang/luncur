package build

import (
	"encoding/json"
	"fmt"
	"strconv"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/sutantodadang/luncur/internal/render"
)

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
}

func ptr[T any](v T) *T { return &v }

func ImageRef(registryHost, project, app string, deployID int64) string {
	return fmt.Sprintf("%s/%s-%s:%d", registryHost, project, app, deployID)
}

func RenderBuildJob(p BuildParams) (render.Object, error) {
	backoffLimit := int32(0)
	restartPolicy := corev1.RestartPolicyNever

	container := corev1.Container{
		Name:  "builder",
		Image: p.BuilderImage,
		Env: []corev1.EnvVar{
			{Name: "LUNCUR_DEPLOY_ID", Value: strconv.FormatInt(p.DeployID, 10)},
			{Name: "LUNCUR_IMAGE_REF", Value: p.ImageRef},
			{Name: "LUNCUR_REGISTRY_HOST", Value: p.RegistryHost},
			{Name: "LUNCUR_SOURCE_TYPE", Value: p.SourceType},
			{Name: "LUNCUR_GIT_URL", Value: p.GitURL},
			{Name: "LUNCUR_GIT_BRANCH", Value: p.GitBranch},
		},
		VolumeMounts: []corev1.VolumeMount{
			{Name: "data", MountPath: "/data"},
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
