package server

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/kabsdk/holdfast/internal/config"
	"github.com/kabsdk/holdfast/internal/restic"
	"github.com/kabsdk/holdfast/internal/schedule"
	"github.com/kabsdk/holdfast/internal/server/store"
)

// Retention (decisions 015, 030, 031 and 032).
//
//   - restic decides what each policy keeps: `forget --dry-run` per job, grouped by host.
//   - Holdfast keeps the union over every policy in force during the last 7 days, and forgets the rest by
//     exact snapshot ID. It forgets only snapshots its agents reported; the others are not its own.
//   - Then it checks the repository's structure and prunes. A failed check blocks the prune.
//   - It runs once a day per host, in the middle of the host's longest quiet stretch, so it rarely meets a
//     backup. Only this server runs forget and prune; agents never do (decision 007).
//   - Prune now and Delete now are the only bypasses of the 7 days: an admin's, behind the host's typed
//     short name.

// Protection is how long a retention cut or a deletion waits before anything is forgotten (decision 015).
const Protection = 7 * 24 * time.Hour

const (
	maintRetryLock   = 10 * time.Minute // a backup that holds the repository finishes first
	retentionTimeout = 6 * time.Hour    // one run: forget, check and prune
	planTimeout      = 10 * time.Minute // the dry runs for one host
	forgetBatch      = 100              // snapshot IDs per restic forget
)

// PolicyInForce is one retention policy a job had during the last 7 days.
type PolicyInForce struct {
	Revision  int64            `json:"revision"` // the newest revision with this policy
	Retention config.Retention `json:"retention"`
	Current   bool             `json:"current"`
	Until     *time.Time       `json:"until,omitempty"` // when it stopped being in force; nil for the current one
	Keeps     int              `json:"keeps"`           // how many snapshots restic says it keeps
}

// Reasons a snapshot is pending.
const (
	PendingCut      = "retention_cut" // the current policy drops it, an older one in force keeps it
	PendingDeletion = "deletion"      // an admin asked to delete it (decision 031)
	PendingOffsite  = "offsite_copy"  // a vault has not copied it yet: held while it is younger than 7 days (decision 039)
)

// PendingSnapshot is a snapshot that waits out the 7 days before it is forgotten.
type PendingSnapshot struct {
	ID          string     `json:"id"`
	ShortID     string     `json:"short_id"`
	Time        time.Time  `json:"time"`
	JobID       string     `json:"job_id,omitempty"`
	Reason      string     `json:"reason"`
	KeptBy      int64      `json:"kept_by_revision,omitempty"` // the revision whose policy still keeps it
	RequestedBy string     `json:"requested_by,omitempty"`
	RequestedAt *time.Time `json:"requested_at,omitempty"`
	Message     string     `json:"message,omitempty"`        // a deletion's reason (decision 055)
	PrunableAt  time.Time  `json:"prunable_at"`              // from then, the daily run forgets and prunes it
	GoneBy      *time.Time `json:"really_gone_by,omitempty"` // and S3's old versions of its data have expired
}

// JobRetention is what the policies in force decide for one job.
type JobRetention struct {
	JobID    string            `json:"job_id"`
	Policies []PolicyInForce   `json:"policies"` // the current one first
	Forget   []string          `json:"forget"`   // the daily run forgets these: no policy in force keeps them
	Pending  []PendingSnapshot `json:"pending"`  // the current policy drops these, an older one still keeps them
	DropNow  []string          `json:"drop_now"` // what the current policy alone drops: what Prune now forgets
	// NotMadeByHoldfast are snapshots the current policy would drop that no agent reported. Holdfast leaves
	// them alone (decision 032).
	NotMadeByHoldfast []string `json:"not_made_by_holdfast"`
	// KeptBy says why the current policy keeps each snapshot it keeps, in restic's words (spec §5.2).
	KeptBy map[string][]string `json:"kept_by"`
}

// RetentionPlan is what the daily run would do on a host now, and what waits (spec §3.4).
type RetentionPlan struct {
	FQDN    string            `json:"fqdn"`
	At      time.Time         `json:"at"`
	Jobs    []JobRetention    `json:"jobs"`
	Forget  []string          `json:"forget"`  // every job's Forget, and the deletions that waited 7 days
	Pending []PendingSnapshot `json:"pending"` // soonest first
	// NoncurrentDays is how long S3 keeps old versions after a prune (the bucket's lifecycle rule); 0 when
	// not known.
	NoncurrentDays int `json:"noncurrent_days"`
}

func shortName(fqdn string) string {
	name, _, _ := strings.Cut(fqdn, ".")
	return name
}

func (s *Server) goneBy(t time.Time, days int) *time.Time {
	if days <= 0 {
		return nil
	}
	g := t.AddDate(0, 0, days)
	return &g
}

func noncurrentDays(h store.Host) int {
	if h.Storage == nil {
		return 0
	}
	return h.Storage.NoncurrentDays
}

// Pending is the pending view: what waits, and until when.
func (s *Server) Pending(ctx context.Context, fqdn string) (RetentionPlan, error) {
	ctx, cancel := context.WithTimeout(ctx, planTimeout)
	defer cancel()
	return s.retentionPlan(ctx, fqdn)
}

