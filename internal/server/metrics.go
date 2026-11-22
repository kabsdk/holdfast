package server

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
)

// handleMetrics exposes Prometheus text format (plan-m1 §11). Ages are left to Prometheus
// (time() - timestamp), so a frozen server is caught by up == 0, not by stale numbers.
// Nothing here is a secret. A backup job is labelled job_id, never job: Prometheus owns the job
// label (the scrape job) and would rename ours to exported_job.
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := s.st.Q()
	var b strings.Builder
	gauge := func(name, help string) { fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s gauge\n", name, help, name) }
	ts := func(t *time.Time) string {
		if t == nil || t.IsZero() {
			return "0"
		}
		return fmt.Sprintf("%d", t.Unix())
	}

	gauge("holdfast_server_up", "1 if the server is serving.")
	b.WriteString("holdfast_server_up 1\n")

	head, err := s.head(ctx, q)
	if err != nil {
		http.Error(w, "metrics unavailable", http.StatusServiceUnavailable)
		return
	}
	gauge("holdfast_config_head_revision", "The newest config revision.")
	fmt.Fprintf(&b, "holdfast_config_head_revision %d\n", head.Number)

	hosts, err := s.Hosts(ctx)
	if err != nil {
		http.Error(w, "metrics unavailable", http.StatusServiceUnavailable)
		return
	}
	gauge("holdfast_host_state", "1 for the host's lifecycle state.")
	for _, h := range hosts {
		for _, st := range []string{"awaiting_enrollment", "enrolled", "retired"} {
			v := 0
			if h.State == st {
				v = 1
			}
			fmt.Fprintf(&b, "holdfast_host_state{host=%q,state=%q} %d\n", h.FQDN, st, v)
		}
	}
	gauge("holdfast_host_first_backup_timestamp_seconds", "When the host's first backup finished; 0 before that.")
	for _, h := range hosts {
		fmt.Fprintf(&b, "holdfast_host_first_backup_timestamp_seconds{host=%q} %s\n", h.FQDN, ts(h.FirstBackupAt))
	}
	gauge("holdfast_agent_last_seen_timestamp_seconds", "Last poll or report from the agent.")
	gauge("holdfast_agent_info", "The agent's versions and OS.")
	gauge("holdfast_agent_applied_revision", "The revision the agent runs.")
	gauge("holdfast_agent_config_in_sync", "1 if the agent runs the host's current config (no drift).")
	gauge("holdfast_agent_clock_offset_seconds", "Agent clock minus server clock, at the last poll.")
	gauge("holdfast_agent_restic_pinned", "1 if the agent runs the restic Holdfast is tested with (decision 032).")
	gauge("holdfast_agent_protocol", "The agent protocol the agent last reported; 0 from an agent before M5, which speaks 1.")
	gauge("holdfast_agent_too_old", "1 if the agent speaks an older protocol than this server accepts: it gets no config changes until it is upgraded.")
	for _, h := range hosts {
		if h.Agent == nil {
			continue
		}
		a := h.Agent
		fmt.Fprintf(&b, "holdfast_agent_last_seen_timestamp_seconds{host=%q} %s\n", h.FQDN, ts(a.LastSeenAt))
		fmt.Fprintf(&b, "holdfast_agent_info{host=%q,agent_version=%q,restic_version=%q,os_id=%q,os_version=%q} 1\n", h.FQDN, a.AgentVersion, a.ResticVersion, a.OSID, a.OSVersion)
		fmt.Fprintf(&b, "holdfast_agent_applied_revision{host=%q} %d\n", h.FQDN, a.AppliedRevision)
		sync := 0
		if h.InSync {
			sync = 1
		}
		fmt.Fprintf(&b, "holdfast_agent_config_in_sync{host=%q} %d\n", h.FQDN, sync)
		fmt.Fprintf(&b, "holdfast_agent_clock_offset_seconds{host=%q} %.3f\n", h.FQDN, float64(a.ClockOffsetMS)/1000)
		if a.ResticVersion != "" {
			pinned := 1
			if h.ResticWarning != "" {
				pinned = 0
			}
			fmt.Fprintf(&b, "holdfast_agent_restic_pinned{host=%q} %d\n", h.FQDN, pinned)
		}
		old := 0
		if h.AgentTooOld {
			old = 1
		}
		fmt.Fprintf(&b, "holdfast_agent_protocol{host=%q} %d\n", h.FQDN, a.Protocol)
		fmt.Fprintf(&b, "holdfast_agent_too_old{host=%q} %d\n", h.FQDN, old)
	}

	gauge("holdfast_job_last_success_timestamp_seconds", "Finish time of the last successful or warning backup.")
	gauge("holdfast_job_longest_gap_seconds", "The schedule's longest gap; the stale alert fires at 2x this, and the off-site lag alert too.")
	gauge("holdfast_job_last_status", "1 for the status of the job's most recent run.")
	for _, h := range hosts {
		for _, j := range h.Jobs {
			var last *time.Time
			status := ""
			if j.Status != nil {
				last, status = j.Status.LastSuccessAt, j.Status.LastStatus
			}
			gap, _ := time.ParseDuration(j.LongestGap)
			fmt.Fprintf(&b, "holdfast_job_last_success_timestamp_seconds{host=%q,job_id=%q} %s\n", h.FQDN, j.ID, ts(last))
			fmt.Fprintf(&b, "holdfast_job_longest_gap_seconds{host=%q,job_id=%q} %.0f\n", h.FQDN, j.ID, gap.Seconds())
			for _, st := range []string{"success", "warning", "failed", "interrupted"} {
				v := 0
				if status == st {
					v = 1
				}
				fmt.Fprintf(&b, "holdfast_job_last_status{host=%q,job_id=%q,status=%q} %d\n", h.FQDN, j.ID, st, v)
			}
		}
	}

	counters, err := q.ListRunCounters(ctx)
	if err == nil {
		fmt.Fprintf(&b, "# HELP holdfast_runs_total Runs reported by agents.\n# TYPE holdfast_runs_total counter\n")
		for _, c := range counters {
			fmt.Fprintf(&b, "holdfast_runs_total{host=%q,kind=%q,job_id=%q,status=%q} %d\n", c.FQDN, c.Kind, c.JobID, c.Status, c.Count)
		}
	}
	if last, err := q.LastCheckSuccess(ctx); err == nil {
		gauge("holdfast_maintenance_last_success_timestamp_seconds", "When the agent's repository check last succeeded.")
		names := make([]string, 0, len(last))
		for f := range last {
			names = append(names, f)
		}
		sort.Strings(names)
		for _, f := range names {
			t := last[f]
			fmt.Fprintf(&b, "holdfast_maintenance_last_success_timestamp_seconds{host=%q,kind=\"check\"} %s\n", f, ts(&t))
		}
	}
	if runs, err := q.RetentionHealth(ctx); err == nil {
		gauge("holdfast_retention_last_run_failed", "1 if the host's newest finished forget-and-prune failed, such as a repository check that blocked the prune.")
		gauge("holdfast_retention_last_success_timestamp_seconds", "When a forget-and-prune on the host last finished well.")
		for _, h := range runs {
			failed := 0
			if h.LastFailed {
				failed = 1
			}
			fmt.Fprintf(&b, "holdfast_retention_last_run_failed{host=%q} %d\n", h.FQDN, failed)
			fmt.Fprintf(&b, "holdfast_retention_last_success_timestamp_seconds{host=%q} %s\n", h.FQDN, ts(h.LastSuccess))
		}
	}
	gauge("holdfast_snapshots", "Snapshots in the host's repository, from the catalogue.")
	gauge("holdfast_snapshots_not_made_by_holdfast", "Snapshots in the repository that no agent reported (decision 032).")
	gauge("holdfast_snapshots_disappeared", "Snapshots gone from the repository without Holdfast deleting them, not yet acknowledged.")
	for _, h := range hosts {
		if h.CatalogueRefreshedAt == nil {
			continue
		}
		fmt.Fprintf(&b, "holdfast_snapshots{host=%q} %d\n", h.FQDN, h.Snapshots)
		fmt.Fprintf(&b, "holdfast_snapshots_not_made_by_holdfast{host=%q} %d\n", h.FQDN, h.NotMadeByHoldfast)
		fmt.Fprintf(&b, "holdfast_snapshots_disappeared{host=%q} %d\n", h.FQDN, h.Disappeared)
	}
	gauge("holdfast_repository_foreign_keys", "Repository keys (restic passwords) added outside Holdfast (decision 032).")
	for _, h := range hosts {
		if h.CatalogueRefreshedAt != nil {
			fmt.Fprintf(&b, "holdfast_repository_foreign_keys{host=%q} %d\n", h.FQDN, len(h.ForeignKeys))
		}
	}
	gauge("holdfast_storage_contract_ok", "1 if the host's storage passed every storage check at the last test (docs/storage.md).")
	gauge("holdfast_storage_contract_failed", "1 for each storage check (code) that failed at the last test.")
	gauge("holdfast_storage_checked_timestamp_seconds", "When the host's storage contract was last tested.")
	for _, h := range hosts {
		if h.Storage == nil {
			continue
		}
		ok := 1
		if !h.Storage.OK() {
			ok = 0
		}
		fmt.Fprintf(&b, "holdfast_storage_contract_ok{host=%q} %d\n", h.FQDN, ok)
		for _, code := range h.Storage.Codes() {
			fmt.Fprintf(&b, "holdfast_storage_contract_failed{host=%q,code=%q} 1\n", h.FQDN, code)
		}
		fmt.Fprintf(&b, "holdfast_storage_checked_timestamp_seconds{host=%q} %s\n", h.FQDN, ts(&h.Storage.CheckedAt))
	}
	if n, err := q.HostsMissingEscrow(ctx); err == nil {
		gauge("holdfast_escrow_missing_hosts", "Hosts whose current credentials are in no escrow bundle.")
		fmt.Fprintf(&b, "holdfast_escrow_missing_hosts %d\n", n)
	}
	// Decision 036: the escrow files in the bucket. Without a recipient none is written.
	on := 0
	if s.escrowOn() {
		on = 1
	}
	gauge("holdfast_escrow_configured", "1 if server.yaml names an age recipient, so repository passwords are escrowed in the bucket.")
	fmt.Fprintf(&b, "holdfast_escrow_configured %d\n", on)
	gauge("holdfast_escrow_current", "1 if escrow/<fqdn>.age in the bucket holds the host's current repository password.")
	gauge("holdfast_repository_bytes", "The host's repository in the bucket: current objects, and old versions kept until the lifecycle rule expires them.")
	gauge("holdfast_repository_objects", "Current objects under the host's prefix.")
	gauge("holdfast_repository_size_timestamp_seconds", "When the host's repository was last measured.")
	gauge("holdfast_host_decommission_due_timestamp_seconds", "When a removed host's repository is to be deleted; 0 once done.")
	for _, h := range hosts {
		if d := h.Decommission; d != nil {
			due := "0"
			if d.DoneAt == nil {
				due = ts(&d.DueAt)
			}
			fmt.Fprintf(&b, "holdfast_host_decommission_due_timestamp_seconds{host=%q} %s\n", h.FQDN, due)
			if d.DoneAt != nil {
				continue
			}
		}
		if s.escrowOn() {
			cur := 0
			if h.Escrow.Current {
				cur = 1
			}
			fmt.Fprintf(&b, "holdfast_escrow_current{host=%q} %d\n", h.FQDN, cur)
		}
		if sz := h.Size; sz != nil {
			fmt.Fprintf(&b, "holdfast_repository_bytes{host=%q,versions=\"current\"} %d\n", h.FQDN, sz.Current)
			fmt.Fprintf(&b, "holdfast_repository_bytes{host=%q,versions=\"noncurrent\"} %d\n", h.FQDN, sz.Noncurrent)
			fmt.Fprintf(&b, "holdfast_repository_objects{host=%q} %d\n", h.FQDN, sz.Objects)
			fmt.Fprintf(&b, "holdfast_repository_size_timestamp_seconds{host=%q} %s\n", h.FQDN, ts(&sz.At))
		}
	}
	// The nightly export (decision 040): only with an age recipient, which it is encrypted to.
	if s.escrowOn() && !s.set.IsVault() {
		if st, err := q.ExportState(ctx); err == nil {
			var good *time.Time
			if st.LastGood != nil {
				good = &st.LastGood.StartedAt
			}
			failed := 0
			if st.Last != nil && st.Last.Error != "" {
				failed = 1
			}
			gauge("holdfast_export_last_success_timestamp_seconds", "When the last nightly export was written to the bucket; 0 before the first.")
			fmt.Fprintf(&b, "holdfast_export_last_success_timestamp_seconds %s\n", ts(good))
			gauge("holdfast_export_failed", "1 if the last attempt at the nightly export failed.")
			fmt.Fprintf(&b, "holdfast_export_failed %d\n", failed)
			if s.offsite != nil {
				var off *time.Time
				if st.LastOffsite != nil {
					off = &st.LastOffsite.StartedAt
				}
				offFailed := 0
				if st.Last != nil && st.Last.Error == "" && st.Last.OffsiteError != "" {
					offFailed = 1
				}
				gauge("holdfast_export_offsite_last_success_timestamp_seconds", "When the last nightly export was written to the off-site bucket; 0 before the first.")
				fmt.Fprintf(&b, "holdfast_export_offsite_last_success_timestamp_seconds %s\n", ts(off))
				gauge("holdfast_export_offsite_failed", "1 if the last nightly export was written to the bucket but not off-site.")
				fmt.Fprintf(&b, "holdfast_export_offsite_failed %d\n", offFailed)
			}
		}
	}
	if s.gw != nil {
		s.gw.WriteMetrics(&b)
	}
	s.writeOffsiteMetrics(ctx, &b) // nothing without offsite:
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	_, _ = w.Write([]byte(b.String()))
}
