package config

import (
	"bytes"
	"errors"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"
)

func bytesReader(b []byte) *bytes.Reader { return bytes.NewReader(b) }

// Problem is one structural problem in a document, located by a path such as
// hosts["web01.example.com"].jobs["system"].schedule.
type Problem struct {
	Path    string `json:"path"`
	Message string `json:"message"`
}

// Problems is every structural problem found; it is an error when not empty.
type Problems []Problem

func (p Problems) Error() string {
	parts := make([]string, len(p))
	for i, x := range p {
		parts[i] = x.Path + ": " + x.Message
	}
	return strings.Join(parts, "; ")
}

// Err returns nil when there are no problems.
func (p Problems) Err() error {
	if len(p) == 0 {
		return nil
	}
	return p
}

var (
	labelRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	jobIDRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	// hostPattern is a binding's host pattern: an FQDN, with * for any characters (internal/access).
	hostPattern = regexp.MustCompile(`^[a-z0-9.*-]+$`)
)

// ValidAgentName checks an agent's name, its identity in Holdfast and its repository's prefix (decision 056): lower
// case letters, digits and hyphens, in one or more dot-separated parts, at most 253 characters. A machine's FQDN is
// one, such as web01.example.com; so is matrix, or k3s-postgres-main.
func ValidAgentName(s string) error {
	switch {
	case s == "":
		return errors.New("the agent's name is empty")
	case len(s) > 253:
		return errors.New("the agent's name is longer than 253 characters")
	case s != strings.ToLower(s):
		return errors.New("the agent's name must be lower case")
	case strings.HasSuffix(s, ".") || strings.HasPrefix(s, "."):
		return errors.New("the agent's name must not start or end with a dot")
	}
	for _, l := range strings.Split(s, ".") {
		if !labelRE.MatchString(l) {
			return fmt.Errorf("%q in the agent's name is not valid: use letters, digits and hyphens, not at the start or end of a part, at most 63 characters a part", l)
		}
	}
	return nil
}

// ValidJobID checks a job ID: an immutable slug, because it is the restic tag job=<id>.
func ValidJobID(s string) error {
	if !jobIDRE.MatchString(s) {
		return fmt.Errorf("job ID %q is not valid: use lower-case letters, digits and hyphens, starting with a letter or digit, at most 63 characters", s)
	}
	return nil
}

// Validate returns every structural problem in the document. The checks engine (M3) adds the
// R, S and I codes on top of this.
func (d Document) Validate() Problems {
	var out Problems
	add := func(p, f string, a ...any) { out = append(out, Problem{Path: p, Message: fmt.Sprintf(f, a...)}) }
	if d.Schema != SchemaVersion {
		add("schema", "schema %d is not supported (this version writes %d)", d.Schema, SchemaVersion)
	}
	for _, fqdn := range sortedKeys(d.Hosts) {
		hp := fmt.Sprintf("hosts[%q]", fqdn)
		if err := ValidAgentName(fqdn); err != nil {
			add(hp, "%v", err)
		}
		conns := d.Hosts[fqdn].PostgreSQL
		for _, name := range sortedKeys(conns) {
			cp := fmt.Sprintf("%s.postgresql[%q]", hp, name)
			if err := ValidConnectionName(name); err != nil {
				add(cp, "%v", err)
			}
			for _, m := range conns[name].Validate() {
				add(cp, "%s", m)
			}
		}
		targets := map[string]string{}
		for _, id := range sortedKeys(d.Hosts[fqdn].Jobs) {
			jp := fmt.Sprintf("%s.jobs[%q]", hp, id)
			if err := ValidJobID(id); err != nil {
				add(jp, "%v", err)
			}
			j := d.Hosts[fqdn].Jobs[id]
			switch j.Engine {
			case EngineRestic:
				if j.PostgreSQL != nil {
					add(jp+".postgresql", "a job of the restic engine backs up paths; a database is a job of the postgresql engine")
				}
			case EnginePostgreSQL:
				p := j.PostgreSQL
				switch {
				case p == nil:
					add(jp+".postgresql", "a postgresql job names a connection and a database, or the globals")
				case len(j.Paths) > 0 || len(j.Excludes) > 0 || j.IsDump():
					add(jp+".paths", "a postgresql job backs up a database, not paths")
				case p.Globals && p.Database != "":
					add(jp+".postgresql", "a job backs up one database, or the roles and other globals, not both")
				default:
					if _, ok := conns[p.Connection]; !ok {
						add(jp+".postgresql.connection", "%s has no PostgreSQL connection named %q", fqdn, p.Connection)
					}
					if !p.Globals {
						if err := ValidDatabaseName(p.Database); err != nil {
							add(jp+".postgresql.database", "%v", err)
						}
					}
					if other, ok := targets[p.target()]; ok {
						add(jp+".postgresql", "job %s backs up the same already", other)
					}
					targets[p.target()] = id
				}
			default:
				add(jp+".engine", "engine %q is not supported: restic (paths) or postgresql (a database)", j.Engine)
			}
			switch {
			case j.IsDump(), j.IsPostgreSQL():
				// A dump job from before 0.6.0 (decision 052) stays valid here, so one left behind does not stop the
				// rest of the config from changing. Saving it is refused (the server's errDumpJob), and agents fail
				// its runs. A PostgreSQL job has no paths.
			case len(j.Paths) == 0:
				add(jp+".paths", "at least one path is needed")
			}
			for _, p := range j.Paths {
				if err := validPath(p); err != nil {
					add(jp+".paths", "%v", err)
				}
			}
			for _, e := range j.Excludes {
				if strings.TrimSpace(e) == "" || strings.ContainsRune(e, 0) {
					add(jp+".excludes", "exclude %q is empty or contains a NUL byte", e)
				}
			}
			if err := j.Schedule.Validate(); err != nil {
				add(jp+".schedule", "%v", err)
			}
			if j.LimitUploadKiB < 0 {
				add(jp+".limit_upload_kib", "the upload limit is in KiB/s, 0 for none; it cannot be negative")
			}
			for _, m := range j.Retention.validate() {
				add(jp+".retention", "%s", m)
			}
		}
	}
	for i, b := range d.Access.Bindings {
		bp := fmt.Sprintf("access.bindings[%d]", i)
		if b.Group == "" {
			add(bp, "group is empty")
		}
		switch b.Role {
		case "viewer", "editor", "admin":
		default:
			add(bp, "role %q is not viewer, editor or admin", b.Role)
		}
		if len(b.Hosts) == 0 {
			add(bp, "hosts is empty; use [\"*\"] for every host")
		}
		for _, h := range b.Hosts {
			if !hostPattern.MatchString(h) {
				add(bp+".hosts", "host pattern %q may hold only lower-case letters, digits, dots, dashes and *", h)
			}
		}
	}
	return out
}

func validPath(p string) error {
	switch {
	case !strings.HasPrefix(p, "/"):
		return fmt.Errorf("path %q is not absolute", p)
	case strings.ContainsRune(p, 0):
		return fmt.Errorf("path %q contains a NUL byte", p)
	case path.Clean(p) != p:
		return fmt.Errorf("path %q is not clean; did you mean %q?", p, path.Clean(p))
	}
	return nil
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
