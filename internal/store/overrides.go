package store

import (
	"encoding/json"
	"fmt"
)

var overridableKinds = map[string]bool{"Deployment": true, "Service": true, "Ingress": true}

func (s *Store) SetOverride(appID int64, kind, patchJSON string) error {
	if !overridableKinds[kind] {
		return fmt.Errorf("unsupported kind %q (Deployment, Service, or Ingress)", kind)
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(patchJSON), &obj); err != nil {
		return fmt.Errorf("override patch must be a JSON object: %w", err)
	}
	_, err := s.db.Exec(
		`INSERT INTO overrides (app_id, kind, patch_json) VALUES (?, ?, ?)
		 ON CONFLICT (app_id, kind) DO UPDATE
		 SET patch_json = excluded.patch_json, updated_at = datetime('now')`,
		appID, kind, patchJSON,
	)
	return err
}

func (s *Store) Overrides(appID int64) (map[string]string, error) {
	rows, err := s.db.Query(`SELECT kind, patch_json FROM overrides WHERE app_id = ?`, appID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var k, p string
		if err := rows.Scan(&k, &p); err != nil {
			return nil, err
		}
		out[k] = p
	}
	return out, rows.Err()
}

func (s *Store) DeleteOverride(appID int64, kind string) error {
	res, err := s.db.Exec(`DELETE FROM overrides WHERE app_id = ? AND kind = ?`, appID, kind)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}