func (s *Server) retentionPlan(ctx context.Context, fqdn string) (RetentionPlan, error) {
	now := s.now()
	plan := RetentionPlan{FQDN: fqdn, At: now, Jobs: []JobRetention{}, Forget: []string{}, Pending: []PendingSnapshot{}}
	h, err := s.st.Q().GetHost(ctx, fqdn)
	if err != nil {
		return plan, fmt.Errorf("host %s: %w", fqdn, err)
	}
	plan.NoncurrentDays = noncurrentDays(h)
	revs, err := s.st.Q().RevisionsInForce(ctx, now.Add(-Protection))
	if err != nil {
		return plan, err
	}
	if len(revs) > 0 {
		head := revs[len(revs)-1]
		ids := make([]string, 0, len(head.Config.Hosts[fqdn].Jobs))
		for id := range head.Config.Hosts[fqdn].Jobs {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		if len(ids) > 0 {
			r, cleanup, err := s.maintRunner(ctx, fqdn)
			if err != nil {
				return plan, err
			}
			defer cleanup()
			wanted := s.vaultsWant(ctx, fqdn)
			for _, id := range ids {
				jr, err := s.jobRetention(ctx, r, fqdn, id, revs)
				if err != nil {
					return plan, fmt.Errorf("the retention of job %s: %w", id, err)
				}
				s.holdForVaults(ctx, fqdn, &jr, wanted, now)
				plan.Jobs = append(plan.Jobs, jr)
				plan.Forget = append(plan.Forget, jr.Forget...)
				plan.Pending = append(plan.Pending, jr.Pending...)
			}
		}
	}
	dels, err := s.st.Q().ListPendingDeletions(ctx, fqdn)
	if err != nil {
		return plan, err
	}
	for _, d := range dels {
		due := d.RequestedAt.Add(Protection)
		if !now.Before(due) {
			if !slices.Contains(plan.Forget, d.SnapshotID) {
				plan.Forget = append(plan.Forget, d.SnapshotID)
			}
			continue
		}
		p := PendingSnapshot{ID: d.SnapshotID, ShortID: d.SnapshotID[:min(8, len(d.SnapshotID))], Reason: PendingDeletion, RequestedBy: d.RequestedBy,
			RequestedAt: &d.RequestedAt, Message: d.Message, PrunableAt: due}
		if sn, err := s.st.Q().GetSnapshot(ctx, fqdn, d.SnapshotID); err == nil {
			p.ShortID, p.Time, p.JobID = sn.ShortID, sn.Time, sn.JobID
		}
		// A deletion and a cut of the same snapshot: the deletion is what the person asked for.
		plan.Pending = slices.DeleteFunc(plan.Pending, func(x PendingSnapshot) bool { return x.ID == p.ID && x.PrunableAt.After(due) })
		plan.Pending = append(plan.Pending, p)
	}
	for i := range plan.Pending {
		plan.Pending[i].GoneBy = s.goneBy(plan.Pending[i].PrunableAt, plan.NoncurrentDays)
	}
	sort.SliceStable(plan.Pending, func(i, k int) bool {
		a, b := plan.Pending[i], plan.Pending[k]
		if !a.PrunableAt.Equal(b.PrunableAt) {
			return a.PrunableAt.Before(b.PrunableAt)
		}
		return a.Time.Before(b.Time)
	})
	return plan, nil
}

// jobRetentionNow is jobRetention for one job of a host, now.
func (s *Server) jobRetentionNow(ctx context.Context, fqdn, jobID string) (JobRetention, error) {
	revs, err := s.st.Q().RevisionsInForce(ctx, s.now().Add(-Protection))
	if err != nil {
		return JobRetention{}, err
	}
	if len(revs) == 0 {
		return JobRetention{}, fmt.Errorf("job %s on %s: %w", jobID, fqdn, ErrNotFound)
	}
	if _, ok := revs[len(revs)-1].Config.Hosts[fqdn].Jobs[jobID]; !ok {
		return JobRetention{}, fmt.Errorf("job %s on %s: %w", jobID, fqdn, ErrNotFound)
	}
	r, cleanup, err := s.maintRunner(ctx, fqdn)
	if err != nil {
		return JobRetention{}, err
	}
	defer cleanup()
	return s.jobRetention(ctx, r, fqdn, jobID, revs)
}

// jobRetention asks restic what each policy that was in force for the job during the last 7 days keeps
// (revs are the revisions in force then, oldest first; the job is in the newest).
func (s *Server) jobRetention(ctx context.Context, r restic.Runner, fqdn, jobID string, revs []store.Revision) (JobRetention, error) {
	type policy struct {
		PolicyInForce
		args         []string
		keep, remove []restic.Snapshot
	}
	var pols []*policy
	byArgs := map[string]*policy{}
	for i, rev := range revs {
		j, ok := rev.Config.Hosts[fqdn].Jobs[jobID]
		if !ok {
			continue
		}
		args := j.Retention.ForgetArgs()
		key := strings.Join(args, " ")
		p := byArgs[key]
		if p == nil {
			p = &policy{args: args}
			byArgs[key] = p
			pols = append(pols, p)
		}
		p.Revision, p.Retention = rev.Number, j.Retention
		if i == len(revs)-1 {
			p.Current, p.Until = true, nil
		} else if until := revs[i+1].CreatedAt; p.Until == nil || until.After(*p.Until) {
			p.Until = &until
		}
	}
	jr := JobRetention{JobID: jobID, Policies: []PolicyInForce{}, Forget: []string{}, Pending: []PendingSnapshot{}, DropNow: []string{}, NotMadeByHoldfast: []string{}}
	var cur *policy
	tag := "job=" + jobID
	removedBy := map[string]int{}
	keptBy := map[string][]*policy{}
	var seen []string
	for _, p := range pols {
		var err error
		if p.keep, p.remove, err = r.ForgetPlan(ctx, tag, p.args); err != nil {
			return jr, err
		}
		p.Keeps = len(p.keep)
		if p.Current {
			cur = p
		}
		for _, sn := range p.remove {
			removedBy[sn.ID]++
			seen = append(seen, sn.ID)
		}
		for _, sn := range p.keep {
			keptBy[sn.ID] = append(keptBy[sn.ID], p)
			seen = append(seen, sn.ID)
		}
	}
	if cur == nil {
		return jr, fmt.Errorf("job %s is not in the newest revision", jobID)
	}
	jr.KeptBy = map[string][]string{}
	for _, sn := range cur.keep {
		jr.KeptBy[sn.ID] = orEmpty(sn.KeptBy)
	}
	slices.SortFunc(pols, func(a, b *policy) int {
		switch {
		case a.Current:
			return -1
		case b.Current:
			return 1
		}
		return b.Until.Compare(*a.Until)
	})
	for _, p := range pols {
		jr.Policies = append(jr.Policies, p.PolicyInForce)
	}
	slices.Sort(seen)
	made, err := s.st.Q().MadeByHoldfast(ctx, fqdn, slices.Compact(seen))
	if err != nil {
		return jr, err
	}
	for _, sn := range cur.remove {
		if !made[sn.ID] {
			jr.NotMadeByHoldfast = append(jr.NotMadeByHoldfast, sn.ID)
			continue
		}
		jr.DropNow = append(jr.DropNow, sn.ID)
		// Forgotten only when every policy in force saw it and drops it. A snapshot that a backup made
		// between two dry runs is seen by some only; the next run decides it.
		if removedBy[sn.ID] == len(pols) {
			jr.Forget = append(jr.Forget, sn.ID)
			continue
		}
		var last *policy
		for _, p := range keptBy[sn.ID] {
			if !p.Current && (last == nil || p.Until.After(*last.Until)) {
				last = p
			}
		}
		if last != nil {
			jr.Pending = append(jr.Pending, PendingSnapshot{ID: sn.ID, ShortID: sn.ShortID, Time: sn.Time, JobID: jobID, Reason: PendingCut,
				KeptBy: last.Revision, PrunableAt: last.Until.Add(Protection)})
		}
	}
	return jr, nil
}

/* ---------------- runs ---------------- */

// busyHosts lets one forget-or-prune run at a time per host.
type busyHosts struct {
	mu   sync.Mutex
	busy map[string]bool
}

func (b *busyHosts) acquire(fqdn string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.busy == nil {
		b.busy = map[string]bool{}
	}
	if b.busy[fqdn] {
		return false
	}
	b.busy[fqdn] = true
	return true
}

func (b *busyHosts) release(fqdn string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.busy, fqdn)
}

