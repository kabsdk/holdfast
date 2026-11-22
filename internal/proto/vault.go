package proto

import (
	"time"

	"github.com/kabsdk/holdfast/internal/config"
	"github.com/kabsdk/holdfast/internal/schedule"
)

// The vault protocol (decisions 027 and 039). A vault, in the other site, opens every connection: it posts its
// report to its source holdfast-server and gets the feed back, and posts each restore grant. Nothing in the source
// site connects to the vault.

// VaultBase is the path prefix of the vault protocol, on the agent listener.
const VaultBase = "/vault/v1"

// VaultSync is what a vault posts every few minutes: its report.
type VaultSync struct {
	Version string            `json:"version"`
	Hosts   []VaultHostReport `json:"hosts"`
}

// VaultHostReport is what the vault has of one host.
type VaultHostReport struct {
	FQDN string `json:"fqdn"`
	// Copies are the copies the vault holds now. From them and its own snapshots the source works out what the vault
	// still wants, which its daily run holds back for up to 7 days (decision 039), whether the vault copies or not.
	Copies    []VaultCopy      `json:"copies"`
	Jobs      []VaultJobReport `json:"jobs"`
	LastCopy  *time.Time       `json:"last_copy,omitempty"`
	CopyError string           `json:"copy_error,omitempty"`
	Retired   bool             `json:"retired,omitempty"` // removed in the source; the vault keeps its data
}

// VaultCopy is one copy in the vault: its own ID, and the source snapshot it was copied from.
type VaultCopy struct {
	ID       string    `json:"id"`
	Original string    `json:"original"`
	Time     time.Time `json:"time"` // the snapshot's, which a copy keeps
	JobID    string    `json:"job_id,omitempty"`
	Dump     bool      `json:"dump,omitempty"`
}

// VaultJobReport is one job as the vault holds it.
type VaultJobReport struct {
	ID        string           `json:"id"`
	Retention config.Retention `json:"retention"` // the off-site retention, the vault's to set
	Excluded  bool             `json:"excluded,omitempty"`
	Retired   bool             `json:"retired,omitempty"`
	Copies    int              `json:"copies"`
	NewestAt  *time.Time       `json:"newest_copy_at,omitempty"`
	Shorter   bool             `json:"shorter_than_primary,omitempty"` // O101
	// BehindS is how far the newest copy is behind the newest snapshot in the source, in seconds; the lag alert
	// fires past twice the job's longest gap (decision 039).
	BehindS int64 `json:"behind_s"`
}

// VaultFeed is the server's answer: what the vault copies, how to read it, and the restores it is asked to grant.
type VaultFeed struct {
	ServerTime time.Time `json:"server_time"`
	// Source is the source site's bucket and the read-only key for its repos/, which the admin gave when the vault
	// was registered.
	Source   VaultSource    `json:"source"`
	Hosts    []VaultHost    `json:"hosts"`
	Requests []VaultRequest `json:"requests"`
}

// VaultSource is the source site's storage, as the vault reads it.
type VaultSource struct {
	Endpoint          string `json:"endpoint"`
	Bucket            string `json:"bucket"`
	Region            string `json:"region"`
	S3AccessKeyID     string `json:"s3_access_key_id"`
	S3SecretAccessKey string `json:"s3_secret_access_key"`
}

// VaultHost is one host of the source, with its repository's password.
type VaultHost struct {
	FQDN         string     `json:"fqdn"`
	Repository   string     `json:"repository"`
	RepoPassword string     `json:"repo_password"`
	Jobs         []VaultJob `json:"jobs"`
	// DeletedInPrimary are the snapshots Holdfast deleted in the source, and when (decision 031).
	DeletedInPrimary map[string]time.Time `json:"deleted_in_primary"`
}

// VaultJob is one job of a host, as the source runs it.
type VaultJob struct {
	ID        string            `json:"id"`
	Paths     []string          `json:"paths,omitempty"`
	Excludes  []string          `json:"excludes,omitempty"`
	Dump      string            `json:"dump,omitempty"`
	Schedule  schedule.Schedule `json:"schedule"`
	Retention config.Retention  `json:"retention"` // the primary retention
}

// VaultRequest asks the vault to grant a restore from one of its copies.
type VaultRequest struct {
	ID       string `json:"id"`
	FQDN     string `json:"fqdn"`
	Snapshot string `json:"snapshot"` // the copy's ID
	By       string `json:"requested_by"`
}

// VaultGrant is the vault's answer to a request: the copy's repository, its password and a read-only key for it,
// or why not. The source sends it, sealed, to the restoring agent, once.
type VaultGrant struct {
	ID                string `json:"id"`
	Refused           string `json:"refused,omitempty"`
	Repository        string `json:"repository,omitempty"`
	RepoPassword      string `json:"repo_password,omitempty"`
	S3AccessKeyID     string `json:"s3_access_key_id,omitempty"`
	S3SecretAccessKey string `json:"s3_secret_access_key,omitempty"`
}
