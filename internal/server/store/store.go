// Package store is holdfast-server's PostgreSQL state.
//
// Every method lives on *Q, which wraps either the pool or a transaction, so a caller can make
// several changes atomically with Store.InTx (for example: a revision, a host row, an enrollment
// code and an audit entry when a host is added).
package store

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kabsdk/holdfast/internal/config"
	"github.com/kabsdk/holdfast/internal/proto"
	"github.com/kabsdk/holdfast/internal/server/storage"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// Errors callers act on.
var (
	ErrNotFound = errors.New("not found")
	ErrConflict = errors.New("conflict: someone saved a newer revision; reload and edit again")
)

// Store is the connection pool.
type Store struct{ pool *pgxpool.Pool }

// Open connects to PostgreSQL.
func Open(ctx context.Context, dsn string) (*Store, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return &Store{pool: pool}, nil
}

// schemaName is a PostgreSQL schema Holdfast makes: lower-case letters and underscores.
var schemaName = regexp.MustCompile(`^[a-z_]{1,32}$`)

// OpenSchema connects to PostgreSQL with every table in schema, which it makes if it is not there. The off-site
// copies keep their own tables there, in the server's own database (decision 041), so nothing more is set up.
func OpenSchema(ctx context.Context, dsn, schema string) (*Store, error) {
	if !schemaName.MatchString(schema) {
		return nil, fmt.Errorf("schema %q is not a name Holdfast makes", schema)
	}
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return nil, err
	}
	_, err = conn.Exec(ctx, `CREATE SCHEMA IF NOT EXISTS `+schema)
	_ = conn.Close(ctx)
	if err != nil {
		return nil, fmt.Errorf("making schema %s: %w", schema, err)
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return &Store{pool: pool}, nil
}

// Close closes the pool.
func (s *Store) Close() { s.pool.Close() }

// Pool exposes the pool for LISTEN connections.
func (s *Store) Pool() *pgxpool.Pool { return s.pool }

type db interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Q runs queries on the pool or inside a transaction.
type Q struct{ db db }

// Q returns a query runner on the pool.
func (s *Store) Q() *Q { return &Q{db: s.pool} }

// InTx runs fn in one transaction, committing only if fn returns nil.
func (s *Store) InTx(ctx context.Context, fn func(q *Q) error) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error { return fn(&Q{db: tx}) })
}

// Migrate applies the embedded migrations that have not run yet, under an advisory lock so two
// servers starting at once do not race.
func (s *Store) Migrate(ctx context.Context) error { return s.MigrateTo(ctx, 0) }

