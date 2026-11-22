package server

import (
	"context"
	"time"

	"github.com/kabsdk/holdfast/internal/access"
	"github.com/kabsdk/holdfast/internal/proto"
	"github.com/kabsdk/holdfast/internal/server/storage"
	"github.com/kabsdk/holdfast/internal/server/store"
)

// Maintenance (decisions 029 and 030, plan-m3):
//   - Each agent checks its own repository once a day, right after a backup, in its one-restic-at-a-time
//     loop. restic check takes an exclusive lock, and there it can never meet the host's own backups.
//   - The server tests each host's storage contract once a day, with plain S3 calls. That takes no
//     restic lock.
//   - The server runs restic against a repository to initialise it, and once a day for retention: forget
//     behind the 7-day protection, then a check, then prune (retention.go).
//
// The leader gate is a stub: there is one server (decision 030).

func (s *Server) isLeader() bool { return true }

const (
	maintTick        = time.Minute      // how often the server looks for work
	storageCheckEach = 24 * time.Hour   // how often each host's storage contract is tested
	storageCheckSkew = 30 * time.Minute // tested a little early rather than a day late
)

var systemActor = Actor{Name: "holdfast-server", Source: "system"}

// CheckResult is what `holdfast check` did for one host. The storage contract is tested at once; the
// repository check is queued on the agent, and its result arrives as a "check" event.
type CheckResult struct {
	FQDN    string   `json:"fqdn"`
	Status  string   `json:"status"` // requested, or failed when the storage contract fails
	Command string   `json:"command,omitempty"`
	Storage []string `json:"storage_codes,omitempty"` // the storage checks that failed (decision 029)
	Error   string   `json:"error,omitempty"`
}

// RunChecks is `holdfast check`. For every enrolled host it tests the storage contract now, and asks the
// agent to check its repository at once.
func (s *Server) RunChecks(ctx context.Context, a Actor) []CheckResult {
	out := []CheckResult{}
	if !s.isLeader() {
		return out
	}
	hosts, err := s.st.Q().ListHosts(ctx)
	if err != nil {
		return append(out, CheckResult{Status: "failed", Error: err.Error()})
	}
	v, err := s.viewerOf(ctx, a)
	if err != nil {
		return append(out, CheckResult{Status: "failed", Error: err.Error()})
	}
	for _, h := range hosts {
		// Only the hosts where the actor may run maintenance; the others are not mentioned.
		if h.State != "enrolled" || !v.can(access.Maintenance, h.FQDN) {
			continue
		}
		res := CheckResult{FQDN: h.FQDN, Status: "requested"}
		if rep, err := s.checkStorage(ctx, h.FQDN); err != nil {
			res.Status, res.Error = "failed", "storage contract test: "+err.Error()
		} else if !rep.OK() {
			res.Status, res.Storage, res.Error = "failed", rep.Codes(), "storage: "+rep.Summary()
		}
		// A vault checks its copies itself, after its next copy run (vault.go).
		if s.set.IsVault() {
			s.vault.mu.Lock()
			delete(s.vault.checked, h.FQDN)
			s.vault.mu.Unlock()
			_ = s.st.Q().Audit(ctx, store.AuditEntry{Actor: a.Name, Source: a.Source, Action: "maintenance.check_requested", FQDN: h.FQDN,
				Detail: map[string]any{"storage_codes": res.Storage}})
			out = append(out, res)
			continue
		}
		id := newID()
		err := s.st.InTx(ctx, func(q *store.Q) error {
			if err := q.EnqueueCommand(ctx, store.Command{ID: id, FQDN: h.FQDN, Kind: proto.CommandCheckNow, CreatedAt: s.now(), CreatedBy: a.Name}); err != nil {
				return err
			}
			if err := q.Audit(ctx, store.AuditEntry{Actor: a.Name, Source: a.Source, Action: "maintenance.check_requested", FQDN: h.FQDN,
				Detail: map[string]any{"command": id, "storage_codes": res.Storage}}); err != nil {
				return err
			}
			return q.NotifyHost(ctx, h.FQDN)
		})
		if err != nil {
			res.Status, res.Error = "failed", err.Error()
		} else {
			res.Command = id
		}
		out = append(out, res)
	}
	return out
}

// checkStorage tests a host's storage contract again: an S3 policy can change after the key was given.
func (s *Server) checkStorage(ctx context.Context, fqdn string) (storage.Report, error) {
	if s.set.IsVault() {
		return s.vaultStorageCheck(ctx, fqdn), nil
	}
	rep := s.probeKey(ctx, fqdn, s.gwKey)
	s.recordStorage(ctx, fqdn, rep)
	return rep, nil
}

// checkStorageDue tests the storage contract of each enrolled host whose last test is a day old.
func (s *Server) checkStorageDue(ctx context.Context) {
	if !s.isLeader() {
		return
	}
	hosts, err := s.st.Q().ListHosts(ctx)
	if err != nil {
		s.log.Warn("cannot list hosts for the daily storage test", "err", err)
		return
	}
	for _, h := range hosts {
		if h.State != "enrolled" || (h.Storage != nil && s.now().Sub(h.Storage.CheckedAt) < storageCheckEach-storageCheckSkew) {
			continue
		}
		if rep, err := s.checkStorage(ctx, h.FQDN); err != nil {
			s.log.Warn("the daily storage test failed to run", "host", h.FQDN, "err", err)
		} else if !rep.OK() {
			s.log.Warn("the host's storage does not meet the contract", "host", h.FQDN, "codes", rep.Codes())
		}
	}
}

func (s *Server) maintenanceLoop(ctx context.Context) {
	t := time.NewTicker(maintTick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.checkStorageDue(ctx)
			s.refreshCatalogueDue(ctx)
			s.expireCommands(ctx)
			s.retentionDue(ctx)
			s.escrowDue(ctx)
			s.measureDue(ctx)
			s.decommissionDue(ctx)
			_ = s.st.Q().PurgeSessions(ctx, s.now())
		}
	}
}
