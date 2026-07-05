package store

import (
	"database/sql"
	"path/filepath"
	"testing"
)

func openTest(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestOpenMigratesSchema(t *testing.T) {
	s := openTest(t)
	for _, table := range []string{
		"users", "api_tokens", "projects", "project_members",
		"apps", "deployments", "env_vars", "domains", "overrides", "invites",
	} {
		var n int
		err := s.DB().QueryRow(
			`SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?`, table,
		).Scan(&n)
		if err != nil || n != 1 {
			t.Errorf("table %s missing (n=%d err=%v)", table, n, err)
		}
	}
}

func TestOpenRejectsPathWithQuestionMark(t *testing.T) {
	if _, err := Open("foo?bar"); err == nil {
		t.Fatal("want error for path containing '?'")
	}
}

// TestMigrateAddsExpiresAt covers the ALTER TABLE branch in migrate: a DB
// created before Task 1 has api_tokens without expires_at, and Open must
// add the column rather than fail.
func TestMigrateAddsExpiresAt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	// Legacy pre-Task-1 DDL: no expires_at column.
	if _, err := db.Exec(`
		CREATE TABLE users (
		  id            INTEGER PRIMARY KEY,
		  email         TEXT NOT NULL UNIQUE,
		  password_hash TEXT NOT NULL,
		  role          TEXT NOT NULL CHECK (role IN ('admin','member')),
		  created_at    TEXT NOT NULL DEFAULT (datetime('now'))
		);
		CREATE TABLE api_tokens (
		  id           INTEGER PRIMARY KEY,
		  user_id      INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		  hash         TEXT NOT NULL UNIQUE,
		  name         TEXT NOT NULL,
		  last_used_at TEXT,
		  created_at   TEXT NOT NULL DEFAULT (datetime('now'))
		);`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open on legacy DB: %v", err)
	}
	defer s.Close()
	var n int
	if err := s.DB().QueryRow(
		`SELECT count(*) FROM pragma_table_info('api_tokens') WHERE name = 'expires_at'`,
	).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("expires_at column missing after migrate (n=%d)", n)
	}
}

// TestMigrateBackfillsDeploymentSeq covers the seq backfill branch in
// migrate: a DB created before the per-app deploy numbering column existed
// has deployments rows with no seq, and Open must both add the column and
// derive each row's per-app position from insertion (id) order, then land
// a unique (app_id, seq) index.
func TestMigrateBackfillsDeploymentSeq(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	// Legacy pre-seq DDL: deployments without the seq column, two apps with
	// interleaved deployment ids.
	if _, err := db.Exec(`
		CREATE TABLE apps (id INTEGER PRIMARY KEY, name TEXT NOT NULL);
		CREATE TABLE deployments (
		  id         INTEGER PRIMARY KEY,
		  app_id     INTEGER NOT NULL REFERENCES apps(id) ON DELETE CASCADE,
		  status     TEXT NOT NULL,
		  image_ref  TEXT,
		  log_path   TEXT,
		  created_by INTEGER,
		  created_at TEXT NOT NULL DEFAULT (datetime('now')),
		  rolled_back_from INTEGER
		);
		INSERT INTO apps (id, name) VALUES (1, 'api'), (2, 'worker');
		INSERT INTO deployments (id, app_id, status, image_ref) VALUES
		  (1, 1, 'live', 'img:1'),  -- api's #1
		  (2, 2, 'live', 'img:1'),  -- worker's #1
		  (3, 1, 'live', 'img:2'),  -- api's #2
		  (4, 1, 'live', 'img:3'),  -- api's #3
		  (5, 2, 'live', 'img:2'); -- worker's #2
		`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open on legacy DB: %v", err)
	}
	defer s.Close()

	// Ids are regenerated to opaque nanoids by the same Open() call (see
	// migrateDeploymentIDsToText), so rows can no longer be looked up by
	// their old integer id — match by (app_id, seq) instead, and confirm
	// every id now looks like a nanoid.
	wantAPISeqs := []int64{1, 2, 3}
	apiDeploys, err := s.ListDeployments(1)
	if err != nil {
		t.Fatalf("ListDeployments(api): %v", err)
	}
	if len(apiDeploys) != len(wantAPISeqs) {
		t.Fatalf("api deploys = %+v, want %d rows", apiDeploys, len(wantAPISeqs))
	}
	gotAPISeqs := map[int64]bool{}
	for _, d := range apiDeploys {
		gotAPISeqs[d.Seq] = true
		if !idPattern.MatchString(d.ID) {
			t.Errorf("deployment id %q not migrated to nanoid shape", d.ID)
		}
	}
	for _, want := range wantAPISeqs {
		if !gotAPISeqs[want] {
			t.Errorf("api deploys missing seq %d: %+v", want, apiDeploys)
		}
	}

	workerDeploys, err := s.ListDeployments(2)
	if err != nil {
		t.Fatalf("ListDeployments(worker): %v", err)
	}
	if len(workerDeploys) != 2 {
		t.Fatalf("worker deploys = %+v, want 2 rows", workerDeploys)
	}

	// Re-running Open (migrate again) must stay idempotent: no change, and
	// the unique index must reject a genuine (app_id, seq) collision.
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("second Open on already-migrated DB: %v", err)
	}
	defer s2.Close()
	if _, err := s.DB().Exec(`INSERT INTO deployments (id, app_id, seq, status) VALUES ('zzzzzzzzzzzz', 1, 1, 'live')`); err == nil {
		t.Fatal("want unique constraint violation inserting a duplicate (app_id, seq)")
	}
}

func TestOpenIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	for i := 0; i < 2; i++ {
		s, err := Open(path)
		if err != nil {
			t.Fatalf("open #%d: %v", i+1, err)
		}
		s.Close()
	}
}
