package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/kabsdk/holdfast/internal/access"
	"github.com/kabsdk/holdfast/internal/config"
	"github.com/kabsdk/holdfast/internal/proto"
	"github.com/kabsdk/holdfast/internal/server/creds"
	"github.com/kabsdk/holdfast/internal/server/storage"
	"github.com/kabsdk/holdfast/internal/server/store"
)

// The source side of off-site copies (decisions 027 and 039). holdfast-server registers the vaults that copy its
// hosts, answers their syncs with the feed, holds back what they have not copied yet, and asks them for restores. A
// vault opens every connection; nothing here connects to it or changes anything in its site.

var vaultName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

var errVaultRestore = fmt.Errorf("a vault runs no restores itself: restore on site A's holdfast-server with --from-vault: %w", ErrConflict)

// vaultSealID is what a vault's read-only key is sealed to.
func vaultSealID(name string) string { return "vault:" + name }

// AddVaultInput registers a vault. S3Key may only read repos/ in this site's bucket: the vault reads every host's
// repository with it, and must be unable to change any.
type AddVaultInput struct {
	Name    string     `json:"name"`
	S3Key   S3KeyInput `json:"s3_key"`
	Message string     `json:"message,omitempty"`
}

// AddVaultResult carries the vault's token, which is shown once: it goes into the vault's token_file.
type AddVaultResult struct {
	Name    string         `json:"name"`
	Token   string         `json:"token"`
	Storage storage.Report `json:"storage"`
}

// AddVault registers a vault, after testing that its key reads a host's repository and cannot write there.
func (s *Server) AddVault(ctx context.Context, a Actor, in AddVaultInput) (AddVaultResult, error) {
	if s.set.IsVault() {
		return AddVaultResult{}, fmt.Errorf("a vault copies from its source and has no vaults of its own: %w", ErrConflict)
	}
	if err := s.requireEverywhere(ctx, a, access.ConfigStorage); err != nil {
		return AddVaultResult{}, err
	}
	in.Name = strings.TrimSpace(in.Name)
	if !vaultName.MatchString(in.Name) {
		return AddVaultResult{}, &ValidationError{config.Problems{{Path: "name", Message: "a vault's name is lower-case letters, digits and dashes, such as site-b"}}}
	}
	key, err := in.S3Key.key()
	if err != nil {
		return AddVaultResult{}, err
	}
	rep := storage.Report{Findings: []storage.Finding{}}
	hosts, err := s.st.Q().ListHosts(ctx)
	if err != nil {
		return AddVaultResult{}, err
	}
	for _, h := range hosts {
		if h.State == "enrolled" {
			rep = s.prober.ProbeReadOnly(ctx, h.FQDN, key) // one host stands for all: the key's policy is on repos/
			break
		}
	}
	rep.CheckedAt = s.now()
	if !rep.OK() {
		return AddVaultResult{}, &StorageError{rep}
	}
	tok := creds.NewToken()
	err = s.st.InTx(ctx, func(q *store.Q) error {
		v := store.Vault{Name: in.Name, S3AccessKeyID: key.AccessKeyID, S3Secret: s.seal.Seal(key.SecretAccessKey, vaultSealID(in.Name)),
			CreatedAt: s.now(), CreatedBy: a.Name}
		if err := q.AddVault(ctx, v, creds.HashToken(tok)); err != nil {
			if errors.Is(err, store.ErrConflict) {
				return fmt.Errorf("vault %s exists already: %w", in.Name, ErrConflict)
			}
			return err
		}
		return q.Audit(ctx, store.AuditEntry{Actor: a.Name, Source: a.Source, Action: "vault.add", Detail: map[string]any{"vault": in.Name, "message": in.Message}})
	})
	if err != nil {
		return AddVaultResult{}, err
	}
	return AddVaultResult{Name: in.Name, Token: tok, Storage: rep}, nil
}

