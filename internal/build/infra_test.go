package build

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestSystemObjects(t *testing.T) {
	objs, err := SystemObjects("luncur-data", "luncur-registry", "registry:2")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, o := range objs {
		var m struct {
			Metadata struct{ Name string } `json:"metadata"`
		}
		json.Unmarshal(o.JSON, &m)
		got[o.Kind+"/"+m.Metadata.Name] = true
	}
	for _, want := range []string{
		"Namespace/luncur-system", "PersistentVolumeClaim/luncur-data",
		"PersistentVolumeClaim/luncur-registry", "Deployment/registry", "Service/registry",
	} {
		if !got[want] {
			t.Fatalf("missing %s (have %v)", want, got)
		}
	}
	all := ""
	for _, o := range objs {
		all += string(o.JSON)
	}
	for _, want := range []string{`"type":"NodePort"`, `"nodePort":30500`, "REGISTRY_STORAGE_DELETE_ENABLED"} {
		if !strings.Contains(all, want) {
			t.Fatalf("registry Service missing %q:\n%s", want, all)
		}
	}
}
