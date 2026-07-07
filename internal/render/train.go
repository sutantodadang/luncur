package render

import (
	"fmt"
	"sort"
	"strconv"

	corev1 "k8s.io/api/core/v1"
)

// TrainFrameworks are the framework env presets a multi-node run may name.
// "" (no preset) is always valid and means the raw LUNCUR_* contract only.
var TrainFrameworks = []string{"torchrun", "torch"}

// completionIndexRef is the downward-API source for a pod's node rank: the
// Job controller stamps every Indexed-Job pod with its completion index.
func completionIndexRef() *corev1.EnvVarSource {
	return &corev1.EnvVarSource{
		FieldRef: &corev1.ObjectFieldSelector{
			FieldPath: "metadata.annotations['batch.kubernetes.io/job-completion-index']",
		},
	}
}

const trainMasterPort = "29500"

// trainEnv builds the rendezvous env for one multi-node run: the LUNCUR_*
// base contract plus the named framework preset's native vars. runName is
// both the Job name and the headless Service name; master addr is rank-0's
// stable pod DNS name (<job>-0.<svc>.<ns> — Indexed Jobs set each pod's
// hostname to <job>-<index> and subdomain wires them to the Service).
func trainEnv(runName, namespace string, nodes int32, framework string) ([]corev1.EnvVar, error) {
	master := fmt.Sprintf("%s-0.%s.%s", runName, runName, namespace)
	n := strconv.Itoa(int(nodes))
	env := []corev1.EnvVar{
		{Name: "LUNCUR_NODE_RANK", ValueFrom: completionIndexRef()},
		{Name: "LUNCUR_NUM_NODES", Value: n},
		{Name: "LUNCUR_MASTER_ADDR", Value: master},
		{Name: "LUNCUR_MASTER_PORT", Value: trainMasterPort},
	}
	switch framework {
	case "":
	case "torchrun":
		// torchrun reads PET_-prefixed env as flag equivalents; the image
		// entrypoint stays a plain `torchrun train.py`.
		env = append(env,
			corev1.EnvVar{Name: "PET_NNODES", Value: n},
			corev1.EnvVar{Name: "PET_NODE_RANK", ValueFrom: completionIndexRef()},
			corev1.EnvVar{Name: "PET_RDZV_BACKEND", Value: "c10d"},
			corev1.EnvVar{Name: "PET_RDZV_ENDPOINT", Value: master + ":" + trainMasterPort},
		)
	case "torch":
		// The torch env:// rendezvous contract — also what deepspeed and
		// accelerate consume. Node-level values: per-GPU process ranks are
		// the in-container launcher's job.
		env = append(env,
			corev1.EnvVar{Name: "MASTER_ADDR", Value: master},
			corev1.EnvVar{Name: "MASTER_PORT", Value: trainMasterPort},
			corev1.EnvVar{Name: "RANK", ValueFrom: completionIndexRef()},
			corev1.EnvVar{Name: "NODE_RANK", ValueFrom: completionIndexRef()},
			corev1.EnvVar{Name: "WORLD_SIZE", Value: n},
			corev1.EnvVar{Name: "NNODES", Value: n},
		)
	default:
		return nil, fmt.Errorf("render: unknown framework %q (valid: torchrun, torch)", framework)
	}
	return env, nil
}

// runEnvVars renders Input.RunEnv deterministically (sorted by key).
func runEnvVars(m map[string]string) []corev1.EnvVar {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]corev1.EnvVar, 0, len(keys))
	for _, k := range keys {
		out = append(out, corev1.EnvVar{Name: k, Value: m[k]})
	}
	return out
}
