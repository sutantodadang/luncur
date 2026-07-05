// Package store persists luncur's control-plane metadata in SQLite.
// Cluster state itself lives in K3s (etcd); this DB holds users, apps,
// deploy history, and overrides.
package store

import (
	"database/sql"
	_ "embed"
	"errors"
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
	// Migrating deployments.id from INTEGER to TEXT (nanoid) rebuilds the
	// whole table, so it runs after the ALTER loop and seq backfill above —
	// both depend on the legacy INTEGER id's natural ordering and on
	// columns (seq, rolled_back_from) that must already exist by this
	// point.
	if err := migrateDeploymentIDsToText(db); err != nil {
		return fmt.Errorf("migrate deployment ids to text: %w", err)
	}

	// Created here rather than in schema.sql: schema.sql's CREATE TABLE IF
	// NOT EXISTS is a no-op on a pre-existing deployments table, so a
	// standalone CREATE INDEX there would run (and fail on the missing
	// column) before the ALTER above ever adds it on a legacy DB. Also
	// re-creates the index dropped along with the old table by
	// migrateDeploymentIDsToText, above.
	if _, err := db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_deployments_app_seq ON deployments(app_id, seq)`); err != nil {
		return fmt.Errorf("create deployments seq index: %w", err)
	}
	return nil
}

// deploymentsTextSchema is the current deployments table shape (kept in sync
// with schema.sql's CREATE TABLE by hand — schema.sql itself can't be
// reused here since it's `CREATE TABLE IF NOT EXISTS`, a no-op mid-migration
// while the legacy `deployments` table still exists under that name).
const deploymentsTextSchema = `
CREATE TABLE deployments_new (
  id               TEXT PRIMARY KEY,
  app_id           INTEGER NOT NULL REFERENCES apps(id) ON DELETE CASCADE,
  seq              INTEGER NOT NULL DEFAULT 0,
  status           TEXT NOT NULL CHECK (status IN ('building','deploying','live','failed')),
  image_ref        TEXT,
  log_path         TEXT,
  created_by       INTEGER REFERENCES users(id) ON DELETE SET NULL,
  created_at       TEXT NOT NULL DEFAULT (datetime('now')),
  rolled_back_from TEXT
)`

// migrateDeploymentIDsToText rebuilds the deployments table so id (and
// rolled_back_from, which references another row's id) become opaque
// nanoids instead of the old global integer counter. Beta software, owner
// has approved a breaking change here: data is preserved, but every id is
// regenerated, and any build log/tarball already on disk under the old
// `<int>.log` / `<int>.tar.gz` names orphans (nothing references those
// paths by the old id anymore — see README).
//
// A single forward pass ordered by the legacy integer id assigns each row
// a fresh id and records old->new in a map; since rolled_back_from always
// points at an earlier (smaller) id, that mapping is always already known
// by the time a row needing it is processed.
func migrateDeploymentIDsToText(db *sql.DB) error {
	var idType string
	err := db.QueryRow(
		`SELECT type FROM pragma_table_info('deployments') WHERE name = 'id'`,
	).Scan(&idType)
	if errors.Is(err, sql.ErrNoRows) {
		// No deployments table at all yet (shouldn't happen post-schemaSQL,
		// but nothing to migrate either way).
		return nil
	}
	if err != nil {
		return err
	}
	if !strings.EqualFold(idType, "INTEGER") {
		// Already TEXT (fresh DB, or already migrated) — nothing to do.
		return nil
	}

	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after a successful Commit

	if _, err := tx.Exec(deploymentsTextSchema); err != nil {
		return fmt.Errorf("create deployments_new: %w", err)
	}

	rows, err := tx.Query(`
		SELECT id, app_id, seq, status, image_ref, log_path, created_by, created_at, rolled_back_from
		FROM deployments ORDER BY id ASC`)
	if err != nil {
		return fmt.Errorf("read legacy deployments: %w", err)
	}
	type legacyRow struct {
		oldID          int64
		appID          int64
		seq            int64
		status         string
		imageRef       sql.NullString
		logPath        sql.NullString
		createdBy      sql.NullInt64
		createdAt      string
		rolledBackFrom sql.NullInt64
	}
	var legacy []legacyRow
	for rows.Next() {
		var r legacyRow
		if err := rows.Scan(&r.oldID, &r.appID, &r.seq, &r.status, &r.imageRef, &r.logPath,
			&r.createdBy, &r.createdAt, &r.rolledBackFrom); err != nil {
			rows.Close()
			return fmt.Errorf("scan legacy deployment: %w", err)
		}
		legacy = append(legacy, r)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	oldToNew := make(map[int64]string, len(legacy))
	for _, r := range legacy {
		newID := NewID()
		for {
			// Astronomically unlikely, but a duplicate would silently drop a
			// row's identity — regenerate rather than risk it.
			dup := false
			for _, existing := range oldToNew {
				if existing == newID {
					dup = true
					break
				}
			}
			if !dup {
				break
			}
			newID = NewID()
		}
		oldToNew[r.oldID] = newID

		var rolledBackFrom any
		if r.rolledBackFrom.Valid {
			mapped, ok := oldToNew[r.rolledBackFrom.Int64]
			if !ok {
				// rolled_back_from pointed at a row this pass hasn't (and,
				// given ascending id order, never will) map — data
				// inconsistency in the legacy table. Drop the lineage
				// pointer rather than fail the whole migration.
				rolledBackFrom = nil
			} else {
				rolledBackFrom = mapped
			}
		}

		if _, err := tx.Exec(
			`INSERT INTO deployments_new (id, app_id, seq, status, image_ref, log_path, created_by, created_at, rolled_back_from)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			newID, r.appID, r.seq, r.status, r.imageRef, r.logPath, r.createdBy, r.createdAt, rolledBackFrom,
		); err != nil {
			return fmt.Errorf("insert migrated deployment: %w", err)
		}
	}

	if _, err := tx.Exec(`DROP TABLE deployments`); err != nil {
		return fmt.Errorf("drop legacy deployments: %w", err)
	}
	if _, err := tx.Exec(`ALTER TABLE deployments_new RENAME TO deployments`); err != nil {
		return fmt.Errorf("rename deployments_new: %w", err)
	}

	return tx.Commit()
}
