// Package server is holdfast-server: the configurator and the maintenance leader (decision 007).
//
// Every operation a person can do is a method here and an /api/v1 endpoint (decision 028); the CLI
// and the UI only call the API. Every operation is audited. Agents talk to it over /agent/v1.
package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/kabsdk/holdfast/internal/config"
	"github.com/kabsdk/holdfast/internal/proto"
	"github.com/kabsdk/holdfast/internal/restic"
	"github.com/kabsdk/holdfast/internal/server/creds"
	"github.com/kabsdk/holdfast/internal/server/storage"
	"github.com/kabsdk/holdfast/internal/server/store"
)

// EnrollmentTTL is how long an enrollment code is valid (spec §5.7). It is not configurable.
const EnrollmentTTL = 15 * time.Minute

// Server is the running control plane.
type Server struct {
	set      Settings
	st       *store.Store
	seal     *creds.Sealer
	prov     storage.Provisioner // nil: the admin gives every host's key (decision 029)
	prober   storage.Prober
	loc      storage.Location
	maintKey storage.Key
	zone     *time.Location
	log      *slog.Logger
	hub      *hub
	now      func() time.Time
	started  time.Time // when this process started; older unfinished checks were interrupted

	mu         sync.Mutex
	checking   map[string]bool // hosts with a check running, whoever started it
	checkSlots chan struct{}   // daily checks running at once, at most parallelChecks
	checks     sync.WaitGroup  // daily checks started by the loop; tests wait for them
}

// Deps are what New needs besides the settings. Tests replace parts of them.
type Deps struct {
	Store       *store.Store
	Sealer      *creds.Sealer
	Provisioner storage.Provisioner // optional: makes the keys Holdfast generates
	Prober      storage.Prober      // tests the storage contract; S3Prober when nil
	Maintenance storage.Key
	Logger      *slog.Logger
	Now         func() time.Time
}

// New builds a server.
func New(set Settings, d Deps) (*Server, error) {
	zone, err := time.LoadLocation(set.DefaultZone)
	if err != nil {
		return nil, err
	}
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	if err := os.MkdirAll(filepath.Join(set.StateDir, "cache"), 0o700); err != nil {
		return nil, err
	}
	loc := storage.Location{Endpoint: set.Storage.Endpoint, Bucket: set.Storage.Bucket, Region: set.Storage.Region}
	if d.Prober == nil {
		d.Prober = storage.S3Prober{Location: loc, Maint: d.Maintenance}
	}
	return &Server{set: set, st: d.Store, seal: d.Sealer, prov: d.Provisioner, prober: d.Prober, maintKey: d.Maintenance,
		loc: loc, zone: zone, log: d.Logger, hub: newHub(), now: d.Now, started: d.Now(),
		checking: map[string]bool{}, checkSlots: make(chan struct{}, parallelChecks)}, nil
}

// Run starts the background work: the notification listener and the maintenance loop.
func (s *Server) Run(ctx context.Context) {
	go s.hub.listen(ctx, s.st.Pool(), s.log)
	go s.maintenanceLoop(ctx)
}

// Actor is who did something, for the audit log.
type Actor struct {
	Name   string // local:<unix user> in M1; the OIDC subject from M4
	Source string // cli, api, ui or system
}

// ValidationError lists structural problems; the API answers 422.
type ValidationError struct{ Problems config.Problems }

func (e *ValidationError) Error() string { return "the change is not valid: " + e.Problems.Error() }

// ErrNotFound and ErrConflict are returned as 404 and 409.
var (
	ErrNotFound = store.ErrNotFound
	ErrConflict = store.ErrConflict
)

func newID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b)
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

/* ---------------- rendering ---------------- */

