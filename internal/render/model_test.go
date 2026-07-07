package render

import (
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
)

func TestParseModelSource(t *testing.T) {
	cases := []struct {
		in                  string
		scheme, repo, file  string
		wantErr             bool
	}{
		{"hf:unsloth/gemma-3n-E4B-it-GGUF/gemma-3n-E4B-it-Q4_K_M.gguf", "hf", "unsloth/gemma-3n-E4B-it-GGUF", "gemma-3n-E4B-it-Q4_K_M.gguf", false},
		{"hf:org/name", "hf", "org/name", "", false},
		{"hf:org/name/sub/dir/f.gguf", "hf", "org/name", "sub/dir/f.gguf", false},
		{"s3:models/llama.gguf", "s3", "", "models/llama.gguf", false},
		{"hf:orgonly", "", "", "", true},
		{"s3:", "", "", "", true},
		{"http://x", "", "", "", true},
	}
	for _, c := range cases {
		scheme, repo, file, err := ParseModelSource(c.in)
		if c.wantErr != (err != nil) {
			t.Fatalf("%s: err = %v", c.in, err)
		}
		if err == nil && (scheme != c.scheme || repo != c.repo || file != c.file) {
			t.Fatalf("%s: got (%s,%s,%s)", c.in, scheme, repo, file)
		}
	}
}

func TestResolveModelRuntime(t *testing.T) {
	// auto + GGUF -> llamacpp, CPU or GPU.
	rt, err := ResolveModelRuntime("hf:o/n/m.gguf", "auto", 0)
	if err != nil || rt.Name != "llamacpp" || rt.Port != 8080 {
		t.Fatalf("gguf auto: %+v %v", rt, err)
	}
	// auto + non-GGUF + GPU -> vllm.
	rt, err = ResolveModelRuntime("hf:o/n", "", 1)
	if err != nil || rt.Name != "vllm" || rt.Port != 8000 {
		t.Fatalf("hf gpu auto: %+v %v", rt, err)
	}
	// auto + non-GGUF + no GPU -> error.
	if _, err := ResolveModelRuntime("hf:o/n", "auto", 0); err == nil {
		t.Fatal("non-gguf cpu auto must error")
	}
	// llamacpp on non-GGUF -> error.
	if _, err := ResolveModelRuntime("hf:o/n", "llamacpp", 0); err == nil {
		t.Fatal("llamacpp non-gguf must error")
	}
	// custom has no image.
	rt, err = ResolveModelRuntime("s3:m/x.bin", "custom", 0)
	if err != nil || rt.Name != "custom" || rt.Image != "" {
		t.Fatalf("custom: %+v %v", rt, err)
	}
	if _, err := ResolveModelRuntime("hf:o/n", "bogus", 0); err == nil {
		t.Fatal("bogus runtime must error")
	}
}

func modelDeployment(t *testing.T, in Input, env map[string]string) (appsv1.Deployment, Rendered) {
	t.Helper()
	r, err := Render(in, env)
	if err != nil {
		t.Fatal(err)
	}
	return findDeployment(t, r), r
}

func TestRenderModelLlamaCppCPU(t *testing.T) {
	d, r := modelDeployment(t, Input{
		AppName: "gemma", Namespace: "ns", Image: "ignored:0", Host: "gemma.example.com",
		Kind: "model", Replicas: 1,
		ModelSource: "hf:unsloth/gemma-3n-E4B-it-GGUF/gemma-3n-E4B-it-Q4_K_M.gguf",
	}, nil)
	spec := d.Spec.Template.Spec
	c := spec.Containers[0]
	if c.Image != llamaCppImage {
		t.Fatalf("image = %s", c.Image)
	}
	args := strings.Join(c.Args, " ")
	if !strings.Contains(args, "-m /models/gemma-3n-E4B-it-Q4_K_M.gguf") || !strings.Contains(args, "--port 8080") {
		t.Fatalf("args = %s", args)
	}
	if strings.Contains(args, "--n-gpu-layers") {
		t.Fatalf("cpu render must not offload layers: %s", args)
	}
	if len(spec.InitContainers) != 1 || spec.InitContainers[0].Image != curlImage {
		t.Fatalf("init containers = %+v", spec.InitContainers)
	}
	dl := strings.Join(spec.InitContainers[0].Command, " ")
	if !strings.Contains(dl, `curl -fL --retry 3 -o "/models/gemma-3n-E4B-it-Q4_K_M.gguf" "$MODEL_URL"`) {
		t.Fatalf("download cmd = %s", dl)
	}
	// The HF URL is passed via env, never as shell text.
	var modelURL string
	for _, e := range spec.InitContainers[0].Env {
		if e.Name == "MODEL_URL" {
			modelURL = e.Value
		}
	}
	if !strings.Contains(modelURL, "huggingface.co/unsloth/gemma-3n-E4B-it-GGUF/resolve/main/gemma-3n-E4B-it-Q4_K_M.gguf") {
		t.Fatalf("MODEL_URL env = %s", modelURL)
	}
	if c.ReadinessProbe == nil || c.ReadinessProbe.HTTPGet.Path != "/health" {
		t.Fatalf("readiness = %+v", c.ReadinessProbe)
	}
	// Service targets 8080; Ingress rendered.
	var kinds []string
	svcOK := false
	for _, o := range r.Objects {
		kinds = append(kinds, o.Kind)
		if o.Kind == "Service" && strings.Contains(string(o.JSON), `"targetPort":8080`) {
			svcOK = true
		}
	}
	joined := strings.Join(kinds, ",")
	if !svcOK || !strings.Contains(joined, "Ingress") {
		t.Fatalf("objects = %s (svcOK=%v)", joined, svcOK)
	}
}

