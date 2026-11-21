// Package restic is a thin, explicit wrapper around the restic CLI.
//
// Holdfast never re-implements the repository format: every byte goes through restic, so a
// repository stays restorable with the plain restic binary if Holdfast is gone (decision 001).
//
// Rules this package enforces (CLAUDE.md):
//   - passwords are passed by file, never in argv;
//   - restic never inherits RESTIC_* or AWS_* variables from the parent process;
//   - unlock only ever removes stale locks (never --remove-all);
//   - there is no forget or prune here: nothing destructive exists before the 7-day protection (M3).
package restic

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// PinnedVersion is the restic that Holdfast is tested with (plan-m1 Q10). The agent's package ships it
// (decision 032), and the server warns about a host that reports another version. Keep it equal to
// RESTIC_VERSION in the Makefile.
const PinnedVersion = "0.19.1"

// DefaultBin is where the agent's and the server's packages put that restic (decision 032). It is the
// default of restic_bin in agent.yaml and server.yaml.
const DefaultBin = "/usr/libexec/holdfast/restic"

// Exit codes documented by restic.
const (
	ExitOK         = 0
	ExitFatal      = 1
	ExitIncomplete = 3 // snapshot written, but some source files could not be read
	ExitNoRepo     = 10
	ExitLocked     = 11
	ExitBadPass    = 12
)

// Runner runs restic against exactly one repository.
type Runner struct {
	Bin          string        // path to restic; "restic" on PATH when empty
	Repo         string        // RESTIC_REPOSITORY
	PasswordFile string        // RESTIC_PASSWORD_FILE
	CacheDir     string        // RESTIC_CACHE_DIR; restic's default when empty
	Env          []string      // extra variables, e.g. AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY
	RetryLock    time.Duration // --retry-lock; 0 means fail at once when locked
	// NoLock reads without creating a lock (--no-lock). The server's reads (snapshot lists, files,
	// changes, find) use it, so they never wait for an agent's check or delay a backup (plan-m2 Q1).
	NoLock bool
}

// Error carries restic's exit code and the tail of its stderr.
type Error struct {
	Code   int
	Stderr string
}

func (e *Error) Error() string {
	return fmt.Sprintf("restic exit %d: %s", e.Code, strings.TrimSpace(e.Stderr))
}

// ExitCode returns restic's exit code for err, or -1 if err is not a restic exit.
func ExitCode(err error) int {
	var re *Error
	if errors.As(err, &re) {
		return re.Code
	}
	return -1
}

func (r Runner) bin() string {
	if r.Bin != "" {
		return r.Bin
	}
	return "restic"
}

func (r Runner) env() []string {
	var env []string
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "RESTIC_") || strings.HasPrefix(kv, "AWS_") {
			continue // never inherit credentials from the parent process
		}
		env = append(env, kv)
	}
	env = append(env, "RESTIC_REPOSITORY="+r.Repo, "RESTIC_PASSWORD_FILE="+r.PasswordFile, "RESTIC_PROGRESS_FPS=0.2")
	if r.CacheDir != "" {
		env = append(env, "RESTIC_CACHE_DIR="+r.CacheDir)
	}
	return append(env, r.Env...)
}

// run executes restic and returns stdout; the last 8 KiB of stderr go into the error.
func (r Runner) run(ctx context.Context, args ...string) ([]byte, error) {
	// Global flags go before the subcommand: after "--" restic would read them as paths.
	if r.NoLock {
		args = append([]string{"--no-lock"}, args...)
	} else if r.RetryLock > 0 {
		args = append([]string{"--retry-lock", r.RetryLock.String()}, args...)
	}
	cmd := exec.CommandContext(ctx, r.bin(), args...)
	cmd.Env = r.env()
	// When ctx ends, restic gets SIGINT first, so it removes its own lock. It is killed if it has not
	// exited within a minute.
	cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
	cmd.WaitDelay = time.Minute
	var stdout bytes.Buffer
	stderr := &tailBuffer{max: 8192}
	cmd.Stdout = &stdout
	cmd.Stderr = stderr
	err := cmd.Run()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return stdout.Bytes(), &Error{Code: ee.ExitCode(), Stderr: stderr.String()}
		}
		return stdout.Bytes(), err
	}
	return stdout.Bytes(), nil
}

