package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/kabsdk/holdfast/internal/access"
	"github.com/kabsdk/holdfast/internal/config"
	"github.com/kabsdk/holdfast/internal/restic"
	"github.com/kabsdk/holdfast/internal/server/creds"
	"github.com/kabsdk/holdfast/internal/server/export"
	"github.com/kabsdk/holdfast/internal/server/storage"
	"github.com/kabsdk/holdfast/internal/server/store"
)

// Replacing a lost server (M9, decisions 040 and 042). There is one server and no standby: a new one, given the same
// bucket, picks up from the nightly export.
//   - the nightly export: every table of the server, and of its off-site instance, in meta/<date>.age in the bucket,
//     encrypted to the escrow's age recipient; with off-site copies, in the off-site bucket too (plan-m9 Q4);
//   - holdfast-server restore-export loads one into a new, empty database (serverd);
//   - an agent that runs a revision newer than a restored server's head keeps it, and is shown (plan-m9 Q6);
//   - holdfast host import brings back a host the export does not have, from its escrow file (plan-m9 Q7).

const (
	exportAt    = 30 * time.Minute // after midnight in the default zone (plan-m9 Q3)
	exportRetry = time.Hour        // a failed export, or a failed write off-site, is tried again after this
	ExportKeep  = 30               // the newest exports kept in each bucket; older ones are deleted
	// ExportStaleAfter is when the lack of a written export is a problem, and an alert.
	ExportStaleAfter = 48 * time.Hour
)

// exporting is the state of the export here.
type exporting struct {
	mu      sync.Mutex  // one export at a time
	running atomic.Bool // a nightly export runs in the background
}

/* ---------------- the nightly export ---------------- */

// exportSlot is the latest 00:30 in the default zone, at or before now.
func (s *Server) exportSlot(now time.Time) time.Time {
	l := now.In(s.zone)
	slot := time.Date(l.Year(), l.Month(), l.Day(), 0, 0, 0, 0, s.zone).Add(exportAt)
	if l.Before(slot) {
		slot = time.Date(l.Year(), l.Month(), l.Day()-1, 0, 0, 0, 0, s.zone).Add(exportAt)
	}
	return slot
}

// exportDueAt says when the next export is due: at once when none was tried since the latest 00:30, which also covers
// a server that starts after missing one, or an hour after a failed one; otherwise at the next 00:30.
func (s *Server) exportDueAt(st store.ExportState, now time.Time) time.Time {
	slot := s.exportSlot(now)
	last := st.Last
	switch {
	case last == nil || last.StartedAt.Before(slot):
		return now
	case last.Error != "" || last.Offsite && last.OffsiteError != "":
		if retry := last.StartedAt.Add(exportRetry); retry.Before(slot.Add(24 * time.Hour)) {
			return retry
		}
	}
	return slot.Add(24 * time.Hour)
}

// exportDue starts the export in the background when it is due. Only the server exports: its export holds the
// off-site instance's tables too.
func (s *Server) exportDue(ctx context.Context) {
	if s.set.IsVault() || !s.escrowOn() {
		return
	}
	st, err := s.st.Q().ExportState(ctx)
	if err != nil {
		return
	}
	if now := s.now(); now.Before(s.exportDueAt(st, now)) || !s.exp.running.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer s.exp.running.Store(false)
		_, _ = s.export(ctx, systemActor)
	}()
}

// ErrNoExport refuses an export without an age recipient to encrypt it to.
var ErrNoExport = errors.New("no export is written without escrow.age_recipient in server.yaml (decision 040): set it, as for escrow")

// ExportNow writes an export at once (plan-m9 Q3: an admin's, UI, API and CLI).
func (s *Server) ExportNow(ctx context.Context, a Actor) (store.ExportRecord, error) {
	if s.set.IsVault() {
		return store.ExportRecord{}, fmt.Errorf("the server exports the off-site tables with its own: %w", ErrNotFound)
	}
	if err := s.requireEverywhere(ctx, a, access.Maintenance); err != nil {
		return store.ExportRecord{}, err
	}
	if !s.escrowOn() {
		return store.ExportRecord{}, &ValidationError{config.Problems{{Path: "escrow.age_recipient", Message: ErrNoExport.Error()}}}
	}
	return s.export(ctx, a)
}

