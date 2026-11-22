package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"filippo.io/age"
	"go.yaml.in/yaml/v3"

	"github.com/kabsdk/holdfast/internal/restic"
	"github.com/kabsdk/holdfast/internal/testpg"
)

// rcloneBin is the pinned rclone the vault reads site A through (decision 039); make e2e fetches it.
var rcloneBin = os.Getenv("HOLDFAST_RCLONE")

// newVaultEnv is site B: a bucket and a database of its own, and holdfast-vault, registered with site A's server
// (a) with a key that may only read A's repos/. Its CLI is the returned env's.
func newVaultEnv(t *testing.T, a *env) *env {
	t.Helper()
	if rcloneBin == "" {
		t.Skip("the vault tests need rclone (HOLDFAST_RCLONE; make e2e fetches it)")
	}
	dir := filepath.Join(a.dir, "site-b")
	must(t, os.MkdirAll(dir, 0o700))
	v := &env{t: t, dir: dir, port: freePort(t), sock: filepath.Join(dir, "run", "api.sock"), bucket: "hf-" + randHex(6), client: http.DefaultClient}
	v.url = fmt.Sprintf("http://127.0.0.1:%d", v.port)
	mc(t, "mb", "--quiet", "hf/"+v.bucket)
	mc(t, "version", "enable", "hf/"+v.bucket)
	mc(t, "ilm", "rule", "add", "--noncurrent-expire-days", "30", "hf/"+v.bucket)
	v.dsn = testpg.DSN(t)
	if out, err := exec.Command(filepath.Join(binDir, "holdfast-vault"), "keygen", filepath.Join(dir, "credentials.key")).CombinedOutput(); err != nil {
		t.Fatalf("keygen: %v %s", err, out)
	}
	mak, msk := v.makeUser(v.maintPolicy())
	maint := filepath.Join(dir, "maintenance.env")
	must(t, os.WriteFile(maint, []byte("AWS_ACCESS_KEY_ID="+mak+"\nAWS_SECRET_ACCESS_KEY="+msk+"\n"), 0o600))
	v.maintKey = mak

	// The vault's key in site A: it may read repos/, and nothing else.
	ak, sk := a.makeUser(`{"Version":"2012-10-17","Statement":[
 {"Effect":"Allow","Action":["s3:GetBucketLocation","s3:ListBucket"],"Resource":["arn:aws:s3:::` + a.bucket + `"]},
 {"Effect":"Allow","Action":["s3:GetObject"],"Resource":["arn:aws:s3:::` + a.bucket + `/repos/*"]}]}`)
	kf := filepath.Join(a.dir, "vault-read.env")
	must(t, os.WriteFile(kf, []byte("AWS_ACCESS_KEY_ID="+ak+"\nAWS_SECRET_ACCESS_KEY="+sk+"\n"), 0o600))
	var reg struct {
		Token string `json:"token"`
	}
	must(t, json.Unmarshal(a.cli("vault", "add", "site-b", "--s3-key-file", kf, "-m", "e2e"), &reg))
	tokenFile := filepath.Join(dir, "token")
	must(t, os.WriteFile(tokenFile, []byte(reg.Token+"\n"), 0o600))

	id, err := age.GenerateX25519Identity()
	must(t, err)
	v.escrowKey = id
	set := map[string]any{
		"role": "vault", "listen": fmt.Sprintf("127.0.0.1:%d", v.port), "api_socket": v.sock, "public_url": v.url,
		"database_dsn": v.dsn, "credentials_key_file": filepath.Join(dir, "credentials.key"), "state_dir": filepath.Join(dir, "state"),
		"restic_bin": resticBin, "default_zone": "UTC", "node_name": "e2e-vault",
		"storage":     map[string]any{"endpoint": s3Endpoint, "bucket": v.bucket, "region": "us-east-1", "maintenance_credentials_file": maint},
		"maintenance": map[string]any{"check": "daily", "check_cycle_days": 30, "retention": "off"},
		"escrow":      map[string]any{"age_recipient": id.Recipient().String()},
		"source":      map[string]any{"server": a.url, "token_file": tokenFile, "poll": "2s", "rclone_bin": rcloneBin},
	}
	b, _ := yaml.Marshal(set)
	v.cfgPath = filepath.Join(dir, "vault.yaml")
	must(t, os.WriteFile(v.cfgPath, b, 0o600))
	v.srvEnv = append(os.Environ(), "TZ=UTC")
	t.Cleanup(func() {
		if t.Failed() {
			for _, l := range v.logs {
				b, _ := os.ReadFile(l)
				t.Logf("==== site B: %s ====\n%s", filepath.Base(l), tail(string(b), 8000))
			}
		}
	})
	v.startVault()
	return v
}