// renderHost builds a host's agent config from a document. Schedules get their zone resolved here,
// so agents never need the fleet default.
func (s *Server) renderHost(doc config.Document, fqdn string) proto.HostConfig {
	hc := proto.HostConfig{FQDN: fqdn, Repository: s.loc.RepoURL(fqdn), Jobs: []proto.JobConfig{}}
	h, ok := doc.Hosts[fqdn]
	if !ok {
		return hc
	}
	ids := make([]string, 0, len(h.Jobs))
	for id := range h.Jobs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		j := h.Jobs[id]
		sch := j.Schedule
		if sch.Zone == "" {
			sch.Zone = s.set.DefaultZone
		}
		hc.Jobs = append(hc.Jobs, proto.JobConfig{ID: id, Engine: j.Engine, Paths: j.Paths, Excludes: j.Excludes, Schedule: sch})
	}
	hc.SHA256 = hc.ComputeSHA256()
	return hc
}

func (s *Server) hostSHAs(doc config.Document) map[string]string {
	out := map[string]string{}
	for fqdn := range doc.Hosts {
		out[fqdn] = s.renderHost(doc, fqdn).SHA256
	}
	return out
}

// head returns the newest revision, or an empty document at revision 0.
func (s *Server) head(ctx context.Context, q *store.Q) (store.Revision, error) {
	r, err := q.Head(ctx)
	if errors.Is(err, store.ErrNotFound) {
		return store.Revision{Number: 0, Config: config.New()}, nil
	}
	return r, err
}

/* ---------------- storage set-up ---------------- */

func (s *Server) passwordFile(fqdn, password string) (string, func(), error) {
	dir := filepath.Join(s.set.StateDir, "tmp")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", nil, err
	}
	f, err := os.CreateTemp(dir, "pass-*")
	if err != nil {
		return "", nil, err
	}
	cleanup := func() { _ = os.Remove(f.Name()) }
	if _, err := f.WriteString(password); err != nil {
		f.Close()
		cleanup()
		return "", nil, err
	}
	if err := f.Close(); err != nil {
		cleanup()
		return "", nil, err
	}
	return f.Name(), cleanup, nil
}

// maintRunner returns a restic runner for a host's repository under the maintenance identity.
func (s *Server) maintRunner(ctx context.Context, fqdn string) (restic.Runner, func(), error) {
	c, err := s.st.Q().GetCredentials(ctx, fqdn)
	if err != nil {
		return restic.Runner{}, nil, err
	}
	pw, err := s.seal.Open(c.RepoPassword, fqdn)
	if err != nil {
		return restic.Runner{}, nil, err
	}
	pf, cleanup, err := s.passwordFile(fqdn, pw)
	if err != nil {
		return restic.Runner{}, nil, err
	}
	return restic.Runner{Bin: s.set.ResticBin, Repo: c.Repository, PasswordFile: pf,
		CacheDir: filepath.Join(s.set.StateDir, "cache"), Env: s.loc.Env(s.maintKey)}, cleanup, nil
}

// hostKey returns a host's current S3 key and who made it.
func (s *Server) hostKey(ctx context.Context, fqdn string) (storage.Key, string, error) {
	c, err := s.st.Q().GetCredentials(ctx, fqdn)
	if err != nil {
		return storage.Key{}, "", err
	}
	secret, err := s.seal.Open(c.S3Secret, fqdn)
	if err != nil {
		return storage.Key{}, "", err
	}
	return storage.Key{AccessKeyID: c.S3AccessKeyID, SecretAccessKey: secret}, c.S3KeySource, nil
}

// prepareKey makes a key that Holdfast generated exist, if a provisioner is configured, and tests the
// storage contract with it (decision 029).
func (s *Server) prepareKey(ctx context.Context, fqdn string, key storage.Key, source string) (storage.Report, error) {
	if source == store.KeyByHoldfast && s.prov != nil {
		if err := s.prov.EnsureHostKey(ctx, fqdn, key); err != nil {
			return storage.Report{}, err
		}
	}
	rep := s.prober.Probe(ctx, fqdn, key)
	rep.CheckedAt = s.now()
	return rep, nil
}

