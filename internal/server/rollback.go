package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/kabsdk/holdfast/internal/config"
	"github.com/kabsdk/holdfast/internal/proto"
	"github.com/kabsdk/holdfast/internal/server/storage"
	"github.com/kabsdk/holdfast/internal/server/store"
)

// Rolling a repository back (decision 038, plan-m6 §2.4). The preview lists the host's prefix with its versions
// and works out what rolling it back to a time changes. Doing it copies each object's version of then over today's,
// and puts a delete marker in front of each object that was not there then. No version is deleted, so a rollback
// can be rolled back too. The host's agent is paused and idle meanwhile; then restic check runs, the snapshot list is
// read again, and the agent resumes.

const pauseFor = 2 * time.Hour // a pause the agent ends by itself if no resume comes (plan-m6 Q3)

// pauseWait is how long a rollback waits for the agent to confirm its pause. Tests shorten it.
var pauseWait = 5 * time.Minute

// RollbackPreview is what a rollback to To would change, or why it is refused.
type RollbackPreview struct {
	FQDN    string    `json:"fqdn"`
	To      time.Time `json:"to"`
	Refused string    `json:"refused,omitempty"` // why it cannot be done; nothing else then counts
	// Oldest is the earliest time that can be chosen: the lifecycle rule keeps old versions this long. A rotation
	// after it moves it later (plan-m6 Q2).
	Oldest       time.Time      `json:"oldest"`
	LastRotation *time.Time     `json:"last_rotation,omitempty"`
	BringBack    map[string]int `json:"bring_back"` // by kind: data, index, snapshots, keys, config
	SetAside     map[string]int `json:"set_aside"`
	Unchanged    int            `json:"unchanged"`
	// The snapshots that come back, and those that are set aside. ForgottenBefore are the ones that come back
	// although Holdfast had forgotten them: the daily run may forget them again (spec §3.4).
	SnapshotsBack     []string `json:"snapshots_back"`
	SnapshotsSetAside []string `json:"snapshots_set_aside"`
	ForgottenBefore   []string `json:"forgotten_before"`
	// SnapshotDeletions are when snapshot files were deleted, to help choose the time.
	SnapshotDeletions []time.Time `json:"snapshot_deletions"`
	Confirm           string      `json:"confirm"` // what to type: the FQDN
	plan              storage.RollbackPlan
}

// RollbackPreviewFor works out a rollback of fqdn's repository to to, without changing anything.
func (s *Server) RollbackPreviewFor(ctx context.Context, fqdn string, to time.Time) (RollbackPreview, error) {
	h, err := s.st.Q().GetHost(ctx, fqdn)
	if err != nil {
		return RollbackPreview{}, fmt.Errorf("host %s: %w", fqdn, err)
	}
	now := s.now()
	p := RollbackPreview{FQDN: fqdn, To: to.UTC(), Confirm: fqdn, BringBack: map[string]int{}, SetAside: map[string]int{},
		SnapshotsBack: []string{}, SnapshotsSetAside: []string{}, ForgottenBefore: []string{}, SnapshotDeletions: []time.Time{}}
	days := 0
	if h.Storage != nil {
		days = h.Storage.NoncurrentDays
	}
	if days > 0 {
		p.Oldest = now.AddDate(0, 0, -days).UTC()
	}
	if last, err := s.st.Q().ListAuditFiltered(ctx, store.AuditFilter{FQDN: fqdn, Action: "host.rotated", Limit: 1}); err == nil && len(last) > 0 {
		at := last[0].At
		p.LastRotation = &at
		if at.After(p.Oldest) {
			p.Oldest = at.UTC()
		}
	}
	switch {
	case decommissioned(h):
		p.Refused = "the host was decommissioned: its repository is deleted"
	case !to.Before(now):
		p.Refused = "the time is not in the past"
	case days == 0:
		p.Refused = "the bucket's lifecycle rule is not known (B011), so it is not known which old versions are still there"
	case p.LastRotation != nil && to.Before(*p.LastRotation):
		p.Refused = fmt.Sprintf("the repository password was rotated at %s, after that time, and the server no longer has the password from before; "+
			"roll back by hand with that time's escrow file and plain restic (docs/runbooks/bare-metal.md), or choose a later time",
			p.LastRotation.UTC().Format(time.RFC3339))
	case to.Before(p.Oldest):
		p.Refused = fmt.Sprintf("the bucket's lifecycle rule keeps old versions for %d days, so the earliest time is %s", days, p.Oldest.Format(time.RFC3339))
	}
	if p.Refused != "" {
		return p, nil
	}
	versions, err := s.bucket.Versions(ctx, fqdn)
	if err != nil {
		return RollbackPreview{}, err
	}
	p.plan = storage.PlanRollback(storage.Prefix(fqdn)+"/", versions, to)
	for _, st := range p.plan.BringBack {
		p.BringBack[st.Kind]++
	}
	for _, st := range p.plan.SetAside {
		p.SetAside[st.Kind]++
	}
	p.Unchanged = p.plan.Unchanged
	p.SnapshotsBack, p.SnapshotsSetAside = storage.SnapshotIDs(p.plan.BringBack), storage.SnapshotIDs(p.plan.SetAside)
	p.SnapshotDeletions = p.plan.Deletions
	if len(p.SnapshotsBack) > 0 {
		snaps, err := s.st.Q().ListSnapshots(ctx, fqdn, store.SnapshotFilter{WithGone: true})
		if err == nil {
			for _, sn := range snaps {
				if sn.DeletedByUs && slices.Contains(p.SnapshotsBack, sn.ID) {
					p.ForgottenBefore = append(p.ForgottenBefore, sn.ID)
				}
			}
		}
	}
	// Copying a version needs s3:GetObjectVersion: tested here, not by the daily storage test (plan-m6 Q4).
	if len(p.plan.BringBack) > 0 {
		st := p.plan.BringBack[0]
		if err := s.bucket.CanReadVersion(ctx, st.Key, st.VersionID); err != nil {
			p.Refused = err.Error()
		}
	}
	return p, nil
}

