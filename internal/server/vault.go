package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/kabsdk/holdfast/internal/checks"
	"github.com/kabsdk/holdfast/internal/config"
	"github.com/kabsdk/holdfast/internal/proto"
	"github.com/kabsdk/holdfast/internal/restic"
	"github.com/kabsdk/holdfast/internal/server/creds"
	"github.com/kabsdk/holdfast/internal/server/storage"
	"github.com/kabsdk/holdfast/internal/server/store"
)

// The off-site instance (decisions 027, 039 and 041; offsite.go): holdfast-server's own code in the vault role, on the
// off-site bucket. In place of agents it has the copier: every offsite.poll it syncs with the server, in-process, and
// copies each job's new snapshots into the off-site repositories with `restic copy`. Everything else, its revisions,
// retention behind the 7-day protection, Prune now, Delete now, decommission, rollback, escrow and size, is the
// server's code, on the off-site bucket.
//
// It reads the primary repositories from the server's loopback endpoint, read-only, and writes off-site through s3
// with the off-site key. Both repositories are used with --no-lock: the source may not be locked, and the copier is
// the off-site repositories' only writer, which runs one thing at a time per host (repoLocks, busyHosts).

const (
	copyTimeout = 12 * time.Hour // a host's first copy is everything it has
	checkEach   = 24 * time.Hour // the daily check of a copied repository, as an agent checks its own
)

// vaultState is what the copier knows between its runs: how far each job is behind, and when each repository was last
// checked. It is worked out again from the repositories on every run.
type vaultState struct {
	mu      sync.Mutex
	feed    *proto.VaultFeed                    // the latest, from the server
	behind  map[string]map[string]time.Duration // fqdn → job → how far its newest copy is behind the newest in primary
	checked map[string]time.Time                // fqdn → the last daily check
}

// vaultSync is the off-site instance's own work.
var vaultSync = offsiteActor

// Version is set at build time, as the agent's is.
var Version = "dev"

