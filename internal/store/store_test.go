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
