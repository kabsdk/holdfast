// Package proto defines the protocol between holdfast-agent and holdfast-server (plan-m1 §7).
//
// Agents always dial out, over HTTPS long-poll, and every poll is also a heartbeat. The server takes
// the host's identity from the agent's credential, never from a request body.
package proto

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"

	"github.com/kabsdk/holdfast/internal/schedule"
)

// Base is the path prefix of the agent protocol. It is not part of the user-facing /api/v1.
const Base = "/agent/v1"

// Facts describe the host. The agent sends them with enrollment and with every poll.
type Facts struct {
	AgentVersion  string    `json:"agent_version"`
	ResticVersion string    `json:"restic_version"`
	OSID          string    `json:"os_id"`         // os-release ID: ubuntu, rhel, ol
	OSVersion     string    `json:"os_version"`    // os-release VERSION_ID
	HostnameFQDN  string    `json:"hostname_fqdn"` // what `hostname -f` says
	Timezone      string    `json:"timezone"`
	Now           time.Time `json:"now"` // the agent's clock, so the server can see skew
}

// EnrollRequest is sent once, with a single-use code.
type EnrollRequest struct {
	FQDN  string `json:"fqdn"`
	Code  string `json:"code"`
	Facts Facts  `json:"facts"`
}

// EnrollResponse returns the agent's credential. M1 uses a bearer token (plan-m1 Q3, a stand-in for
// OQ3); the server stores only its hash.
type EnrollResponse struct {
	Token string `json:"token"`
}

// Applied is what the agent runs: the revision in which its host config last changed, and the
// sha256 of that host config. Drift is a mismatch with what the server wants (plan-m1 Q8).
type Applied struct {
	Revision int64  `json:"revision"`
	SHA256   string `json:"sha256"`
}

// Running describes the run in progress, if any.
type Running struct {
	Kind  string    `json:"kind"`
	JobID string    `json:"job_id,omitempty"`
	Since time.Time `json:"since"`
}

// PollRequest is one long-poll. It is also the heartbeat.
type PollRequest struct {
	Applied            Applied  `json:"applied"`
	CredentialsVersion int      `json:"credentials_version"`
	AckedCommands      []string `json:"acked_commands,omitempty"`
	Running            *Running `json:"running,omitempty"`
	Facts              Facts    `json:"facts"`
}

// JobConfig is one job as the agent runs it. Retention is not sent: agents never forget or prune.
// The schedule's zone is always resolved by the server.
type JobConfig struct {
	ID       string            `json:"id"`
	Engine   string            `json:"engine"`
	Paths    []string          `json:"paths"`
	Excludes []string          `json:"excludes,omitempty"`
	Schedule schedule.Schedule `json:"schedule"`
}

// HostConfig is the desired state of one host.
type HostConfig struct {
	FQDN       string       `json:"fqdn"`
	Revision   int64        `json:"revision"` // the revision in which this host's config last changed
	SHA256     string       `json:"sha256"`
	Repository string       `json:"repository"`
	Jobs       []JobConfig  `json:"jobs"`
	Check      *CheckConfig `json:"check,omitempty"` // nil: no daily check, only checks on request
}

// CheckConfig is the agent's daily repository check (decision 030). After a backup, once a day, the
// agent runs restic check --read-data-subset n/CycleDays, so all data is read once per cycle. It runs in
// the agent's one-restic-at-a-time loop, so it can never meet the host's own backups.
type CheckConfig struct {
	CycleDays int `json:"cycle_days"`
}

// ComputeSHA256 returns the sha256 of the host config without its Revision and SHA256 fields, so
// the hash describes what the agent runs and nothing else.
func (hc HostConfig) ComputeSHA256() string {
	c := hc
	c.Revision, c.SHA256 = 0, ""
	if c.Jobs == nil {
		c.Jobs = []JobConfig{}
	}
	b, err := json.Marshal(c)
	if err != nil {
		panic(err) // only JSON-safe fields
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Credentials are sent only when their version changes, and never logged.
type Credentials struct {
	Version           int    `json:"version"`
	RepoPassword      string `json:"repo_password"`
	S3AccessKeyID     string `json:"s3_access_key_id"`
	S3SecretAccessKey string `json:"s3_secret_access_key"`
}

// Command kinds.
const (
	CommandBackupNow = "backup_now"
	CommandCheckNow  = "check_now" // `holdfast check`: run the repository check now
)

// Command is work queued for the agent. Commands are re-sent until acknowledged; the agent
// deduplicates them by ID.
type Command struct {
	ID    string `json:"id"`
	Kind  string `json:"kind"`
	JobID string `json:"job_id,omitempty"`
}

// PollResponse answers a poll. Config and Credentials are present only when they changed.
type PollResponse struct {
	Config      *HostConfig  `json:"config,omitempty"`
	Credentials *Credentials `json:"credentials,omitempty"`
	Commands    []Command    `json:"commands,omitempty"`
	ServerTime  time.Time    `json:"server_time"`
}

// Run statuses. Every run ends in exactly one of these, and it is always reported.
const (
	StatusSuccess     = "success"
	StatusWarning     = "warning"     // restic exit 3: snapshot written, some files unreadable
	StatusFailed      = "failed"      // no snapshot
	StatusInterrupted = "interrupted" // the agent died mid-run; found at the next start
	StatusSkipped     = "skipped"     // the slot came while the previous run of the job was still going
)

// Event kinds.
const (
	KindBackup = "backup"
	KindCheck  = "check" // restic check of the host's repository, run by the agent
)

// Event is the final result of one run. The agent spools it on disk until the server acknowledges it.
type Event struct {
	ID           string     `json:"id"`
	Kind         string     `json:"kind"`
	JobID        string     `json:"job_id,omitempty"`
	CommandID    string     `json:"command_id,omitempty"`
	Status       string     `json:"status"`
	ScheduledFor *time.Time `json:"scheduled_for,omitempty"`
	SpreadDelayS int        `json:"spread_delay_s,omitempty"`
	Started      time.Time  `json:"started"`
	Finished     time.Time  `json:"finished"`
	Revision     int64      `json:"revision"`
	SnapshotID   string     `json:"snapshot_id,omitempty"`
	FilesNew     int64      `json:"files_new,omitempty"`
	FilesChanged int64      `json:"files_changed,omitempty"`
	BytesAdded   int64      `json:"bytes_added,omitempty"`
	TotalFiles   int64      `json:"total_files,omitempty"`
	TotalBytes   int64      `json:"total_bytes,omitempty"`
	ResticExit   int        `json:"restic_exit"`
	Error        string     `json:"error,omitempty"`
}

// EventsRequest delivers spooled events.
type EventsRequest struct {
	Events []Event `json:"events"`
}

// EventsResponse lists the IDs the server stored, or already had. Re-sent events are acknowledged
// again and counted once.
type EventsResponse struct {
	Acked []string `json:"acked"`
}

// ErrorResponse is the body of every non-2xx answer on the agent protocol and the API.
type ErrorResponse struct {
	Error string `json:"error"`
}
