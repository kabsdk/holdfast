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
	if r.RetryLock > 0 {
		// A global flag goes before the subcommand: after "--" restic would read it as a path.
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

// Init creates the repository unless it exists already.
func (r Runner) Init(ctx context.Context) error {
	if _, err := r.run(ctx, "cat", "config"); err == nil {
		return nil
	}
	_, err := r.run(ctx, "init")
	return err
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
	ID       string    `json:"id"`
	ShortID  string    `json:"short_id"`
	Time     time.Time `json:"time"`
	Hostname string    `json:"hostname"`
	Tags     []string  `json:"tags"`
	Paths    []string  `json:"paths"`
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
