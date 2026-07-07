package store

import (
	"database/sql"
	"errors"
	"fmt"
)

// GPUInstance tracks one rented GPU cloud VM so luncur can stop billing on
// idle and show provenance in the panel. Provider state (vast.ai's
// actual_status) is fetched live; Status here is luncur's lifecycle intent:
// renting -> active -> destroyed.
type GPUInstance struct {
	ID          int64
	Provider    string // "vastai", "nebius"
	ExternalRef string // provider's contract/instance id (vast.ai contract ints, Nebius "computeinstance-…" ids)
	Label       string
	GPUName     string
	NumGPUs     int
	Status      string // renting|active|destroyed
	CreatedAt   string
}

// CreateGPUInstance records a rent that was just accepted by the provider.
func (s *Store) CreateGPUInstance(provider string, externalRef string, label, gpuName string, numGPUs int) (GPUInstance, error) {
	return s.CreateGPUInstanceWithStatus(provider, externalRef, label, gpuName, numGPUs, "active")
}

// CreateGPUInstanceWithStatus is CreateGPUInstance with an explicit initial
// status (used for ambiguous rents recorded as "renting").
func (s *Store) CreateGPUInstanceWithStatus(provider, externalRef, label, gpuName string, numGPUs int, status string) (GPUInstance, error) {
	res, err := s.db.Exec(
		`INSERT INTO gpu_instances (provider, external_ref, label, gpu_name, num_gpus, status) VALUES (?, ?, ?, ?, ?, ?)`,
		provider, externalRef, label, gpuName, numGPUs, status,
	)
	if err != nil {
		return GPUInstance{}, fmt.Errorf("insert gpu instance: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return GPUInstance{}, err
	}
	return s.GetGPUInstance(id)
}

// GetGPUInstance fetches one row by luncur id.
func (s *Store) GetGPUInstance(id int64) (GPUInstance, error) {
	row := s.db.QueryRow(
		`SELECT id, provider, external_ref, label, gpu_name, num_gpus, status, created_at FROM gpu_instances WHERE id = ?`, id)
	return scanGPUInstance(row)
}

// ListGPUInstances returns every non-destroyed instance, newest first.
func (s *Store) ListGPUInstances() ([]GPUInstance, error) {
	rows, err := s.db.Query(
		`SELECT id, provider, external_ref, label, gpu_name, num_gpus, status, created_at FROM gpu_instances WHERE status != 'destroyed' ORDER BY id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []GPUInstance
	for rows.Next() {
		g, err := scanGPUInstance(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// MarkGPUInstanceDestroyed flips a row to destroyed after the provider
// confirmed the delete.
func (s *Store) MarkGPUInstanceDestroyed(id int64) error {
	res, err := s.db.Exec(`UPDATE gpu_instances SET status = 'destroyed' WHERE id = ?`, id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

type rowScanner interface{ Scan(dest ...any) error }

func scanGPUInstance(r rowScanner) (GPUInstance, error) {
	var g GPUInstance
	err := r.Scan(&g.ID, &g.Provider, &g.ExternalRef, &g.Label, &g.GPUName, &g.NumGPUs, &g.Status, &g.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return GPUInstance{}, ErrNotFound
	}
	if err != nil {
		return GPUInstance{}, err
	}
	return g, nil
}
