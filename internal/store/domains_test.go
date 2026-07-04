package store

import (
	"errors"
	"testing"
)

func TestDomainRoundTrip(t *testing.T) {
	s := openTest(t)
	p, err := s.CreateProject("proj")
	if err != nil {
		t.Fatal(err)
	}
	a, err := s.CreateApp(p.ID, "web", 8080, "web", "")
	if err != nil {
		t.Fatal(err)
	}

	d, err := s.AddDomain(a.ID, "WWW.Example.com")
	if err != nil {
		t.Fatal(err)
	}
	if d.Hostname != "www.example.com" {
		t.Fatalf("hostname = %q, want lowercased", d.Hostname)
	}
	if d.CertStatus != "none" {
		t.Fatalf("cert status = %q, want none", d.CertStatus)
	}

	if _, err := s.AddDomain(a.ID, "www.example.com"); err == nil {
		t.Fatal("duplicate hostname accepted")
	}
	for _, bad := range []string{"", "nodot", "-x.example.com", "x..com", "ex ample.com"} {
		if _, err := s.AddDomain(a.ID, bad); err == nil {
			t.Fatalf("invalid hostname %q accepted", bad)
		}
	}

	if err := s.SetDomainCert(d.ID, "issued", "", "2027-01-01T00:00:00Z"); err != nil {
		t.Fatal(err)
	}
	list, err := s.ListDomains(a.ID)
	if err != nil || len(list) != 1 {
		t.Fatalf("list = %+v err=%v", list, err)
	}
	if list[0].CertStatus != "issued" || list[0].CertExpiresAt == "" {
		t.Fatalf("cert fields not persisted: %+v", list[0])
	}

	all, err := s.AllDomains()
	if err != nil || len(all) != 1 {
		t.Fatalf("all = %+v err=%v", all, err)
	}

	if err := s.DeleteDomain(a.ID, "www.example.com"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteDomain(a.ID, "www.example.com"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete: %v, want ErrNotFound", err)
	}
}

func TestAddDomainWildcard(t *testing.T) {
	s := openTest(t)
	p, err := s.CreateProject("proj")
	if err != nil {
		t.Fatal(err)
	}
	a, err := s.CreateApp(p.ID, "web", 8080, "web", "")
	if err != nil {
		t.Fatal(err)
	}

	d, err := s.AddDomain(a.ID, "*.Example.COM")
	if err != nil {
		t.Fatal(err)
	}
	if d.Hostname != "*.example.com" {
		t.Fatalf("hostname = %q", d.Hostname)
	}

	for _, bad := range []string{"*", "*.", "*.*.example.com", "foo.*.example.com", "*example.com"} {
		if _, err := s.AddDomain(a.ID, bad); err == nil {
			t.Fatalf("%q accepted", bad)
		}
	}
}