// recordStorage keeps a host's latest contract test and audits a failure.
func (s *Server) recordStorage(ctx context.Context, fqdn string, rep storage.Report) {
	q := s.st.Q()
	if err := q.RecordStorageCheck(ctx, fqdn, rep); err != nil {
		s.log.Error("recording a storage check", "host", fqdn, "err", err)
	}
	if !rep.OK() {
		if err := q.Audit(ctx, store.AuditEntry{Actor: systemActor.Name, Source: systemActor.Source, Action: "storage.check_failed", FQDN: fqdn,
			Detail: map[string]any{"codes": rep.Codes(), "findings": rep.Summary()}}); err != nil {
			s.log.Error("auditing a storage check", "host", fqdn, "err", err)
		}
	}
}

// ensureStorage makes a stored host's storage ready, idempotently: it tests the contract again (the S3
// policies may have changed since the key was given), records the result, and initialises the
// repository once the contract holds.
func (s *Server) ensureStorage(ctx context.Context, fqdn string) (storage.Report, error) {
	key, source, err := s.hostKey(ctx, fqdn)
	if err != nil {
		return storage.Report{}, err
	}
	rep, err := s.prepareKey(ctx, fqdn, key, source)
	if err != nil {
		return storage.Report{}, err
	}
	s.recordStorage(ctx, fqdn, rep)
	if !rep.OK() {
		return rep, nil
	}
	return rep, s.initRepository(ctx, fqdn)
}

// StorageError refuses a key that does not meet the storage contract. The API answers 422 with the
// report, so the UI and the CLI show the same findings.
type StorageError struct{ Report storage.Report }

func (e *StorageError) Error() string {
	return "the S3 key does not meet the storage contract (docs/storage.md): " + e.Report.Summary()
}

// S3KeyInput is a host's S3 key, as the admin made it. It is input only: no response, log, audit
// entry or revision ever contains it.
type S3KeyInput struct {
	AccessKeyID     string `json:"access_key_id"`
	SecretAccessKey string `json:"secret_access_key"`
}

func (k *S3KeyInput) key() (storage.Key, error) {
	key := storage.Key{AccessKeyID: strings.TrimSpace(k.AccessKeyID), SecretAccessKey: strings.TrimSpace(k.SecretAccessKey)}
	if err := key.Validate(); err != nil {
		return storage.Key{}, &ValidationError{config.Problems{{Path: "s3_key", Message: err.Error()}}}
	}
	return key, nil
}

func newAccessKeyID() string { return "HF" + strings.ToUpper(hex.EncodeToString(creds.Random(9))) }

/* ---------------- operations (each is an /api/v1 endpoint) ---------------- */

// AddHostInput adds a host, optionally with its first job.
type AddHostInput struct {
	FQDN    string      `json:"fqdn"`
	Message string      `json:"message"`
	JobID   string      `json:"job_id,omitempty"`
	Job     *config.Job `json:"job,omitempty"`
	Base    *int64      `json:"base_revision,omitempty"` // omitted: the current head
	// S3Key is the host's key (decision 029). It may be omitted only when a provisioner is configured.
	S3Key *S3KeyInput `json:"s3_key,omitempty"`
}

// Enrollment is what a person needs to enroll a host.
type Enrollment struct {
	FQDN           string    `json:"fqdn"`
	Code           string    `json:"code"`
	ExpiresAt      time.Time `json:"expires_at"`
	InstallCommand string    `json:"install_command"`
}

// AddHostResult is the outcome of AddHost.
type AddHostResult struct {
	Revision   int64          `json:"revision"`
	Enrollment Enrollment     `json:"enrollment"`
	Storage    storage.Report `json:"storage"`
	Warning    string         `json:"warning,omitempty"`
}

func (s *Server) installCommand(fqdn, code string) string {
	cmd := fmt.Sprintf("holdfast-agent enroll --server %s --fqdn %s --code %s", s.set.PublicURL, fqdn, code)
	if s.set.CASHA256 != "" {
		cmd += " --ca-sha256 " + s.set.CASHA256
	}
	return cmd
}

