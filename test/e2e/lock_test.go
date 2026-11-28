package e2e

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/kabsdk/holdfast/internal/agent"
	"github.com/kabsdk/holdfast/internal/restic"
	"github.com/minio/minio-go/v7"
)

// locks lists the lock files in a host's repository, straight from the bucket.
func (e *env) locks(fqdn string) []string {
	e.t.Helper()
	var out []string
	for o := range s3(e.t).ListObjects(context.Background(), e.bucket, minio.ListObjectsOptions{Prefix: "repos/" + fqdn + "/locks/"}) {
		if o.Err == nil {
			out = append(out, strings.TrimPrefix(o.Key, "repos/"+fqdn+"/locks/"))
		}
	}
	return out
}

// resticCmd is restic as a process the test can freeze, with the same repository and identity as r.
func resticCmd(r restic.Runner, args ...string) *exec.Cmd {
	if r.CACert != "" {
		args = append([]string{"--cacert", r.CACert}, args...)
	}
	if r.TLSClientCert != "" {
		args = append([]string{"--tls-client-cert", r.TLSClientCert}, args...)
	}
	cmd := exec.Command(r.Bin, args...)
	cmd.Env = append(os.Environ(), "RESTIC_REPOSITORY="+r.Repo, "RESTIC_PASSWORD_FILE="+r.PasswordFile, "RESTIC_CACHE_DIR="+r.CacheDir)
	cmd.Env = append(cmd.Env, r.Env...)
	return cmd
}

