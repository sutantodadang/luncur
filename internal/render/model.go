package render

import (
	"fmt"
	"path"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// Built-in model runtime images. Pinned where the upstream publishes stable
// tags; bumped deliberately, never floated.
const (
	// llama.cpp's release process retags :server continuously; there is no
	// long-lived pinned server tag, so this rides the official rolling tag.
	llamaCppImage = "ghcr.io/ggml-org/llama.cpp:server"
	vllmImage     = "vllm/vllm-openai:v0.8.5"
	curlImage     = "curlimages/curl:8.13.0"
	awsCLIImage   = "amazon/aws-cli:2.22.35"

	// ModelDir is where downloaded model files land inside the pod.
	ModelDir = "/models"

	llamaCppPort int32 = 8080
	vllmPort     int32 = 8000
	customPort   int32 = 8080
)

// ModelRuntimeInfo describes the resolved serving runtime for a model app.
type ModelRuntimeInfo struct {
	Name  string // llamacpp|vllm|custom
	Image string // "" for custom (the user's deployed image is used)
	Port  int32
}

// ParseModelSource splits a model source into its scheme and parts.
// "hf:org/name[/path/to/file]" -> ("hf", "org/name", "path/to/file").
// "s3:key/in/bucket"           -> ("s3", "", "key/in/bucket").
func ParseModelSource(source string) (scheme, repo, file string, err error) {
	switch {
	case strings.HasPrefix(source, "hf:"):
		rest := strings.TrimPrefix(source, "hf:")
		parts := strings.SplitN(rest, "/", 3)
		if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
			return "", "", "", fmt.Errorf("hf source must be hf:<org>/<name>[/<file>], got %q", source)
		}
		repo = parts[0] + "/" + parts[1]
		if len(parts) == 3 {
			file = parts[2]
		}
		return "hf", repo, file, nil
	case strings.HasPrefix(source, "s3:"):
		key := strings.TrimPrefix(source, "s3:")
		if key == "" {
			return "", "", "", fmt.Errorf("s3 source must be s3:<key>, got %q", source)
		}
		return "s3", "", key, nil
	default:
		return "", "", "", fmt.Errorf("model source must start with hf: or s3:, got %q", source)
	}
}

func isGGUF(source string) bool {
	return strings.HasSuffix(strings.ToLower(source), ".gguf")
}

// ResolveModelRuntime picks the serving runtime for a model app.
// auto (or ""): GGUF sources serve with llama.cpp (CPU or GPU); anything
// else needs vLLM, which needs a GPU.
func ResolveModelRuntime(source, runtime string, gpu int64) (ModelRuntimeInfo, error) {
	if _, _, _, err := ParseModelSource(source); err != nil {
		return ModelRuntimeInfo{}, err
	}
	switch runtime {
	case "", "auto":
		if isGGUF(source) {
			return ModelRuntimeInfo{Name: "llamacpp", Image: llamaCppImage, Port: llamaCppPort}, nil
		}
		if gpu > 0 {
			return ModelRuntimeInfo{Name: "vllm", Image: vllmImage, Port: vllmPort}, nil
		}
		return ModelRuntimeInfo{}, fmt.Errorf("cannot auto-select a runtime: non-GGUF model with no GPU — use a .gguf source (llama.cpp), or add --gpu for vLLM, or --runtime custom")
	case "llamacpp":
		if !isGGUF(source) {
			return ModelRuntimeInfo{}, fmt.Errorf("llama.cpp serves GGUF files; source %q is not a .gguf", source)
		}
		return ModelRuntimeInfo{Name: "llamacpp", Image: llamaCppImage, Port: llamaCppPort}, nil
	case "vllm":
		return ModelRuntimeInfo{Name: "vllm", Image: vllmImage, Port: vllmPort}, nil
	case "custom":
		return ModelRuntimeInfo{Name: "custom", Port: customPort}, nil
	default:
		return ModelRuntimeInfo{}, fmt.Errorf("invalid runtime %q (auto|llamacpp|vllm|custom)", runtime)
	}
}

