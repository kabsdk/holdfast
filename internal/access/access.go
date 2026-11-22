// Package access decides who may do what on which host (spec §6.2, decisions 025, 033 and 035).
//
// There are three roles, each a flat set of permissions, with no inheritance and no custom roles. A
// binding grants a role to a group on host patterns, and several bindings combine per host. A host that no
// binding of yours covers does not exist for you. The server checks every API call against this package;
// the UI and the CLI only show what it allows.
package access

import (
	"fmt"
	"path"
	"regexp"
	"slices"

	"github.com/kabsdk/holdfast/internal/config"
)

// Permission names (spec §6.2).
const (
	View           = "view"             // hosts, jobs, schedule, retention, status, history, problems
	Browse         = "browse"           // file names and metadata: Files, Changes, Find, restore paths
	BackupRun      = "backup.run"       // back up now
	RestoreNewDir  = "restore.new_dir"  // restore into a new directory under the host's restore root
	ConfigJobs     = "config.jobs"      // add, change and check jobs; Keep longer
	RestoreInPlace = "restore.in_place" // restore in place, or into another directory
	RetentionCut   = "retention.cut"    // accept R104, Prune now, run retention now
	SnapshotDelete = "snapshot.delete"  // delete a snapshot, Delete now, cancel a deletion
	ConfigAccess   = "config.access"    // bindings and sessions
	ConfigStorage  = "config.storage"   // add a host, its S3 key, enrollment codes
	Maintenance    = "maintenance"      // acknowledge changes made outside Holdfast, holdfast check
	HostRemove     = "host.remove"      // remove a host or a job; never deletes data
	// HostDecommission deletes a removed host's repository, after a 7-day wait (plan-m5 item 7).
	HostDecommission = "host.decommission"
	// RepositoryRollback rolls a host's repository back to a point in time from S3's versions (decision 038).
	RepositoryRollback = "repository.rollback"
)

// Roles are the three roles and their permissions.
var Roles = map[string][]string{
	"viewer": {View},
	"editor": {View, Browse, BackupRun, RestoreNewDir, ConfigJobs},
	"admin": {View, Browse, BackupRun, RestoreNewDir, ConfigJobs, RestoreInPlace, RetentionCut, SnapshotDelete,
		ConfigAccess, ConfigStorage, Maintenance, HostRemove, HostDecommission, RepositoryRollback},
}

// All lists every permission, in the order of spec §6.2.
var All = Roles["admin"]

var patternChars = regexp.MustCompile(`^[a-z0-9.*-]+$`)

// ValidPattern checks a binding's host pattern: an FQDN, or one with * standing for any characters, such as
// "*" or "*.dc1.example.com".
func ValidPattern(p string) error {
	if !patternChars.MatchString(p) {
		return fmt.Errorf("host pattern %q may hold only lower-case letters, digits, dots, dashes and *", p)
	}
	return nil
}

// Matches says whether a host pattern covers an FQDN.
func Matches(pattern, fqdn string) bool {
	ok, err := path.Match(pattern, fqdn)
	return err == nil && ok
}

// Principal is who makes a request.
type Principal struct {
	Name    string   `json:"name"`              // the username; local:<user> on the root socket
	Subject string   `json:"subject,omitempty"` // the OIDC sub
	Source  string   `json:"source"`            // ui, cli, api or socket
	Groups  []string `json:"groups"`
	// Everywhere is admin on every host, whatever the bindings: root on the server's socket, or a user in
	// auth.bootstrap_admins (decision 034).
	Everywhere bool `json:"everywhere,omitempty"`
}

// On returns the permissions p has on a host under the bindings, in the order of All. Empty means the host
// does not exist for p.
func (p Principal) On(fqdn string, bindings []config.Binding) []string {
	if p.Everywhere {
		return slices.Clone(All)
	}
	have := map[string]bool{}
	for _, b := range bindings {
		if !slices.Contains(p.Groups, b.Group) {
			continue
		}
		for _, pat := range b.Hosts {
			if Matches(pat, fqdn) {
				for _, perm := range Roles[b.Role] {
					have[perm] = true
				}
				break
			}
		}
	}
	out := []string{}
	for _, perm := range All {
		if have[perm] {
			out = append(out, perm)
		}
	}
	return out
}

// Can says whether p has perm on a host.
func (p Principal) Can(perm, fqdn string, bindings []config.Binding) bool {
	return slices.Contains(p.On(fqdn, bindings), perm)
}

// Sees says whether the host exists for p.
func (p Principal) Sees(fqdn string, bindings []config.Binding) bool {
	return len(p.On(fqdn, bindings)) > 0
}

// AdminEverywhere says whether p is admin on every host: by Everywhere, or by an admin binding on "*". Only
// they see what belongs to no host, such as global audit entries, and everything in a revision.
func (p Principal) AdminEverywhere(bindings []config.Binding) bool {
	if p.Everywhere {
		return true
	}
	for _, b := range bindings {
		if b.Role == "admin" && slices.Contains(p.Groups, b.Group) && slices.Contains(b.Hosts, "*") {
			return true
		}
	}
	return false
}