// RollbackInput confirms a rollback.
type RollbackInput struct {
	To      time.Time `json:"to"`
	Confirm string    `json:"confirm"` // the FQDN, typed
	// AgentDown says the host is down, so its agent cannot be paused: only for an agent that is offline.
	AgentDown bool   `json:"agent_down,omitempty"`
	Message   string `json:"message,omitempty"`
}

// Rollback starts a rollback of fqdn's repository, in the background, and returns its record.
func (s *Server) Rollback(ctx context.Context, a Actor, fqdn string, in RollbackInput) (store.Rollback, error) {
	if strings.TrimSpace(in.Confirm) != fqdn {
		return store.Rollback{}, &ValidationError{config.Problems{{Path: "confirm", Message: "type the host's FQDN, " + fqdn + ", to confirm"}}}
	}
	p, err := s.RollbackPreviewFor(ctx, fqdn, in.To)
	if err != nil {
		return store.Rollback{}, err
	}
	if p.Refused != "" {
		return store.Rollback{}, &ValidationError{config.Problems{{Path: "to", Message: p.Refused}}}
	}
	if len(p.plan.BringBack)+len(p.plan.SetAside) == 0 {
		return store.Rollback{}, &ValidationError{config.Problems{{Path: "to", Message: "the repository is as it was then: there is nothing to roll back"}}}
	}
	q := s.st.Q()
	if running, err := q.RunningRollbacks(ctx); err == nil && running[fqdn] {
		return store.Rollback{}, fmt.Errorf("a rollback of %s is under way already: %w", fqdn, ErrConflict)
	}
	h, err := q.GetHost(ctx, fqdn)
	if err != nil {
		return store.Rollback{}, err
	}
	// The agent backs up on its own, so it is paused first, unless the host is removed or down (plan-m6 Q3).
	pause := h.State == "enrolled"
	if pause {
		ag, err := q.GetAgent(ctx, fqdn)
		online := err == nil && ag.LastSeenAt != nil && s.now().Sub(*ag.LastSeenAt) < OfflineAfter
		switch {
		case in.AgentDown && online:
			return store.Rollback{}, &ValidationError{config.Problems{{Path: "agent_down", Message: "the agent is online, so it is paused instead"}}}
		case in.AgentDown:
			pause = false
		case !online:
			return store.Rollback{}, &ValidationError{config.Problems{{Path: "agent_down", Message: "the agent is offline, so it cannot be paused: " +
				"if the host is down, say so (agent_down), and make sure it stays down until the rollback is done"}}}
		case (proto.Facts{Protocol: ag.Protocol}).AgentProtocol() < proto.ProtocolRecovery:
			return store.Rollback{}, fmt.Errorf("%s's agent cannot be paused: upgrade holdfast-agent first: %w", fqdn, ErrConflict)
		}
	}
	r := store.Rollback{FQDN: fqdn, Actor: a.Name, To: p.To, RequestedAt: s.now(), Status: "running", AgentDown: in.AgentDown, Message: in.Message}
	err = s.st.InTx(ctx, func(q *store.Q) error {
		id, err := q.StartRollback(ctx, r)
		if err != nil {
			return err
		}
		r.ID = id
		return q.Audit(ctx, store.AuditEntry{Actor: a.Name, Source: a.Source, Action: "repository.rollback_started", FQDN: fqdn,
			Detail: map[string]any{"rollback": id, "to": p.To, "bring_back": len(p.plan.BringBack), "set_aside": len(p.plan.SetAside),
				"snapshots_back": p.SnapshotsBack, "snapshots_set_aside": p.SnapshotsSetAside, "agent_down": in.AgentDown, "message": in.Message}})
	})
	if err != nil {
		return store.Rollback{}, err
	}
	go s.runRollback(r, pause)
	return r, nil
}