// RemoveVault forgets a vault: its token stops working, and this site no longer holds anything back for it. Its
// copies in the other site stay; only that site's vault admin can delete them.
func (s *Server) RemoveVault(ctx context.Context, a Actor, name string, in RemoveInput) error {
	if err := s.requireEverywhere(ctx, a, access.ConfigStorage); err != nil {
		return err
	}
	return s.st.InTx(ctx, func(q *store.Q) error {
		if err := q.RemoveVault(ctx, name); err != nil {
			return fmt.Errorf("vault %s: %w", name, err)
		}
		return q.Audit(ctx, store.AuditEntry{Actor: a.Name, Source: a.Source, Action: "vault.remove", Detail: map[string]any{"vault": name, "message": in.Message}})
	})
}

// VaultView is one registered vault.
type VaultView struct {
	Name       string     `json:"name"`
	CreatedAt  time.Time  `json:"created_at"`
	CreatedBy  string     `json:"created_by"`
	LastSeenAt *time.Time `json:"last_seen_at,omitempty"`
	Hosts      int        `json:"hosts"`       // hosts it copies
	Pending    int        `json:"pending"`     // snapshots it wants and has not copied yet
	CopyErrors int        `json:"copy_errors"` // hosts whose last copy failed
}

// Vaults lists the registered vaults.
func (s *Server) Vaults(ctx context.Context, a Actor) ([]VaultView, error) {
	if err := s.requireEverywhere(ctx, a, access.ConfigStorage); err != nil {
		return nil, err
	}
	reports, err := s.vaultReports(ctx)
	if err != nil {
		return nil, err
	}
	out := []VaultView{}
	for _, r := range reports {
		v := VaultView{Name: r.Name, CreatedAt: r.CreatedAt, CreatedBy: r.CreatedBy, LastSeenAt: r.LastSeenAt}
		for _, h := range r.sync.Hosts {
			if h.Retired {
				continue
			}
			v.Hosts++
			if w, err := s.wanted(ctx, h); err == nil {
				v.Pending += len(w)
			}
			if h.CopyError != "" {
				v.CopyErrors++
			}
		}
		out = append(out, v)
	}
	return out, nil
}

// wanted returns the snapshots here, of the jobs a vault copies, that it holds no copy of: what it still wants, as
// far as this site knows. A copy the vault deleted counts as wanted again, which holds the original back at most
// until it is 7 days old.
func (s *Server) wanted(ctx context.Context, h proto.VaultHostReport) ([]store.Snapshot, error) {
	jobs := map[string]bool{}
	for _, j := range h.Jobs {
		if !j.Excluded && !j.Retired {
			jobs[j.ID] = true
		}
	}
	if len(jobs) == 0 {
		return nil, nil
	}
	have := map[string]bool{}
	for _, c := range h.Copies {
		have[c.Original] = true
	}
	rows, err := s.st.Q().ListSnapshots(ctx, h.FQDN, store.SnapshotFilter{})
	if err != nil {
		return nil, err
	}
	var out []store.Snapshot
	for _, r := range rows {
		if jobs[r.JobID] && !have[r.ID] {
			out = append(out, r)
		}
	}
	return out, nil
}

type vaultReport struct {
	store.Vault
	sync proto.VaultSync
}

// vaultReports returns each vault with its latest report decoded.
func (s *Server) vaultReports(ctx context.Context) ([]vaultReport, error) {
	vs, err := s.st.Q().ListVaults(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]vaultReport, 0, len(vs))
	for _, v := range vs {
		r := vaultReport{Vault: v}
		if len(v.Report) > 0 {
			if err := json.Unmarshal(v.Report, &r.sync); err != nil {
				s.log.Warn("a vault's report does not decode", "vault", v.Name, "err", err)
			}
		}
		out = append(out, r)
	}
	return out, nil
}

/* ---------------- what the hosts' pages show ---------------- */

// HostOffsite is what one vault reports of a host. It is read-only here: the off-site retention is set in the
// vault (spec §3.3).
type HostOffsite struct {
	Vault      string                 `json:"vault"`
	ReportedAt *time.Time             `json:"reported_at,omitempty"`
	LastCopy   *time.Time             `json:"last_copy,omitempty"`
	CopyError  string                 `json:"copy_error,omitempty"`
	Copies     int                    `json:"copies"`
	Pending    int                    `json:"pending"`
	Jobs       []proto.VaultJobReport `json:"jobs"`
}

