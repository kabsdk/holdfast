package store

import (
	"context"
	"time"
)

/* ---------------- retention, Prune now and Delete snapshot (M3, plan-m3 §3) ---------------- */

// RevisionsInForce returns, oldest first and with their configs, every revision that was in force at some
// time after since: the newest one saved at or before since, and every one saved after it.
func (q *Q) RevisionsInForce(ctx context.Context, since time.Time) ([]Revision, error) {
	rows, err := q.db.Query(ctx, `SELECT `+revCols+`, config::text FROM revisions
		WHERE created_at > $1 OR number = (SELECT max(number) FROM revisions WHERE created_at <= $1)
		ORDER BY number`, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Revision
	for rows.Next() {
		r, err := scanRevision(rows, true)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// MadeByHoldfast returns which of ids an agent of the host reported in an event (decision 032).
func (q *Q) MadeByHoldfast(ctx context.Context, fqdn string, ids []string) (map[string]bool, error) {
	out := map[string]bool{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := q.db.Query(ctx, `SELECT DISTINCT snapshot_id FROM events WHERE fqdn = $1 AND snapshot_id = ANY($2)`, fqdn, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// MarkDeleting marks snapshots Holdfast is about to forget, before restic forgets them, so a catalogue
// refresh in between never reports them as disappeared.
func (q *Q) MarkDeleting(ctx context.Context, fqdn string, ids []string) error {
	_, err := q.db.Exec(ctx, `UPDATE snapshots SET deleted_by_holdfast = true WHERE fqdn = $1 AND id = ANY($2)`, fqdn, nonNil(ids))
	return err
}

// MarkDeleted gives the snapshots restic forgot their tombstones.
func (q *Q) MarkDeleted(ctx context.Context, fqdn string, ids []string, now time.Time) error {
	_, err := q.db.Exec(ctx, `UPDATE snapshots SET deleted_by_holdfast = true, gone_at = COALESCE(gone_at, $3)
		WHERE fqdn = $1 AND id = ANY($2)`, fqdn, nonNil(ids), now)
	return err
}

// UnmarkDeleting takes the mark off snapshots that are still there after a forget failed.
func (q *Q) UnmarkDeleting(ctx context.Context, fqdn string, ids []string) error {
	_, err := q.db.Exec(ctx, `UPDATE snapshots SET deleted_by_holdfast = false WHERE fqdn = $1 AND id = ANY($2) AND gone_at IS NULL`, fqdn, nonNil(ids))
	return err
}

// SetPruneNeeded records whether forgotten snapshots still wait for a prune.
func (q *Q) SetPruneNeeded(ctx context.Context, fqdn string, needed bool) error {
	_, err := q.db.Exec(ctx, `UPDATE hosts SET prune_needed = $2 WHERE fqdn = $1`, fqdn, needed)
	return err
}

// Maintenance run kinds.
const (
	RunScheduled = "scheduled"  // the daily run
	RunNow       = "run_now"    // the daily run, started by a person
	RunPruneNow  = "prune_now"  // Prune now: the current policy only, without the 7-day protection
	RunDeleteNow = "delete_now" // Delete now: one snapshot, without the 7-day wait
)

// MaintenanceRun is one forget-and-prune on a host. The list of them is the list of past prunes (OQ2).
type MaintenanceRun struct {
	ID         int64      `json:"id"`
	FQDN       string     `json:"fqdn"`
	Kind       string     `json:"kind"`
	Actor      string     `json:"actor"`
	StartedAt  time.Time  `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	Status     string     `json:"status"` // running, success or failed
	Forgotten  []string   `json:"forgotten"`
	Pruned     bool       `json:"pruned"`
	Freed      string     `json:"freed,omitempty"` // as restic printed it
	Error      string     `json:"error,omitempty"`
}

// StartMaintenanceRun records a run that begins.
func (q *Q) StartMaintenanceRun(ctx context.Context, fqdn, kind, actor string, now time.Time) (int64, error) {
	var id int64
	err := q.db.QueryRow(ctx, `INSERT INTO maintenance_runs (fqdn, kind, actor, started_at) VALUES ($1, $2, $3, $4) RETURNING id`,
		fqdn, kind, actor, now).Scan(&id)
	return id, err
}

// FinishMaintenanceRun records how a run ended.
func (q *Q) FinishMaintenanceRun(ctx context.Context, r MaintenanceRun) error {
	_, err := q.db.Exec(ctx, `UPDATE maintenance_runs SET finished_at = $2, status = $3, forgotten = $4, pruned = $5, freed = $6, error = $7
		WHERE id = $1`, r.ID, r.FinishedAt, r.Status, nonNil(r.Forgotten), r.Pruned, r.Freed, r.Error)
	return err
}

const runCols = `id, fqdn, kind, actor, started_at, finished_at, status, forgotten, pruned, freed, error`

// ListMaintenanceRuns returns a host's runs, newest first; every host's when fqdn is empty.
func (q *Q) ListMaintenanceRuns(ctx context.Context, fqdn string, limit int) ([]MaintenanceRun, error) {
	rows, err := q.db.Query(ctx, `SELECT `+runCols+` FROM maintenance_runs WHERE $1 = '' OR fqdn = $1 ORDER BY started_at DESC, id DESC LIMIT $2`, fqdn, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []MaintenanceRun{}
	for rows.Next() {
		var r MaintenanceRun
		if err := rows.Scan(&r.ID, &r.FQDN, &r.Kind, &r.Actor, &r.StartedAt, &r.FinishedAt, &r.Status, &r.Forgotten, &r.Pruned, &r.Freed, &r.Error); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// RunHealth is, for one host, how its forgets and prunes have gone: for the metrics.
type RunHealth struct {
	FQDN        string
	LastFailed  bool       // the newest finished run failed
	LastSuccess *time.Time // when a run last finished well
}

// RetentionHealth returns RunHealth for every host that has had a run.
func (q *Q) RetentionHealth(ctx context.Context) ([]RunHealth, error) {
	rows, err := q.db.Query(ctx, `SELECT l.fqdn, l.status = 'failed',
		(SELECT max(finished_at) FROM maintenance_runs s WHERE s.fqdn = l.fqdn AND s.status = 'success')
		FROM (SELECT DISTINCT ON (fqdn) fqdn, status FROM maintenance_runs WHERE status <> 'running' ORDER BY fqdn, started_at DESC, id DESC) l
		ORDER BY l.fqdn`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RunHealth
	for rows.Next() {
		var h RunHealth
		if err := rows.Scan(&h.FQDN, &h.LastFailed, &h.LastSuccess); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// LastScheduledRun returns when the host's last daily run started, or nil.
func (q *Q) LastScheduledRun(ctx context.Context, fqdn string) (*time.Time, error) {
	var t *time.Time
	err := q.db.QueryRow(ctx, `SELECT max(started_at) FROM maintenance_runs WHERE fqdn = $1 AND kind = 'scheduled'`, fqdn).Scan(&t)
	return t, err
}

// PendingDeletion is a snapshot an admin asked to delete (decision 031).
type PendingDeletion struct {
	FQDN        string    `json:"fqdn"`
	SnapshotID  string    `json:"snapshot_id"`
	RequestedBy string    `json:"requested_by"`
	RequestedAt time.Time `json:"requested_at"`
	Message     string    `json:"message,omitempty"`
}

// AddPendingDeletion records a deletion request. A second request for the same snapshot is a conflict.
func (q *Q) AddPendingDeletion(ctx context.Context, d PendingDeletion) error {
	_, err := q.db.Exec(ctx, `INSERT INTO pending_deletions (fqdn, snapshot_id, requested_by, requested_at, message) VALUES ($1, $2, $3, $4, $5)`,
		d.FQDN, d.SnapshotID, d.RequestedBy, d.RequestedAt, d.Message)
	if isUniqueViolation(err) {
		return ErrConflict
	}
	return err
}

// RemovePendingDeletion cancels a request, or clears it once done. It returns ErrNotFound if there was none.
func (q *Q) RemovePendingDeletion(ctx context.Context, fqdn, id string) error {
	tag, err := q.db.Exec(ctx, `DELETE FROM pending_deletions WHERE fqdn = $1 AND snapshot_id = $2`, fqdn, id)
	if err == nil && tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return err
}

// ListPendingDeletions returns a host's deletion requests, oldest first.
func (q *Q) ListPendingDeletions(ctx context.Context, fqdn string) ([]PendingDeletion, error) {
	rows, err := q.db.Query(ctx, `SELECT fqdn, snapshot_id, requested_by, requested_at, message FROM pending_deletions
		WHERE fqdn = $1 ORDER BY requested_at, snapshot_id`, fqdn)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []PendingDeletion{}
	for rows.Next() {
		var d PendingDeletion
		if err := rows.Scan(&d.FQDN, &d.SnapshotID, &d.RequestedBy, &d.RequestedAt, &d.Message); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}
