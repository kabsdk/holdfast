// Package store is holdfast-server's PostgreSQL state (plan-m1 §6).
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
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kabsdk/holdfast/internal/config"
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
func (s *Store) Migrate(ctx context.Context) error {
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
}

// CreateHost records a new host. A host that exists already (even retired) is refused.
func (q *Q) CreateHost(ctx context.Context, fqdn string, rev int64, now time.Time) error {
	_, err := q.db.Exec(ctx, `INSERT INTO hosts (fqdn, state, added_revision, added_at) VALUES ($1, 'awaiting_enrollment', $2, $3)`, fqdn, rev, now)
	if isUniqueViolation(err) {
		return fmt.Errorf("host %s exists already: %w", fqdn, ErrConflict)
	}
	return err
}

const hostCols = `fqdn, state, added_revision, added_at, connected_at, first_backup_at, storage_checked_at, storage_findings`

func scanHost(row pgx.Row) (Host, error) {
	var h Host
	var checked *time.Time
	var findings []byte
	err := row.Scan(&h.FQDN, &h.State, &h.AddedRevision, &h.AddedAt, &h.ConnectedAt, &h.FirstBackupAt, &checked, &findings)
	if err == nil && checked != nil {
		h.Storage = &storage.Report{CheckedAt: *checked, Findings: []storage.Finding{}}
		if len(findings) > 0 {
			err = json.Unmarshal(findings, &h.Storage.Findings)
		}
	}
	return h, notFound(err)
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
	_, err = q.db.Exec(ctx, `UPDATE hosts SET storage_checked_at = $2, storage_findings = $3 WHERE fqdn = $1`, fqdn, r.CheckedAt, b)
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
	FQDN               string     `json:"fqdn"`
	EnrolledAt         time.Time  `json:"enrolled_at"`
	LastSeenAt         *time.Time `json:"last_seen_at,omitempty"`
	AgentVersion       string     `json:"agent_version"`
	ResticVersion      string     `json:"restic_version"`
	OSID               string     `json:"os_id"`
	OSVersion          string     `json:"os_version"`
	HostnameFQDN       string     `json:"hostname_fqdn"`
	Timezone           string     `json:"timezone"`
	ClockOffsetMS      int64      `json:"clock_offset_ms"`
	AppliedRevision    int64      `json:"applied_revision"`
	AppliedSHA256      string     `json:"applied_sha256"`
	AppliedAt          *time.Time `json:"applied_at,omitempty"`
	CredentialsVersion int        `json:"credentials_version"`
}

// EnrollAgent records (or replaces, on re-enrollment) the agent's credential and marks the host enrolled.
func (q *Q) EnrollAgent(ctx context.Context, fqdn, tokenHash string, now time.Time) error {
	if _, err := q.db.Exec(ctx, `INSERT INTO agents (fqdn, credential_kind, credential_sha256, enrolled_at, last_seen_at)
		VALUES ($1, 'token', $2, $3, $3)
		ON CONFLICT (fqdn) DO UPDATE SET credential_kind = 'token', credential_sha256 = $2, enrolled_at = $3, last_seen_at = $3`,
		fqdn, tokenHash, now); err != nil {
		return err
	}
	_, err := q.db.Exec(ctx, `UPDATE hosts SET state = 'enrolled', connected_at = COALESCE(connected_at, $2) WHERE fqdn = $1`, fqdn, now)
	return err
}

// AgentByToken returns the FQDN that owns the token hash.
func (q *Q) AgentByToken(ctx context.Context, tokenHash string) (string, error) {
	var fqdn string
	err := q.db.QueryRow(ctx, `SELECT a.fqdn FROM agents a JOIN hosts h ON h.fqdn = a.fqdn
		WHERE a.credential_sha256 = $1 AND h.state = 'enrolled'`, tokenHash).Scan(&fqdn)
	return fqdn, notFound(err)
}

// Heartbeat records a poll: facts, what the agent runs, and when it was seen.
type Heartbeat struct {
	AgentVersion, ResticVersion, OSID, OSVersion, HostnameFQDN, Timezone string
	ClockOffsetMS                                                        int64
	AppliedRevision                                                      int64
	AppliedSHA256                                                        string
	CredentialsVersion                                                   int
}

// RecordHeartbeat updates the agent row. applied_at moves only when the applied sha256 changes.
func (q *Q) RecordHeartbeat(ctx context.Context, fqdn string, h Heartbeat, now time.Time) error {
	_, err := q.db.Exec(ctx, `UPDATE agents SET last_seen_at = $2, agent_version = $3, restic_version = $4, os_id = $5, os_version = $6,
		hostname_fqdn = $7, timezone = $8, clock_offset_ms = $9,
		applied_at = CASE WHEN applied_sha256 IS DISTINCT FROM $11 THEN $2 ELSE applied_at END,
		applied_revision = $10, applied_sha256 = $11, credentials_version = $12
		WHERE fqdn = $1`,
		fqdn, now, h.AgentVersion, h.ResticVersion, h.OSID, h.OSVersion, h.HostnameFQDN, h.Timezone, h.ClockOffsetMS,
		h.AppliedRevision, h.AppliedSHA256, h.CredentialsVersion)
	return err
}

