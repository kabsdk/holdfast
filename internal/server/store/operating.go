package store

import (
	"context"
	"time"

	"github.com/kabsdk/holdfast/internal/server/storage"
)

// M5: the escrow file in the bucket, the repository's size, rotating a host's password, and
// decommission.

// HostEscrow is a host's escrow/<fqdn>.age (decision 036).
type HostEscrow struct {
	// Current says the file holds the host's current credentials version, so its password.
	Current bool       `json:"current"`
	Version *int       `json:"credentials_version,omitempty"` // the version the file holds; nil: none written
	At      *time.Time `json:"written_at,omitempty"`
	Error   string     `json:"error,omitempty"` // the last write's failure
	TriedAt *time.Time `json:"-"`
}

// Decommission is a host's decommission: asked for, then done after the wait.
type Decommission struct {
	RequestedAt time.Time  `json:"requested_at"`
	RequestedBy string     `json:"requested_by"`
	DueAt       time.Time  `json:"due_at"`
	Error       string     `json:"error,omitempty"` // the last attempt's failure
	TriedAt     *time.Time `json:"-"`
	DoneAt      *time.Time `json:"done_at,omitempty"`
	Objects     int        `json:"objects_deleted,omitempty"`
	// GoneAt is when S3's old versions are expired by the bucket's lifecycle rule: the data is really gone.
	GoneAt *time.Time `json:"really_gone_at,omitempty"`
}

// RecordEscrow keeps the outcome of writing a host's escrow file: the version written, or the failure.
func (q *Q) RecordEscrow(ctx context.Context, fqdn string, version int, now time.Time, failure string) error {
	if failure != "" {
		_, err := q.db.Exec(ctx, `UPDATE hosts SET escrow_tried_at = $2, escrow_error = $3 WHERE fqdn = $1`, fqdn, now, failure)
		return err
	}
	_, err := q.db.Exec(ctx, `UPDATE hosts SET escrowed_version = $2, escrowed_at = $3, escrow_tried_at = $3, escrow_error = '' WHERE fqdn = $1`,
		fqdn, version, now)
	return err
}

// RecordSize keeps a measurement of a host's repository, or its failure.
func (q *Q) RecordSize(ctx context.Context, fqdn string, s storage.Size, now time.Time, failure string) error {
	if failure != "" {
		_, err := q.db.Exec(ctx, `UPDATE hosts SET size_tried_at = $2, size_error = $3 WHERE fqdn = $1`, fqdn, now, failure)
		return err
	}
	_, err := q.db.Exec(ctx, `UPDATE hosts SET size_at = $2, size_tried_at = $2, size_error = '', size_current_bytes = $3,
		size_noncurrent_bytes = $4, size_objects = $5 WHERE fqdn = $1`, fqdn, now, s.Current, s.Noncurrent, s.Objects)
	return err
}

// StartRotation records a host's new repository key and the old one, which is still to be removed. It runs in
// the transaction that stores the new password, before the old key is removed, so a crash in between leaves
// the old key on record.
func (q *Q) StartRotation(ctx context.Context, fqdn, newKey, oldKey string) error {
	_, err := q.db.Exec(ctx, `UPDATE hosts SET repo_key_retiring = $3,
		repo_keys_known = CASE WHEN $2 = ANY(repo_keys_known) THEN repo_keys_known ELSE array_append(repo_keys_known, $2) END,
		repo_keys_foreign = array_remove(repo_keys_foreign, $2)
		WHERE fqdn = $1`, fqdn, newKey, oldKey)
	return err
}

// FinishRotation records that the old key is gone from the repository.
func (q *Q) FinishRotation(ctx context.Context, fqdn, oldKey string) error {
	_, err := q.db.Exec(ctx, `UPDATE hosts SET repo_key_retiring = CASE WHEN repo_key_retiring = $2 THEN '' ELSE repo_key_retiring END,
		repo_keys_known = array_remove(repo_keys_known, $2),
		repo_keys_foreign = array_remove(repo_keys_foreign, $2) WHERE fqdn = $1`, fqdn, oldKey)
	return err
}

// RevokeAgent unpins an enrolled agent's certificate: the host waits for a new enrollment, and only enrolled hosts'
// certificates work. replacement is stored instead of the certificate's sha256, and matches no certificate.
func (q *Q) RevokeAgent(ctx context.Context, fqdn, replacement string) error {
	if _, err := q.db.Exec(ctx, `UPDATE agents SET credential_sha256 = $2 WHERE fqdn = $1`, fqdn, replacement); err != nil {
		return err
	}
	_, err := q.db.Exec(ctx, `UPDATE hosts SET state = 'awaiting_enrollment' WHERE fqdn = $1 AND state = 'enrolled'`, fqdn)
	return err
}

// RequestDecommission asks for a retired host's decommission, due at due. It refuses a host that is not
// retired, or whose decommission is asked for or done already.
func (q *Q) RequestDecommission(ctx context.Context, fqdn, by string, now, due time.Time) error {
	tag, err := q.db.Exec(ctx, `UPDATE hosts SET decommission_requested_at = $3, decommission_requested_by = $2, decommission_due_at = $4,
		decommission_tried_at = NULL, decommission_error = ''
		WHERE fqdn = $1 AND state = 'retired' AND decommission_requested_at IS NULL`, fqdn, by, now, due)
	if err == nil && tag.RowsAffected() == 0 {
		return ErrConflict
	}
	return err
}

// CancelDecommission cancels a decommission that has not run yet.
func (q *Q) CancelDecommission(ctx context.Context, fqdn string) error {
	tag, err := q.db.Exec(ctx, `UPDATE hosts SET decommission_requested_at = NULL, decommission_requested_by = '', decommission_due_at = NULL,
		decommission_tried_at = NULL, decommission_error = ''
		WHERE fqdn = $1 AND decommission_requested_at IS NOT NULL AND decommissioned_at IS NULL`, fqdn)
	if err == nil && tag.RowsAffected() == 0 {
		return ErrConflict
	}
	return err
}

// RecordDecommissionFailure keeps why a decommission could not be done yet.
func (q *Q) RecordDecommissionFailure(ctx context.Context, fqdn string, now time.Time, failure string) error {
	_, err := q.db.Exec(ctx, `UPDATE hosts SET decommission_tried_at = $2, decommission_error = $3 WHERE fqdn = $1`, fqdn, now, failure)
	return err
}

// MarkDecommissioned records that a host's repository and escrow file were deleted. Its snapshots get
// tombstones marked deleted by Holdfast, and what described the repository is cleared.
func (q *Q) MarkDecommissioned(ctx context.Context, fqdn string, now time.Time, objects int) error {
	if _, err := q.db.Exec(ctx, `UPDATE hosts SET decommissioned_at = $2, decommissioned_objects = $3, decommission_tried_at = $2,
		decommission_error = '', escrowed_version = NULL, escrowed_at = NULL, escrow_error = '', size_at = NULL, size_error = '',
		size_current_bytes = 0, size_noncurrent_bytes = 0, size_objects = 0, prune_needed = false, repo_key_retiring = '',
		repo_keys_known = '{}', repo_keys_foreign = '{}'
		WHERE fqdn = $1`, fqdn, now, objects); err != nil {
		return err
	}
	if _, err := q.db.Exec(ctx, `UPDATE snapshots SET deleted_by_holdfast = true, gone_at = $2 WHERE fqdn = $1 AND gone_at IS NULL`, fqdn, now); err != nil {
		return err
	}
	_, err := q.db.Exec(ctx, `DELETE FROM pending_deletions WHERE fqdn = $1`, fqdn)
	return err
}
