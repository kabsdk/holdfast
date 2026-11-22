package store

import (
	"context"
	"encoding/json"
	"time"
)

// M7 (decisions 027 and 039). On holdfast-server: the vaults that copy its hosts, and restores from them. In the
// vault: its source's feed, the snapshots deleted in primary, and each host's copies.

// Vault is a vault registered with this server.
type Vault struct {
	Name          string          `json:"name"`
	S3AccessKeyID string          `json:"-"`
	S3Secret      []byte          `json:"-"`
	CreatedAt     time.Time       `json:"created_at"`
	CreatedBy     string          `json:"created_by"`
	LastSeenAt    *time.Time      `json:"last_seen_at,omitempty"`
	ReportedAt    *time.Time      `json:"reported_at,omitempty"`
	Report        json.RawMessage `json:"report,omitempty"`
}

// AddVault registers a vault with its token's hash and its read-only key.
func (q *Q) AddVault(ctx context.Context, v Vault, tokenHash string) error {
	_, err := q.db.Exec(ctx, `INSERT INTO vaults (name, token_sha256, s3_access_key_id, s3_secret, created_at, created_by) VALUES ($1, $2, $3, $4, $5, $6)`,
		v.Name, tokenHash, v.S3AccessKeyID, v.S3Secret, v.CreatedAt, v.CreatedBy)
	if isUniqueViolation(err) {
		return ErrConflict
	}
	return err
}

// RemoveVault forgets a vault: its token stops working.
func (q *Q) RemoveVault(ctx context.Context, name string) error {
	tag, err := q.db.Exec(ctx, `DELETE FROM vaults WHERE name = $1`, name)
	if err == nil && tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return err
}

const vaultCols = `name, s3_access_key_id, s3_secret, created_at, created_by, last_seen_at, reported_at, report`

func scanVault(row interface{ Scan(...any) error }) (Vault, error) {
	var v Vault
	var report []byte
	err := row.Scan(&v.Name, &v.S3AccessKeyID, &v.S3Secret, &v.CreatedAt, &v.CreatedBy, &v.LastSeenAt, &v.ReportedAt, &report)
	v.Report = report
	return v, notFound(err)
}

// VaultByToken returns the vault that owns a token hash.
func (q *Q) VaultByToken(ctx context.Context, tokenHash string) (Vault, error) {
	return scanVault(q.db.QueryRow(ctx, `SELECT `+vaultCols+` FROM vaults WHERE token_sha256 = $1`, tokenHash))
}

// ListVaults returns every vault, by name.
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

// RecordVaultReport keeps a vault's latest report, and when it was last seen.
func (q *Q) RecordVaultReport(ctx context.Context, name string, report []byte, now time.Time) error {
	_, err := q.db.Exec(ctx, `UPDATE vaults SET last_seen_at = $2, reported_at = $2, report = $3 WHERE name = $1`, name, now, report)
	return err
}

// VaultRequest is a restore from a vault, asked on this server.
type VaultRequest struct {
	ID          string          `json:"id"`
	Vault       string          `json:"vault"`
	FQDN        string          `json:"fqdn"`
	Snapshot    string          `json:"snapshot"` // the copy, in the vault
	Original    string          `json:"original"` // the snapshot here
	Spec        json.RawMessage `json:"spec"`
	RequestedBy string          `json:"requested_by"`
	RequestedAt time.Time       `json:"requested_at"`
	State       string          `json:"state"`
	AnsweredAt  *time.Time      `json:"answered_at,omitempty"`
	Error       string          `json:"error,omitempty"`
}

// AddVaultRequest asks a vault for a restore.
func (q *Q) AddVaultRequest(ctx context.Context, r VaultRequest) error {
	_, err := q.db.Exec(ctx, `INSERT INTO vault_requests (id, vault, fqdn, snapshot, original, spec, requested_by, requested_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`, r.ID, r.Vault, r.FQDN, r.Snapshot, r.Original, []byte(r.Spec), r.RequestedBy, r.RequestedAt)
	return err
}

const vaultRequestCols = `id, vault, fqdn, snapshot, original, spec, requested_by, requested_at, state, answered_at, error`

