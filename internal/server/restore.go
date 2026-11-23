package server

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/kabsdk/holdfast/internal/access"
	"github.com/kabsdk/holdfast/internal/config"
	"github.com/kabsdk/holdfast/internal/gateway"
	"github.com/kabsdk/holdfast/internal/proto"
	"github.com/kabsdk/holdfast/internal/restic"
	"github.com/kabsdk/holdfast/internal/server/store"
)

// Restore runs on the agent, as a command in its one-restic-at-a-time loop. The server checks
// the request, and the host's bounds as its agent last reported them, but the agent decides: the bounds
// are in its own config, which only root on the host can change (decision 030).

// RestoreExpiry is how long a queued restore waits for its host.
const RestoreExpiry = 24 * time.Hour

// RestoreInput is a restore request (spec §5.4). Everything is selected unless left out (Exclude), or
// only some paths are asked for (Include, "restore only this").
type RestoreInput struct {
	Target    string   `json:"target"`              // new (the default), in_place or directory
	Directory string   `json:"directory,omitempty"` // for target directory
	Exclude   []string `json:"exclude,omitempty"`
	Include   []string `json:"include,omitempty"`
	DryRun    bool     `json:"dry_run,omitempty"`
	// Overwrite is restic's --overwrite, for a file that exists already: always, if-changed, if-newer or never.
	// Empty: restic's default. A new directory is empty, so it applies to the other targets only.
	Overwrite string `json:"overwrite,omitempty"`
	// Message is why, for the audit log: the "Reason" in the UI.
	Message string `json:"message,omitempty"`
	// ToHost restores the snapshot onto another host. That host reads this one's repository through
	// the gateway, which lets it for this restore only, and only to read (decision 041).
	ToHost string `json:"to_host,omitempty"`
	// FromOffsite restores the off-site copy of the snapshot (decisions 039 and 041). The snapshot is the copy's ID or
	// the original's here. The host reads the copy through the gateway's off-site endpoint, read-only, for this restore.
	FromOffsite bool `json:"from_offsite,omitempty"`
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
	if in.Target == "" {
		in.Target = proto.TargetNew
	}
	// A new directory under the host's restore root is restore.new_dir, which the route checks. In place, or
	// another directory, writes outside it: restore.in_place, an admin's (spec §6.2).
	if in.ToHost == fqdn {
		in.ToHost = ""
	}
	// The host the restore runs on: this one, or another, which is restore.in_place on both.
	target := fqdn
	if in.ToHost != "" {
		target = in.ToHost
		if err := s.allowed(ctx, a, access.RestoreInPlace, fqdn); err != nil {
			return RestoreResult{}, err
		}
		if err := s.allowed(ctx, a, access.RestoreInPlace, target); err != nil {
			return RestoreResult{}, err
		}
		if in.Target == proto.TargetInPlace {
			return RestoreResult{}, &ValidationError{config.Problems{{Path: "target", Message: "another host's snapshot is restored into a new or another directory, never in place"}}}
		}
	}
	if s.set.IsVault() {
		return RestoreResult{}, errVaultRestore
	}
	if in.FromOffsite {
		if s.offsite == nil {
			return RestoreResult{}, fmt.Errorf("off-site copies are not set up: %w", ErrNotFound)
		}
		if in.ToHost != "" {
			return RestoreResult{}, &ValidationError{config.Problems{{Path: "to_host", Message: "an off-site copy is restored on its own host"}}}
		}
		if in.Target == proto.TargetInPlace {
			return RestoreResult{}, &ValidationError{config.Problems{{Path: "target", Message: "an off-site copy is restored into a new or another directory, never in place"}}}
		}
	}
	if in.Target != proto.TargetNew {
		if err := s.allowed(ctx, a, access.RestoreInPlace, target); err != nil {
			return RestoreResult{}, err
		}
	}
	h, err := s.st.Q().GetHost(ctx, target)
	if err != nil {
		return RestoreResult{}, fmt.Errorf("host %s: %w", target, err)
	}
	if h.State != "enrolled" {
		return RestoreResult{}, fmt.Errorf("host %s is %s, not enrolled: %w", target, h.State, ErrConflict)
	}
	var sn store.Snapshot
	var copied proto.VaultCopy
	if in.FromOffsite {
		if copied, err = s.offsiteCopy(ctx, fqdn, snapshotID); err != nil {
			return RestoreResult{}, err
		}
		sn = store.Snapshot{ID: copied.ID, JobID: copied.JobID}
	} else if sn, err = s.snapshot(ctx, fqdn, snapshotID); err != nil {
		return RestoreResult{}, err
	}
	// A dump is one file: into a new directory only, and loading it is the admin's job (decision 037).
	dump := copied.Dump || slices.ContainsFunc(sn.Tags, func(t string) bool { return strings.HasPrefix(t, "dump=") })
	if dump && in.Target != proto.TargetNew {
		return RestoreResult{}, &ValidationError{config.Problems{{Path: "target", Message: "a dump is restored into a new directory only; load it into the database on the host"}}}
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
	if in.Overwrite != "" {
		if !slices.Contains(restic.OverwriteModes, in.Overwrite) {
			return RestoreResult{}, &ValidationError{config.Problems{{Path: "overwrite", Message: "overwrite is one of restic's: " + strings.Join(restic.OverwriteModes, ", ")}}}
		}
		if in.Target == proto.TargetNew {
			return RestoreResult{}, &ValidationError{config.Problems{{Path: "overwrite", Message: "a new directory is empty, so there is nothing to overwrite"}}}
		}
	}
	ag, agErr := s.st.Q().GetAgent(ctx, target)
	if agErr == nil && ag.RestoreBounds != nil {
		switch b := ag.RestoreBounds; {
		case in.Target == proto.TargetInPlace && !b.AllowInPlace:
			return RestoreResult{}, &ValidationError{config.Problems{{Path: "target", Message: target + " does not allow in-place restores; root on the host can allow them in agent.yaml (restore.allow_in_place)"}}}
		case in.Target == proto.TargetDirectory && !b.AllowOtherDirectories:
			return RestoreResult{}, &ValidationError{config.Problems{{Path: "target", Message: target + " allows restores only into " + b.Root + "; root on the host can allow other directories in agent.yaml (restore.allow_other_directories)"}}}
		case in.ToHost != "" && !b.AllowOtherHosts:
			return RestoreResult{}, &ValidationError{config.Problems{{Path: "to_host", Message: target + " does not allow restores from other hosts; root on it can allow them in agent.yaml (restore.allow_other_hosts)"}}}
		}
	}
	spec := proto.RestoreSpec{Snapshot: sn.ID, Target: in.Target, Directory: in.Directory, Exclude: in.Exclude, Include: in.Include,
		DryRun: in.DryRun, JobID: sn.JobID, User: restoreUser(a), Overwrite: in.Overwrite, Dump: dump}
	var sealed []byte
	if in.ToHost != "" {
		if agErr != nil || (proto.Facts{Protocol: ag.Protocol}).AgentProtocol() < proto.ProtocolRecovery {
			return RestoreResult{}, fmt.Errorf("%s's agent does not know restores from other hosts: upgrade holdfast-agent there first: %w", target, ErrConflict)
		}
		if sealed, err = s.sealSource(ctx, fqdn, target); err != nil {
			return RestoreResult{}, err
		}
		spec.JobID = "" // the safety backup's job: none on another host, which is why it is never in place
	}
	now := s.now()
	res := RestoreResult{Command: newID(), Snapshot: sn.ID, Target: in.Target, ExpiresAt: now.Add(RestoreExpiry)}
	if in.FromOffsite {
		if agErr != nil || (proto.Facts{Protocol: ag.Protocol}).AgentProtocol() < proto.Protocol {
			return RestoreResult{}, fmt.Errorf("%s's agent does not know restores from off-site: upgrade holdfast-agent there first: %w", target, ErrConflict)
		}
		// The off-site instance checks the copy is still there, and audits the grant in its own log.
		pw, err := s.offsite.copySource(ctx, a, fqdn, copied.ID, res.Command)
		if err != nil {
			return RestoreResult{}, err
		}
		b, err := json.Marshal(proto.RestoreSource{FQDN: fqdn, Offsite: true, Repository: gateway.OffsiteRepoURL(s.set.PublicURL, fqdn), RepoPassword: pw})
		if err != nil {
			return RestoreResult{}, err
		}
		sealed = s.seal.Seal(string(b), fqdn)
	}
	payload, err := json.Marshal(spec)
	if err != nil {
		return RestoreResult{}, err
	}
	err = s.st.InTx(ctx, func(q *store.Q) error {
		detail := map[string]any{"command": res.Command, "snapshot": sn.ID, "target": in.Target, "directory": in.Directory, "dry_run": in.DryRun,
			"left_out": len(in.Exclude), "only": len(in.Include), "overwrite": in.Overwrite, "message": in.Message}
		if err := q.EnqueueCommand(ctx, store.Command{ID: res.Command, FQDN: target, Kind: proto.CommandRestore, JobID: spec.JobID, CreatedAt: now,
			CreatedBy: a.Name, Payload: payload, ExpiresAt: &res.ExpiresAt, Sealed: sealed}); err != nil {
			return err
		}
		// From off-site, the host may read its own copy through the gateway's off-site endpoint until it reports it.
		if in.FromOffsite {
			detail["from_offsite"], detail["original"] = true, copied.Original
			if err := q.AddGrant(ctx, store.Grant{CommandID: res.Command, Reader: fqdn, Repo: fqdn, Site: store.SiteOffsite, CreatedAt: now,
				ExpiresAt: res.ExpiresAt.Add(GrantAfterExpiry)}); err != nil {
				return err
			}
		}
		if in.ToHost != "" {
			detail["from_host"], detail["to_host"] = fqdn, target
			// The target may read this host's repository through the gateway until it reports the restore.
			if err := q.AddGrant(ctx, store.Grant{CommandID: res.Command, Reader: target, Repo: fqdn, CreatedAt: now,
				ExpiresAt: res.ExpiresAt.Add(GrantAfterExpiry)}); err != nil {
				return err
			}
			// The snapshot's own host has it in its history too.
			if err := q.Audit(ctx, store.AuditEntry{Actor: a.Name, Source: a.Source, Action: "restore.requested_elsewhere", FQDN: fqdn, JobID: sn.JobID,
				Detail: detail}); err != nil {
				return err
			}
		}
		if err := q.Audit(ctx, store.AuditEntry{Actor: a.Name, Source: a.Source, Action: "restore.requested", FQDN: target, JobID: spec.JobID,
			Detail: detail}); err != nil {
			return err
		}
		return q.NotifyHost(ctx, target)
	})
	return res, err
}

// GrantAfterExpiry is how long a grant outlives its restore command's pick-up deadline: the agent's longest restore,
// and an hour to report it. The grant usually ends sooner, when the restore is reported.
const GrantAfterExpiry = 25 * time.Hour

// sealSource seals what the target's agent needs to restore from host from: from's password. Only the target's agent
// ever gets it, once. It reads the repository through the gateway, under the grant the restore comes with.
func (s *Server) sealSource(ctx context.Context, from, target string) ([]byte, error) {
	c, err := s.st.Q().GetCredentials(ctx, from)
	if err != nil {
		return nil, err
	}
	pw, err := s.seal.Open(c.RepoPassword, from)
	if err != nil {
		return nil, err
	}
	b, err := json.Marshal(proto.RestoreSource{FQDN: from, Repository: gateway.RepoURL(s.set.PublicURL, from), RepoPassword: pw})
	if err != nil {
		return nil, err
	}
	return s.seal.Seal(string(b), target), nil
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
// host's history as a failed restore, and audited.
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
			if err := q.EndGrant(ctx, c.ID, now); err != nil {
				return err
			}
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