// Version returns restic's version, such as "0.19.1".
func (r Runner) Version(ctx context.Context) (string, error) {
	cmd := exec.CommandContext(ctx, r.bin(), "version")
	cmd.Env = r.env()
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("restic version: %w", err)
	}
	f := strings.Fields(string(out)) // "restic 0.19.1 compiled with go1.24 on linux/amd64"
	if len(f) < 2 || f[0] != "restic" {
		return "", fmt.Errorf("restic version: unexpected output %q", strings.TrimSpace(string(out)))
	}
	return f[1], nil
}

// Init creates the repository unless it exists already, and says whether it created it.
func (r Runner) Init(ctx context.Context) (created bool, err error) {
	if _, err := r.run(ctx, "cat", "config"); err == nil {
		return false, nil
	}
	if _, err := r.run(ctx, "init"); err != nil {
		return false, err
	}
	return true, nil
}

// BackupOptions describes one backup run.
type BackupOptions struct {
	Paths    []string
	Excludes []string
	Tags     []string
	Host     string
	Time     *time.Time // restic --time; the e2e tests use it to inject a clock
}

// BackupResult is restic's JSON summary.
type BackupResult struct {
	SnapshotID          string `json:"snapshot_id"`
	FilesNew            int64  `json:"files_new"`
	FilesChanged        int64  `json:"files_changed"`
	FilesUnmodified     int64  `json:"files_unmodified"`
	DataAdded           int64  `json:"data_added"`
	TotalFilesProcessed int64  `json:"total_files_processed"`
	TotalBytesProcessed int64  `json:"total_bytes_processed"`
	Incomplete          bool   `json:"-"` // exit 3: some files could not be read
	Warnings            string `json:"-"`
}

// Backup runs restic backup. Exit 3 returns a result with Incomplete set: never a silent success.
func (r Runner) Backup(ctx context.Context, o BackupOptions) (BackupResult, error) {
	args := []string{"backup", "--json"}
	if o.Host != "" {
		args = append(args, "--host", o.Host)
	}
	for _, t := range o.Tags {
		args = append(args, "--tag", t)
	}
	for _, e := range o.Excludes {
		args = append(args, "--exclude", e)
	}
	if o.Time != nil {
		args = append(args, "--time", o.Time.Format("2006-01-02 15:04:05"))
	}
	args = append(args, "--")
	args = append(args, o.Paths...)

	out, err := r.run(ctx, args...)
	res, perr := parseSummary(out)
	if err != nil {
		if ExitCode(err) == ExitIncomplete && perr == nil {
			res.Incomplete, res.Warnings = true, err.Error()
			return res, nil
		}
		return res, err
	}
	if perr != nil {
		return res, fmt.Errorf("restic backup succeeded but its summary was unreadable: %w", perr)
	}
	return res, nil
}

func parseSummary(out []byte) (BackupResult, error) {
	var res BackupResult
	found := false
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	for sc.Scan() {
		var probe struct {
			MessageType string `json:"message_type"`
		}
		if json.Unmarshal(sc.Bytes(), &probe) != nil || probe.MessageType != "summary" {
			continue
		}
		if err := json.Unmarshal(sc.Bytes(), &res); err != nil {
			return res, err
		}
		found = true
	}
	if !found || res.SnapshotID == "" {
		return res, errors.New("no summary with snapshot_id in restic output")
	}
	return res, nil
}

// Snapshot is one entry of restic snapshots --json.
type Snapshot struct {
	ID       string           `json:"id"`
	ShortID  string           `json:"short_id"`
	Time     time.Time        `json:"time"`
	Hostname string           `json:"hostname"`
	Tags     []string         `json:"tags"`
	Paths    []string         `json:"paths"`
	Parent   string           `json:"parent"`
	Summary  *SnapshotSummary `json:"summary"` // restic 0.17 and later
}

// SnapshotSummary is what restic recorded about the backup that made a snapshot.
type SnapshotSummary struct {
	BackupStart         time.Time `json:"backup_start"`
	BackupEnd           time.Time `json:"backup_end"`
	DataAdded           int64     `json:"data_added"`
	TotalFilesProcessed int64     `json:"total_files_processed"`
	TotalBytesProcessed int64     `json:"total_bytes_processed"`
}

