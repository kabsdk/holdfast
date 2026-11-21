package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

/* ---------------- the snapshot catalogue (M2, plan-m2 Q2) ---------------- */

// Snapshot is one row of the catalogue.
type Snapshot struct {
	FQDN           string     `json:"fqdn"`
	ID             string     `json:"id"`
	ShortID        string     `json:"short_id"`
	Time           time.Time  `json:"time"`
	Hostname       string     `json:"hostname"`
	JobID          string     `json:"job_id,omitempty"`
	Tags           []string   `json:"tags"`
	Paths          []string   `json:"paths"`
	Parent         string     `json:"parent,omitempty"`
	TotalFiles     *int64     `json:"total_files,omitempty"`
	TotalBytes     *int64     `json:"total_bytes,omitempty"`
	DataAdded      *int64     `json:"data_added,omitempty"`
	FirstSeen      time.Time  `json:"first_seen"`
	GoneAt         *time.Time `json:"gone_at,omitempty"` // no longer in the repository
	DeletedByUs    bool       `json:"deleted_by_holdfast,omitempty"`
	MadeByHoldfast bool       `json:"made_by_holdfast"` // an agent reported this snapshot ID (decision 032)
	Acknowledged   *time.Time `json:"acknowledged_at,omitempty"`
}

// SyncSnapshots makes the catalogue of a host match a fresh listing of its repository. A listed snapshot
// is added, or brought back if it had gone. A known snapshot that is not listed any more gets a tombstone.
// It returns how many were added and how many went.
func (q *Q) SyncSnapshots(ctx context.Context, fqdn string, listed []Snapshot, now time.Time) (added, gone int, err error) {
	ids := make([]string, 0, len(listed))
	for _, s := range listed {
		ids = append(ids, s.ID)
		tag, err := q.db.Exec(ctx, `INSERT INTO snapshots (fqdn, id, short_id, time, hostname, job_id, tags, paths, parent,
			total_files, total_bytes, data_added, first_seen)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)
			ON CONFLICT (fqdn, id) DO UPDATE SET gone_at = NULL, acknowledged_at = NULL WHERE snapshots.gone_at IS NOT NULL`,
			fqdn, s.ID, s.ShortID, s.Time, s.Hostname, s.JobID, nonNil(s.Tags), nonNil(s.Paths), s.Parent,
			s.TotalFiles, s.TotalBytes, s.DataAdded, now)
		if err != nil {
			return 0, 0, err
		}
		added += int(tag.RowsAffected())
	}
	tag, err := q.db.Exec(ctx, `UPDATE snapshots SET gone_at = $2 WHERE fqdn = $1 AND gone_at IS NULL AND NOT (id = ANY($3))`, fqdn, now, ids)
	if err != nil {
		return 0, 0, err
	}
	gone = int(tag.RowsAffected())
	_, err = q.db.Exec(ctx, `UPDATE hosts SET catalogue_refreshed_at = $2, catalogue_error = '' WHERE fqdn = $1`, fqdn, now)
	return added, gone, err
}

// RecordCatalogueError keeps why a host's catalogue could not be refreshed.
func (q *Q) RecordCatalogueError(ctx context.Context, fqdn, msg string) error {
	_, err := q.db.Exec(ctx, `UPDATE hosts SET catalogue_error = $2 WHERE fqdn = $1`, fqdn, msg)
	return err
}

const snapshotCols = `s.fqdn, s.id, s.short_id, s.time, s.hostname, s.job_id, s.tags, s.paths, s.parent, s.total_files, s.total_bytes,
	s.data_added, s.first_seen, s.gone_at, s.deleted_by_holdfast, s.acknowledged_at,
	EXISTS (SELECT 1 FROM events e WHERE e.fqdn = s.fqdn AND e.snapshot_id = s.id)`

func scanSnapshot(row pgx.Row) (Snapshot, error) {
	var s Snapshot
	err := row.Scan(&s.FQDN, &s.ID, &s.ShortID, &s.Time, &s.Hostname, &s.JobID, &s.Tags, &s.Paths, &s.Parent, &s.TotalFiles, &s.TotalBytes,
		&s.DataAdded, &s.FirstSeen, &s.GoneAt, &s.DeletedByUs, &s.Acknowledged, &s.MadeByHoldfast)
	return s, notFound(err)
}

