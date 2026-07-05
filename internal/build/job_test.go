package build

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/sutantodadang/luncur/internal/render"
)

func TestImageRef(t *testing.T) {
	if got := ImageRef("registry.luncur-system:5000", "web", "api", 42); got != "registry.luncur-system:5000/web-api:42" {
		t.Fatalf("ImageRef=%q", got)
	}
}

func TestCacheRef(t *testing.T) {
	want := "registry.luncur-system:5000/luncur-cache/web-api:buildcache"
	if got := CacheRef("registry.luncur-system:5000", "web", "api"); got != want {
		t.Fatalf("CacheRef=%q, want %q", got, want)
	}
}

func jobEnv(t *testing.T, obj render.Object) map[string]string {
	t.Helper()
	var j struct {
		Spec struct {
			Template struct {
				Spec struct {
					Containers []struct {
						Env []struct{ Name, Value string } `json:"env"`
					} `json:"containers"`
				} `json:"spec"`
			} `json:"template"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(obj.JSON, &j); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{}
	for _, e := range j.Spec.Template.Spec.Containers[0].Env {
		env[e.Name] = e.Value
	}
	return env
}

func TestRenderBuildJobWithCacheRef(t *testing.T) {
	obj, err := RenderBuildJob(BuildParams{
		Namespace: "luncur-system", Name: "build-42", BuilderImage: "luncur/builder:latest",
		DataPVC: "luncur-data", ImageRef: "registry.luncur-system:5000/web-api:42",
		RegistryHost: "registry.luncur-system:5000", SourceType: "tarball", DeployID: 42,
		CacheRef: "registry.luncur-system:5000/luncur-cache/web-api:buildcache",
	})
	if err != nil {
		t.Fatal(err)
	}
	env := jobEnv(t, obj)
	if env["LUNCUR_CACHE_REF"] != "registry.luncur-system:5000/luncur-cache/web-api:buildcache" {
		t.Fatalf("LUNCUR_CACHE_REF=%q", env["LUNCUR_CACHE_REF"])
	}
}

func TestRenderBuildJobWithoutCacheRef(t *testing.T) {
	obj, err := RenderBuildJob(BuildParams{
		Namespace: "luncur-system", Name: "build-42", BuilderImage: "luncur/builder:latest",
		DataPVC: "luncur-data", ImageRef: "registry.luncur-system:5000/web-api:42",
		RegistryHost: "registry.luncur-system:5000", SourceType: "tarball", DeployID: 42,
	})
	if err != nil {
		t.Fatal(err)
	}
	env := jobEnv(t, obj)
	if _, ok := env["LUNCUR_CACHE_REF"]; ok {
		t.Fatalf("LUNCUR_CACHE_REF present, want absent: %+v", env)
	}
}

func TestRenderBuildJobRootlessSecurity(t *testing.T) {
	obj, err := RenderBuildJob(BuildParams{
		Namespace: "luncur-system", Name: "build-42", BuilderImage: "luncur/builder:latest",
		DataPVC: "luncur-data", ImageRef: "registry.luncur-system:5000/web-api:42",
		RegistryHost: "registry.luncur-system:5000", SourceType: "tarball", DeployID: 42,
	})
	if err != nil {
		t.Fatal(err)
	}
	s := string(obj.JSON)
	for _, want := range []string{
		`"container.apparmor.security.beta.kubernetes.io/builder":"unconfined"`,
		`"seccompProfile":{"type":"Unconfined"}`,
		`"runAsUser":1000`,
		`"runAsGroup":1000`,
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("build job missing %s:\n%s", want, s)
		}
	}
}

func TestRenderBuildJobWithBuildPath(t *testing.T) {
	obj, err := RenderBuildJob(BuildParams{
		Namespace: "luncur-system", Name: "build-42", BuilderImage: "luncur/builder:latest",
		DataPVC: "luncur-data", ImageRef: "registry.luncur-system:5000/web-api:42",
		RegistryHost: "registry.luncur-system:5000", SourceType: "git", DeployID: 42,
		BuildPath: "backend",
	})
	if err != nil {
		t.Fatal(err)
	}
	env := jobEnv(t, obj)
	if env["LUNCUR_BUILD_PATH"] != "backend" {
		t.Fatalf("LUNCUR_BUILD_PATH=%q", env["LUNCUR_BUILD_PATH"])
	}
}

func TestRenderBuildJobWithoutBuildPath(t *testing.T) {
	obj, err := RenderBuildJob(BuildParams{
		Namespace: "luncur-system", Name: "build-42", BuilderImage: "luncur/builder:latest",
		DataPVC: "luncur-data", ImageRef: "registry.luncur-system:5000/web-api:42",
		RegistryHost: "registry.luncur-system:5000", SourceType: "tarball", DeployID: 42,
	})
	if err != nil {
		t.Fatal(err)
	}
	env := jobEnv(t, obj)
	if _, ok := env["LUNCUR_BUILD_PATH"]; ok {
		t.Fatalf("LUNCUR_BUILD_PATH present, want absent: %+v", env)
	}
}

func TestRenderBuildJob(t *testing.T) {
	obj, err := RenderBuildJob(BuildParams{
		Namespace: "luncur-system", Name: "build-42", BuilderImage: "luncur/builder:latest",
		DataPVC: "luncur-data", ImageRef: "registry.luncur-system:5000/web-api:42",
		RegistryHost: "registry.luncur-system:5000", SourceType: "tarball", DeployID: 42,
	})
	if err != nil {
		t.Fatal(err)
	}
	if obj.Kind != "Job" {
		t.Fatalf("Kind=%q", obj.Kind)
	}
	var j struct {
		APIVersion string                           `json:"apiVersion"`
		Metadata   struct{ Name, Namespace string } `json:"metadata"`
		Spec       struct {
			BackoffLimit            *int32 `json:"backoffLimit"`
			TTLSecondsAfterFinished *int32 `json:"ttlSecondsAfterFinished"`
			ActiveDeadlineSeconds   *int64 `json:"activeDeadlineSeconds"`
			Template                struct {
				Spec struct {
					RestartPolicy string `json:"restartPolicy"`
					Containers    []struct {
						Image        string                             `json:"image"`
						Env          []struct{ Name, Value string }     `json:"env"`
						VolumeMounts []struct{ Name, MountPath string } `json:"volumeMounts"`
						Resources    struct {
							Requests map[string]string `json:"requests"`
							Limits   map[string]string `json:"limits"`
						} `json:"resources"`
					} `json:"containers"`
					Volumes []struct {
						Name                  string                     `json:"name"`
						PersistentVolumeClaim struct{ ClaimName string } `json:"persistentVolumeClaim"`
					} `json:"volumes"`
				} `json:"spec"`
			} `json:"template"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(obj.JSON, &j); err != nil {
		t.Fatal(err)
	}
	if j.APIVersion != "batch/v1" || j.Metadata.Namespace != "luncur-system" {
		t.Fatalf("bad meta: %+v", j.Metadata)
	}
	if j.Spec.BackoffLimit == nil || *j.Spec.BackoffLimit != 0 {
		t.Fatalf("backoffLimit not 0")
	}
	if j.Spec.TTLSecondsAfterFinished == nil || *j.Spec.TTLSecondsAfterFinished != 3600 {
		t.Fatalf("ttlSecondsAfterFinished not 3600")
	}
	if j.Spec.ActiveDeadlineSeconds == nil || *j.Spec.ActiveDeadlineSeconds != 900 {
		t.Fatalf("activeDeadlineSeconds not 900")
	}
	c := j.Spec.Template.Spec.Containers[0]
	if c.Image != "luncur/builder:latest" {
		t.Fatalf("image=%q", c.Image)
	}
	if c.Resources.Requests["cpu"] != "100m" || c.Resources.Requests["memory"] != "256Mi" {
		t.Fatalf("resource requests: %+v", c.Resources.Requests)
	}
	if c.Resources.Limits["memory"] != "2Gi" {
		t.Fatalf("resource limits: %+v", c.Resources.Limits)
	}
	env := map[string]string{}
	for _, e := range c.Env {
		env[e.Name] = e.Value
	}
	if env["LUNCUR_IMAGE_REF"] != "registry.luncur-system:5000/web-api:42" || env["LUNCUR_DEPLOY_ID"] != "42" {
		t.Fatalf("env missing/wrong: %+v", env)
	}
	if c.VolumeMounts[0].MountPath != "/data" || j.Spec.Template.Spec.Volumes[0].PersistentVolumeClaim.ClaimName != "luncur-data" {
		t.Fatalf("data volume not wired")
	}
	if j.Spec.Template.Spec.RestartPolicy != "Never" {
		t.Fatalf("restartPolicy=%q", j.Spec.Template.Spec.RestartPolicy)
	}
}
