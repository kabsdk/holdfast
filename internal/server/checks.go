package server

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/kabsdk/holdfast/internal/checks"
	"github.com/kabsdk/holdfast/internal/config"
	"github.com/kabsdk/holdfast/internal/restic"
	"github.com/kabsdk/holdfast/internal/server/store"
)

// The checks on every config save (spec §4.5). Errors block the save. Warnings block it unless the save
// accepts their codes, and the accepted codes are stored in the revision and in the audit log.

// ChecksError refuses a save that a check blocks. The API answers 422 with every finding, so the UI and
// the CLI show the same explanation.
type ChecksError struct {
	Findings []checks.Finding // every check that fired
	Blocking []checks.Finding // the errors, and the warnings that were not accepted
}

func (e *ChecksError) Error() string {
	var b strings.Builder
	b.WriteString("the change does not pass the checks:")
	var warnings []string
	for _, f := range e.Blocking {
		fmt.Fprintf(&b, "\n  %s %s  %s", f.Code, f.Severity, f.Message)
		if f.Severity == checks.Warning {
			warnings = append(warnings, f.Code)
		}
	}
	if len(warnings) > 0 {
		fmt.Fprintf(&b, "\nto save anyway, accept the warnings: --accept %s", strings.Join(warnings, ","))
	}
	return b.String()
}

// JobCheckInput is a job to check without saving it.
type JobCheckInput struct {
	Job    config.Job `json:"job"`
	Accept []string   `json:"accept,omitempty"`
}

// JobCheckResult is every check that fired on a job, and the ones that would block saving it.
type JobCheckResult struct {
	Findings []checks.Finding `json:"findings"`
	Blocking []checks.Finding `json:"blocking"`
}

// CheckJob runs every check on a job as SetJob would, and saves nothing.
func (s *Server) CheckJob(ctx context.Context, fqdn, jobID string, in JobCheckInput) (JobCheckResult, error) {
	head, err := s.head(ctx, s.st.Q())
	if err != nil {
		return JobCheckResult{}, err
	}
	h, ok := head.Config.Hosts[fqdn]
	if !ok {
		return JobCheckResult{}, fmt.Errorf("host %s: %w", fqdn, ErrNotFound)
	}
	var old *config.Job
	if j, ok := h.Jobs[jobID]; ok {
		old = &j
	}
	if in.Job.Engine == "" {
		in.Job.Engine = config.EngineRestic
	}
	fs := s.jobFindings(ctx, fqdn, jobID, old, in.Job)
	return JobCheckResult{Findings: fs, Blocking: orEmpty(checks.Blocking(fs, in.Accept))}, nil
}

func orEmpty[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

// jobFindings runs every check on job, which replaces old (nil for a new job).
func (s *Server) jobFindings(ctx context.Context, fqdn, jobID string, old *config.Job, job config.Job) []checks.Finding {
	runs, err := s.st.Q().RunTimes(ctx, fqdn, jobID, 20)
	if err != nil {
		s.log.Warn("reading a job's run times for S002", "host", fqdn, "job", jobID, "err", err)
	}
	fs := checks.Job(job, checks.History{RunTimes: runs})
	if old != nil && !slices.Equal(old.Retention.ForgetArgs(), job.Retention.ForgetArgs()) {
		if f := s.retentionCut(ctx, fqdn, jobID, old.Retention, job.Retention); f != nil {
			fs = append(fs, *f)
			checks.Sort(fs)
		}
	}
	return orEmpty(fs)
}

// retentionCut is R104: restic says, on the real repository, what the old and the new policy keep
// (spec §4.5). A job with no snapshots in the catalogue yet has nothing to cut. If restic cannot be
// asked, the change is treated as a cut, so it needs R104 accepted; the 7-day protection applies either way.
func (s *Server) retentionCut(ctx context.Context, fqdn, jobID string, old, cur config.Retention) *checks.Finding {
	snaps, err := s.st.Q().ListSnapshots(ctx, fqdn, store.SnapshotFilter{JobID: jobID})
	if err == nil && len(snaps) == 0 {
		return nil
	}
	unknown := func(err error) *checks.Finding {
		s.log.Warn("asking restic what a retention change keeps", "host", fqdn, "job", jobID, "err", err)
		return &checks.Finding{Code: "R104", Severity: checks.Warning, Path: "retention",
			Message: "restic could not be asked what the change keeps (" + err.Error() + "), so it may cut retention, after the 7-day protection"}
	}
	r, cleanup, err := s.maintRunner(ctx, fqdn)
	if err != nil {
		return unknown(err)
	}
	defer cleanup()
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	tag := "job=" + jobID
	oldKeep, oldRemove, err := r.ForgetPlan(ctx, tag, old.ForgetArgs())
	if err != nil {
		return unknown(err)
	}
	newKeep, newRemove, err := r.ForgetPlan(ctx, tag, cur.ForgetArgs())
	if err != nil {
		return unknown(err)
	}
	// A backup can finish between the two dry runs, so only the snapshots both of them saw are compared.
	seenOld := map[string]bool{}
	for _, id := range restic.IDs(slices.Concat(oldKeep, oldRemove)) {
		seenOld[id] = true
	}
	both := map[string]bool{}
	for _, id := range restic.IDs(slices.Concat(newKeep, newRemove)) {
		if seenOld[id] {
			both[id] = true
		}
	}
	inBoth := func(snaps []restic.Snapshot) []string {
		return slices.DeleteFunc(restic.IDs(snaps), func(id string) bool { return !both[id] })
	}
	return checks.RetentionCut(inBoth(oldKeep), inBoth(newKeep))
}
