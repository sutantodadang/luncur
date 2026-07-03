package store

import (
	"testing"
)

func TestCreateDeploymentAttribution(t *testing.T) {
	st := openTest(t)
	u, err := st.CreateUser("dev@x.io", "password123", "admin")
	if err != nil {
		t.Fatal(err)
	}
	p, _ := st.CreateProject("web")
	a, _ := st.CreateApp(p.ID, "api", 8080)

	d, err := st.CreateDeployment(a.ID, "building", "", u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetDeploymentImage(d.ID, "reg/web-api:1"); err != nil {
		t.Fatal(err)
	}
	if err := st.SetDeploymentLog(d.ID, "/data/logs/1.log"); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetDeployment(d.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ImageRef != "reg/web-api:1" || got.LogPath != "/data/logs/1.log" {
		t.Fatalf("image/log not persisted: %+v", got)
	}
	if !got.CreatedBy.Valid || got.CreatedBy.Int64 != u.ID {
		t.Fatalf("created_by = %+v, want %d", got.CreatedBy, u.ID)
	}
}
