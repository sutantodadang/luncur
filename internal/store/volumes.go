package store

import (
	"fmt"
	"path"
	"strings"
)

// Volume is a per-app persistent volume: a single RWO PersistentVolumeClaim
// mounted into the app's Deployment at Path. Only web/worker apps support
// volumes — the render/server layers enforce that gate; this file only
// enforces the shape of a single row.
type Volume struct {
	ID        int64
	AppID     int64
	Name      string
	Path      string
	SizeGB    int
	CreatedAt string
}

// validVolumePath enforces an absolute, printable path within a sane length
// so it can become a container mountPath unmodified.
func validVolumePath(p string) error {
	if !strings.HasPrefix(p, "/") {
		return validationErrorf("volume path must be absolute (start with '/'), got %q", p)
	}
	if len(p) > 256 {
		return validationErrorf("volume path must be at most 256 characters, got %d", len(p))
	}
	for _, r := range p {
		if r <= ' ' || r == 0x7f {
			return validationErrorf("volume path must not contain whitespace or control characters")
		}
	}
	return nil
}

// AddVolume registers a new volume for appID. An empty name defaults to the
// last path segment of volPath. Returns a *ValidationError for bad input or
// a name/path collision with an existing volume on the same app.
func (s *Store) AddVolume(appID int64, name, volPath string, sizeGB int) (Volume, error) {
	if err := validVolumePath(volPath); err != nil {
		return Volume{}, err
	}
	if name == "" {
		name = path.Base(volPath)
	}
	if !validName(name) {
		return Volume{}, validationErrorf("invalid volume name %q (lowercase letters, digits, dashes; max 40 chars)", name)
	}
	if sizeGB < 1 || sizeGB > 1000 {
		return Volume{}, validationErrorf("volume size must be 1-1000 GB, got %d", sizeGB)
	}

	existing, err := s.ListVolumes(appID)
	if err != nil {
		return Volume{}, fmt.Errorf("list volumes: %w", err)
	}
	for _, v := range existing {
		if v.Name == name {
			return Volume{}, validationErrorf("volume name %q is already in use", name)
		}
		if v.Path == volPath {
			return Volume{}, validationErrorf("volume path %q is already in use", volPath)
		}
	}

	res, err := s.db.Exec(
		`INSERT INTO volumes (app_id, name, path, size_gb) VALUES (?, ?, ?, ?)`,
		appID, name, volPath, sizeGB,
	)
	if err != nil {
		return Volume{}, fmt.Errorf("insert volume: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Volume{}, err
	}
	return Volume{ID: id, AppID: appID, Name: name, Path: volPath, SizeGB: sizeGB}, nil
}

// ListVolumes returns appID's volumes ordered by name.
func (s *Store) ListVolumes(appID int64) ([]Volume, error) {
	rows, err := s.db.Query(
		`SELECT id, app_id, name, path, size_gb, created_at FROM volumes WHERE app_id = ? ORDER BY name`,
		appID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Volume
	for rows.Next() {
		var v Volume
		if err := rows.Scan(&v.ID, &v.AppID, &v.Name, &v.Path, &v.SizeGB, &v.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// DeleteVolume removes appID's volume named name. ErrNotFound when absent.
func (s *Store) DeleteVolume(appID int64, name string) error {
	res, err := s.db.Exec(`DELETE FROM volumes WHERE app_id = ? AND name = ?`, appID, name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}