// runRollback does it: pause, roll back, check, read the snapshot list again, resume.
func (s *Server) runRollback(r store.Rollback, pause bool) {
	ctx := s.background()
	q := s.st.Q()
	finish := func(err error) {
		now := s.now()
		r.FinishedAt, r.Status = &now, "success"
		action := "repository.rolled_back"
		if err != nil {
			r.Status, r.Error, action = "failed", err.Error(), "repository.rollback_failed"
			s.log.Error("rollback failed", "host", r.FQDN, "err", err)
		}
		if err := q.FinishRollback(ctx, r); err != nil {
			s.log.Error("recording a rollback", "host", r.FQDN, "err", err)
		}
		_ = q.Audit(ctx, store.AuditEntry{Actor: systemActor.Name, Source: systemActor.Source, Action: action, FQDN: r.FQDN,
			Detail: map[string]any{"rollback": r.ID, "to": r.To, "brought_back": r.BroughtBack, "set_aside": r.SetAside,
				"snapshots_back": r.SnapshotsBack, "snapshots_set_aside": r.SnapshotsSetAside, "check_passed": r.CheckPassed, "error": r.Error}})
	}
	if !s.maintBusy.acquire(r.FQDN) {
		finish(errors.New("a forget or prune is running on the host; nothing was changed: roll back again when it is done"))
		return
	}
	defer s.maintBusy.release(r.FQDN)
	lock := s.repoLock.get(r.FQDN) // in a vault, a copy that runs finishes first
	lock.Lock()
	defer lock.Unlock()

	if pause {
		id, err := s.pauseAgent(ctx, r.FQDN, r.Actor)
		if err != nil {
			finish(err)
			return
		}
		defer s.resumeAgent(ctx, r.FQDN, r.Actor, id)
	}

	// The prefix is listed again: it may have changed since the preview.
	versions, err := s.bucket.Versions(ctx, r.FQDN)
	if err != nil {
		finish(err)
		return
	}
	plan := storage.PlanRollback(storage.Prefix(r.FQDN)+"/", versions, r.To)
	r.SnapshotsBack, r.SnapshotsSetAside = storage.SnapshotIDs(plan.BringBack), storage.SnapshotIDs(plan.SetAside)
	// Snapshots set aside get tombstones marked deleted by Holdfast, not "disappeared" (decision 032).
	if err := q.MarkDeleting(ctx, r.FQDN, r.SnapshotsSetAside); err != nil {
		finish(err)
		return
	}
	for _, st := range plan.BringBack {
		if err := s.bucket.CopyVersion(ctx, st.Key, st.VersionID); err != nil {
			finish(fmt.Errorf("%w; %d of %d objects were brought back and none set aside: look at the preview again, and roll back again to finish",
				err, r.BroughtBack, len(plan.BringBack)))
			return
		}
		r.BroughtBack++
	}
	for _, st := range plan.SetAside {
		if err := s.bucket.SetAside(ctx, st.Key); err != nil {
			finish(fmt.Errorf("%w; every object was brought back, and %d of %d set aside: roll back again to finish", err, r.SetAside, len(plan.SetAside)))
			return
		}
		r.SetAside++
	}
	if err := q.MarkDeleted(ctx, r.FQDN, r.SnapshotsSetAside, s.now()); err != nil {
		s.log.Error("recording the snapshots a rollback set aside", "host", r.FQDN, "err", err)
	}

	// restic check: the repository as it was then is what a crash then would have left.
	runner, cleanup, err := s.maintRunner(ctx, r.FQDN)
	if err != nil {
		finish(err)
		return
	}
	cerr := runner.Check(ctx, "")
	cleanup()
	passed := cerr == nil
	r.CheckPassed = &passed

	// The snapshot list again: what came back is no longer disappeared, nor forgotten.
	if err := s.RefreshCatalogue(ctx, r.FQDN); err != nil {
		s.log.Warn("reading the snapshot list after a rollback", "host", r.FQDN, "err", err)
	}
	if err := q.UnmarkDeleting(ctx, r.FQDN, r.SnapshotsBack); err != nil {
		s.log.Warn("clearing the tombstones of snapshots a rollback brought back", "host", r.FQDN, "err", err)
	}
	if cerr != nil {
		finish(fmt.Errorf("the rollback is done, but restic check failed afterwards: %v", cerr))
		return
	}
	finish(nil)
}