func TestRenderModelVLLMGPU(t *testing.T) {
	d, _ := modelDeployment(t, Input{
		AppName: "llm", Namespace: "ns", Image: "ignored:0", Host: "llm.example.com",
		Kind: "model", Replicas: 1, GPU: 1,
		ModelSource: "hf:meta-llama/Llama-3.1-8B-Instruct",
	}, nil)
	spec := d.Spec.Template.Spec
	c := spec.Containers[0]
	if c.Image != vllmImage {
		t.Fatalf("image = %s", c.Image)
	}
	args := strings.Join(c.Args, " ")
	if !strings.Contains(args, "--model meta-llama/Llama-3.1-8B-Instruct") || !strings.Contains(args, "--port 8000") {
		t.Fatalf("args = %s", args)
	}
	if len(spec.InitContainers) != 0 {
		t.Fatalf("vllm+hf must not have an init download: %+v", spec.InitContainers)
	}
	if spec.RuntimeClassName == nil || *spec.RuntimeClassName != GPURuntimeClass {
		t.Fatalf("runtimeClassName = %v", spec.RuntimeClassName)
	}
	// GPU model deployments use Recreate (single GPU can't host both pods).
	if d.Spec.Strategy.Type != appsv1.RecreateDeploymentStrategyType {
		t.Fatalf("strategy = %s, want Recreate", d.Spec.Strategy.Type)
	}
}

func TestRenderModelS3Source(t *testing.T) {
	d, _ := modelDeployment(t, Input{
		AppName: "m", Namespace: "ns", Image: "ignored:0", Host: "m.example.com",
		Kind: "model", Replicas: 1,
		ModelSource: "s3:models/tiny.gguf",
	}, map[string]string{"LUNCUR_S3_KEY": "k"})
	spec := d.Spec.Template.Spec
	if len(spec.InitContainers) != 1 || spec.InitContainers[0].Image != awsCLIImage {
		t.Fatalf("init = %+v", spec.InitContainers)
	}
	cmd := strings.Join(spec.InitContainers[0].Command, " ")
	// The S3 key is passed via env, never spliced into the shell command.
	if !strings.Contains(cmd, `s3://$LUNCUR_S3_BUCKET/$MODEL_KEY`) {
		t.Fatalf("cmd = %s", cmd)
	}
	var modelKey string
	for _, e := range spec.InitContainers[0].Env {
		if e.Name == "MODEL_KEY" {
			modelKey = e.Value
		}
	}
	if modelKey != "models/tiny.gguf" {
		t.Fatalf("MODEL_KEY env = %q, want models/tiny.gguf", modelKey)
	}
	if len(spec.InitContainers[0].EnvFrom) != 1 {
		t.Fatal("s3 init must read the app secret for credentials")
	}
}

func TestParseModelSourceRejectsInjection(t *testing.T) {
	bad := []string{
		"s3:models/$(whoami).gguf",
		"s3:a;rm -rf /",
		"hf:org/name/../../etc/passwd",
		"s3:models/`id`.gguf",
		"hf:org/na me/f.gguf",
		`s3:"; curl evil ;"`,
	}
	for _, src := range bad {
		if _, _, _, err := ParseModelSource(src); err == nil {
			t.Fatalf("source %q accepted, want rejected", src)
		}
	}
	for _, src := range []string{"hf:org/name/model.Q4_K_M.gguf", "s3:models/sub/tiny.gguf", "hf:org/name"} {
		if _, _, _, err := ParseModelSource(src); err != nil {
			t.Fatalf("source %q rejected: %v", src, err)
		}
	}
}

func TestRenderModelCustomRuntime(t *testing.T) {
	d, _ := modelDeployment(t, Input{
		AppName: "m", Namespace: "ns", Image: "user/serving:1", Host: "m.example.com",
		Kind: "model", Replicas: 1, Runtime: "custom",
		ModelSource: "hf:org/name/w.bin",
	}, nil)
	c := d.Spec.Template.Spec.Containers[0]
	if c.Image != "user/serving:1" {
		t.Fatalf("custom runtime must keep the deployed image, got %s", c.Image)
	}
	env := map[string]string{}
	for _, e := range c.Env {
		env[e.Name] = e.Value
	}
	if env["MODEL_PATH"] != "/models/w.bin" || env["LUNCUR_MODEL_SOURCE"] != "hf:org/name/w.bin" {
		t.Fatalf("env = %v", env)
	}
	if c.ReadinessProbe != nil {
		t.Fatal("custom runtime gets no built-in probes")
	}
}

func TestRenderModelRequiresSource(t *testing.T) {
	_, err := Render(Input{AppName: "m", Namespace: "ns", Image: "i", Host: "h", Kind: "model"}, nil)
	if err == nil {
		t.Fatal("model without source must error")
	}
}
