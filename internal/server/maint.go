package server

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/kabsdk/holdfast/internal/config"
	"github.com/kabsdk/holdfast/internal/schedule"
	"github.com/kabsdk/holdfast/internal/server/store"
)

// Maintenance in M1 is `restic check` only (plan-m1 Q1). There is no forget and no prune: nothing
// destructive exists before the 7-day protection (M3). The leader gate is a stub: a single server is
// always the leader until failover lands (M6).
//
// restic check takes an exclusive lock (measured on 0.19.1; the e2e suite asserts it). While check holds
// it, a backup of the same host waits out its --retry-lock and then fails with exit 11; a check that
// meets a running backup does the same. So each host is checked once a day, in the middle of that day's
// longest quiet stretch, as far as possible from its runs on either side (plan-m1 §10).

func (s *Server) isLeader() bool { return true }

const (
	checkTick      = time.Minute      // how often the server looks for checks that are due
	parallelChecks = 2                // checks of different hosts that may run at the same time
	checkRetryLock = 10 * time.Minute // how long a check waits for a backup that holds a lock
)

var systemActor = Actor{Name: "holdfast-server", Source: "system"}

// CheckResult is the outcome of one host's check.
type CheckResult struct {
	FQDN   string `json:"fqdn"`
	Subset string `json:"subset"`
	Status string `json:"status"` // success, failed or skipped
	Error  string `json:"error,omitempty"`
}

// RunChecks checks every enrolled host's repository now, reading a rotating subset of the data. It is
// `holdfast check`. A host whose check is already running is reported as skipped.
func (s *Server) RunChecks(ctx context.Context, a Actor) []CheckResult {
	out := []CheckResult{}
	if !s.isLeader() {
		return out
	}
	hosts, err := s.st.Q().ListHosts(ctx)
	if err != nil {
		return append(out, CheckResult{Status: "failed", Error: err.Error()})
	}
	for _, h := range hosts {
		if h.State != "enrolled" {
			continue
		}
		if !s.claimCheck(h.FQDN) {
			out = append(out, CheckResult{FQDN: h.FQDN, Status: "skipped", Error: "a check of this host is already running"})
			continue
		}
		out = append(out, s.checkHost(ctx, a, h.FQDN))
		s.releaseCheck(h.FQDN)
	}
	return out
}

func (s *Server) subset(now time.Time) string {
	cycle := s.set.Maintenance.CheckCycleDays
	return fmt.Sprintf("%d/%d", now.YearDay()%cycle+1, cycle)
}

// checkHost runs, records and audits one check. The caller holds the host's claim.
func (s *Server) checkHost(ctx context.Context, a Actor, fqdn string) CheckResult {
	now := s.now()
	res := CheckResult{FQDN: fqdn, Subset: s.subset(now), Status: "success"}
	id, err := s.st.Q().StartMaintenance(ctx, fqdn, "check", res.Subset, s.set.NodeName, now)
	if err != nil {
		res.Status, res.Error = "failed", err.Error()
		return res
	}
	if err := s.checkOne(ctx, fqdn, res.Subset); err != nil {
		res.Status, res.Error = "failed", err.Error()
	}
	s.finishCheck(ctx, a, id, res)
	return res
}

func (s *Server) finishCheck(ctx context.Context, a Actor, id int64, res CheckResult) {
	q := s.st.Q()
	if err := q.FinishMaintenance(ctx, id, res.Status, res.Error, s.now()); err != nil {
		s.log.Error("recording a check", "host", res.FQDN, "err", err)
	}
	if err := q.Audit(ctx, store.AuditEntry{Actor: a.Name, Source: a.Source, Action: "maintenance.check", FQDN: res.FQDN,
		Detail: map[string]any{"subset": res.Subset, "status": res.Status, "error": res.Error}}); err != nil {
		s.log.Error("auditing a check", "host", res.FQDN, "err", err)
	}
}

func (s *Server) checkOne(ctx context.Context, fqdn, subset string) error {
	r, cleanup, err := s.maintRunner(ctx, fqdn)
	if err != nil {
		return err
	}
	defer cleanup()
	r.RetryLock = checkRetryLock
	return r.Check(ctx, subset)
}

// claimCheck makes sure a host has one check at a time, whoever started it.
func (s *Server) claimCheck(fqdn string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.checking[fqdn] {
		return false
	}
	s.checking[fqdn] = true
	return true
}

func (s *Server) releaseCheck(fqdn string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.checking, fqdn)
}

/* ---------------- the daily check ---------------- */

// checkPlan returns when the check of a host is planned on the day that contains t, in the fleet's
// zone, and when that day's quiet stretch ends.
func (s *Server) checkPlan(h config.Host, t time.Time) (at, until time.Time) {
	runs := make([]schedule.Placed, 0, len(h.Jobs))
	for _, j := range h.Jobs {
		loc, err := j.Schedule.Location(s.zone)
		if err != nil || j.Schedule.Validate() != nil {
			continue // a saved revision is valid; this only guards the arithmetic
		}
		runs = append(runs, schedule.Placed{Schedule: j.Schedule, Loc: loc})
	}
	start, end, _ := schedule.QuietWindow(t, s.zone, runs)
	return start.Add(end.Sub(start) / 2), end
}

