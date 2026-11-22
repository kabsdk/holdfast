package e2e

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/kabsdk/holdfast/internal/agent"
	"github.com/kabsdk/holdfast/internal/restic"
)

type restoreResult struct {
	Kind, CommandID, Status, Error string
	TotalFiles                     int64             `json:"total_files"`
	Detail                         map[string]string `json:"detail"`
}

// setRestoreBounds edits the agent's own config, as root on the host would (decision 030).
func setRestoreBounds(t *testing.T, a *agentProc, root string, inPlace bool) {
	t.Helper()
	cfg, err := agent.LoadConfig(a.cfgPath)
	must(t, err)
	cfg.Restore.Root, cfg.Restore.AllowInPlace = root, inPlace
	must(t, agent.WriteConfig(a.cfgPath, cfg))
}

// TestRestoreThroughTheAgent (M2): a restore runs on the host, through its agent, and keeps all 13 metadata
// properties. A dry run writes nothing. The host's own bounds are enforced: no in-place restore unless the
// host allows it, and no snapshot of another host. An in-place restore takes a safety backup first.
func TestRestoreThroughTheAgent(t *testing.T) {
	requireRoot(t)
	src := filepath.Join(t.TempDir(), "data")
	buildFixture(t, src)
	must(t, os.WriteFile(filepath.Join(src, "plain.txt"), []byte("original"), 0o644))
	e := newEnv(t)
	const fqdn = "rst01.example.com"
	code := e.addHost(fqdn, "system", "--path", src, "--daily", "02:00")
	a := e.enrollAgent(fqdn, code)
	restoreRoot := filepath.Join(t.TempDir(), "restore")
	setRestoreBounds(t, a, restoreRoot, false)
	e.runAgent(a)
	e.waitProgress(fqdn, "first_backup_done", 2*time.Minute)
	var snaps []catSnapshot
	waitFor(t, "the first snapshot in the catalogue", time.Minute, func() bool {
		snaps = nil
		_ = json.Unmarshal(e.cli("snapshot", "list", fqdn), &snaps)
		return len(snaps) == 1
	})
	id := snaps[0].ShortID
	restore := func(args ...string) restoreResult {
		t.Helper()
		var ev restoreResult
		out := e.cli(append([]string{"restore", fqdn, id, "--wait"}, args...)...)
		must(t, json.Unmarshal(out, &ev))
		return ev
	}

	// Into a new root-only directory: every metadata property survives.
	got := restore()
	if got.Status != "success" || !strings.HasPrefix(got.Detail["directory"], restoreRoot+"/") {
		t.Fatalf("restore: %+v", got)
	}
	if st, err := os.Stat(got.Detail["directory"]); err != nil || st.Mode().Perm() != 0o700 {
		t.Fatalf("the restore directory must be root-only: %v %v", st.Mode(), err)
	}
	for _, res := range compareTrees(scan(t, src), scan(t, filepath.Join(got.Detail["directory"], src))) {
		t.Logf("%-50s %-5v %s", res.Property, res.Pass, res.Detail)
		if !res.Pass {
			t.Errorf("metadata lost in a restore through the agent: %s: %s", res.Property, res.Detail)
		}
	}

	// A dry run reports and writes nothing.
	entries, _ := os.ReadDir(restoreRoot)
	if dry := restore("--dry-run"); dry.Status != "success" || dry.Detail["dry_run"] != "true" || dry.TotalFiles == 0 {
		t.Fatalf("dry run: %+v", dry)
	}
	if after, _ := os.ReadDir(restoreRoot); len(after) != len(entries) {
		t.Fatal("a dry run created a directory")
	}

	// The host refuses an in-place restore; the server knows its bounds and says so at once.
	if out, err := e.cliErr("restore", fqdn, id, "--in-place"); err == nil || !strings.Contains(string(out), "does not allow in-place") {
		t.Fatalf("an in-place restore on a host that refuses them: %v %s", err, out)
	}

	// A snapshot of another host, in this host's repository: the agent refuses it.
	host := e.resticAsAgent(a, e.escrow()[fqdn])
	if _, err := host.Backup(context.Background(), restic.BackupOptions{Paths: []string{filepath.Join(src, "plain.txt")}, Host: "other.example.com"}); err != nil {
		t.Fatal(err)
	}
	var all []catSnapshot
	must(t, json.Unmarshal(e.cli("snapshot", "list", fqdn, "--refresh"), &all))
	other := ""
	for _, s := range all {
		if !s.MadeByHoldfast {
			other = s.ShortID
		}
	}
	if out, err := e.cliErr("restore", fqdn, other, "--wait"); err == nil || !strings.Contains(string(out), "restores only its own snapshots") {
		t.Fatalf("restoring another host's snapshot: %v %s", err, out)
	}

	// The host allows in-place restores now: the file comes back, after a safety backup of what was there.
	a.kill(syscall.SIGTERM)
	setRestoreBounds(t, a, restoreRoot, true)
	e.runAgent(a)
	waitFor(t, "the agent's new bounds", 30*time.Second, func() bool {
		b, _ := e.hostRaw(fqdn)["agent"].(map[string]any)["restore_bounds"].(map[string]any)
		return b != nil && b["allow_in_place"] == true
	})
	must(t, os.WriteFile(filepath.Join(src, "plain.txt"), []byte("changed by mistake"), 0o644))
	got = restore("--in-place", "--only", filepath.Join(src, "plain.txt"))
	if got.Status != "success" || got.Detail["pre_restore"] == "" {
		t.Fatalf("in-place restore: %+v", got)
	}
	if b, _ := os.ReadFile(filepath.Join(src, "plain.txt")); string(b) != "original" {
		t.Fatalf("after the in-place restore plain.txt holds %q", b)
	}
	waitFor(t, "the safety backup in the catalogue, made by Holdfast", time.Minute, func() bool {
		var list []catSnapshot
		_ = json.Unmarshal(e.cli("snapshot", "list", fqdn), &list)
		for _, s := range list {
			if s.ID == got.Detail["pre_restore"] && s.MadeByHoldfast {
				return true
			}
		}
		return false
	})

	var hist struct {
		Done []restoreResult
	}
	must(t, json.Unmarshal(e.cli("restores"), &hist))
	if len(hist.Done) < 4 {
		t.Fatalf("restore history: %+v", hist.Done)
	}
	seen := map[string]bool{}
	for _, en := range e.audit(fqdn) {
		seen[en.Action] = true
	}
	if !seen["restore.requested"] {
		t.Fatal("restores are not audited")
	}
}