const agentCols = `fqdn, enrolled_at, last_seen_at, agent_version, restic_version, os_id, os_version, hostname_fqdn, timezone,
	clock_offset_ms, applied_revision, applied_sha256, applied_at, credentials_version`

func scanAgent(row pgx.Row) (Agent, error) {
	var a Agent
	err := row.Scan(&a.FQDN, &a.EnrolledAt, &a.LastSeenAt, &a.AgentVersion, &a.ResticVersion, &a.OSID, &a.OSVersion, &a.HostnameFQDN,
		&a.Timezone, &a.ClockOffsetMS, &a.AppliedRevision, &a.AppliedSHA256, &a.AppliedAt, &a.CredentialsVersion)
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

// SealedCredentials is a host's credentials as stored: secrets are ciphertext.
type SealedCredentials struct {
	FQDN           string
	Version        int
	Repository     string
	RepoPassword   []byte
	S3AccessKeyID  string
	S3Secret       []byte
	S3KeySource    string // KeyByAdmin or KeyByHoldfast
	CreatedAt      time.Time
	EscrowExportID *int64
}

// Who made a host's S3 key (decision 029).
const (
	KeyByAdmin    = "admin"    // given at host add or set-key
	KeyByHoldfast = "holdfast" // generated, then made by the provisioner command
)

// PutCredentials stores a host's credentials, replacing any older version.
func (q *Q) PutCredentials(ctx context.Context, c SealedCredentials) error {
	if c.S3KeySource == "" {
		c.S3KeySource = KeyByHoldfast
	}
	_, err := q.db.Exec(ctx, `INSERT INTO host_credentials (fqdn, version, repository, repo_password, s3_access_key_id, s3_secret, s3_key_source, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (fqdn) DO UPDATE SET version = $2, repository = $3, repo_password = $4, s3_access_key_id = $5, s3_secret = $6,
		s3_key_source = $7, created_at = $8, escrow_export_id = NULL`,
		c.FQDN, c.Version, c.Repository, c.RepoPassword, c.S3AccessKeyID, c.S3Secret, c.S3KeySource, c.CreatedAt)
	return err
}

const credCols = `fqdn, version, repository, repo_password, s3_access_key_id, s3_secret, s3_key_source, created_at, escrow_export_id`

func scanCreds(row pgx.Row) (SealedCredentials, error) {
	var c SealedCredentials
	err := row.Scan(&c.FQDN, &c.Version, &c.Repository, &c.RepoPassword, &c.S3AccessKeyID, &c.S3Secret, &c.S3KeySource, &c.CreatedAt, &c.EscrowExportID)
	return c, notFound(err)
}

// GetCredentials returns a host's sealed credentials.
func (q *Q) GetCredentials(ctx context.Context, fqdn string) (SealedCredentials, error) {
	return scanCreds(q.db.QueryRow(ctx, `SELECT `+credCols+` FROM host_credentials WHERE fqdn = $1`, fqdn))
}

// AllCredentials returns every host's sealed credentials, for escrow export.
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

// RecordEscrowExport records a bundle and marks the given hosts' current credentials as escrowed.
func (q *Q) RecordEscrowExport(ctx context.Context, actor string, fqdns []string, bundleSHA string) (int64, error) {
	var id int64
	if err := q.db.QueryRow(ctx, `INSERT INTO escrow_exports (actor, host_count, bundle_sha256) VALUES ($1, $2, $3) RETURNING id`,
		actor, len(fqdns), bundleSHA).Scan(&id); err != nil {
		return 0, err
	}
	_, err := q.db.Exec(ctx, `UPDATE host_credentials SET escrow_export_id = $1 WHERE fqdn = ANY($2)`, id, fqdns)
	return id, err
}

// HostsMissingEscrow counts hosts whose current credentials are in no escrow bundle.
func (q *Q) HostsMissingEscrow(ctx context.Context) (int, error) {
	var n int
	err := q.db.QueryRow(ctx, `SELECT count(*) FROM host_credentials WHERE escrow_export_id IS NULL`).Scan(&n)
	return n, err
}

/* ---------------- events ---------------- */

// Event is a stored run result.
type Event struct {
	FQDN         string     `json:"fqdn"`
	ID           string     `json:"id"`
	Kind         string     `json:"kind"`
	JobID        string     `json:"job_id"`
	CommandID    string     `json:"command_id,omitempty"`
	Status       string     `json:"status"`
	ScheduledFor *time.Time `json:"scheduled_for,omitempty"`
	SpreadDelayS int        `json:"spread_delay_s"`
	Started      time.Time  `json:"started"`
	Finished     time.Time  `json:"finished"`
	Revision     int64      `json:"revision"`
	SnapshotID   string     `json:"snapshot_id,omitempty"`
	FilesNew     int64      `json:"files_new"`
	FilesChanged int64      `json:"files_changed"`
	BytesAdded   int64      `json:"bytes_added"`
	TotalFiles   int64      `json:"total_files"`
	TotalBytes   int64      `json:"total_bytes"`
	ResticExit   int        `json:"restic_exit"`
	Error        string     `json:"error,omitempty"`
}

// InsertEvent stores an event once. It returns false if the event was stored before (a re-sent
// event), in which case nothing else changes. A new event updates the job's status and the counters.
func (q *Q) InsertEvent(ctx context.Context, e Event) (bool, error) {
	tag, err := q.db.Exec(ctx, `INSERT INTO events (fqdn, id, kind, job_id, command_id, status, scheduled_for, spread_delay_s, started_at, finished_at,
		revision, snapshot_id, files_new, files_changed, bytes_added, total_files, total_bytes, restic_exit, error)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19) ON CONFLICT (fqdn, id) DO NOTHING`,
		e.FQDN, e.ID, e.Kind, e.JobID, e.CommandID, e.Status, e.ScheduledFor, e.SpreadDelayS, e.Started, e.Finished,
		e.Revision, e.SnapshotID, e.FilesNew, e.FilesChanged, e.BytesAdded, e.TotalFiles, e.TotalBytes, e.ResticExit, e.Error)
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

// ListEvents returns a host's events, newest first.
func (q *Q) ListEvents(ctx context.Context, fqdn string, limit int) ([]Event, error) {
	rows, err := q.db.Query(ctx, `SELECT fqdn, id, kind, job_id, command_id, status, scheduled_for, spread_delay_s, started_at, finished_at,
		revision, snapshot_id, files_new, files_changed, bytes_added, total_files, total_bytes, restic_exit, error
		FROM events WHERE fqdn = $1 ORDER BY finished_at DESC, received_at DESC LIMIT $2`, fqdn, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		if err := rows.Scan(&e.FQDN, &e.ID, &e.Kind, &e.JobID, &e.CommandID, &e.Status, &e.ScheduledFor, &e.SpreadDelayS, &e.Started, &e.Finished,
			&e.Revision, &e.SnapshotID, &e.FilesNew, &e.FilesChanged, &e.BytesAdded, &e.TotalFiles, &e.TotalBytes, &e.ResticExit, &e.Error); err != nil {
			return nil, err
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
	ID        string    `json:"id"`
	FQDN      string    `json:"fqdn"`
	Kind      string    `json:"kind"`
	JobID     string    `json:"job_id,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	CreatedBy string    `json:"created_by"`
}

// EnqueueCommand queues a command.
func (q *Q) EnqueueCommand(ctx context.Context, c Command) error {
	_, err := q.db.Exec(ctx, `INSERT INTO commands (id, fqdn, kind, job_id, created_at, created_by) VALUES ($1, $2, $3, $4, $5, $6)`,
		c.ID, c.FQDN, c.Kind, c.JobID, c.CreatedAt, c.CreatedBy)
	return err
}

// PendingCommands returns the host's commands that the agent has not acknowledged yet.
func (q *Q) PendingCommands(ctx context.Context, fqdn string) ([]Command, error) {
	rows, err := q.db.Query(ctx, `SELECT id, fqdn, kind, job_id, created_at, created_by FROM commands WHERE fqdn = $1 AND acked_at IS NULL ORDER BY created_at`, fqdn)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Command
	for rows.Next() {
		var c Command
		if err := rows.Scan(&c.ID, &c.FQDN, &c.Kind, &c.JobID, &c.CreatedAt, &c.CreatedBy); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// AckCommands marks the host's commands as received by the agent.
func (q *Q) AckCommands(ctx context.Context, fqdn string, ids []string, now time.Time) error {
	if len(ids) == 0 {
		return nil
	}
	_, err := q.db.Exec(ctx, `UPDATE commands SET acked_at = $3 WHERE fqdn = $1 AND id = ANY($2) AND acked_at IS NULL`, fqdn, ids, now)
	return err
}

/* ---------------- maintenance ---------------- */

// StartMaintenance records the start of a maintenance run and returns its ID.
func (q *Q) StartMaintenance(ctx context.Context, fqdn, kind, subset, node string, now time.Time) (int64, error) {
	var id int64
	err := q.db.QueryRow(ctx, `INSERT INTO maintenance_runs (fqdn, kind, subset, started_at, node) VALUES ($1, $2, $3, $4, $5) RETURNING id`,
		fqdn, kind, subset, now, node).Scan(&id)
	return id, err
}

// FinishMaintenance records how a maintenance run ended.
func (q *Q) FinishMaintenance(ctx context.Context, id int64, status, errText string, now time.Time) error {
	_, err := q.db.Exec(ctx, `UPDATE maintenance_runs SET finished_at = $2, status = $3, error = $4 WHERE id = $1`, id, now, status, errText)
	return err
}

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
	rows, err := q.db.Query(ctx, `SELECT id, at, actor, source, action, fqdn, job_id, revision, detail FROM audit_log
		WHERE $1 = '' OR fqdn = $1 ORDER BY id DESC LIMIT $2`, fqdn, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AuditEntry
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
