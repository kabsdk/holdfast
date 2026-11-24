package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/kabsdk/holdfast/internal/config"
	"github.com/kabsdk/holdfast/internal/proto"
	"github.com/kabsdk/holdfast/internal/server/store"
)

// The server's side of off-site copies (decisions 027, 039 and 041; offsite.go). It answers its off-site instance's
// syncs with the feed, holds back what is not copied yet, and shows the copies on the hosts' pages. Without offsite:
// in server.yaml there is no report, so nothing is held back and nothing is shown.

var errVaultRestore = fmt.Errorf("the off-site instance runs no restores itself: restore on the server with --from-offsite: %w", ErrConflict)

// wanted returns the snapshots here, of the jobs copied off-site, that have no copy there: what is still wanted, as
// far as the server knows. A copy deleted off-site counts as wanted again, which holds the original back at most until
// it is 7 days old.
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

// vaultReports returns the off-site instance's latest report, decoded: none without offsite:.
func (s *Server) vaultReports(ctx context.Context) ([]vaultReport, error) {
	if s.offsite == nil {
		return nil, nil
	}
	vs, err := s.st.Q().ListVaults(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]vaultReport, 0, len(vs))
	for _, v := range vs {
		if v.Name != store.OffsiteName {
			continue
		}
		r := vaultReport{Vault: v}
		if len(v.Report) > 0 {
			if err := json.Unmarshal(v.Report, &r.sync); err != nil {
				s.log.Warn("the off-site report does not decode", "err", err)
			}
		}
		out = append(out, r)
	}
	return out, nil
}

/* ---------------- what the hosts' pages show ---------------- */

// HostOffsite is what the off-site instance reports of a host. Its settings are set under /api/v1/offsite/.
type HostOffsite struct {
	ReportedAt *time.Time             `json:"reported_at,omitempty"`
	LastCopy   *time.Time             `json:"last_copy,omitempty"`
	CopyError  string                 `json:"copy_error,omitempty"`
	Copies     int                    `json:"copies"`
	Pending    int                    `json:"pending"`
	Jobs       []proto.VaultJobReport `json:"jobs"`
}

// offsiteByHost returns what the off-site instance reports, by host.
func (s *Server) offsiteByHost(ctx context.Context) map[string]*HostOffsite {
	out := map[string]*HostOffsite{}
	reports, err := s.vaultReports(ctx)
	if err != nil {
		s.log.Warn("reading the off-site report", "err", err)
		return out
	}
	for _, r := range reports {
		for _, h := range r.sync.Hosts {
			jobs := h.Jobs
			if jobs == nil {
				jobs = []proto.VaultJobReport{}
			}
			w, _ := s.wanted(ctx, h)
			out[h.FQDN] = &HostOffsite{ReportedAt: r.ReportedAt, LastCopy: h.LastCopy, CopyError: h.CopyError, Copies: len(h.Copies), Pending: len(w), Jobs: jobs}
		}
	}
	return out
}

// OffsiteCopies lists the off-site copies of a host, as last reported, newest first: what a restore from off-site can
// choose from.
func (s *Server) OffsiteCopies(ctx context.Context, fqdn string) ([]proto.VaultCopy, error) {
	if s.offsite == nil {
		return nil, fmt.Errorf("off-site copies are not set up: %w", ErrNotFound)
	}
	reports, err := s.vaultReports(ctx)
	if err != nil {
		return nil, err
	}
	out := []proto.VaultCopy{}
	for _, r := range reports {
		for _, h := range r.sync.Hosts {
			if h.FQDN == fqdn {
				out = append(out, h.Copies...)
			}
		}
	}
	sort.SliceStable(out, func(i, k int) bool { return out[i].Time.After(out[k].Time) })
	return out, nil
}

