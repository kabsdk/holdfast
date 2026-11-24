// Package config is the configuration document stored in every revision (spec §3.2): hosts, jobs,
// library and access. It never holds credentials.
//
// Canonical returns the bytes a revision stores and hashes. They are deterministic: struct fields
// keep their order and map keys are sorted, so the same config always has the same sha256.
package config

import (
	"crypto/sha256"
	"encoding/json"

	"github.com/kabsdk/holdfast/internal/schedule"
)

// SchemaVersion is the version of Document this code writes.
const SchemaVersion = 1

// Document is the full configuration in one revision.
type Document struct {
	Schema  int             `json:"schema"`
	Hosts   map[string]Host `json:"hosts"` // key: FQDN
	Library Library         `json:"library"`
	Access  Access          `json:"access"`
}

// Host is one agent, identified by its name (the map key in Document.Hosts): the agent_name in its agent.yaml,
// which may be a machine's FQDN or any other name (decision 056). Holdfast called them hosts until 0.1.0; the
// document keeps the word.
type Host struct {
	Jobs map[string]Job `json:"jobs"` // key: job ID
	// PostgreSQL are the agent's PostgreSQL connections, by name (decision 055). Their passwords are not here.
	PostgreSQL map[string]PGConnection `json:"postgresql,omitempty"`
}

// Job is one backup job on one host.
type Job struct {
	Engine    string            `json:"engine"` // "restic": paths; "postgresql": a database (decision 055)
	Paths     []string          `json:"paths"`
	Excludes  []string          `json:"excludes,omitempty"`
	Schedule  schedule.Schedule `json:"schedule"`
	Retention Retention         `json:"retention"` // stored from M1; acted on from M3
	// LimitUploadKiB caps the backup's upload, in KiB/s: restic's --limit-upload. 0: no limit (M5).
	LimitUploadKiB int `json:"limit_upload_kib,omitempty"`
	// Dump named a command in the host's agent.yaml whose output the job backed up, until 0.6.0 removed dump
	// jobs: Holdfast runs no command on a host (decision 052). It is kept so that a revision from before reads, and
	// a job that still has it is refused with a reason; snapshots it made are restored as any other.
	Dump string `json:"dump,omitempty"`
	// PostgreSQL is what a job of the postgresql engine backs up (decision 055); nil for any other.
	PostgreSQL *PGJob `json:"postgresql,omitempty"`

	// In a vault only (decisions 027 and 039): Retention is then the off-site retention, the vault's own;
	// Primary is the source's, which the vault shows and never sets; Excluded stops copying and keeps the
	// data; Retired is a job the source removed, whose copies the vault keeps under its retention.
	Primary  *Retention `json:"primary,omitempty"`
	Excluded bool       `json:"excluded,omitempty"`
	Retired  bool       `json:"retired,omitempty"`
	// OwnRetention says the off-site retention was set off-site, and stays as it is. Without it, the off-site
	// retention is primary's, and follows every change there (decision 053).
	OwnRetention bool `json:"own_retention,omitempty"`
}

// IsDump says whether the job is a dump job from before 0.6.0 (decision 052).
func (j Job) IsDump() bool { return j.Dump != "" }

// Library holds paths items, protection items and profiles (M5). Until then the entries are kept
// opaque so a document round-trips unchanged.
type Library struct {
	Paths      map[string]json.RawMessage `json:"paths"`
	Protection map[string]json.RawMessage `json:"protection"`
	Profiles   map[string]json.RawMessage `json:"profiles"`
}

// Access holds the role bindings (spec §6.2, §6.3).
type Access struct {
	Bindings []Binding `json:"bindings"`
}

// Binding grants a role to a group on the hosts that match any of Hosts ("*" matches all).
type Binding struct {
	Group string   `json:"group"`
	Role  string   `json:"role"` // viewer, editor or admin
	Hosts []string `json:"hosts"`
}

// EngineRestic is the only engine today (decision 001).
const EngineRestic = "restic"

// DefaultAccess returns the default bindings of spec §6.3.
func DefaultAccess() Access {
	all := []string{"*"}
	return Access{Bindings: []Binding{
		{Group: "holdfast-admins", Role: "admin", Hosts: all},
		{Group: "holdfast-editors", Role: "editor", Hosts: all},
		{Group: "holdfast-viewers", Role: "viewer", Hosts: all},
	}}
}

// DefaultJob is where a new job starts. With paths, it passes every check with no warnings:
// daily at 02:00 with a 20-minute spread; one per day for 14 days, per week for 8 weeks, per month for 12
// months and per year for 3 years; and always the newest 3.
func DefaultJob() Job {
	return Job{Engine: EngineRestic, Paths: []string{},
		Schedule: schedule.Schedule{Daily: &schedule.Daily{Times: []schedule.ClockTime{"02:00"}}, Spread: schedule.Spread{Enabled: true, Minutes: 20}},
		Retention: Retention{Daily: &Duration{14, Days}, Weekly: &Duration{8, Weeks}, Monthly: &Duration{12, Months}, Yearly: &Duration{3, Years},
			KeepLast: DefaultKeepLast}}
}

// New returns the empty document an installation starts from.
func New() Document {
	d := Document{Schema: SchemaVersion, Access: DefaultAccess()}
	d.normalise()
	return d
}

// normalise replaces nil maps with empty ones, so an empty and a missing map encode the same.
func (d *Document) normalise() {
	if d.Hosts == nil {
		d.Hosts = map[string]Host{}
	}
	for fqdn, h := range d.Hosts {
		if h.Jobs == nil {
			h.Jobs = map[string]Job{}
			d.Hosts[fqdn] = h
		}
	}
	if d.Library.Paths == nil {
		d.Library.Paths = map[string]json.RawMessage{}
	}
	if d.Library.Protection == nil {
		d.Library.Protection = map[string]json.RawMessage{}
	}
	if d.Library.Profiles == nil {
		d.Library.Profiles = map[string]json.RawMessage{}
	}
	if d.Access.Bindings == nil {
		d.Access.Bindings = []Binding{}
	}
}

// Canonical returns the deterministic encoding a revision stores.
func (d Document) Canonical() ([]byte, error) {
	c := d.Clone()
	c.normalise()
	return json.Marshal(c)
}

// SHA256 returns the hash of Canonical.
func (d Document) SHA256() ([32]byte, error) {
	b, err := d.Canonical()
	if err != nil {
		return [32]byte{}, err
	}
	return sha256.Sum256(b), nil
}

// Clone returns a deep copy.
func (d Document) Clone() Document {
	b, err := json.Marshal(d)
	if err != nil {
		panic(err) // Document only holds JSON-safe values
	}
	var c Document
	if err := json.Unmarshal(b, &c); err != nil {
		panic(err)
	}
	return c
}

// Parse decodes a stored document. Unknown fields are refused, so a document written by a newer
// version is never half-read.
func Parse(b []byte) (Document, error) {
	var d Document
	dec := json.NewDecoder(bytesReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&d); err != nil {
		return Document{}, err
	}
	d.normalise()
	return d, nil
}