// AddHost tests the host's S3 key against the storage contract and refuses a key that fails it. It then
// creates a revision with the host, its credentials and a 15-minute enrollment code, all in one
// transaction, and initialises the repository. An initialisation failure is reported but not fatal:
// enrollment does it again before it uses the code.
func (s *Server) AddHost(ctx context.Context, a Actor, in AddHostInput) (AddHostResult, error) {
	in.FQDN = strings.TrimSpace(in.FQDN)
	if err := config.ValidFQDN(in.FQDN); err != nil {
		return AddHostResult{}, &ValidationError{config.Problems{{Path: "fqdn", Message: err.Error()}}}
	}
	if strings.TrimSpace(in.Message) == "" {
		in.Message = "Add host " + in.FQDN
	}
	var key storage.Key
	source := store.KeyByAdmin
	switch {
	case in.S3Key != nil:
		k, err := in.S3Key.key()
		if err != nil {
			return AddHostResult{}, err
		}
		key = k
	case s.prov != nil:
		key, source = storage.Key{AccessKeyID: newAccessKeyID(), SecretAccessKey: hex.EncodeToString(creds.Random(20))}, store.KeyByHoldfast
	default:
		return AddHostResult{}, &ValidationError{config.Problems{{Path: "s3_key", Message: "the host's S3 key is required: make a key that may use only " +
			storage.Prefix(in.FQDN) + "/ (docs/storage.md) and give it with --s3-key-file"}}}
	}
	if _, err := s.st.Q().GetHost(ctx, in.FQDN); err == nil {
		return AddHostResult{}, fmt.Errorf("host %s exists already: %w", in.FQDN, ErrConflict)
	}
	rep, err := s.prepareKey(ctx, in.FQDN, key, source)
	if err != nil {
		return AddHostResult{}, err
	}
	if !rep.OK() {
		return AddHostResult{}, &StorageError{rep}
	}
	now := s.now()
	code := creds.NewEnrollmentCode()
	var res AddHostResult
	err = s.st.InTx(ctx, func(q *store.Q) error {
		head, err := s.head(ctx, q)
		if err != nil {
			return err
		}
		if in.Base != nil && *in.Base != head.Number {
			return ErrConflict
		}
		if _, exists := head.Config.Hosts[in.FQDN]; exists {
			return fmt.Errorf("host %s is in the config already: %w", in.FQDN, ErrConflict)
		}
		doc := head.Config.Clone()
		h := config.Host{Jobs: map[string]config.Job{}}
		if in.Job != nil {
			if in.Job.Engine == "" {
				in.Job.Engine = config.EngineRestic
			}
			h.Jobs[in.JobID] = *in.Job
		}
		doc.Hosts[in.FQDN] = h
		if p := doc.Validate(); len(p) > 0 {
			return &ValidationError{p}
		}
		rev, err := q.SaveRevision(ctx, store.SaveRevision{Base: head.Number, Actor: a.Name, Source: a.Source, Message: in.Message, Config: doc, HostSHAs: s.hostSHAs(doc)})
		if err != nil {
			return err
		}
		if err := q.CreateHost(ctx, in.FQDN, rev.Number, now); err != nil {
			return err
		}
		if err := q.PutCredentials(ctx, store.SealedCredentials{FQDN: in.FQDN, Version: 1, Repository: s.loc.RepoURL(in.FQDN),
			RepoPassword: s.seal.Seal(creds.NewPassword(), in.FQDN), S3AccessKeyID: key.AccessKeyID, S3Secret: s.seal.Seal(key.SecretAccessKey, in.FQDN),
			S3KeySource: source, CreatedAt: now}); err != nil {
			return err
		}
		if err := q.RecordStorageCheck(ctx, in.FQDN, rep); err != nil {
			return err
		}
		expires := now.Add(EnrollmentTTL)
		if err := q.CreateEnrollmentCode(ctx, newID(), in.FQDN, creds.HashToken(creds.NormaliseCode(code)), now, expires, a.Name); err != nil {
			return err
		}
		detail := map[string]any{"message": in.Message, "s3_key_by": source}
		if in.Job != nil {
			detail["job"] = in.JobID
		}
		if err := q.Audit(ctx, store.AuditEntry{Actor: a.Name, Source: a.Source, Action: "host.add", FQDN: in.FQDN, Revision: rev.Number, Detail: detail}); err != nil {
			return err
		}
		if err := q.Audit(ctx, store.AuditEntry{Actor: a.Name, Source: a.Source, Action: "enrollment.code_issued", FQDN: in.FQDN,
			Detail: map[string]any{"expires_at": expires}}); err != nil {
			return err
		}
		res = AddHostResult{Revision: rev.Number, Enrollment: Enrollment{FQDN: in.FQDN, Code: code, ExpiresAt: expires, InstallCommand: s.installCommand(in.FQDN, code)},
			Storage: rep}
		return nil
	})
	if err != nil {
		return AddHostResult{}, err
	}
	if err := s.initRepository(ctx, in.FQDN); err != nil {
		s.log.Warn("repository initialisation failed; enrollment will retry it", "host", in.FQDN, "err", err)
		res.Warning = "the host was added, but its repository could not be initialised yet (" + err.Error() + "); enrollment will try again"
		_ = s.st.Q().Audit(ctx, store.AuditEntry{Actor: systemActor.Name, Source: systemActor.Source, Action: "storage.setup_failed", FQDN: in.FQDN, Detail: map[string]any{"error": err.Error()}})
	}
	return res, nil
}

