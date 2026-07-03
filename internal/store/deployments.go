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
	CreatedAt string
}

func (s *Store) CreateDeployment(appID int64, status, imageRef string) (Deployment, error) {
	res, err := s.db.Exec(
		`INSERT INTO deployments (app_id, status, image_ref) VALUES (?, ?, ?)`,
		appID, status, imageRef,
	)
	if err != nil {
		return Deployment{}, fmt.Errorf("insert deployment: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Deployment{}, err
	}
	return Deployment{ID: id, AppID: appID, Status: status, ImageRef: imageRef}, nil
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

func (s *Store) LatestDeployment(appID int64) (Deployment, error) {
	var d Deployment
	err := s.db.QueryRow(
		`SELECT id, app_id, status, image_ref, created_at FROM deployments
		 WHERE app_id = ? ORDER BY id DESC LIMIT 1`, appID,
	).Scan(&d.ID, &d.AppID, &d.Status, &d.ImageRef, &d.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Deployment{}, ErrNotFound
	}
	return d, err
}