// Node is one file system entry in a snapshot: names and metadata, never contents.
type Node struct {
	Name        string    `json:"name"`
	Type        string    `json:"type"` // file, dir, symlink, dev, chardev, fifo, socket
	Path        string    `json:"path"`
	UID         uint32    `json:"uid"`
	GID         uint32    `json:"gid"`
	User        string    `json:"user,omitempty"`
	Group       string    `json:"group,omitempty"`
	Size        uint64    `json:"size"`
	Mode        uint32    `json:"mode"`
	Permissions string    `json:"permissions"`
	MTime       time.Time `json:"mtime"`
	LinkTarget  string    `json:"link_target,omitempty"` // restic ls does not print it in 0.19
}

// Ls lists one directory of a snapshot: its direct children, not the directory itself and not deeper.
func (r Runner) Ls(ctx context.Context, snapshot, dir string) ([]Node, error) {
	out, err := r.run(ctx, "ls", "--json", "--long", snapshot, dir)
	if err != nil {
		return nil, err
	}
	nodes := []Node{}
	err = eachJSONLine(out, func(line []byte) error {
		var probe struct {
			StructType string `json:"struct_type"`
			Path       string `json:"path"`
		}
		if json.Unmarshal(line, &probe) != nil || probe.StructType != "node" || probe.Path == dir {
			return nil
		}
		var n Node
		if err := json.Unmarshal(line, &n); err != nil {
			return err
		}
		nodes = append(nodes, n)
		return nil
	})
	return nodes, err
}

// Change is one path in restic diff: "+" added, "-" removed, "M" content changed, "U" metadata changed,
// "T" type changed, "?" bitrot.
type Change struct {
	Path     string `json:"path"`
	Modifier string `json:"modifier"`
}

// DiffStats are restic diff's totals.
type DiffStats struct {
	ChangedFiles int `json:"changed_files"`
	Added        struct {
		Files int   `json:"files"`
		Dirs  int   `json:"dirs"`
		Bytes int64 `json:"bytes"`
	} `json:"added"`
	Removed struct {
		Files int   `json:"files"`
		Dirs  int   `json:"dirs"`
		Bytes int64 `json:"bytes"`
	} `json:"removed"`
}

// Diff compares two snapshots, metadata changes included (spec §5.3).
func (r Runner) Diff(ctx context.Context, from, to string) ([]Change, DiffStats, error) {
	out, err := r.run(ctx, "diff", "--json", "--metadata", from, to)
	if err != nil {
		return nil, DiffStats{}, err
	}
	changes := []Change{}
	var stats DiffStats
	err = eachJSONLine(out, func(line []byte) error {
		var probe struct {
			MessageType string `json:"message_type"`
		}
		if json.Unmarshal(line, &probe) != nil {
			return nil
		}
		switch probe.MessageType {
		case "change":
			var c Change
			if err := json.Unmarshal(line, &c); err != nil {
				return err
			}
			changes = append(changes, c)
		case "statistics":
			return json.Unmarshal(line, &stats)
		}
		return nil
	})
	return changes, stats, err
}

// Found is one snapshot's matches in restic find.
type Found struct {
	Snapshot string `json:"snapshot"`
	Matches  []Node `json:"matches"`
}

// Find looks for a name pattern in every snapshot.
func (r Runner) Find(ctx context.Context, pattern string) ([]Found, error) {
	out, err := r.run(ctx, "find", "--json", "--", pattern)
	if err != nil {
		return nil, err
	}
	found := []Found{}
	if len(bytes.TrimSpace(out)) == 0 {
		return found, nil
	}
	return found, json.Unmarshal(out, &found)
}

// RepoKey is one password (key) of the repository, from restic key list.
type RepoKey struct {
	ID       string `json:"id"`
	Current  bool   `json:"current"`
	UserName string `json:"userName"`
	HostName string `json:"hostName"`
	Created  string `json:"created"`
}

// Keys lists the repository's keys (decision 032: a key added outside Holdfast is reported).
func (r Runner) Keys(ctx context.Context) ([]RepoKey, error) {
	out, err := r.run(ctx, "key", "list", "--json")
	if err != nil {
		return nil, err
	}
	var keys []RepoKey
	return keys, json.Unmarshal(out, &keys)
}

func eachJSONLine(out []byte, fn func([]byte) error) error {
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	for sc.Scan() {
		if err := fn(sc.Bytes()); err != nil {
			return err
		}
	}
	return sc.Err()
}