// nextCheck returns when the host's next daily check starts, given when its last check started (zero
// if never). A host is checked at most once a day. A check that could not start before the day's quiet
// stretch ended waits for the next day rather than run into a backup.
func (s *Server) nextCheck(h config.Host, last, now time.Time) time.Time {
	dayStart, dayEnd := schedule.Day(now, s.zone)
	if last.Before(dayStart) {
		if at, until := s.checkPlan(h, now); now.Before(until) {
			if at.Before(now) {
				return now // due, and starting within a tick
			}
			return at
		}
	}
	at, _ := s.checkPlan(h, dayEnd)
	return at
}

type dueCheck struct {
	fqdn  string
	until time.Time // the end of the host's quiet stretch
}

// dueChecks returns the enrolled hosts whose daily check should start now, the most urgent first.
func (s *Server) dueChecks(ctx context.Context, now time.Time) ([]dueCheck, error) {
	q := s.st.Q()
	hosts, err := q.ListHosts(ctx)
	if err != nil {
		return nil, err
	}
	head, err := s.head(ctx, q)
	if err != nil {
		return nil, err
	}
	last, err := q.LastMaintenanceStart(ctx, "check")
	if err != nil {
		return nil, err
	}
	var due []dueCheck
	for _, h := range hosts {
		if h.State != "enrolled" {
			continue
		}
		hc := head.Config.Hosts[h.FQDN]
		if s.nextCheck(hc, last[h.FQDN], now).After(now) {
			continue
		}
		_, until := s.checkPlan(hc, now)
		due = append(due, dueCheck{fqdn: h.FQDN, until: until})
	}
	slices.SortFunc(due, func(a, b dueCheck) int { return a.until.Compare(b.until) })
	return due, nil
}

// startDueChecks starts the checks that are due, at most parallelChecks at a time. A check still
// waiting for a free slot when its host's quiet stretch ends is recorded as skipped for the day.
func (s *Server) startDueChecks(ctx context.Context) {
	if !s.isLeader() {
		return
	}
	due, err := s.dueChecks(ctx, s.now())
	if err != nil {
		s.log.Warn("cannot plan the daily checks", "err", err)
		return
	}
	for _, d := range due {
		if !s.claimCheck(d.fqdn) {
			continue
		}
		s.checks.Add(1)
		go func() {
			defer s.checks.Done()
			defer s.releaseCheck(d.fqdn)
			select {
			case s.checkSlots <- struct{}{}:
			default:
				t := time.NewTimer(d.until.Sub(s.now()))
				defer t.Stop()
				select {
				case s.checkSlots <- struct{}{}:
				case <-t.C:
					s.skipCheck(ctx, d.fqdn, "no check slot became free before the host's quiet stretch ended")
					return
				case <-ctx.Done():
					return
				}
			}
			defer func() { <-s.checkSlots }()
			s.checkHost(ctx, systemActor, d.fqdn)
		}()
	}
}

func (s *Server) skipCheck(ctx context.Context, fqdn, why string) {
	now := s.now()
	res := CheckResult{FQDN: fqdn, Subset: s.subset(now), Status: "skipped", Error: why}
	id, err := s.st.Q().StartMaintenance(ctx, fqdn, "check", res.Subset, s.set.NodeName, now)
	if err != nil {
		s.log.Error("recording a skipped check", "host", fqdn, "err", err)
		return
	}
	s.finishCheck(ctx, systemActor, id, res)
}

// recoverInterruptedChecks runs when the server starts. A check that was running when the server
// stopped left its exclusive lock behind, and the lock names this server's host, so an agent cannot tell
// it is stale for 30 minutes: until then that host's backups fail. restic unlock, run here, sees that the
// process is gone and removes it. It never removes a live lock (no --remove-all).
func (s *Server) recoverInterruptedChecks(ctx context.Context) {
	q := s.st.Q()
	runs, err := q.UnfinishedMaintenance(ctx, s.set.NodeName, s.started)
	if err != nil {
		s.log.Error("looking for interrupted checks", "err", err)
		return
	}
	for _, m := range runs {
		res := CheckResult{FQDN: m.FQDN, Subset: m.Subset, Status: "interrupted", Error: "the server stopped during the check"}
		if r, cleanup, err := s.maintRunner(ctx, m.FQDN); err != nil {
			res.Error += "; preparing restic unlock: " + err.Error()
		} else {
			report, err := r.Unlock(ctx)
			cleanup()
			if err != nil {
				res.Error += "; restic unlock: " + err.Error()
			} else if report != "" {
				s.log.Info("removed a stale lock left by an interrupted check", "host", m.FQDN, "restic", report)
			}
		}
		s.finishCheck(ctx, systemActor, m.ID, res)
	}
}

func (s *Server) maintenanceLoop(ctx context.Context) {
	s.recoverInterruptedChecks(ctx)
	if s.set.Maintenance.Check != CheckDaily {
		return
	}
	t := time.NewTicker(checkTick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.startDueChecks(ctx)
		}
	}
}