func (b *busyHosts) isBusy(fqdn string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.busy[fqdn]
}

// repoLocks are one mutex per repository, which a vault's copy (vault.go) and a forget or prune both take: a copy
// runs with --no-lock, so restic's own lock does not keep a prune away from it. A forget or prune waits for a copy
// that runs; a copy is skipped while one waits or runs.
type repoLocks struct {
	mu sync.Mutex
	m  map[string]*sync.Mutex
}

func (r *repoLocks) get(fqdn string) *sync.Mutex {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.m == nil {
		r.m = map[string]*sync.Mutex{}
	}
	if r.m[fqdn] == nil {
		r.m[fqdn] = &sync.Mutex{}
	}
	return r.m[fqdn]
}

// RunStarted is a forget-and-prune that started in the background. Its outcome is in the list of prunes.
type RunStarted struct {
	Run  int64  `json:"run"`
	FQDN string `json:"fqdn"`
	Kind string `json:"kind"`
}

// beginRun records a run, if none runs on the host already.
func (s *Server) beginRun(ctx context.Context, fqdn, kind string, a Actor) (int64, error) {
	if !s.maintBusy.acquire(fqdn) {
		return 0, fmt.Errorf("a forget or prune is running on %s already: %w", fqdn, ErrConflict)
	}
	id, err := s.st.Q().StartMaintenanceRun(ctx, fqdn, kind, a.Name, s.now())
	if err != nil {
		s.maintBusy.release(fqdn)
		return 0, err
	}
	return id, nil
}

// pickFunc chooses the snapshots a run forgets.
type pickFunc func(ctx context.Context) ([]string, error)