// SnapshotFilter narrows ListSnapshots.
type SnapshotFilter struct {
	JobID       string
	From, Until time.Time // Until is exclusive; zero means unbounded
	WithGone    bool      // include tombstones
}

// ListSnapshots returns a host's snapshots, newest first.
func (q *Q) ListSnapshots(ctx context.Context, fqdn string, f SnapshotFilter) ([]Snapshot, error) {
	rows, err := q.db.Query(ctx, `SELECT `+snapshotCols+` FROM snapshots s
		WHERE s.fqdn = $1 AND ($2 = '' OR s.job_id = $2) AND ($3::timestamptz IS NULL OR s.time >= $3) AND ($4::timestamptz IS NULL OR s.time < $4)
		AND ($5 OR s.gone_at IS NULL) ORDER BY s.time DESC, s.id`, fqdn, f.JobID, tsOrNil(f.From), tsOrNil(f.Until), f.WithGone)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Snapshot{}
	for rows.Next() {
		s, err := scanSnapshot(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// ErrAmbiguous is returned for a snapshot ID prefix that matches more than one snapshot.
var ErrAmbiguous = fmt.Errorf("the snapshot ID prefix matches more than one snapshot: %w", ErrConflict)

// GetSnapshot finds one of a host's snapshots by its ID or a prefix of at least 8 characters.
func (q *Q) GetSnapshot(ctx context.Context, fqdn, idOrPrefix string) (Snapshot, error) {
	rows, err := q.db.Query(ctx, `SELECT `+snapshotCols+` FROM snapshots s WHERE s.fqdn = $1 AND s.id LIKE $2 || '%' LIMIT 2`, fqdn, idOrPrefix)
	if err != nil {
		return Snapshot{}, err
	}
	defer rows.Close()
	var found []Snapshot
	for rows.Next() {
		s, err := scanSnapshot(rows)
		if err != nil {
			return Snapshot{}, err
		}
		found = append(found, s)
	}
	switch {
	case rows.Err() != nil:
		return Snapshot{}, rows.Err()
	case len(found) == 0:
		return Snapshot{}, ErrNotFound
	case len(found) > 1:
		return Snapshot{}, ErrAmbiguous
	}
	return found[0], nil
}

// AcknowledgeDisappeared marks a host's disappeared snapshots as seen, which clears the banner. The
// tombstones stay. It returns how many it marked.
func (q *Q) AcknowledgeDisappeared(ctx context.Context, fqdn string, now time.Time) (int, error) {
	tag, err := q.db.Exec(ctx, `UPDATE snapshots SET acknowledged_at = $2
		WHERE fqdn = $1 AND gone_at IS NOT NULL AND NOT deleted_by_holdfast AND acknowledged_at IS NULL`, fqdn, now)
	return int(tag.RowsAffected()), err
}

// CatalogueCounts are what the decision 032 banners show for one host.
type CatalogueCounts struct {
	Snapshots         int `json:"snapshots"`
	NotMadeByHoldfast int `json:"not_made_by_holdfast"`
	Disappeared       int `json:"disappeared"` // gone without Holdfast deleting them, not yet acknowledged
}

// CountCatalogue returns the banner counts of every host.
func (q *Q) CountCatalogue(ctx context.Context) (map[string]CatalogueCounts, error) {
	rows, err := q.db.Query(ctx, `SELECT s.fqdn,
		count(*) FILTER (WHERE s.gone_at IS NULL),
		count(*) FILTER (WHERE s.gone_at IS NULL AND NOT EXISTS (SELECT 1 FROM events e WHERE e.fqdn = s.fqdn AND e.snapshot_id = s.id)),
		count(*) FILTER (WHERE s.gone_at IS NOT NULL AND NOT s.deleted_by_holdfast AND s.acknowledged_at IS NULL)
		FROM snapshots s GROUP BY s.fqdn`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]CatalogueCounts{}
	for rows.Next() {
		var f string
		var c CatalogueCounts
		if err := rows.Scan(&f, &c.Snapshots, &c.NotMadeByHoldfast, &c.Disappeared); err != nil {
			return nil, err
		}
		out[f] = c
	}
	return out, rows.Err()
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func tsOrNil(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t
}
