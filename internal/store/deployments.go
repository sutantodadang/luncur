package store

import (
	"database/sql"
	"errors"
	"fmt"
)

type Deployment struct {
	ID        int64
	AppID     int64
	Status    string
	ImageRef  string
	LogPath   string
	CreatedBy sql.NullInt64
	CreatedAt string
}

// CreateDeployment inserts a deployment row. createdBy of 0 is stored as
// NULL (unattributed).
func (s *Store) CreateDeployment(appID int64, status, imageRef string, createdBy int64) (Deployment, error) {
	var by any
	if createdBy != 0 {
		by = createdBy
	}
	res, err := s.db.Exec(
		`INSERT INTO deployments (app_id, status, image_ref, created_by) VALUES (?, ?, ?, ?)`,
		appID, status, imageRef, by,
	)
	if err != nil {
		return Deployment{}, fmt.Errorf("insert deployment: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Deployment{}, err
	}
	return Deployment{ID: id, AppID: appID, Status: status, ImageRef: imageRef,
		CreatedBy: sql.NullInt64{Int64: createdBy, Valid: createdBy != 0}}, nil
}

func (s *Store) SetDeploymentStatus(id int64, status string) error {
	res, err := s.db.Exec(`UPDATE deployments SET status = ? WHERE id = ?`, status, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) SetDeploymentImage(id int64, imageRef string) error {
	res, err := s.db.Exec(`UPDATE deployments SET image_ref = ? WHERE id = ?`, imageRef, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) SetDeploymentLog(id int64, logPath string) error {
	res, err := s.db.Exec(`UPDATE deployments SET log_path = ? WHERE id = ?`, logPath, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) GetDeployment(id int64) (Deployment, error) {
	var d Deployment
	var img, logp sql.NullString
	err := s.db.QueryRow(
		`SELECT id, app_id, status, image_ref, log_path, created_by, created_at
		 FROM deployments WHERE id = ?`, id,
	).Scan(&d.ID, &d.AppID, &d.Status, &img, &logp, &d.CreatedBy, &d.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Deployment{}, ErrNotFound
	}
	d.ImageRef, d.LogPath = img.String, logp.String
	return d, err
}

func (s *Store) LatestDeployment(appID int64) (Deployment, error) {
	var d Deployment
	var img, logp sql.NullString
	err := s.db.QueryRow(
		`SELECT id, app_id, status, image_ref, log_path, created_by, created_at FROM deployments
		 WHERE app_id = ? ORDER BY id DESC LIMIT 1`, appID,
	).Scan(&d.ID, &d.AppID, &d.Status, &img, &logp, &d.CreatedBy, &d.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Deployment{}, ErrNotFound
	}
	d.ImageRef, d.LogPath = img.String, logp.String
	return d, err
}
