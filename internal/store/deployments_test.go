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
	a, _ := st.CreateApp(p.ID, "api", 8080, "web", "")

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

func TestListDeployments(t *testing.T) {
	st := openTest(t)
	p, _ := st.CreateProject("web")
	a, _ := st.CreateApp(p.ID, "api", 8080, "web", "")
	d1, _ := st.CreateDeployment(a.ID, "failed", "img:1", 0)
	d2, _ := st.CreateDeployment(a.ID, "live", "img:2", 0)
	list, err := st.ListDeployments(a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].ID != d2.ID || list[1].ID != d1.ID {
		t.Fatalf("want [d2 d1] newest-first, got %+v", list)
	}
}

func TestRollbackDeployment(t *testing.T) {
	st := openTest(t)
	p, _ := st.CreateProject("proj")
	a, _ := st.CreateApp(p.ID, "web", 8080, "web", "")
	u, err := st.CreateUser("rollback@example.com", "password123", "member")
	if err != nil {
		t.Fatal(err)
	}
	d1, err := st.CreateDeployment(a.ID, "live", "img:1", 0)
	if err != nil {
		t.Fatal(err)
	}
	rb, err := st.CreateRollbackDeployment(a.ID, "img:1", u.ID, d1.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rb.Status != "deploying" || rb.ImageRef != "img:1" || rb.RolledBackFrom != d1.ID {
		t.Fatalf("rollback row = %+v", rb)
	}
	got, err := st.GetDeployment(rb.ID)
	if err != nil || got.RolledBackFrom != d1.ID {
		t.Fatalf("get: %+v err=%v", got, err)
	}
	// Non-rollback rows read back 0.
	got, err = st.GetDeployment(d1.ID)
	if err != nil || got.RolledBackFrom != 0 {
		t.Fatalf("plain row rolled_back_from = %d err=%v", got.RolledBackFrom, err)
	}
}

func TestPing(t *testing.T) {
	st := openTest(t)
	if err := st.Ping(); err != nil {
		t.Fatalf("Ping: %v", err)
	}
}

func TestStuckDeployments(t *testing.T) {
	st := openTest(t)
	p, _ := st.CreateProject("web")
	a, _ := st.CreateApp(p.ID, "api", 8080, "web", "")

	stuck, err := st.CreateDeployment(a.ID, "building", "img:stuck", 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec(
		`UPDATE deployments SET created_at = datetime('now', '-45 minutes') WHERE id = ?`, stuck.ID,
	); err != nil {
		t.Fatal(err)
	}

	if _, err := st.CreateDeployment(a.ID, "building", "img:fresh", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateDeployment(a.ID, "live", "img:live", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateDeployment(a.ID, "failed", "img:failed", 0); err != nil {
		t.Fatal(err)
	}

	got, err := st.StuckDeployments(30)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != stuck.ID {
		t.Fatalf("StuckDeployments = %+v, want only %d", got, stuck.ID)
	}
}

func TestCountDeployments(t *testing.T) {
	st := openTest(t)
	p, _ := st.CreateProject("web")
	a, _ := st.CreateApp(p.ID, "api", 8080, "web", "")
	empty, _ := st.CreateApp(p.ID, "empty", 8081, "web", "")

	if _, err := st.CreateDeployment(a.ID, "failed", "img:1", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateDeployment(a.ID, "live", "img:2", 0); err != nil {
		t.Fatal(err)
	}

	n, err := st.CountDeployments(a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("count = %d, want 2", n)
	}

	n, err = st.CountDeployments(empty.ID)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("count = %d, want 0", n)
	}
}
