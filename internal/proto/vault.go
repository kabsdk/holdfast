package proto

import (
	"time"

	"github.com/kabsdk/holdfast/internal/config"
	"github.com/kabsdk/holdfast/internal/schedule"
)

// What holdfast-server and its off-site instance tell each other (decisions 027, 039 and 041). They run in one
// process, and these pass in-process: the off-site instance hands over its report and takes the feed every poll. M7's
// vault, another program, sent them over HTTPS; that protocol is gone.

// VaultSync is the off-site instance's report.
type VaultSync struct {
	Version string            `json:"version"`
	Hosts   []VaultHostReport `json:"hosts"`
}

// VaultHostReport is what the off-site instance has of one host.
type VaultHostReport struct {
	FQDN string `json:"fqdn"`
	// Copies are the copies held off-site now. From them and its own snapshots the server works out what is still
	// wanted, which its daily run holds back for up to 7 days (decision 039), whether the copies go on or not.
	Copies    []VaultCopy      `json:"copies"`
	Jobs      []VaultJobReport `json:"jobs"`
	LastCopy  *time.Time       `json:"last_copy,omitempty"`
	CopyError string           `json:"copy_error,omitempty"`
	Retired   bool             `json:"retired,omitempty"` // removed in primary; the copies stay
}

// VaultCopy is one copy off-site: its own ID, and the primary snapshot it was copied from.
type VaultCopy struct {
	ID       string    `json:"id"`
	Original string    `json:"original"`
	Time     time.Time `json:"time"` // the snapshot's, which a copy keeps
	JobID    string    `json:"job_id,omitempty"`
	Dump     bool      `json:"dump,omitempty"`
}

// VaultJobReport is one job as the off-site instance holds it.
type VaultJobReport struct {
	ID        string           `json:"id"`
	Retention config.Retention `json:"retention"` // the off-site retention
	Excluded  bool             `json:"excluded,omitempty"`
	Retired   bool             `json:"retired,omitempty"`
	Copies    int              `json:"copies"`
	NewestAt  *time.Time       `json:"newest_copy_at,omitempty"`
	Shorter   bool             `json:"shorter_than_primary,omitempty"` // O101
	// BehindS is how far the newest copy is behind the newest snapshot in primary, in seconds; the lag alert fires
	// past twice the job's longest gap (decision 039).
	BehindS int64 `json:"behind_s"`
}

// VaultFeed is the server's answer: what is copied, and from where.
type VaultFeed struct {
	ServerTime time.Time `json:"server_time"`
	// Source is the base of the loopback endpoint that serves the primary repositories, read-only, to the copier: a
	// host's repository is Source + its FQDN + "/". It carries its own secret.
	Source string      `json:"-"`
	Hosts  []VaultHost `json:"hosts"`
	// Access is the server's bindings: the off-site pages answer to the same people as the rest (decision 041).
	Access config.Access `json:"access"`
}

// VaultHost is one host of the server, with its repository's password.
type VaultHost struct {
	FQDN         string     `json:"fqdn"`
	Repository   string     `json:"repository"`
	RepoPassword string     `json:"repo_password"`
	Jobs         []VaultJob `json:"jobs"`
	// DeletedInPrimary are the snapshots Holdfast deleted in primary on request, and when (decision 031).
	DeletedInPrimary map[string]time.Time `json:"deleted_in_primary"`
}

// VaultJob is one job of a host, as it runs.
type VaultJob struct {
	ID        string            `json:"id"`
	Paths     []string          `json:"paths,omitempty"`
	Excludes  []string          `json:"excludes,omitempty"`
	Dump      string            `json:"dump,omitempty"`
	Schedule  schedule.Schedule `json:"schedule"`
	Retention config.Retention  `json:"retention"` // the primary retention
}
