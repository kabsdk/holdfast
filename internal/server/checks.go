package server

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/kabsdk/holdfast/internal/checks"
	"github.com/kabsdk/holdfast/internal/config"
	"github.com/kabsdk/holdfast/internal/proto"
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

// JobCheckResult is every check that fired on a job, the ones that would block saving it, and what the
// job gets, so the UI shows them without working anything out itself (decision 028).
type JobCheckResult struct {
	Findings []checks.Finding `json:"findings"`
	Blocking []checks.Finding `json:"blocking"`
	// Problems are structural ones, such as a path that is not absolute. They block the save as well.
	Problems config.Problems `json:"problems"`
	Gets     JobGets         `json:"gets"`
	Changes  []Change        `json:"changes"` // against the saved job; all of it for a new one
	CLI      string          `json:"cli"`     // the holdfast job set command for this change
}

// JobGets is what a job's schedule and retention give (spec §4.1).
type JobGets struct {
	NextRuns      []time.Time `json:"next_runs"`
	LongestGap    string      `json:"longest_gap"`
	WorstCaseLoss string      `json:"worst_case_loss"`
	StaleAfter    string      `json:"stale_after"`
	checks.Expect
	ResticFlags []string `json:"restic_flags"` // the restic forget flags the retention maps to (spec §4.2)
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
	if s.set.IsVault() { // as SetJob: only the off-site retention is the vault's
		if old == nil {
			return JobCheckResult{}, errVaultHosts
		}
		j := *old
		j.Retention = in.Job.Retention
		in.Job = j
	}
	res := JobCheckResult{Problems: orEmpty(withJob(head.Config, fqdn, jobID, in.Job).Validate()), CLI: jobSetCommand(fqdn, jobID, in.Job, in.Accept)}
	fs := s.jobFindings(ctx, fqdn, jobID, old, in.Job)
	res.Findings, res.Blocking = fs, orEmpty(checks.Blocking(fs, in.Accept))
	if err := in.Job.Schedule.Validate(); err == nil {
		loc, _ := in.Job.Schedule.Location(s.zone)
		res.Gets = JobGets{NextRuns: in.Job.Schedule.NextN(s.now(), 5, loc), LongestGap: in.Job.Schedule.LongestGap().String(),
			WorstCaseLoss: in.Job.Schedule.WorstCaseLoss().String(), StaleAfter: in.Job.Schedule.StaleAfter().String()}
	}
	res.Gets.Expect, res.Gets.ResticFlags = checks.Estimate(in.Job), orEmpty(in.Job.Retention.ForgetArgs())
	var before any = map[string]any{}
	if old != nil {
		before = toAny(*old)
	}
	res.Changes = diffValues(before, toAny(in.Job))
	return res, nil
}

// jobSetCommand is the holdfast job set command that makes this change: the UI shows it, so people can
// script what they did (decision 014).
func jobSetCommand(fqdn, jobID string, j config.Job, accept []string) string {
	args := []string{"holdfast", "job", "set", fqdn, jobID}
	for _, p := range j.Paths {
		args = append(args, "--path", shellWord(p))
	}
	for _, e := range j.Excludes {
		args = append(args, "--exclude", shellWord(e))
	}
	sc := j.Schedule
	switch {
	case sc.Daily != nil:
		times := make([]string, 0, len(sc.Daily.Times))
		for _, t := range sc.Daily.Times {
			times = append(times, string(t))
		}
		args = append(args, "--daily", strings.Join(times, ","))
	case sc.Every != nil:
		args = append(args, "--every", fmt.Sprintf("%d%s", sc.Every.N, map[string]string{"minutes": "m", "hours": "h"}[sc.Every.Unit]))
		if sc.Every.From != "" && sc.Every.From != "00:00" {
			args = append(args, "--from", string(sc.Every.From))
		}
	case sc.Weekly != nil:
		args = append(args, "--weekly", strings.Join(sc.Weekly.Days, ","), "--at", string(sc.Weekly.At))
	}
	if sc.Spread.Minutes > 0 {
		args = append(args, "--spread", fmt.Sprint(sc.Spread.Minutes))
		if !sc.Spread.Enabled {
			args = append(args, "--no-spread")
		}
	}
	if sc.Zone != "" {
		args = append(args, "--zone", sc.Zone)
	}
	r := j.Retention
	for _, t := range []struct {
		flag string
		d    *config.Duration
	}{{"--keep-within", r.Within}, {"--keep-hourly", r.Hourly}, {"--keep-daily", r.Daily}, {"--keep-weekly", r.Weekly}, {"--keep-monthly", r.Monthly}, {"--keep-yearly", r.Yearly}} {
		if t.d != nil {
			args = append(args, t.flag, fmt.Sprintf("%d%s", t.d.N, map[config.Unit]string{config.Hours: "h", config.Days: "d", config.Weeks: "w", config.Months: "mo", config.Years: "y"}[t.d.Unit]))
		}
	}
	args = append(args, "--keep-last", fmt.Sprint(r.KeepLast))
	if j.LimitUploadKiB > 0 {
		args = append(args, "--limit-upload", fmt.Sprint(j.LimitUploadKiB))
	}
	if len(accept) > 0 {
		args = append(args, "--accept", strings.Join(accept, ","))
	}
	return strings.Join(args, " ")
}

