package store

import "testing"

func TestAuditAppendListRoundTrip(t *testing.T) {
	s := openTest(t)
	if err := s.AppendAudit("a@b.co", "POST /v1/projects", "/v1/projects"); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendAudit("b@b.co", "DELETE /v1/apps/{app}", "/v1/apps/1"); err != nil {
		t.Fatal(err)
	}
	list, err := s.ListAudit(0, 0, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("list = %+v", list)
	}
	// Newest-first.
	if list[0].UserEmail != "b@b.co" || list[1].UserEmail != "a@b.co" {
		t.Fatalf("order = %+v", list)
	}
}

func TestAuditUserFilter(t *testing.T) {
	s := openTest(t)
	if err := s.AppendAudit("a@b.co", "act1", "t1"); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendAudit("b@b.co", "act2", "t2"); err != nil {
		t.Fatal(err)
	}
	list, err := s.ListAudit(0, 0, "a@b.co", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].UserEmail != "a@b.co" {
		t.Fatalf("list = %+v", list)
	}
}

func TestAuditContainsFilter(t *testing.T) {
	s := openTest(t)
	if err := s.AppendAudit("a@b.co", "POST /v1/projects", "/v1/projects"); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendAudit("a@b.co", "DELETE /v1/apps/{app}", "/v1/apps/1"); err != nil {
		t.Fatal(err)
	}
	list, err := s.ListAudit(0, 0, "", "apps")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Action != "DELETE /v1/apps/{app}" {
		t.Fatalf("list = %+v", list)
	}
}

func TestAuditLimitOffset(t *testing.T) {
	s := openTest(t)
	for i := 0; i < 5; i++ {
		if err := s.AppendAudit("a@b.co", "act", "t"); err != nil {
			t.Fatal(err)
		}
	}
	list, err := s.ListAudit(2, 1, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("list = %+v", list)
	}
}

func TestAuditLimitCap(t *testing.T) {
	s := openTest(t)
	for i := 0; i < 3; i++ {
		if err := s.AppendAudit("a@b.co", "act", "t"); err != nil {
			t.Fatal(err)
		}
	}
	list, err := s.ListAudit(500, 0, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 3 {
		t.Fatalf("list = %+v, want limit capped at 200 (only 3 rows exist)", list)
	}
}

// TestAuditPrune inserts an old row directly (bypassing AppendAudit, which
// always stamps "now") to exercise PruneAudit's age cutoff.
func TestAuditPrune(t *testing.T) {
	s := openTest(t)
	if _, err := s.db.Exec(
		`INSERT INTO audit_log (created_at, user_email, action, target) VALUES (datetime('now', '-100 days'), 'old@b.co', 'act', 't')`,
	); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendAudit("new@b.co", "act", "t"); err != nil {
		t.Fatal(err)
	}
	n, err := s.PruneAudit(90)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("pruned = %d, want 1", n)
	}
	list, err := s.ListAudit(0, 0, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].UserEmail != "new@b.co" {
		t.Fatalf("list after prune = %+v", list)
	}
}

func TestAuditPruneNoopWhenKeepDaysZero(t *testing.T) {
	s := openTest(t)
	if _, err := s.db.Exec(
		`INSERT INTO audit_log (created_at, user_email, action, target) VALUES (datetime('now', '-1000 days'), 'old@b.co', 'act', 't')`,
	); err != nil {
		t.Fatal(err)
	}
	n, err := s.PruneAudit(0)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("pruned = %d, want 0 (keepDays<=0 is a no-op)", n)
	}
	list, err := s.ListAudit(0, 0, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("list = %+v, want old row preserved", list)
	}
}
