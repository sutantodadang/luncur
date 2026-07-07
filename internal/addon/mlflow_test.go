package addon

import (
	"strings"
	"testing"
)

func TestRenderMlflow(t *testing.T) {
	objs, err := Render(Params{
		Namespace: "ns", Type: "mlflow", Name: "track1",
		Version: "v2.22.0", SizeGB: 5,
		URLPrefix: "/ui/mlflow/prj-x/track1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(objs) != 3 {
		t.Fatalf("want 3 objects, got %d", len(objs))
	}
	sts := string(objs[0].JSON)
	for _, want := range []string{
		`ghcr.io/mlflow/mlflow:v2.22.0`,
		`"--backend-store-uri"`,
		`sqlite:////data/mlflow.db`,
		`"--static-prefix"`,
		`/ui/mlflow/prj-x/track1/health`,
		`"containerPort":5000`,
		`/data/artifacts`,
	} {
		if !strings.Contains(sts, want) {
			t.Fatalf("StatefulSet missing %s:\n%s", want, sts)
		}
	}
	svc := string(objs[1].JSON)
	if !strings.Contains(svc, `"port":5000`) {
		t.Fatalf("Service must expose 5000:\n%s", svc)
	}
}

func TestRenderMlflowS3(t *testing.T) {
	objs, err := Render(Params{
		Namespace: "ns", Type: "mlflow", Name: "track1",
		Version: "v2.22.0", SizeGB: 5,
		URLPrefix: "/ui/mlflow/prj-x/track1",
		S3: &S3Ref{Endpoint: "http://addon-store1.ns:9000", Key: "k", Secret: "sec", Bucket: "luncur"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(objs) != 3 {
		t.Fatalf("want 3 objects, got %d", len(objs))
	}
	sts := string(objs[0].JSON)
	for _, want := range []string{
		`pip install --quiet boto3`,
		`s3://luncur/mlflow`,
	} {
		if !strings.Contains(sts, want) {
			t.Fatalf("StatefulSet missing %s:\n%s", want, sts)
		}
	}
	if strings.Contains(sts, `/data/artifacts`) {
		t.Fatalf("StatefulSet should not use PVC artifacts path when S3 is set:\n%s", sts)
	}
	sec := string(objs[2].JSON)
	for _, want := range []string{"MLFLOW_S3_ENDPOINT_URL", "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY"} {
		if !strings.Contains(sec, want) {
			t.Fatalf("Secret missing %s:\n%s", want, sec)
		}
	}
}
