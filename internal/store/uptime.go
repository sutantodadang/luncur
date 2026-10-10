package store

import (
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

// UptimeCheck is a web app's uptime check config and live state. A missing
// row reads as enabled with defaults (DefaultUptimeCheck).
type UptimeCheck struct {
	AppID   int64 `json:"-"`
	Enabled bool  `json:"enabled"`
	// External probes the public URL (catches TLS/DNS problems) instead of
	// the in-cluster Service (no hairpin NAT needed).
	External bool `json:"external"`
	// Path overrides the probed path ("" = the app's health path, or /).
	Path       string `json:"path"`
	State      string `json:"state"` // unknown|up|down
	FailStreak int    `json:"fail_streak"`
	OKStreak   int    `json:"ok_streak"`
	LastAt     string `json:"last_at"`
	LastError  string `json:"last_error"`
	// Stored reports whether a row exists (false = defaults apply).
	Stored bool `json:"-"`
}

// DefaultUptimeCheck is the check of an app with no uptime_checks row.
func DefaultUptimeCheck(appID int64) UptimeCheck {
	return UptimeCheck{AppID: appID, Enabled: true, State: "unknown"}
}

// GetUptimeCheck returns the app's check (DefaultUptimeCheck when none).
func (s *Store) GetUptimeCheck(appID int64) (UptimeCheck, error) {
	c := UptimeCheck{AppID: appID, Stored: true}
	err := s.db.QueryRow(`SELECT enabled, external, path, state, fail_streak, ok_streak, last_at, last_error FROM uptime_checks WHERE app_id = ?`, appID).
		Scan(&c.Enabled, &c.External, &c.Path, &c.State, &c.FailStreak, &c.OKStreak, &c.LastAt, &c.LastError)
	if errors.Is(err, sql.ErrNoRows) {
		return DefaultUptimeCheck(appID), nil
	}
	return c, err
}

// SetUptimeConfig upserts the check's user-set fields.
func (s *Store) SetUptimeConfig(appID int64, enabled, external bool, path string) error {
	if path != "" && !strings.HasPrefix(path, "/") {
		return fmt.Errorf("uptime path must start with /")
	}
	_, err := s.db.Exec(`INSERT INTO uptime_checks (app_id, enabled, external, path) VALUES (?, ?, ?, ?)
		ON CONFLICT (app_id) DO UPDATE SET enabled = excluded.enabled, external = excluded.external, path = excluded.path`,
		appID, enabled, external, path)
	return err
}

// SetUptimeState records the prober's view after a probe.
func (s *Store) SetUptimeState(c UptimeCheck) error {
	_, err := s.db.Exec(`INSERT INTO uptime_checks (app_id, enabled, external, path, state, fail_streak, ok_streak, last_at, last_error)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (app_id) DO UPDATE SET state = excluded.state, fail_streak = excluded.fail_streak,
		  ok_streak = excluded.ok_streak, last_at = excluded.last_at, last_error = excluded.last_error`,
		c.AppID, c.Enabled, c.External, c.Path, c.State, c.FailStreak, c.OKStreak, c.LastAt, c.LastError)
	return err
}

// UptimeSample is one probe result.
type UptimeSample struct {
	At        time.Time
	OK        bool
	LatencyMS int
	Code      int
}

// AddUptimeSample stores one probe result and bumps the day's roll-up.
func (s *Store) AddUptimeSample(appID int64, smp UptimeSample) error {
	at := smp.At.UTC()
	okN := 0
	if smp.OK {
		okN = 1
	}
	if _, err := s.db.Exec(`INSERT INTO uptime_samples (app_id, at, ok, latency_ms, code) VALUES (?, ?, ?, ?, ?)`,
		appID, at.Format(time.RFC3339), okN, smp.LatencyMS, smp.Code); err != nil {
		return err
	}
	_, err := s.db.Exec(`INSERT INTO uptime_daily (app_id, day, ok_count, total) VALUES (?, ?, ?, 1)
		ON CONFLICT (app_id, day) DO UPDATE SET ok_count = ok_count + excluded.ok_count, total = total + 1`,
		appID, at.Format("2006-01-02"), okN)
	return err
}

// UptimeStats summarizes an app's availability.
type UptimeStats struct {
	Pct24h float64 `json:"pct_24h"`
	Pct30d float64 `json:"pct_30d"`
	Pct90d float64 `json:"pct_90d"`
	P95MS  int     `json:"p95_ms_24h"`
	// Recent are the last 24h of samples' ok flags, oldest first, bucketed
	// to at most 48 points (true = every probe in the bucket passed).
	Recent []int `json:"recent"`
}

// UptimeDay is one day's roll-up.
type UptimeDay struct {
	Day     string  `json:"day"`
	OK      int     `json:"ok"`
	Total   int     `json:"total"`
	Pct     float64 `json:"pct"`
	P95MS   int     `json:"p95_ms"`
	NoData  bool    `json:"no_data"`
	Outage  bool    `json:"outage"`
	Partial bool    `json:"partial"`
}

func pct(ok, total int) float64 {
	if total == 0 {
		return 100
	}
	return float64(int(float64(ok)*10000/float64(total))) / 100
}

// GetUptimeStats computes an app's 24h/30d/90d availability and 24h p95.
func (s *Store) GetUptimeStats(appID int64, now time.Time) (UptimeStats, error) {
	var st UptimeStats
	since := now.UTC().Add(-24 * time.Hour).Format(time.RFC3339)
	rows, err := s.db.Query(`SELECT ok, latency_ms FROM uptime_samples WHERE app_id = ? AND at >= ? ORDER BY at`, appID, since)
	if err != nil {
		return st, err
	}
	var oks []bool
	var lat []int
	for rows.Next() {
		var ok bool
		var l int
		if err := rows.Scan(&ok, &l); err != nil {
			rows.Close()
			return st, err
		}
		oks = append(oks, ok)
		if ok {
			lat = append(lat, l)
		}
	}
	rows.Close()
	okN := 0
	for _, ok := range oks {
		if ok {
			okN++
		}
	}
	st.Pct24h = pct(okN, len(oks))
	st.P95MS = p95(lat)
	st.Recent = bucket(oks, 48)
	for _, w := range []struct {
		days int
		dst  *float64
	}{{30, &st.Pct30d}, {90, &st.Pct90d}} {
		var ok, total sql.NullInt64
		if err := s.db.QueryRow(`SELECT SUM(ok_count), SUM(total) FROM uptime_daily WHERE app_id = ? AND day > ?`,
			appID, now.UTC().AddDate(0, 0, -w.days).Format("2006-01-02")).Scan(&ok, &total); err != nil {
			return st, err
		}
		*w.dst = pct(int(ok.Int64), int(total.Int64))
	}
	return st, nil
}

func p95(lat []int) int {
	if len(lat) == 0 {
		return 0
	}
	sorted := append([]int(nil), lat...)
	sort.Ints(sorted)
	// Nearest-rank: the smallest value with at least 95% at or below it.
	idx := (len(sorted)*95+99)/100 - 1
	if idx < 0 {
		idx = 0
	}
	return sorted[idx]
}

// bucket folds ok flags into at most n buckets: 1 = all passed, 0 = any
// failed.
func bucket(oks []bool, n int) []int {
	if len(oks) == 0 {
		return nil
	}
	size := (len(oks) + n - 1) / n
	var out []int
	for i := 0; i < len(oks); i += size {
		v := 1
		for j := i; j < i+size && j < len(oks); j++ {
			if !oks[j] {
				v = 0
			}
		}
		out = append(out, v)
	}
	return out
}

// UptimeDays returns the last `days` daily roll-ups oldest first, with
// empty days filled (NoData).
func (s *Store) UptimeDays(appID int64, days int, now time.Time) ([]UptimeDay, error) {
	start := now.UTC().AddDate(0, 0, -(days - 1))
	rows, err := s.db.Query(`SELECT day, ok_count, total, p95_ms FROM uptime_daily WHERE app_id = ? AND day >= ?`, appID, start.Format("2006-01-02"))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	byDay := map[string]UptimeDay{}
	for rows.Next() {
		var d UptimeDay
		if err := rows.Scan(&d.Day, &d.OK, &d.Total, &d.P95MS); err != nil {
			return nil, err
		}
		byDay[d.Day] = d
	}
	out := make([]UptimeDay, 0, days)
	for i := 0; i < days; i++ {
		day := start.AddDate(0, 0, i).Format("2006-01-02")
		d, ok := byDay[day]
		if !ok || d.Total == 0 {
			out = append(out, UptimeDay{Day: day, NoData: true, Pct: 100})
			continue
		}
		d.Pct = pct(d.OK, d.Total)
		d.Outage = d.Pct < 95
		d.Partial = !d.Outage && d.OK < d.Total
		out = append(out, d)
	}
	return out, rows.Err()
}

// RollupUptimeP95 recomputes today's p95 latency for every app from the
// raw samples, then prunes samples older than 48h and roll-ups older than
// 400 days.
func (s *Store) RollupUptimeP95(now time.Time) error {
	day := now.UTC().Format("2006-01-02")
	rows, err := s.db.Query(`SELECT app_id, latency_ms FROM uptime_samples WHERE ok = 1 AND at >= ?`, day+"T00:00:00Z")
	if err != nil {
		return err
	}
	lat := map[int64][]int{}
	for rows.Next() {
		var id int64
		var l int
		if err := rows.Scan(&id, &l); err != nil {
			rows.Close()
			return err
		}
		lat[id] = append(lat[id], l)
	}
	rows.Close()
	for id, ls := range lat {
		if _, err := s.db.Exec(`UPDATE uptime_daily SET p95_ms = ? WHERE app_id = ? AND day = ?`, p95(ls), id, day); err != nil {
			return err
		}
	}
	if _, err := s.db.Exec(`DELETE FROM uptime_samples WHERE at < ?`, now.UTC().Add(-48*time.Hour).Format(time.RFC3339)); err != nil {
		return err
	}
	_, err = s.db.Exec(`DELETE FROM uptime_daily WHERE day < ?`, now.UTC().AddDate(0, 0, -400).Format("2006-01-02"))
	return err
}

// Incident is an outage or maintenance notice.
type Incident struct {
	ID         int64            `json:"id"`
	ProjectID  int64            `json:"-"`
	AppID      int64            `json:"-"`
	App        string           `json:"app,omitempty"`
	Title      string           `json:"title"`
	Status     string           `json:"status"`
	Auto       bool             `json:"auto"`
	OpenedAt   string           `json:"opened_at"`
	ResolvedAt string           `json:"resolved_at,omitempty"`
	Updates    []IncidentUpdate `json:"updates,omitempty"`
}

// IncidentUpdate is one note on an incident.
type IncidentUpdate struct {
	Body      string `json:"body"`
	CreatedAt string `json:"created_at"`
}

// OpenIncident opens an incident (appID 0 = project-wide).
func (s *Store) OpenIncident(projectID, appID int64, title string, auto bool) (Incident, error) {
	title = strings.TrimSpace(title)
	if title == "" || len(title) > 200 {
		return Incident{}, fmt.Errorf("incident title must be 1-200 characters")
	}
	res, err := s.db.Exec(`INSERT INTO incidents (project_id, app_id, title, auto) VALUES (?, ?, ?, ?)`, projectID, appID, title, auto)
	if err != nil {
		return Incident{}, err
	}
	id, _ := res.LastInsertId()
	return s.GetIncident(id)
}

// GetIncident returns one incident with its updates.
func (s *Store) GetIncident(id int64) (Incident, error) {
	var in Incident
	err := s.db.QueryRow(`SELECT i.id, i.project_id, i.app_id, COALESCE(a.name, ''), i.title, i.status, i.auto, i.opened_at, i.resolved_at
		FROM incidents i LEFT JOIN apps a ON a.id = i.app_id WHERE i.id = ?`, id).
		Scan(&in.ID, &in.ProjectID, &in.AppID, &in.App, &in.Title, &in.Status, &in.Auto, &in.OpenedAt, &in.ResolvedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Incident{}, ErrNotFound
	}
	if err != nil {
		return Incident{}, err
	}
	rows, err := s.db.Query(`SELECT body, created_at FROM incident_updates WHERE incident_id = ? ORDER BY id DESC`, id)
	if err != nil {
		return in, err
	}
	defer rows.Close()
	for rows.Next() {
		var u IncidentUpdate
		if err := rows.Scan(&u.Body, &u.CreatedAt); err != nil {
			return in, err
		}
		in.Updates = append(in.Updates, u)
	}
	return in, rows.Err()
}

// ListIncidents returns a project's incidents, open first then newest.
func (s *Store) ListIncidents(projectID int64, limit int) ([]Incident, error) {
	rows, err := s.db.Query(`SELECT id FROM incidents WHERE project_id = ? ORDER BY status = 'resolved', id DESC LIMIT ?`, projectID, limit)
	if err != nil {
		return nil, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	out := make([]Incident, 0, len(ids))
	for _, id := range ids {
		in, err := s.GetIncident(id)
		if err != nil {
			return nil, err
		}
		out = append(out, in)
	}
	return out, nil
}

// OpenAutoIncident returns the app's open prober-opened incident.
func (s *Store) OpenAutoIncident(appID int64) (Incident, error) {
	var id int64
	err := s.db.QueryRow(`SELECT id FROM incidents WHERE app_id = ? AND auto = 1 AND status = 'open' ORDER BY id DESC LIMIT 1`, appID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return Incident{}, ErrNotFound
	}
	if err != nil {
		return Incident{}, err
	}
	return s.GetIncident(id)
}

// AddIncidentUpdate appends a note.
func (s *Store) AddIncidentUpdate(id int64, body string) error {
	body = strings.TrimSpace(body)
	if body == "" || len(body) > 2000 {
		return fmt.Errorf("update must be 1-2000 characters")
	}
	_, err := s.db.Exec(`INSERT INTO incident_updates (incident_id, body) VALUES (?, ?)`, id, body)
	return err
}

// ResolveIncident marks an incident resolved.
func (s *Store) ResolveIncident(id int64) error {
	res, err := s.db.Exec(`UPDATE incidents SET status = 'resolved', resolved_at = datetime('now') WHERE id = ? AND status = 'open'`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		if _, err := s.GetIncident(id); err != nil {
			return err
		}
		return fmt.Errorf("incident %d is already resolved", id)
	}
	return nil
}

// StatusPage is a project's public status page config.
type StatusPage struct {
	ProjectID int64    `json:"-"`
	Slug      string   `json:"slug"`
	Title     string   `json:"title"`
	Apps      []string `json:"apps"`
	Enabled   bool     `json:"enabled"`
}

var slugRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,38}[a-z0-9]$`)

// ValidSlug reports whether s is a usable status page slug.
func ValidSlug(s string) bool { return slugRe.MatchString(s) }

func scanStatusPage(sc rowScanner) (StatusPage, error) {
	var sp StatusPage
	var apps string
	err := sc.Scan(&sp.ProjectID, &sp.Slug, &sp.Title, &apps, &sp.Enabled)
	if errors.Is(err, sql.ErrNoRows) {
		return StatusPage{}, ErrNotFound
	}
	if apps != "" {
		sp.Apps = strings.Split(apps, ",")
	}
	return sp, err
}

// GetStatusPage returns the project's status page.
func (s *Store) GetStatusPage(projectID int64) (StatusPage, error) {
	return scanStatusPage(s.db.QueryRow(`SELECT project_id, slug, title, apps, enabled FROM status_pages WHERE project_id = ?`, projectID))
}

// GetStatusPageBySlug returns the status page published at slug.
func (s *Store) GetStatusPageBySlug(slug string) (StatusPage, error) {
	return scanStatusPage(s.db.QueryRow(`SELECT project_id, slug, title, apps, enabled FROM status_pages WHERE slug = ?`, slug))
}

// ErrSlugTaken is returned when another project already uses a slug.
var ErrSlugTaken = errors.New("that status page slug is taken")

// SetStatusPage upserts the project's status page.
func (s *Store) SetStatusPage(sp StatusPage) error {
	if !ValidSlug(sp.Slug) {
		return fmt.Errorf("slug must be 3-40 lowercase letters, digits or dashes")
	}
	if len(sp.Title) > 80 {
		return fmt.Errorf("title must be at most 80 characters")
	}
	if other, err := s.GetStatusPageBySlug(sp.Slug); err == nil && other.ProjectID != sp.ProjectID {
		return ErrSlugTaken
	}
	_, err := s.db.Exec(`INSERT INTO status_pages (project_id, slug, title, apps, enabled) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (project_id) DO UPDATE SET slug = excluded.slug, title = excluded.title, apps = excluded.apps, enabled = excluded.enabled`,
		sp.ProjectID, sp.Slug, sp.Title, strings.Join(sp.Apps, ","), sp.Enabled)
	return err
}

// DeleteStatusPage unpublishes the project's status page.
func (s *Store) DeleteStatusPage(projectID int64) error {
	_, err := s.db.Exec(`DELETE FROM status_pages WHERE project_id = ?`, projectID)
	return err
}
