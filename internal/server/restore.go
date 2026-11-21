package server

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/kabsdk/holdfast/internal/config"
	"github.com/kabsdk/holdfast/internal/proto"
	"github.com/kabsdk/holdfast/internal/server/store"
)

// Restore runs on the agent, as a command in its one-restic-at-a-time loop (plan-m2 §3). The server checks
// the request, and the host's bounds as its agent last reported them, but the agent decides: the bounds
// are in its own config, which only root on the host can change (decision 030).

// RestoreExpiry is how long a queued restore waits for its host (plan-m2 Q6).
const RestoreExpiry = 24 * time.Hour

// RestoreInput is a restore request (spec §5.4). Everything is selected unless left out (Exclude), or
// only some paths are asked for (Include, "restore only this").
type RestoreInput struct {
	Target    string   `json:"target"`              // new (the default), in_place or directory
	Directory string   `json:"directory,omitempty"` // for target directory
	Exclude   []string `json:"exclude,omitempty"`
	Include   []string `json:"include,omitempty"`
	DryRun    bool     `json:"dry_run,omitempty"`
}

// RestoreResult is a queued restore.
type RestoreResult struct {
	Command   string    `json:"command"`
	Snapshot  string    `json:"snapshot"`
	Target    string    `json:"target"`
	ExpiresAt time.Time `json:"expires_at"`
}

var unsafeName = regexp.MustCompile(`[^a-z0-9._-]+`)

// restoreUser is the actor's name as it goes into a directory name: local:root becomes root.
func restoreUser(a Actor) string {
	n := strings.ToLower(a.Name)
	if _, after, ok := strings.Cut(n, ":"); ok {
		n = after
	}
	if n = strings.Trim(unsafeName.ReplaceAllString(n, "-"), "-"); n == "" {
		n = "unknown"
	}
	return n
}

func absPaths(field string, paths []string) error {
	if len(paths) > 1000 {
		return &ValidationError{config.Problems{{Path: field, Message: "at most 1000 paths"}}}
	}
	for _, p := range paths {
		if !strings.HasPrefix(p, "/") || strings.ContainsRune(p, 0) {
			return &ValidationError{config.Problems{{Path: field, Message: fmt.Sprintf("%q is not an absolute path", p)}}}
		}
	}
	return nil
}

// Restore queues a restore of a snapshot on its host, and audits the request.
func (s *Server) Restore(ctx context.Context, a Actor, fqdn, snapshotID string, in RestoreInput) (RestoreResult, error) {
	h, err := s.st.Q().GetHost(ctx, fqdn)
	if err != nil {
		return RestoreResult{}, fmt.Errorf("host %s: %w", fqdn, err)
	}
	if h.State != "enrolled" {
		return RestoreResult{}, fmt.Errorf("host %s is %s, not enrolled: %w", fqdn, h.State, ErrConflict)
	}
	sn, err := s.snapshot(ctx, fqdn, snapshotID)
	if err != nil {
		return RestoreResult{}, err
	}
	if in.Target == "" {
		in.Target = proto.TargetNew
	}
	switch in.Target {
	case proto.TargetNew, proto.TargetInPlace:
		if in.Directory != "" {
			return RestoreResult{}, &ValidationError{config.Problems{{Path: "directory", Message: "a directory goes only with the target directory"}}}
		}
	case proto.TargetDirectory:
		if err := absPaths("directory", []string{in.Directory}); err != nil || path.Clean(in.Directory) == "/" {
			return RestoreResult{}, &ValidationError{config.Problems{{Path: "directory", Message: "give an absolute directory other than /"}}}
		}
		in.Directory = path.Clean(in.Directory)
	default:
		return RestoreResult{}, &ValidationError{config.Problems{{Path: "target", Message: "the target is new, in_place or directory"}}}
	}
	if len(in.Include) > 0 && len(in.Exclude) > 0 {
		return RestoreResult{}, &ValidationError{config.Problems{{Path: "include", Message: "give what to leave out, or only what to restore, not both"}}}
	}
	if err := absPaths("exclude", in.Exclude); err != nil {
		return RestoreResult{}, err
	}
	if err := absPaths("include", in.Include); err != nil {
		return RestoreResult{}, err
	}
	if ag, err := s.st.Q().GetAgent(ctx, fqdn); err == nil && ag.RestoreBounds != nil {
		switch b := ag.RestoreBounds; {
		case in.Target == proto.TargetInPlace && !b.AllowInPlace:
			return RestoreResult{}, &ValidationError{config.Problems{{Path: "target", Message: fqdn + " does not allow in-place restores; root on the host can allow them in agent.yaml (restore.allow_in_place)"}}}
		case in.Target == proto.TargetDirectory && !b.AllowOtherDirectories:
			return RestoreResult{}, &ValidationError{config.Problems{{Path: "target", Message: fqdn + " allows restores only into " + b.Root + "; root on the host can allow other directories in agent.yaml (restore.allow_other_directories)"}}}
		}
	}
	spec := proto.RestoreSpec{Snapshot: sn.ID, Target: in.Target, Directory: in.Directory, Exclude: in.Exclude, Include: in.Include,
		DryRun: in.DryRun, JobID: sn.JobID, User: restoreUser(a)}
	payload, err := json.Marshal(spec)
	if err != nil {
		return RestoreResult{}, err
	}
	now := s.now()
	res := RestoreResult{Command: newID(), Snapshot: sn.ID, Target: in.Target, ExpiresAt: now.Add(RestoreExpiry)}
	err = s.st.InTx(ctx, func(q *store.Q) error {
		if err := q.EnqueueCommand(ctx, store.Command{ID: res.Command, FQDN: fqdn, Kind: proto.CommandRestore, JobID: sn.JobID, CreatedAt: now,
			CreatedBy: a.Name, Payload: payload, ExpiresAt: &res.ExpiresAt}); err != nil {
			return err
		}
		if err := q.Audit(ctx, store.AuditEntry{Actor: a.Name, Source: a.Source, Action: "restore.requested", FQDN: fqdn, JobID: sn.JobID,
			Detail: map[string]any{"command": res.Command, "snapshot": sn.ID, "target": in.Target, "directory": in.Directory, "dry_run": in.DryRun,
				"left_out": len(in.Exclude), "only": len(in.Include)}}); err != nil {
			return err
		}
		return q.NotifyHost(ctx, fqdn)
	})
	return res, err
}