// runMaintenance forgets what pick chooses, then, unless it is Delete now, checks the repository and
// prunes it if something waits for a prune. It records the outcome on the run.
func (s *Server) runMaintenance(id int64, fqdn, kind string, a Actor, pick pickFunc) store.MaintenanceRun {
	defer s.maintBusy.release(fqdn)
	lock := s.repoLock.get(fqdn)
	lock.Lock() // a vault's copy that runs finishes first
	defer lock.Unlock()
	ctx, cancel := context.WithTimeout(s.background(), retentionTimeout)
	defer cancel()
	run := store.MaintenanceRun{ID: id, FQDN: fqdn, Kind: kind, Actor: a.Name, Status: "success", Forgotten: []string{}}
	if err := s.maintain(ctx, &run, pick); err != nil {
		run.Status, run.Error = "failed", err.Error()
		s.log.Warn("a forget or prune failed", "host", fqdn, "kind", kind, "err", err)
		_ = s.st.Q().Audit(context.Background(), store.AuditEntry{Actor: a.Name, Source: a.Source, Action: "maintenance.failed", FQDN: fqdn,
			Detail: map[string]any{"run": id, "kind": kind, "error": err.Error(), "forgotten": run.Forgotten}})
	}
	t := s.now()
	run.FinishedAt = &t
	fctx, fcancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer fcancel()
	if err := s.st.Q().FinishMaintenanceRun(fctx, run); err != nil {
		s.log.Error("recording a maintenance run", "host", fqdn, "run", id, "err", err)
	}
	if len(run.Forgotten) > 0 {
		s.refreshSoon(fqdn)
	}
	return run
}

func (s *Server) maintain(ctx context.Context, run *store.MaintenanceRun, pick pickFunc) error {
	ids, err := pick(ctx)
	if err != nil {
		return err
	}
	r, cleanup, err := s.maintRunner(ctx, run.FQDN)
	if err != nil {
		return err
	}
	defer cleanup()
	q := s.st.Q()
	r.RetryLock = maintRetryLock
	s.unlockStale(ctx, &r, run)
	if len(ids) > 0 {
		forgotten, err := s.forget(ctx, r, run.FQDN, ids)
		run.Forgotten = forgotten
		if len(forgotten) > 0 {
			// A deletion an admin asked for, done; or none. A vault shows those as deleted in primary (decision 031).
			var onRequest []string
			for _, id := range forgotten {
				if q.RemovePendingDeletion(ctx, run.FQDN, id) == nil || run.Kind == store.RunDeleteNow {
					onRequest = append(onRequest, id)
				}
			}
			if len(onRequest) > 0 {
				if err := q.MarkDeletedOnRequest(ctx, run.FQDN, onRequest, s.now()); err != nil {
					s.log.Error("recording deletions", "host", run.FQDN, "err", err)
				}
			}
			action := map[string]string{store.RunPruneNow: "retention.prune_now_forgot", store.RunDeleteNow: "snapshot.deleted"}[run.Kind]
			if action == "" {
				action = "retention.forgot"
			}
			if aerr := q.Audit(ctx, store.AuditEntry{Actor: run.Actor, Source: "system", Action: action, FQDN: run.FQDN,
				Detail: map[string]any{"run": run.ID, "kind": run.Kind, "snapshots": forgotten}}); aerr != nil {
				s.log.Error("auditing a forget", "host", run.FQDN, "err", aerr)
			}
		}
		if err != nil {
			return err
		}
	}
	if run.Kind == store.RunDeleteNow {
		return nil // the space is freed at the next prune (decision 031)
	}
	h, err := q.GetHost(ctx, run.FQDN)
	if err != nil || !h.PruneNeeded {
		return err
	}
	// A structural check first: a damaged repository is not pruned.
	if err := r.Check(ctx, ""); err != nil {
		_ = q.Audit(ctx, store.AuditEntry{Actor: run.Actor, Source: "system", Action: "repository.prune_blocked", FQDN: run.FQDN,
			Detail: map[string]any{"run": run.ID, "error": err.Error()}})
		return fmt.Errorf("the repository check failed, so nothing was pruned: %w", err)
	}
	rep, err := r.Prune(ctx)
	if err != nil {
		return fmt.Errorf("restic prune: %w", err)
	}
	run.Pruned, run.Freed = true, rep.Freed
	if err := q.SetPruneNeeded(ctx, run.FQDN, false); err != nil {
		return err
	}
	return q.Audit(ctx, store.AuditEntry{Actor: run.Actor, Source: "system", Action: "repository.pruned", FQDN: run.FQDN,
		Detail: map[string]any{"run": run.ID, "kind": run.Kind, "freed": rep.Freed, "remaining": rep.Remaining}})
}

// unlockSkew is how far a host's clock may be off the server's for the server to remove the host's stale locks.
const unlockSkew = 5 * time.Minute

