package e2e

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/kabsdk/holdfast/internal/pin"
)

// The first backup runs right after enrollment. Its snapshot, restored with plain restic from the
// escrow bundle alone (as after losing Holdfast), keeps every metadata class (PoC test 1). Restore
// through the agent is M2.
func TestBackupFidelityAndEscrow(t *testing.T) {
	requireRoot(t)
	src := filepath.Join(t.TempDir(), "data")
	buildFixture(t, src)
	e := newEnv(t)
	code := e.addHost("web01.example.com", "system", "--path", src, "--daily", "02:00")
	e.startAgent("web01.example.com", code)
	e.waitProgress("web01.example.com", "first_backup_done", 2*time.Minute)

	h := e.escrow()["web01.example.com"]
	r := e.resticFor(h)
	snaps, err := r.Snapshots(context.Background(), "job=system")
	if err != nil || len(snaps) != 1 {
		t.Fatalf("snapshots: %d %v", len(snaps), err)
	}
	if snaps[0].Hostname != "web01.example.com" {
		t.Errorf("snapshot hostname %q, want the FQDN", snaps[0].Hostname)
	}
	target := t.TempDir()
	must(t, r.Restore(context.Background(), snaps[0].ID, target, nil))
	for _, res := range compareTrees(scan(t, src), scan(t, filepath.Join(target, src))) {
		t.Logf("%-50s %-5v %s", res.Property, res.Pass, res.Detail)
		if !res.Pass {
			t.Errorf("metadata lost: %s: %s", res.Property, res.Detail)
		}
	}
}

// SIGKILL the server. Backups go through its gateway (decision 041), so none is made while it is down: restic
// waits and retries, and a backup that fails is retried as soon as the server answers again. Once it is back, a
// backup that was due during the outage succeeds, and every result arrives, counted once. The schedule's smallest
// unit is a minute, so the outage spans a slot.
func TestBackupsWaitForTheServer(t *testing.T) {
	src := filepath.Join(t.TempDir(), "data")
	must(t, os.MkdirAll(src, 0o755))
	must(t, os.WriteFile(filepath.Join(src, "f.txt"), []byte("x"), 0o644))
	e := newEnv(t)
	const fqdn = "db01.example.com"
	code := e.addHost(fqdn, "etc", "--path", src, "--every", "1m")
	a := e.startAgent(fqdn, code)
	e.waitProgress(fqdn, "first_backup_done", 2*time.Minute)
	r := e.resticFor(e.escrow()[fqdn])
	before := snapshots(t, r)

	e.srv.kill(syscall.SIGKILL)
	down := time.Now()
	outage := 90 * time.Second
	time.Sleep(outage)
	if made := snapshots(t, r) - before; made != 0 {
		t.Fatalf("%d snapshots were made while the server, and so its gateway, was down", made)
	}

	e.startServer()
	up := time.Now()
	waitFor(t, "a backup that was due during the outage, done once the server is back", 5*time.Minute, func() bool {
		for _, ev := range e.events(fqdn) {
			if ev.Kind != "backup" || ev.Status != "success" || ev.Finished.Before(up) {
				continue
			}
			if ev.ScheduledFor != nil && ev.ScheduledFor.After(down) && ev.ScheduledFor.Before(up) {
				return true // restic waited for the gateway
			}
			if ev.Started.After(up) && ev.ScheduledFor == nil {
				return true // it failed, and was retried when the server answered
			}
		}
		return false
	})
	key := `holdfast_runs_total{host="db01.example.com",kind="backup",job_id="etc",status="success"}`
	waitFor(t, "spool drained and every snapshot counted once", 90*time.Second, func() bool {
		total := snapshots(t, r)
		m := metrics(t, e.url+"/metrics")
		return a.spooled() == 0 && m[key] == float64(total)
	})
	t.Logf("outage %s: no snapshot while the server was down; the backup due meanwhile was done once it was back", outage)
}