// shellWord quotes a word for a shell when it needs it.
func shellWord(s string) string {
	if s != "" && strings.Trim(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789/._-:,=+@") == "" {
		return s
	}
	return shellQuote(s)
}

func orEmpty[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

// jobFindings runs every check on job, which replaces old (nil for a new job).
func (s *Server) jobFindings(ctx context.Context, fqdn, jobID string, old *config.Job, job config.Job) []checks.Finding {
	if s.set.IsVault() {
		return s.vaultJobFindings(ctx, fqdn, jobID, old, job)
	}
	runs, err := s.st.Q().RunTimes(ctx, fqdn, jobID, 20)
	if err != nil {
		s.log.Warn("reading a job's run times for S002", "host", fqdn, "job", jobID, "err", err)
	}
	fs := checks.Job(job, checks.History{RunTimes: runs})
	fs = append(fs, s.dumpFindings(ctx, fqdn, job)...)
	checks.Sort(fs)
	if old != nil && !slices.Equal(old.Retention.ForgetArgs(), job.Retention.ForgetArgs()) {
		if f := s.retentionCut(ctx, fqdn, jobID, old.Retention, job.Retention); f != nil {
			fs = append(fs, *f)
			checks.Sort(fs)
		}
	}
	return orEmpty(fs)
}

// dumpFindings checks a dump job against what the host's agent last reported (decision 037):
//   - D002, an error: the agent does not know dump jobs (protocol 1), and would back it up as a job with no paths;
//   - D001, an error: its agent.yaml defines no dump of that name;
//   - I002, info: no agent has enrolled yet, so the name is checked when one does.
func (s *Server) dumpFindings(ctx context.Context, fqdn string, job config.Job) []checks.Finding {
	if !job.IsDump() {
		return nil
	}
	a, err := s.st.Q().GetAgent(ctx, fqdn)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return []checks.Finding{{Code: "I002", Severity: checks.Info, Path: "dump",
			Message: fmt.Sprintf("no agent has enrolled yet, so whether it defines the dump %q is not known; the job fails until it does", job.Dump)}}
	case err != nil:
		s.log.Warn("reading the agent for a dump check", "host", fqdn, "err", err)
		return nil
	case (proto.Facts{Protocol: a.Protocol}).AgentProtocol() < proto.ProtocolRecovery:
		return []checks.Finding{{Code: "D002", Severity: checks.Error, Path: "dump",
			Message: fmt.Sprintf("this host's agent (%s) does not know dump jobs: upgrade holdfast-agent first (docs/install.md)", a.AgentVersion)}}
	case !slices.Contains(a.Dumps, job.Dump):
		have := "none"
		if len(a.Dumps) > 0 {
			have = strings.Join(a.Dumps, ", ")
		}
		return []checks.Finding{{Code: "D001", Severity: checks.Error, Path: "dump",
			Message: fmt.Sprintf("this host's agent.yaml defines no dump named %q (it has: %s)", job.Dump, have)}}
	}
	return nil
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