// unlockStale removes the repository's stale locks before a run's first step, and has restic remove them again when a
// step finds the repository locked, which then runs once more (restic.Runner.UnlockStale). A lock that a restic left
// behind when it was killed blocks every forget, check and prune, and an exclusive one the host's backups, until
// restic unlock removes it: restic never does by itself. restic unlock removes stale locks only: one older than 30
// minutes, while a running restic renews its own every 5. It reads a lock's time, which the host's clock wrote, so
// the server leaves the locks alone while the host's clock is more than unlockSkew off its own
// (HoldfastAgentClockSkew fires at a minute). Each unlock that removed a lock is audited.
func (s *Server) unlockStale(ctx context.Context, r *restic.Runner, run *store.MaintenanceRun) {
	q := s.st.Q()
	if a, err := q.GetAgent(ctx, run.FQDN); err == nil && time.Duration(max(a.ClockOffsetMS, -a.ClockOffsetMS))*time.Millisecond > unlockSkew {
		s.log.Warn("stale restic locks are left alone: the host's clock is off the server's", "host", run.FQDN, "offset_ms", a.ClockOffsetMS)
		return
	}
	r.UnlockStale = true
	r.OnUnlock = func(rep string) {
		s.log.Info("removed stale restic locks", "host", run.FQDN, "restic", rep)
		_ = q.Audit(ctx, store.AuditEntry{Actor: run.Actor, Source: "system", Action: "repository.stale_locks_removed", FQDN: run.FQDN,
			Detail: map[string]any{"run": run.ID, "restic": rep}})
	}
	if _, err := r.RemoveStaleLocks(ctx); err != nil {
		s.log.Warn("removing stale restic locks", "host", run.FQDN, "err", err)
	}
}

// forget has restic forget exactly these snapshots. Their catalogue rows are marked first, so a refresh in
// between does not report them as disappeared. It returns the ones that are gone afterwards.
func (s *Server) forget(ctx context.Context, r restic.Runner, fqdn string, ids []string) ([]string, error) {
	q := s.st.Q()
	if err := q.MarkDeleting(ctx, fqdn, ids); err != nil {
		return nil, err
	}
	for batch := range slices.Chunk(ids, forgetBatch) {
		if err := r.Forget(ctx, batch...); err != nil {
			return s.settleForget(ctx, r, fqdn, ids), fmt.Errorf("restic forget: %w", err)
		}
	}
	if err := q.MarkDeleted(ctx, fqdn, ids, s.now()); err != nil {
		return ids, err
	}
	return ids, q.SetPruneNeeded(ctx, fqdn, true)
}

// settleForget finds out, after a forget failed, which of the snapshots are gone, and marks only those.
func (s *Server) settleForget(ctx context.Context, r restic.Runner, fqdn string, ids []string) []string {
	r.NoLock = true
	list, err := r.Snapshots(ctx)
	if err != nil {
		s.log.Warn("listing the snapshots after a failed forget", "host", fqdn, "err", err)
		return nil // the marks stay, so none of them is reported as disappeared
	}
	present := map[string]bool{}
	for _, sn := range list {
		present[sn.ID] = true
	}
	var gone, still []string
	for _, id := range ids {
		if present[id] {
			still = append(still, id)
		} else {
			gone = append(gone, id)
		}
	}
	q := s.st.Q()
	_ = q.UnmarkDeleting(ctx, fqdn, still)
	if len(gone) > 0 {
		_ = q.MarkDeleted(ctx, fqdn, gone, s.now())
		_ = q.SetPruneNeeded(ctx, fqdn, true)
	}
	return gone
}

/* ---------------- the daily run ---------------- */

// retentionSlot is when a host's daily run is due today: the middle of the longest quiet stretch of its
// schedules, which ends at end.
func (s *Server) retentionSlot(now time.Time, h config.Host) (slot, end time.Time) {
	var runs []schedule.Placed
	for _, j := range h.Jobs {
		if loc, err := j.Schedule.Location(s.zone); err == nil {
			runs = append(runs, schedule.Placed{Schedule: j.Schedule, Loc: loc})
		}
	}
	start, end, _ := schedule.QuietWindow(now, s.zone, runs)
	return start.Add(end.Sub(start) / 2), end
}

// retentionDue starts the daily run of each enrolled host whose slot has come today. A run missed because
// the server was down waits for the next day's slot, so it never lands in the middle of the backups.
func (s *Server) retentionDue(ctx context.Context) {
	if !s.isLeader() || s.set.Maintenance.Retention == CheckOff {
		return
	}
	hosts, err := s.st.Q().ListHosts(ctx)
	if err != nil {
		return
	}
	head, err := s.head(ctx, s.st.Q())
	if err != nil {
		return
	}
	now := s.now()
	for _, h := range hosts {
		hc, ok := head.Config.Hosts[h.FQDN]
		if h.State != "enrolled" || !ok {
			continue
		}
		slot, end := s.retentionSlot(now, hc)
		if now.Before(slot) || !now.Before(end) {
			continue
		}
		if last, err := s.st.Q().LastScheduledRun(ctx, h.FQDN); err != nil || (last != nil && !last.Before(slot)) {
			continue
		}
		id, err := s.beginRun(ctx, h.FQDN, store.RunScheduled, systemActor)
		if err != nil {
			continue // a run on the host is still going
		}
		go s.runMaintenance(id, h.FQDN, store.RunScheduled, systemActor, s.pickRetention(h.FQDN))
	}
}

func (s *Server) pickRetention(fqdn string) pickFunc {
	return func(ctx context.Context) ([]string, error) {
		pctx, cancel := context.WithTimeout(ctx, planTimeout)
		defer cancel()
		plan, err := s.retentionPlan(pctx, fqdn)
		return plan.Forget, err
	}
}