// vaultLoop syncs, and copies, every offsite.poll from when the instance starts. They are two loops: a copy can take
// hours, or hang while the off-site storage fails and restic retries, and the server must still get the reports, by
// which it holds back what is not copied yet.
func (s *Server) vaultLoop(ctx context.Context) {
	if !s.set.IsVault() {
		return
	}
	go s.every(ctx, s.syncOnce)
	for s.currentFeed() == nil { // the first copy right after the first sync
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
	s.every(ctx, s.copyAll)
}

func (s *Server) every(ctx context.Context, f func(context.Context)) {
	for {
		if s.isLeader() {
			f(ctx)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(s.set.Offsite.Poll):
		}
	}
}

func (s *Server) currentFeed() *proto.VaultFeed {
	s.vault.mu.Lock()
	defer s.vault.mu.Unlock()
	return s.vault.feed
}

// syncOnce hands the server the report and takes its feed: hosts and jobs, and the bindings.
func (s *Server) syncOnce(ctx context.Context) {
	rep, err := s.vaultReport(ctx)
	if err != nil {
		s.log.Error("the off-site report", "err", err)
		return
	}
	feed, err := s.source.offsiteSync(ctx, rep)
	if err != nil {
		s.log.Error("the off-site sync with the server", "err", err)
		return
	}
	s.vault.mu.Lock()
	s.vault.feed = &feed
	s.vault.mu.Unlock()
	if err := s.applyFeed(ctx, feed); err != nil {
		s.log.Error("applying the server's feed off-site", "err", err)
	}
}

// copyAll copies every host of the latest feed, one at a time.
func (s *Server) copyAll(ctx context.Context) {
	feed := s.currentFeed()
	if feed == nil {
		return
	}
	for _, h := range feed.Hosts {
		if ctx.Err() != nil {
			return
		}
		s.copyHost(ctx, feed.Source, h)
	}
}

/* ---------------- hosts and jobs from the server ---------------- */

// applyFeed makes the off-site config follow the server's, in one off-site revision when anything changed:
//   - a new host or job is added; a new job's off-site retention starts as a copy of its primary retention;
//   - a job's paths, schedule and primary retention follow the server; its off-site retention and Exclude do not;
//   - a job the server no longer has is Retired: its copies stay, under its off-site retention, until an admin
//     removes the host off-site and decommissions it there;
//   - the bindings are the server's: the off-site pages answer to the same people.
func (s *Server) applyFeed(ctx context.Context, feed proto.VaultFeed) error {
	var added []string
	err := s.st.InTx(ctx, func(q *store.Q) error {
		head, err := s.head(ctx, q)
		if err != nil {
			return err
		}
		doc, changes := followFeed(head.Config, feed)
		if len(changes) == 0 {
			return nil
		}
		if p := doc.Validate(); len(p) > 0 {
			return &ValidationError{p}
		}
		msg := "From the server: " + strings.Join(changes, "; ")
		if len(msg) > 2000 {
			msg = msg[:2000] + "…"
		}
		rev, err := q.SaveRevision(ctx, store.SaveRevision{Base: head.Number, Actor: vaultSync.Name, Source: vaultSync.Source, Message: msg,
			Accepted: []string{}, Config: doc, HostSHAs: s.hostSHAs(doc)})
		if err != nil {
			return err
		}
		for fqdn := range doc.Hosts {
			if _, before := head.Config.Hosts[fqdn]; before {
				continue
			}
			if err := s.activateCopiedHost(ctx, q, fqdn, rev.Number); err != nil {
				return err
			}
			added = append(added, fqdn)
		}
		return q.Audit(ctx, store.AuditEntry{Actor: vaultSync.Name, Source: vaultSync.Source, Action: "offsite.synced", Revision: rev.Number,
			Detail: map[string]any{"changes": changes}})
	})
	if err != nil {
		return err
	}
	for _, h := range feed.Hosts {
		if err := s.st.Q().SetPrimaryDeleted(ctx, h.FQDN, h.DeletedInPrimary); err != nil && !errors.Is(err, ErrNotFound) {
			s.log.Warn("recording the snapshots deleted in primary", "host", h.FQDN, "err", err)
		}
	}
	for _, fqdn := range added {
		s.vaultStorageCheck(ctx, fqdn)
		_ = s.writeEscrow(ctx, fqdn) // the off-site password, escrowed in the off-site bucket (decision 036); retried in the background
	}
	return nil
}

// followFeed returns the off-site config with the feed applied, and what changed.
func followFeed(cur config.Document, feed proto.VaultFeed) (config.Document, []string) {
	doc := cur.Clone()
	var changes []string
	if feed.Access.Bindings != nil && !reflect.DeepEqual(doc.Access.Bindings, feed.Access.Bindings) {
		doc.Access.Bindings = slices.Clone(feed.Access.Bindings)
		changes = append(changes, "the bindings follow the server's")
	}
	inFeed := map[string]bool{}
	for _, fh := range feed.Hosts {
		inFeed[fh.FQDN] = true
		h, exists := doc.Hosts[fh.FQDN]
		if !exists {
			changes = append(changes, "add host "+fh.FQDN)
		}
		if h.Jobs == nil {
			h.Jobs = map[string]config.Job{}
		}
		seen := map[string]bool{}
		for _, fj := range fh.Jobs {
			seen[fj.ID] = true
			old, had := h.Jobs[fj.ID]
			j := old
			if !had {
				j = config.Job{Engine: config.EngineRestic, Retention: fj.Retention} // off-site retention starts as primary's
			}
			primary := fj.Retention
			j.Paths, j.Excludes, j.Dump, j.Schedule, j.Primary, j.Retired = fj.Paths, fj.Excludes, fj.Dump, fj.Schedule, &primary, false
			if j.Paths == nil {
				j.Paths = []string{}
			}
			switch {
			case !had:
				changes = append(changes, fmt.Sprintf("add job %s on %s", fj.ID, fh.FQDN))
			case old.Retired:
				changes = append(changes, fmt.Sprintf("job %s on %s is back", fj.ID, fh.FQDN))
			case !reflect.DeepEqual(old, j):
				changes = append(changes, fmt.Sprintf("job %s on %s changed", fj.ID, fh.FQDN))
			}
			h.Jobs[fj.ID] = j
		}
		for id, j := range h.Jobs {
			if !seen[id] && !j.Retired {
				j.Retired = true
				h.Jobs[id] = j
				changes = append(changes, fmt.Sprintf("job %s on %s retired", id, fh.FQDN))
			}
		}
		doc.Hosts[fh.FQDN] = h
	}
	for fqdn, h := range doc.Hosts {
		if inFeed[fqdn] {
			continue
		}
		for id, j := range h.Jobs {
			if !j.Retired {
				j.Retired = true
				h.Jobs[id] = j
				changes = append(changes, fmt.Sprintf("job %s on %s retired", id, fqdn))
			}
		}
	}
	sort.Strings(changes)
	return doc, changes
}

// activateCopiedHost records a host the vault starts to copy, with a password of its own for its repository in
// site B. A host the vault had removed comes back with its repository, unless that was decommissioned.
func (s *Server) activateCopiedHost(ctx context.Context, q *store.Q, fqdn string, rev int64) error {
	now := s.now()
	h, err := q.GetHost(ctx, fqdn)
	switch {
	case errors.Is(err, store.ErrNotFound):
		if err := q.ActivateHost(ctx, fqdn, rev, now, false); err != nil {
			return err
		}
		return q.PutCredentials(ctx, store.SealedCredentials{FQDN: fqdn, Version: 1, Repository: s.loc.RepoURL(fqdn),
			RepoPassword: s.seal.Seal(creds.NewPassword(), fqdn), CreatedAt: now})
	case err != nil:
		return err
	case h.State != "retired":
		return nil
	}
	fresh := decommissioned(h)
	if err := q.ActivateHost(ctx, fqdn, rev, now, true); err != nil {
		return err
	}
	if !fresh {
		return nil
	}
	c, err := q.GetCredentials(ctx, fqdn)
	if err != nil {
		return err
	}
	c.Version++
	c.RepoPassword, c.CreatedAt = s.seal.Seal(creds.NewPassword(), fqdn), now
	return q.PutCredentials(ctx, c)
}

/* ---------------- the copier ---------------- */

// copyRunners are restic on a host's primary repository, read-only through the server's loopback endpoint, and on its
// off-site one, with the primary as the source of a copy.
func (s *Server) copyRunners(ctx context.Context, src string, fh proto.VaultHost) (a, b restic.Runner, cleanup func(), err error) {
	b, cleanB, err := s.maintRunner(ctx, fh.FQDN)
	if err != nil {
		return a, b, nil, err
	}
	pf, cleanA, err := s.passwordFile(fh.FQDN, fh.RepoPassword)
	if err != nil {
		cleanB()
		return a, b, nil, err
	}
	cleanup = func() { cleanA(); cleanB() }
	a = restic.Runner{Bin: s.set.ResticBin, Repo: "rest:" + src + fh.FQDN + "/", PasswordFile: pf, CacheDir: filepath.Join(s.set.StateDir, "cache"), NoLock: true}
	b.NoLock = true
	b.From = &restic.Source{Repo: a.Repo, PasswordFile: pf}
	return a, b, cleanup, nil
}

// copyHost copies a host's new snapshots, of the jobs it copies, unless a forget or prune runs on its repository.
func (s *Server) copyHost(ctx context.Context, src string, fh proto.VaultHost) {
	head, err := s.head(ctx, s.st.Q())
	if err != nil {
		return
	}
	jobs := map[string]bool{}
	for id, j := range head.Config.Hosts[fh.FQDN].Jobs {
		if !j.Excluded && !j.Retired {
			jobs[id] = true
		}
	}
	if len(jobs) == 0 {
		s.setCopyState(fh.FQDN, nil)
		return
	}
	// Not while a forget or prune waits or runs: the next run copies it.
	lock := s.repoLock.get(fh.FQDN)
	if s.maintBusy.isBusy(fh.FQDN) || !lock.TryLock() {
		return
	}
	defer lock.Unlock()
	ctx, cancel := context.WithTimeout(ctx, copyTimeout)
	defer cancel()
	err = s.copyOnce(ctx, src, fh, jobs)
	msg := ""
	if err != nil {
		msg = err.Error()
		s.log.Warn("the copy to off-site failed", "host", fh.FQDN, "err", err)
	}
	if rerr := s.st.Q().RecordCopy(ctx, fh.FQDN, s.now(), msg); rerr != nil {
		s.log.Error("recording a copy", "host", fh.FQDN, "err", rerr)
	}
}

func jobOf(tags []string) string {
	for _, t := range tags {
		if id, ok := strings.CutPrefix(t, "job="); ok {
			return id
		}
	}
	return ""
}

func (s *Server) copyOnce(ctx context.Context, src string, fh proto.VaultHost, jobs map[string]bool) error {
	a, b, cleanup, err := s.copyRunners(ctx, src, fh)
	if err != nil {
		return err
	}
	defer cleanup()
	listed, err := a.Snapshots(ctx)
	if err != nil {
		return fmt.Errorf("listing the snapshots in primary: %w", err)
	}
	// How far behind each job is, before the copy too: so it shows while this site's storage fails.
	s.updateCopyState(ctx, fh.FQDN, listed, jobs)
	// The storage first, as for a primary repository (ensureStorage): restic init would make a missing bucket itself.
	created, err := b.InitFrom(ctx, func() error {
		if rep := s.vaultStorageCheck(ctx, fh.FQDN); !rep.OK() {
			return &StorageError{Report: rep}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("initialising the off-site repository with the primary's chunker parameters: %w", err)
	}
	if created {
		s.recordInitialised(ctx, b, fh.FQDN)
	}
	// What was ever copied, tombstones included: a copy that retention or Delete here too removed is not made again.
	known, err := s.st.Q().ListSnapshots(ctx, fh.FQDN, store.SnapshotFilter{WithGone: true})
	if err != nil {
		return err
	}
	copied := map[string]bool{}
	for _, k := range known {
		if k.Original != "" {
			copied[k.Original] = true
		}
	}
	var want []restic.Snapshot
	for _, sn := range listed {
		if jobs[jobOf(sn.Tags)] && !copied[sn.ID] {
			want = append(want, sn)
		}
	}
	sort.Slice(want, func(i, k int) bool { return want[i].Time.Before(want[k].Time) })
	var copyErr error
	if len(want) > 0 {
		ids := make([]string, 0, len(want))
		for _, sn := range want {
			ids = append(ids, sn.ID)
		}
		started := s.now()
		if err := b.Copy(ctx, ids); err != nil {
			copyErr = fmt.Errorf("restic copy: %w", err)
		}
		// Whatever was copied, even by a copy that failed part way, is in the repository now.
		if err := s.RefreshCatalogue(ctx, fh.FQDN); err != nil {
			return errors.Join(copyErr, err)
		}
		s.recordCopies(ctx, fh.FQDN, want, started)
	}
	s.updateCopyState(ctx, fh.FQDN, listed, jobs)
	if copyErr == nil {
		s.dailyCheck(ctx, b, fh.FQDN)
	}
	return copyErr
}

// recordInitialised keeps the new repository's keys as Holdfast's own, as initRepository does, and audits it.
func (s *Server) recordInitialised(ctx context.Context, r restic.Runner, fqdn string) {
	var ids []string
	if keys, err := r.Keys(ctx); err == nil {
		for _, k := range keys {
			ids = append(ids, k.ID)
		}
	}
	err := s.st.InTx(ctx, func(q *store.Q) error {
		again, err := q.ResetRepositoryKeys(ctx, fqdn, ids)
		if err != nil {
			return err
		}
		return q.Audit(ctx, store.AuditEntry{Actor: vaultSync.Name, Source: vaultSync.Source, Action: "repository.initialised", FQDN: fqdn,
			Detail: map[string]any{"repository": r.Repo, "again": again, "chunker_from": r.From.Repo}})
	})
	if err != nil {
		s.log.Error("recording a new repository", "host", fqdn, "err", err)
	}
	s.vaultStorageCheck(ctx, fqdn)
}

// recordCopies puts each new copy in the host's history as a run, so the copies count as made by Holdfast.
func (s *Server) recordCopies(ctx context.Context, fqdn string, want []restic.Snapshot, started time.Time) {
	wanted := map[string]bool{}
	for _, sn := range want {
		wanted[sn.ID] = true
	}
	rows, err := s.st.Q().ListSnapshots(ctx, fqdn, store.SnapshotFilter{})
	if err != nil {
		s.log.Error("listing the new copies", "host", fqdn, "err", err)
		return
	}
	now := s.now()
	for _, row := range rows {
		if !wanted[row.Original] || row.MadeByHoldfast {
			continue
		}
		ev := store.Event{FQDN: fqdn, ID: "copy-" + row.ID, Kind: proto.KindBackup, JobID: row.JobID, Status: proto.StatusSuccess, Started: started,
			Finished: now, SnapshotID: row.ID, Detail: map[string]string{"copied_from": row.Original}, InitiatedBy: offsiteActor.Name}
		if row.TotalFiles != nil {
			ev.TotalFiles = *row.TotalFiles
		}
		if row.TotalBytes != nil {
			ev.TotalBytes = *row.TotalBytes
		}
		if err := s.st.InTx(ctx, func(q *store.Q) error { _, err := q.InsertEvent(ctx, ev); return err }); err != nil {
			s.log.Error("recording a copy", "host", fqdn, "copy", row.ID, "err", err)
		}
	}
}

// updateCopyState works out, from the source's listing and the catalogue, how far behind each job is.
func (s *Server) updateCopyState(ctx context.Context, fqdn string, listed []restic.Snapshot, jobs map[string]bool) {
	known, err := s.st.Q().ListSnapshots(ctx, fqdn, store.SnapshotFilter{WithGone: true})
	if err != nil {
		return
	}
	copied := map[string]bool{}
	newestCopy := map[string]time.Time{}
	for _, k := range known {
		if k.Original == "" {
			continue
		}
		copied[k.Original] = true
		if k.Time.After(newestCopy[k.JobID]) {
			newestCopy[k.JobID] = k.Time
		}
	}
	newest, oldestWanted := map[string]time.Time{}, map[string]time.Time{}
	for _, sn := range listed {
		job := jobOf(sn.Tags)
		if !jobs[job] {
			continue
		}
		if sn.Time.After(newest[job]) {
			newest[job] = sn.Time
		}
		if !copied[sn.ID] {
			if o, ok := oldestWanted[job]; !ok || sn.Time.Before(o) {
				oldestWanted[job] = sn.Time
			}
		}
	}
	behind := map[string]time.Duration{}
	now := s.now()
	for job, n := range newest {
		switch c, ok := newestCopy[job]; {
		case ok && !c.Before(n):
			behind[job] = 0
		case ok:
			behind[job] = n.Sub(c)
		default:
			behind[job] = now.Sub(oldestWanted[job]) // nothing copied yet: how long the oldest has waited
		}
	}
	s.setCopyState(fqdn, behind)
}

func (s *Server) setCopyState(fqdn string, behind map[string]time.Duration) {
	s.vault.mu.Lock()
	defer s.vault.mu.Unlock()
	if s.vault.behind == nil {
		s.vault.behind = map[string]map[string]time.Duration{}
	}
	s.vault.behind[fqdn] = behind
}

func (s *Server) copyState(fqdn string) map[string]time.Duration {
	s.vault.mu.Lock()
	defer s.vault.mu.Unlock()
	return s.vault.behind[fqdn]
}

// dailyCheck checks a copied repository once a day, reading 1/check_cycle_days of its data, as an agent checks its
// own (decision 030). It is a "check" run in the host's history.
func (s *Server) dailyCheck(ctx context.Context, b restic.Runner, fqdn string) {
	if s.set.Maintenance.Check != CheckDaily {
		return
	}
	s.vault.mu.Lock()
	if s.vault.checked == nil {
		s.vault.checked = map[string]time.Time{}
	}
	last := s.vault.checked[fqdn]
	s.vault.mu.Unlock()
	now := s.now()
	if now.Sub(last) < checkEach {
		return
	}
	cycle := s.set.Maintenance.CheckCycleDays
	subset := fmt.Sprintf("%d/%d", int(now.Unix()/86400)%cycle+1, cycle)
	b.NoLock, b.From, b.Options = false, nil, nil // restic check takes its lock; the copy is done
	err := b.Check(ctx, subset)
	ev := store.Event{FQDN: fqdn, ID: "check-" + newID(), Kind: proto.KindCheck, Status: proto.StatusSuccess, Started: now, Finished: s.now(),
		Detail: map[string]string{"subset": subset}, InitiatedBy: offsiteActor.Name}
	if err != nil {
		ev.Status, ev.Error = proto.StatusFailed, err.Error()
	}
	if ierr := s.st.InTx(ctx, func(q *store.Q) error { _, err := q.InsertEvent(ctx, ev); return err }); ierr != nil {
		s.log.Error("recording a check", "host", fqdn, "err", ierr)
	}
	s.vault.mu.Lock()
	s.vault.checked[fqdn] = now
	s.vault.mu.Unlock()
}

/* ---------------- the report ---------------- */

// vaultReport is what the vault tells its source: each host's copies, what it still wants, and its jobs.
func (s *Server) vaultReport(ctx context.Context) (proto.VaultSync, error) {
	rep := proto.VaultSync{Version: Version, Hosts: []proto.VaultHostReport{}}
	q := s.st.Q()
	head, err := s.head(ctx, q)
	if err != nil {
		return rep, err
	}
	hosts, err := q.ListHosts(ctx)
	if err != nil {
		return rep, err
	}
	for _, h := range hosts {
		hc, ok := head.Config.Hosts[h.FQDN]
		if !ok {
			continue
		}
		rows, err := q.ListSnapshots(ctx, h.FQDN, store.SnapshotFilter{})
		if err != nil {
			return rep, err
		}
		behind := s.copyState(h.FQDN)
		hr := proto.VaultHostReport{FQDN: h.FQDN, Copies: []proto.VaultCopy{}, Jobs: []proto.VaultJobReport{},
			LastCopy: h.CopiedAt, CopyError: h.CopyError, Retired: len(hc.Jobs) > 0}
		count, newest := map[string]int{}, map[string]time.Time{}
		for _, r := range rows {
			if r.Original == "" {
				continue
			}
			hr.Copies = append(hr.Copies, proto.VaultCopy{ID: r.ID, Original: r.Original, Time: r.Time, JobID: r.JobID,
				Dump: slices.ContainsFunc(r.Tags, func(t string) bool { return strings.HasPrefix(t, "dump=") })})
			count[r.JobID]++
			if r.Time.After(newest[r.JobID]) {
				newest[r.JobID] = r.Time
			}
		}
		ids := make([]string, 0, len(hc.Jobs))
		for id := range hc.Jobs {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			j := hc.Jobs[id]
			jr := proto.VaultJobReport{ID: id, Retention: j.Retention, Excluded: j.Excluded, Retired: j.Retired, Copies: count[id],
				BehindS: int64(behind[id] / time.Second)}
			if t, ok := newest[id]; ok {
				jr.NewestAt = &t
			}
			if j.Primary != nil {
				jr.Shorter = len(checks.Shorter(j.Retention, *j.Primary)) > 0
			}
			hr.Retired = hr.Retired && j.Retired
			hr.Jobs = append(hr.Jobs, jr)
		}
		rep.Hosts = append(rep.Hosts, hr)
	}
	return rep, nil
}

/* ---------------- restores from off-site ---------------- */

// copySource checks that a copy can be restored, and returns its password and its copy's ID: the server queues the
// restore at once, and the host reads the copy through the gateway's off-site endpoint (decision 041). It is audited
// here as granted, or refused with why.
func (s *Server) copySource(ctx context.Context, a Actor, fqdn, copyID, command string) (string, error) {
	detail := map[string]any{"command": command, "snapshot": copyID, "requested_by": a.Name}
	refuse := func(format string, args ...any) (string, error) {
		msg := fmt.Sprintf(format, args...)
		detail["refused"] = msg
		_ = s.st.Q().Audit(ctx, store.AuditEntry{Actor: vaultSync.Name, Source: vaultSync.Source, Action: "restore.refused", FQDN: fqdn, Detail: detail})
		return "", fmt.Errorf("%s: %w", msg, ErrConflict)
	}
	q := s.st.Q()
	h, err := q.GetHost(ctx, fqdn)
	if err != nil {
		return refuse("nothing of %s is copied off-site", fqdn)
	}
	if h.Decommission != nil && h.Decommission.DoneAt != nil {
		return refuse("%s's off-site copies were decommissioned", fqdn)
	}
	sn, err := q.GetSnapshot(ctx, fqdn, copyID)
	if err != nil || sn.GoneAt != nil {
		return refuse("the copy %s is no longer off-site", copyID)
	}
	c, err := q.GetCredentials(ctx, fqdn)
	if err != nil {
		return refuse("there is no off-site password for %s", fqdn)
	}
	pw, err := s.seal.Open(c.RepoPassword, fqdn)
	if err != nil {
		return refuse("the off-site password of %s cannot be opened", fqdn)
	}
	if err := q.Audit(ctx, store.AuditEntry{Actor: vaultSync.Name, Source: vaultSync.Source, Action: "restore.granted", FQDN: fqdn, Detail: detail}); err != nil {
		return "", err
	}
	return pw, nil
}

/* ---------------- storage ---------------- */

// vaultStorageCheck tests the off-site storage for a host: the bucket's side of the contract with the off-site key
// (versioning, the lifecycle rule, what the key must not do).
func (s *Server) vaultStorageCheck(ctx context.Context, fqdn string) storage.Report {
	rep := s.prober.ProbeMaintenance(ctx)
	rep.CheckedAt = s.now()
	s.recordStorage(ctx, fqdn, rep)
	return rep
}

/* ---------------- what the off-site instance does not do ---------------- */

var errVaultHosts = fmt.Errorf("the off-site hosts and jobs follow the server's; add them there, not under offsite: %w", ErrConflict)

// vaultJobFindings are the checks on a vault's job: only those about retention, since the rest is the source's,
// with O101 when the off-site retention keeps less than primary, and R104 for a cut.
func (s *Server) vaultJobFindings(ctx context.Context, fqdn, jobID string, old *config.Job, job config.Job) []checks.Finding {
	fs := []checks.Finding{}
	for _, f := range checks.Job(job, checks.History{}) {
		if strings.HasPrefix(f.Path, "retention") {
			fs = append(fs, f)
		}
	}
	if job.Primary != nil {
		if f := checks.Offsite(job.Retention, *job.Primary); f != nil {
			fs = append(fs, *f)
		}
	}
	if old != nil && !slices.Equal(old.Retention.ForgetArgs(), job.Retention.ForgetArgs()) {
		if f := s.retentionCut(ctx, fqdn, jobID, old.Retention, job.Retention); f != nil {
			fs = append(fs, *f)
		}
	}
	checks.Sort(fs)
	return fs
}

// OffsiteInput is a vault action on one job.
type OffsiteInput struct {
	Message string   `json:"message,omitempty"`
	Accept  []string `json:"accept,omitempty"` // for Match primary: R104, if primary keeps less than the job does now
}

// SetExcluded is Exclude (true) or Include (false): an excluded job is not copied any more, and its copies stay
// under its off-site retention (decision 027).
func (s *Server) SetExcluded(ctx context.Context, a Actor, fqdn, jobID string, excluded bool, in OffsiteInput) (int64, error) {
	if !s.set.IsVault() {
		return 0, errNotVault
	}
	verb := "Include"
	if excluded {
		verb = "Exclude"
	}
	if strings.TrimSpace(in.Message) == "" {
		in.Message = fmt.Sprintf("%s job %s on %s", verb, jobID, fqdn)
	}
	var revNum int64
	err := s.st.InTx(ctx, func(q *store.Q) error {
		head, err := s.head(ctx, q)
		if err != nil {
			return err
		}
		j, ok := head.Config.Hosts[fqdn].Jobs[jobID]
		if !ok {
			return fmt.Errorf("job %s on %s: %w", jobID, fqdn, ErrNotFound)
		}
		if j.Excluded == excluded {
			revNum = head.Number
			return nil
		}
		j.Excluded = excluded
		doc := withJob(head.Config, fqdn, jobID, j)
		rev, err := q.SaveRevision(ctx, store.SaveRevision{Base: head.Number, Actor: a.Name, Source: a.Source, Message: in.Message, Accepted: []string{},
			Config: doc, HostSHAs: s.hostSHAs(doc)})
		if err != nil {
			return err
		}
		revNum = rev.Number
		return q.Audit(ctx, store.AuditEntry{Actor: a.Name, Source: a.Source, Action: "job." + strings.ToLower(verb), FQDN: fqdn, JobID: jobID, Revision: rev.Number,
			Detail: map[string]any{"message": in.Message}})
	})
	return revNum, err
}

// MatchPrimary sets a job's off-site retention to its primary retention again, with the checks of any change.
func (s *Server) MatchPrimary(ctx context.Context, a Actor, fqdn, jobID string, in OffsiteInput) (SetJobResult, error) {
	if !s.set.IsVault() {
		return SetJobResult{}, errNotVault
	}
	head, err := s.head(ctx, s.st.Q())
	if err != nil {
		return SetJobResult{}, err
	}
	j, ok := head.Config.Hosts[fqdn].Jobs[jobID]
	if !ok {
		return SetJobResult{}, fmt.Errorf("job %s on %s: %w", jobID, fqdn, ErrNotFound)
	}
	if j.Primary == nil {
		return SetJobResult{}, fmt.Errorf("job %s on %s has no primary retention from the source yet: %w", jobID, fqdn, ErrConflict)
	}
	if strings.TrimSpace(in.Message) == "" {
		in.Message = fmt.Sprintf("Match primary: job %s on %s gets its primary retention", jobID, fqdn)
	}
	j.Retention = *j.Primary
	base := head.Number
	return s.SetJob(ctx, a, fqdn, jobID, SetJobInput{Base: &base, Message: in.Message, Job: j, Accept: in.Accept})
}

var errNotVault = fmt.Errorf("off-site settings are set under offsite: holdfast --offsite, or /api/v1/offsite/: %w", ErrConflict)

func (s *Server) apiExclude(excluded bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var in OffsiteInput
		if r.ContentLength != 0 && !decode(w, r, &in) {
			return
		}
		rev, err := s.SetExcluded(r.Context(), actorOf(r), r.PathValue("fqdn"), r.PathValue("job"), excluded, in)
		if err != nil {
			apiError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"revision": rev, "excluded": excluded})
	}
}

func (s *Server) apiMatchPrimary(w http.ResponseWriter, r *http.Request) {
	var in OffsiteInput
	if r.ContentLength != 0 && !decode(w, r, &in) {
		return
	}
	res, err := s.MatchPrimary(r.Context(), actorOf(r), r.PathValue("fqdn"), r.PathValue("job"), in)
	if err != nil {
		apiError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}
