package store

import (
	"database/sql"
	"errors"
	"fmt"
)

// ProjectS3 is a project's external S3 configuration; keys are sealed with
// the same sealer as env vars.
type ProjectS3 struct {
	ProjectID    int64
	Endpoint     string
	Region       string
	Bucket       string
	AccessKeyEnc []byte
	SecretKeyEnc []byte
}

// SetProjectS3 upserts a project's external S3 configuration.
func (s *Store) SetProjectS3(c ProjectS3) error {
	if c.Endpoint == "" || c.Bucket == "" {
		return fmt.Errorf("endpoint and bucket are required")
	}
	if len(c.AccessKeyEnc) == 0 || len(c.SecretKeyEnc) == 0 {
		return fmt.Errorf("access key and secret key are required")
	}
	_, err := s.db.Exec(`
INSERT INTO project_s3 (project_id, endpoint, region, bucket, access_key_enc, secret_key_enc)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT(project_id) DO UPDATE SET
  endpoint = excluded.endpoint, region = excluded.region, bucket = excluded.bucket,
  access_key_enc = excluded.access_key_enc, secret_key_enc = excluded.secret_key_enc`,
		c.ProjectID, c.Endpoint, c.Region, c.Bucket, c.AccessKeyEnc, c.SecretKeyEnc)
	return err
}

func (s *Store) GetProjectS3(projectID int64) (ProjectS3, error) {
	var c ProjectS3
	err := s.db.QueryRow(
		`SELECT project_id, endpoint, region, bucket, access_key_enc, secret_key_enc
		 FROM project_s3 WHERE project_id = ?`, projectID,
	).Scan(&c.ProjectID, &c.Endpoint, &c.Region, &c.Bucket, &c.AccessKeyEnc, &c.SecretKeyEnc)
	if errors.Is(err, sql.ErrNoRows) {
		return ProjectS3{}, ErrNotFound
	}
	return c, err
}

func (s *Store) DeleteProjectS3(projectID int64) error {
	res, err := s.db.Exec(`DELETE FROM project_s3 WHERE project_id = ?`, projectID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}
