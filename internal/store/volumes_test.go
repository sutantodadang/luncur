package store

import (
	"errors"
	"testing"
)

func setupVolumeApp(t *testing.T) (*Store, int64) {
	t.Helper()
	s := openTest(t)
	p, err := s.CreateProject("proj")
	if err != nil {
		t.Fatal(err)
	}
	a, err := s.CreateApp(p.ID, "web", 8080, "web", "")
	if err != nil {
		t.Fatal(err)
	}
	return s, a.ID
}

func TestVolumeRoundTrip(t *testing.T) {
	s, appID := setupVolumeApp(t)

	v, err := s.AddVolume(appID, "data", "/data", 10)
	if err != nil {
		t.Fatal(err)
	}
	if v.ID == 0 || v.Name != "data" || v.Path != "/data" || v.SizeGB != 10 {
		t.Fatalf("added volume: %+v", v)
	}

	list, err := s.ListVolumes(appID)
	if err != nil || len(list) != 1 {
		t.Fatalf("list = %+v err=%v", list, err)
	}
	if list[0].Name != "data" {
		t.Fatalf("list[0] = %+v", list[0])
	}

	if err := s.DeleteVolume(appID, "data"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteVolume(appID, "data"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete: %v, want ErrNotFound", err)
	}
}

func TestVolumeNameDefaultsToLastPathSegment(t *testing.T) {
	s, appID := setupVolumeApp(t)

	v, err := s.AddVolume(appID, "", "/var/lib/postgres", 5)
	if err != nil {
		t.Fatal(err)
	}
	if v.Name != "postgres" {
		t.Fatalf("name = %q, want %q", v.Name, "postgres")
	}
}

func TestVolumeValidationMatrix(t *testing.T) {
	s, appID := setupVolumeApp(t)

	cases := []struct {
		name, path string
		size       int
	}{
		{"", "relative/path", 5},                 // not absolute
		{"", "/" + string(make([]byte, 260)), 5}, // too long
		{"", "/bad path", 5},                     // whitespace
		{"", "/data\x7f", 5},                     // control char
		{"Bad_Name", "/data", 5},                 // invalid name (uppercase/underscore)
		{"", "/data", 0},                         // size too small
		{"", "/data", 1001},                      // size too large
	}
	for i, c := range cases {
		if _, err := s.AddVolume(appID, c.name, c.path, c.size); err == nil {
			t.Fatalf("case %d (%+v): want error, got none", i, c)
		}
	}
}

func TestVolumeUniqueNameCollision(t *testing.T) {
	s, appID := setupVolumeApp(t)
	if _, err := s.AddVolume(appID, "data", "/data", 5); err != nil {
		t.Fatal(err)
	}
	_, err := s.AddVolume(appID, "data", "/other", 5)
	if err == nil {
		t.Fatal("duplicate name accepted")
	}
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("want *ValidationError, got %T: %v", err, err)
	}
}

func TestVolumeUniquePathCollision(t *testing.T) {
	s, appID := setupVolumeApp(t)
	if _, err := s.AddVolume(appID, "data", "/data", 5); err != nil {
		t.Fatal(err)
	}
	_, err := s.AddVolume(appID, "other", "/data", 5)
	if err == nil {
		t.Fatal("duplicate path accepted")
	}
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("want *ValidationError, got %T: %v", err, err)
	}
}

func TestVolumeCascadesOnAppDelete(t *testing.T) {
	s, appID := setupVolumeApp(t)
	if _, err := s.AddVolume(appID, "data", "/data", 5); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteApp(appID); err != nil {
		t.Fatal(err)
	}
	list, err := s.ListVolumes(appID)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Fatalf("volumes survived app delete: %+v", list)
	}
}