// export writes one: it reads every table, seals them, writes meta/<date>.age to the bucket, and with off-site
// copies to the off-site bucket too, then deletes the exports beyond the newest ExportKeep in each. A failed off-site
// write leaves the export counted. Each attempt is recorded and audited.
func (s *Server) export(ctx context.Context, a Actor) (store.ExportRecord, error) {
	s.exp.mu.Lock()
	defer s.exp.mu.Unlock()
	start := s.now()
	rec := store.ExportRecord{StartedAt: start, Actor: a.Name, Name: storage.MetaName(start), Offsite: s.offsite != nil}
	data, tables, err := s.sealExport(ctx, start)
	if err == nil {
		err = s.bucket.PutMeta(ctx, rec.Name, data)
	}
	var pruned []string
	if err == nil {
		rec.Bytes = int64(len(data))
		pruned = s.pruneExports(ctx, s.bucket)
		if s.offsite != nil {
			if oerr := s.offsite.bucket.PutMeta(ctx, rec.Name, data); oerr != nil {
				rec.OffsiteError = oerr.Error()
			} else {
				pruned = append(pruned, s.pruneExports(ctx, s.offsite.bucket)...)
			}
		}
	} else {
		rec.Error = err.Error()
	}
	rec.FinishedAt = s.now()
	q := s.st.Q()
	id, rerr := q.RecordExport(ctx, rec)
	if rerr != nil {
		s.log.Error("recording an export", "err", rerr)
	}
	rec.ID = id
	detail := map[string]any{"object": storage.MetaKey(rec.Name), "bytes": rec.Bytes, "tables": tables}
	switch {
	case rec.Error != "":
		s.log.Warn("the nightly export failed", "err", rec.Error)
		_ = q.Audit(ctx, store.AuditEntry{Actor: a.Name, Source: a.Source, Action: "export.failed", Detail: map[string]any{"object": storage.MetaKey(rec.Name), "error": rec.Error}})
	default:
		if rec.Offsite {
			detail["offsite"] = rec.OffsiteError == ""
		}
		if len(pruned) > 0 {
			detail["deleted_older"] = pruned
		}
		_ = q.Audit(ctx, store.AuditEntry{Actor: a.Name, Source: a.Source, Action: "export.written", Detail: detail})
		if rec.OffsiteError != "" {
			s.log.Warn("the nightly export was not written off-site", "err", rec.OffsiteError)
			_ = q.Audit(ctx, store.AuditEntry{Actor: a.Name, Source: a.Source, Action: "export.offsite_failed",
				Detail: map[string]any{"object": storage.MetaKey(rec.Name), "error": rec.OffsiteError}})
		}
	}
	if rec.Error != "" {
		return rec, fmt.Errorf("the export failed: %s", rec.Error)
	}
	return rec, nil
}

// sealExport reads the server's tables, and the off-site instance's, and encrypts them to the age recipient. It
// returns the sealed export and how many tables it holds.
func (s *Server) sealExport(ctx context.Context, now time.Time) ([]byte, int, error) {
	c := export.Contents{Manifest: export.Manifest{HoldfastVersion: Version, CreatedAt: now.UTC(), Node: s.set.NodeName},
		Tables: map[string][]store.TableData{}}
	add := func(name string, st *store.Store) error {
		tables, version, err := st.ExportTables(ctx)
		if err != nil {
			return fmt.Errorf("reading the %s tables: %w", name, err)
		}
		c.Manifest.Schemas = append(c.Manifest.Schemas, export.Schema{Name: name, Version: version})
		c.Tables[name] = tables
		return nil
	}
	if err := add("public", s.st); err != nil {
		return nil, 0, err
	}
	if s.offsite != nil {
		if err := add(OffsiteSchema, s.offsite.st); err != nil {
			return nil, 0, err
		}
	}
	n := 0
	for _, t := range c.Tables {
		n += len(t)
	}
	files, err := s.exportFiles()
	if err != nil {
		return nil, 0, err
	}
	c.Files = files
	data, err := export.Seal(c, s.set.Escrow.AgeRecipient)
	return data, n, err
}

// ServerFiles names the server's files a rebuild needs besides its database (plan-m10 item 3), by the setting that
// gives their path: the agent listener's certificate, key and CA, which the agents pin, and the UI's certificate and
// key. The nightly export carries them, and restore-export writes them where the new server.yaml says.
func ServerFiles(set Settings) map[string]string {
	return map[string]string{"tls.cert_file": set.TLS.CertFile, "tls.key_file": set.TLS.KeyFile, "tls.ca_file": set.TLS.CAFile,
		"tls.ca_key_file": set.TLS.CAKeyFile, "ui.tls.cert_file": set.UI.TLS.CertFile, "ui.tls.key_file": set.UI.TLS.KeyFile}
}