// RunRetention starts the daily run on a host now. It keeps the 7-day protection; it only saves waiting
// for the host's slot.
func (s *Server) RunRetention(ctx context.Context, a Actor, fqdn string) (RunStarted, error) {
	if _, err := s.st.Q().GetHost(ctx, fqdn); err != nil {
		return RunStarted{}, fmt.Errorf("host %s: %w", fqdn, err)
	}
	id, err := s.beginRun(ctx, fqdn, store.RunNow, a)
	if err != nil {
		return RunStarted{}, err
	}
	_ = s.st.Q().Audit(ctx, store.AuditEntry{Actor: a.Name, Source: a.Source, Action: "retention.run_requested", FQDN: fqdn, Detail: map[string]any{"run": id}})
	go s.runMaintenance(id, fqdn, store.RunNow, a, s.pickRetention(fqdn))
	return RunStarted{Run: id, FQDN: fqdn, Kind: store.RunNow}, nil
}

/* ---------------- Prune now ---------------- */

// PruneNowPreview is Prune now's confirmation page (decision 015): exactly what would be forgotten, and
// the restic commands.
type PruneNowPreview struct {
	FQDN      string            `json:"fqdn"`
	Confirm   string            `json:"confirm"` // type this to confirm: the host's short name
	Count     int               `json:"count"`   // forgotten
	Kept      int               `json:"kept"`    // what the current policies keep
	From      *time.Time        `json:"from,omitempty"`
	To        *time.Time        `json:"to,omitempty"`
	Snapshots []PendingSnapshot `json:"snapshots"` // oldest first
	Commands  []string          `json:"commands"`
	// NotMadeByHoldfast counts snapshots the policy would drop that Holdfast leaves alone (decision 032).
	NotMadeByHoldfast int        `json:"not_made_by_holdfast"`
	GoneBy            *time.Time `json:"really_gone_by,omitempty"`
}

// PruneNowInput confirms Prune now.
type PruneNowInput struct {
	Confirm string `json:"confirm"`
	Message string `json:"message"`
}

// PruneNowPlan shows what Prune now would do: forget what the current policy alone drops, ignoring the
// 7-day union, then check and prune.
func (s *Server) PruneNowPlan(ctx context.Context, fqdn string) (PruneNowPreview, error) {
	ctx, cancel := context.WithTimeout(ctx, planTimeout)
	defer cancel()
	plan, err := s.retentionPlan(ctx, fqdn)
	if err != nil {
		return PruneNowPreview{}, err
	}
	p := PruneNowPreview{FQDN: fqdn, Confirm: shortName(fqdn), Snapshots: []PendingSnapshot{}}
	var ids []string
	for _, j := range plan.Jobs {
		ids = append(ids, j.DropNow...)
		p.NotMadeByHoldfast += len(j.NotMadeByHoldfast)
		if len(j.Policies) > 0 {
			p.Kept += j.Policies[0].Keeps // the current policy comes first
		}
	}
	for _, id := range ids {
		ps := PendingSnapshot{ID: id, ShortID: id[:min(8, len(id))]}
		if sn, err := s.st.Q().GetSnapshot(ctx, fqdn, id); err == nil {
			ps.ShortID, ps.Time, ps.JobID = sn.ShortID, sn.Time, sn.JobID
		}
		p.Snapshots = append(p.Snapshots, ps)
	}
	sort.SliceStable(p.Snapshots, func(i, k int) bool { return p.Snapshots[i].Time.Before(p.Snapshots[k].Time) })
	p.Count = len(p.Snapshots)
	if p.Count > 0 {
		from, to := p.Snapshots[0].Time, p.Snapshots[p.Count-1].Time
		p.From, p.To = &from, &to
	}
	repo := s.loc.RepoURL(fqdn)
	if p.Count > 0 {
		p.Commands = append(p.Commands, fmt.Sprintf("restic -r %s --retry-lock %s forget -- %s", repo, maintRetryLock, strings.Join(ids, " ")))
	}
	p.Commands = append(p.Commands, fmt.Sprintf("restic -r %s --retry-lock %s check", repo, maintRetryLock),
		fmt.Sprintf("restic -r %s --retry-lock %s prune", repo, maintRetryLock))
	p.GoneBy = s.goneBy(s.now(), plan.NoncurrentDays)
	return p, nil
}

func (s *Server) confirmed(fqdn, typed string) error {
	if strings.TrimSpace(typed) != shortName(fqdn) {
		return &ValidationError{config.Problems{{Path: "confirm", Message: fmt.Sprintf("type the host's short name, %s, to confirm", shortName(fqdn))}}}
	}
	return nil
}

