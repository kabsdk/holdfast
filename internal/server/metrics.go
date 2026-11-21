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
// Nothing here is a secret.
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
	}

	gauge("holdfast_job_last_success_timestamp_seconds", "Finish time of the last successful or warning backup.")
	gauge("holdfast_job_longest_gap_seconds", "The schedule's longest gap; the stale alert fires at 2x this.")
	gauge("holdfast_job_last_status", "1 for the status of the job's most recent run.")
	for _, h := range hosts {
		for _, j := range h.Jobs {
			var last *time.Time
			status := ""
			if j.Status != nil {
				last, status = j.Status.LastSuccessAt, j.Status.LastStatus
			}
			gap, _ := time.ParseDuration(j.LongestGap)
			fmt.Fprintf(&b, "holdfast_job_last_success_timestamp_seconds{host=%q,job=%q} %s\n", h.FQDN, j.ID, ts(last))
			fmt.Fprintf(&b, "holdfast_job_longest_gap_seconds{host=%q,job=%q} %.0f\n", h.FQDN, j.ID, gap.Seconds())
			for _, st := range []string{"success", "warning", "failed", "interrupted"} {
				v := 0
				if status == st {
					v = 1
				}
				fmt.Fprintf(&b, "holdfast_job_last_status{host=%q,job=%q,status=%q} %d\n", h.FQDN, j.ID, st, v)
			}
		}
	}

	counters, err := q.ListRunCounters(ctx)
	if err == nil {
		fmt.Fprintf(&b, "# HELP holdfast_runs_total Runs reported by agents.\n# TYPE holdfast_runs_total counter\n")
		for _, c := range counters {
			fmt.Fprintf(&b, "holdfast_runs_total{host=%q,kind=%q,job=%q,status=%q} %d\n", c.FQDN, c.Kind, c.JobID, c.Status, c.Count)
		}
	}
	if last, err := q.LastMaintenanceSuccess(ctx, "check"); err == nil {
		gauge("holdfast_maintenance_last_success_timestamp_seconds", "When maintenance of this kind last succeeded.")
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
	if n, err := q.HostsMissingEscrow(ctx); err == nil {
		gauge("holdfast_escrow_missing_hosts", "Hosts whose current credentials are in no escrow bundle.")
		fmt.Fprintf(&b, "holdfast_escrow_missing_hosts %d\n", n)
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	_, _ = w.Write([]byte(b.String()))
}