func (s *Server) initRepository(ctx context.Context, fqdn string) error {
	r, cleanup, err := s.maintRunner(ctx, fqdn)
	if err != nil {
		return err
	}
	defer cleanup()
	if err := r.Init(ctx); err != nil {
		return fmt.Errorf("initialising the repository of %s: %w", fqdn, err)
	}
	return nil
}

// SetS3KeyInput replaces a host's S3 key.
type SetS3KeyInput struct {
	S3Key   S3KeyInput `json:"s3_key"`
	Message string     `json:"message"`
}

// SetS3KeyResult is the outcome of SetS3Key.
type SetS3KeyResult struct {
	CredentialsVersion int            `json:"credentials_version"`
	Storage            storage.Report `json:"storage"`
}

// SetS3Key replaces a host's S3 key with one the admin made, after testing it against the storage
// contract. A key that fails is refused and the old one stays. The agent gets the new key on its next
// poll. The escrow bundle is out of date until the next export.
func (s *Server) SetS3Key(ctx context.Context, a Actor, fqdn string, in SetS3KeyInput) (SetS3KeyResult, error) {
	key, err := in.S3Key.key()
	if err != nil {
		return SetS3KeyResult{}, err
	}
	h, err := s.st.Q().GetHost(ctx, fqdn)
	if err != nil {
		return SetS3KeyResult{}, fmt.Errorf("host %s: %w", fqdn, err)
	}
	if h.State == "retired" {
		return SetS3KeyResult{}, fmt.Errorf("host %s is retired: %w", fqdn, ErrConflict)
	}
	rep, err := s.prepareKey(ctx, fqdn, key, store.KeyByAdmin)
	if err != nil {
		return SetS3KeyResult{}, err
	}
	if !rep.OK() {
		return SetS3KeyResult{}, &StorageError{rep}
	}
	res := SetS3KeyResult{Storage: rep}
	err = s.st.InTx(ctx, func(q *store.Q) error {
		c, err := q.GetCredentials(ctx, fqdn)
		if err != nil {
			return err
		}
		c.Version++
		c.S3AccessKeyID, c.S3Secret, c.S3KeySource, c.CreatedAt = key.AccessKeyID, s.seal.Seal(key.SecretAccessKey, fqdn), store.KeyByAdmin, s.now()
		if err := q.PutCredentials(ctx, c); err != nil {
			return err
		}
		if err := q.RecordStorageCheck(ctx, fqdn, rep); err != nil {
			return err
		}
		res.CredentialsVersion = c.Version
		if err := q.Audit(ctx, store.AuditEntry{Actor: a.Name, Source: a.Source, Action: "host.s3_key_set", FQDN: fqdn,
			Detail: map[string]any{"credentials_version": c.Version, "message": in.Message}}); err != nil {
			return err
		}
		return q.NotifyHost(ctx, fqdn)
	})
	return res, err
}

