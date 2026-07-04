package store

import "fmt"

// AuditEntry is one recorded mutating request: who did what, to what, when.
type AuditEntry struct {
	ID        int64
	CreatedAt string
	UserEmail string
	Action    string
	Target    string
}

// AppendAudit records one audit-log row. Called after a mutating request
// succeeds; the caller only logs failures here, never surfaces them to the
// client — an audit-log write must not turn a successful request into one.
func (s *Store) AppendAudit(email, action, target string) error {
	_, err := s.db.Exec(
		`INSERT INTO audit_log (user_email, action, target) VALUES (?, ?, ?)`,
		email, action, target,
	)
	if err != nil {
		return fmt.Errorf("insert audit log: %w", err)
	}
	return nil
}

// ListAudit returns audit rows newest-first, optionally filtered by an exact
// user_email match (user) and/or a substring match against "action target"
// (contains). limit <= 0 or > 200 is capped to 200.
func (s *Store) ListAudit(limit, offset int, user, contains string) ([]AuditEntry, error) {
	if limit <= 0 || limit > 200 {
		limit = 200
	}
	query := `SELECT id, created_at, user_email, action, target FROM audit_log WHERE 1=1`
	var args []any
	if user != "" {
		query += ` AND user_email = ?`
		args = append(args, user)
	}
	if contains != "" {
		query += ` AND (action || ' ' || target) LIKE ?`
		args = append(args, "%"+contains+"%")
	}
	query += ` ORDER BY id DESC LIMIT ? OFFSET ?`
	args = append(args, limit, offset)

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AuditEntry
	for rows.Next() {
		var e AuditEntry
		if err := rows.Scan(&e.ID, &e.CreatedAt, &e.UserEmail, &e.Action, &e.Target); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// PruneAudit deletes audit rows older than keepDays. keepDays <= 0 is a
// no-op — callers use it to mean "keep forever". Returns the number of rows
// deleted.
func (s *Store) PruneAudit(keepDays int) (int64, error) {
	if keepDays <= 0 {
		return 0, nil
	}
	res, err := s.db.Exec(
		`DELETE FROM audit_log WHERE created_at < datetime('now', ?)`,
		fmt.Sprintf("-%d days", keepDays),
	)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
