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
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/kabsdk/holdfast/internal/access"
	"github.com/kabsdk/holdfast/internal/checks"
	"github.com/kabsdk/holdfast/internal/config"
	"github.com/kabsdk/holdfast/internal/gateway"
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
	gw       *gateway.Gateway // restic's REST protocol for the hosts (decision 041); nil in a vault
	gwKey    storage.Key      // the gateway's key, which the storage contract tests
	prober   storage.Prober
	bucket   storage.Bucket // size, escrow files and decommission, with the maintenance key
	loc      storage.Location
	maintKey storage.Key
	zone     *time.Location
	log      *slog.Logger
	hub      *hub
	now      func() time.Time

	runCtx    context.Context // set by Run; background work stops with it
	refresher catalogueRefresher
	maintBusy busyHosts  // one forget-or-prune at a time per host
	repoLock  repoLocks  // a vault's copy, or a forget-or-prune: never both at once
	oidc      oidcClient // the server's side of the OIDC provider (decision 034)
	op        operating  // escrow, size and decommission (operating.go)
	exp       exporting  // the nightly export (rebuild.go)
	// minProtocol is the oldest agent protocol accepted (protocol.go): proto.MinProtocol, or a test's.
	minProtocol int
	vault       vaultState // in the off-site instance only (vault.go)

	// Off-site copies (offsite.go), set by AttachOffsite; all nil or empty without offsite: in server.yaml.
	offsite      *Server          // in the server: its off-site instance
	source       *Server          // in the off-site instance: the server
	offsiteGW    *gateway.Gateway // the off-site repositories, read-only, for restores from off-site
	copierSource string           // the loopback endpoint the copier reads the primary repositories from
	offsiteAPI   [2]http.Handler  // the off-site instance's API: [0] for people, [1] for the root socket
}

// background is the context for work that outlives a request, such as a catalogue refresh.
func (s *Server) background() context.Context {
	if s.runCtx != nil {
		return s.runCtx
	}
	return context.Background()
}

// Deps are what New needs besides the settings. Tests replace parts of them.
type Deps struct {
	Store       *store.Store
	Sealer      *creds.Sealer
	Prober      storage.Prober // tests the storage contract; S3Prober when nil
	Bucket      storage.Bucket // S3Bucket when nil
	Maintenance storage.Key
	Gateway     storage.Key // the gateway key (decision 041); not in a vault
	Logger      *slog.Logger
	Now         func() time.Time
	// MinProtocol raises the oldest agent protocol accepted, for the end-to-end tests only; 0 is
	// proto.MinProtocol.
	MinProtocol int
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
		d.Prober = storage.S3Prober{Location: loc, Maint: d.Maintenance, Escrow: set.Escrow.AgeRecipient != ""}
	}
	if d.Bucket == nil {
		d.Bucket = storage.S3Bucket{Location: loc, Maint: d.Maintenance}
	}
	if d.MinProtocol < proto.MinProtocol {
		d.MinProtocol = proto.MinProtocol
	}
	s := &Server{set: set, st: d.Store, seal: d.Sealer, gwKey: d.Gateway, prober: d.Prober, bucket: d.Bucket, maintKey: d.Maintenance,
		loc: loc, zone: zone, log: d.Logger, hub: newHub(), now: d.Now, minProtocol: d.MinProtocol,
		refresher: catalogueRefresher{running: map[string]bool{}, again: map[string]bool{}}}
	if !set.IsVault() {
		gw, err := gateway.New(gateway.Options{Endpoint: loc.Endpoint, Bucket: loc.Bucket, Region: loc.Region,
			AccessKeyID: d.Gateway.AccessKeyID, SecretAccessKey: d.Gateway.SecretAccessKey,
			Access: s.gatewayAccess, Refused: s.gatewayRefused, Log: d.Logger})
		if err != nil {
			return nil, err
		}
		s.gw = gw
	}
	return s, nil
}

