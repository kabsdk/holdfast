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

// ValidFQDN checks a host identity (plan-m1 Q9): lower case, no trailing dot, letters, digits and
// hyphens, at least two labels, at most 253 characters.
func ValidFQDN(s string) error {
	switch {
	case s == "":
		return errors.New("the FQDN is empty")
	case len(s) > 253:
		return errors.New("the FQDN is longer than 253 characters")
	case s != strings.ToLower(s):
		return errors.New("the FQDN must be lower case")
	case strings.HasSuffix(s, "."):
		return errors.New("the FQDN must not end with a dot")
	}
	labels := strings.Split(s, ".")
	if len(labels) < 2 {
		return errors.New("the FQDN needs at least two labels, such as web01.example.com")
	}
	for _, l := range labels {
		if !labelRE.MatchString(l) {
			return fmt.Errorf("label %q is not valid: use letters, digits and hyphens, not at the start or end, at most 63 characters", l)
		}
	}
	return nil
}

// ValidJobID checks a job ID (plan-m1 Q12): an immutable slug, because it is the restic tag job=<id>.
func ValidJobID(s string) error {
	if !jobIDRE.MatchString(s) {
		return fmt.Errorf("job ID %q is not valid: use lower-case letters, digits and hyphens, starting with a letter or digit, at most 63 characters", s)
	}
	return nil
}

// ValidDumpName checks a dump's name (decision 037), as agent.yaml and a job both write it.
func ValidDumpName(s string) error {
	if !jobIDRE.MatchString(s) {
		return fmt.Errorf("dump name %q is not valid: use lower-case letters, digits and hyphens, at most 63 characters", s)
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
		if err := ValidFQDN(fqdn); err != nil {
			add(hp, "%v", err)
		}
		for _, id := range sortedKeys(d.Hosts[fqdn].Jobs) {
			jp := fmt.Sprintf("%s.jobs[%q]", hp, id)
			if err := ValidJobID(id); err != nil {
				add(jp, "%v", err)
			}
			j := d.Hosts[fqdn].Jobs[id]
			if j.Engine != EngineRestic {
				add(jp+".engine", "engine %q is not supported; restic is the only engine", j.Engine)
			}
			switch {
			case j.IsDump() && (len(j.Paths) > 0 || len(j.Excludes) > 0):
				add(jp+".dump", "a job backs up either paths or a dump, not both")
			case j.IsDump():
				if err := ValidDumpName(j.Dump); err != nil {
					add(jp+".dump", "%v", err)
				}
			case len(j.Paths) == 0:
				add(jp+".paths", "at least one path is needed, or a dump")
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
