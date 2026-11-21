package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/kabsdk/holdfast/internal/proto"
	"github.com/kabsdk/holdfast/internal/restic"
)

// restoreTimeout bounds one restore, so a hung one does not stop the host's backups for good.
const restoreTimeout = 24 * time.Hour

var unsafeName = regexp.MustCompile(`[^a-z0-9._-]+`)

// runRestore runs a restore the server asked for (plan-m2 §3), within this host's own bounds. Those come
// from agent.yaml, which only root here can change (decision 030). A refusal is reported with its reason.
// It returns false if the agent is shutting down.
func (a *Agent) runRestore(ctx context.Context, c proto.Command) bool {
	started := a.now().UTC()
	spec := c.Restore
	a.mu.Lock()
	rev := a.revision()
	a.mu.Unlock()
	ev := proto.Event{Kind: proto.KindRestore, CommandID: c.ID, JobID: c.JobID, Started: started, Revision: rev, Detail: map[string]string{}}
	fail := func(format string, args ...any) bool {
		ev.Status, ev.Error, ev.Finished = proto.StatusFailed, fmt.Sprintf(format, args...), a.now().UTC()
		a.spool(ev)
		return true
	}
	if spec == nil {
		return fail("refused: the command carries no restore")
	}
	ev.Detail["snapshot"], ev.Detail["target"] = spec.Snapshot, spec.Target
	if spec.DryRun {
		ev.Detail["dry_run"] = "true"
	}
	a.verifyCredentials()
	a.mu.Lock()
	r := a.runner()
	a.running = &proto.Running{Kind: proto.KindRestore, Since: started}
	a.mu.Unlock()
	defer func() { a.mu.Lock(); a.running = nil; a.mu.Unlock() }()

	sn, err := r.SnapshotInfo(ctx, spec.Snapshot)
	if ctx.Err() != nil {
		return false
	}
	if err != nil {
		return fail("snapshot %s: %v", spec.Snapshot, err)
	}
	if sn.Hostname != a.cfg.FQDN {
		return fail("refused by this host: snapshot %s was made by %q, and this host restores only its own snapshots", sn.ShortID, sn.Hostname)
	}

	b := a.cfg.Restore
	var target string
	switch spec.Target {
	case proto.TargetNew, "":
		user := strings.Trim(unsafeName.ReplaceAllString(strings.ToLower(spec.User), "-"), "-")
		if user == "" {
			user = "unknown"
		}
		target = filepath.Join(b.Root, started.Format("20060102-150405")+"-"+user+"-"+sn.ShortID)
		if !spec.DryRun {
			if err := os.MkdirAll(b.Root, 0o700); err != nil {
				return fail("creating %s: %v", b.Root, err)
			}
			if err := os.Mkdir(target, 0o700); err != nil {
				return fail("creating %s: %v", target, err)
			}
		}
	case proto.TargetInPlace:
		if !b.AllowInPlace {
			return fail("refused by this host: in-place restores are not allowed (agent.yaml restore.allow_in_place)")
		}
		target = "/"
	case proto.TargetDirectory:
		if !b.AllowOtherDirectories {
			return fail("refused by this host: restores go only into %s (agent.yaml restore.allow_other_directories)", b.Root)
		}
		target = filepath.Clean(spec.Directory)
		if !filepath.IsAbs(target) || target == "/" {
			return fail("refused by this host: %q is not a directory to restore into", spec.Directory)
		}
		if !spec.DryRun {
			if err := os.MkdirAll(target, 0o700); err != nil {
				return fail("creating %s: %v", target, err)
			}
		}
	default:
		return fail("refused by this host: unknown target %q", spec.Target)
	}
	ev.Detail["directory"] = target

	// In place: first back up what will be replaced, so the restore can be undone (decision 023, plan-m2 Q4).
	if spec.Target == proto.TargetInPlace && !spec.DryRun {
		pre, err := a.safetyBackup(ctx, r, c, spec, sn)
		if ctx.Err() != nil {
			return false
		}
		if err != nil {
			return fail("the safety backup failed, so nothing was restored: %v", err)
		}
		if pre != "" {
			ev.Detail["pre_restore"] = pre
		}
	}

	a.mark(runMarker{Kind: proto.KindRestore, CommandID: c.ID, Started: started, Revision: rev})
	defer os.Remove(a.path("running.json"))
	rctx, cancel := context.WithTimeout(ctx, restoreTimeout)
	sum, err := r.RestoreWith(rctx, sn.ID, restic.RestoreOptions{Target: target, Include: spec.Include, Exclude: spec.Exclude, DryRun: spec.DryRun})
	timedOut := errors.Is(rctx.Err(), context.DeadlineExceeded)
	cancel()
	if ctx.Err() != nil {
		return false
	}
	ev.Finished = a.now().UTC()
	ev.TotalFiles, ev.TotalBytes = sum.TotalFiles, sum.TotalBytes
	ev.Detail["files_restored"] = strconv.FormatInt(sum.FilesRestored, 10)
	ev.Detail["bytes_restored"] = strconv.FormatInt(sum.BytesRestored, 10)
	switch {
	case timedOut:
		ev.Status, ev.ResticExit, ev.Error = proto.StatusFailed, restic.ExitCode(err), fmt.Sprintf("the restore ran for %s and was stopped", restoreTimeout)
		a.unlockStale(ctx)
	case err != nil:
		ev.Status, ev.ResticExit, ev.Error = proto.StatusFailed, restic.ExitCode(err), err.Error()
	default:
		ev.Status = proto.StatusSuccess
	}
	a.spool(ev)
	return true
}

// safetyBackup backs up the paths an in-place restore will replace, and reports it as a pre_restore
// event. It returns the new snapshot's ID, or "" if none of those paths exists now.
func (a *Agent) safetyBackup(ctx context.Context, r restic.Runner, c proto.Command, spec *proto.RestoreSpec, sn restic.Snapshot) (string, error) {
	paths := spec.Include
	if len(paths) == 0 {
		paths = sn.Paths
	}
	var existing []string
	for _, p := range paths {
		if _, err := os.Lstat(p); err == nil {
			existing = append(existing, p)
		}
	}
	if len(existing) == 0 {
		return "", nil
	}
	tags := []string{"pre-restore=" + c.ID}
	if spec.JobID != "" {
		tags = append([]string{"job=" + spec.JobID}, tags...)
	}
	started := a.now().UTC()
	res, err := r.Backup(ctx, restic.BackupOptions{Paths: existing, Excludes: append(append([]string(nil), spec.Exclude...), a.cfg.StateDir),
		Tags: tags, Host: a.cfg.FQDN})
	if err != nil {
		return "", err
	}
	a.spool(proto.Event{Kind: proto.KindPreRestore, JobID: spec.JobID, CommandID: c.ID, Status: proto.StatusSuccess, Started: started,
		Finished: a.now().UTC(), SnapshotID: res.SnapshotID, FilesNew: res.FilesNew, FilesChanged: res.FilesChanged, BytesAdded: res.DataAdded,
		TotalFiles: res.TotalFilesProcessed, TotalBytes: res.TotalBytesProcessed, Detail: map[string]string{"restore_of": spec.Snapshot}})
	return res.SnapshotID, nil
}