// offsiteCopy finds the off-site copy of one of a host's snapshots, by the copy's ID or the original's, or a prefix
// of either.
func (s *Server) offsiteCopy(ctx context.Context, fqdn, id string) (proto.VaultCopy, error) {
	if !snapshotIDPattern.MatchString(id) {
		return proto.VaultCopy{}, &ValidationError{config.Problems{{Path: "snapshot", Message: "give a snapshot ID, or a prefix of at least 8 characters"}}}
	}
	copies, err := s.OffsiteCopies(ctx, fqdn)
	if err != nil {
		return proto.VaultCopy{}, err
	}
	var found []proto.VaultCopy
	for _, c := range copies {
		if strings.HasPrefix(c.ID, id) || strings.HasPrefix(c.Original, id) {
			found = append(found, c)
		}
	}
	switch len(found) {
	case 0:
		return proto.VaultCopy{}, fmt.Errorf("no off-site copy of %s on %s: %w", id, fqdn, ErrNotFound)
	case 1:
		return found[0], nil
	}
	return proto.VaultCopy{}, store.ErrAmbiguous
}

/* ---------------- the hold ---------------- */

// vaultsWant returns the snapshots of a host that are not copied off-site yet.
func (s *Server) vaultsWant(ctx context.Context, fqdn string) map[string]bool {
	out := map[string]bool{}
	if s.set.IsVault() {
		return out
	}
	reports, err := s.vaultReports(ctx)
	if err != nil {
		s.log.Warn("reading the off-site report", "err", err)
		return out
	}
	for _, r := range reports {
		for _, h := range r.sync.Hosts {
			if h.FQDN != fqdn {
				continue
			}
			w, err := s.wanted(ctx, h)
			if err != nil {
				s.log.Warn("working out what is not copied off-site yet", "host", fqdn, "err", err)
			}
			for _, sn := range w {
				out[sn.ID] = true
			}
		}
	}
	return out
}

// holdForVaults keeps back, from what the daily run forgets, the snapshots not copied off-site yet, while they are
// younger than 7 days (decision 039). A copier that stalls longer does not keep the primary's data for ever: its lag
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

/* ---------------- the sync, in-process ---------------- */

// offsiteSync keeps the off-site instance's report and answers with the feed.
func (s *Server) offsiteSync(ctx context.Context, rep proto.VaultSync) (proto.VaultFeed, error) {
	raw, err := json.Marshal(rep)
	if err != nil {
		return proto.VaultFeed{}, err
	}
	if err := s.st.Q().RecordVaultReport(ctx, store.OffsiteName, raw, s.now()); err != nil {
		return proto.VaultFeed{}, err
	}
	return s.vaultFeed(ctx)
}

// vaultFeed is what is copied: every enrolled host with its jobs as they run, its repository's password and the
// snapshots deleted on request; where the copier reads them; and the bindings.
func (s *Server) vaultFeed(ctx context.Context) (proto.VaultFeed, error) {
	feed := proto.VaultFeed{ServerTime: s.now(), Hosts: []proto.VaultHost{}, Source: s.copierSource}
	q := s.st.Q()
	head, err := s.head(ctx, q)
	if err != nil {
		return proto.VaultFeed{}, err
	}
	feed.Access = head.Config.Access
	hosts, err := q.ListHosts(ctx)
	if err != nil {
		return proto.VaultFeed{}, err
	}
	for _, h := range hosts {
		hc, ok := head.Config.Hosts[h.FQDN]
		if h.State != "enrolled" || !ok {
			continue // a removed host's copies are retired off-site
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
		vh := proto.VaultHost{FQDN: h.FQDN, Repository: c.Repository, RepoPassword: pw, Jobs: []proto.VaultJob{}, DeletedInPrimary: del,
			PostgreSQL: hc.PostgreSQL}
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
			vh.Jobs = append(vh.Jobs, proto.VaultJob{ID: id, Engine: j.Engine, PostgreSQL: j.PostgreSQL, Paths: j.Paths, Excludes: j.Excludes, Dump: j.Dump,
				Schedule: sch, Retention: j.Retention})
		}
		feed.Hosts = append(feed.Hosts, vh)
	}
	return feed, nil
}

/* ---------------- /api/v1 ---------------- */

func (s *Server) apiOffsiteCopies(w http.ResponseWriter, r *http.Request) {
	cs, err := s.OffsiteCopies(r.Context(), r.PathValue("fqdn"))
	if err != nil {
		apiError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, cs)
}