func (e *env) startVault() {
	e.srv = start(e.t, e.logFile("vault"), e.srvEnv, "holdfast-vault", "-config", e.cfgPath)
	waitFor(e.t, "vault healthz", 20*time.Second, func() bool {
		r, err := e.client.Get(e.url + "/healthz")
		if err != nil {
			return false
		}
		r.Body.Close()
		_, serr := os.Stat(e.sock)
		return r.StatusCode == 200 && serr == nil
	})
}

type snapRow struct {
	ID               string     `json:"id"`
	Original         string     `json:"original"`
	MadeByHoldfast   bool       `json:"made_by_holdfast"`
	GoneAt           *time.Time `json:"gone_at"`
	DeletedInPrimary *time.Time `json:"deleted_in_primary_at"`
}

func (e *env) snaps(fqdn string, flags ...string) []snapRow {
	e.t.Helper()
	var rows []snapRow
	out, err := e.cliErr(append([]string{"snapshot", "list", fqdn}, flags...)...)
	if err == nil {
		_ = json.Unmarshal(out, &rows)
	}
	return rows
}

// copyOf returns the vault's copy of a site A snapshot, if it holds one now.
func copyOf(rows []snapRow, original string) (snapRow, bool) {
	for _, r := range rows {
		if r.Original == original && r.GoneAt == nil {
			return r, true
		}
	}
	return snapRow{}, false
}

func presentIDs(rows []snapRow) []string {
	var out []string
	for _, r := range rows {
		if r.GoneAt == nil {
			out = append(out, r.ID)
		}
	}
	slices.Sort(out)
	return out
}