// gatewayAccess is what a host may do with a repository through the gateway: its own, and another host's only to
// read it under a grant, for a restore onto it (decision 041).
func (s *Server) gatewayAccess(ctx context.Context, host, repo string) gateway.Access {
	if host == repo {
		return gateway.Own
	}
	if ok, err := s.st.Q().HasGrant(ctx, host, repo, store.SitePrimary, s.now()); err == nil && ok {
		return gateway.ReadOnly
	}
	return gateway.NoAccess
}

// gatewayRefused audits a refusal, in the host's history and, when it was another host's repository, in that one's.
func (s *Server) gatewayRefused(ctx context.Context, host string, r gateway.Request, ref *gateway.Refusal) {
	s.log.Warn("the gateway refused a host's request", "host", host, "repository", r.Repo, "op", r.Op, "kind", r.Kind, "reason", ref.Reason)
	detail := map[string]any{"repository": r.Repo, "op": r.Op, "kind": r.Kind, "file": r.Name, "reason": ref.Reason, "message": ref.Message}
	q := s.st.Q()
	_ = q.Audit(ctx, store.AuditEntry{Actor: "agent:" + host, Source: "agent", Action: "gateway.refused", FQDN: host, Detail: detail})
	if r.Repo != "" && r.Repo != host {
		if _, err := q.GetHost(ctx, r.Repo); err == nil {
			_ = q.Audit(ctx, store.AuditEntry{Actor: "agent:" + host, Source: "agent", Action: "gateway.refused", FQDN: r.Repo, Detail: detail})
		}
	}
}

