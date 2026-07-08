package store

import "testing"

func TestProjectGPUQuota(t *testing.T) {
	s := openTest(t)
	p := seedProject(t, s)

	// Default: 0 = unlimited.
	got, err := s.GetProject(p.Name)
	if err != nil {
		t.Fatal(err)
	}
	if got.GPUQuota != 0 {
		t.Fatalf("default quota = %d, want 0", got.GPUQuota)
	}

	if err := s.SetProjectGPUQuota(p.ID, 4); err != nil {
		t.Fatal(err)
	}
	got, err = s.GetProject(p.Name)
	if err != nil {
		t.Fatal(err)
	}
	if got.GPUQuota != 4 {
		t.Fatalf("quota = %d, want 4", got.GPUQuota)
	}

	if err := s.SetProjectGPUQuota(p.ID, -1); err == nil {
		t.Fatal("negative quota accepted")
	}
	if err := s.SetProjectGPUQuota(99999, 1); err == nil {
		t.Fatal("unknown project accepted")
	}
}

func TestProjectResourceQuota(t *testing.T) {
	s := openTest(t)
	p := seedProject(t, s)

	// Default: 0/0 = unlimited.
	got, err := s.GetProject(p.Name)
	if err != nil {
		t.Fatal(err)
	}
	if got.CPUQuotaMilli != 0 || got.MemQuotaMB != 0 {
		t.Fatalf("default quota = %d/%d, want 0/0", got.CPUQuotaMilli, got.MemQuotaMB)
	}

	if err := s.SetProjectResourceQuota(p.ID, 4000, 8192); err != nil {
		t.Fatal(err)
	}
	got, err = s.GetProject(p.Name)
	if err != nil {
		t.Fatal(err)
	}
	if got.CPUQuotaMilli != 4000 || got.MemQuotaMB != 8192 {
		t.Fatalf("quota = %d/%d, want 4000/8192", got.CPUQuotaMilli, got.MemQuotaMB)
	}

	if err := s.SetProjectResourceQuota(p.ID, -1, 0); err == nil {
		t.Fatal("negative cpu quota accepted")
	}
	if err := s.SetProjectResourceQuota(p.ID, 0, -1); err == nil {
		t.Fatal("negative memory quota accepted")
	}
	if err := s.SetProjectResourceQuota(99999, 1, 1); err == nil {
		t.Fatal("unknown project accepted")
	}
}

func TestSumProjectGPURequests(t *testing.T) {
	s := openTest(t)
	p := seedProject(t, s)

	// No apps: sum 0.
	sum, err := s.SumProjectGPURequests(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if sum != 0 {
		t.Fatalf("empty sum = %d", sum)
	}

	// web gpu=1 replicas=2 → 2; worker gpu=2 replicas=1 → 2; cron gpu=1 → 1 (cron counts once).
	web, err := s.CreateApp(p.ID, "w", 8080, "web", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetGPU(web.ID, 1); err != nil {
		t.Fatal(err)
	}
	if err := s.SetReplicas(web.ID, 2); err != nil {
		t.Fatal(err)
	}
	wk, err := s.CreateApp(p.ID, "wk", 0, "worker", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetGPU(wk.ID, 2); err != nil {
		t.Fatal(err)
	}
	cr, err := s.CreateApp(p.ID, "cr", 0, "cron", "0 3 * * *")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetGPU(cr.ID, 1); err != nil {
		t.Fatal(err)
	}

	sum, err = s.SumProjectGPURequests(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if sum != 5 {
		t.Fatalf("sum = %d, want 5 (2+2+1)", sum)
	}

	// job gpu=2 nodes=3 → 6 (job apps budget at gpu × nodes, not replicas).
	job, err := s.CreateApp(p.ID, "trainer", 0, "job", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetGPU(job.ID, 2); err != nil {
		t.Fatal(err)
	}
	if err := s.SetAppTraining(job.ID, 3, ""); err != nil {
		t.Fatal(err)
	}

	sum, err = s.SumProjectGPURequests(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if sum != 11 {
		t.Fatalf("sum = %d, want 11 (2+2+1+6)", sum)
	}
}