// A host reaches only its own repository through the gateway, and deletes only its lock files; each refusal is
// audited, on both hosts, and counted (decision 041). The bucket is versioned (spec §2.3).
func TestGatewayIsolation(t *testing.T) {
	e := newEnv(t)
	const web, db = "web01.example.com", "db01.example.com"
	wa := e.enrollAgent(web, e.addHost(web, "system", "--path", "/etc", "--daily", "02:00"))
	e.enrollAgent(db, e.addHost(db, "system", "--path", "/etc", "--daily", "02:00"))
	c := e.agentClient(wa)
	id := strings.Repeat("ab", 32)
	do := func(method, path string) int {
		t.Helper()
		req, err := http.NewRequest(method, e.url+"/gateway/v1/"+path, strings.NewReader("x"))
		must(t, err)
		resp, err := c.Do(req)
		must(t, err)
		resp.Body.Close()
		return resp.StatusCode
	}
	for _, tc := range []struct {
		method, path string
		want         int
	}{
		{"GET", web + "/config", http.StatusOK},
		{"GET", web + "/keys/", http.StatusOK},
		{"POST", web + "/locks/" + id, http.StatusOK},
		{"DELETE", web + "/locks/" + id, http.StatusOK},
		{"GET", db + "/config", http.StatusForbidden},
		{"GET", db + "/snapshots/", http.StatusForbidden},
		{"POST", db + "/data/" + id, http.StatusForbidden},
		{"DELETE", web + "/snapshots/" + id, http.StatusForbidden},
		{"DELETE", web + "/data/" + id, http.StatusForbidden},
		{"POST", web + "/config", http.StatusForbidden},
		{"POST", web + "/keys/" + id, http.StatusForbidden},
		{"GET", web + "/data%2F" + id, http.StatusBadRequest},
		{"GET", web + "/..%2F" + db + "/config", http.StatusBadRequest},
	} {
		if got := do(tc.method, tc.path); got != tc.want {
			t.Errorf("%s %s: %d, want %d", tc.method, tc.path, got, tc.want)
		}
	}
	// Without a certificate, nothing.
	anon, err := pin.Client(e.url, e.tls.pin, nil)
	must(t, err)
	if resp, err := anon.Get(e.url + "/gateway/v1/" + web + "/config"); err != nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("the gateway without a certificate: %v %v", resp, err)
	}

	// Audited: in web01's history, and the attempt on db01's repository in db01's too.
	refused := func(fqdn, reason string) bool {
		for _, a := range e.audit(fqdn) {
			if a.Action == "gateway.refused" && a.Detail["reason"] == reason && a.Actor == "agent:"+web {
				return true
			}
		}
		return false
	}
	for _, x := range []struct{ fqdn, reason string }{{web, "other_repository"}, {db, "other_repository"}, {web, "delete"}, {web, "server_owned"}, {web, "malformed"}} {
		if !refused(x.fqdn, x.reason) {
			t.Errorf("no gateway.refused (%s) in %s's history", x.reason, x.fqdn)
		}
	}
	m := metrics(t, e.url+"/metrics")
	if m[`holdfast_gateway_refusals_total{host="web01.example.com",reason="other_repository"}`] < 3 || m[`holdfast_gateway_refusals_total{host="web01.example.com",reason="delete"}`] < 2 ||
		m[`holdfast_gateway_refusals_total{host="web01.example.com",reason="malformed"}`] < 2 {
		t.Errorf("the refusals are not counted: %v", m)
	}
	if out := mc(t, "version", "info", "hf/"+e.bucket); !strings.Contains(out, "versioning is enabled") {
		t.Errorf("bucket versioning is not enabled: %s", out)
	}
}

