package store

// RecordWebhookDelivery records a webhook delivery id and reports whether
// it is new (false = a replay of one already handled). Ids older than 72h
// are pruned on the way, so the table stays small without a sweeper.
func (s *Store) RecordWebhookDelivery(id string) (fresh bool, err error) {
	if _, err := s.db.Exec(`DELETE FROM webhook_deliveries WHERE seen_at < datetime('now', '-72 hours')`); err != nil {
		return false, err
	}
	res, err := s.db.Exec(`INSERT OR IGNORE INTO webhook_deliveries (id) VALUES (?)`, id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}
