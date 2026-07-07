package store

import (
	"errors"
	"testing"
)

func TestGPUInstances(t *testing.T) {
	s := openTest(t)

	g, err := s.CreateGPUInstance("vastai", "computeinstance-abc123", "luncur-gpu-1", "RTX 4090", 1)
	if err != nil {
		t.Fatal(err)
	}
	if g.Provider != "vastai" || g.ExternalRef != "computeinstance-abc123" || g.Status != "active" {
		t.Fatalf("created = %+v", g)
	}

	list, err := s.ListGPUInstances()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].ID != g.ID || list[0].ExternalRef != "computeinstance-abc123" {
		t.Fatalf("list = %+v", list)
	}

	if err := s.MarkGPUInstanceDestroyed(g.ID); err != nil {
		t.Fatal(err)
	}
	list, err = s.ListGPUInstances()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Fatalf("destroyed instance still listed: %+v", list)
	}
	if err := s.MarkGPUInstanceDestroyed(9999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestCreateGPUInstanceWithStatus(t *testing.T) {
	s := openTest(t)

	g, err := s.CreateGPUInstanceWithStatus("nebius", "", "luncur-gpu-2", "H100", 1, "renting")
	if err != nil {
		t.Fatal(err)
	}
	if g.Provider != "nebius" || g.ExternalRef != "" || g.Status != "renting" {
		t.Fatalf("created = %+v", g)
	}

	got, err := s.GetGPUInstance(g.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "renting" {
		t.Fatalf("status = %q, want renting", got.Status)
	}
}

func TestGPUInstanceExternalRefMigration(t *testing.T) {
	s := openTest(t)
	// Simulate a pre-A2 row: write external_id directly, external_ref empty.
	if _, err := s.db.Exec(`INSERT INTO gpu_instances (provider, external_id, external_ref, label, gpu_name, num_gpus, status) VALUES ('vastai', 777, '', 'luncur-gpu-1', 'RTX 4090', 1, 'active')`); err != nil {
		t.Fatal(err)
	}
	if err := backfillGPUExternalRef(s.db); err != nil {
		t.Fatal(err)
	}
	list, err := s.ListGPUInstances()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].ExternalRef != "777" {
		t.Fatalf("backfill: %+v", list)
	}
}
