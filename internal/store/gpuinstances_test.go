package store

import (
	"errors"
	"testing"
)

func TestGPUInstances(t *testing.T) {
	s := openTest(t)

	g, err := s.CreateGPUInstance("vastai", 777, "luncur-gpu-1", "RTX 4090", 1)
	if err != nil {
		t.Fatal(err)
	}
	if g.Provider != "vastai" || g.ExternalID != 777 || g.Status != "active" {
		t.Fatalf("created = %+v", g)
	}

	list, err := s.ListGPUInstances()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].ID != g.ID {
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
