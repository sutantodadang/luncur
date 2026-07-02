package store

import (
	"fmt"
	"regexp"
)

var envKeyRe = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)

// SetEnv upserts one env var. Values arrive already sealed — the store
// never sees plaintext.
func (s *Store) SetEnv(appID int64, key string, sealed []byte) error {
	if !envKeyRe.MatchString(key) {
		return fmt.Errorf("invalid env key %q (must match [A-Z_][A-Z0-9_]*)", key)
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
