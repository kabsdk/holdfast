package store

import (
	"context"
	"time"
)

/* ---------------- login sessions (M4, decision 034) ---------------- */

// Session is one login. Its token is never stored, only its sha256.
type Session struct {
	TokenSHA256 string        `json:"-"`
	ID          string        `json:"id"`   // the first 12 characters of the sha256, to name a session in lists
	Kind        string        `json:"kind"` // browser or cli
	Subject     string        `json:"subject"`
	Username    string        `json:"username"`
	Groups      []string      `json:"groups"`
	CreatedAt   time.Time     `json:"created_at"`
	ExpiresAt   time.Time     `json:"expires_at"`
	Idle        time.Duration `json:"-"` // a browser session ends after this long without use; 0: never
	LastUsedAt  time.Time     `json:"last_used_at"`
}

// CreateSession stores a new session.
func (q *Q) CreateSession(ctx context.Context, s Session) error {
	_, err := q.db.Exec(ctx, `INSERT INTO sessions (token_sha256, kind, subject, username, groups, created_at, expires_at, idle_seconds, last_used_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $6)`, s.TokenSHA256, s.Kind, s.Subject, s.Username, nonNil(s.Groups), s.CreatedAt, s.ExpiresAt, int(s.Idle.Seconds()))
	return err
}

const (
	sessionCols = `token_sha256, left(token_sha256, 12), kind, subject, username, groups, created_at, expires_at, idle_seconds, last_used_at`
	sessionLive = `expires_at > $1 AND (idle_seconds = 0 OR last_used_at + make_interval(secs => idle_seconds) > $1)`
)

type rowScanner interface{ Scan(dest ...any) error }

func scanSession(row rowScanner) (Session, error) {
	var s Session
	var idle int
	err := row.Scan(&s.TokenSHA256, &s.ID, &s.Kind, &s.Subject, &s.Username, &s.Groups, &s.CreatedAt, &s.ExpiresAt, &idle, &s.LastUsedAt)
	s.Idle = time.Duration(idle) * time.Second
	return s, err
}

// UseSession returns the session with this token hash and marks it used now, or ErrNotFound if there is
// none, or it has ended: past its end, or idle for longer than it may be.
func (q *Q) UseSession(ctx context.Context, tokenSHA256 string, now time.Time) (Session, error) {
	s, err := scanSession(q.db.QueryRow(ctx, `UPDATE sessions SET last_used_at = $1 WHERE token_sha256 = $2 AND `+sessionLive+`
		RETURNING `+sessionCols, now, tokenSHA256))
	return s, notFound(err)
}

// DeleteSession ends one session.
func (q *Q) DeleteSession(ctx context.Context, tokenSHA256 string) error {
	_, err := q.db.Exec(ctx, `DELETE FROM sessions WHERE token_sha256 = $1`, tokenSHA256)
	return err
}

// EndSessionsOf ends every session of a user, and returns how many there were.
func (q *Q) EndSessionsOf(ctx context.Context, username string) (int, error) {
	tag, err := q.db.Exec(ctx, `DELETE FROM sessions WHERE username = $1`, username)
	return int(tag.RowsAffected()), err
}

// ListSessions returns the sessions that have not ended, newest first.
func (q *Q) ListSessions(ctx context.Context, now time.Time) ([]Session, error) {
	rows, err := q.db.Query(ctx, `SELECT `+sessionCols+` FROM sessions WHERE `+sessionLive+` ORDER BY created_at DESC`, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Session{}
	for rows.Next() {
		s, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// PurgeSessions deletes the sessions that have ended.
func (q *Q) PurgeSessions(ctx context.Context, now time.Time) error {
	_, err := q.db.Exec(ctx, `DELETE FROM sessions WHERE NOT (`+sessionLive+`)`, now)
	return err
}
