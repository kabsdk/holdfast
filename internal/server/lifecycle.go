package server

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/kabsdk/holdfast/internal/server/store"
)

// Removing a job or a host (spec §3.5). Neither deletes any data: the snapshots stay, and a
// removed host's repository can still be browsed. Decommission, which deletes a repository, is not built.

// RemoveInput carries the revision message and, optionally, the base revision.
type RemoveInput struct {
	Base    *int64 `json:"base_revision,omitempty"`
	Message string `json:"message"`
}

// RemoveJob saves a revision without the job. Its snapshots stay, and the daily run no longer touches them.
func (s *Server) RemoveJob(ctx context.Context, a Actor, fqdn, jobID string, in RemoveInput) (int64, error) {
	if strings.TrimSpace(in.Message) == "" {
		in.Message = fmt.Sprintf("Remove job %s from %s", jobID, fqdn)
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
		j, ok := head.Config.Hosts[fqdn].Jobs[jobID]
		if !ok {
			return fmt.Errorf("job %s on %s: %w", jobID, fqdn, ErrNotFound)
		}
		// In a vault, a job the source still runs comes back on the next sync: Exclude stops copying it.
		if s.set.IsVault() && !j.Retired {
			return fmt.Errorf("job %s on %s is still in the source; exclude it to stop copying it (holdfast offsite exclude): %w", jobID, fqdn, ErrConflict)
		}
		doc := head.Config.Clone()
		delete(doc.Hosts[fqdn].Jobs, jobID)
		rev, err := q.SaveRevision(ctx, store.SaveRevision{Base: head.Number, Actor: a.Name, Source: a.Source, Message: in.Message, Config: doc, HostSHAs: s.hostSHAs(doc)})
		if err != nil {
			return err
		}
		revNum = rev.Number
		if err := q.Audit(ctx, store.AuditEntry{Actor: a.Name, Source: a.Source, Action: "job.remove", FQDN: fqdn, JobID: jobID, Revision: rev.Number,
			Detail: map[string]any{"message": in.Message}}); err != nil {
			return err
		}
		return q.NotifyHost(ctx, fqdn)
	})
	return revNum, err
}

// RemoveHost saves a revision without the host. Its agent gets an empty config on its next poll, so it
// stops backing up, and the host is retired once the agent has applied it: from then on the agent's
// certificate no longer works. A host that never enrolled is retired at once. The S3 key is the admin's to delete at the
// S3 server; Holdfast never deletes keys (decision 029).
func (s *Server) RemoveHost(ctx context.Context, a Actor, fqdn string, in RemoveInput) (int64, error) {
	if strings.TrimSpace(in.Message) == "" {
		in.Message = "Remove host " + fqdn
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
		hc, ok := head.Config.Hosts[fqdn]
		if !ok {
			return fmt.Errorf("host %s: %w", fqdn, ErrNotFound)
		}
		if s.set.IsVault() {
			for id, j := range hc.Jobs {
				if !j.Retired {
					return fmt.Errorf("host %s is still on the server (job %s); off-site, only a host the server removed is removed: %w", fqdn, id, ErrConflict)
				}
			}
		}
		doc := head.Config.Clone()
		delete(doc.Hosts, fqdn)
		rev, err := q.SaveRevision(ctx, store.SaveRevision{Base: head.Number, Actor: a.Name, Source: a.Source, Message: in.Message, Config: doc, HostSHAs: s.hostSHAs(doc)})
		if err != nil {
			return err
		}
		revNum = rev.Number
		if err := q.MarkRemoved(ctx, fqdn, s.now()); err != nil {
			return err
		}
		if s.set.IsVault() { // no agent to take the empty config: retired at once, so it can be decommissioned
			if err := q.RetireHost(ctx, fqdn); err != nil {
				return err
			}
		}
		if err := q.Audit(ctx, store.AuditEntry{Actor: a.Name, Source: a.Source, Action: "host.remove", FQDN: fqdn, Revision: rev.Number,
			Detail: map[string]any{"message": in.Message}}); err != nil {
			return err
		}
		return q.NotifyHost(ctx, fqdn)
	})
	return revNum, err
}

// retireIfDone retires a removed host. It is called when the agent already runs the config it should,
// which for a removed host is the empty one.
func (s *Server) retireIfDone(ctx context.Context, fqdn string) {
	h, err := s.st.Q().GetHost(ctx, fqdn)
	if err != nil || h.RemovedAt == nil || h.State == "retired" {
		return
	}
	if err := s.st.InTx(ctx, func(q *store.Q) error {
		if err := q.RetireHost(ctx, fqdn); err != nil {
			return err
		}
		return q.Audit(ctx, store.AuditEntry{Actor: systemActor.Name, Source: systemActor.Source, Action: "host.retired", FQDN: fqdn,
			Detail: map[string]any{"reason": "its agent applied the empty config"}})
	}); err != nil {
		s.log.Error("retiring a removed host", "host", fqdn, "err", err)
	}
}

func (s *Server) apiRemoveJob(w http.ResponseWriter, r *http.Request) {
	var in RemoveInput
	if r.ContentLength != 0 && !decode(w, r, &in) {
		return
	}
	rev, err := s.RemoveJob(r.Context(), actorOf(r), r.PathValue("fqdn"), r.PathValue("job"), in)
	if err != nil {
		apiError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"revision": rev})
}

func (s *Server) apiRemoveHost(w http.ResponseWriter, r *http.Request) {
	var in RemoveInput
	if r.ContentLength != 0 && !decode(w, r, &in) {
		return
	}
	rev, err := s.RemoveHost(r.Context(), actorOf(r), r.PathValue("fqdn"), in)
	if err != nil {
		apiError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"revision": rev})
}
