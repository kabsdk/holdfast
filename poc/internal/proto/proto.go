// Package proto defines the wire protocol between holdfast-agent and holdfast-server.
//
// Agents always dial out (HTTPS long-poll). Every poll doubles as a heartbeat, so
// there is no long-lived session whose loss could cancel running work.
package proto

import "time"

// Retention is a restic forget policy, applied per job by the server (never the agent).
type Retention struct {
	KeepLast    int `json:"keep_last,omitempty"`
	KeepDaily   int `json:"keep_daily,omitempty"`
	KeepWeekly  int `json:"keep_weekly,omitempty"`
	KeepMonthly int `json:"keep_monthly,omitempty"`
	KeepYearly  int `json:"keep_yearly,omitempty"`
}

// Job is one scheduled file backup on one host.
type Job struct {
	ID        string    `json:"id"`
	Paths     []string  `json:"paths"`
	Excludes  []string  `json:"excludes,omitempty"`
	Interval  Duration  `json:"interval"`
	Retry     Duration  `json:"retry,omitempty"`
	Retention Retention `json:"retention"`
	// ExtraArgs is an allow-listed set of extra restic backup flags (validated by the server).
	ExtraArgs []string `json:"extra_args,omitempty"`
}

// HostConfig is the desired state the server hands to one agent. The agent caches it
// on disk and keeps executing it while the server is unreachable.
type HostConfig struct {
	Version    int64  `json:"version"`
	Host       string `json:"host"`
	Repository string `json:"repository"`
	// Password is the per-host repository password. It is only ever sent to this host.
	Password string `json:"password"`
	Jobs     []Job  `json:"jobs"`
}

// EnrollRequest is sent once with a single-use enrollment code.
type EnrollRequest struct {
	Host string `json:"host"`
	Code string `json:"code"`
}

// EnrollResponse returns the agent's long-lived token.
type EnrollResponse struct {
	Token string `json:"token"`
}

// Command is work the server asks the agent to do now (restores, ad-hoc backups).
type Command struct {
	ID       string   `json:"id"`
	Kind     string   `json:"kind"` // "restore" | "backup_now"
	JobID    string   `json:"job_id,omitempty"`
	Snapshot string   `json:"snapshot,omitempty"`
	Target   string   `json:"target,omitempty"`
	Include  []string `json:"include,omitempty"`
}

// PollResponse is the answer to a long-poll.
type PollResponse struct {
	Config   *HostConfig `json:"config,omitempty"` // only set when Version changed
	Commands []Command   `json:"commands,omitempty"`
}

// Run statuses. Every run ends in exactly one of these and is always reported.
const (
	StatusSuccess     = "success"
	StatusWarning     = "warning"     // restic exit 3: snapshot created but some files unreadable
	StatusFailed      = "failed"      // no snapshot
	StatusInterrupted = "interrupted" // agent died mid-run; detected at next start
)

// RunEvent is a terminal result of one run, spooled on disk until the server acks it.
type RunEvent struct {
	ID         string    `json:"id"`
	Host       string    `json:"host"`
	JobID      string    `json:"job_id,omitempty"`
	CommandID  string    `json:"command_id,omitempty"`
	Kind       string    `json:"kind"` // "backup" | "restore"
	Status     string    `json:"status"`
	Started    time.Time `json:"started"`
	Finished   time.Time `json:"finished"`
	SnapshotID string    `json:"snapshot_id,omitempty"`
	FilesNew   int64     `json:"files_new,omitempty"`
	BytesAdded int64     `json:"bytes_added,omitempty"`
	Error      string    `json:"error,omitempty"`
}

// ReportRequest delivers spooled events. The server acks by ID; the agent deletes acked events.
type ReportRequest struct {
	Events []RunEvent `json:"events"`
}

// ReportResponse lists accepted event IDs (idempotent: re-sent events are acked again).
type ReportResponse struct {
	Acked []string `json:"acked"`
}

// RestoreRequest is the admin API body for queuing a restore on an agent.
type RestoreRequest struct {
	Snapshot string   `json:"snapshot"`
	Target   string   `json:"target"`
	Include  []string `json:"include,omitempty"`
}

// Duration is a time.Duration that marshals as a Go duration string ("24h", "15m").
type Duration struct{ time.Duration }

func (d Duration) MarshalJSON() ([]byte, error) { return []byte(`"` + d.String() + `"`), nil }

func (d *Duration) UnmarshalJSON(b []byte) error {
	s := string(b)
	if len(s) >= 2 && s[0] == '"' {
		s = s[1 : len(s)-1]
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	d.Duration = v
	return nil
}
