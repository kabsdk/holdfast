package store

import (
	"context"
	"time"
)

// NotifyRow is a problem the server has seen, for its own alerts (plan-m10 item 2).
type NotifyRow struct {
	Key       string
	Severity  string
	Message   string
	FirstSeen time.Time
	Notified  bool
}

// NotifyState returns every problem the server has seen and that has not ended.
func (q *Q) NotifyState(ctx context.Context) (map[string]NotifyRow, error) {
	rows, err := q.db.Query(ctx, `SELECT key, severity, message, first_seen, notified FROM notify_state`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]NotifyRow{}
	for rows.Next() {
		var r NotifyRow
		if err := rows.Scan(&r.Key, &r.Severity, &r.Message, &r.FirstSeen, &r.Notified); err != nil {
			return nil, err
		}
		out[r.Key] = r
	}
	return out, rows.Err()
}

// SeeProblem records a problem the first time it is seen, and keeps its message current.
func (q *Q) SeeProblem(ctx context.Context, r NotifyRow) error {
	_, err := q.db.Exec(ctx, `INSERT INTO notify_state (key, severity, message, first_seen) VALUES ($1, $2, $3, $4)
		ON CONFLICT (key) DO UPDATE SET severity = $2, message = $3`, r.Key, r.Severity, r.Message, r.FirstSeen)
	return err
}

// MarkNotified records that the problems' start was sent.
func (q *Q) MarkNotified(ctx context.Context, keys []string) error {
	_, err := q.db.Exec(ctx, `UPDATE notify_state SET notified = true WHERE key = ANY($1)`, keys)
	return err
}

// ForgetProblems drops problems that ended.
func (q *Q) ForgetProblems(ctx context.Context, keys []string) error {
	_, err := q.db.Exec(ctx, `DELETE FROM notify_state WHERE key = ANY($1)`, keys)
	return err
}
