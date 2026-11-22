// Package gateway is restic's REST protocol in front of the bucket (decision 041). Agents back up to it instead of
// to S3, so no host holds an S3 key: holdfast-server holds one gateway key per bucket, and decides here what each host
// may do.
//
// A host reads and adds to its own repository. It never deletes anything but its own lock files, never rewrites a
// file that exists, and never writes the repository's config or keys: the server made those. For a restore onto
// another host, the server lets that host read one other repository for a while (a grant), and nothing more.
package gateway

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"github.com/kabsdk/holdfast/internal/config"
)

// Base is where the gateway is served, on the agent listener. A host's repository is Base + its FQDN + "/".
const Base = "/gateway/v1/"

// RepoURL is the restic repository URL of a host's repository at a server.
func RepoURL(server, fqdn string) string {
	return "rest:" + strings.TrimSuffix(server, "/") + Base + fqdn + "/"
}

// Op is what a request does.
type Op string

// The operations of restic's REST protocol.
const (
	OpStat   Op = "stat"   // HEAD a file
	OpRead   Op = "read"   // GET a file
	OpList   Op = "list"   // GET a type's directory
	OpWrite  Op = "write"  // POST a file
	OpDelete Op = "delete" // DELETE a file
	OpCreate Op = "create" // POST /?create=true, which restic init sends
)

// The file types of a restic repository. Config is one file; the others are directories of files named by their ID.
const (
	KindConfig    = "config"
	KindData      = "data"
	KindKeys      = "keys"
	KindLocks     = "locks"
	KindSnapshots = "snapshots"
	KindIndex     = "index"
)

var kinds = map[string]bool{KindData: true, KindKeys: true, KindLocks: true, KindSnapshots: true, KindIndex: true}

// id is a restic file's name: the sha256 of its content, in lower-case hex.
var id = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Request is one parsed gateway request.
type Request struct {
	Repo string // the host whose repository it is
	Kind string // a file type; "" for create
	Name string // the file's ID; "" for config, a list and create
	Op   Op
}

// Key is the request's object in the bucket: repos/<fqdn>/ as restic's S3 backend lays it out, with data files in
// subdirectories by their first two characters. A list's key is its directory, with a slash.
func (r Request) Key() string {
	p := "repos/" + r.Repo + "/"
	switch {
	case r.Kind == KindConfig:
		return p + "config"
	case r.Name == "":
		return p + r.Kind + "/"
	case r.Kind == KindData:
		return p + KindData + "/" + r.Name[:2] + "/" + r.Name
	default:
		return p + r.Kind + "/" + r.Name
	}
}

// ErrMalformed is a request that is not restic's REST protocol, or names something no repository holds.
var ErrMalformed = errors.New("not a request of restic's REST protocol for a Holdfast repository")

// Parse reads a request from its method, its path as sent (escaped) and its query. It takes nothing it does not
// know: every part of the path is checked against what restic sends, so no path trick reaches the bucket.
func Parse(method, escapedPath string, query url.Values) (Request, error) {
	rest, ok := strings.CutPrefix(escapedPath, Base)
	// Nothing restic sends is escaped: a % could only hide a slash or a dot.
	if !ok || strings.ContainsAny(rest, "%\\") {
		return Request{}, ErrMalformed
	}
	parts := strings.Split(rest, "/")
	if config.ValidFQDN(parts[0]) != nil {
		return Request{}, ErrMalformed
	}
	r := Request{Repo: parts[0]}
	switch {
	case len(parts) == 2 && parts[1] == "" && method == "POST" && query.Get("create") == "true":
		r.Op = OpCreate
		return r, nil
	case len(parts) == 2 && parts[1] == KindConfig:
		r.Kind = KindConfig
		return r, r.method(method, false)
	case len(parts) == 3 && kinds[parts[1]] && parts[2] == "" && method == "GET":
		r.Kind, r.Op = parts[1], OpList
		return r, nil
	case len(parts) == 3 && kinds[parts[1]] && id.MatchString(parts[2]):
		r.Kind, r.Name = parts[1], parts[2]
		return r, r.method(method, true)
	}
	return Request{}, ErrMalformed
}

func (r *Request) method(m string, deletable bool) error {
	switch m {
	case "HEAD":
		r.Op = OpStat
	case "GET":
		r.Op = OpRead
	case "POST":
		r.Op = OpWrite
	case "DELETE":
		if !deletable {
			return ErrMalformed
		}
		r.Op = OpDelete
	default:
		return ErrMalformed
	}
	return nil
}

// Access is what a host may do with a repository.
type Access int

// The kinds of access.
const (
	NoAccess Access = iota
	ReadOnly        // a grant: another host's repository, for a restore onto this host
	Own             // the host's own repository
)

// Refusal is a request the gateway refuses. Reason is stable: it labels the metric and the audit entry.
type Refusal struct {
	Reason  string
	Message string
}

func (r *Refusal) Error() string { return r.Message }

// The reasons for a refusal.
const (
	ReasonOtherRepository = "other_repository"
	ReasonReadOnly        = "read_only"
	ReasonDelete          = "delete"
	ReasonServerOwned     = "server_owned"
	ReasonMalformed       = "malformed"
)

// Check says whether a host with this access to the request's repository may make the request.
func Check(r Request, a Access) error {
	read := r.Op == OpStat || r.Op == OpRead || r.Op == OpList
	switch {
	case a == NoAccess:
		return &Refusal{ReasonOtherRepository, fmt.Sprintf("this host may not use the repository of %s", r.Repo)}
	case a == ReadOnly && !read:
		return &Refusal{ReasonReadOnly, fmt.Sprintf("this host may only read the repository of %s, for a restore", r.Repo)}
	case read:
		return nil
	case r.Op == OpDelete && r.Kind != KindLocks:
		return &Refusal{ReasonDelete, "a host deletes nothing but its own lock files: forget and prune are the server's"}
	case r.Op == OpCreate || r.Kind == KindConfig || r.Kind == KindKeys:
		return &Refusal{ReasonServerOwned, "the server creates the repository and holds its keys: a host does not write them"}
	}
	return nil
}