// offsiteByHost returns what every vault reports, by host.
func (s *Server) offsiteByHost(ctx context.Context) map[string][]HostOffsite {
	out := map[string][]HostOffsite{}
	reports, err := s.vaultReports(ctx)
	if err != nil {
		s.log.Warn("reading the vaults' reports", "err", err)
		return out
	}
	for _, r := range reports {
		for _, h := range r.sync.Hosts {
			jobs := h.Jobs
			if jobs == nil {
				jobs = []proto.VaultJobReport{}
			}
			w, _ := s.wanted(ctx, h)
			out[h.FQDN] = append(out[h.FQDN], HostOffsite{Vault: r.Name, ReportedAt: r.ReportedAt, LastCopy: h.LastCopy, CopyError: h.CopyError,
				Copies: len(h.Copies), Pending: len(w), Jobs: jobs})
		}
	}
	return out
}

// VaultCopies are one vault's copies of a host, newest first.
type VaultCopies struct {
	Vault      string            `json:"vault"`
	ReportedAt *time.Time        `json:"reported_at,omitempty"`
	Copies     []proto.VaultCopy `json:"copies"`
}

// OffsiteCopies lists the copies every vault holds of a host, as they last reported: what a restore from a vault
// can choose from.
func (s *Server) OffsiteCopies(ctx context.Context, fqdn string) ([]VaultCopies, error) {
	reports, err := s.vaultReports(ctx)
	if err != nil {
		return nil, err
	}
	out := []VaultCopies{}
	for _, r := range reports {
		for _, h := range r.sync.Hosts {
			if h.FQDN == fqdn {
				cs := append([]proto.VaultCopy{}, h.Copies...)
				sort.SliceStable(cs, func(i, k int) bool { return cs[i].Time.After(cs[k].Time) })
				out = append(out, VaultCopies{Vault: r.Name, ReportedAt: r.ReportedAt, Copies: cs})
			}
		}
	}
	return out, nil
}

// vaultCopy finds a vault's copy of one of a host's snapshots, by the copy's ID or the original's, or a prefix of
// either.
func (s *Server) vaultCopy(ctx context.Context, vault, fqdn, id string) (proto.VaultCopy, error) {
	if !snapshotIDPattern.MatchString(id) {
		return proto.VaultCopy{}, &ValidationError{config.Problems{{Path: "snapshot", Message: "give a snapshot ID, or a prefix of at least 8 characters"}}}
	}
	reports, err := s.vaultReports(ctx)
	if err != nil {
		return proto.VaultCopy{}, err
	}
	for _, r := range reports {
		if r.Name != vault {
			continue
		}
		var found []proto.VaultCopy
		for _, h := range r.sync.Hosts {
			if h.FQDN != fqdn {
				continue
			}
			for _, c := range h.Copies {
				if strings.HasPrefix(c.ID, id) || strings.HasPrefix(c.Original, id) {
					found = append(found, c)
				}
			}
		}
		switch len(found) {
		case 0:
			return proto.VaultCopy{}, fmt.Errorf("vault %s reported no copy of %s on %s: %w", vault, id, fqdn, ErrNotFound)
		case 1:
			return found[0], nil
		}
		return proto.VaultCopy{}, store.ErrAmbiguous
	}
	return proto.VaultCopy{}, fmt.Errorf("vault %s: %w", vault, ErrNotFound)
}

/* ---------------- the hold ---------------- */

// vaultsWant returns the snapshots of a host that a vault wants and has not copied yet.
func (s *Server) vaultsWant(ctx context.Context, fqdn string) map[string]bool {
	out := map[string]bool{}
	if s.set.IsVault() {
		return out
	}
	reports, err := s.vaultReports(ctx)
	if err != nil {
		s.log.Warn("reading the vaults' reports", "err", err)
		return out
	}
	for _, r := range reports {
		for _, h := range r.sync.Hosts {
			if h.FQDN != fqdn {
				continue
			}
			w, err := s.wanted(ctx, h)
			if err != nil {
				s.log.Warn("working out what a vault wants", "vault", r.Name, "host", fqdn, "err", err)
			}
			for _, sn := range w {
				out[sn.ID] = true
			}
		}
	}
	return out
}