// IssueEnrollmentCode gives a host a new 15-minute code and revokes the open one. It also serves
// re-enrollment of a host that was enrolled before (plan-m1 Q9): the repository and its password stay.
func (s *Server) IssueEnrollmentCode(ctx context.Context, a Actor, fqdn string) (Enrollment, error) {
	now := s.now()
	code := creds.NewEnrollmentCode()
	expires := now.Add(EnrollmentTTL)
	err := s.st.InTx(ctx, func(q *store.Q) error {
		h, err := q.GetHost(ctx, fqdn)
		if err != nil {
			return err
		}
		if h.State == "retired" {
			return fmt.Errorf("host %s is retired: %w", fqdn, ErrConflict)
		}
		if err := q.CreateEnrollmentCode(ctx, newID(), fqdn, creds.HashToken(creds.NormaliseCode(code)), now, expires, a.Name); err != nil {
			return err
		}
		return q.Audit(ctx, store.AuditEntry{Actor: a.Name, Source: a.Source, Action: "enrollment.code_issued", FQDN: fqdn, Detail: map[string]any{"expires_at": expires}})
	})
	if err != nil {
		return Enrollment{}, err
	}
	return Enrollment{FQDN: fqdn, Code: code, ExpiresAt: expires, InstallCommand: s.installCommand(fqdn, code)}, nil
}

// SetJobInput creates or replaces one job.
type SetJobInput struct {
	Base    *int64     `json:"base_revision,omitempty"`
	Message string     `json:"message"`
	Job     config.Job `json:"job"`
}

// SetJob saves a revision with the job created or replaced. A save from a stale base is refused.
func (s *Server) SetJob(ctx context.Context, a Actor, fqdn, jobID string, in SetJobInput) (int64, error) {
	if in.Job.Engine == "" {
		in.Job.Engine = config.EngineRestic
	}
	if strings.TrimSpace(in.Message) == "" {
		in.Message = fmt.Sprintf("Set job %s on %s", jobID, fqdn)
	}
	var revNum int64
	err := s.st.InTx(ctx, func(q *store.Q) error {
		head, err := s.head(ctx, q)
		if err != nil {
			return err
		}
		if in.Base != nil && *in.Base != head.Number {
			return ErrConflict
		}
		h, ok := head.Config.Hosts[fqdn]
		if !ok {
			return fmt.Errorf("host %s: %w", fqdn, ErrNotFound)
		}
		doc := head.Config.Clone()
		h = doc.Hosts[fqdn]
		_, existed := h.Jobs[jobID]
		h.Jobs[jobID] = in.Job
		doc.Hosts[fqdn] = h
		if p := doc.Validate(); len(p) > 0 {
			return &ValidationError{p}
		}
		rev, err := q.SaveRevision(ctx, store.SaveRevision{Base: head.Number, Actor: a.Name, Source: a.Source, Message: in.Message, Config: doc, HostSHAs: s.hostSHAs(doc)})
		if err != nil {
			return err
		}
		revNum = rev.Number
		action := "job.add"
		if existed {
			action = "job.change"
		}
		if err := q.Audit(ctx, store.AuditEntry{Actor: a.Name, Source: a.Source, Action: action, FQDN: fqdn, JobID: jobID, Revision: rev.Number,
			Detail: map[string]any{"message": in.Message}}); err != nil {
			return err
		}
		return q.NotifyHost(ctx, fqdn)
	})
	return revNum, err
}

