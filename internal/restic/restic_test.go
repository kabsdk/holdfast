package restic

import (
	"context"
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

func TestVersionAndUnlock(t *testing.T) {
	bin, log := fake(t, "echo 'restic 0.19.1 compiled with go1.24.4 on linux/amd64'\n")
	v, err := Runner{Bin: bin}.Version(context.Background())
	if err != nil || v != "0.19.1" {
		t.Fatalf("version = %q, %v", v, err)
	}
	if err := (Runner{Bin: bin}).Unlock(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := read(t, log); !strings.Contains(got, "ARGS unlock") || strings.Contains(got, "remove-all") {
		t.Fatalf("unlock must only remove stale locks: %s", got)
	}
}
