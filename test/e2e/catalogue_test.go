package e2e

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kabsdk/holdfast/internal/restic"
)

type catSnapshot struct {
	ID             string     `json:"id"`
	ShortID        string     `json:"short_id"`
	JobID          string     `json:"job_id"`
	MadeByHoldfast bool       `json:"made_by_holdfast"`
	GoneAt         *time.Time `json:"gone_at"`
}

// TestSnapshotCatalogueAndReads (M2): the catalogue follows each backup. Files, changes and find read the
// repository on the server without a lock, and show names and metadata, never contents. A snapshot made
// by hand, and one deleted by hand, are shown and alerted (decision 032), and the alert can be
// acknowledged.
func TestSnapshotCatalogueAndReads(t *testing.T) {
	src := filepath.Join(t.TempDir(), "data")
	must(t, os.MkdirAll(filepath.Join(src, "sub"), 0o755))
	const secret = "the-contents-never-leave-the-host"
	must(t, os.WriteFile(filepath.Join(src, "a.txt"), []byte(secret), 0o644))
	must(t, os.WriteFile(filepath.Join(src, "sub", "b.txt"), []byte("b"), 0o644))
	must(t, os.WriteFile(filepath.Join(src, "meta.txt"), []byte("m"), 0o644))
	e := newEnv(t)
	const fqdn = "cat01.example.com"
	code := e.addHost(fqdn, "data", "--path", src, "--daily", "03:00")
	e.startAgent(fqdn, code)
	e.waitProgress(fqdn, "first_backup_done", 2*time.Minute)

	// A second snapshot: one file changed, one added, one removed, one with new permissions only.
	must(t, os.WriteFile(filepath.Join(src, "a.txt"), []byte(secret+" v2"), 0o644))
	must(t, os.WriteFile(filepath.Join(src, "c.txt"), []byte("c"), 0o644))
	must(t, os.Remove(filepath.Join(src, "sub", "b.txt")))
	must(t, os.Chmod(filepath.Join(src, "meta.txt"), 0o600))
	e.cli("job", "run", fqdn, "data")
	list := func(args ...string) []catSnapshot {
		t.Helper()
		var rows []catSnapshot
		must(t, json.Unmarshal(e.cli(append([]string{"snapshot", "list", fqdn}, args...)...), &rows))
		return rows
	}
	waitFor(t, "the catalogue to follow the second backup", time.Minute, func() bool { return len(list()) == 2 })
	snaps := list()
	if !snaps[0].MadeByHoldfast || snaps[0].JobID != "data" {
		t.Fatalf("catalogue: %+v", snaps)
	}
	newest, older := snaps[0], snaps[1]

	// Files: one directory, names and metadata only.
	out := e.cli("snapshot", "files", fqdn, newest.ShortID, src)
	if strings.Contains(string(out), secret) {
		t.Fatal("a file's contents left the host through snapshot files")
	}
	var files struct {
		Entries []struct{ Name, Type, Permissions string }
		Total   int
	}
	must(t, json.Unmarshal(out, &files))
	names := map[string]string{}
	for _, n := range files.Entries {
		names[n.Name] = n.Permissions
	}
	if len(names) != 4 || names["meta.txt"] != "-rw-------" || names["sub"] == "" || names["c.txt"] == "" {
		t.Fatalf("files of %s: %+v", src, files.Entries)
	}

	// Changes against the previous snapshot of the job, metadata included.
	var ch struct {
		Against                   string
		Added, Removed            int
		Content                   int `json:"content_changed"`
		Metadata                  int `json:"metadata_changed"`
		Changes                   []struct{ Path, Modifier string }
	}
	out = e.cli("snapshot", "changes", fqdn, newest.ShortID)
	must(t, json.Unmarshal(out, &ch))
	if ch.Against != older.ID || ch.Added != 1 || ch.Removed != 1 || ch.Content != 1 || ch.Metadata < 1 || strings.Contains(string(out), secret) {
		t.Fatalf("changes: %s", out)
	}

	// Find, across both snapshots.
	var found []struct {
		Snapshot string
		Matches  []struct{ Path string }
	}
	must(t, json.Unmarshal(e.cli("snapshot", "find", fqdn, "a.txt"), &found))
	if len(found) != 2 {
		t.Fatalf("find a.txt: %+v", found)
	}

	// A backup made by hand, with the host's own key: shown, and alerted, but left alone.
	esc := e.escrow()[fqdn]
	host := e.resticFor(esc, true)
	if _, err := host.Backup(context.Background(), restic.BackupOptions{Paths: []string{src}, Host: fqdn}); err != nil {
		t.Fatal(err)
	}
	snaps = list("--refresh")
	manual := 0
	for _, s := range snaps {
		if !s.MadeByHoldfast {
			manual++
		}
	}
	if len(snaps) != 3 || manual != 1 || e.hostRaw(fqdn)["not_made_by_holdfast"] != float64(1) {
		t.Fatalf("after a manual backup: %+v", snaps)
	}

	// A snapshot deleted by hand (an admin with the bucket's root key): a tombstone, a page, and no reads.
	mc(t, "rm", "--quiet", "hf/"+e.bucket+"/repos/"+fqdn+"/snapshots/"+older.ID)
	if snaps = list("--refresh"); len(snaps) != 2 {
		t.Fatalf("a deleted snapshot is still listed: %+v", snaps)
	}
	all := list("--all")
	gone := 0
	for _, s := range all {
		if s.GoneAt != nil && s.ID == older.ID {
			gone++
		}
	}
	if len(all) != 3 || gone != 1 {
		t.Fatalf("--all must show the tombstone: %+v", all)
	}
	if m := metrics(t, e.url+"/metrics"); m[`holdfast_snapshots_disappeared{host="cat01.example.com"}`] != 1 || m[`holdfast_snapshots_not_made_by_holdfast{host="cat01.example.com"}`] != 1 {
		t.Fatal("the banners are not exported as metrics")
	}
	if out, err := e.cliErr("snapshot", "files", fqdn, older.ShortID); err == nil || !strings.Contains(string(out), "no longer in the repository") {
		t.Fatalf("reading a snapshot that has gone: %v %s", err, out)
	}
	e.cli("host", "acknowledge", fqdn)
	if d := e.hostRaw(fqdn)["disappeared"]; d != float64(0) {
		t.Fatalf("after acknowledge, disappeared = %v", d)
	}
	actions := map[string]bool{}
	for _, a := range e.audit(fqdn) {
		actions[a.Action] = true
	}
	if !actions["snapshot.disappeared"] || !actions["snapshot.disappearance_acknowledged"] {
		t.Fatalf("audit: %v", actions)
	}
	if out, err := e.cliErr("snapshot", "files", fqdn, "not-hex"); err == nil || !strings.Contains(string(out), "hex") {
		t.Fatalf("a malformed snapshot ID: %v %s", err, out)
	}
}