func (q *Q) vaultRequests(ctx context.Context, sql string, args ...any) ([]VaultRequest, error) {
	rows, err := q.db.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []VaultRequest{}
	for rows.Next() {
		var r VaultRequest
		var spec []byte
		if err := rows.Scan(&r.ID, &r.Vault, &r.FQDN, &r.Snapshot, &r.Original, &spec, &r.RequestedBy, &r.RequestedAt, &r.State, &r.AnsweredAt, &r.Error); err != nil {
			return nil, err
		}
		r.Spec = spec
		out = append(out, r)
	}
	return out, rows.Err()
}

// WaitingVaultRequests returns a vault's restores that wait for its grant.
func (q *Q) WaitingVaultRequests(ctx context.Context, vault string) ([]VaultRequest, error) {
	return q.vaultRequests(ctx, `SELECT `+vaultRequestCols+` FROM vault_requests WHERE vault = $1 AND state = 'waiting' ORDER BY requested_at`, vault)
}

// ExpireVaultRequests gives up on the restores no vault answered within ttl, and returns them.
func (q *Q) ExpireVaultRequests(ctx context.Context, now time.Time, ttl time.Duration) ([]VaultRequest, error) {
	return q.vaultRequests(ctx, `UPDATE vault_requests SET state = 'expired', answered_at = $1, error = 'the vault did not answer in time'
		WHERE state = 'waiting' AND requested_at < $2 RETURNING `+vaultRequestCols, now, now.Add(-ttl))
}

// GetVaultRequest returns one request.
func (q *Q) GetVaultRequest(ctx context.Context, id string) (VaultRequest, error) {
	rs, err := q.vaultRequests(ctx, `SELECT `+vaultRequestCols+` FROM vault_requests WHERE id = $1`, id)
	if err != nil {
		return VaultRequest{}, err
	}
	if len(rs) == 0 {
		return VaultRequest{}, ErrNotFound
	}
	return rs[0], nil
}

// AnswerVaultRequest records a grant or a refusal; a request answered already is a conflict.
func (q *Q) AnswerVaultRequest(ctx context.Context, id, vault, state, failure string, now time.Time) error {
	tag, err := q.db.Exec(ctx, `UPDATE vault_requests SET state = $3, error = $4, answered_at = $5 WHERE id = $1 AND vault = $2 AND state = 'waiting'`,
		id, vault, state, failure, now)
	if err == nil && tag.RowsAffected() == 0 {
		return ErrConflict
	}
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

// TouchVault records that a vault was seen, without a report.
func (q *Q) TouchVault(ctx context.Context, name string, now time.Time) error {
	_, err := q.db.Exec(ctx, `UPDATE vaults SET last_seen_at = $2 WHERE name = $1`, name, now)
	return err
}

// WaitingForVaults returns the restores that wait for a vault's grant, of every vault.
func (q *Q) WaitingForVaults(ctx context.Context) ([]VaultRequest, error) {
	return q.vaultRequests(ctx, `SELECT `+vaultRequestCols+` FROM vault_requests WHERE state = 'waiting' ORDER BY requested_at`)
}

/* ---------------- in the vault ---------------- */

// SaveVaultFeed keeps the source's last feed, sealed.
func (q *Q) SaveVaultFeed(ctx context.Context, sealed []byte, now time.Time) error {
	_, err := q.db.Exec(ctx, `INSERT INTO vault_feed (id, sealed, fetched_at) VALUES (1, $1, $2)
		ON CONFLICT (id) DO UPDATE SET sealed = $1, fetched_at = $2`, sealed, now)
	return err
}

// VaultFeed returns the source's last feed, sealed, and when it came; ErrNotFound before the first.
func (q *Q) VaultFeed(ctx context.Context) ([]byte, time.Time, error) {
	var b []byte
	var at time.Time
	err := q.db.QueryRow(ctx, `SELECT sealed, fetched_at FROM vault_feed WHERE id = 1`).Scan(&b, &at)
	return b, at, notFound(err)
}

// ActivateHost records a host the vault copies, or brings back one it had retired: it is active at once, as no
// agent enrolls for it.
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

// SetPrimaryDeleted records which of a host's source snapshots the source deleted, replacing the list.
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
