// Package store persists luncur's control-plane metadata in SQLite.
// Cluster state itself lives in K3s (etcd); this DB holds users, apps,
// deploy history, and overrides.
package store

import (
	"database/sql"
	_ "embed"
	"fmt"

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
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// DB exposes the raw handle for queries owned by sibling files/plans.
func (s *Store) DB() *sql.DB { return s.db }
