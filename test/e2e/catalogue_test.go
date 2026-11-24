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
	agent := e.startAgent(fqdn, code)
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
		Against        string
		Added, Removed int
		Content        int `json:"content_changed"`
		Metadata       int `json:"metadata_changed"`
		Changes        []struct{ Path, Modifier string }
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

	// A backup made by hand on the host, through the gateway with the agent's certificate: shown, and alerted, but
	// left alone.
	esc := e.escrow()[fqdn]
	host := e.resticAsAgent(agent, esc)
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

	// Search still finds it, marked as disappeared; and it finds hosts and jobs by name (spec §5.6).
	var sr struct {
		Hosts     []string
		Jobs      []struct{ FQDN string }
		Snapshots []struct{ ID, State string }
	}
	must(t, json.Unmarshal(e.cli("search", older.ShortID), &sr))
	if len(sr.Snapshots) != 1 || sr.Snapshots[0].ID != older.ID || sr.Snapshots[0].State != "disappeared" {
		t.Fatalf("search for a disappeared snapshot: %+v", sr)
	}
	sr.Hosts, sr.Jobs = nil, nil
	must(t, json.Unmarshal(e.cli("search", "cat01"), &sr))
	if len(sr.Hosts) != 1 || sr.Hosts[0] != fqdn {
		t.Fatalf("search for a host: %+v", sr)
	}
	sr.Jobs = nil
	must(t, json.Unmarshal(e.cli("search", "dat"), &sr))
	if len(sr.Jobs) != 1 || sr.Jobs[0].FQDN != fqdn {
		t.Fatalf("search for a job: %+v", sr)
	}
	// A restic password added by hand. Not by the host: the gateway refuses a host writing the repository's keys
	// (decision 041). By someone with S3 access, such as the storage admin: shown and alerted.
	newPass := filepath.Join(t.TempDir(), "new-password")
	must(t, os.WriteFile(newPass, []byte("someone-elses-password"), 0o600))
	if out, err := resticCmd(host, "key", "add", "--new-password-file", newPass).CombinedOutput(); err == nil || !strings.Contains(string(out), "403") {
		t.Fatalf("the host added a restic key through the gateway: %v %s", err, out)
	}
	if out, err := resticCmd(e.resticFor(esc), "key", "add", "--new-password-file", newPass).CombinedOutput(); err != nil {
		t.Fatalf("restic key add: %v %s", err, out)
	}
	list("--refresh")
	if keys, _ := e.hostRaw(fqdn)["foreign_keys"].([]any); len(keys) != 1 {
		t.Fatalf("a key added by hand is not shown: %v", e.hostRaw(fqdn)["foreign_keys"])
	}
	if m := metrics(t, e.url+"/metrics"); m[`holdfast_repository_foreign_keys{host="cat01.example.com"}`] != 1 {
		t.Fatal("the added key is not exported as a metric")
	}

	var ack struct{ Snapshots, Keys int }
	must(t, json.Unmarshal(e.cli("agent", "acknowledge", fqdn), &ack))
	if ack.Snapshots != 1 || ack.Keys != 1 {
		t.Fatalf("acknowledge: %+v", ack)
	}
	if h := e.hostRaw(fqdn); h["disappeared"] != float64(0) || h["foreign_keys"] != nil {
		t.Fatalf("after acknowledge: disappeared %v, foreign keys %v", h["disappeared"], h["foreign_keys"])
	}
	list("--refresh") // the accepted key stays accepted
	if h := e.hostRaw(fqdn); h["foreign_keys"] != nil {
		t.Fatalf("an acknowledged key came back as foreign: %v", h["foreign_keys"])
	}
	actions := map[string]bool{}
	for _, a := range e.audit(fqdn) {
		actions[a.Action] = true
	}
	if !actions["snapshot.disappeared"] || !actions["repository.key_added_outside"] || !actions["host.outside_changes_acknowledged"] {
		t.Fatalf("audit: %v", actions)
	}
	if out, err := e.cliErr("snapshot", "files", fqdn, "not-hex"); err == nil || !strings.Contains(string(out), "hex") {
		t.Fatalf("a malformed snapshot ID: %v %s", err, out)
	}
}