// holdForVaults keeps back, from what the daily run forgets, the snapshots a vault still wants, while they are
// younger than 7 days (decision 039). A vault that stalls longer does not keep this site's data for ever: its lag
// alerts long before.
func (s *Server) holdForVaults(ctx context.Context, fqdn string, jr *JobRetention, wanted map[string]bool, now time.Time) {
	if len(wanted) == 0 || len(jr.Forget) == 0 {
		return
	}
	kept := make([]string, 0, len(jr.Forget))
	for _, id := range jr.Forget {
		if !wanted[id] {
			kept = append(kept, id)
			continue
		}
		sn, err := s.st.Q().GetSnapshot(ctx, fqdn, id)
		if err != nil || !now.Before(sn.Time.Add(Protection)) {
			kept = append(kept, id)
			continue
		}
		jr.Pending = append(jr.Pending, PendingSnapshot{ID: id, ShortID: sn.ShortID, Time: sn.Time, JobID: jr.JobID, Reason: PendingOffsite,
			PrunableAt: sn.Time.Add(Protection)})
	}
	jr.Forget = kept
}

/* ---------------- the vault protocol ---------------- */

type vaultHandler func(w http.ResponseWriter, r *http.Request, v store.Vault)

// vaultAuth resolves a vault's bearer token to the vault.
func (s *Server) vaultAuth(next vaultHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		if tok == "" || s.set.IsVault() {
			writeErr(w, http.StatusUnauthorized, "missing vault credential")
			return
		}
		v, err := s.st.Q().VaultByToken(r.Context(), creds.HashToken(tok))
		if err != nil {
			writeErr(w, http.StatusUnauthorized, "unknown vault credential")
			return
		}
		next(w, r, v)
	}
}

func (s *Server) handleVaultSync(w http.ResponseWriter, r *http.Request, v store.Vault) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<20))
	var rep proto.VaultSync
	if err == nil {
		err = json.Unmarshal(body, &rep)
	}
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad request")
		return
	}
	feed, err := s.VaultSync(r.Context(), v, body)
	if err != nil {
		s.log.Error("a vault's sync failed", "vault", v.Name, "err", err)
		writeErr(w, http.StatusInternalServerError, "sync failed")
		return
	}
	writeJSON(w, http.StatusOK, feed)
}

// VaultSync keeps a vault's report and answers with its feed.
func (s *Server) VaultSync(ctx context.Context, v store.Vault, report []byte) (proto.VaultFeed, error) {
	if err := s.st.Q().RecordVaultReport(ctx, v.Name, report, s.now()); err != nil {
		return proto.VaultFeed{}, err
	}
	return s.vaultFeed(ctx, v)
}