// TestCheckLockIsExclusive asserts the restic behaviour behind two choices (decision 030), with Holdfast's
// own identities. The agent runs its check inside its one-restic-at-a-time loop, and the server (M3)
// places its prunes away from backups. Here the maintenance key checks, and the host backs up through the
// gateway with its agent's certificate (decision 041).
//
// While check holds its lock: the lock is exclusive; a backup without --retry-lock fails at once with
// exit 11; a locking read fails the same way and a --no-lock read works; unlock leaves the live lock
// alone; and a backup with --retry-lock waits, then succeeds. A check that meets a running backup fails
// with exit 11 too. Each restic is frozen with SIGSTOP once its lock is in the bucket, so the test does
// not depend on how long a small check or backup takes.
func TestCheckLockIsExclusive(t *testing.T) {
	e := newEnv(t)
	const fqdn = "lock01.example.com"
	src := filepath.Join(t.TempDir(), "data")
	must(t, os.MkdirAll(src, 0o755))
	buf := make([]byte, 32<<20)
	_, _ = rand.Read(buf)
	must(t, os.WriteFile(filepath.Join(src, "random.bin"), buf, 0o644))
	// Adding the host initialises its repository; enrolling gives the host its certificate. The agent does not run.
	a := e.enrollAgent(fqdn, e.addHost(fqdn, "data", "--path", src, "--daily", "03:00"))
	esc := e.escrow()[fqdn]
	maint, host := e.resticFor(esc), e.resticAsAgent(a, esc)
	ctx := context.Background()
	backup := func(r restic.Runner, path string) error {
		_, err := r.Backup(ctx, restic.BackupOptions{Paths: []string{path}, Tags: []string{"job=data"}, Host: fqdn})
		return err
	}
	must(t, backup(host, src))

	locks := func() []string { return e.locks(fqdn) }
	// frozen starts restic and stops it once its lock is in the bucket.
	frozen := func(r restic.Runner, args ...string) *exec.Cmd {
		t.Helper()
		cmd := resticCmd(r, args...)
		must(t, cmd.Start())
		deadline := time.Now().Add(time.Minute)
		for len(locks()) == 0 {
			if time.Now().After(deadline) {
				t.Fatalf("restic %s never took a lock", args[0])
			}
			time.Sleep(5 * time.Millisecond)
		}
		must(t, cmd.Process.Signal(syscall.SIGSTOP))
		t.Cleanup(func() { _ = cmd.Process.Signal(syscall.SIGCONT); _ = cmd.Process.Kill(); _ = cmd.Wait() })
		return cmd
	}

	check := frozen(maint, "check", "--read-data")
	held := locks()
	if len(held) != 1 {
		t.Fatalf("locks while check runs: %v", held)
	}
	lock, err := resticCmd(maint, "cat", "lock", held[0], "--no-lock").Output()
	if err != nil || !strings.Contains(string(lock), `"exclusive": true`) {
		t.Fatalf("restic check's lock is not exclusive: %s %v", lock, err)
	}

	start := time.Now()
	if err := backup(host, src); restic.ExitCode(err) != restic.ExitLocked {
		t.Fatalf("a backup during check: %v; want exit %d at once", err, restic.ExitLocked)
	}
	if took := time.Since(start); took > 30*time.Second {
		t.Fatalf("without --retry-lock the backup should fail at once, it took %s", took)
	}
	if err := resticCmd(host, "snapshots").Run(); err == nil {
		t.Fatal("a locking read succeeded during check")
	}
	if out, err := resticCmd(host, "snapshots", "--no-lock").CombinedOutput(); err != nil {
		t.Fatalf("a --no-lock read failed during check: %v %s", err, out)
	}
	if _, err := maint.Unlock(ctx); err != nil {
		t.Fatal(err)
	}
	if got := locks(); len(got) != 1 || got[0] != held[0] {
		t.Fatalf("unlock removed a live lock: %v", got)
	}

	waiting := host
	waiting.RetryLock = 2 * time.Minute
	go func() { time.Sleep(3 * time.Second); _ = check.Process.Signal(syscall.SIGCONT) }()
	start = time.Now()
	if err := backup(waiting, src); err != nil {
		t.Fatalf("a backup with --retry-lock did not get through after the check: %v", err)
	}
	if waited := time.Since(start); waited < 2*time.Second {
		t.Fatalf("the backup finished in %s while check held the lock", waited)
	}
	if err := check.Wait(); err != nil {
		t.Fatalf("check: %v", err)
	}

	// The other way round: a check that meets a running backup.
	more := filepath.Join(t.TempDir(), "more")
	must(t, os.MkdirAll(more, 0o755))
	_, _ = rand.Read(buf)
	must(t, os.WriteFile(filepath.Join(more, "random.bin"), buf, 0o644))
	running := frozen(host, "backup", "--host", fqdn, "--tag", "job=data", "--", more)
	if err := maint.Check(ctx, ""); restic.ExitCode(err) != restic.ExitLocked {
		t.Fatalf("a check during a backup: %v; want exit %d", err, restic.ExitLocked)
	}
	must(t, running.Process.Signal(syscall.SIGCONT))
	if err := running.Wait(); err != nil {
		t.Fatalf("the backup that the check met: %v", err)
	}
	if got := locks(); len(got) != 0 {
		t.Fatalf("locks left after both finished: %v", got)
	}
}

// staleLock leaves the lock of a restic killed while it held it, as the OOM killer or a broken pipe leaves one: a
// restic check, whose lock is exclusive, killed with SIGKILL once its lock is in the bucket. restic counts it stale
// (made on this host, by a process that is gone), and never removes it by itself.
func (e *env) staleLock(r restic.Runner, fqdn string) {
	e.t.Helper()
	cmd := resticCmd(r, "check", "--read-data")
	must(e.t, cmd.Start())
	deadline := time.Now().Add(time.Minute)
	for len(e.locks(fqdn)) == 0 {
		if time.Now().After(deadline) {
			_ = cmd.Process.Kill()
			e.t.Fatal("restic check never took a lock")
		}
		time.Sleep(5 * time.Millisecond)
	}
	must(e.t, cmd.Process.Kill())
	_ = cmd.Wait()
	if l := e.locks(fqdn); len(l) != 1 {
		e.t.Fatalf("the killed check should leave its one lock: %v", l)
	}
}

