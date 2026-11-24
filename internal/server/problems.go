package server

import (
	"fmt"
	"strings"
	"time"
)

// Problems are what the server knows is wrong with a host: what the UI's Overview lists under each host and counts in
// its tiles, and what the server's own alerts send (notify.go). The server is the storage witness too:
// not_in_bucket compares the agents' reports with what restic reads from the bucket.

// Problem is one thing wrong with a host, or with one of its jobs.
type Problem struct {
	Code     string `json:"code"`     // stable, such as stale or storage
	Severity string `json:"severity"` // error: something is not being backed up or is at risk; warning: look into it
	JobID    string `json:"job_id,omitempty"`
	Message  string `json:"message"`
}

// OfflineAfter is how long an enrolled agent may be silent before it is a problem. It is a warning: a host may be
// down on purpose, and a job that goes stale or fails is the error.
const OfflineAfter = 10 * time.Minute

func ago(d time.Duration) string {
	switch {
	case d >= 48*time.Hour:
		return fmt.Sprintf("%d days", int(d/(24*time.Hour)))
	case d >= 2*time.Hour:
		return fmt.Sprintf("%d hours", int(d/time.Hour))
	}
	return fmt.Sprintf("%d minutes", int(d/time.Minute))
}

// problems works out a host's problems from its view. retentionFailed says whether its last forget-and-prune
// failed; escrowOn whether server.yaml names an age recipient.
func problems(v HostView, now time.Time, retentionFailed, escrowOn bool) []Problem {
	out := []Problem{}
	add := func(code, sev, job, format string, a ...any) {
		out = append(out, Problem{Code: code, Severity: sev, JobID: job, Message: fmt.Sprintf(format, a...)})
	}
	if v.RemovedAt != nil {
		d := v.Decommission
		switch {
		case v.State != "retired":
			add("removing", "warning", "", "removed; waiting for its agent to take the empty config, after which the host is retired")
		case d != nil && d.DoneAt == nil && d.Error != "":
			add("decommission", "error", "", "the decommission asked for by %s could not delete the repository: %s; it is tried again", d.RequestedBy, d.Error)
		case d != nil && d.DoneAt == nil:
			add("decommission", "warning", "", "its repository is deleted on %s, as %s asked (holdfast host decommission %s --cancel stops it)",
				d.DueAt.UTC().Format("2006-01-02 15:04 UTC"), d.RequestedBy, v.FQDN)
		}
		return out
	}
	if v.RollingBack {
		add("rollback", "warning", "", "its repository is being rolled back; its agent is paused meanwhile (holdfast host rollbacks %s)", v.FQDN)
	}
	if v.RepoKeyRetiring != "" {
		add("rotation", "error", "", "the old repository password still opens the repository (key %s): run holdfast host rotate %s again", v.RepoKeyRetiring, v.FQDN)
	}
	if v.State == "awaiting_enrollment" && v.Agent != nil {
		add("enroll", "warning", "", "waiting for the host to enroll again with its new code; until then it gets no config and reports nothing")
	}
	switch {
	case !escrowOn:
		add("escrow", "warning", "", "server.yaml names no escrow.age_recipient, so the repository password is not escrowed in the bucket (decision 036)")
	case !v.Escrow.Current && v.Escrow.Error != "":
		add("escrow", "warning", "", "the escrow file could not be written: %s", v.Escrow.Error)
	}
	if v.SizeError != "" {
		add("size", "warning", "", "the repository's size could not be measured: %s", v.SizeError)
	}
	if v.State == "enrolled" && v.Agent != nil && v.Agent.LastSeenAt != nil && now.Sub(*v.Agent.LastSeenAt) > OfflineAfter {
		add("offline", "warning", "", "the agent has not been in touch for %s: its backups go through this server, so none runs until it is back", ago(now.Sub(*v.Agent.LastSeenAt)))
	}
	if v.Storage != nil && !v.Storage.OK() {
		add("storage", "error", "", "the S3 key fails the storage contract: %s (docs/storage.md)", strings.Join(v.Storage.Codes(), ", "))
	}
	if v.Disappeared > 0 {
		add("disappeared", "error", "", "%d snapshots disappeared without Holdfast deleting them", v.Disappeared)
	}
	if len(v.ForeignKeys) > 0 {
		add("foreign_keys", "error", "", "%d restic passwords were added to the repository outside Holdfast", len(v.ForeignKeys))
	}
	if v.ResticWarning != "" {
		add("restic", "warning", "", "%s", v.ResticWarning)
	}
	if v.AgentTooOld {
		add("agent_too_old", "warning", "", "the agent (%s) is older than this server accepts: upgrade holdfast-agent. It backs up on its cached config and gets no changes meanwhile",
			v.Agent.AgentVersion)
	}
	if v.NotMadeByHoldfast > 0 {
		add("not_made_by_holdfast", "warning", "", "%d snapshots in the repository were not made by Holdfast", v.NotMadeByHoldfast)
	}
	switch {
	case v.AheadOfServer != nil:
		add("ahead", "warning", "", "the agent runs revision %d, newer than this server's head %d when it was restored from an export: "+
			"that change was lost here, so make it again; the agent keeps its own config meanwhile", v.Agent.AppliedRevision, *v.AheadOfServer)
	case v.State == "enrolled" && v.Online && !v.InSync:
		add("drift", "warning", "", "the agent runs revision %d, not %d", v.Agent.AppliedRevision, v.DesiredRevision)
	}
	if v.CatalogueError != "" {
		add("catalogue", "warning", "", "the snapshot list could not be read: %s", v.CatalogueError)
	}
	if retentionFailed {
		add("retention", "warning", "", "the last forget and prune failed (holdfast prunes %s says why)", v.FQDN)
	}
	// Off-site (decision 039): on the off-site pages, and on the host's own when the copies fail or lag.
	if v.CopyError != "" {
		add("copy", "error", "", "the last copy to off-site failed: %s", v.CopyError)
	}
	if o := v.Offsite; o != nil && o.CopyError != "" {
		add("offsite_copy", "error", "", "the last copy to off-site failed: %s", o.CopyError)
	}
	if o := v.Offsite; o != nil {
		// A job whose copies fall behind as the off-site pages' lag does: past twice its longest gap (decision 039).
		staleAfter := map[string]time.Duration{}
		for _, j := range v.Jobs {
			if d, err := time.ParseDuration(j.StaleAfter); err == nil {
				staleAfter[j.ID] = d
			}
		}
		for _, oj := range o.Jobs {
			behind := time.Duration(oj.BehindS) * time.Second
			if d := staleAfter[oj.ID]; !oj.Excluded && !oj.Retired && d > 0 && behind > d {
				add("offsite_lag", "error", oj.ID, "the newest off-site copy is %s behind the newest snapshot, more than twice the job's longest gap", ago(behind))
			}
		}
	}
	for _, j := range v.Jobs {
		// The storage witness: the agent said it backed up, and restic, reading the bucket after the
		// report, did not find the snapshot. Whatever the agent says, that backup is not there to restore.
		if j.InBucket != nil && !*j.InBucket && j.Status != nil {
			add("not_in_bucket", "error", j.ID, "the agent reported a backup (snapshot %.8s), but the bucket does not hold it: the server read the repository after the report and did not find it", j.Status.LastSnapshotID)
		}
		switch {
		case j.Lagging:
			add("lag", "error", j.ID, "the newest off-site copy is %s behind the newest snapshot in primary, more than twice the job's longest gap", ago(time.Duration(*j.BehindS)*time.Second))
		case j.Stale:
			last := "no successful backup yet"
			if j.Status != nil && j.Status.LastSuccessAt != nil {
				last = "no successful backup for " + ago(now.Sub(*j.Status.LastSuccessAt))
			}
			add("stale", "error", j.ID, "%s, more than twice its longest gap", last)
		case j.Status != nil && (j.Status.LastStatus == "failed" || j.Status.LastStatus == "interrupted"):
			add("failed", "error", j.ID, "the last backup %s: %s", j.Status.LastStatus, j.Status.LastError)
		case j.Status != nil && j.Status.LastStatus == "warning":
			add("incomplete", "warning", j.ID, "the last backup is incomplete: some files could not be read")
		}
	}
	return out
}