// RunNow queues "back up now" for one job.
func (s *Server) RunNow(ctx context.Context, a Actor, fqdn, jobID string) (string, error) {
	id := newID()
	err := s.st.InTx(ctx, func(q *store.Q) error {
		head, err := s.head(ctx, q)
		if err != nil {
			return err
		}
		if _, ok := head.Config.Hosts[fqdn].Jobs[jobID]; !ok {
			return fmt.Errorf("job %s on %s: %w", jobID, fqdn, ErrNotFound)
		}
		if err := q.EnqueueCommand(ctx, store.Command{ID: id, FQDN: fqdn, Kind: proto.CommandBackupNow, JobID: jobID, CreatedAt: s.now(), CreatedBy: a.Name}); err != nil {
			return err
		}
		if err := q.Audit(ctx, store.AuditEntry{Actor: a.Name, Source: a.Source, Action: "backup.run_now", FQDN: fqdn, JobID: jobID, Detail: map[string]any{"command": id}}); err != nil {
			return err
		}
		return q.NotifyHost(ctx, fqdn)
	})
	return id, err
}

/* ---------------- views ---------------- */

// JobView is one job as the API shows it.
type JobView struct {
	ID            string           `json:"id"`
	Paths         []string         `json:"paths"`
	Excludes      []string         `json:"excludes,omitempty"`
	Schedule      any              `json:"schedule"`
	Retention     config.Retention `json:"retention"`
	NextRuns      []time.Time      `json:"next_runs"`
	LongestGap    string           `json:"longest_gap"`
	WorstCaseLoss string           `json:"worst_case_loss"`
	StaleAfter    string           `json:"stale_after"`
	Status        *store.JobStatus `json:"status,omitempty"`
}

// HostView is one host as the API shows it.
type HostView struct {
	store.Host
	Progress        string       `json:"progress"` // waiting, connected, first_backup_done
	Agent           *store.Agent `json:"agent,omitempty"`
	Online          bool         `json:"online"`
	DesiredRevision int64        `json:"desired_revision"`
	DesiredSHA256   string       `json:"desired_sha256"`
	InSync          bool         `json:"in_sync"`
	// NextCheck is when the server's daily restic check of this host starts: the middle of the day's
	// longest quiet stretch. It is absent for a host that is not enrolled, or when checks are off.
	NextCheck *time.Time `json:"next_check,omitempty"`
	Jobs      []JobView  `json:"jobs"`
}

// OnlineWithin is how recently an agent must have polled to count as online.
const OnlineWithin = 90 * time.Second

// Hosts returns every host with its agent, drift and jobs.
func (s *Server) Hosts(ctx context.Context) ([]HostView, error) {
	q := s.st.Q()
	hosts, err := q.ListHosts(ctx)
	if err != nil {
		return nil, err
	}
	head, err := s.head(ctx, q)
	if err != nil {
		return nil, err
	}
	agents, err := q.ListAgents(ctx)
	if err != nil {
		return nil, err
	}
	byHost := map[string]store.Agent{}
	for _, a := range agents {
		byHost[a.FQDN] = a
	}
	statuses, err := q.ListJobStatus(ctx)
	if err != nil {
		return nil, err
	}
	lastCheck, err := q.LastMaintenanceStart(ctx, "check")
	if err != nil {
		return nil, err
	}
	now := s.now()
	out := make([]HostView, 0, len(hosts))
	for _, h := range hosts {
		v := HostView{Host: h, Jobs: []JobView{}}
		switch {
		case h.FirstBackupAt != nil:
			v.Progress = "first_backup_done"
		case h.ConnectedAt != nil:
			v.Progress = "connected"
		default:
			v.Progress = "waiting"
		}
		if a, ok := byHost[h.FQDN]; ok {
			a := a
			v.Agent = &a
			v.Online = a.LastSeenAt != nil && now.Sub(*a.LastSeenAt) < OnlineWithin
		}
		if rev, sha, err := q.HostDesired(ctx, h.FQDN); err == nil {
			v.DesiredRevision, v.DesiredSHA256 = rev, sha
			v.InSync = v.Agent != nil && v.Agent.AppliedSHA256 == sha
		}
		hc := head.Config.Hosts[h.FQDN]
		if h.State == "enrolled" && s.set.Maintenance.Check == CheckDaily {
			next := s.nextCheck(hc, lastCheck[h.FQDN], now)
			v.NextCheck = &next
		}
		ids := make([]string, 0, len(hc.Jobs))
		for id := range hc.Jobs {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			j := hc.Jobs[id]
			loc, _ := j.Schedule.Location(s.zone)
			jv := JobView{ID: id, Paths: j.Paths, Excludes: j.Excludes, Schedule: j.Schedule, Retention: j.Retention,
				NextRuns: j.Schedule.NextN(now, 5, loc), LongestGap: j.Schedule.LongestGap().String(),
				WorstCaseLoss: j.Schedule.WorstCaseLoss().String(), StaleAfter: j.Schedule.StaleAfter().String()}
			for i := range statuses {
				if statuses[i].FQDN == h.FQDN && statuses[i].JobID == id {
					jv.Status = &statuses[i]
				}
			}
			v.Jobs = append(v.Jobs, jv)
		}
		out = append(out, v)
	}
	return out, nil
}

