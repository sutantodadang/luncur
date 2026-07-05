// Package store persists luncur's control-plane metadata in SQLite.
// Cluster state itself lives in K3s (etcd); this DB holds users, apps,
// deploy history, and overrides.
package store

import (
	"database/sql"
	_ "embed"
	"fmt"
	"strings"

	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schemaSQL string

type Store struct {
	db *sql.DB
}

// Open opens (creating if needed) the SQLite DB at path and applies the
// schema. Safe to call repeatedly on the same file.
func Open(path string) (*Store, error) {
	// ponytail: real filesystem paths never contain '?'; a full URL-escape
	// of path is unnecessary — reject the one character that would break
	// the DSN query string below.
	if strings.Contains(path, "?") {
		return nil, fmt.Errorf("db path may not contain '?'")
	}
	dsn := path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	// modernc sqlite is single-writer; avoid SQLITE_BUSY churn.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schemaSQL); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	if err := migrate(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// DB exposes the raw handle for queries owned by sibling files/plans.
func (s *Store) DB() *sql.DB { return s.db }

// migrate adds columns introduced after a table first shipped; schema.sql
// only creates missing tables, so pre-existing DBs need explicit ALTERs.
func migrate(db *sql.DB) error {
	for _, col := range []struct{ table, name, ddl string }{
		{"api_tokens", "expires_at", `ALTER TABLE api_tokens ADD COLUMN expires_at TEXT`},
		{"domains", "cert_status", `ALTER TABLE domains ADD COLUMN cert_status TEXT NOT NULL DEFAULT 'none'`},
		{"domains", "cert_error", `ALTER TABLE domains ADD COLUMN cert_error TEXT NOT NULL DEFAULT ''`},
		{"domains", "cert_expires_at", `ALTER TABLE domains ADD COLUMN cert_expires_at TEXT NOT NULL DEFAULT ''`},
		{"deployments", "rolled_back_from", `ALTER TABLE deployments ADD COLUMN rolled_back_from INTEGER`},
		{"deployments", "seq", `ALTER TABLE deployments ADD COLUMN seq INTEGER NOT NULL DEFAULT 0`},
		{"invites", "created_by", `ALTER TABLE invites ADD COLUMN created_by INTEGER`},
		{"invites", "used_by", `ALTER TABLE invites ADD COLUMN used_by INTEGER`},
		{"invites", "used_at", `ALTER TABLE invites ADD COLUMN used_at TEXT`},
		{"apps", "ejected", `ALTER TABLE apps ADD COLUMN ejected INTEGER NOT NULL DEFAULT 0`},
		{"apps", "cpu_milli", `ALTER TABLE apps ADD COLUMN cpu_milli INTEGER NOT NULL DEFAULT 0`},
		{"apps", "memory_mb", `ALTER TABLE apps ADD COLUMN memory_mb INTEGER NOT NULL DEFAULT 0`},
		{"apps", "health_path", `ALTER TABLE apps ADD COLUMN health_path TEXT NOT NULL DEFAULT ''`},
		{"apps", "kind", `ALTER TABLE apps ADD COLUMN kind TEXT NOT NULL DEFAULT 'web'`},
		{"apps", "schedule", `ALTER TABLE apps ADD COLUMN schedule TEXT NOT NULL DEFAULT ''`},
		{"apps", "webhook_secret", `ALTER TABLE apps ADD COLUMN webhook_secret BLOB`},
		{"apps", "build_path", `ALTER TABLE apps ADD COLUMN build_path TEXT NOT NULL DEFAULT ''`},
		{"apps", "internal", `ALTER TABLE apps ADD COLUMN internal INTEGER NOT NULL DEFAULT 0`},
	} {
		var n int
		if err := db.QueryRow(
			`SELECT count(*) FROM pragma_table_info(?) WHERE name = ?`, col.table, col.name,
		).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			if _, err := db.Exec(col.ddl); err != nil {
				return err
			}
		}
	}

	// Backfill per-app seq for rows written before the column existed:
	// position within the app's history ordered by id. Guarded by
	// `WHERE seq = 0` so it's a no-op once every row has a real seq (new
	// rows are always inserted with seq >= 1 by CreateDeployment) — safe to
	// run unconditionally on every Open.
	if _, err := db.Exec(`
		UPDATE deployments SET seq = (
			SELECT COUNT(*) FROM deployments d2
			WHERE d2.app_id = deployments.app_id AND d2.id <= deployments.id
		) WHERE seq = 0`); err != nil {
		return fmt.Errorf("backfill deployments.seq: %w", err)
	}
	// Created here rather than in schema.sql: schema.sql's CREATE TABLE IF
	// NOT EXISTS is a no-op on a pre-existing deployments table, so a
	// standalone CREATE INDEX there would run (and fail on the missing
	// column) before the ALTER above ever adds it on a legacy DB.
	if _, err := db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_deployments_app_seq ON deployments(app_id, seq)`); err != nil {
		return fmt.Errorf("create deployments seq index: %w", err)
	}
	return nil
}