// MigrateTo applies the migrations that have not run yet up to version max, or all of them when max is 0. Restoring an
// export migrates to the export's version first, loads it, then migrates on.
func (s *Store) MigrateTo(ctx context.Context, max int) error {
	names, err := fs.Glob(migrationFS, "migrations/*.sql")
	if err != nil {
		return err
	}
	sort.Strings(names)
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(4207735001)`); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (version integer PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
			return err
		}
		for _, name := range names {
			v, err := strconv.Atoi(strings.SplitN(strings.TrimPrefix(name, "migrations/"), "_", 2)[0])
			if err != nil {
				return fmt.Errorf("migration %s: name must start with a number", name)
			}
			if max > 0 && v > max {
				break
			}
			var done bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = $1)`, v).Scan(&done); err != nil {
				return err
			}
			if done {
				continue
			}
			sql, err := migrationFS.ReadFile(name)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, string(sql)); err != nil {
				return fmt.Errorf("migration %s: %w", name, err)
			}
			if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations (version) VALUES ($1)`, v); err != nil {
				return err
			}
		}
		return nil
	})
}

func isUniqueViolation(err error) bool {
	var pe *pgconn.PgError
	return errors.As(err, &pe) && pe.Code == "23505"
}

func notFound(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

/* ---------------- revisions ---------------- */

// Revision is one saved config.
type Revision struct {
	Number    int64           `json:"number"`
	CreatedAt time.Time       `json:"created_at"`
	Actor     string          `json:"actor"`
	Source    string          `json:"source"`
	Message   string          `json:"message"`
	Accepted  []string        `json:"accepted_codes"`
	SHA256    string          `json:"sha256"`
	Config    config.Document `json:"-"`
}

// SaveRevision is the input for a new revision. HostSHAs holds, for every host in Config, the
// sha256 of its rendered agent config; the store records a host_desired row for each host whose
// sha256 changed (and a NULL row for each host that left the config).
type SaveRevision struct {
	Base     int64 // the revision the change was made on; 0 for the first
	Actor    string
	Source   string
	Message  string
	Accepted []string
	Config   config.Document
	HostSHAs map[string]string
}

// SaveRevision inserts revision Base+1. If someone else saved Base+1 first it returns ErrConflict.
func (q *Q) SaveRevision(ctx context.Context, in SaveRevision) (Revision, error) {
	canon, err := in.Config.Canonical()
	if err != nil {
		return Revision{}, err
	}
	sum, _ := in.Config.SHA256()
	accepted := in.Accepted
	if accepted == nil {
		accepted = []string{}
	}
	r := Revision{Number: in.Base + 1, Actor: in.Actor, Source: in.Source, Message: in.Message, Accepted: accepted,
		SHA256: fmt.Sprintf("%x", sum), Config: in.Config}
	err = q.db.QueryRow(ctx, `INSERT INTO revisions (number, actor, source, message, accepted_codes, config, config_sha256)
		VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING created_at`,
		r.Number, r.Actor, r.Source, r.Message, accepted, string(canon), sum[:]).Scan(&r.CreatedAt)
	if isUniqueViolation(err) {
		return Revision{}, ErrConflict
	}
	if err != nil {
		return Revision{}, err
	}
	current, err := q.desiredAll(ctx)
	if err != nil {
		return Revision{}, err
	}
	for fqdn, sha := range in.HostSHAs {
		if old, ok := current[fqdn]; !ok || old != sha {
			if _, err := q.db.Exec(ctx, `INSERT INTO host_desired (fqdn, revision, sha256) VALUES ($1, $2, $3)`, fqdn, r.Number, sha); err != nil {
				return Revision{}, err
			}
		}
	}
	for fqdn := range current {
		if _, still := in.HostSHAs[fqdn]; !still {
			if _, err := q.db.Exec(ctx, `INSERT INTO host_desired (fqdn, revision, sha256) VALUES ($1, $2, NULL)`, fqdn, r.Number); err != nil {
				return Revision{}, err
			}
		}
	}
	return r, nil
}

// desiredAll returns the current sha256 of every host still in the config.
func (q *Q) desiredAll(ctx context.Context) (map[string]string, error) {
	rows, err := q.db.Query(ctx, `SELECT DISTINCT ON (fqdn) fqdn, sha256 FROM host_desired ORDER BY fqdn, revision DESC`)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for rows.Next() {
		var fqdn string
		var sha *string
		if err := rows.Scan(&fqdn, &sha); err != nil {
			return nil, err
		}
		if sha != nil {
			out[fqdn] = *sha
		}
	}
	return out, rows.Err()
}

// HostDesired returns the revision in which the host's agent config last changed, and its sha256.
func (q *Q) HostDesired(ctx context.Context, fqdn string) (int64, string, error) {
	var rev int64
	var sha *string
	err := q.db.QueryRow(ctx, `SELECT revision, sha256 FROM host_desired WHERE fqdn = $1 ORDER BY revision DESC LIMIT 1`, fqdn).Scan(&rev, &sha)
	if err != nil {
		return 0, "", notFound(err)
	}
	if sha == nil {
		return rev, "", ErrNotFound
	}
	return rev, *sha, nil
}

func scanRevision(row pgx.Row, withConfig bool) (Revision, error) {
	var r Revision
	var sum []byte
	var cfg string
	dest := []any{&r.Number, &r.CreatedAt, &r.Actor, &r.Source, &r.Message, &r.Accepted, &sum}
	if withConfig {
		dest = append(dest, &cfg)
	}
	if err := row.Scan(dest...); err != nil {
		return Revision{}, notFound(err)
	}
	r.SHA256 = fmt.Sprintf("%x", sum)
	if withConfig {
		d, err := config.Parse([]byte(cfg))
		if err != nil {
			return Revision{}, fmt.Errorf("revision %d: %w", r.Number, err)
		}
		r.Config = d
	}
	return r, nil
}

const revCols = `number, created_at, actor, source, message, accepted_codes, config_sha256`

// Head returns the newest revision with its config, or ErrNotFound if there is none yet.
func (q *Q) Head(ctx context.Context) (Revision, error) {
	return scanRevision(q.db.QueryRow(ctx, `SELECT `+revCols+`, config::text FROM revisions ORDER BY number DESC LIMIT 1`), true)
}

// GetRevision returns one revision with its config.
func (q *Q) GetRevision(ctx context.Context, n int64) (Revision, error) {
	return scanRevision(q.db.QueryRow(ctx, `SELECT `+revCols+`, config::text FROM revisions WHERE number = $1`, n), true)
}

// ListRevisions returns up to limit revisions, newest first, without their configs.
func (q *Q) ListRevisions(ctx context.Context, limit int) ([]Revision, error) {
	rows, err := q.db.Query(ctx, `SELECT `+revCols+` FROM revisions ORDER BY number DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Revision
	for rows.Next() {
		r, err := scanRevision(rows, false)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

/* ---------------- hosts and enrollment ---------------- */

// Host is a host's lifecycle state.
type Host struct {
	FQDN          string     `json:"fqdn"`
	State         string     `json:"state"`
	AddedRevision int64      `json:"added_revision"`
	AddedAt       time.Time  `json:"added_at"`
	ConnectedAt   *time.Time `json:"connected_at,omitempty"`
	FirstBackupAt *time.Time `json:"first_backup_at,omitempty"`
	// Storage is the last storage contract test (decision 029); nil before the first.
	Storage *storage.Report `json:"storage,omitempty"`
	// CatalogueRefreshedAt is when the snapshot catalogue last read the repository (M2).
	CatalogueRefreshedAt *time.Time `json:"catalogue_refreshed_at,omitempty"`
	CatalogueError       string     `json:"catalogue_error,omitempty"`
	// PruneNeeded is set when snapshots were forgotten and not pruned yet (M3).
	PruneNeeded bool `json:"prune_needed"`
	// RemovedAt is when the host was removed (M4). It is retired once its agent has applied the empty
	// config, or at once if it never enrolled.
	RemovedAt *time.Time `json:"removed_at,omitempty"`
	// M5 (operating.go): the escrow file, the repository's size, a rotation not finished, decommission.
	Escrow          HostEscrow    `json:"escrow"`
	Size            *storage.Size `json:"size,omitempty"` // nil before the first measurement
	SizeError       string        `json:"size_error,omitempty"`
	SizeTriedAt     *time.Time    `json:"-"`
	RepoKeyRetiring string        `json:"repo_key_retiring,omitempty"`
	Decommission    *Decommission `json:"decommission,omitempty"`
	// In a vault (M7): its last copy from the source, and why the last attempt failed, if it did.
	CopiedAt  *time.Time `json:"copied_at,omitempty"`
	CopyError string     `json:"copy_error,omitempty"`
}

// MarkRemoved records that a host was removed. A host that never enrolled has no agent to tell, so it is
// retired at once.
func (q *Q) MarkRemoved(ctx context.Context, fqdn string, now time.Time) error {
	_, err := q.db.Exec(ctx, `UPDATE hosts SET retired_at = $2, state = CASE WHEN state = 'awaiting_enrollment' THEN 'retired' ELSE state END
		WHERE fqdn = $1`, fqdn, now)
	return err
}

// RetireHost retires a removed host. Its agent's certificate stops working: only enrolled hosts' certificates do.
func (q *Q) RetireHost(ctx context.Context, fqdn string) error {
	_, err := q.db.Exec(ctx, `UPDATE hosts SET state = 'retired' WHERE fqdn = $1 AND retired_at IS NOT NULL`, fqdn)
	return err
}

// ReviveHost brings a removed host back, waiting for a new enrollment. Its credentials and its catalogue
// stay. A decommissioned host's record of it is cleared: it starts over with a new repository.
func (q *Q) ReviveHost(ctx context.Context, fqdn string, rev int64) error {
	_, err := q.db.Exec(ctx, `UPDATE hosts SET state = 'awaiting_enrollment', retired_at = NULL, added_revision = $2,
		decommission_requested_at = NULL, decommission_requested_by = '', decommission_due_at = NULL, decommission_tried_at = NULL,
		decommission_error = '', decommissioned_at = NULL, decommissioned_objects = 0
		WHERE fqdn = $1`, fqdn, rev)
	return err
}

// CreateHost records a new host. A host that exists already (even retired) is refused.
func (q *Q) CreateHost(ctx context.Context, fqdn string, rev int64, now time.Time) error {
	_, err := q.db.Exec(ctx, `INSERT INTO hosts (fqdn, state, added_revision, added_at) VALUES ($1, 'awaiting_enrollment', $2, $3)`, fqdn, rev, now)
	if isUniqueViolation(err) {
		return fmt.Errorf("host %s exists already: %w", fqdn, ErrConflict)
	}
	return err
}

const hostCols = `fqdn, state, added_revision, added_at, connected_at, first_backup_at, storage_checked_at, storage_findings,
	storage_noncurrent_days, catalogue_refreshed_at, catalogue_error, prune_needed, retired_at,
	(SELECT c.version FROM host_credentials c WHERE c.fqdn = hosts.fqdn), escrowed_version, escrowed_at, escrow_tried_at, escrow_error,
	size_at, size_tried_at, size_current_bytes, size_noncurrent_bytes, size_objects, size_error, repo_key_retiring,
	decommission_requested_at, decommission_requested_by, decommission_due_at, decommission_tried_at, decommission_error,
	decommissioned_at, decommissioned_objects, copied_at, copy_error`

func scanHost(row pgx.Row) (Host, error) {
	var h Host
	var checked, sizeAt *time.Time
	var findings []byte
	var days int
	var credVersion *int
	var sz storage.Size
	var d Decommission
	var dReq, dDue *time.Time
	err := row.Scan(&h.FQDN, &h.State, &h.AddedRevision, &h.AddedAt, &h.ConnectedAt, &h.FirstBackupAt, &checked, &findings,
		&days, &h.CatalogueRefreshedAt, &h.CatalogueError, &h.PruneNeeded, &h.RemovedAt,
		&credVersion, &h.Escrow.Version, &h.Escrow.At, &h.Escrow.TriedAt, &h.Escrow.Error,
		&sizeAt, &h.SizeTriedAt, &sz.Current, &sz.Noncurrent, &sz.Objects, &h.SizeError, &h.RepoKeyRetiring,
		&dReq, &d.RequestedBy, &dDue, &d.TriedAt, &d.Error, &d.DoneAt, &d.Objects, &h.CopiedAt, &h.CopyError)
	if err != nil {
		return h, notFound(err)
	}
	if checked != nil {
		h.Storage = &storage.Report{CheckedAt: *checked, Findings: []storage.Finding{}, NoncurrentDays: days}
		if len(findings) > 0 {
			err = json.Unmarshal(findings, &h.Storage.Findings)
		}
	}
	h.Escrow.Current = credVersion != nil && h.Escrow.Version != nil && *credVersion == *h.Escrow.Version
	if sizeAt != nil {
		sz.At = *sizeAt
		h.Size = &sz
	}
	if dReq != nil {
		d.RequestedAt, d.DueAt = *dReq, *dDue
		if d.DoneAt != nil && days > 0 {
			gone := d.DoneAt.AddDate(0, 0, days)
			d.GoneAt = &gone
		}
		h.Decommission = &d
	}
	return h, err
}

// RecordStorageCheck keeps a host's latest storage contract test.
func (q *Q) RecordStorageCheck(ctx context.Context, fqdn string, r storage.Report) error {
	findings := r.Findings
	if findings == nil {
		findings = []storage.Finding{}
	}
	b, err := json.Marshal(findings)
	if err != nil {
		return err
	}
	_, err = q.db.Exec(ctx, `UPDATE hosts SET storage_checked_at = $2, storage_findings = $3, storage_noncurrent_days = $4 WHERE fqdn = $1`,
		fqdn, r.CheckedAt, b, r.NoncurrentDays)
	return err
}

// GetHost returns one host.
func (q *Q) GetHost(ctx context.Context, fqdn string) (Host, error) {
	return scanHost(q.db.QueryRow(ctx, `SELECT `+hostCols+` FROM hosts WHERE fqdn = $1`, fqdn))
}

// ListHosts returns every host, sorted by FQDN.
func (q *Q) ListHosts(ctx context.Context) ([]Host, error) {
	rows, err := q.db.Query(ctx, `SELECT `+hostCols+` FROM hosts ORDER BY fqdn`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Host
	for rows.Next() {
		h, err := scanHost(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// CreateEnrollmentCode stores a new code for the host and revokes any open one.
func (q *Q) CreateEnrollmentCode(ctx context.Context, id, fqdn, codeHash string, now, expires time.Time, by string) error {
	if _, err := q.db.Exec(ctx, `UPDATE enrollment_codes SET revoked_at = $2 WHERE fqdn = $1 AND used_at IS NULL AND revoked_at IS NULL`, fqdn, now); err != nil {
		return err
	}
	_, err := q.db.Exec(ctx, `INSERT INTO enrollment_codes (id, fqdn, code_sha256, created_at, expires_at, created_by) VALUES ($1, $2, $3, $4, $5, $6)`,
		id, fqdn, codeHash, now, expires, by)
	return err
}

// Enrollment refusals. The agent sees one generic message; the audit log keeps the reason.
var (
	ErrCodeInvalid = errors.New("enrollment code is not valid for this host")
	ErrCodeExpired = errors.New("enrollment code has expired")
	ErrCodeUsed    = errors.New("enrollment code has been used already")
	ErrCodeRevoked = errors.New("enrollment code was replaced by a newer one")
)

// CheckEnrollmentCode reports whether the code is valid for the host at now, without using it.
// Enrollment checks it before it touches storage, so a stranger who knows only an FQDN causes no S3
// traffic (decision 030).
func (q *Q) CheckEnrollmentCode(ctx context.Context, fqdn, codeHash string, now time.Time) error {
	return q.enrollmentCode(ctx, fqdn, codeHash, now, "")
}

// RedeemEnrollmentCode marks the code used if it is valid for the host at now.
func (q *Q) RedeemEnrollmentCode(ctx context.Context, fqdn, codeHash string, now time.Time) error {
	if err := q.enrollmentCode(ctx, fqdn, codeHash, now, " FOR UPDATE"); err != nil {
		return err
	}
	_, err := q.db.Exec(ctx, `UPDATE enrollment_codes SET used_at = $2 WHERE code_sha256 = $1`, codeHash, now)
	return err
}

func (q *Q) enrollmentCode(ctx context.Context, fqdn, codeHash string, now time.Time, lock string) error {
	var expires time.Time
	var used, revoked *time.Time
	var codeHost string
	err := q.db.QueryRow(ctx, `SELECT fqdn, expires_at, used_at, revoked_at FROM enrollment_codes WHERE code_sha256 = $1`+lock, codeHash).
		Scan(&codeHost, &expires, &used, &revoked)
	switch {
	case errors.Is(err, pgx.ErrNoRows), err == nil && codeHost != fqdn:
		return ErrCodeInvalid
	case err != nil:
		return err
	case used != nil:
		return ErrCodeUsed
	case revoked != nil:
		return ErrCodeRevoked
	case !now.Before(expires):
		return ErrCodeExpired
	}
	return nil
}

/* ---------------- agents ---------------- */

// Agent is the server's view of one host's agent.
type Agent struct {
	FQDN               string               `json:"fqdn"`
	EnrolledAt         time.Time            `json:"enrolled_at"`
	LastSeenAt         *time.Time           `json:"last_seen_at,omitempty"`
	AgentVersion       string               `json:"agent_version"`
	ResticVersion      string               `json:"restic_version"`
	OSID               string               `json:"os_id"`
	OSVersion          string               `json:"os_version"`
	HostnameFQDN       string               `json:"hostname_fqdn"`
	Timezone           string               `json:"timezone"`
	ClockOffsetMS      int64                `json:"clock_offset_ms"`
	AppliedRevision    int64                `json:"applied_revision"`
	AppliedSHA256      string               `json:"applied_sha256"`
	AppliedAt          *time.Time           `json:"applied_at,omitempty"`
	CredentialsVersion int                  `json:"credentials_version"`
	RestoreBounds      *proto.RestoreBounds `json:"restore_bounds,omitempty"` // the host's own limits
	// Protocol is the agent protocol it last reported; 0 from an agent before M5, which speaks 1.
	Protocol int `json:"protocol"`
	// Dumps held the dumps an agent before 0.6.0 reported; no agent reports them since dump jobs were removed
	// (decision 052). The column stays, empty, and is not shown.
	Dumps []string `json:"-"`
	// Paused is set while the agent reports a pause, and idle.
	Paused *proto.Paused `json:"paused,omitempty"`
}

// EnrollAgent pins (or replaces, on re-enrollment) the agent's certificate, by its sha256, and marks the host
// enrolled (decision 041). A certificate another host is pinned to is a conflict.
func (q *Q) EnrollAgent(ctx context.Context, fqdn, certificate string, now time.Time) error {
	if _, err := q.db.Exec(ctx, `INSERT INTO agents (fqdn, credential_kind, credential_sha256, enrolled_at, last_seen_at)
		VALUES ($1, 'certificate', $2, $3, $3)
		ON CONFLICT (fqdn) DO UPDATE SET credential_kind = 'certificate', credential_sha256 = $2, enrolled_at = $3, last_seen_at = $3`,
		fqdn, certificate, now); err != nil {
		if isUniqueViolation(err) {
			return fmt.Errorf("another host is enrolled with this certificate: %w", ErrConflict)
		}
		return err
	}
	_, err := q.db.Exec(ctx, `UPDATE hosts SET state = 'enrolled', connected_at = COALESCE(connected_at, $2) WHERE fqdn = $1`, fqdn, now)
	return err
}

// AgentByCertificate returns the FQDN of the enrolled host pinned to a certificate's sha256. A token from before
// decision 041 matches nothing: it was stored as another kind.
func (q *Q) AgentByCertificate(ctx context.Context, certificate string) (string, error) {
	var fqdn string
	err := q.db.QueryRow(ctx, `SELECT a.fqdn FROM agents a JOIN hosts h ON h.fqdn = a.fqdn
		WHERE a.credential_sha256 = $1 AND a.credential_kind = 'certificate' AND h.state = 'enrolled'`, certificate).Scan(&fqdn)
	return fqdn, notFound(err)
}

// Heartbeat records a poll: facts, what the agent runs, and when it was seen.
type Heartbeat struct {
	AgentVersion, ResticVersion, OSID, OSVersion, HostnameFQDN, Timezone string
	ClockOffsetMS                                                        int64
	AppliedRevision                                                      int64
	AppliedSHA256                                                        string
	CredentialsVersion                                                   int
	RestoreBounds                                                        *proto.RestoreBounds
	Protocol                                                             int
	Dumps                                                                []string
	Paused                                                               *proto.Paused
}

// RecordHeartbeat updates the agent row. applied_at moves only when the applied sha256 changes.
func (q *Q) RecordHeartbeat(ctx context.Context, fqdn string, h Heartbeat, now time.Time) error {
	var bounds any
	if h.RestoreBounds != nil {
		b, err := json.Marshal(h.RestoreBounds)
		if err != nil {
			return err
		}
		bounds = b
	}
	pausedCommand, pausedUntil := "", (*time.Time)(nil)
	if h.Paused != nil {
		pausedCommand, pausedUntil = h.Paused.Command, &h.Paused.Until
	}
	_, err := q.db.Exec(ctx, `UPDATE agents SET last_seen_at = $2, agent_version = $3, restic_version = $4, os_id = $5, os_version = $6,
		hostname_fqdn = $7, timezone = $8, clock_offset_ms = $9,
		applied_at = CASE WHEN applied_sha256 IS DISTINCT FROM $11 THEN $2 ELSE applied_at END,
		applied_revision = $10, applied_sha256 = $11, credentials_version = $12, restore_bounds = COALESCE($13, restore_bounds),
		protocol = $14, dumps = $15, paused_command = $16, paused_until = $17
		WHERE fqdn = $1`,
		fqdn, now, h.AgentVersion, h.ResticVersion, h.OSID, h.OSVersion, h.HostnameFQDN, h.Timezone, h.ClockOffsetMS,
		h.AppliedRevision, h.AppliedSHA256, h.CredentialsVersion, bounds, h.Protocol, nonNil(h.Dumps), pausedCommand, pausedUntil)
	return err
}

const agentCols = `fqdn, enrolled_at, last_seen_at, agent_version, restic_version, os_id, os_version, hostname_fqdn, timezone,
	clock_offset_ms, applied_revision, applied_sha256, applied_at, credentials_version, restore_bounds, protocol, dumps,
	paused_command, paused_until`

func scanAgent(row pgx.Row) (Agent, error) {
	var a Agent
	var bounds []byte
	var pausedCommand string
	var pausedUntil *time.Time
	err := row.Scan(&a.FQDN, &a.EnrolledAt, &a.LastSeenAt, &a.AgentVersion, &a.ResticVersion, &a.OSID, &a.OSVersion, &a.HostnameFQDN,
		&a.Timezone, &a.ClockOffsetMS, &a.AppliedRevision, &a.AppliedSHA256, &a.AppliedAt, &a.CredentialsVersion, &bounds, &a.Protocol, &a.Dumps,
		&pausedCommand, &pausedUntil)
	if err == nil && pausedCommand != "" && pausedUntil != nil {
		a.Paused = &proto.Paused{Command: pausedCommand, Until: *pausedUntil}
	}
	if err == nil && len(bounds) > 0 {
		a.RestoreBounds = &proto.RestoreBounds{}
		err = json.Unmarshal(bounds, a.RestoreBounds)
	}
	return a, notFound(err)
}

// GetAgent returns one agent.
func (q *Q) GetAgent(ctx context.Context, fqdn string) (Agent, error) {
	return scanAgent(q.db.QueryRow(ctx, `SELECT `+agentCols+` FROM agents WHERE fqdn = $1`, fqdn))
}

// ListAgents returns every agent.
func (q *Q) ListAgents(ctx context.Context) ([]Agent, error) {
	rows, err := q.db.Query(ctx, `SELECT `+agentCols+` FROM agents ORDER BY fqdn`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Agent
	for rows.Next() {
		a, err := scanAgent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

/* ---------------- credentials ---------------- */

// SealedCredentials is a host's credentials as stored: the repository and its password, which is ciphertext. A host
// has no S3 key (decision 041).
type SealedCredentials struct {
	FQDN         string
	Version      int
	Repository   string
	RepoPassword []byte
	CreatedAt    time.Time
}

// PutCredentials stores a host's credentials, replacing any older version.
func (q *Q) PutCredentials(ctx context.Context, c SealedCredentials) error {
	_, err := q.db.Exec(ctx, `INSERT INTO host_credentials (fqdn, version, repository, repo_password, created_at)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (fqdn) DO UPDATE SET version = $2, repository = $3, repo_password = $4, created_at = $5`,
		c.FQDN, c.Version, c.Repository, c.RepoPassword, c.CreatedAt)
	return err
}

const credCols = `fqdn, version, repository, repo_password, created_at`

func scanCreds(row pgx.Row) (SealedCredentials, error) {
	var c SealedCredentials
	err := row.Scan(&c.FQDN, &c.Version, &c.Repository, &c.RepoPassword, &c.CreatedAt)
	return c, notFound(err)
}

// GetCredentials returns a host's sealed credentials.
func (q *Q) GetCredentials(ctx context.Context, fqdn string) (SealedCredentials, error) {
	return scanCreds(q.db.QueryRow(ctx, `SELECT `+credCols+` FROM host_credentials WHERE fqdn = $1`, fqdn))
}

// AllCredentials returns every host's sealed credentials.
func (q *Q) AllCredentials(ctx context.Context) ([]SealedCredentials, error) {
	rows, err := q.db.Query(ctx, `SELECT `+credCols+` FROM host_credentials ORDER BY fqdn`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SealedCredentials
	for rows.Next() {
		c, err := scanCreds(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

/* ---------------- events ---------------- */

// Event is a stored run result.
type Event struct {
	FQDN         string            `json:"fqdn"`
	ID           string            `json:"id"`
	Kind         string            `json:"kind"`
	JobID        string            `json:"job_id"`
	CommandID    string            `json:"command_id,omitempty"`
	Status       string            `json:"status"`
	ScheduledFor *time.Time        `json:"scheduled_for,omitempty"`
	SpreadDelayS int               `json:"spread_delay_s"`
	Started      time.Time         `json:"started"`
	Finished     time.Time         `json:"finished"`
	Revision     int64             `json:"revision"`
	SnapshotID   string            `json:"snapshot_id,omitempty"`
	FilesNew     int64             `json:"files_new"`
	FilesChanged int64             `json:"files_changed"`
	BytesAdded   int64             `json:"bytes_added"`
	TotalFiles   int64             `json:"total_files"`
	TotalBytes   int64             `json:"total_bytes"`
	ResticExit   int               `json:"restic_exit"`
	Error        string            `json:"error,omitempty"`
	Detail       map[string]string `json:"detail,omitempty"`
	// InitiatedBy is who asked for the run, such as "Back up now"; empty for a scheduled one (M4).
	InitiatedBy string `json:"initiated_by,omitempty"`
}

// InsertEvent stores an event once. It returns false if the event was stored before (a re-sent
// event), in which case nothing else changes. A new event updates the job's status and the counters.
func (q *Q) InsertEvent(ctx context.Context, e Event) (bool, error) {
	detail := e.Detail
	if detail == nil {
		detail = map[string]string{}
	}
	detailJSON, err := json.Marshal(detail)
	if err != nil {
		return false, err
	}
	tag, err := q.db.Exec(ctx, `INSERT INTO events (fqdn, id, kind, job_id, command_id, status, scheduled_for, spread_delay_s, started_at, finished_at,
		revision, snapshot_id, files_new, files_changed, bytes_added, total_files, total_bytes, restic_exit, error, detail)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20) ON CONFLICT (fqdn, id) DO NOTHING`,
		e.FQDN, e.ID, e.Kind, e.JobID, e.CommandID, e.Status, e.ScheduledFor, e.SpreadDelayS, e.Started, e.Finished,
		e.Revision, e.SnapshotID, e.FilesNew, e.FilesChanged, e.BytesAdded, e.TotalFiles, e.TotalBytes, e.ResticExit, e.Error, detailJSON)
	if err != nil || tag.RowsAffected() == 0 {
		return false, err
	}
	if _, err := q.db.Exec(ctx, `INSERT INTO run_counters (fqdn, kind, job_id, status, count) VALUES ($1, $2, $3, $4, 1)
		ON CONFLICT (fqdn, kind, job_id, status) DO UPDATE SET count = run_counters.count + 1`, e.FQDN, e.Kind, e.JobID, e.Status); err != nil {
		return true, err
	}
	if e.Kind == "backup" && e.JobID != "" && e.Status != "skipped" {
		// The latest report wins. The agent sends its reports in the order its runs end, so the order they
		// arrive in is the order of the runs. The agent's clock does not decide it (decision 030).
		ok := e.Status == "success" || e.Status == "warning"
		if _, err := q.db.Exec(ctx, `INSERT INTO job_status (fqdn, job_id, last_run_at, last_status, last_error, last_success_at, last_snapshot_id)
			VALUES ($1, $2, $3, $4, $5, CASE WHEN $6 THEN $3::timestamptz END, CASE WHEN $6 THEN $7 ELSE '' END)
			ON CONFLICT (fqdn, job_id) DO UPDATE SET
			  last_run_at = EXCLUDED.last_run_at,
			  last_status = EXCLUDED.last_status,
			  last_error = EXCLUDED.last_error,
			  last_success_at = CASE WHEN $6 THEN EXCLUDED.last_run_at ELSE job_status.last_success_at END,
			  last_snapshot_id = CASE WHEN $6 THEN $7 ELSE job_status.last_snapshot_id END`,
			e.FQDN, e.JobID, e.Finished, e.Status, e.Error, ok, e.SnapshotID); err != nil {
			return true, err
		}
		if ok {
			if _, err := q.db.Exec(ctx, `UPDATE hosts SET first_backup_at = COALESCE(first_backup_at, $2) WHERE fqdn = $1`, e.FQDN, e.Finished); err != nil {
				return true, err
			}
		}
	}
	return true, nil
}

// RunTimes returns how long a job's last backups took, newest first, at most limit of them. Runs that did
// not finish (failed, interrupted or skipped) do not count (S002, spec §4.5).
func (q *Q) RunTimes(ctx context.Context, fqdn, jobID string, limit int) ([]time.Duration, error) {
	rows, err := q.db.Query(ctx, `SELECT extract(epoch FROM finished_at - started_at)::float8 FROM events
		WHERE fqdn = $1 AND job_id = $2 AND kind = 'backup' AND status IN ('success','warning')
		ORDER BY finished_at DESC LIMIT $3`, fqdn, jobID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []time.Duration
	for rows.Next() {
		var secs float64
		if err := rows.Scan(&secs); err != nil {
			return nil, err
		}
		out = append(out, time.Duration(secs*float64(time.Second)))
	}
	return out, rows.Err()
}

// ListEvents returns a host's events, newest first.
func (q *Q) ListEvents(ctx context.Context, fqdn string, limit int) ([]Event, error) {
	return q.queryEvents(ctx, `WHERE fqdn = $1 ORDER BY finished_at DESC, received_at DESC LIMIT $2`, fqdn, limit)
}

// Outcome is how one backup ended, for the host page's strip of days.
type Outcome struct {
	JobID    string
	Status   string
	Finished time.Time
}

// BackupOutcomes returns how a host's backups that finished since a time ended, oldest first.
func (q *Q) BackupOutcomes(ctx context.Context, fqdn string, since time.Time) ([]Outcome, error) {
	rows, err := q.db.Query(ctx, `SELECT job_id, status, finished_at FROM events
		WHERE fqdn = $1 AND kind = 'backup' AND finished_at >= $2 ORDER BY finished_at`, fqdn, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Outcome
	for rows.Next() {
		var o Outcome
		if err := rows.Scan(&o.JobID, &o.Status, &o.Finished); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// FirstBackups returns when each of a host's jobs first finished a backup, of any result.
func (q *Q) FirstBackups(ctx context.Context, fqdn string) (map[string]time.Time, error) {
	rows, err := q.db.Query(ctx, `SELECT job_id, min(finished_at) FROM events WHERE fqdn = $1 AND kind = 'backup' GROUP BY job_id`, fqdn)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]time.Time{}
	for rows.Next() {
		var id string
		var t time.Time
		if err := rows.Scan(&id, &t); err != nil {
			return nil, err
		}
		out[id] = t
	}
	return out, rows.Err()
}

// RestoreEvent returns the result of the restore that a command asked for, or ErrNotFound while it has not
// been reported.
func (q *Q) RestoreEvent(ctx context.Context, fqdn, commandID string) (Event, error) {
	evs, err := q.queryEvents(ctx, `WHERE fqdn = $1 AND command_id = $2 AND kind = 'restore' ORDER BY finished_at DESC LIMIT 1`, fqdn, commandID)
	if err != nil {
		return Event{}, err
	}
	if len(evs) == 0 {
		return Event{}, ErrNotFound
	}
	return evs[0], nil
}

// ListRestores returns the restores of every host, newest first (spec §5.5).
func (q *Q) ListRestores(ctx context.Context, limit int) ([]Event, error) {
	return q.queryEvents(ctx, `WHERE kind = 'restore' ORDER BY finished_at DESC, received_at DESC LIMIT $1`, limit)
}

func (q *Q) queryEvents(ctx context.Context, where string, args ...any) ([]Event, error) {
	// Who started a run: the person whose command it ran, or nobody for a scheduled one.
	rows, err := q.db.Query(ctx, `SELECT fqdn, id, kind, job_id, command_id, status, scheduled_for, spread_delay_s, started_at, finished_at,
		revision, snapshot_id, files_new, files_changed, bytes_added, total_files, total_bytes, restic_exit, error, detail,
		COALESCE((SELECT c.created_by FROM commands c WHERE c.id::text = events.command_id AND events.command_id <> ''), '')
		FROM events `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		var detail []byte
		if err := rows.Scan(&e.FQDN, &e.ID, &e.Kind, &e.JobID, &e.CommandID, &e.Status, &e.ScheduledFor, &e.SpreadDelayS, &e.Started, &e.Finished,
			&e.Revision, &e.SnapshotID, &e.FilesNew, &e.FilesChanged, &e.BytesAdded, &e.TotalFiles, &e.TotalBytes, &e.ResticExit, &e.Error, &detail,
			&e.InitiatedBy); err != nil {
			return nil, err
		}
		if len(detail) > 2 {
			_ = json.Unmarshal(detail, &e.Detail)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// JobStatus is the latest state of one job, built only from events.
type JobStatus struct {
	FQDN           string     `json:"fqdn"`
	JobID          string     `json:"job_id"`
	LastRunAt      *time.Time `json:"last_run_at,omitempty"`
	LastStatus     string     `json:"last_status"`
	LastError      string     `json:"last_error,omitempty"`
	LastSuccessAt  *time.Time `json:"last_success_at,omitempty"`
	LastSnapshotID string     `json:"last_snapshot_id,omitempty"`
}

// ListJobStatus returns the status of every job that has reported.
func (q *Q) ListJobStatus(ctx context.Context) ([]JobStatus, error) {
	rows, err := q.db.Query(ctx, `SELECT fqdn, job_id, last_run_at, last_status, last_error, last_success_at, last_snapshot_id FROM job_status ORDER BY fqdn, job_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []JobStatus
	for rows.Next() {
		var s JobStatus
		if err := rows.Scan(&s.FQDN, &s.JobID, &s.LastRunAt, &s.LastStatus, &s.LastError, &s.LastSuccessAt, &s.LastSnapshotID); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// RunCounter is one holdfast_runs_total series.
type RunCounter struct {
	FQDN, Kind, JobID, Status string
	Count                     int64
}

// ListRunCounters returns every run counter.
func (q *Q) ListRunCounters(ctx context.Context) ([]RunCounter, error) {
	rows, err := q.db.Query(ctx, `SELECT fqdn, kind, job_id, status, count FROM run_counters ORDER BY fqdn, kind, job_id, status`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RunCounter
	for rows.Next() {
		var c RunCounter
		if err := rows.Scan(&c.FQDN, &c.Kind, &c.JobID, &c.Status, &c.Count); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

/* ---------------- commands ---------------- */

// Command is work queued for an agent.
type Command struct {
	ID        string          `json:"id"`
	FQDN      string          `json:"fqdn"`
	Kind      string          `json:"kind"`
	JobID     string          `json:"job_id,omitempty"`
	CreatedAt time.Time       `json:"created_at"`
	CreatedBy string          `json:"created_by"`
	Payload   json.RawMessage `json:"payload,omitempty"`    // a restore's spec, or a pause's end
	ExpiresAt *time.Time      `json:"expires_at,omitempty"` // not sent once this has passed
	// Sealed is a command's secrets, sealed (a restore from another host). Never in an answer;
	// cleared once the agent has the command.
	Sealed []byte `json:"-"`
}

// EnqueueCommand queues a command.
func (q *Q) EnqueueCommand(ctx context.Context, c Command) error {
	var payload any
	if len(c.Payload) > 0 {
		payload = []byte(c.Payload)
	}
	_, err := q.db.Exec(ctx, `INSERT INTO commands (id, fqdn, kind, job_id, created_at, created_by, payload, expires_at, sealed)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		c.ID, c.FQDN, c.Kind, c.JobID, c.CreatedAt, c.CreatedBy, payload, c.ExpiresAt, c.Sealed)
	return err
}

const commandCols = `id, fqdn, kind, job_id, created_at, created_by, payload, expires_at, sealed`

func scanCommands(rows pgx.Rows) ([]Command, error) {
	defer rows.Close()
	var out []Command
	for rows.Next() {
		var c Command
		var payload []byte
		if err := rows.Scan(&c.ID, &c.FQDN, &c.Kind, &c.JobID, &c.CreatedAt, &c.CreatedBy, &payload, &c.ExpiresAt, &c.Sealed); err != nil {
			return nil, err
		}
		c.Payload = payload
		out = append(out, c)
	}
	return out, rows.Err()
}

// PendingCommands returns the host's commands that the agent has not acknowledged yet, and that have
// neither expired, by the server's clock, nor been cancelled.
func (q *Q) PendingCommands(ctx context.Context, fqdn string, now time.Time) ([]Command, error) {
	rows, err := q.db.Query(ctx, `SELECT `+commandCols+` FROM commands WHERE fqdn = $1 AND acked_at IS NULL AND cancelled_at IS NULL
		AND (expires_at IS NULL OR expires_at > $2) ORDER BY created_at`, fqdn, now)
	if err != nil {
		return nil, err
	}
	return scanCommands(rows)
}

// ExpireCommands cancels the commands whose time ran out before their agent picked them up, and returns
// them.
func (q *Q) ExpireCommands(ctx context.Context, now time.Time) ([]Command, error) {
	rows, err := q.db.Query(ctx, `UPDATE commands SET cancelled_at = $1, sealed = NULL WHERE acked_at IS NULL AND cancelled_at IS NULL
		AND expires_at IS NOT NULL AND expires_at <= $1 RETURNING `+commandCols, now)
	if err != nil {
		return nil, err
	}
	return scanCommands(rows)
}

// PendingRestores returns every queued restore that no agent has picked up yet, oldest first.
func (q *Q) PendingRestores(ctx context.Context) ([]Command, error) {
	rows, err := q.db.Query(ctx, `SELECT `+commandCols+` FROM commands WHERE kind = 'restore' AND acked_at IS NULL AND cancelled_at IS NULL
		ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	return scanCommands(rows)
}

// AckCommands marks the host's commands as received by the agent.
func (q *Q) AckCommands(ctx context.Context, fqdn string, ids []string, now time.Time) error {
	if len(ids) == 0 {
		return nil
	}
	_, err := q.db.Exec(ctx, `UPDATE commands SET acked_at = $3, sealed = NULL WHERE fqdn = $1 AND id = ANY($2) AND acked_at IS NULL`, fqdn, ids, now)
	return err
}

/* ---------------- maintenance ---------------- */

// LastCheckSuccess returns, per host, when the agent's repository check last succeeded (decision 030).
func (q *Q) LastCheckSuccess(ctx context.Context) (map[string]time.Time, error) {
	return q.timesByHost(ctx, `SELECT fqdn, max(finished_at) FROM events WHERE kind = 'check' AND status = 'success' GROUP BY fqdn`)
}

func (q *Q) timesByHost(ctx context.Context, sql string, args ...any) (map[string]time.Time, error) {
	rows, err := q.db.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]time.Time{}
	for rows.Next() {
		var f string
		var t time.Time
		if err := rows.Scan(&f, &t); err != nil {
			return nil, err
		}
		out[f] = t
	}
	return out, rows.Err()
}

/* ---------------- audit ---------------- */

// AuditEntry is one audited action.
type AuditEntry struct {
	ID       int64          `json:"id"`
	At       time.Time      `json:"at"`
	Actor    string         `json:"actor"`
	Source   string         `json:"source"`
	Action   string         `json:"action"`
	FQDN     string         `json:"fqdn,omitempty"`
	JobID    string         `json:"job_id,omitempty"`
	Revision int64          `json:"revision,omitempty"`
	Detail   map[string]any `json:"detail,omitempty"`
}

// Audit appends an entry. Detail must never hold a secret.
func (q *Q) Audit(ctx context.Context, e AuditEntry) error {
	detail := e.Detail
	if detail == nil {
		detail = map[string]any{}
	}
	b, err := json.Marshal(detail)
	if err != nil {
		return err
	}
	_, err = q.db.Exec(ctx, `INSERT INTO audit_log (actor, source, action, fqdn, job_id, revision, detail) VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		e.Actor, e.Source, e.Action, e.FQDN, e.JobID, e.Revision, b)
	return err
}

// ListAudit returns the newest entries, optionally for one host.
func (q *Q) ListAudit(ctx context.Context, fqdn string, limit int) ([]AuditEntry, error) {
	return q.ListAuditFiltered(ctx, AuditFilter{FQDN: fqdn, Limit: limit})
}

// AuditFilter narrows ListAuditFiltered.
type AuditFilter struct {
	FQDN   string   // one host; empty for all
	Action string   // one action; empty for all
	Hosts  []string // nil: every entry; otherwise only these hosts' entries, and none that belong to no host
	Before int64    // entries older than this ID, for paging; 0 for the newest
	Limit  int
}

// ListAuditFiltered returns audit entries, newest first.
func (q *Q) ListAuditFiltered(ctx context.Context, f AuditFilter) ([]AuditEntry, error) {
	rows, err := q.db.Query(ctx, `SELECT id, at, actor, source, action, fqdn, job_id, revision, detail FROM audit_log
		WHERE ($1 = '' OR fqdn = $1) AND ($2 = '' OR action = $2) AND ($3::text[] IS NULL OR fqdn = ANY($3)) AND ($4 = 0 OR id < $4)
		ORDER BY id DESC LIMIT $5`, f.FQDN, f.Action, f.Hosts, f.Before, f.Limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AuditEntry{}
	for rows.Next() {
		var e AuditEntry
		var detail []byte
		if err := rows.Scan(&e.ID, &e.At, &e.Actor, &e.Source, &e.Action, &e.FQDN, &e.JobID, &e.Revision, &detail); err != nil {
			return nil, err
		}
		_ = json.Unmarshal(detail, &e.Detail)
		out = append(out, e)
	}
	return out, rows.Err()
}

/* ---------------- notifications ---------------- */

// NotifyChannel wakes long polls: the payload is a host's FQDN.
const NotifyChannel = "holdfast_agent"

// NotifyHost wakes the long poll of one host on every server.
func (q *Q) NotifyHost(ctx context.Context, fqdn string) error {
	_, err := q.db.Exec(ctx, `SELECT pg_notify($1, $2)`, NotifyChannel, fqdn)
	return err
}