// Run starts the background work: the notification listener and the maintenance loop.
func (s *Server) Run(ctx context.Context) {
	s.runCtx = ctx
	// A rollback runs in this process: one a stopped server left is not resumed (decision 038).
	if err := s.st.Q().AbandonRollbacks(ctx, s.now()); err != nil {
		s.log.Error("marking abandoned rollbacks", "err", err)
	}
	go s.hub.listen(ctx, s.st.Pool(), s.log)
	go s.maintenanceLoop(ctx)
	go s.vaultLoop(ctx) // in the vault role only
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
	hc := proto.HostConfig{FQDN: fqdn, Repository: gateway.RepoURL(s.set.PublicURL, fqdn), Jobs: []proto.JobConfig{}}
	h, ok := doc.Hosts[fqdn]
	if !ok {
		hc.SHA256 = hc.ComputeSHA256() // a removed host's agent gets this empty config (plan-m4 Q7)
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
		hc.Jobs = append(hc.Jobs, proto.JobConfig{ID: id, Engine: j.Engine, Paths: j.Paths, Excludes: j.Excludes, Schedule: sch, LimitUploadKiB: j.LimitUploadKiB,
			Dump: j.Dump})
	}
	if s.set.Maintenance.Check == CheckDaily {
		hc.Check = &proto.CheckConfig{CycleDays: s.set.Maintenance.CheckCycleDays}
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
		doc := config.New() // off-site too: the server's bindings replace the defaults at the first sync
		return store.Revision{Number: 0, Config: doc}, nil
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

// probeStorage tests the storage contract on a host's prefix. In the server it tests the gateway key, which reads and
// writes for every host (decision 041), and the maintenance key; off-site, the off-site key alone.
func (s *Server) probeStorage(ctx context.Context, fqdn string) storage.Report {
	var rep storage.Report
	if s.set.IsVault() {
		rep = s.prober.ProbeMaintenance(ctx)
	} else {
		rep = s.prober.Probe(ctx, fqdn, s.gwKey)
	}
	rep.CheckedAt = s.now()
	return rep
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
// policies may have changed since the last test), records the result, and initialises the
// repository once the contract holds.
func (s *Server) ensureStorage(ctx context.Context, fqdn string) (storage.Report, error) {
	rep := s.probeStorage(ctx, fqdn)
	s.recordStorage(ctx, fqdn, rep)
	if !rep.OK() {
		return rep, nil
	}
	return rep, s.initRepository(ctx, fqdn)
}

// StorageError refuses what the storage contract does not allow. The API answers 422 with the
// report, so the UI and the CLI show the same findings.
type StorageError struct{ Report storage.Report }

func (e *StorageError) Error() string {
	return "the storage does not meet the contract (docs/storage.md): " + e.Report.Summary()
}

/* ---------------- operations (each is an /api/v1 endpoint) ---------------- */

// AddHostInput adds a host, optionally with its first job.
type AddHostInput struct {
	FQDN    string      `json:"fqdn"`
	Message string      `json:"message"`
	JobID   string      `json:"job_id,omitempty"`
	Job     *config.Job `json:"job,omitempty"`
	Base    *int64      `json:"base_revision,omitempty"` // omitted: the current head
	// Accept lists the warning codes the person accepts for the job (spec §4.5).
	Accept []string `json:"accept,omitempty"`
}

// Enrollment is what a person needs to enroll a host: the one-line install, which installs the agent's packages and
// enrolls (decision 044), and the enrollment command alone, for a host that has the packages already.
type Enrollment struct {
	FQDN           string    `json:"fqdn"`
	Code           string    `json:"code"`
	ExpiresAt      time.Time `json:"expires_at"`
	InstallScript  string    `json:"install_script"`
	InstallCommand string    `json:"install_command"`
}

// Releases is where Holdfast's releases are published: install.sh, VERSION, SHA256SUMS and the packages.
const Releases = "https://github.com/kabsdk/holdfast/releases"

func (s *Server) enrollment(fqdn, code string, expires time.Time) Enrollment {
	return Enrollment{FQDN: fqdn, Code: code, ExpiresAt: expires, InstallScript: s.installScript(fqdn, code), InstallCommand: s.installCommand(fqdn, code)}
}

// installScript is the one line to run on the host, as k3s's (decision 044): it installs the packages of this server's
// release, from download_url or GitHub, and enrolls with the code.
func (s *Server) installScript(fqdn, code string) string {
	vars := []string{"HOLDFAST_SERVER=" + shellWord(s.set.PublicURL), "HOLDFAST_FQDN=" + shellWord(fqdn), "HOLDFAST_CODE=" + shellWord(code)}
	if s.set.CASHA256 != "" {
		vars = append(vars, "HOLDFAST_CA_SHA256="+shellWord(s.set.CASHA256))
	}
	base := strings.TrimSuffix(s.set.DownloadURL, "/")
	switch {
	case base != "":
		vars = append(vars, "HOLDFAST_DOWNLOAD_URL="+shellWord(base))
	case Version != "" && Version != "dev":
		base = Releases + "/download/v" + Version
		vars = append(vars, "HOLDFAST_VERSION="+shellWord(Version))
	default:
		base = Releases + "/latest/download"
	}
	return "curl -sfL " + shellWord(base+"/install.sh") + " | " + strings.Join(vars, " ") + " sh -"
}

// AddHostResult is the outcome of AddHost.
type AddHostResult struct {
	Revision   int64          `json:"revision"`
	Enrollment Enrollment     `json:"enrollment"`
	Storage    storage.Report `json:"storage"`
	Warning    string         `json:"warning,omitempty"`
	// Findings are the checks that fired on the job, if one was added: accepted warnings, and info.
	Findings []checks.Finding `json:"findings"`
	Accepted []string         `json:"accepted_codes"`
}

func (s *Server) installCommand(fqdn, code string) string {
	cmd := fmt.Sprintf("holdfast-agent enroll --server %s --fqdn %s --code %s", s.set.PublicURL, fqdn, code)
	if s.set.CASHA256 != "" {
		cmd += " --ca-sha256 " + s.set.CASHA256
	}
	return cmd
}

// AddHost tests the storage contract on the host's prefix, with the gateway key, and refuses a host whose storage
// fails it. A host needs no S3 key of its own (decision 041). It then creates a revision with the host, its
// credentials and a 15-minute enrollment code, all in one transaction, and initialises the repository. An
// initialisation failure is reported but not fatal: enrollment does it again before it uses the code.
func (s *Server) AddHost(ctx context.Context, a Actor, in AddHostInput) (AddHostResult, error) {
	if s.set.IsVault() {
		return AddHostResult{}, errVaultHosts
	}
	in.FQDN = strings.TrimSpace(in.FQDN)
	if err := config.ValidFQDN(in.FQDN); err != nil {
		return AddHostResult{}, &ValidationError{config.Problems{{Path: "fqdn", Message: err.Error()}}}
	}
	if strings.TrimSpace(in.Message) == "" {
		in.Message = "Add host " + in.FQDN
	}
	// Checked on the new FQDN against the bindings' patterns, before anything says whether it exists.
	if err := s.allowed(ctx, a, access.ConfigStorage, in.FQDN); err != nil {
		return AddHostResult{}, err
	}
	// A removed host comes back with its repository, its password and its history (spec §3.5).
	// A decommissioned host starts over: its repository is gone, so it gets a new password.
	revive, fresh := false, false
	if h, err := s.st.Q().GetHost(ctx, in.FQDN); err == nil {
		switch {
		case h.State != "retired":
			return AddHostResult{}, fmt.Errorf("host %s exists already: %w", in.FQDN, ErrConflict)
		case h.Decommission != nil && h.Decommission.DoneAt == nil:
			return AddHostResult{}, fmt.Errorf("host %s is waiting to be decommissioned; cancel that first (holdfast host decommission %s --cancel): %w", in.FQDN, in.FQDN, ErrConflict)
		}
		revive, fresh = true, decommissioned(h)
	}
	// The job's checks come before the storage test, so a job that would be refused costs no S3 calls. A new
	// host has no runs and no snapshots, so S002 and R104 cannot fire.
	fs := []checks.Finding{}
	if in.Job != nil {
		if in.Job.Engine == "" {
			in.Job.Engine = config.EngineRestic
		}
		if p := withJob(config.New(), in.FQDN, in.JobID, *in.Job).Validate(); len(p) > 0 {
			return AddHostResult{}, &ValidationError{p}
		}
		fs = append(checks.Job(*in.Job, checks.History{}), s.dumpFindings(ctx, in.FQDN, *in.Job)...)
		checks.Sort(fs)
		if blocking := checks.Blocking(fs, in.Accept); len(blocking) > 0 {
			return AddHostResult{}, &ChecksError{Findings: fs, Blocking: blocking}
		}
	}
	accepted := checks.Accepted(fs, in.Accept)
	rep := s.probeStorage(ctx, in.FQDN)
	if !rep.OK() {
		return AddHostResult{}, &StorageError{rep}
	}
	now := s.now()
	code := creds.NewEnrollmentCode()
	var res AddHostResult
	err := s.st.InTx(ctx, func(q *store.Q) error {
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
			h.Jobs[in.JobID] = *in.Job
		}
		doc.Hosts[in.FQDN] = h
		if p := doc.Validate(); len(p) > 0 {
			return &ValidationError{p}
		}
		rev, err := q.SaveRevision(ctx, store.SaveRevision{Base: head.Number, Actor: a.Name, Source: a.Source, Message: in.Message,
			Accepted: accepted, Config: doc, HostSHAs: s.hostSHAs(doc)})
		if err != nil {
			return err
		}
		switch {
		case !revive:
			if err := q.CreateHost(ctx, in.FQDN, rev.Number, now); err != nil {
				return err
			}
			if err := q.PutCredentials(ctx, store.SealedCredentials{FQDN: in.FQDN, Version: 1, Repository: s.loc.RepoURL(in.FQDN),
				RepoPassword: s.seal.Seal(creds.NewPassword(), in.FQDN), CreatedAt: now}); err != nil {
				return err
			}
		default:
			if err := q.ReviveHost(ctx, in.FQDN, rev.Number); err != nil {
				return err
			}
			if fresh { // the repository password stays, unless the repository was decommissioned: it is what opens it
				c, err := q.GetCredentials(ctx, in.FQDN)
				if err != nil {
					return err
				}
				c.Version++
				c.RepoPassword, c.CreatedAt = s.seal.Seal(creds.NewPassword(), in.FQDN), now
				if err := q.PutCredentials(ctx, c); err != nil {
					return err
				}
			}
		}
		if err := q.RecordStorageCheck(ctx, in.FQDN, rep); err != nil {
			return err
		}
		expires := now.Add(EnrollmentTTL)
		if err := q.CreateEnrollmentCode(ctx, newID(), in.FQDN, creds.HashToken(creds.NormaliseCode(code)), now, expires, a.Name); err != nil {
			return err
		}
		detail := map[string]any{"message": in.Message, "back_after_removal": revive}
		if fresh {
			detail["after_decommission"] = true
		}
		if in.Job != nil {
			detail["job"] = in.JobID
			detail["accepted_codes"] = accepted
		}
		if err := q.Audit(ctx, store.AuditEntry{Actor: a.Name, Source: a.Source, Action: "host.add", FQDN: in.FQDN, Revision: rev.Number, Detail: detail}); err != nil {
			return err
		}
		if err := q.Audit(ctx, store.AuditEntry{Actor: a.Name, Source: a.Source, Action: "enrollment.code_issued", FQDN: in.FQDN,
			Detail: map[string]any{"expires_at": expires}}); err != nil {
			return err
		}
		res = AddHostResult{Revision: rev.Number, Enrollment: s.enrollment(in.FQDN, code, expires),
			Storage: rep, Findings: fs, Accepted: accepted}
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
	// The password goes into escrow at once (decision 036). A failure is tried again in the background.
	if err := s.writeEscrow(ctx, in.FQDN); err != nil {
		res.Warning = strings.TrimPrefix(res.Warning+"; the escrow file could not be written yet ("+err.Error()+"); it is tried again", "; ")
	}
	return res, nil
}

func (s *Server) initRepository(ctx context.Context, fqdn string) error {
	r, cleanup, err := s.maintRunner(ctx, fqdn)
	if err != nil {
		return err
	}
	defer cleanup()
	created, err := r.Init(ctx)
	if err != nil {
		return fmt.Errorf("initialising the repository of %s: %w", fqdn, err)
	}
	if !created {
		return nil
	}
	// Holdfast made this repository, so its keys are Holdfast's own; a later one was added outside it
	// (decision 032). A repository made again, after the old one was lost, starts over in the same way, and
	// the audit log says so.
	r.NoLock = true
	var ids []string
	if keys, err := r.Keys(ctx); err == nil {
		for _, k := range keys {
			ids = append(ids, k.ID)
		}
	} else {
		s.log.Warn("listing the new repository's keys", "host", fqdn, "err", err)
	}
	return s.st.InTx(ctx, func(q *store.Q) error {
		again, err := q.ResetRepositoryKeys(ctx, fqdn, ids)
		if err != nil {
			return err
		}
		detail := map[string]any{"repository": r.Repo, "again": again}
		if again {
			s.log.Warn("the repository was gone, so Holdfast initialised it again", "host", fqdn)
		}
		return q.Audit(ctx, store.AuditEntry{Actor: systemActor.Name, Source: systemActor.Source, Action: "repository.initialised", FQDN: fqdn, Detail: detail})
	})
}

// IssueEnrollmentCode gives a host a new 15-minute code and revokes the open one. It also serves
// re-enrollment of a host that was enrolled before (plan-m1 Q9): the repository and its password stay.
func (s *Server) IssueEnrollmentCode(ctx context.Context, a Actor, fqdn string) (Enrollment, error) {
	if s.set.IsVault() {
		return Enrollment{}, fmt.Errorf("no agent enrolls off-site: agents enroll with the server: %w", ErrConflict)
	}
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
	return s.enrollment(fqdn, code, expires), nil
}

// SetJobInput creates or replaces one job. Accept lists the warning codes the person accepts (spec §4.5).
type SetJobInput struct {
	Base    *int64     `json:"base_revision,omitempty"`
	Message string     `json:"message"`
	Job     config.Job `json:"job"`
	Accept  []string   `json:"accept,omitempty"`
}

// SetJobResult is the saved revision, and every check that fired: the accepted warnings, and info.
type SetJobResult struct {
	Revision int64            `json:"revision"`
	Findings []checks.Finding `json:"findings"`
	Accepted []string         `json:"accepted_codes"`
}

// SetJob saves a revision with the job created or replaced, after the checks (spec §4.5). A save from a
// stale base is refused, and so is a save that a check blocks.
func (s *Server) SetJob(ctx context.Context, a Actor, fqdn, jobID string, in SetJobInput) (SetJobResult, error) {
	if in.Job.Engine == "" {
		in.Job.Engine = config.EngineRestic
	}
	if strings.TrimSpace(in.Message) == "" {
		in.Message = fmt.Sprintf("Set job %s on %s", jobID, fqdn)
	}
	// The checks run before the transaction, since R104 asks restic. The transaction then makes sure the
	// job they compared against is still the one in force.
	before, err := s.head(ctx, s.st.Q())
	if err != nil {
		return SetJobResult{}, err
	}
	if in.Base != nil && *in.Base != before.Number {
		return SetJobResult{}, ErrConflict
	}
	h, ok := before.Config.Hosts[fqdn]
	if !ok {
		return SetJobResult{}, fmt.Errorf("host %s: %w", fqdn, ErrNotFound)
	}
	var old *config.Job
	if j, ok := h.Jobs[jobID]; ok {
		old = &j
	}
	// In a vault only the off-site retention is the vault's to set; the rest of the job is the source's.
	if s.set.IsVault() {
		if old == nil {
			return SetJobResult{}, errVaultHosts
		}
		j := *old
		j.Retention = in.Job.Retention
		in.Job = j
	}
	if p := withJob(before.Config, fqdn, jobID, in.Job).Validate(); len(p) > 0 {
		return SetJobResult{}, &ValidationError{p}
	}
	fs := s.jobFindings(ctx, fqdn, jobID, old, in.Job)
	if blocking := checks.Blocking(fs, in.Accept); len(blocking) > 0 {
		return SetJobResult{}, &ChecksError{Findings: fs, Blocking: blocking}
	}
	res := SetJobResult{Findings: fs, Accepted: checks.Accepted(fs, in.Accept)}
	// Accepting a retention cut is retention.cut, an admin's (spec §6.2).
	if slices.Contains(res.Accepted, "R104") {
		if err := s.allowed(ctx, a, access.RetentionCut, fqdn); err != nil {
			return SetJobResult{}, err
		}
	}
	err = s.st.InTx(ctx, func(q *store.Q) error {
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
		cur, existed := h.Jobs[jobID]
		if existed != (old != nil) || (existed && !reflect.DeepEqual(cur, *old)) {
			return fmt.Errorf("job %s on %s changed while it was being checked: %w", jobID, fqdn, ErrConflict)
		}
		doc := withJob(head.Config, fqdn, jobID, in.Job)
		if p := doc.Validate(); len(p) > 0 {
			return &ValidationError{p}
		}
		rev, err := q.SaveRevision(ctx, store.SaveRevision{Base: head.Number, Actor: a.Name, Source: a.Source, Message: in.Message,
			Accepted: res.Accepted, Config: doc, HostSHAs: s.hostSHAs(doc)})
		if err != nil {
			return err
		}
		res.Revision = rev.Number
		action := "job.add"
		if existed {
			action = "job.change"
		}
		if err := q.Audit(ctx, store.AuditEntry{Actor: a.Name, Source: a.Source, Action: action, FQDN: fqdn, JobID: jobID, Revision: rev.Number,
			Detail: map[string]any{"message": in.Message, "accepted_codes": res.Accepted}}); err != nil {
			return err
		}
		return q.NotifyHost(ctx, fqdn)
	})
	if err != nil {
		return SetJobResult{}, err
	}
	return res, nil
}

// withJob returns a copy of doc with the host's job created or replaced.
func withJob(doc config.Document, fqdn, jobID string, j config.Job) config.Document {
	doc = doc.Clone()
	h := doc.Hosts[fqdn]
	if h.Jobs == nil {
		h.Jobs = map[string]config.Job{}
	}
	h.Jobs[jobID] = j
	doc.Hosts[fqdn] = h
	return doc
}

// RunNow queues "back up now" for one job.
func (s *Server) RunNow(ctx context.Context, a Actor, fqdn, jobID string) (string, error) {
	if s.set.IsVault() {
		return "", fmt.Errorf("nothing is backed up off-site: the copies follow the agents' backups, every offsite.poll: %w", ErrConflict)
	}
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
	ID             string           `json:"id"`
	Paths          []string         `json:"paths"`
	Excludes       []string         `json:"excludes,omitempty"`
	Schedule       any              `json:"schedule"`
	Retention      config.Retention `json:"retention"`
	LimitUploadKiB int              `json:"limit_upload_kib,omitempty"`
	Dump           string           `json:"dump,omitempty"`
	NextRuns       []time.Time      `json:"next_runs"`
	LongestGap     string           `json:"longest_gap"`
	WorstCaseLoss  string           `json:"worst_case_loss"`
	StaleAfter     string           `json:"stale_after"`
	Status         *store.JobStatus `json:"status,omitempty"`
	Stale          bool             `json:"stale"` // no success for more than StaleAfter
	// In a vault (decision 039): the source's retention, Exclude and Retired, and how far the newest copy is behind
	// the source's newest snapshot. Lagging is that lag past StaleAfter, which alerts.
	Primary  *config.Retention `json:"primary_retention,omitempty"`
	Excluded bool              `json:"excluded,omitempty"`
	Retired  bool              `json:"retired,omitempty"`
	BehindS  *int64            `json:"behind_s,omitempty"`
	Lagging  bool              `json:"lagging,omitempty"`
}

// HostView is one host as the API shows it.
type HostView struct {
	store.Host
	Progress        string       `json:"progress"` // waiting, connected, first_backup_done
	Agent           *store.Agent `json:"agent,omitempty"`
	Online          bool         `json:"online"`
	DesiredRevision int64        `json:"desired_revision"`
	DesiredSHA256   string       `json:"desired_sha256"` // what the host's agent should run now
	InSync          bool         `json:"in_sync"`
	// ResticWarning is set when the agent runs another restic than the one Holdfast is tested with.
	ResticWarning string `json:"restic_warning,omitempty"`
	// AgentTooOld says the agent speaks an older protocol than this server accepts (protocol.go).
	AgentTooOld bool `json:"agent_too_old"`
	// RollingBack says a rollback of the repository is under way (rollback.go).
	RollingBack bool `json:"rolling_back"`
	// The snapshot catalogue's counts, and its banners (decision 032): snapshots in the repository that
	// Holdfast did not make, and snapshots that disappeared without Holdfast deleting them.
	Snapshots         int       `json:"snapshots"`
	NotMadeByHoldfast int       `json:"not_made_by_holdfast"`
	Disappeared       int       `json:"disappeared"`
	ForeignKeys       []string  `json:"foreign_keys,omitempty"` // repository keys (passwords) added outside Holdfast
	Jobs              []JobView `json:"jobs"`
	// Actions are the permissions the asker has on this host (spec §6.2). The UI shows an action only if it
	// is here, and works out nothing itself (decision 028).
	Actions []string `json:"actions"`
	// Problems are what the server knows is wrong with the host (problems.go).
	Problems []Problem `json:"problems"`
	// Offsite is what the off-site instance reports of the host (vaultsrc.go); nil without offsite:.
	Offsite *HostOffsite `json:"offsite,omitempty"`
	// AheadOfServer is the head this server was restored at, when the agent runs a newer revision that no revision
	// since has replaced (plan-m9 Q6): its config is held back, and the host shows it.
	AheadOfServer *int64 `json:"ahead_of_server,omitempty"`
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
	counts, err := q.CountCatalogue(ctx)
	if err != nil {
		return nil, err
	}
	foreign, err := q.ForeignRepositoryKeys(ctx)
	if err != nil {
		return nil, err
	}
	health, err := q.RetentionHealth(ctx)
	if err != nil {
		return nil, err
	}
	retentionFailed := map[string]bool{}
	for _, r := range health {
		retentionFailed[r.FQDN] = r.LastFailed
	}
	rolling, err := q.RunningRollbacks(ctx)
	if err != nil {
		return nil, err
	}
	offsite := s.offsiteByHost(ctx)
	restored, err := q.LastRestoreFromExport(ctx)
	if err != nil {
		return nil, err
	}
	now := s.now()
	out := make([]HostView, 0, len(hosts))
	for _, h := range hosts {
		v := HostView{Host: h, Jobs: []JobView{}, RollingBack: rolling[h.FQDN], Offsite: offsite[h.FQDN]}
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
			if a.ResticVersion != "" && a.ResticVersion != restic.PinnedVersion {
				v.ResticWarning = fmt.Sprintf("this host runs restic %s; Holdfast is tested with %s, which the agent's package ships", a.ResticVersion, restic.PinnedVersion)
			}
			v.AgentTooOld = s.agentTooOld(v.Agent)
		}
		if rev, _, err := q.HostDesired(ctx, h.FQDN); err == nil {
			v.DesiredRevision = rev
		}
		// In sync means the agent runs what the server would send it now. That includes server settings
		// such as the daily check, so it is compared with the config as rendered now, not with the hash
		// stored when the revision was saved.
		c := counts[h.FQDN]
		v.Snapshots, v.NotMadeByHoldfast, v.Disappeared, v.ForeignKeys = c.Snapshots, c.NotMadeByHoldfast, c.Disappeared, foreign[h.FQDN]
		v.DesiredSHA256 = s.renderHost(head.Config, h.FQDN).SHA256
		v.InSync = v.Agent != nil && v.Agent.AppliedSHA256 == v.DesiredSHA256
		if restored != nil && v.Agent != nil && !v.InSync && v.Agent.AppliedRevision > restored.Head && v.DesiredRevision <= restored.Head {
			at := restored.Head
			v.AheadOfServer = &at
		}
		hc := head.Config.Hosts[h.FQDN]
		ids := make([]string, 0, len(hc.Jobs))
		for id := range hc.Jobs {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			j := hc.Jobs[id]
			loc, _ := j.Schedule.Location(s.zone)
			jv := JobView{ID: id, Paths: j.Paths, Excludes: j.Excludes, Schedule: j.Schedule, Retention: j.Retention, LimitUploadKiB: j.LimitUploadKiB, Dump: j.Dump,
				NextRuns: j.Schedule.NextN(now, 5, loc), LongestGap: j.Schedule.LongestGap().String(),
				WorstCaseLoss: j.Schedule.WorstCaseLoss().String(), StaleAfter: j.Schedule.StaleAfter().String()}
			for i := range statuses {
				if statuses[i].FQDN == h.FQDN && statuses[i].JobID == id {
					jv.Status = &statuses[i]
				}
			}
			// Stale: no success for twice the longest gap, the threshold of the stale alert (spec §4.1). A job
			// counts from when the host's first backup could have run.
			since := h.FirstBackupAt
			if jv.Status != nil && jv.Status.LastSuccessAt != nil {
				since = jv.Status.LastSuccessAt
			}
			jv.Stale = h.State == "enrolled" && since != nil && now.Sub(*since) > j.Schedule.StaleAfter()
			if s.set.IsVault() {
				// Off-site jobs lag rather than go stale: the agents back them up in primary (decision 039).
				jv.Primary, jv.Excluded, jv.Retired, jv.Stale = j.Primary, j.Excluded, j.Retired, false
				if behind := s.copyState(h.FQDN); behind != nil {
					if b, ok := behind[id]; ok {
						secs := int64(b / time.Second)
						jv.BehindS, jv.Lagging = &secs, !j.Excluded && !j.Retired && b > j.Schedule.StaleAfter()
					}
				}
			}
			v.Jobs = append(v.Jobs, jv)
		}
		v.Problems = problems(v, now, retentionFailed[h.FQDN], s.escrowOn())
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

// EscrowHost is one host's credentials in an escrow bundle: its repository and the password that opens it. A host
// has no S3 key (decision 041).
type EscrowHost struct {
	FQDN         string `json:"fqdn"`
	Repository   string `json:"repository"`
	RepoPassword string `json:"repo_password"`
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
		pw, err := s.seal.Open(c.RepoPassword, c.FQDN)
		if err != nil {
			return EscrowBundle{}, fmt.Errorf("%s: %w", c.FQDN, err)
		}
		b.Hosts = append(b.Hosts, EscrowHost{FQDN: c.FQDN, Repository: c.Repository, RepoPassword: pw})
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
