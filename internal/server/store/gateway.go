package store

import (
	"context"
	"time"
)

// Grant lets reader read repo's repository through the gateway, for one restore onto reader (decision 041).
type Grant struct {
	CommandID string
	Reader    string
	Repo      string
	CreatedAt time.Time
	ExpiresAt time.Time
}

// AddGrant records a grant. It goes in the transaction that queues its restore.
func (q *Q) AddGrant(ctx context.Context, g Grant) error {
	_, err := q.db.Exec(ctx, `INSERT INTO gateway_grants (command_id, reader, repo, created_at, expires_at) VALUES ($1, $2, $3, $4, $5)`,
		g.CommandID, g.Reader, g.Repo, g.CreatedAt, g.ExpiresAt)
	return err
}

// HasGrant says whether reader may read repo's repository now.
func (q *Q) HasGrant(ctx context.Context, reader, repo string, now time.Time) (bool, error) {
	var ok bool
	err := q.db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM gateway_grants WHERE reader = $1 AND repo = $2 AND ended_at IS NULL AND expires_at > $3)`,
		reader, repo, now).Scan(&ok)
	return ok, err
}

// EndGrant ends the grant of a restore command, if it has one: its restore was reported, or it expired.
func (q *Q) EndGrant(ctx context.Context, commandID string, now time.Time) error {
	_, err := q.db.Exec(ctx, `UPDATE gateway_grants SET ended_at = $2 WHERE command_id = $1 AND ended_at IS NULL`, commandID, now)
	return err
}
