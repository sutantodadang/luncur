package store

// Backup is a completed backup archive: SQLite snapshot + sealer key +
// addon dumps, tarred and optionally uploaded to S3.
type Backup struct {
	ID        int64
	Path      string
	SizeBytes int64
	Uploaded  bool
	CreatedAt string
}

const backupCols = `id, path, size_bytes, uploaded, created_at`

// CreateBackup records a completed backup archive.
func (s *Store) CreateBackup(path string, size int64, uploaded bool) (Backup, error) {
	res, err := s.db.Exec(
		`INSERT INTO backups (path, size_bytes, uploaded) VALUES (?, ?, ?)`,
		path, size, uploaded)
	if err != nil {
		return Backup{}, err
	}
	id, _ := res.LastInsertId()
	var b Backup
	err = s.db.QueryRow(`SELECT `+backupCols+` FROM backups WHERE id = ?`, id).
		Scan(&b.ID, &b.Path, &b.SizeBytes, &b.Uploaded, &b.CreatedAt)
	return b, err
}

// ListBackups returns all backups, newest first.
func (s *Store) ListBackups() ([]Backup, error) {
	rows, err := s.db.Query(`SELECT ` + backupCols + ` FROM backups ORDER BY id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Backup
	for rows.Next() {
		var b Backup
		if err := rows.Scan(&b.ID, &b.Path, &b.SizeBytes, &b.Uploaded, &b.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// DeleteBackup removes a backup row (the caller is responsible for the
// local file and any remote object).
func (s *Store) DeleteBackup(id int64) error {
	res, err := s.db.Exec(`DELETE FROM backups WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}
