package store

import (
	"path/filepath"
	"testing"
)

func gpuTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "gpu.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestSetGPURoundtrip(t *testing.T) {
	st := gpuTestStore(t)
	p, err := st.CreateProject("mlproj")
	if err != nil {
		t.Fatal(err)
	}
	a, err := st.CreateApp(p.ID, "trainer", 0, "worker", "")
	if err != nil {
		t.Fatal(err)
	}
	if a.GPUCount != 0 {
		t.Fatalf("new app gpu = %d, want 0", a.GPUCount)
	}
	if err := st.SetGPU(a.ID, 2); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetApp(p.ID, "trainer")
	if err != nil {
		t.Fatal(err)
	}
	if got.GPUCount != 2 {
		t.Fatalf("gpu = %d, want 2", got.GPUCount)
	}
	byID, err := st.GetAppByID(a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if byID.GPUCount != 2 {
		t.Fatalf("byID gpu = %d, want 2", byID.GPUCount)
	}
	list, err := st.ListApps(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].GPUCount != 2 {
		t.Fatalf("list gpu = %+v", list)
	}
	if err := st.SetGPU(a.ID, 0); err != nil {
		t.Fatal(err)
	}
}

func TestSetGPUValidation(t *testing.T) {
	st := gpuTestStore(t)
	p, _ := st.CreateProject("mlproj2")
	a, err := st.CreateApp(p.ID, "web", 8080, "web", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetGPU(a.ID, -1); err == nil {
		t.Fatal("negative gpu must be rejected")
	}
	if err := st.SetGPU(a.ID, 17); err == nil {
		t.Fatal("gpu > 16 must be rejected")
	}
	if err := st.SetGPU(99999, 1); err != ErrNotFound {
		t.Fatalf("missing app: err = %v, want ErrNotFound", err)
	}
}
