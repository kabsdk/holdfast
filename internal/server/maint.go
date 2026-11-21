package server

import (
	"context"
	"fmt"
	"time"

	"github.com/kabsdk/holdfast/internal/server/store"
)

// Maintenance in M1 is `restic check` only (plan-m1 Q1). There is no forget and no prune: nothing
// destructive exists before the 7-day protection (M3). The leader gate is a stub: a single server is
// always the leader until failover lands (M6).

func (s *Server) isLeader() bool { return true }

// CheckResult is the outcome of one host's check.
type CheckResult struct {
	FQDN   string `json:"fqdn"`
	Subset string `json:"subset"`
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}

// RunChecks checks every enrolled host's repository, reading a rotating subset of the data.
func (s *Server) RunChecks(ctx context.Context, a Actor) []CheckResult {
	out := []CheckResult{}
	if !s.isLeader() {
		return out
	}
	hosts, err := s.st.Q().ListHosts(ctx)
	if err != nil {
		return append(out, CheckResult{Status: "failed", Error: err.Error()})
	}
	now := s.now()
	cycle := s.set.Maintenance.CheckCycleDays
	subset := fmt.Sprintf("%d/%d", now.YearDay()%cycle+1, cycle)
	for _, h := range hosts {
		if h.State != "enrolled" {
			continue
		}
		res := CheckResult{FQDN: h.FQDN, Subset: subset, Status: "success"}
		q := s.st.Q()
		id, err := q.StartMaintenance(ctx, h.FQDN, "check", subset, s.set.NodeName, now)
		if err != nil {
			res.Status, res.Error = "failed", err.Error()
			out = append(out, res)
			continue
		}
		if err := s.checkOne(ctx, h.FQDN, subset); err != nil {
			res.Status, res.Error = "failed", err.Error()
		}
		_ = q.FinishMaintenance(ctx, id, res.Status, res.Error, s.now())
		_ = q.Audit(ctx, store.AuditEntry{Actor: a.Name, Source: a.Source, Action: "maintenance.check", FQDN: h.FQDN,
			Detail: map[string]any{"subset": subset, "status": res.Status, "error": res.Error}})
		out = append(out, res)
	}
	return out
}

func (s *Server) checkOne(ctx context.Context, fqdn, subset string) error {
	r, cleanup, err := s.maintRunner(ctx, fqdn)
	if err != nil {
		return err
	}
	defer cleanup()
	r.RetryLock = 10 * time.Minute // a backup in progress holds a lock; wait for it rather than fail
	return r.Check(ctx, subset)
}

func (s *Server) maintenanceLoop(ctx context.Context) {
	every := s.set.Maintenance.CheckEvery
	if every <= 0 {
		return
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.RunChecks(ctx, Actor{Name: "holdfast-server", Source: "system"})
		}
	}
}