// exportFiles reads ServerFiles, and the settings in force without the database's DSN, as server.yaml. The S3 keys,
// the database's password and the OIDC secret stay out of the export: the admin gives them again.
func (s *Server) exportFiles() (map[string][]byte, error) {
	out := map[string][]byte{}
	for name, p := range ServerFiles(s.set) {
		if p == "" {
			continue
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		out[name] = b
	}
	set := s.set
	set.DatabaseDSN = ""
	y, err := yaml.Marshal(set)
	if err != nil {
		return nil, err
	}
	out["server.yaml"] = y
	return out, nil
}

// pruneExports deletes the exports beyond the newest ExportKeep, and returns their names. Versioning keeps their old
// versions for the lifecycle rule's days. A failure is logged and tried again after the next export.
func (s *Server) pruneExports(ctx context.Context, b storage.Bucket) []string {
	names, err := b.ListMeta(ctx)
	if err != nil {
		s.log.Warn("listing the exports", "err", err)
		return nil
	}
	var gone []string
	for len(names) > ExportKeep {
		if err := b.DeleteMeta(ctx, names[0]); err != nil {
			s.log.Warn("deleting an old export", "export", names[0], "err", err)
			break
		}
		gone, names = append(gone, names[0]), names[1:]
	}
	return gone
}

// ExportsView is the Admin page's exports: whether they are on, the latest attempts, and when the next is due.
type ExportsView struct {
	On      bool `json:"on"`      // an age recipient is set
	Offsite bool `json:"offsite"` // each export is written off-site too
	Keep    int  `json:"keep"`
	store.ExportState
	NextAt   *time.Time               `json:"next_at,omitempty"`
	Recent   []store.ExportRecord     `json:"recent"`
	Restored *store.RestoreFromExport `json:"restored,omitempty"`
}

// Exports returns it.
func (s *Server) Exports(ctx context.Context) (ExportsView, error) {
	q := s.st.Q()
	v := ExportsView{On: s.escrowOn() && !s.set.IsVault(), Offsite: s.offsite != nil, Keep: ExportKeep}
	var err error
	if v.ExportState, err = q.ExportState(ctx); err != nil {
		return v, err
	}
	if v.Recent, err = q.ListExports(ctx, 30); err != nil {
		return v, err
	}
	if v.Restored, err = q.LastRestoreFromExport(ctx); err != nil {
		return v, err
	}
	if v.On {
		next := s.exportDueAt(v.ExportState, s.now())
		v.NextAt = &next
	}
	return v, nil
}

// serverProblems are the server's own, not a host's: its export (plan-m9 Q3, Q4).
func (s *Server) serverProblems(ctx context.Context) []Problem {
	ps := []Problem{}
	if s.set.IsVault() {
		return ps
	}
	// The recovery file belongs offline (plan-m10 item 3): while it is on the server, a thief of the server's disk
	// gets the key to every backup, and a lost server takes it along.
	if p := s.set.RecoveryFile; p != "" {
		if _, err := os.Stat(p); err == nil {
			ps = append(ps, Problem{Code: "recovery_file", Severity: "warning",
				Message: fmt.Sprintf("the recovery file is still on the server, at %s: copy it somewhere safe off this server, such as a password manager, then delete it here", p)})
		}
	}
	s.notes.mu.Lock()
	for _, ch := range []string{"mail", "webhook", "heartbeat"} {
		if s.notes.failed[ch] {
			ps = append(ps, Problem{Code: "notify", Severity: "warning",
				Message: fmt.Sprintf("the server's alerts by %s fail; its log says why, and holdfast notify test tries again", ch)})
		}
	}
	s.notes.mu.Unlock()
	if !s.escrowOn() {
		return append(ps, Problem{Code: "export", Severity: "warning",
			Message: "no nightly export is written, so a lost server cannot be rebuilt from one: set escrow.age_recipient in server.yaml (decision 040)"})
	}
	st, err := s.st.Q().ExportState(ctx)
	if err != nil {
		return ps
	}
	now := s.now()
	switch {
	case st.Last != nil && st.Last.Error != "":
		ps = append(ps, Problem{Code: "export", Severity: "error", Message: fmt.Sprintf("the nightly export failed at %s: %s; it is tried again every hour",
			st.Last.StartedAt.In(s.zone).Format("2006-01-02 15:04"), st.Last.Error)})
	case st.LastGood != nil && now.Sub(st.LastGood.StartedAt) > ExportStaleAfter:
		ps = append(ps, Problem{Code: "export", Severity: "error", Message: fmt.Sprintf("no nightly export was written since %s",
			st.LastGood.StartedAt.In(s.zone).Format("2006-01-02 15:04"))})
	}
	if s.offsite != nil && st.Last != nil && st.Last.Error == "" && st.Last.OffsiteError != "" {
		ps = append(ps, Problem{Code: "export_offsite", Severity: "error", Message: fmt.Sprintf("the nightly export of %s was not written off-site: %s; it is tried again every hour",
			st.Last.StartedAt.In(s.zone).Format("2006-01-02"), st.Last.OffsiteError)})
	}
	return ps
}

/* ---------------- after a restore from an export ---------------- */

// aheadOfRestore says whether an agent's config is held back: the server was restored from an export, the agent runs
// a revision newer than the head the export had, and no revision since has touched the host (plan-m9 Q6). The agent
// keeps its own config, and the host shows the problem, until the change is made again.
func (s *Server) aheadOfRestore(ctx context.Context, applied, desired int64) (bool, error) {
	r, err := s.st.Q().LastRestoreFromExport(ctx)
	if err != nil || r == nil {
		return false, err
	}
	return applied > r.Head && desired <= r.Head, nil
}

/* ---------------- a host back from its escrow file ---------------- */

// ImportHostInput is a host the server does not know, with its escrow file, decrypted (plan-m9 §2.4). The file's
// contents are never kept, logged or shown back: only its password, sealed, as for any host.
type ImportHostInput struct {
	FQDN       string `json:"fqdn"`
	EscrowFile string `json:"escrow_file"`
	Message    string `json:"message"`
}

// ImportHostResult is the outcome: the host, with its repository and password, and an enrollment code. It has no
// job: the admin sets them again.
type ImportHostResult struct {
	Revision           int64          `json:"revision"`
	Enrollment         Enrollment     `json:"enrollment"`
	Storage            storage.Report `json:"storage"`
	CredentialsVersion int            `json:"credentials_version"`
	RepositoryKeys     int            `json:"repository_keys"`
	Warning            string         `json:"warning,omitempty"`
}

// ImportHost adds a host back from its escrow file, when there is no export, or the export does not have it. It is an
// admin's (plan-m9 Q7). It checks that the file is the host's and names this server's repository for it, tests the
// storage contract, and proves that the password opens the repository before it adds anything.
func (s *Server) ImportHost(ctx context.Context, a Actor, in ImportHostInput) (ImportHostResult, error) {
	if s.set.IsVault() {
		return ImportHostResult{}, errVaultHosts
	}
	if err := s.requireEverywhere(ctx, a, access.ConfigStorage); err != nil {
		return ImportHostResult{}, err
	}
	in.FQDN = strings.TrimSpace(in.FQDN)
	bad := func(path, format string, args ...any) error {
		return &ValidationError{config.Problems{{Path: path, Message: fmt.Sprintf(format, args...)}}}
	}
	if err := config.ValidFQDN(in.FQDN); err != nil {
		return ImportHostResult{}, bad("fqdn", "%v", err)
	}
	var f EscrowFile
	if err := json.Unmarshal([]byte(in.EscrowFile), &f); err != nil || f.Format != EscrowFormatVersion {
		return ImportHostResult{}, bad("escrow_file", "this is not a decrypted escrow file of format %s (age -d -i <key> %s)", EscrowFormatVersion, storage.EscrowKey(in.FQDN))
	}
	want := s.loc.RepoURL(in.FQDN)
	switch {
	case f.FQDN != in.FQDN:
		return ImportHostResult{}, bad("escrow_file", "the escrow file is %s's, not %s's", f.FQDN, in.FQDN)
	case f.RepoPassword == "":
		return ImportHostResult{}, bad("escrow_file", "the escrow file has no repository password")
	case f.Repository != want:
		return ImportHostResult{}, bad("escrow_file", "the escrow file names the repository %s; this server keeps %s's at %s", f.Repository, in.FQDN, want)
	}
	if f.CredentialsVersion < 1 {
		f.CredentialsVersion = 1
	}
	if strings.TrimSpace(in.Message) == "" {
		in.Message = "Import host " + in.FQDN + " from its escrow file"
	}
	if _, err := s.st.Q().GetHost(ctx, in.FQDN); err == nil {
		return ImportHostResult{}, fmt.Errorf("Holdfast knows host %s already: holdfast host enroll-code %s gives its agent a new code: %w", in.FQDN, in.FQDN, ErrConflict)
	} else if !errors.Is(err, store.ErrNotFound) {
		return ImportHostResult{}, err
	}
	rep := s.probeStorage(ctx, in.FQDN)
	if !rep.OK() {
		return ImportHostResult{}, &StorageError{rep}
	}
	// The password must open the repository: its keys, listed with it, are then Holdfast's own (decision 032).
	pf, cleanup, err := s.passwordFile(in.FQDN, f.RepoPassword)
	if err != nil {
		return ImportHostResult{}, err
	}
	defer cleanup()
	r := restic.Runner{Bin: s.set.ResticBin, Repo: want, PasswordFile: pf, CacheDir: filepath.Join(s.set.StateDir, "cache"), Env: s.loc.Env(s.maintKey), NoLock: true}
	keys, err := r.Keys(ctx)
	if err != nil {
		return ImportHostResult{}, bad("escrow_file", "the escrow file's password does not open %s: %v", want, err)
	}
	ids := make([]string, 0, len(keys))
	for _, k := range keys {
		ids = append(ids, k.ID)
	}
	now := s.now()
	code := creds.NewEnrollmentCode()
	var res ImportHostResult
	err = s.st.InTx(ctx, func(q *store.Q) error {
		head, err := s.head(ctx, q)
		if err != nil {
			return err
		}
		if _, exists := head.Config.Hosts[in.FQDN]; exists {
			return fmt.Errorf("host %s is in the config already: %w", in.FQDN, ErrConflict)
		}
		doc := head.Config.Clone()
		doc.Hosts[in.FQDN] = config.Host{Jobs: map[string]config.Job{}}
		if p := doc.Validate(); len(p) > 0 {
			return &ValidationError{p}
		}
		rev, err := q.SaveRevision(ctx, store.SaveRevision{Base: head.Number, Actor: a.Name, Source: a.Source, Message: in.Message,
			Accepted: []string{}, Config: doc, HostSHAs: s.hostSHAs(doc)})
		if err != nil {
			return err
		}
		if err := q.CreateHost(ctx, in.FQDN, rev.Number, now); err != nil {
			return err
		}
		if err := q.PutCredentials(ctx, store.SealedCredentials{FQDN: in.FQDN, Version: f.CredentialsVersion, Repository: want,
			RepoPassword: s.seal.Seal(f.RepoPassword, in.FQDN), CreatedAt: now}); err != nil {
			return err
		}
		if err := q.RecordStorageCheck(ctx, in.FQDN, rep); err != nil {
			return err
		}
		if _, err := q.ResetRepositoryKeys(ctx, in.FQDN, ids); err != nil {
			return err
		}
		expires := now.Add(EnrollmentTTL)
		if err := q.CreateEnrollmentCode(ctx, newID(), in.FQDN, creds.HashToken(creds.NormaliseCode(code)), now, expires, a.Name); err != nil {
			return err
		}
		if err := q.Audit(ctx, store.AuditEntry{Actor: a.Name, Source: a.Source, Action: "host.imported", FQDN: in.FQDN, Revision: rev.Number,
			Detail: map[string]any{"message": in.Message, "repository": want, "credentials_version": f.CredentialsVersion, "repository_keys": len(ids)}}); err != nil {
			return err
		}
		if err := q.Audit(ctx, store.AuditEntry{Actor: a.Name, Source: a.Source, Action: "enrollment.code_issued", FQDN: in.FQDN,
			Detail: map[string]any{"expires_at": expires}}); err != nil {
			return err
		}
		res = ImportHostResult{Revision: rev.Number, Storage: rep, CredentialsVersion: f.CredentialsVersion, RepositoryKeys: len(ids),
			Enrollment: s.enrollment(in.FQDN, code, expires)}
		return nil
	})
	if err != nil {
		return ImportHostResult{}, err
	}
	if err := s.writeEscrow(ctx, in.FQDN); err != nil {
		res.Warning = "the escrow file could not be written yet (" + err.Error() + "); it is tried again"
	}
	return res, nil
}

/* ---------------- API ---------------- */

func (s *Server) apiExports(w http.ResponseWriter, r *http.Request) {
	v, err := s.Exports(r.Context())
	if err != nil {
		apiError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) apiExportNow(w http.ResponseWriter, r *http.Request) {
	rec, err := s.ExportNow(r.Context(), actorOf(r))
	switch {
	case err != nil && rec.ID != 0: // tried, and recorded: the answer says why it failed
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error(), "export": rec})
	case err != nil:
		apiError(w, err)
	default:
		writeJSON(w, http.StatusOK, rec)
	}
}

func (s *Server) apiImportHost(w http.ResponseWriter, r *http.Request) {
	var in ImportHostInput
	if !decode(w, r, &in) {
		return
	}
	res, err := s.ImportHost(r.Context(), actorOf(r), in)
	if err != nil {
		apiError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, res)
}