// Snapshots lists snapshots that carry all the given tags.
func (r Runner) Snapshots(ctx context.Context, tags ...string) ([]Snapshot, error) {
	args := []string{"snapshots", "--json"}
	if len(tags) > 0 {
		args = append(args, "--tag", strings.Join(tags, ","))
	}
	out, err := r.run(ctx, args...)
	if err != nil {
		return nil, err
	}
	var s []Snapshot
	if err := json.Unmarshal(out, &s); err != nil {
		return nil, err
	}
	return s, nil
}

// Check verifies the repository structure and re-reads a subset of the pack data. subset uses
// restic's syntax, such as "3/30" for day 3 of a 30-day cycle; empty checks structure only.
func (r Runner) Check(ctx context.Context, subset string) error {
	args := []string{"check"}
	if subset != "" {
		args = append(args, "--read-data-subset", subset)
	}
	_, err := r.run(ctx, args...)
	return err
}

// Unlock removes stale locks only, and returns restic's report of what it removed. It never passes
// --remove-all. A lock is stale when it is older than 30 minutes, or when it was made on this host by a
// process that no longer exists.
func (r Runner) Unlock(ctx context.Context) (string, error) {
	out, err := r.run(ctx, "unlock")
	return strings.TrimSpace(string(out)), err
}

// Restore restores a snapshot into target, keeping metadata. --sparse keeps holes in sparse files and
// --verify re-reads what was restored. M1 uses it only in tests; restores through the agent are M2.
func (r Runner) Restore(ctx context.Context, snapshot, target string, include []string) error {
	args := []string{"restore", snapshot, "--target", target, "--sparse", "--verify"}
	for _, inc := range include {
		args = append(args, "--include", inc)
	}
	_, err := r.run(ctx, args...)
	return err
}

// RestoreOptions describe one restore through the agent (plan-m2 §3).
type RestoreOptions struct {
	Target  string
	Include []string // restore only these
	Exclude []string // leave these out
	DryRun  bool
}

// RestoreSummary is restic restore's JSON summary.
type RestoreSummary struct {
	TotalFiles    int64 `json:"total_files"`
	FilesRestored int64 `json:"files_restored"`
	FilesSkipped  int64 `json:"files_skipped"`
	TotalBytes    int64 `json:"total_bytes"`
	BytesRestored int64 `json:"bytes_restored"`
}

// RestoreWith restores a snapshot, keeping metadata: --sparse keeps holes, and --verify re-reads what was
// written. A dry run writes nothing and reports what would be restored.
func (r Runner) RestoreWith(ctx context.Context, snapshot string, o RestoreOptions) (RestoreSummary, error) {
	args := []string{"restore", snapshot, "--target", o.Target, "--json"}
	if o.DryRun {
		args = append(args, "--dry-run")
	} else {
		args = append(args, "--sparse", "--verify")
	}
	for _, p := range o.Include {
		args = append(args, "--include", p)
	}
	for _, p := range o.Exclude {
		args = append(args, "--exclude", p)
	}
	out, err := r.run(ctx, args...)
	var sum RestoreSummary
	found := false
	_ = eachJSONLine(out, func(line []byte) error {
		var probe struct {
			MessageType string `json:"message_type"`
		}
		if json.Unmarshal(line, &probe) == nil && probe.MessageType == "summary" && json.Unmarshal(line, &sum) == nil {
			found = true
		}
		return nil
	})
	if err != nil {
		return sum, err
	}
	if !found {
		return sum, errors.New("restic restore printed no summary")
	}
	return sum, nil
}

// SnapshotInfo returns one snapshot.
func (r Runner) SnapshotInfo(ctx context.Context, id string) (Snapshot, error) {
	out, err := r.run(ctx, "snapshots", "--json", id)
	if err != nil {
		return Snapshot{}, err
	}
	var s []Snapshot
	if err := json.Unmarshal(out, &s); err != nil {
		return Snapshot{}, err
	}
	if len(s) != 1 {
		return Snapshot{}, fmt.Errorf("snapshot %s not found", id)
	}
	return s[0], nil
}

// tailBuffer keeps the last max bytes written to it.
type tailBuffer struct {
	max int
	buf []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.max {
		t.buf = t.buf[len(t.buf)-t.max:]
	}
	return len(p), nil
}

func (t *tailBuffer) String() string { return string(t.buf) }
