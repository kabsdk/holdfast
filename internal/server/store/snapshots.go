package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

/* ---------------- the snapshot catalogue (M2) ---------------- */

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
	// In the vault (decision 039): the snapshot in the source site this one is a copy of, and when the source
	// deleted it, if it did (decision 031's "Deleted in primary").
	Original         string     `json:"original,omitempty"`
	DeletedInPrimary *time.Time `json:"deleted_in_primary_at,omitempty"`
	// Filled in by the server's snapshot list (decision 031): when an admin asked to delete it, and, once
	// Holdfast deleted it and pruned, when S3's old versions of its data expire.
	DeletionRequestedAt *time.Time `json:"deletion_requested_at,omitempty"`
	ReallyGoneBy        *time.Time `json:"really_gone_by,omitempty"`
	// Who asked to delete it and why (decision 055), kept after Holdfast deleted it, and when it did.
	DeletionRequestedBy string     `json:"deletion_requested_by,omitempty"`
	DeletionReason      string     `json:"deletion_reason,omitempty"`
	DeletedOnRequest    *time.Time `json:"deleted_on_request_at,omitempty"`
	// Filled in for one job's list on request (spec §5.2): why the current retention keeps it, in restic's
	// words, or, if only an older policy still keeps it, from when the daily run may forget it.
	KeptBy       []string   `json:"kept_by,omitempty"`
	PendingUntil *time.Time `json:"pending_until,omitempty"`
	// Filled in by the server's snapshot list for a PostgreSQL database's job: the database a restore beside the
	// original makes, <database>-restore-<YYYYMMDD>-<HHMM> (decision 055).
	RestoreDatabase string `json:"restore_database,omitempty"`
}