// PruneNow forgets what the current policy alone drops and prunes, without the 7-day protection
// (decision 015). It runs in the background.
func (s *Server) PruneNow(ctx context.Context, a Actor, fqdn string, in PruneNowInput) (RunStarted, error) {
	if _, err := s.st.Q().GetHost(ctx, fqdn); err != nil {
		return RunStarted{}, fmt.Errorf("host %s: %w", fqdn, err)
	}
	if err := s.confirmed(fqdn, in.Confirm); err != nil {
		return RunStarted{}, err
	}
	id, err := s.beginRun(ctx, fqdn, store.RunPruneNow, a)
	if err != nil {
		return RunStarted{}, err
	}
	_ = s.st.Q().Audit(ctx, store.AuditEntry{Actor: a.Name, Source: a.Source, Action: "retention.prune_now", FQDN: fqdn,
		Detail: map[string]any{"run": id, "message": in.Message}})
	pick := func(ctx context.Context) ([]string, error) {
		pctx, cancel := context.WithTimeout(ctx, planTimeout)
		defer cancel()
		plan, err := s.retentionPlan(pctx, fqdn)
		var ids []string
		for _, j := range plan.Jobs {
			ids = append(ids, j.DropNow...)
		}
		return ids, err
	}
	go s.runMaintenance(id, fqdn, store.RunPruneNow, a, pick)
	return RunStarted{Run: id, FQDN: fqdn, Kind: store.RunPruneNow}, nil
}

/* ---------------- Delete snapshot (decision 031) ---------------- */

// DeleteSnapshotInput asks to delete one snapshot: after 7 days, or now, with the confirmation.
type DeleteSnapshotInput struct {
	Message string `json:"message"`
	Now     bool   `json:"now,omitempty"`
	Confirm string `json:"confirm,omitempty"` // with now: the host's short name
}

// DeleteSnapshotResult says what happens to the snapshot.
type DeleteSnapshotResult struct {
	SnapshotID string     `json:"snapshot_id"`
	Status     string     `json:"status"`                // pending, or deleting with Delete now
	PrunableAt *time.Time `json:"prunable_at,omitempty"` // when the daily run forgets it
	Run        int64      `json:"run,omitempty"`         // Delete now's run
	// GoneBy is when S3's old versions of its data expire, counted from the forget's next prune.
	GoneBy *time.Time `json:"really_gone_by,omitempty"`
}

// DeleteSnapshot asks restic to forget one snapshot: after the 7-day wait, or at once with Delete now.
// The space is freed at the next prune.
func (s *Server) DeleteSnapshot(ctx context.Context, a Actor, fqdn, id string, in DeleteSnapshotInput) (DeleteSnapshotResult, error) {
	// A deletion always says why: the reason goes into the audit log and onto the snapshot (decision 055).
	if strings.TrimSpace(in.Message) == "" {
		return DeleteSnapshotResult{}, &ValidationError{config.Problems{{Path: "message", Message: "say why: deleting a snapshot needs a reason, which goes into the audit log"}}}
	}
	h, err := s.st.Q().GetHost(ctx, fqdn)
	if err != nil {
		return DeleteSnapshotResult{}, fmt.Errorf("host %s: %w", fqdn, err)
	}
	sn, err := s.snapshot(ctx, fqdn, id)
	if err != nil {
		return DeleteSnapshotResult{}, err
	}
	days := noncurrentDays(h)
	if in.Now {
		if err := s.confirmed(fqdn, in.Confirm); err != nil {
			return DeleteSnapshotResult{}, err
		}
		run, err := s.beginRun(ctx, fqdn, store.RunDeleteNow, a)
		if err != nil {
			return DeleteSnapshotResult{}, err
		}
		if err := s.st.Q().SetDeletionReason(ctx, fqdn, sn.ID, a.Name, in.Message); err != nil {
			s.log.Warn("keeping a deletion's reason", "host", fqdn, "snapshot", sn.ID, "err", err)
		}
		_ = s.st.Q().Audit(ctx, store.AuditEntry{Actor: a.Name, Source: a.Source, Action: "snapshot.delete_now", FQDN: fqdn, JobID: sn.JobID,
			Detail: map[string]any{"snapshot": sn.ID, "run": run, "message": in.Message}})
		go s.runMaintenance(run, fqdn, store.RunDeleteNow, a, func(context.Context) ([]string, error) { return []string{sn.ID}, nil })
		return DeleteSnapshotResult{SnapshotID: sn.ID, Status: "deleting", Run: run}, nil
	}
	now := s.now()
	err = s.st.InTx(ctx, func(q *store.Q) error {
		if err := q.AddPendingDeletion(ctx, store.PendingDeletion{FQDN: fqdn, SnapshotID: sn.ID, RequestedBy: a.Name, RequestedAt: now, Message: in.Message}); err != nil {
			if errors.Is(err, store.ErrConflict) {
				return fmt.Errorf("snapshot %s is pending deletion already: %w", sn.ShortID, ErrConflict)
			}
			return err
		}
		if err := q.SetDeletionReason(ctx, fqdn, sn.ID, a.Name, in.Message); err != nil {
			return err
		}
		return q.Audit(ctx, store.AuditEntry{Actor: a.Name, Source: a.Source, Action: "snapshot.delete_requested", FQDN: fqdn, JobID: sn.JobID,
			Detail: map[string]any{"snapshot": sn.ID, "message": in.Message}})
	})
	if err != nil {
		return DeleteSnapshotResult{}, err
	}
	due := now.Add(Protection)
	return DeleteSnapshotResult{SnapshotID: sn.ID, Status: "pending", PrunableAt: &due, GoneBy: s.goneBy(due, days)}, nil
}

