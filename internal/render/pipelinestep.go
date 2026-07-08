package render

import (
	"encoding/json"
	"fmt"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// PipelineStepJobName is the deterministic Job name for one pipeline step
// attempt: "pl-<runID>-<step>-a<attempt>". runID is a 12-char store.NewID,
// step is capped at 20 chars (pipeline.Compile enforces this) — the full
// name (3 + 12 + 1 + 20 + 2 + attempt digits) stays well under the
// Kubernetes 63-char object name limit.
func PipelineStepJobName(runID, stepName string, attempt int) string {
	return fmt.Sprintf("pl-%s-%s-a%d", runID, stepName, attempt)
}

// pipelineStepLabels tags a step's Job (and its pods) for cleanup queries:
// managed-by identifies luncur-owned objects the same way every other
// rendered object does, and pipeline-run scopes to one run so the engine
// (and `luncur down`) can list/delete every Job a run created.
func pipelineStepLabels(runID string) map[string]string {
	return map[string]string{
		"app.kubernetes.io/managed-by": "luncur",
		"luncur.dev/pipeline-run":      runID,
	}
}

// PipelineStepJob renders the plain K8s Job for one inline image step
// attempt. Pod hardening mirrors Render's job-kind branch: RestartPolicyNever,
// backoffLimit 0 (one attempt — retries relaunch a brand new Job named with
// the bumped attempt, they don't rely on in-Job backoff), a 24h TTL, and
// GPU>0 wiring identical to the job kind (nvidia.com/gpu requests==limits,
// runtimeClassName, node selector) via the shared applyGPU helper. env is
// rendered directly on the container (sorted via runEnvVars) — pipeline
// step env is plaintext by design (see pipeline.Compile's doc comment), no
// Secret indirection. An empty command lets the image's own entrypoint run.
func PipelineStepJob(namespace, runID, stepName string, attempt int, image string, command []string, env map[string]string, gpu int) []Object {
	labels := pipelineStepLabels(runID)

	container := corev1.Container{
		Name:  "app",
		Image: image,
		Env:   runEnvVars(env),
	}
	if len(command) > 0 {
		container.Command = command
	}
	if gpu > 0 {
		res := corev1.ResourceList{
			corev1.ResourceName(GPUResource): *resource.NewQuantity(int64(gpu), resource.DecimalSI),
		}
		container.Resources = corev1.ResourceRequirements{Requests: res, Limits: res}
	}

	job := &batchv1.Job{
		TypeMeta: metav1.TypeMeta{APIVersion: "batch/v1", Kind: "Job"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      PipelineStepJobName(runID, stepName, attempt),
			Namespace: namespace,
			Labels:    labels,
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            int32Ptr(0),
			TTLSecondsAfterFinished: int32Ptr(86400),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers:    []corev1.Container{container},
				},
			},
		},
	}
	applyGPU(&job.Spec.Template.Spec, int64(gpu))

	b, err := json.Marshal(job)
	if err != nil {
		// Job is a plain typed struct built entirely from this function's
		// own fields — marshal cannot fail in practice.
		panic(err)
	}
	return []Object{{Kind: "Job", JSON: b}}
}