// HostEvents returns a host's most recent runs, newest first.
func (s *Server) HostEvents(ctx context.Context, fqdn string, limit int) ([]store.Event, error) {
	evs, err := s.st.Q().ListEvents(ctx, fqdn, limit)
	if evs == nil {
		evs = []store.Event{}
	}
	return evs, err
}

// Host returns one host.
func (s *Server) Host(ctx context.Context, fqdn string) (HostView, error) {
	all, err := s.Hosts(ctx)
	if err != nil {
		return HostView{}, err
	}
	for _, h := range all {
		if h.FQDN == fqdn {
			return h, nil
		}
	}
	return HostView{}, ErrNotFound
}

/* ---------------- escrow ---------------- */

// EscrowHost is one host's credentials in an escrow bundle.
type EscrowHost struct {
	FQDN              string `json:"fqdn"`
	Repository        string `json:"repository"`
	RepoPassword      string `json:"repo_password"`
	S3AccessKeyID     string `json:"s3_access_key_id"`
	S3SecretAccessKey string `json:"s3_secret_access_key"`
}

// EscrowBundle is what escrow export returns. Its encrypted format is still open (plan-m1 Q5); M1
// returns it in the clear on the root-only socket, and the CLI writes it to a 0600 file.
type EscrowBundle struct {
	CreatedAt time.Time    `json:"created_at"`
	ExportID  int64        `json:"export_id"`
	Hosts     []EscrowHost `json:"hosts"`
}

// EscrowExport returns every host's credentials and records the export.
func (s *Server) EscrowExport(ctx context.Context, a Actor) (EscrowBundle, error) {
	all, err := s.st.Q().AllCredentials(ctx)
	if err != nil {
		return EscrowBundle{}, err
	}
	b := EscrowBundle{CreatedAt: s.now()}
	var fqdns []string
	for _, c := range all {
		pw, err1 := s.seal.Open(c.RepoPassword, c.FQDN)
		sec, err2 := s.seal.Open(c.S3Secret, c.FQDN)
		if err := errors.Join(err1, err2); err != nil {
			return EscrowBundle{}, fmt.Errorf("%s: %w", c.FQDN, err)
		}
		b.Hosts = append(b.Hosts, EscrowHost{FQDN: c.FQDN, Repository: c.Repository, RepoPassword: pw, S3AccessKeyID: c.S3AccessKeyID, S3SecretAccessKey: sec})
		fqdns = append(fqdns, c.FQDN)
	}
	err = s.st.InTx(ctx, func(q *store.Q) error {
		id, err := q.RecordEscrowExport(ctx, a.Name, fqdns, creds.HashToken(strings.Join(fqdns, ",")+b.CreatedAt.String()))
		if err != nil {
			return err
		}
		b.ExportID = id
		return q.Audit(ctx, store.AuditEntry{Actor: a.Name, Source: a.Source, Action: "escrow.exported", Detail: map[string]any{"hosts": len(fqdns), "export_id": id}})
	})
	return b, err
}