// modelDownloadInit builds the init container that fetches the model into
// the shared /models volume, or nil when the runtime pulls it itself
// (vLLM + HF). hasEnv wires the app Secret in (s3 downloads need the
// injected LUNCUR_S3_* credentials).
func modelDownloadInit(in Input, scheme, repo, file, rtName string, hasEnv bool) *corev1.Container {
	var c *corev1.Container
	switch scheme {
	case "hf":
		if rtName == "vllm" || file == "" {
			// vLLM downloads from the Hub itself; whole-repo sources have
			// nothing single to fetch.
			return nil
		}
		base := path.Base(file)
		url := fmt.Sprintf("https://huggingface.co/%s/resolve/main/%s", repo, file)
		c = &corev1.Container{
			Name:  "model-download",
			Image: curlImage,
			Command: []string{"sh", "-c", fmt.Sprintf(
				"test -f %s/%s || curl -fL --retry 3 -o %s/%s %q",
				ModelDir, base, ModelDir, base, url)},
		}
	case "s3":
		base := path.Base(file)
		c = &corev1.Container{
			Name:  "model-download",
			Image: awsCLIImage,
			Command: []string{"sh", "-c", fmt.Sprintf(
				`test -f %s/%s || { export AWS_ACCESS_KEY_ID="$LUNCUR_S3_KEY" AWS_SECRET_ACCESS_KEY="$LUNCUR_S3_SECRET"; aws s3 cp "s3://$LUNCUR_S3_BUCKET/%s" %s/%s --endpoint-url "$LUNCUR_S3_ENDPOINT"; }`,
				ModelDir, base, file, ModelDir, base)},
		}
	}
	if c == nil {
		return nil
	}
	c.VolumeMounts = []corev1.VolumeMount{{Name: "model-cache", MountPath: ModelDir}}
	if hasEnv {
		c.EnvFrom = []corev1.EnvFromSource{{
			SecretRef: &corev1.SecretEnvSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: SecretName(in.AppName)},
			},
		}}
	}
	return c
}

// applyModel rewires the generic app container for a model runtime and
// returns the init containers, extra pod volumes, and serving port.
func applyModel(in Input, container *corev1.Container, hasEnv bool) (inits []corev1.Container, vols []corev1.Volume, port int32, err error) {
	rt, err := ResolveModelRuntime(in.ModelSource, in.Runtime, in.GPU)
	if err != nil {
		return nil, nil, 0, err
	}
	scheme, repo, file, err := ParseModelSource(in.ModelSource)
	if err != nil {
		return nil, nil, 0, err
	}

	port = rt.Port
	if rt.Image != "" {
		container.Image = rt.Image
	}
	container.Ports = []corev1.ContainerPort{{ContainerPort: port}}

	vols = append(vols, corev1.Volume{
		Name:         "model-cache",
		VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
	})
	container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{Name: "model-cache", MountPath: ModelDir})

	base := path.Base(file)
	modelPath := ModelDir + "/" + base

	switch rt.Name {
	case "llamacpp":
		args := []string{"-m", modelPath, "--host", "0.0.0.0", "--port", fmt.Sprint(port)}
		if in.GPU > 0 {
			args = append(args, "--n-gpu-layers", "999")
		}
		container.Args = args
	case "vllm":
		model := modelPath
		if scheme == "hf" {
			// vLLM pulls from the Hub itself; cache under the shared volume
			// so a container restart doesn't re-download.
			model = repo
			container.Env = append(container.Env, corev1.EnvVar{Name: "HF_HOME", Value: ModelDir + "/hf"})
		}
		container.Args = []string{"--model", model, "--host", "0.0.0.0", "--port", fmt.Sprint(port)}
	case "custom":
		// The user's image serves; luncur only wires the model volume and
		// tells it where the model landed.
		container.Env = append(container.Env,
			corev1.EnvVar{Name: "LUNCUR_MODEL_SOURCE", Value: in.ModelSource},
		)
		if file != "" {
			container.Env = append(container.Env, corev1.EnvVar{Name: "MODEL_PATH", Value: modelPath})
		}
	}

	if rt.Name != "custom" {
		probe := func() *corev1.Probe {
			return &corev1.Probe{ProbeHandler: corev1.ProbeHandler{
				HTTPGet: &corev1.HTTPGetAction{Path: "/health", Port: intstr.FromInt32(port)},
			}}
		}
		r := probe()
		// Model load can take minutes (multi-GB weights); be patient before
		// marking the pod unready/dead.
		r.PeriodSeconds, r.FailureThreshold = 10, 60
		l := probe()
		l.InitialDelaySeconds, l.PeriodSeconds, l.FailureThreshold = 120, 30, 20
		container.ReadinessProbe, container.LivenessProbe = r, l
	}

	if init := modelDownloadInit(in, scheme, repo, file, rt.Name, hasEnv); init != nil {
		inits = append(inits, *init)
	}
	return inits, vols, port, nil
}