// The server is the storage witness: it reads each repository from the bucket with restic and the
// maintenance key, not through the gateway, and reports each job's newest snapshot there. It agrees with the agents'
// reports while healthy, and shows the gap when the newest snapshot disappears from storage behind Holdfast's back (PoC
// test 5). The alert thresholds themselves are checked by the promtool rule tests.
func TestStalenessAndStorageWitness(t *testing.T) {
	src := filepath.Join(t.TempDir(), "data")
	must(t, os.MkdirAll(src, 0o755))
	must(t, os.WriteFile(filepath.Join(src, "f.txt"), []byte("x"), 0o644))
	e := newEnv(t)
	const fqdn = "file01.example.com"
	code := e.addHost(fqdn, "home", "--path", src, "--every", "1m")
	a := e.startAgent(fqdn, code)
	key := `holdfast_runs_total{host="file01.example.com",kind="backup",job_id="home",status="success"}`
	// Three backups: the first runs at enrollment and the next at the coming minute slot, which can be
	// seconds later; the third is a full slot after the second.
	waitFor(t, "three backups", 4*time.Minute, func() bool { return metrics(t, e.url+"/metrics")[key] >= 3 })
	a.kill(syscall.SIGTERM)

	const stored = `holdfast_storage_newest_snapshot_timestamp_seconds{host="file01.example.com",job_id="home"}`
	e.cli("snapshot", "list", fqdn, "--refresh")
	m := metrics(t, e.url+"/metrics")
	reported, newest := m[`holdfast_job_last_success_timestamp_seconds{host="file01.example.com",job_id="home"}`], m[stored]
	if newest == 0 || reported-newest > 60 || reported < newest {
		t.Fatalf("while healthy the bucket's newest snapshot must match the last reported backup: reported %v, bucket %v", reported, newest)
	}
	if m[`holdfast_catalogue_read_failed{host="file01.example.com"}`] != 0 || m[`holdfast_catalogue_read_timestamp_seconds{host="file01.example.com"}`] == 0 {
		t.Fatalf("the catalogue read metrics: %v", m)
	}
	// The exact test: the snapshot the agent last reported is in the bucket, as the server read it after the report.
	const held = `holdfast_job_snapshot_in_bucket{host="file01.example.com",job_id="home"}`
	if v, ok := m[held]; !ok || v != 1 {
		t.Fatalf("%s = %v (present %v), want 1", held, v, ok)
	}
	if h := e.host(fqdn); hasProblem(h, "not_in_bucket") || h.Jobs[0].InBucket == nil || !*h.Jobs[0].InBucket {
		t.Fatalf("file01 while healthy: %+v", h)
	}

	// Storage loses the newest snapshot (a delete marker, as the bucket is versioned). The server, reading the bucket
	// again, falls back to the second-newest, and the agents' report now leads it by at least their gap.
	var snaps []struct {
		ID   string    `json:"id"`
		Time time.Time `json:"time"`
	}
	must(t, json.Unmarshal(e.cli("snapshot", "list", fqdn), &snaps))
	sort.Slice(snaps, func(i, j int) bool { return snaps[i].Time.After(snaps[j].Time) })
	if len(snaps) < 3 {
		t.Fatalf("expected at least 3 snapshots, got %d", len(snaps))
	}
	mc(t, "rm", "hf/"+e.bucket+"/repos/"+fqdn+"/snapshots/"+snaps[0].ID)
	e.cli("snapshot", "list", fqdn, "--refresh")
	after := metrics(t, e.url+"/metrics")[stored]
	if want := float64(snaps[1].Time.Unix()); after != want {
		t.Fatalf("after the deletion the server reports the bucket's newest at %v, want the second-newest snapshot's %v", after, want)
	}
	if gap := reported - after; gap < 30 {
		t.Fatalf("the reported backup leads the bucket by %.0fs; want at least 30s", gap)
	}
	// It was there when the server read it, so it went later: a disappearance, not a backup that never arrived.
	if h := e.host(fqdn); !hasProblem(h, "disappeared") || hasProblem(h, "not_in_bucket") {
		t.Fatalf("file01 after its newest snapshot went: %+v", h.Problems)
	}
	if lg := metrics(t, e.url+"/metrics")[`holdfast_job_longest_gap_seconds{host="file01.example.com",job_id="home"}`]; lg != 60 {
		t.Errorf("longest gap metric = %v, want 60 (every 1 minute)", lg)
	}
}