// TestStaleLocksClearThemselves: a lock that a killed restic left behind blocks, until restic unlock removes it, the
// host's backups (an exclusive lock does), its repository check and the server's forget and prune. Found in the lab: a
// restic cut off by a dry-run restore left a lock that failed every check and prune for days. Each of them now removes
// stale locks when restic finds the repository locked, and runs once more; the server's run also removes them before
// its first step, and audits it. None needs an operator.
func TestStaleLocksClearThemselves(t *testing.T) {
	e := newEnv(t)
	const fqdn = "stale01.example.com"
	src := filepath.Join(t.TempDir(), "data")
	must(t, os.MkdirAll(src, 0o755))
	buf := make([]byte, 32<<20) // so that a check --read-data runs long enough to be caught with its lock
	_, _ = rand.Read(buf)
	must(t, os.WriteFile(filepath.Join(src, "random.bin"), buf, 0o644))
	a := e.enrollAgent(fqdn, e.addHost(fqdn, "data", "--path", src, "--daily", "03:00", "--keep-last", "1"))
	cfg, err := agent.LoadConfig(a.cfgPath)
	must(t, err)
	cfg.RetryLock = 2 * time.Second // a backup waits 15 minutes for a lock by default
	must(t, agent.WriteConfig(a.cfgPath, cfg))
	e.runAgent(a)
	e.waitProgress(fqdn, "first_backup_done", 2*time.Minute)
	maint := e.resticFor(e.escrow()[fqdn])
	noLocks := func(after string) {
		t.Helper()
		if l := e.locks(fqdn); len(l) != 0 {
			t.Fatalf("locks left after %s: %v", after, l)
		}
	}

	// A backup behind a stale exclusive lock.
	e.staleLock(maint, fqdn)
	if ev := e.runJob(fqdn, "data"); ev.Status != "success" {
		t.Fatalf("a backup behind a stale lock: %+v", ev)
	}
	noLocks("the backup")

	// The agent's repository check.
	e.staleLock(maint, fqdn)
	var checks []struct{ FQDN, Status, Command, Error string }
	must(t, json.Unmarshal(e.cli("check"), &checks))
	if len(checks) != 1 || checks[0].Command == "" {
		t.Fatalf("check: %+v", checks)
	}
	waitFor(t, "the agent's check", 2*time.Minute, func() bool {
		for _, ev := range e.events(fqdn) {
			if ev.Kind == "check" && ev.CommandID == checks[0].Command {
				if ev.Status != "success" {
					t.Fatalf("a check behind a stale lock: %+v", ev)
				}
				return true
			}
		}
		return false
	})
	noLocks("the check")
	if m := metrics(t, e.url+"/metrics"); m[`holdfast_check_last_run_failed{host="stale01.example.com",reason=""}`] != 0 {
		t.Fatalf("the check passed, so no failed check is exported: %v", m[`holdfast_check_last_run_failed{host="stale01.example.com",reason=""}`])
	}

	// The server's forget and prune: Prune now, which forgets what --keep-last 1 drops of the two backups.
	e.staleLock(maint, fqdn)
	var pre struct {
		Confirm string `json:"confirm"`
		Count   int    `json:"count"`
	}
	must(t, json.Unmarshal(e.cli("prune-now", fqdn), &pre))
	if pre.Count == 0 {
		t.Fatalf("Prune now has nothing to forget: %+v", pre)
	}
	start := time.Now()
	if run := e.run("prune-now", fqdn, "--confirm", pre.Confirm, "-m", "e2e: behind a stale lock"); run.Status != "success" || len(run.Forgotten) != pre.Count || !run.Pruned {
		t.Fatalf("Prune now behind a stale lock: %+v", run)
	}
	if took := time.Since(start); took > 5*time.Minute {
		t.Fatalf("Prune now waited %s: the server removes stale locks before its first step, without waiting for them", took)
	}
	noLocks("Prune now")
	var audit []auditRow
	must(t, json.Unmarshal(e.cli("audit", "--host", fqdn), &audit))
	if !slices.ContainsFunc(audit, func(a auditRow) bool { return a.Action == "repository.stale_locks_removed" }) {
		t.Fatal("the server's removal of a stale lock must be in the audit log")
	}
}

