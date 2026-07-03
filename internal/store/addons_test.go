package store

import (
	"errors"
	"testing"
)

func TestAddonLifecycle(t *testing.T) {
	s := openTest(t)
	p, _ := s.CreateProject("proj")
	a1, _ := s.CreateApp(p.ID, "web", 8080)
	a2, _ := s.CreateApp(p.ID, "worker", 8080)

	ad, err := s.CreateAddon(p.ID, "postgres", "db1", "16", 1, []byte("sealed"))
	if err != nil {
		t.Fatal(err)
	}
	if ad.Type != "postgres" || ad.Name != "db1" || ad.SizeGB != 1 || string(ad.CredsEnc) != "sealed" {
		t.Fatalf("addon = %+v", ad)
	}
	if _, err := s.CreateAddon(p.ID, "mysql", "db2", "8", 1, nil); err == nil {
		t.Fatal("bad type accepted")
	}
	if _, err := s.CreateAddon(p.ID, "postgres", "db1", "16", 1, nil); err == nil {
		t.Fatal("duplicate name accepted")
	}
	if _, err := s.CreateAddon(p.ID, "postgres", "Bad_Name", "16", 1, nil); err == nil {
		t.Fatal("invalid name accepted")
	}

	got, err := s.GetAddon(p.ID, "db1")
	if err != nil || got.ID != ad.ID {
		t.Fatalf("get: %+v err=%v", got, err)
	}
	if _, err := s.GetAddon(p.ID, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing addon: %v", err)
	}

	if err := s.AttachAddon(ad.ID, a1.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.AttachAddon(ad.ID, a1.ID); err == nil {
		t.Fatal("duplicate attach accepted")
	}
	if err := s.AttachAddon(ad.ID, a2.ID); err != nil {
		t.Fatal(err)
	}

	forApp, err := s.AddonsForApp(a1.ID)
	if err != nil || len(forApp) != 1 || forApp[0].Name != "db1" {
		t.Fatalf("addons for app: %+v err=%v", forApp, err)
	}
	apps, err := s.AppsForAddon(ad.ID)
	if err != nil || len(apps) != 2 {
		t.Fatalf("apps for addon: %+v err=%v", apps, err)
	}

	if err := s.DetachAddon(ad.ID, a1.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.DetachAddon(ad.ID, a1.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second detach: %v", err)
	}

	// Destroying an app cascades its attachments but keeps the addon.
	if err := s.DeleteApp(a2.ID); err != nil {
		t.Fatal(err)
	}
	if apps, _ := s.AppsForAddon(ad.ID); len(apps) != 0 {
		t.Fatalf("attachment survived app delete: %+v", apps)
	}
	if _, err := s.GetAddon(p.ID, "db1"); err != nil {
		t.Fatalf("addon deleted with app: %v", err)
	}

	if err := s.DeleteAddon(ad.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteAddon(ad.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete: %v", err)
	}
}

func TestAllAddons(t *testing.T) {
	s := openTest(t)
	p1, _ := s.CreateProject("proj1")
	p2, _ := s.CreateProject("proj2")

	a1, err := s.CreateAddon(p1.ID, "postgres", "db1", "16", 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	a2, err := s.CreateAddon(p2.ID, "redis", "cache1", "7", 1, nil)
	if err != nil {
		t.Fatal(err)
	}

	all, err := s.AllAddons()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 || all[0].ID != a1.ID || all[1].ID != a2.ID {
		t.Fatalf("all addons = %+v", all)
	}
}
