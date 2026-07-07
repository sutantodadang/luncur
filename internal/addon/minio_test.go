package addon

import (
	"strings"
	"testing"
)

func TestRenderMinio(t *testing.T) {
	objs, err := Render(Params{
		Namespace: "ns", Type: "minio", Name: "store1",
		Version: "RELEASE.2025-04-22T22-12-26Z", SizeGB: 10,
		Creds: Creds{User: "luncur", Password: "pw", DB: "luncur"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(objs) != 3 {
		t.Fatalf("want 3 objects, got %d", len(objs))
	}
	sts := string(objs[0].JSON)
	for _, want := range []string{
		`minio/minio:RELEASE.2025-04-22T22-12-26Z`,
		`"--console-address"`,
		`/minio/health/live`,
		`"containerPort":9000`,
		`"containerPort":9001`,
	} {
		if !strings.Contains(sts, want) {
			t.Fatalf("StatefulSet missing %s:\n%s", want, sts)
		}
	}
	svc := string(objs[1].JSON)
	if !strings.Contains(svc, `"port":9000`) || !strings.Contains(svc, `"port":9001`) {
		t.Fatalf("Service must expose 9000 and 9001:\n%s", svc)
	}
	sec := string(objs[2].JSON)
	if !strings.Contains(sec, "MINIO_ROOT_USER") || !strings.Contains(sec, "MINIO_ROOT_PASSWORD") {
		t.Fatalf("Secret missing minio creds:\n%s", sec)
	}
}

func TestRenderRejectsMlflowHere(t *testing.T) {
	// mlflow is a valid store type but renders via its own path (C5); the
	// generic addon renderer only knows postgres/redis/minio.
	if _, err := Render(Params{Namespace: "ns", Type: "bogus", Name: "x"}); err == nil {
		t.Fatal("bogus type must error")
	}
}