// TestCheckRetriedAfterALiveLock: the daily check meets a lock that is still held, here a backup frozen with its lock
// in the bucket as a restic running elsewhere holds one, and fails as locked; HoldfastCheckFailed's metric says so.
// Once the lock is gone, the retry after the next backup, check_retry_after later, reads the same part of the data,
// passes, and clears the metric, with no operator (decision 060).
func TestCheckRetriedAfterALiveLock(t *testing.T) {
	e := newEnvWith(t, envOpts{DailyChecks: true})
	const fqdn = "retry01.example.com"
	src := smallSource(t)
	a := e.enrollAgent(fqdn, e.addHost(fqdn, "data", "--path", src, "--every", "1m"))
	cfg, err := agent.LoadConfig(a.cfgPath)
	must(t, err)
	cfg.RetryLock = 2 * time.Second        // a check waits 15 minutes for a lock by default
	cfg.CheckRetryAfter = 20 * time.Second // and its retry an hour
	must(t, agent.WriteConfig(a.cfgPath, cfg))

	// The held lock: a backup's, which the agent's backups share and its check cannot.
	more := filepath.Join(t.TempDir(), "more")
	must(t, os.MkdirAll(more, 0o755))
	must(t, os.WriteFile(filepath.Join(more, "f"), []byte("held"), 0o644))
	holder := resticCmd(e.resticAsAgent(a, e.escrow()[fqdn]), "backup", "--host", fqdn, "--tag", "job=data", "--", more)
	must(t, holder.Start())
	waitFor(t, "the holder's lock in the bucket", time.Minute, func() bool { return len(e.locks(fqdn)) > 0 })
	must(t, holder.Process.Signal(syscall.SIGSTOP))
	t.Cleanup(func() { _ = holder.Process.Signal(syscall.SIGCONT); _ = holder.Process.Kill(); _ = holder.Wait() })

	e.runAgent(a)
	checks := func() []event {
		var out []event
		for _, ev := range e.events(fqdn) {
			if ev.Kind == "check" {
				out = append(out, ev)
			}
		}
		return out
	}
	waitFor(t, "the daily check", 3*time.Minute, func() bool { return len(checks()) > 0 })
	failed := checks()[0]
	if failed.Status != "failed" || failed.Detail["reason"] != "locked" || failed.Detail["next_is_retry"] != "true" || failed.Detail["next_check"] == "" {
		t.Fatalf("a check behind a held lock must fail as locked, with a retry to come: %+v", failed)
	}
	failedMetric := `holdfast_check_last_run_failed{host="` + fqdn + `",reason="locked"}`
	if m := metrics(t, e.url+"/metrics"); m[failedMetric] != 1 {
		t.Fatalf("%s = %v, want 1", failedMetric, m[failedMetric])
	}

	// The holder stops as restic does on Ctrl-C, and removes its lock.
	must(t, holder.Process.Signal(os.Interrupt))
	must(t, holder.Process.Signal(syscall.SIGCONT))
	_ = holder.Wait()
	waitFor(t, "the holder's lock to go", time.Minute, func() bool { return len(e.locks(fqdn)) == 0 })

	var retry event
	waitFor(t, "the retry after the next backup", 3*time.Minute, func() bool {
		retry = checks()[0]
		return retry.Detail["retry"] != ""
	})
	if retry.Status != "success" || retry.Detail["retry"] != "1" || retry.Detail["read_data_subset"] != failed.Detail["read_data_subset"] ||
		retry.Detail["retry_of"] == "" {
		t.Fatalf("the retry must read the part that failed, and pass: %+v (the failed check: %+v)", retry, failed)
	}
	if took := retry.Started.Sub(failed.Finished); took < cfg.CheckRetryAfter {
		t.Fatalf("the retry started %s after the failure, before check_retry_after (%s)", took, cfg.CheckRetryAfter)
	}
	m := metrics(t, e.url+"/metrics")
	if v, ok := m[`holdfast_check_last_run_failed{host="`+fqdn+`",reason=""}`]; !ok || v != 0 {
		t.Fatalf("the retry passed, so no failed check is exported: %v", m)
	}
	if m[failedMetric] != 0 {
		t.Fatalf("%s is still exported after the retry passed", failedMetric)
	}
}
