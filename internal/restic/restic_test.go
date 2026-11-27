package restic

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fake writes a shell script that stands in for restic: it records its argv and environment and
// answers like restic would. The e2e tests use the real binary.
func fake(t *testing.T, body string) (bin, log string) {
	t.Helper()
	dir := t.TempDir()
	log = filepath.Join(dir, "log")
	bin = filepath.Join(dir, "restic")
	script := "#!/bin/sh\n{ echo \"ARGS $*\"; env | grep -E '^(RESTIC|AWS)_' | sort; } >> " + log + "\n" + body
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, log
}

func read(t *testing.T, p string) string {
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

const summary = `{"message_type":"status","percent_done":0.5}
{"message_type":"summary","snapshot_id":"abc123def456","files_new":3,"data_added":42,"total_files_processed":10}
`

func TestBackupSuccessAndEnvironment(t *testing.T) {
	t.Setenv("AWS_SECRET_ACCESS_KEY", "inherited-secret")
	t.Setenv("RESTIC_PASSWORD", "inherited-password")
	bin, log := fake(t, "cat <<'EOF'\n"+summary+"EOF\n")
	r := Runner{Bin: bin, Repo: "s3:https://s3.example/b/repos/web01.example.com", PasswordFile: "/etc/holdfast/credentials/repo.pass",
		Env: []string{"AWS_ACCESS_KEY_ID=AKIAHOST", "AWS_SECRET_ACCESS_KEY=host-secret"}, RetryLock: 5 * time.Minute}
	res, err := r.Backup(context.Background(), BackupOptions{Paths: []string{"/etc", "-weird"}, Excludes: []string{"/etc/x"}, Tags: []string{"job=system"}, Host: "web01.example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if res.SnapshotID != "abc123def456" || res.FilesNew != 3 || res.DataAdded != 42 || res.Incomplete {
		t.Fatalf("unexpected result %+v", res)
	}
	got := read(t, log)
	for _, want := range []string{
		"ARGS --retry-lock 5m0s backup --json --host web01.example.com --tag job=system --exclude /etc/x -- /etc -weird",
		"RESTIC_PASSWORD_FILE=/etc/holdfast/credentials/repo.pass", "AWS_SECRET_ACCESS_KEY=host-secret",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	for _, leak := range []string{"inherited-secret", "inherited-password"} {
		if strings.Contains(got, leak) {
			t.Errorf("restic inherited %q from the parent process", leak)
		}
	}
}

func TestBackupIncompleteIsAWarningNotASuccess(t *testing.T) {
	bin, _ := fake(t, "cat <<'EOF'\n"+summary+"EOF\necho 'error: open /etc/shadow-: permission denied' >&2\nexit 3\n")
	res, err := Runner{Bin: bin}.Backup(context.Background(), BackupOptions{Paths: []string{"/etc"}})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Incomplete || !strings.Contains(res.Warnings, "permission denied") {
		t.Fatalf("exit 3 must be reported as incomplete with its warnings, got %+v", res)
	}
}

func TestBackupFailure(t *testing.T) {
	bin, _ := fake(t, "echo 'Fatal: unable to open repository' >&2\nexit 10\n")
	_, err := Runner{Bin: bin}.Backup(context.Background(), BackupOptions{Paths: []string{"/etc"}})
	if ExitCode(err) != ExitNoRepo || !strings.Contains(err.Error(), "unable to open repository") {
		t.Fatalf("expected exit 10 with stderr, got %v", err)
	}
	bin, _ = fake(t, "echo '{\"message_type\":\"status\"}'\n")
	if _, err := (Runner{Bin: bin}).Backup(context.Background(), BackupOptions{Paths: []string{"/etc"}}); err == nil {
		t.Fatal("a backup without a summary must not count as a success")
	}
}

// The JSON restic 0.19.1 printed for forget --dry-run --json --group-by host (trimmed).
const forgetPlan = `[{"tags":null,"host":"web01","paths":null,
 "keep":[{"time":"2026-11-21T12:00:00Z","id":"aaa111","short_id":"aaa1"},{"time":"2026-11-20T12:00:00Z","id":"bbb222","short_id":"bbb2"}],
 "remove":[{"time":"2026-11-11T12:00:00Z","id":"ccc333","short_id":"ccc3"}],
 "reasons":[{"snapshot":{"id":"aaa111"},"matches":["daily within 14d","last snapshot"]},{"snapshot":{"id":"bbb222"},"matches":["daily within 14d"]}]}]`

func TestForgetPlanForgetAndPrune(t *testing.T) {
	bin, log := fake(t, "case \"$*\" in *'forget --dry-run'*) echo '"+strings.ReplaceAll(forgetPlan, "\n", "")+"' ;; esac\n")
	r := Runner{Bin: bin, RetryLock: time.Minute}
	keep, remove, err := r.ForgetPlan(context.Background(), "job=sys", []string{"--keep-within-daily", "14d", "--keep-last", "3"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(IDs(keep), ",") != "aaa111,bbb222" || strings.Join(IDs(remove), ",") != "ccc333" || remove[0].Time.Day() != 11 ||
		strings.Join(keep[0].KeptBy, ";") != "daily within 14d;last snapshot" {
		t.Fatalf("keep %v, remove %v", keep, remove)
	}
	if err := r.Forget(context.Background(), "ccc333"); err != nil {
		t.Fatal(err)
	}
	got := read(t, log)
	for _, want := range []string{"ARGS --no-lock forget --dry-run --json --tag job=sys --group-by host --keep-within-daily 14d --keep-last 3", "ARGS --retry-lock 1m0s forget -- ccc333"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}

	// restic prints nothing when no snapshot has the tag
	bin, _ = fake(t, "")
	if keep, remove, err := (Runner{Bin: bin}).ForgetPlan(context.Background(), "job=none", []string{"--keep-last", "1"}); err != nil || len(keep)+len(remove) != 0 {
		t.Fatalf("an empty plan: %v %v %v", keep, remove, err)
	}
}

func TestParsePrune(t *testing.T) {
	out := `to repack:             0 blobs / 0 B
this removes:          0 blobs / 0 B
to delete:             8 blobs / 2.164 KiB
total prune:           8 blobs / 2.164 KiB
remaining:            10 blobs / 5.724 MiB
unused size after prune: 0 B (0.00% of remaining size)
done`
	if p := parsePrune(out); p.Freed != "2.164 KiB" || p.Remaining != "5.724 MiB" {
		t.Fatalf("%+v", p)
	}
}

// restic init makes a missing bucket itself, so InitFrom runs the server's storage test first, and makes nothing when
// it fails; a repository that exists is neither tested nor made again.
func TestInitFromTestsTheStorageFirst(t *testing.T) {
	ctx := context.Background()
	src := &Source{Repo: "rest:http://127.0.0.1:1/gateway/v1/web01.example.com/", PasswordFile: "/run/a.pass"}
	bin, log := fake(t, "[ \"$1\" = cat ] && exit 10\nexit 0\n")
	r := Runner{Bin: bin, Repo: "s3:https://s3.example/offsite/repos/web01.example.com", PasswordFile: "/run/b.pass", From: src}
	refused := errors.New("the storage does not meet the contract")
	if created, err := r.InitFrom(ctx, func() error { return refused }); !errors.Is(err, refused) || created {
		t.Fatalf("a failed storage test: created %v, err %v", created, err)
	}
	if got := read(t, log); strings.Contains(got, "ARGS init") {
		t.Fatalf("init ran although the storage test failed:\n%s", got)
	}
	tested := false
	if created, err := r.InitFrom(ctx, func() error { tested = true; return nil }); err != nil || !created || !tested {
		t.Fatalf("a passed storage test: created %v, tested %v, err %v", created, tested, err)
	}
	if got := read(t, log); !strings.Contains(got, "ARGS init --copy-chunker-params") {
		t.Fatalf("init did not run:\n%s", got)
	}

	bin, _ = fake(t, "exit 0\n")
	r.Bin = bin
	if created, err := r.InitFrom(ctx, func() error { t.Fatal("the storage was tested for a repository that exists"); return nil }); err != nil || created {
		t.Fatalf("an existing repository: created %v, err %v", created, err)
	}
}

func TestVersionAndUnlock(t *testing.T) {
	bin, log := fake(t, "echo 'restic 0.19.1 compiled with go1.24.4 on linux/amd64'\n")
	v, err := Runner{Bin: bin}.Version(context.Background())
	if err != nil || v != "0.19.1" {
		t.Fatalf("version = %q, %v", v, err)
	}
	if _, err := (Runner{Bin: bin}).Unlock(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := read(t, log); !strings.Contains(got, "ARGS unlock") || strings.Contains(got, "remove-all") {
		t.Fatalf("unlock must only remove stale locks: %s", got)
	}
}

// TestStaleLocks: with UnlockStale, a command that finds the repository locked runs restic unlock (stale locks only),
// and once more if that removed one; a live lock leaves it failed after one unlock; a restic killed by a signal gets
// its lock removed, and is not run again. Without UnlockStale nothing of that happens.
func TestStaleLocks(t *testing.T) {
	ctx := context.Background()
	// The fake fails its first check with exit 11, and passes the next; unlock says what $UNLOCKED says.
	body := `state="$(dirname "$0")/state"
case "$*" in
  *unlock*) [ -n "$UNLOCKED" ] && echo "$UNLOCKED"; exit 0 ;;
  *check*) if [ -e "$state" ]; then exit 0; fi; touch "$state"; echo "unable to create lock in backend: repository is already locked" >&2; exit 11 ;;
  *dump*) kill -9 $$ ;;
esac
`
	args := func(log string) []string {
		var out []string
		for _, l := range strings.Split(read(t, log), "\n") {
			if a, ok := strings.CutPrefix(l, "ARGS "); ok {
				out = append(out, strings.Fields(a)[0])
			}
		}
		return out
	}

	t.Setenv("UNLOCKED", "successfully removed 1 locks")
	bin, log := fake(t, body)
	var reports []string
	r := Runner{Bin: bin, UnlockStale: true, OnUnlock: func(rep string) { reports = append(reports, rep) }}
	if err := r.Check(ctx, ""); err != nil {
		t.Fatalf("a check after a stale lock was removed: %v", err)
	}
	if got := strings.Join(args(log), " "); got != "check unlock check" {
		t.Fatalf("runs: %s", got)
	}
	if len(reports) != 1 || reports[0] != "successfully removed 1 locks" {
		t.Fatalf("OnUnlock got %q", reports)
	}

	// A live lock: unlock removes nothing, and the check fails once, as before.
	t.Setenv("UNLOCKED", "")
	bin, log = fake(t, body)
	r = Runner{Bin: bin, UnlockStale: true}
	if err := r.Check(ctx, ""); ExitCode(err) != ExitLocked {
		t.Fatalf("a check behind a live lock: %v", err)
	}
	if got := strings.Join(args(log), " "); got != "check unlock" {
		t.Fatalf("runs behind a live lock: %s", got)
	}

	// Without UnlockStale: no unlock.
	bin, log = fake(t, body)
	if err := (Runner{Bin: bin}).Check(ctx, ""); ExitCode(err) != ExitLocked {
		t.Fatalf("a locked check: %v", err)
	}
	if got := strings.Join(args(log), " "); got != "check" {
		t.Fatalf("runs without UnlockStale: %s", got)
	}

	// A restic killed by a signal: its stale lock is removed, and the command is not run again.
	t.Setenv("UNLOCKED", "successfully removed 1 locks")
	bin, log = fake(t, body)
	r = Runner{Bin: bin, UnlockStale: true}
	var re *Error
	if err := r.Dump(ctx, "abc", "/x", io.Discard); !errors.As(err, &re) || !re.Killed {
		t.Fatalf("a killed dump: %v", err)
	}
	if got := strings.Join(args(log), " "); got != "dump unlock" {
		t.Fatalf("runs after a killed dump: %s", got)
	}
}