// RestoresView is the fleet's restore history (spec §5.5): queued ones first, then finished ones.
type RestoresView struct {
	Queued []store.Command `json:"queued"`
	Done   []store.Event   `json:"done"`
}

// Restores returns the restore history of every host.
func (s *Server) Restores(ctx context.Context, limit int) (RestoresView, error) {
	q := s.st.Q()
	queued, err := q.PendingRestores(ctx)
	if err != nil {
		return RestoresView{}, err
	}
	done, err := q.ListRestores(ctx, limit)
	if err != nil {
		return RestoresView{}, err
	}
	if queued == nil {
		queued = []store.Command{}
	}
	if done == nil {
		done = []store.Event{}
	}
	return RestoresView{Queued: queued, Done: done}, nil
}

// expireCommands cancels queued restores whose host did not pick them up in time. Each is reported in the
// host's history as a failed restore, and audited (plan-m2 Q6).
func (s *Server) expireCommands(ctx context.Context) {
	now := s.now()
	expired, err := s.st.Q().ExpireCommands(ctx, now)
	if err != nil {
		s.log.Warn("expiring commands", "err", err)
		return
	}
	for _, c := range expired {
		var spec proto.RestoreSpec
		_ = json.Unmarshal(c.Payload, &spec)
		err := s.st.InTx(ctx, func(q *store.Q) error {
			// A restore's snapshot goes in the detail: SnapshotID means "this event made it" (decision 032).
			if _, err := q.InsertEvent(ctx, store.Event{FQDN: c.FQDN, ID: "expired-" + c.ID, Kind: proto.KindRestore, JobID: c.JobID, CommandID: c.ID,
				Status: proto.StatusFailed, Started: c.CreatedAt, Finished: now,
				Error:  fmt.Sprintf("cancelled: the host did not pick the restore up within %s", RestoreExpiry),
				Detail: map[string]string{"snapshot": spec.Snapshot, "target": spec.Target}}); err != nil {
				return err
			}
			return q.Audit(ctx, store.AuditEntry{Actor: systemActor.Name, Source: systemActor.Source, Action: "restore.expired", FQDN: c.FQDN, JobID: c.JobID,
				Detail: map[string]any{"command": c.ID, "requested_by": c.CreatedBy}})
		})
		if err != nil {
			s.log.Warn("recording an expired restore", "host", c.FQDN, "err", err)
		}
	}
}
