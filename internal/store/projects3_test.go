package store

import (
	"path/filepath"
	"testing"
)

func TestProjectS3CRUD(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "s3.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	p, _ := st.CreateProject("mlp")

	if _, err := st.GetProjectS3(p.ID); err != ErrNotFound {
		t.Fatalf("unset config: err = %v, want ErrNotFound", err)
	}
	cfg := ProjectS3{
		ProjectID: p.ID, Endpoint: "https://s3.example.com", Region: "eu-1",
		Bucket: "models", AccessKeyEnc: []byte("ak"), SecretKeyEnc: []byte("sk"),
	}
	if err := st.SetProjectS3(cfg); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetProjectS3(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Endpoint != cfg.Endpoint || got.Bucket != "models" || string(got.AccessKeyEnc) != "ak" {
		t.Fatalf("roundtrip: %+v", got)
	}

	// Upsert.
	cfg.Bucket = "artifacts"
	if err := st.SetProjectS3(cfg); err != nil {
		t.Fatal(err)
	}
	got, _ = st.GetProjectS3(p.ID)
	if got.Bucket != "artifacts" {
		t.Fatalf("upsert bucket = %s", got.Bucket)
	}

	if err := st.DeleteProjectS3(p.ID); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteProjectS3(p.ID); err != ErrNotFound {
		t.Fatalf("double delete: %v", err)
	}
}

func TestProjectS3Validation(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "s3v.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.SetProjectS3(ProjectS3{ProjectID: 1}); err == nil {
		t.Fatal("empty config must be rejected")
	}
}

func TestMinioAddonType(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	p, _ := st.CreateProject("mlp")
	a, err := st.CreateAddon(p.ID, "minio", "store1", "RELEASE.X", 10, []byte("sealed"))
	if err != nil {
		t.Fatal(err)
	}
	if a.Type != "minio" {
		t.Fatalf("type = %s", a.Type)
	}
	if _, err := st.CreateAddon(p.ID, "bogus", "x", "1", 1, nil); err == nil {
		t.Fatal("bogus addon type must be rejected")
	}
}

func TestInjectS3Flag(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "i.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	p, _ := st.CreateProject("mlp")
	a, _ := st.CreateApp(p.ID, "train", 0, "job", "")
	if a.InjectS3 {
		t.Fatal("inject_s3 must default off")
	}
	if err := st.SetInjectS3(a.ID, true); err != nil {
		t.Fatal(err)
	}
	got, _ := st.GetApp(p.ID, "train")
	if !got.InjectS3 {
		t.Fatal("inject_s3 not persisted")
	}
}
