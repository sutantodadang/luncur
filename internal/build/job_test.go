package build

import (
	"encoding/json"
	"testing"
)

func TestImageRef(t *testing.T) {
	if got := ImageRef("registry.luncur-system:5000", "web", "api", 42); got != "registry.luncur-system:5000/web-api:42" {
		t.Fatalf("ImageRef=%q", got)
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
		APIVersion string `json:"apiVersion"`
		Metadata   struct{ Name, Namespace string } `json:"metadata"`
		Spec       struct {
			BackoffLimit *int32 `json:"backoffLimit"`
			Template     struct {
				Spec struct {
					RestartPolicy string `json:"restartPolicy"`
					Containers    []struct {
						Image        string `json:"image"`
						Env          []struct{ Name, Value string } `json:"env"`
						VolumeMounts []struct{ Name, MountPath string } `json:"volumeMounts"`
					} `json:"containers"`
					Volumes []struct {
						Name                  string `json:"name"`
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
	c := j.Spec.Template.Spec.Containers[0]
	if c.Image != "luncur/builder:latest" {
		t.Fatalf("image=%q", c.Image)
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