// CancelDeletion cancels a pending deletion.
func (s *Server) CancelDeletion(ctx context.Context, a Actor, fqdn, id string) error {
	sn, err := s.st.Q().GetSnapshot(ctx, fqdn, id)
	if err == nil {
		id = sn.ID
	}
	return s.st.InTx(ctx, func(q *store.Q) error {
		if err := q.RemovePendingDeletion(ctx, fqdn, id); err != nil {
			return fmt.Errorf("no pending deletion of snapshot %s on %s: %w", id, fqdn, err)
		}
		if err := q.SetDeletionReason(ctx, fqdn, id, "", ""); err != nil {
			return err
		}
		return q.Audit(ctx, store.AuditEntry{Actor: a.Name, Source: a.Source, Action: "snapshot.delete_cancelled", FQDN: fqdn,
			Detail: map[string]any{"snapshot": id}})
	})
}

/* ---------------- Keep longer ---------------- */

// KeepLongerInput chooses the older retention to bring back: by default the newest one that differs from
// the current one.
type KeepLongerInput struct {
	Revision int64    `json:"revision,omitempty"`
	Message  string   `json:"message"`
	Accept   []string `json:"accept,omitempty"`
}

// KeepLonger saves a new revision in which the job has an older retention again, one that was in force
// during the last 7 days, so the snapshots it keeps are no longer pending (decision 015).
func (s *Server) KeepLonger(ctx context.Context, a Actor, fqdn, jobID string, in KeepLongerInput) (SetJobResult, error) {
	revs, err := s.st.Q().RevisionsInForce(ctx, s.now().Add(-Protection))
	if err != nil {
		return SetJobResult{}, err
	}
	if len(revs) == 0 {
		return SetJobResult{}, fmt.Errorf("job %s on %s: %w", jobID, fqdn, ErrNotFound)
	}
	cur, ok := revs[len(revs)-1].Config.Hosts[fqdn].Jobs[jobID]
	if !ok {
		return SetJobResult{}, fmt.Errorf("job %s on %s: %w", jobID, fqdn, ErrNotFound)
	}
	curArgs := cur.Retention.ForgetArgs()
	var from *store.Revision
	for i := len(revs) - 1; i >= 0; i-- {
		j, ok := revs[i].Config.Hosts[fqdn].Jobs[jobID]
		if !ok || (in.Revision != 0 && revs[i].Number != in.Revision) {
			continue
		}
		if !slices.Equal(j.Retention.ForgetArgs(), curArgs) {
			from = &revs[i]
			break
		}
		if in.Revision != 0 {
			return SetJobResult{}, &ValidationError{config.Problems{{Path: "revision", Message: fmt.Sprintf("revision %d has the current retention", in.Revision)}}}
		}
	}
	if from == nil {
		msg := "no other retention was in force for this job during the last 7 days"
		if in.Revision != 0 {
			msg = fmt.Sprintf("revision %d was not in force during the last 7 days, or has no job %s", in.Revision, jobID)
		}
		return SetJobResult{}, &ValidationError{config.Problems{{Path: "revision", Message: msg}}}
	}
	job := cur
	job.Retention = from.Config.Hosts[fqdn].Jobs[jobID].Retention
	if strings.TrimSpace(in.Message) == "" {
		in.Message = fmt.Sprintf("Keep longer: job %s on %s gets the retention of revision %d again", jobID, fqdn, from.Number)
	}
	return s.SetJob(ctx, a, fqdn, jobID, SetJobInput{Message: in.Message, Job: job, Accept: in.Accept})
}

/* ---------------- the list of prunes (OQ2) ---------------- */

// PruneEntry is one past forget-and-prune.
type PruneEntry struct {
	store.MaintenanceRun
	// GoneBy is when S3's old versions of what the prune deleted expire (the bucket's lifecycle rule).
	GoneBy *time.Time `json:"really_gone_by,omitempty"`
}

// Prunes lists a host's past runs, newest first; every host's when fqdn is empty. Daily runs that found
// nothing to do are left out unless all is set.
func (s *Server) Prunes(ctx context.Context, fqdn string, all bool, limit int) ([]PruneEntry, error) {
	days := map[string]int{}
	hosts, err := s.st.Q().ListHosts(ctx)
	if err != nil {
		return nil, err
	}
	known := false
	for _, h := range hosts {
		days[h.FQDN] = noncurrentDays(h)
		known = known || h.FQDN == fqdn
	}
	if fqdn != "" && !known {
		return nil, fmt.Errorf("host %s: %w", fqdn, ErrNotFound)
	}
	runs, err := s.st.Q().ListMaintenanceRuns(ctx, fqdn, limit)
	if err != nil {
		return nil, err
	}
	out := []PruneEntry{}
	for _, r := range runs {
		if !all && r.Status == "success" && !r.Pruned && len(r.Forgotten) == 0 {
			continue
		}
		e := PruneEntry{MaintenanceRun: r}
		if r.Pruned && r.FinishedAt != nil {
			e.GoneBy = s.goneBy(*r.FinishedAt, days[r.FQDN])
		}
		out = append(out, e)
	}
	return out, nil
}
