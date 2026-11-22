package store

import (
	"context"
	"encoding/json"
	"time"
)

// Off-site copies (decisions 027, 039 and 041). On the server: the off-site instance's latest report, by which the
// daily run holds back what is not copied yet and the hosts' pages show the copies. In the off-site instance: the
// snapshots deleted in primary, and each host's copies. M7's vault used the same tables, as another program.

// OffsiteName is the off-site instance's row in vaults: there is one, and it registers itself.
const OffsiteName = "offsite"

// Vault is the off-site instance's row, with its latest report.
type Vault struct {
	Name       string          `json:"name"`
	CreatedAt  time.Time       `json:"created_at"`
	CreatedBy  string          `json:"created_by"`
	LastSeenAt *time.Time      `json:"last_seen_at,omitempty"`
	ReportedAt *time.Time      `json:"reported_at,omitempty"`
	Report     json.RawMessage `json:"report,omitempty"`
}

// EnsureVault makes the off-site instance's row, if it is not there.
func (q *Q) EnsureVault(ctx context.Context, name, by string, now time.Time) error {
	_, err := q.db.Exec(ctx, `INSERT INTO vaults (name, created_at, created_by) VALUES ($1, $2, $3) ON CONFLICT (name) DO NOTHING`, name, now, by)
	return err
}

const vaultCols = `name, created_at, created_by, last_seen_at, reported_at, report`

func scanVault(row interface{ Scan(...any) error }) (Vault, error) {
	var v Vault
	var report []byte
	err := row.Scan(&v.Name, &v.CreatedAt, &v.CreatedBy, &v.LastSeenAt, &v.ReportedAt, &report)
	v.Report = report
	return v, notFound(err)
}

// ListVaults returns every row, by name: the off-site instance's, once it ran.
func (q *Q) ListVaults(ctx context.Context) ([]Vault, error) {
	rows, err := q.db.Query(ctx, `SELECT `+vaultCols+` FROM vaults ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Vault{}
	for rows.Next() {
		v, err := scanVault(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// RecordVaultReport keeps the off-site instance's latest report.
func (q *Q) RecordVaultReport(ctx context.Context, name string, report []byte, now time.Time) error {
	_, err := q.db.Exec(ctx, `UPDATE vaults SET last_seen_at = $2, reported_at = $2, report = $3 WHERE name = $1`, name, now, report)
	return err
}

// MarkDeletedOnRequest records that Holdfast deleted these snapshots because an admin asked (decision 031).
func (q *Q) MarkDeletedOnRequest(ctx context.Context, fqdn string, ids []string, now time.Time) error {
	_, err := q.db.Exec(ctx, `UPDATE snapshots SET deleted_on_request_at = $3 WHERE fqdn = $1 AND id = ANY($2) AND deleted_on_request_at IS NULL`, fqdn, ids, now)
	return err
}

// DeletedOnRequest returns the snapshots of a host that were deleted because an admin asked, and when.
func (q *Q) DeletedOnRequest(ctx context.Context, fqdn string) (map[string]time.Time, error) {
	rows, err := q.db.Query(ctx, `SELECT id, deleted_on_request_at FROM snapshots WHERE fqdn = $1 AND deleted_on_request_at IS NOT NULL`, fqdn)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]time.Time{}
	for rows.Next() {
		var id string
		var at time.Time
		if err := rows.Scan(&id, &at); err != nil {
			return nil, err
		}
		out[id] = at
	}
	return out, rows.Err()
}

/* ---------------- in the off-site instance ---------------- */

// ActivateHost records a host the off-site instance copies, or brings back one it had retired: it is active at once,
// as no agent enrolls for it.
func (q *Q) ActivateHost(ctx context.Context, fqdn string, rev int64, now time.Time, revive bool) error {
	if revive {
		if err := q.ReviveHost(ctx, fqdn, rev); err != nil {
			return err
		}
	} else if err := q.CreateHost(ctx, fqdn, rev, now); err != nil {
		return err
	}
	_, err := q.db.Exec(ctx, `UPDATE hosts SET state = 'enrolled', connected_at = COALESCE(connected_at, $2) WHERE fqdn = $1`, fqdn, now)
	return err
}

// SetPrimaryDeleted records which of a host's primary snapshots were deleted on request, replacing the list.
func (q *Q) SetPrimaryDeleted(ctx context.Context, fqdn string, deleted map[string]time.Time) error {
	if _, err := q.db.Exec(ctx, `DELETE FROM primary_deleted WHERE fqdn = $1`, fqdn); err != nil {
		return err
	}
	for id, at := range deleted {
		if _, err := q.db.Exec(ctx, `INSERT INTO primary_deleted (fqdn, original, deleted_at) VALUES ($1, $2, $3)`, fqdn, id, at); err != nil {
			return err
		}
	}
	return nil
}

// RecordCopy keeps how a host's last copy went.
func (q *Q) RecordCopy(ctx context.Context, fqdn string, now time.Time, failure string) error {
	if failure != "" {
		_, err := q.db.Exec(ctx, `UPDATE hosts SET copy_tried_at = $2, copy_error = $3 WHERE fqdn = $1`, fqdn, now, failure)
		return err
	}
	_, err := q.db.Exec(ctx, `UPDATE hosts SET copy_tried_at = $2, copied_at = $2, copy_error = '' WHERE fqdn = $1`, fqdn, now)
	return err
}

// CopyError returns a host's last copy failure, if its last copy failed.
func (q *Q) CopyError(ctx context.Context, fqdn string) (string, error) {
	var e string
	err := q.db.QueryRow(ctx, `SELECT copy_error FROM hosts WHERE fqdn = $1`, fqdn).Scan(&e)
	return e, notFound(err)
}
