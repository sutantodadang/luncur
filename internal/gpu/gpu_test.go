package gpu

import (
	"strings"
	"testing"
)

func TestObjects(t *testing.T) {
	objs, err := Objects("luncur-system")
	if err != nil {
		t.Fatal(err)
	}
	if len(objs) != 2 {
		t.Fatalf("want 2 objects, got %d", len(objs))
	}
	if objs[0].Kind != "RuntimeClass" || objs[1].Kind != "DaemonSet" {
		t.Fatalf("kinds = %s, %s", objs[0].Kind, objs[1].Kind)
	}
	rc := string(objs[0].JSON)
	for _, want := range []string{`"name":"nvidia"`, `"handler":"nvidia"`} {
		if !strings.Contains(rc, want) {
			t.Fatalf("RuntimeClass missing %s:\n%s", want, rc)
		}
	}
	ds := string(objs[1].JSON)
	for _, want := range []string{
		`"namespace":"luncur-system"`,
		`"luncur.dev/gpu":"true"`,
		`"runtimeClassName":"nvidia"`,
		`nvcr.io/nvidia/k8s-device-plugin`,
		`/var/lib/kubelet/device-plugins`,
	} {
		if !strings.Contains(ds, want) {
			t.Fatalf("DaemonSet missing %s:\n%s", want, ds)
		}
	}
}
