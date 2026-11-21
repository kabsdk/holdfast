// Package restic is a thin, explicit wrapper around the restic CLI.
//
// Holdfast never re-implements the repository format: every byte goes through restic,
// so a repository stays restorable with the plain restic binary if Holdfast is gone.
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

// Exit codes documented by restic 0.17+.
const (
	ExitOK         = 0
	ExitFatal      = 1
	ExitIncomplete = 3 // snapshot written, some source files could not be read
	ExitNoRepo     = 10
	ExitLocked     = 11
	ExitBadPass    = 12
)

// Runner runs restic against exactly one repository.
type Runner struct {
	Bin          string // path to restic
	Repo         string // RESTIC_REPOSITORY
	PasswordFile string // RESTIC_PASSWORD_FILE (never pass passwords via argv)
	CacheDir     string
	ExtraEnv     []string // e.g. AWS_ACCESS_KEY_ID for S3 backends
}

// Error carries restic's exit code and the tail of stderr.
type Error struct {
	Code   int
	Stderr string
}

func (e *Error) Error() string {
	return fmt.Sprintf("restic exit %d: %s", e.Code, strings.TrimSpace(e.Stderr))
}

// ExitCode returns restic's exit code for err, or -1.
func ExitCode(err error) int {
	var re *Error
	if errors.As(err, &re) {
		return re.Code
	}
	return -1
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
	return append(env, r.ExtraEnv...)
}

func (r Runner) bin() string {
	if r.Bin != "" {
		return r.Bin
	}
	return "restic"
}

// run executes restic and returns stdout. stderr is kept (last 8 KiB) for errors.
func (r Runner) run(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, r.bin(), args...)
	cmd.Env = r.env()
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

// Init creates the repository if it does not exist yet. Idempotent.
func (r Runner) Init(ctx context.Context) error {
	if _, err := r.run(ctx, "cat", "config"); err == nil {
		return nil
	}
	_, err := r.run(ctx, "init")
	return err
}

// BackupOptions describes one backup run.
type BackupOptions struct {
	Paths     []string
	Excludes  []string
	Tags      []string
	Host      string
	Time      *time.Time // injectable clock (restic --time); used by retention tests
	ExtraArgs []string
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
	Incomplete          bool   `json:"-"` // exit 3
	Warnings            string `json:"-"`
}

// Backup runs restic backup. Exit 3 (incomplete) returns a result AND Incomplete=true,
// never a silent success.
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
	args = append(args, o.ExtraArgs...)
	args = append(args, "--")
	args = append(args, o.Paths...)

	out, err := r.run(ctx, args...)
	res, perr := parseSummary(out)
	if err != nil {
		if ExitCode(err) == ExitIncomplete && perr == nil {
			res.Incomplete = true
			res.Warnings = err.Error()
			return res, nil
		}
		return res, err
	}
	if perr != nil {
		return res, fmt.Errorf("restic backup succeeded but summary was unreadable: %w", perr)
	}
	return res, nil
}

func parseSummary(out []byte) (BackupResult, error) {
	var res BackupResult
	found := false
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	for sc.Scan() {
		line := sc.Bytes()
		var probe struct {
			MessageType string `json:"message_type"`
		}
		if json.Unmarshal(line, &probe) != nil || probe.MessageType != "summary" {
			continue
		}
		if err := json.Unmarshal(line, &res); err != nil {
			return res, err
		}
		found = true
	}
	if !found || res.SnapshotID == "" {
		return res, errors.New("no summary with snapshot_id in restic output")
	}
	return res, nil
}

// Restore restores a snapshot (or "latest") into target, preserving metadata.
// --sparse keeps holes in sparse files; --verify re-reads restored content.
func (r Runner) Restore(ctx context.Context, snapshot, target string, include []string) error {
	args := []string{"restore", snapshot, "--target", target, "--sparse", "--verify"}
	for _, inc := range include {
		args = append(args, "--include", inc)
	}
	_, err := r.run(ctx, args...)
	return err
}

// Snapshot is one entry of restic snapshots --json.
type Snapshot struct {
	ID       string    `json:"id"`
	ShortID  string    `json:"short_id"`
	Time     time.Time `json:"time"`
	Hostname string    `json:"hostname"`
	Tags     []string  `json:"tags"`
	Paths    []string  `json:"paths"`
}

// Snapshots lists snapshots, optionally filtered by tag.
func (r Runner) Snapshots(ctx context.Context, tag string) ([]Snapshot, error) {
	args := []string{"snapshots", "--json"}
	if tag != "" {
		args = append(args, "--tag", tag)
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

// Forget applies a retention policy to snapshots with the given tag and optionally prunes.
// Only the server's maintenance identity calls this; agents never forget or prune.
func (r Runner) Forget(ctx context.Context, tag string, keep map[string]int, prune bool) error {
	args := []string{"forget", "--tag", tag, "--group-by", "host,tags"}
	any := false
	for _, k := range []string{"last", "daily", "weekly", "monthly", "yearly"} {
		if n := keep[k]; n > 0 {
			args = append(args, fmt.Sprintf("--keep-%s", k), fmt.Sprint(n))
			any = true
		}
	}
	if !any {
		return errors.New("refusing to forget with an empty retention policy")
	}
	if prune {
		args = append(args, "--prune")
	}
	_, err := r.run(ctx, args...)
	return err
}

// Check verifies repository structure and re-reads a rotating subset of pack data.
// subset is restic syntax, e.g. "3/30" (day 3 of a 30-day cycle).
func (r Runner) Check(ctx context.Context, subset string) error {
	args := []string{"check"}
	if subset != "" {
		args = append(args, "--read-data-subset", subset)
	}
	_, err := r.run(ctx, args...)
	return err
}

// Unlock removes only stale locks (never --remove-all).
func (r Runner) Unlock(ctx context.Context) error {
	_, err := r.run(ctx, "unlock")
	return err
}

// tailBuffer keeps the last max bytes written.
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
