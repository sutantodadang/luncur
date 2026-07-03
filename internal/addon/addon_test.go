package addon

import (
	"strings"
	"testing"
)

func TestRenderPostgres(t *testing.T) {
	objs, err := Render(Params{
		Namespace: "proj", Type: "postgres", Name: "db1", Version: "16",
		SizeGB: 2, Creds: Creds{User: "app", Password: "pw123", DB: "app"},
	})
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]int{}
	all := ""
	for _, o := range objs {
		kinds[o.Kind]++
		all += string(o.JSON)
	}
	if kinds["StatefulSet"] != 1 || kinds["Service"] != 1 || kinds["Secret"] != 1 {
		t.Fatalf("kinds = %v", kinds)
	}
	for _, want := range []string{
		"postgres:16-alpine", "addon-db1", "addon-db1-creds",
		`"2Gi"`, "POSTGRES_PASSWORD", "pg_isready", `"clusterIP":"None"`,
	} {
		if !strings.Contains(all, want) {
			t.Fatalf("manifests missing %q", want)
		}
	}
}

func TestRenderRedis(t *testing.T) {
	objs, err := Render(Params{
		Namespace: "proj", Type: "redis", Name: "cache", Version: "7",
		SizeGB: 1, Creds: Creds{Password: "pw123"},
	})
	if err != nil {
		t.Fatal(err)
	}
	all := ""
	for _, o := range objs {
		all += string(o.JSON)
	}
	for _, want := range []string{"redis:7-alpine", "requirepass", "REDIS_PASSWORD", "6379"} {
		if !strings.Contains(all, want) {
			t.Fatalf("manifests missing %q", want)
		}
	}
}

func TestRenderRejectsBadType(t *testing.T) {
	if _, err := Render(Params{Namespace: "p", Type: "mysql", Name: "x", Version: "8"}); err == nil {
		t.Fatal("bad type accepted")
	}
}