// vaultFeed is what a vault copies: every enrolled host with its jobs as they run here, its repository's password
// and the snapshots deleted on request; how to read the bucket; and the restores it is asked to grant.
func (s *Server) vaultFeed(ctx context.Context, v store.Vault) (proto.VaultFeed, error) {
	secret, err := s.seal.Open(v.S3Secret, vaultSealID(v.Name))
	if err != nil {
		return proto.VaultFeed{}, err
	}
	feed := proto.VaultFeed{ServerTime: s.now(), Hosts: []proto.VaultHost{}, Requests: []proto.VaultRequest{},
		Source: proto.VaultSource{Endpoint: s.loc.Endpoint, Bucket: s.loc.Bucket, Region: s.loc.Region, S3AccessKeyID: v.S3AccessKeyID, S3SecretAccessKey: secret}}
	q := s.st.Q()
	head, err := s.head(ctx, q)
	if err != nil {
		return proto.VaultFeed{}, err
	}
	hosts, err := q.ListHosts(ctx)
	if err != nil {
		return proto.VaultFeed{}, err
	}
	for _, h := range hosts {
		hc, ok := head.Config.Hosts[h.FQDN]
		if h.State != "enrolled" || !ok {
			continue // a removed host's copies are retired in the vault
		}
		c, err := q.GetCredentials(ctx, h.FQDN)
		if err != nil {
			return proto.VaultFeed{}, err
		}
		pw, err := s.seal.Open(c.RepoPassword, h.FQDN)
		if err != nil {
			return proto.VaultFeed{}, err
		}
		del, err := q.DeletedOnRequest(ctx, h.FQDN)
		if err != nil {
			return proto.VaultFeed{}, err
		}
		vh := proto.VaultHost{FQDN: h.FQDN, Repository: c.Repository, RepoPassword: pw, Jobs: []proto.VaultJob{}, DeletedInPrimary: del}
		ids := make([]string, 0, len(hc.Jobs))
		for id := range hc.Jobs {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			j := hc.Jobs[id]
			sch := j.Schedule
			if sch.Zone == "" {
				sch.Zone = s.set.DefaultZone
			}
			vh.Jobs = append(vh.Jobs, proto.VaultJob{ID: id, Paths: j.Paths, Excludes: j.Excludes, Dump: j.Dump, Schedule: sch, Retention: j.Retention})
		}
		feed.Hosts = append(feed.Hosts, vh)
	}
	reqs, err := q.WaitingVaultRequests(ctx, v.Name)
	if err != nil {
		return proto.VaultFeed{}, err
	}
	for _, r := range reqs {
		feed.Requests = append(feed.Requests, proto.VaultRequest{ID: r.ID, FQDN: r.FQDN, Snapshot: r.Snapshot, By: r.RequestedBy})
	}
	return feed, nil
}

