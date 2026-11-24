package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

/* ---------------- PostgreSQL connections' passwords (decision 055) ---------------- */

// PGPassword is one sealed password of a PostgreSQL connection: the backup user's, or the restore user's.
type PGPassword struct {
	FQDN       string
	Connection string
	Kind       string // "backup" or "restore"
	Sealed     []byte
	UpdatedAt  time.Time
}

// PG password kinds.
const (
	PGBackup  = "backup"
	PGRestore = "restore"
)

// PutPGPassword stores a sealed password, replacing the one before.
func (q *Q) PutPGPassword(ctx context.Context, p PGPassword) error {
	_, err := q.db.Exec(ctx, `INSERT INTO pg_passwords (fqdn, connection, kind, sealed, updated_at) VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (fqdn, connection, kind) DO UPDATE SET sealed = $4, updated_at = $5`, p.FQDN, p.Connection, p.Kind, p.Sealed, p.UpdatedAt)
	return err
}

// DeletePGPassword removes one password: a connection without a restore user keeps none for it.
func (q *Q) DeletePGPassword(ctx context.Context, fqdn, conn, kind string) (bool, error) {
	tag, err := q.db.Exec(ctx, `DELETE FROM pg_passwords WHERE fqdn = $1 AND connection = $2 AND kind = $3`, fqdn, conn, kind)
	return tag.RowsAffected() > 0, err
}

// DeletePGPasswords removes a connection's passwords, when the connection is removed.
func (q *Q) DeletePGPasswords(ctx context.Context, fqdn, conn string) error {
	_, err := q.db.Exec(ctx, `DELETE FROM pg_passwords WHERE fqdn = $1 AND connection = $2`, fqdn, conn)
	return err
}

// PGPasswords returns an agent's sealed PostgreSQL passwords.
func (q *Q) PGPasswords(ctx context.Context, fqdn string) ([]PGPassword, error) {
	rows, err := q.db.Query(ctx, `SELECT fqdn, connection, kind, sealed, updated_at FROM pg_passwords WHERE fqdn = $1 ORDER BY connection, kind`, fqdn)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PGPassword
	for rows.Next() {
		var p PGPassword
		if err := rows.Scan(&p.FQDN, &p.Connection, &p.Kind, &p.Sealed, &p.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// BumpCredentials gives an agent's credentials a new version, so its next poll fetches them again: after a
// PostgreSQL password changed.
func (q *Q) BumpCredentials(ctx context.Context, fqdn string) error {
	_, err := q.db.Exec(ctx, `UPDATE host_credentials SET version = version + 1 WHERE fqdn = $1`, fqdn)
	return err
}

// LatestPGTests returns, for each connection of an agent, the newest test the agent reported: a postgresql_test
// event, whose detail holds the result.
func (q *Q) LatestPGTests(ctx context.Context, fqdn string) (map[string]Event, error) {
	evs, err := q.queryEvents(ctx, `WHERE fqdn = $1 AND kind = 'postgresql_test' ORDER BY finished_at DESC, received_at DESC LIMIT 500`, fqdn)
	if err != nil {
		return nil, err
	}
	out := map[string]Event{}
	for _, e := range evs {
		c := e.Detail["connection"]
		if _, seen := out[c]; c != "" && !seen {
			out[c] = e
		}
	}
	return out, nil
}

// UnfinishedPGTest returns the newest test of a connection, asked since since, whose result has not come back yet:
// queued, or running on the agent. "" if there is none.
func (q *Q) UnfinishedPGTest(ctx context.Context, fqdn, connection string, since, now time.Time) (string, error) {
	var id string
	err := q.db.QueryRow(ctx, `SELECT c.id FROM commands c WHERE c.fqdn = $1 AND c.kind = 'postgresql_test' AND c.payload->>'connection' = $2
		AND c.cancelled_at IS NULL AND (c.expires_at IS NULL OR c.expires_at > $4) AND c.created_at >= $3
		AND NOT EXISTS (SELECT 1 FROM events e WHERE e.fqdn = c.fqdn AND e.command_id = c.id)
		ORDER BY c.created_at DESC LIMIT 1`, fqdn, connection, since, now).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return id, err
}

// PGTestDetail decodes a test event's result, or returns nil.
func PGTestDetail[T any](e Event) *T {
	var r T
	if s, ok := e.Detail["result"]; ok && json.Unmarshal([]byte(s), &r) == nil {
		return &r
	}
	return nil
}
