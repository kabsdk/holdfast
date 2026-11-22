package e2e

import (
	"context"
	"crypto/rand"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/kabsdk/holdfast/internal/restic"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

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

	u, _ := url.Parse(s3Endpoint)
	root, err := minio.New(u.Host, &minio.Options{Creds: credentials.NewStaticV4(s3User, s3Pass, ""), Region: "us-east-1"})
	must(t, err)
	locks := func() []string {
		var out []string
		for o := range root.ListObjects(ctx, e.bucket, minio.ListObjectsOptions{Prefix: "repos/" + fqdn + "/locks/"}) {
			if o.Err == nil {
				out = append(out, strings.TrimPrefix(o.Key, "repos/"+fqdn+"/locks/"))
			}
		}
		return out
	}
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