// TestVault is decisions 027, 031 and 039 end to end: site A's holdfast-server and agent, site B's vault, two
// buckets, two databases, real restic and rclone.
//   - The vault copies each new snapshot, which keeps its host, tags and original; the copies count as made by
//     Holdfast; the off-site retention starts as primary's; site A sees what was copied.
//   - The hold: while site B's storage fails, site A's daily run keeps what the vault has not copied yet.
//   - Deleted in primary, and Delete here too; a copy the vault deleted is not made again.
//   - Exclude and Include.
//   - A restore on site A's host from the copy in site B.
//   - Copies go on while site A's server is down.
func TestVault(t *testing.T) {
	src := smallSource(t)
	a := newEnv(t)
	const fqdn = "web01.example.com"
	a.startAgent(fqdn, a.addHost(fqdn, "data", "--path", src, "--daily", "02:00", "--keep-last", "1"))
	a.waitProgress(fqdn, "first_backup_done", 2*time.Minute)
	backups := 1
	backup := func() string {
		t.Helper()
		must(t, os.WriteFile(filepath.Join(src, fmt.Sprintf("f%d.txt", backups+1)), []byte(fmt.Sprintf("version %d\n", backups+1)), 0o644))
		a.cli("job", "run", fqdn, "data")
		backups++
		waitStatus(t, a, fqdn, "data", "success", backups)
		return a.latestSnapshot(fqdn, "data")
	}
	s1 := a.latestSnapshot(fqdn, "data")
	s2 := backup()
	esc := a.escrow()

	// Site B starts copying at once.
	v := newVaultEnv(t, a)
	// Each copy is a run in the host's history, recorded just after the catalogue sees it: then it counts as made by
	// Holdfast.
	waitFor(t, "the vault's first copies", 3*time.Minute, func() bool {
		rows := v.snaps(fqdn)
		c1, ok1 := copyOf(rows, s1)
		c2, ok2 := copyOf(rows, s2)
		return ok1 && ok2 && c1.MadeByHoldfast && c2.MadeByHoldfast
	})
	if evs := v.events(fqdn); countStatus(evs, "data", "success") < 2 {
		t.Fatalf("each copy must be a run in the vault's history: %+v", evs)
	}
	vh := v.hostRaw(fqdn)
	j := vh["jobs"].([]any)[0].(map[string]any)
	if j["retention"].(map[string]any)["keep_last"] != float64(1) || j["primary_retention"].(map[string]any)["keep_last"] != float64(1) {
		t.Fatalf("the off-site retention starts as primary's: %+v", j)
	}
	waitFor(t, "site A to see the copies", time.Minute, func() bool {
		off, _ := a.hostRaw(fqdn)["offsite"].([]any)
		return len(off) == 1 && off[0].(map[string]any)["copies"] == float64(2) && off[0].(map[string]any)["pending"] == float64(0)
	})

	// The hold: site B's storage fails. Two backups in A, neither copied; A's daily run forgets the older one only
	// once the vault has it.
	mc(t, "admin", "user", "disable", "hf", v.maintKey)
	s3 := backup()
	s4 := backup()
	waitFor(t, "the vault to report what it wants", time.Minute, func() bool {
		var vs []struct{ Pending int }
		_ = json.Unmarshal(a.cli("vault", "list"), &vs)
		return len(vs) == 1 && vs[0].Pending == 2
	})
	var pend struct {
		Pending []struct{ ID, Reason string }
	}
	must(t, json.Unmarshal(a.cli("pending", fqdn), &pend))
	if !slices.ContainsFunc(pend.Pending, func(p struct{ ID, Reason string }) bool { return p.ID == s3 && p.Reason == "offsite_copy" }) {
		t.Fatalf("the snapshot the vault wants must wait: %+v", pend)
	}
	a.cli("retention", "run", fqdn)
	waitFor(t, "the daily run to forget what was copied", 2*time.Minute, func() bool {
		got := presentIDs(a.snaps(fqdn, "--refresh"))
		return slices.Equal(got, sortedIDs(s3, s4))
	})
	mc(t, "admin", "user", "enable", "hf", v.maintKey)
	waitFor(t, "the copies to catch up", 3*time.Minute, func() bool {
		rows := v.snaps(fqdn)
		_, ok3 := copyOf(rows, s3)
		_, ok4 := copyOf(rows, s4)
		return ok3 && ok4
	})
	waitFor(t, "the vault to want nothing", time.Minute, func() bool {
		var vs []struct{ Pending int }
		_ = json.Unmarshal(a.cli("vault", "list"), &vs)
		return len(vs) == 1 && vs[0].Pending == 0
	})
	a.cli("retention", "run", fqdn)
	waitFor(t, "the held snapshot to go", 2*time.Minute, func() bool { return slices.Equal(presentIDs(a.snaps(fqdn, "--refresh")), []string{s4}) })
	if got := presentIDs(v.snaps(fqdn, "--refresh")); len(got) != 4 {
		t.Fatalf("site B keeps its copies under its own retention, which has not run: %v", got)
	}

	// Deleted in primary: an admin deletes s4 in A; the vault shows it, and a vault admin deletes it here too.
	a.cli("snapshot", "delete", fqdn, s4[:12], "--now", "--confirm", "web01", "-m", "e2e")
	var c4 snapRow
	waitFor(t, "the copy marked deleted in primary", 2*time.Minute, func() bool {
		var ok bool
		c4, ok = copyOf(v.snaps(fqdn), s4)
		return ok && c4.DeletedInPrimary != nil
	})
	v.cli("snapshot", "delete", fqdn, c4.ID[:12], "--now", "--confirm", "web01", "-m", "deleted in primary")
	waitFor(t, "the copy deleted here too", 2*time.Minute, func() bool { _, ok := copyOf(v.snaps(fqdn, "--refresh"), s4); return !ok })

	// A copy the vault deleted is not made again while A still has the snapshot.
	s5 := backup()
	var c5 snapRow
	waitFor(t, "the copy of s5", 2*time.Minute, func() bool { var ok bool; c5, ok = copyOf(v.snaps(fqdn), s5); return ok })
	v.cli("snapshot", "delete", fqdn, c5.ID[:12], "--now", "--confirm", "web01", "-m", "not wanted here")
	waitFor(t, "the copy of s5 deleted", 2*time.Minute, func() bool { _, ok := copyOf(v.snaps(fqdn, "--refresh"), s5); return !ok })
	time.Sleep(8 * time.Second) // a few of the vault's polls
	if _, ok := copyOf(v.snaps(fqdn, "--refresh"), s5); ok {
		t.Fatal("the vault copied again a snapshot it had deleted")
	}

	// Exclude: no copies until Include.
	v.cli("offsite", "exclude", fqdn, "data", "-m", "e2e")
	waitFor(t, "site A to see the exclusion", time.Minute, func() bool {
		off, _ := a.hostRaw(fqdn)["offsite"].([]any)
		if len(off) != 1 {
			return false
		}
		jobs, _ := off[0].(map[string]any)["jobs"].([]any)
		return len(jobs) == 1 && jobs[0].(map[string]any)["excluded"] == true
	})
	s6 := backup()
	time.Sleep(8 * time.Second)
	if _, ok := copyOf(v.snaps(fqdn, "--refresh"), s6); ok {
		t.Fatal("an excluded job was copied")
	}
	v.cli("offsite", "include", fqdn, "data")
	var c6 snapRow
	waitFor(t, "the copy of s6 after Include", 2*time.Minute, func() bool { var ok bool; c6, ok = copyOf(v.snaps(fqdn), s6); return ok })

	// A restore on site A's host from the copy in site B, once site A knows of it from the vault's report. The vault
	// needs a read-only key for the host first.
	waitFor(t, "site A to list the copy of s6", time.Minute, func() bool {
		var vs []struct {
			Vault  string
			Copies []struct{ ID, Original string }
		}
		_ = json.Unmarshal(a.cli("offsite", "copies", fqdn), &vs)
		return len(vs) == 1 && slices.ContainsFunc(vs[0].Copies, func(c struct{ ID, Original string }) bool { return c.ID == c6.ID && c.Original == s6 })
	})
	if out, err := a.restoreWait(fqdn, c6.ID[:12], "--from-vault", "site-b"); err == nil || !strings.Contains(err.Error(), "read-only key") {
		t.Fatalf("a restore the vault cannot grant: %v %+v", err, out)
	}
	p := "repos/" + fqdn
	roID, roSecret := v.makeUser(`{"Version":"2012-10-17","Statement":[
 {"Effect":"Allow","Action":["s3:GetBucketLocation"],"Resource":["arn:aws:s3:::` + v.bucket + `"]},
 {"Effect":"Allow","Action":["s3:ListBucket"],"Resource":["arn:aws:s3:::` + v.bucket + `"],"Condition":{"StringLike":{"s3:prefix":["` + p + `","` + p + `/*"]}}},
 {"Effect":"Allow","Action":["s3:GetObject"],"Resource":["arn:aws:s3:::` + v.bucket + `/` + p + `/*"]}]}`)
	roFile := filepath.Join(v.dir, "restore-key.env")
	must(t, os.WriteFile(roFile, []byte("AWS_ACCESS_KEY_ID="+roID+"\nAWS_SECRET_ACCESS_KEY="+roSecret+"\n"), 0o600))
	v.cli("host", "set-key", fqdn, "--s3-key-file", roFile)
	ev, err := a.restoreWait(fqdn, c6.ID[:12], "--from-vault", "site-b", "-m", "e2e: from site B")
	if err != nil || ev.Status != "success" {
		t.Fatalf("restore from site B: %+v %v", ev, err)
	}
	got, err := os.ReadFile(filepath.Join(ev.Detail["directory"], src, fmt.Sprintf("f%d.txt", backups)))
	if err != nil || string(got) != fmt.Sprintf("version %d\n", backups) {
		t.Fatalf("the restored file: %q %v", got, err)
	}
	var vaultAudit []auditEntry
	must(t, json.Unmarshal(v.cli("audit", "--host", fqdn), &vaultAudit))
	if !slices.ContainsFunc(vaultAudit, func(x auditEntry) bool { return x.Action == "restore.granted" }) {
		t.Fatalf("the vault's audit log must record the grant: %+v", vaultAudit)
	}

	// Copies go on while site A's server is down: a snapshot made in A with plain restic is copied from the vault's
	// last feed.
	a.srv.kill(syscall.SIGTERM)
	r := a.resticFor(esc[fqdn], false)
	res, err := r.Backup(context.Background(), restic.BackupOptions{Paths: []string{src}, Tags: []string{"job=data"}, Host: fqdn})
	must(t, err)
	waitFor(t, "a copy made while site A's server is down", 2*time.Minute, func() bool { _, ok := copyOf(v.snaps(fqdn), res.SnapshotID); return ok })

	m := metrics(t, v.url+"/metrics")
	if _, ok := m[`holdfast_offsite_behind_seconds{host="web01.example.com",job_id="data"}`]; !ok {
		t.Fatalf("the vault's lag metric: %v", m)
	}
}

func sortedIDs(xs ...string) []string {
	out := slices.Clone(xs)
	slices.Sort(out)
	return out
}
