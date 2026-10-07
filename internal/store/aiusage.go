package store

import "time"

// AIUsageRow is one (day, user, workflow) usage aggregate.
type AIUsageRow struct {
	Day          string `json:"day"`
	UserEmail    string `json:"user,omitempty"`
	Workflow     string `json:"workflow"`
	InputTokens  int64  `json:"input_tokens"`
	OutputTokens int64  `json:"output_tokens"`
	Requests     int64  `json:"requests"`
}

func aiDay(t time.Time) string { return t.UTC().Format("2006-01-02") }

// AddAIUsage records one assistant request's token usage under today's UTC
// day. userID 0 means a system-initiated request (notification summaries).
func (s *Store) AddAIUsage(userID int64, workflow string, inputTokens, outputTokens int64) error {
	_, err := s.db.Exec(
		`INSERT INTO ai_usage (day, user_id, workflow, input_tokens, output_tokens, requests)
		 VALUES (?, ?, ?, ?, ?, 1)
		 ON CONFLICT (day, user_id, workflow) DO UPDATE SET
		   input_tokens = input_tokens + excluded.input_tokens,
		   output_tokens = output_tokens + excluded.output_tokens,
		   requests = requests + 1`,
		aiDay(time.Now()), userID, workflow, inputTokens, outputTokens)
	return err
}

// AITokensToday is the install-wide input+output token total for today (UTC).
func (s *Store) AITokensToday() (int64, error) {
	var n int64
	err := s.db.QueryRow(
		`SELECT COALESCE(SUM(input_tokens + output_tokens), 0) FROM ai_usage WHERE day = ?`,
		aiDay(time.Now())).Scan(&n)
	return n, err
}

// AIUsage returns per-day/user/workflow rows for the last `days` days
// (today included), newest first.
func (s *Store) AIUsage(days int) ([]AIUsageRow, error) {
	if days < 1 {
		days = 1
	}
	since := aiDay(time.Now().AddDate(0, 0, -(days - 1)))
	rows, err := s.db.Query(
		`SELECT u.day, COALESCE(us.email, ''), u.workflow, u.input_tokens, u.output_tokens, u.requests
		 FROM ai_usage u LEFT JOIN users us ON us.id = u.user_id
		 WHERE u.day >= ? ORDER BY u.day DESC, u.workflow, us.email`, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AIUsageRow
	for rows.Next() {
		var r AIUsageRow
		if err := rows.Scan(&r.Day, &r.UserEmail, &r.Workflow, &r.InputTokens, &r.OutputTokens, &r.Requests); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
