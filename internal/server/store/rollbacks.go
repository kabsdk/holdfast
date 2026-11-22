package store

import (
	"context"
	"time"
)

// Rollback is one repository rollback (decision 038): asked for, run in the background, and its outcome.
type Rollback struct {
	ID                int64      `json:"id"`
	FQDN              string     `json:"fqdn"`
	Actor             string     `json:"actor"`
	To                time.Time  `json:"to"`
	RequestedAt       time.Time  `json:"requested_at"`
	FinishedAt        *time.Time `json:"finished_at,omitempty"`
	Status            string     `json:"status"` // running, success or failed
	AgentDown         bool       `json:"agent_down"`
	Message           string     `json:"message,omitempty"`
	BroughtBack       int        `json:"brought_back"`
	SetAside          int        `json:"set_aside"`
	SnapshotsBack     []string   `json:"snapshots_back"`
	SnapshotsSetAside []string   `json:"snapshots_set_aside"`
	CheckPassed       *bool      `json:"check_passed,omitempty"`
	Error             string     `json:"error,omitempty"`
}

// StartRollback records a rollback that is starting, and returns its ID.
func (q *Q) StartRollback(ctx context.Context, r Rollback) (int64, error) {
	var id int64
	err := q.db.QueryRow(ctx, `INSERT INTO rollbacks (fqdn, actor, to_time, requested_at, agent_down, message) VALUES ($1, $2, $3, $4, $5, $6) RETURNING id`,
		r.FQDN, r.Actor, r.To, r.RequestedAt, r.AgentDown, r.Message).Scan(&id)
	return id, err
}

// FinishRollback records how a rollback went.
func (q *Q) FinishRollback(ctx context.Context, r Rollback) error {
	_, err := q.db.Exec(ctx, `UPDATE rollbacks SET finished_at = $2, status = $3, brought_back = $4, set_aside = $5, snapshots_back = $6,
		snapshots_set_aside = $7, check_passed = $8, error = $9 WHERE id = $1`,
		r.ID, r.FinishedAt, r.Status, r.BroughtBack, r.SetAside, nonNil(r.SnapshotsBack), nonNil(r.SnapshotsSetAside), r.CheckPassed, r.Error)
	return err
}

// ListRollbacks returns a host's rollbacks, newest first.
func (q *Q) ListRollbacks(ctx context.Context, fqdn string, limit int) ([]Rollback, error) {
	rows, err := q.db.Query(ctx, `SELECT id, fqdn, actor, to_time, requested_at, finished_at, status, agent_down, message, brought_back, set_aside,
		snapshots_back, snapshots_set_aside, check_passed, error FROM rollbacks WHERE fqdn = $1 ORDER BY requested_at DESC, id DESC LIMIT $2`, fqdn, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Rollback{}
	for rows.Next() {
		var r Rollback
		if err := rows.Scan(&r.ID, &r.FQDN, &r.Actor, &r.To, &r.RequestedAt, &r.FinishedAt, &r.Status, &r.AgentDown, &r.Message, &r.BroughtBack,
			&r.SetAside, &r.SnapshotsBack, &r.SnapshotsSetAside, &r.CheckPassed, &r.Error); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// RunningRollbacks returns the hosts with a rollback under way.
func (q *Q) RunningRollbacks(ctx context.Context) (map[string]bool, error) {
	rows, err := q.db.Query(ctx, `SELECT DISTINCT fqdn FROM rollbacks WHERE status = 'running'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var f string
		if err := rows.Scan(&f); err != nil {
			return nil, err
		}
		out[f] = true
	}
	return out, rows.Err()
}

// AbandonRollbacks marks rollbacks left running by a server that stopped as failed: a restart does not resume
// one. It runs at start.
func (q *Q) AbandonRollbacks(ctx context.Context, now time.Time) error {
	_, err := q.db.Exec(ctx, `UPDATE rollbacks SET status = 'failed', finished_at = $1,
		error = 'the server stopped while it ran; look at the preview again, and roll back again if anything is left' WHERE status = 'running'`, now)
	return err
}
