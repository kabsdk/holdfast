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

// Protocol is the version of the agent protocol this build speaks (plan-m5 item 1). It goes up only when
// a change needs both sides to know of it; a field the other side may ignore does not change it. Agents
// before M5 sent none, and speak 1.
const Protocol = 1

// MinProtocol is the oldest agent protocol this server accepts. A server can then be upgraded before its
// agents, and the agents one by one. An older agent's polls are refused with 426 and the range; it keeps
// backing up on its cached config, and its reports are still taken.
const MinProtocol = 1

// ProtocolRange is what the server accepts, sent with every answer to a poll.
type ProtocolRange struct {
	Min int `json:"min"`
	Max int `json:"max"`
}

// TooOld is the body of a 426 answer to an agent older than the server accepts.
type TooOld struct {
	Error    string        `json:"error"`
	Protocol ProtocolRange `json:"protocol"`
}

// Facts describe the host. The agent sends them with enrollment and with every poll.
type Facts struct {
	// Protocol is the agent's protocol version; 0 from an agent before M5, which speaks 1.
	Protocol      int       `json:"protocol,omitempty"`
	AgentVersion  string    `json:"agent_version"`
	ResticVersion string    `json:"restic_version"`
	OSID          string    `json:"os_id"`         // os-release ID: ubuntu, rhel, ol
	OSVersion     string    `json:"os_version"`    // os-release VERSION_ID
	HostnameFQDN  string    `json:"hostname_fqdn"` // what `hostname -f` says
	Timezone      string    `json:"timezone"`
	Now           time.Time `json:"now"` // the agent's clock, so the server can see skew
	// RestoreBounds are the host's own limits on restores, from agent.yaml. The server shows them and
	// refuses early, but the agent decides (decision 030, plan-m2 Q5).
	RestoreBounds *RestoreBounds `json:"restore_bounds,omitempty"`
}

// RestoreBounds limit what a restore may do on this host. Only root on the host can change them.
type RestoreBounds struct {
	Root                  string `json:"root" yaml:"root"`                                       // new directories go under it
	AllowInPlace          bool   `json:"allow_in_place" yaml:"allow_in_place"`                   // overwrite the original files
	AllowOtherDirectories bool   `json:"allow_other_directories" yaml:"allow_other_directories"` // any other target directory
}

// Restore targets.
const (
	TargetNew       = "new"       // a new root-only directory under the bounds' root (the default)
	TargetInPlace   = "in_place"  // the original location, after a safety backup
	TargetDirectory = "directory" // another directory
)

// RestoreSpec is a restore the server asks the agent to run (spec §5.4).
type RestoreSpec struct {
	Snapshot  string   `json:"snapshot"` // the full snapshot ID
	Target    string   `json:"target"`
	Directory string   `json:"directory,omitempty"` // for TargetDirectory
	Exclude   []string `json:"exclude,omitempty"`   // what was left out
	Include   []string `json:"include,omitempty"`   // "restore only this"
	DryRun    bool     `json:"dry_run,omitempty"`
	JobID     string   `json:"job_id,omitempty"` // the snapshot's job, for the in-place safety backup
	User      string   `json:"user"`             // who asked, for the default directory's name
	// Overwrite is restic's --overwrite for files that exist already: always, if-changed, if-newer or never.
	// Empty: restic's default. An agent that does not know the field ignores it, and restic's default holds.
	Overwrite string `json:"overwrite,omitempty"`
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
	// LimitUploadKiB is restic's --limit-upload, in KiB/s; 0: no limit (plan-m5 item 2).
	LimitUploadKiB int `json:"limit_upload_kib,omitempty"`
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
	CommandRestore   = "restore"   // run a restore (plan-m2 §3)
)

// Command is work queued for the agent. Commands are re-sent until acknowledged; the agent
// deduplicates them by ID.
type Command struct {
	ID      string       `json:"id"`
	Kind    string       `json:"kind"`
	JobID   string       `json:"job_id,omitempty"`
	Restore *RestoreSpec `json:"restore,omitempty"`
}

// PollResponse answers a poll. Config and Credentials are present only when they changed.
type PollResponse struct {
	Config      *HostConfig    `json:"config,omitempty"`
	Credentials *Credentials   `json:"credentials,omitempty"`
	Commands    []Command      `json:"commands,omitempty"`
	ServerTime  time.Time      `json:"server_time"`
	Protocol    *ProtocolRange `json:"protocol,omitempty"` // what the server accepts; nil from a server before M5
}

// AgentProtocol is the protocol an agent speaks, from its facts.
func (f Facts) AgentProtocol() int {
	if f.Protocol == 0 {
		return 1
	}
	return f.Protocol
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
	KindBackup     = "backup"
	KindCheck      = "check"       // restic check of the host's repository, run by the agent
	KindNotice     = "notice"      // something the agent found and repaired, such as changed credential files
	KindRestore    = "restore"     // a restore, or its dry run
	KindPreRestore = "pre_restore" // the safety backup an in-place restore takes first
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
	// Detail carries what a kind needs beyond the common fields, such as a restore's target and its
	// safety backup. Never a secret, never file contents.
	Detail map[string]string `json:"detail,omitempty"`
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
