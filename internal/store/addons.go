package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// Addon is a project-level Postgres/Redis instance; credentials are sealed
// (same pattern as env_vars) and materialized into a K8s Secret at
// provision time.
type Addon struct {
	ID        int64
	ProjectID int64
	Type      string
	Name      string
	Version   string
	SizeGB    int
	CredsEnc  []byte
	CreatedAt string
}

func (s *Store) CreateAddon(projectID int64, typ, name, version string, sizeGB int, credsEnc []byte) (Addon, error) {
	if typ != "postgres" && typ != "redis" {
		return Addon{}, fmt.Errorf("unsupported addon type %q (postgres|redis)", typ)
	}
	if !validName(name) {
		return Addon{}, fmt.Errorf("invalid addon name %q (lowercase letters, digits, dashes; max 40 chars)", name)
	}
	if sizeGB < 1 {
		sizeGB = 1
	}
	res, err := s.db.Exec(
		`INSERT INTO addons (project_id, type, name, version, size_gb, creds_enc)
		 VALUES (?, ?, ?, ?, ?, ?)`, projectID, typ, name, version, sizeGB, credsEnc)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return Addon{}, fmt.Errorf("addon %q already exists in this project", name)
		}
		return Addon{}, err
	}
	id, _ := res.LastInsertId()
	return s.getAddonByID(id)
}

const addonCols = `id, project_id, type, name, version, size_gb, creds_enc, created_at`

func (s *Store) scanAddon(row *sql.Row) (Addon, error) {
	var a Addon
	err := row.Scan(&a.ID, &a.ProjectID, &a.Type, &a.Name, &a.Version, &a.SizeGB, &a.CredsEnc, &a.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Addon{}, ErrNotFound
	}
	return a, err
}

func (s *Store) getAddonByID(id int64) (Addon, error) {
	return s.scanAddon(s.db.QueryRow(`SELECT `+addonCols+` FROM addons WHERE id = ?`, id))
}

func (s *Store) GetAddon(projectID int64, name string) (Addon, error) {
	return s.scanAddon(s.db.QueryRow(
		`SELECT `+addonCols+` FROM addons WHERE project_id = ? AND name = ?`, projectID, name))
}

func (s *Store) listAddons(query string, args ...any) ([]Addon, error) {
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Addon
	for rows.Next() {
		var a Addon
		if err := rows.Scan(&a.ID, &a.ProjectID, &a.Type, &a.Name, &a.Version, &a.SizeGB, &a.CredsEnc, &a.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Store) ListAddons(projectID int64) ([]Addon, error) {
	return s.listAddons(`SELECT `+addonCols+` FROM addons WHERE project_id = ? ORDER BY id`, projectID)
}

// AllAddons returns every addon across all projects, ordered by id.
func (s *Store) AllAddons() ([]Addon, error) {
	return s.listAddons(`SELECT ` + addonCols + ` FROM addons ORDER BY id`)
}

func (s *Store) AddonsForApp(appID int64) ([]Addon, error) {
	return s.listAddons(
		`SELECT a.id, a.project_id, a.type, a.name, a.version, a.size_gb, a.creds_enc, a.created_at
		 FROM addons a JOIN addon_attachments t ON t.addon_id = a.id
		 WHERE t.app_id = ? ORDER BY a.id`, appID)
}

// SetAddonVersion updates an addon's recorded version. The caller is
// responsible for re-rendering and applying the addon's manifests.
func (s *Store) SetAddonVersion(id int64, version string) error {
	res, err := s.db.Exec(`UPDATE addons SET version = ? WHERE id = ?`, version, id)
	if err != nil {
		return err
	}
	if affected, _ := res.RowsAffected(); affected == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) DeleteAddon(id int64) error {
	res, err := s.db.Exec(`DELETE FROM addons WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) AttachAddon(addonID, appID int64) error {
	_, err := s.db.Exec(
		`INSERT INTO addon_attachments (addon_id, app_id) VALUES (?, ?)`, addonID, appID)
	if err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed") {
		return fmt.Errorf("addon is already attached to this app")
	}
	return err
}

func (s *Store) DetachAddon(addonID, appID int64) error {
	res, err := s.db.Exec(
		`DELETE FROM addon_attachments WHERE addon_id = ? AND app_id = ?`, addonID, appID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// AppsForAddon lists the apps an addon is attached to.
func (s *Store) AppsForAddon(addonID int64) ([]App, error) {
	rows, err := s.db.Query(
		`SELECT a.id FROM apps a JOIN addon_attachments t ON t.app_id = a.id
		 WHERE t.addon_id = ? ORDER BY a.id`, addonID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]App, 0, len(ids))
	for _, id := range ids {
		a, err := s.GetAppByID(id)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, nil
}
