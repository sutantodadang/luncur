package store

import (
	"regexp"
)

var envKeyRe = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)

// SetEnv upserts one env var. Values arrive already sealed — the store
// never sees plaintext.
func (s *Store) SetEnv(appID int64, key string, sealed []byte) error {
	if !envKeyRe.MatchString(key) {
		return validationErrorf("invalid env key %q (must match [A-Z_][A-Z0-9_]*)", key)
	}
	_, err := s.db.Exec(
		`INSERT INTO env_vars (app_id, key, value_enc) VALUES (?, ?, ?)
		 ON CONFLICT (app_id, key) DO UPDATE SET value_enc = excluded.value_enc`,
		appID, key, sealed,
	)
	return err
}

func (s *Store) UnsetEnv(appID int64, key string) error {
	res, err := s.db.Exec(`DELETE FROM env_vars WHERE app_id = ? AND key = ?`, appID, key)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ReplaceEnv swaps an app's entire env-var set for vars in one transaction:
// every existing row is deleted, then each entry inserted. Values are
// sealed bytes — same contract as SetEnv. Keys are validated up front so an
// invalid key rejects the whole call without touching the existing set.
func (s *Store) ReplaceEnv(appID int64, vars map[string][]byte) error {
	for k := range vars {
		if !envKeyRe.MatchString(k) {
			return validationErrorf("invalid env key %q (must match [A-Z_][A-Z0-9_]*)", k)
		}
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after a successful Commit
	if _, err := tx.Exec(`DELETE FROM env_vars WHERE app_id = ?`, appID); err != nil {
		return err
	}
	for k, v := range vars {
		if _, err := tx.Exec(
			`INSERT INTO env_vars (app_id, key, value_enc) VALUES (?, ?, ?)`,
			appID, k, v,
		); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) ListEnv(appID int64) (map[string][]byte, error) {
	rows, err := s.db.Query(`SELECT key, value_enc FROM env_vars WHERE app_id = ?`, appID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]byte{}
	for rows.Next() {
		var k string
		var v []byte
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, rows.Err()
}