// pauseAgent asks the host's agent to pause, and waits until it reports that it is paused and idle.
func (s *Server) pauseAgent(ctx context.Context, fqdn, actor string) (string, error) {
	id := newID()
	until := s.now().Add(pauseFor)
	payload, _ := json.Marshal(until)
	err := s.st.InTx(ctx, func(q *store.Q) error {
		if err := q.EnqueueCommand(ctx, store.Command{ID: id, FQDN: fqdn, Kind: proto.CommandPause, CreatedAt: s.now(), CreatedBy: actor, Payload: payload}); err != nil {
			return err
		}
		return q.NotifyHost(ctx, fqdn)
	})
	if err != nil {
		return "", err
	}
	deadline := time.Now().Add(pauseWait)
	for time.Now().Before(deadline) {
		if ag, err := s.st.Q().GetAgent(ctx, fqdn); err == nil && ag.Paused != nil && ag.Paused.Command == id {
			return id, nil
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	s.resumeAgent(ctx, fqdn, actor, id)
	return "", fmt.Errorf("the agent did not confirm within %s that it is paused and idle; nothing was changed", pauseWait)
}

// resumeAgent ends the pause.
func (s *Server) resumeAgent(ctx context.Context, fqdn, actor, pauseID string) {
	err := s.st.InTx(ctx, func(q *store.Q) error {
		if err := q.EnqueueCommand(ctx, store.Command{ID: newID(), FQDN: fqdn, Kind: proto.CommandResume, CreatedAt: s.now(), CreatedBy: actor,
			Payload: json.RawMessage(fmt.Sprintf("%q", pauseID))}); err != nil {
			return err
		}
		return q.NotifyHost(ctx, fqdn)
	})
	if err != nil {
		s.log.Error("resuming the agent after a rollback; it resumes by itself when the pause ends", "host", fqdn, "err", err)
	}
}

/* ---------------- API ---------------- */

func (s *Server) apiRollbackPreview(w http.ResponseWriter, r *http.Request) {
	to, err := time.Parse(time.RFC3339, r.URL.Query().Get("to"))
	if err != nil {
		apiError(w, &ValidationError{config.Problems{{Path: "to", Message: "give the time as RFC 3339, such as 2026-11-20T03:00:00Z"}}})
		return
	}
	p, err := s.RollbackPreviewFor(r.Context(), r.PathValue("fqdn"), to)
	if err != nil {
		apiError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (s *Server) apiRollback(w http.ResponseWriter, r *http.Request) {
	var in RollbackInput
	if !decode(w, r, &in) {
		return
	}
	rb, err := s.Rollback(r.Context(), actorOf(r), r.PathValue("fqdn"), in)
	if err != nil {
		apiError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, rb)
}

func (s *Server) apiRollbacks(w http.ResponseWriter, r *http.Request) {
	list, err := s.st.Q().ListRollbacks(r.Context(), r.PathValue("fqdn"), 50)
	if err != nil {
		apiError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}