func (s *Server) handleVaultGrant(w http.ResponseWriter, r *http.Request, v store.Vault) {
	var g proto.VaultGrant
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&g); err != nil {
		writeErr(w, http.StatusBadRequest, "bad request")
		return
	}
	if err := s.VaultGrant(r.Context(), v, g); err != nil {
		var ve *ValidationError
		switch {
		case errors.As(err, &ve):
			writeErr(w, http.StatusBadRequest, err.Error())
		case errors.Is(err, ErrConflict):
			writeErr(w, http.StatusConflict, err.Error())
		case errors.Is(err, ErrNotFound):
			writeErr(w, http.StatusNotFound, err.Error())
		default:
			s.log.Error("a vault's grant failed", "vault", v.Name, "err", err)
			writeErr(w, http.StatusInternalServerError, "grant failed")
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// VaultGrant takes a vault's answer to a restore request. A grant queues the restore on the host, with the copy's
// repository, password and read-only key sealed for that host's agent, which gets them once (decision 039).
func (s *Server) VaultGrant(ctx context.Context, v store.Vault, g proto.VaultGrant) error {
	req, err := s.st.Q().GetVaultRequest(ctx, g.ID)
	if err != nil || req.Vault != v.Name {
		return fmt.Errorf("restore request %s: %w", g.ID, ErrNotFound)
	}
	if req.State != "waiting" {
		return fmt.Errorf("restore request %s is %s already: %w", g.ID, req.State, ErrConflict)
	}
	now := s.now()
	if g.Refused != "" {
		return s.st.InTx(ctx, func(q *store.Q) error {
			if err := q.AnswerVaultRequest(ctx, req.ID, v.Name, "refused", g.Refused, now); err != nil {
				return err
			}
			return s.failVaultRequest(ctx, q, req, "refused by vault "+v.Name+": "+g.Refused, "restore.vault_refused", now)
		})
	}
	if g.Repository == "" || g.RepoPassword == "" || g.S3AccessKeyID == "" || g.S3SecretAccessKey == "" {
		return &ValidationError{config.Problems{{Path: "grant", Message: "a grant carries the repository, its password and a read-only key"}}}
	}
	src, err := json.Marshal(proto.RestoreSource{FQDN: req.FQDN, Repository: g.Repository, RepoPassword: g.RepoPassword,
		S3AccessKeyID: g.S3AccessKeyID, S3SecretAccessKey: g.S3SecretAccessKey})
	if err != nil {
		return err
	}
	var spec proto.RestoreSpec
	if err := json.Unmarshal(req.Spec, &spec); err != nil {
		return err
	}
	expires := req.RequestedAt.Add(RestoreExpiry)
	return s.st.InTx(ctx, func(q *store.Q) error {
		if err := q.AnswerVaultRequest(ctx, req.ID, v.Name, "granted", "", now); err != nil {
			return err
		}
		if err := q.EnqueueCommand(ctx, store.Command{ID: req.ID, FQDN: req.FQDN, Kind: proto.CommandRestore, JobID: spec.JobID, CreatedAt: now,
			CreatedBy: req.RequestedBy, Payload: req.Spec, ExpiresAt: &expires, Sealed: s.seal.Seal(string(src), req.FQDN)}); err != nil {
			return err
		}
		if err := q.Audit(ctx, store.AuditEntry{Actor: "vault:" + v.Name, Source: "vault", Action: "restore.vault_granted", FQDN: req.FQDN, JobID: spec.JobID,
			Detail: map[string]any{"command": req.ID, "vault": v.Name, "snapshot": req.Snapshot, "original": req.Original, "requested_by": req.RequestedBy}}); err != nil {
			return err
		}
		return q.NotifyHost(ctx, req.FQDN)
	})
}

// failVaultRequest puts a restore that never reached its host in the host's history as failed, and audits it.
func (s *Server) failVaultRequest(ctx context.Context, q *store.Q, req store.VaultRequest, msg, action string, now time.Time) error {
	var spec proto.RestoreSpec
	_ = json.Unmarshal(req.Spec, &spec)
	if _, err := q.InsertEvent(ctx, store.Event{FQDN: req.FQDN, ID: "vault-" + req.ID, Kind: proto.KindRestore, JobID: spec.JobID, CommandID: req.ID,
		Status: proto.StatusFailed, Started: req.RequestedAt, Finished: now, Error: msg,
		Detail: map[string]string{"snapshot": req.Snapshot, "target": spec.Target, "from_vault": req.Vault}}); err != nil {
		return err
	}
	return q.Audit(ctx, store.AuditEntry{Actor: systemActor.Name, Source: systemActor.Source, Action: action, FQDN: req.FQDN, JobID: spec.JobID,
		Detail: map[string]any{"command": req.ID, "vault": req.Vault, "requested_by": req.RequestedBy, "error": msg}})
}

// expireVaultRequests gives up on the restores no vault granted within the restore's 24 hours.
func (s *Server) expireVaultRequests(ctx context.Context) {
	now := s.now()
	reqs, err := s.st.Q().ExpireVaultRequests(ctx, now, RestoreExpiry)
	if err != nil {
		s.log.Warn("expiring restores from vaults", "err", err)
		return
	}
	for _, r := range reqs {
		err := s.st.InTx(ctx, func(q *store.Q) error {
			return s.failVaultRequest(ctx, q, r, fmt.Sprintf("cancelled: vault %s did not answer within %s", r.Vault, RestoreExpiry), "restore.expired", now)
		})
		if err != nil {
			s.log.Warn("recording an expired restore from a vault", "host", r.FQDN, "err", err)
		}
	}
}

/* ---------------- /api/v1 ---------------- */

func (s *Server) apiVaults(w http.ResponseWriter, r *http.Request) {
	vs, err := s.Vaults(r.Context(), actorOf(r))
	if err != nil {
		apiError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, vs)
}

func (s *Server) apiAddVault(w http.ResponseWriter, r *http.Request) {
	var in AddVaultInput
	if !decode(w, r, &in) {
		return
	}
	res, err := s.AddVault(r.Context(), actorOf(r), in)
	if err != nil {
		apiError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, res)
}

func (s *Server) apiRemoveVault(w http.ResponseWriter, r *http.Request) {
	var in RemoveInput
	if r.ContentLength != 0 && !decode(w, r, &in) {
		return
	}
	if err := s.RemoveVault(r.Context(), actorOf(r), r.PathValue("name"), in); err != nil {
		apiError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"removed": r.PathValue("name")})
}

func (s *Server) apiOffsiteCopies(w http.ResponseWriter, r *http.Request) {
	cs, err := s.OffsiteCopies(r.Context(), r.PathValue("fqdn"))
	if err != nil {
		apiError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, cs)
}