// SyncSnapshots makes the catalogue of a host match a fresh listing of its repository. A listed snapshot
// is added, or brought back if it had gone. A known snapshot that is not listed any more gets a tombstone.
// It returns how many were added and how many went.
func (q *Q) SyncSnapshots(ctx context.Context, fqdn string, listed []Snapshot, now time.Time) (added, gone int, err error) {
	ids := make([]string, 0, len(listed))
	for _, s := range listed {
		ids = append(ids, s.ID)
		tag, err := q.db.Exec(ctx, `INSERT INTO snapshots (fqdn, id, short_id, time, hostname, job_id, tags, paths, parent,
			total_files, total_bytes, data_added, first_seen, original)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)
			ON CONFLICT (fqdn, id) DO UPDATE SET gone_at = NULL, acknowledged_at = NULL WHERE snapshots.gone_at IS NOT NULL`,
			fqdn, s.ID, s.ShortID, s.Time, s.Hostname, s.JobID, nonNil(s.Tags), nonNil(s.Paths), s.Parent,
			s.TotalFiles, s.TotalBytes, s.DataAdded, now, s.Original)
		if err != nil {
			return 0, 0, err
		}
		added += int(tag.RowsAffected())
	}
	// A snapshot Holdfast is deleting is marked before restic forgets it, so it never counts as gone here.
	err = q.db.QueryRow(ctx, `WITH g AS (UPDATE snapshots SET gone_at = $2 WHERE fqdn = $1 AND gone_at IS NULL AND NOT (id = ANY($3))
		RETURNING deleted_by_holdfast) SELECT count(*) FILTER (WHERE NOT deleted_by_holdfast) FROM g`, fqdn, now, ids).Scan(&gone)
	if err != nil {
		return 0, 0, err
	}
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
	EXISTS (SELECT 1 FROM events e WHERE e.fqdn = s.fqdn AND e.snapshot_id = s.id), s.original,
	(SELECT p.deleted_at FROM primary_deleted p WHERE p.fqdn = s.fqdn AND p.original = s.original AND s.original <> ''),
	s.deletion_requested_by, s.deletion_reason, s.deleted_on_request_at`

func scanSnapshot(row pgx.Row) (Snapshot, error) {
	var s Snapshot
	err := row.Scan(&s.FQDN, &s.ID, &s.ShortID, &s.Time, &s.Hostname, &s.JobID, &s.Tags, &s.Paths, &s.Parent, &s.TotalFiles, &s.TotalBytes,
		&s.DataAdded, &s.FirstSeen, &s.GoneAt, &s.DeletedByUs, &s.Acknowledged, &s.MadeByHoldfast, &s.Original, &s.DeletedInPrimary,
		&s.DeletionRequestedBy, &s.DeletionReason, &s.DeletedOnRequest)
	return s, notFound(err)
}

// SetDeletionReason keeps on a snapshot who asked to delete it and why (decision 055); empty strings clear them, as a
// cancelled deletion does.
func (q *Q) SetDeletionReason(ctx context.Context, fqdn, id, by, reason string) error {
	_, err := q.db.Exec(ctx, `UPDATE snapshots SET deletion_requested_by = $3, deletion_reason = $4 WHERE fqdn = $1 AND id = $2`, fqdn, id, by, reason)
	return err
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

// SearchSnapshots finds snapshots of any host by an ID prefix, tombstones included (spec §5.6).
func (q *Q) SearchSnapshots(ctx context.Context, prefix string, limit int) ([]Snapshot, error) {
	rows, err := q.db.Query(ctx, `SELECT `+snapshotCols+` FROM snapshots s WHERE s.id LIKE $1 || '%' ORDER BY s.time DESC LIMIT $2`, prefix, limit)
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

// RecordRepositoryKeys compares a fresh `restic key list` with the keys Holdfast knows, and keeps the others
// as foreign. With no known keys yet, as for a repository made before this was recorded, the first list
// becomes the known one. It returns the foreign IDs.
func (q *Q) RecordRepositoryKeys(ctx context.Context, fqdn string, ids []string) (foreign, fresh []string, err error) {
	var known, before []string
	if err := q.db.QueryRow(ctx, `SELECT repo_keys_known, repo_keys_foreign FROM hosts WHERE fqdn = $1`, fqdn).Scan(&known, &before); err != nil {
		return nil, nil, notFound(err)
	}
	if len(known) == 0 {
		_, err := q.db.Exec(ctx, `UPDATE hosts SET repo_keys_known = $2, repo_keys_foreign = '{}' WHERE fqdn = $1`, fqdn, nonNil(ids))
		return []string{}, []string{}, err
	}
	in := func(list []string) map[string]bool {
		m := map[string]bool{}
		for _, k := range list {
			m[k] = true
		}
		return m
	}
	isKnown, wasForeign := in(known), in(before)
	foreign, fresh = []string{}, []string{}
	for _, id := range ids {
		if !isKnown[id] {
			foreign = append(foreign, id)
			if !wasForeign[id] {
				fresh = append(fresh, id)
			}
		}
	}
	_, err = q.db.Exec(ctx, `UPDATE hosts SET repo_keys_foreign = $2 WHERE fqdn = $1`, fqdn, foreign)
	return foreign, fresh, err
}

// ResetRepositoryKeys makes ids the known keys of a repository that Holdfast has just initialised. It
// reports whether the host had known keys before, which means the repository was initialised again after
// the old one was lost.
func (q *Q) ResetRepositoryKeys(ctx context.Context, fqdn string, ids []string) (again bool, err error) {
	var known []string
	if err := q.db.QueryRow(ctx, `SELECT repo_keys_known FROM hosts WHERE fqdn = $1`, fqdn).Scan(&known); err != nil {
		return false, notFound(err)
	}
	_, err = q.db.Exec(ctx, `UPDATE hosts SET repo_keys_known = $2, repo_keys_foreign = '{}' WHERE fqdn = $1`, fqdn, nonNil(ids))
	return len(known) > 0, err
}

// AcknowledgeRepositoryKeys accepts a host's foreign keys as known, which clears that banner. It returns
// how many it accepted.
func (q *Q) AcknowledgeRepositoryKeys(ctx context.Context, fqdn string) (int, error) {
	var foreign []string
	if err := q.db.QueryRow(ctx, `SELECT repo_keys_foreign FROM hosts WHERE fqdn = $1`, fqdn).Scan(&foreign); err != nil {
		return 0, notFound(err)
	}
	_, err := q.db.Exec(ctx, `UPDATE hosts SET repo_keys_known = repo_keys_known || repo_keys_foreign, repo_keys_foreign = '{}' WHERE fqdn = $1`, fqdn)
	return len(foreign), err
}

// ForeignRepositoryKeys returns, per host, the repository keys Holdfast did not make.
func (q *Q) ForeignRepositoryKeys(ctx context.Context) (map[string][]string, error) {
	rows, err := q.db.Query(ctx, `SELECT fqdn, repo_keys_foreign FROM hosts WHERE cardinality(repo_keys_foreign) > 0`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]string{}
	for rows.Next() {
		var f string
		var ids []string
		if err := rows.Scan(&f, &ids); err != nil {
			return nil, err
		}
		out[f] = ids
	}
	return out, rows.Err()
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

// JobNewest is the newest snapshot of one job that the repository held when the catalogue was last read.
type JobNewest struct {
	FQDN, JobID string
	Newest      time.Time
}

// NewestSnapshots returns, for every job with a snapshot in its repository, the newest one's time, as restic read it
// from the bucket: the storage witness the server is.
func (q *Q) NewestSnapshots(ctx context.Context) ([]JobNewest, error) {
	rows, err := q.db.Query(ctx, `SELECT fqdn, job_id, max(time) FROM snapshots WHERE gone_at IS NULL AND job_id <> ''
		GROUP BY fqdn, job_id ORDER BY fqdn, job_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []JobNewest
	for rows.Next() {
		var n JobNewest
		if err := rows.Scan(&n.FQDN, &n.JobID, &n.Newest); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// ReportedSnapshot is the snapshot an agent last reported for a job, and whether the bucket held it when the server
// next read the repository.
type ReportedSnapshot struct {
	FQDN, JobID, SnapshotID string
	InBucket                bool
}

// ReportedInBucket is the storage witness: for every job whose last reported snapshot the server has
// read the repository since, whether restic found it there. The read is the server's, with the maintenance key
// straight from S3, and the report's arrival is the server's clock, so the agent's clock plays no part. A snapshot
// that was there and went later stays in the catalogue with gone_at set: it counts as held here, and as disappeared.
func (q *Q) ReportedInBucket(ctx context.Context) ([]ReportedSnapshot, error) {
	rows, err := q.db.Query(ctx, `SELECT js.fqdn, js.job_id, js.last_snapshot_id,
		EXISTS (SELECT 1 FROM snapshots s WHERE s.fqdn = js.fqdn AND s.id = js.last_snapshot_id)
		FROM job_status js JOIN hosts h ON h.fqdn = js.fqdn
		WHERE js.last_snapshot_id <> '' AND h.catalogue_refreshed_at >
			(SELECT min(e.received_at) FROM events e WHERE e.fqdn = js.fqdn AND e.snapshot_id = js.last_snapshot_id)
		ORDER BY js.fqdn, js.job_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ReportedSnapshot
	for rows.Next() {
		var r ReportedSnapshot
		if err := rows.Scan(&r.FQDN, &r.JobID, &r.SnapshotID, &r.InBucket); err != nil {
			return nil, err
		}
		out = append(out, r)
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
