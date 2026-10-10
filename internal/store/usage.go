package store

import (
	"database/sql"
	"time"
)

// UsageHour is one app's per-pod resource usage aggregate for an hour.
type UsageHour struct {
	Hour        string
	CPUP95Milli int64
	CPUMaxMilli int64
	MemMaxMB    int64
	Samples     int
}

// UpsertUsageHour stores (or replaces) an app's aggregate for an hour and
// prunes aggregates older than 30 days.
func (s *Store) UpsertUsageHour(appID int64, u UsageHour) error {
	if _, err := s.db.Exec(`INSERT INTO app_usage_hourly (app_id, hour, cpu_p95_milli, cpu_max_milli, mem_max_mb, samples)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (app_id, hour) DO UPDATE SET cpu_p95_milli = excluded.cpu_p95_milli, cpu_max_milli = excluded.cpu_max_milli,
		  mem_max_mb = excluded.mem_max_mb, samples = excluded.samples`,
		appID, u.Hour, u.CPUP95Milli, u.CPUMaxMilli, u.MemMaxMB, u.Samples); err != nil {
		return err
	}
	_, err := s.db.Exec(`DELETE FROM app_usage_hourly WHERE hour < ?`, time.Now().UTC().AddDate(0, 0, -30).Format("2006-01-02T15"))
	return err
}

// UsageSummary is an app's usage over a window: the highest hourly p95
// CPU and the peak memory, per pod.
type UsageSummary struct {
	Hours       int   `json:"hours"`
	CPUP95Milli int64 `json:"cpu_p95_millicores"`
	CPUMaxMilli int64 `json:"cpu_max_millicores"`
	MemMaxMB    int64 `json:"memory_max_mib"`
}

// AppUsageSummary summarizes an app's hourly aggregates since `since`.
func (s *Store) AppUsageSummary(appID int64, since time.Time) (UsageSummary, error) {
	var u UsageSummary
	var cpu, cmax, mem sql.NullInt64
	err := s.db.QueryRow(`SELECT count(*), MAX(cpu_p95_milli), MAX(cpu_max_milli), MAX(mem_max_mb) FROM app_usage_hourly WHERE app_id = ? AND hour >= ?`,
		appID, since.UTC().Format("2006-01-02T15")).Scan(&u.Hours, &cpu, &cmax, &mem)
	u.CPUP95Milli, u.CPUMaxMilli, u.MemMaxMB = cpu.Int64, cmax.Int64, mem.Int64
	return u, err
}
