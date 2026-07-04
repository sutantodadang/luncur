package store

import (
	"fmt"
	"regexp"
	"strings"
)

// Domain is a custom hostname attached to an app, with TLS cert state.
type Domain struct {
	ID            int64
	AppID         int64
	Hostname      string
	CertStatus    string // none|pending|issued|failed|external
	CertError     string
	CertExpiresAt string // RFC3339, empty until issued
}

var hostnameRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$`)

func (s *Store) AddDomain(appID int64, hostname string) (Domain, error) {
	h := strings.ToLower(strings.TrimSpace(hostname))
	// One leading "*." makes a wildcard; the remainder must be a normal
	// hostname either way. Policy (wildcards need a dns provider) is the
	// server's job — here it's just an ordinary hostname value.
	base, _ := strings.CutPrefix(h, "*.")
	if strings.Contains(base, "*") || !hostnameRe.MatchString(base) {
		return Domain{}, fmt.Errorf("invalid hostname %q", hostname)
	}
	res, err := s.db.Exec(
		`INSERT INTO domains (app_id, hostname, cert_status) VALUES (?, ?, 'none')`, appID, h)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return Domain{}, fmt.Errorf("hostname %q is already registered", h)
		}
		return Domain{}, err
	}
	id, _ := res.LastInsertId()
	return Domain{ID: id, AppID: appID, Hostname: h, CertStatus: "none"}, nil
}

func (s *Store) scanDomains(query string, args ...any) ([]Domain, error) {
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Domain
	for rows.Next() {
		var d Domain
		if err := rows.Scan(&d.ID, &d.AppID, &d.Hostname, &d.CertStatus, &d.CertError, &d.CertExpiresAt); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

const domainCols = `id, app_id, hostname, cert_status, cert_error, cert_expires_at`

func (s *Store) ListDomains(appID int64) ([]Domain, error) {
	return s.scanDomains(`SELECT `+domainCols+` FROM domains WHERE app_id = ? ORDER BY id`, appID)
}

func (s *Store) AllDomains() ([]Domain, error) {
	return s.scanDomains(`SELECT ` + domainCols + ` FROM domains ORDER BY id`)
}

func (s *Store) DeleteDomain(appID int64, hostname string) error {
	res, err := s.db.Exec(`DELETE FROM domains WHERE app_id = ? AND hostname = ?`,
		appID, strings.ToLower(hostname))
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) SetDomainCert(id int64, status, certErr, expiresAt string) error {
	_, err := s.db.Exec(
		`UPDATE domains SET cert_status = ?, cert_error = ?, cert_expires_at = ? WHERE id = ?`,
		status, certErr, expiresAt, id)
	return err
}
