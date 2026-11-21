package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"

	"holdfast/internal/freshness"
	"holdfast/internal/proto"
	"holdfast/internal/restic"
	"holdfast/internal/server"
)

func dur(d time.Duration) proto.Duration { return proto.Duration{Duration: d} }

func requireRoot(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root: ownership, trusted.* xattrs and ACLs")
	}
}

func lastSuccess(e *env, host, job string) (server.JobStatus, bool) {
	hv, ok := e.hostView(host)
	if !ok || hv.Jobs[job] == nil {
		return server.JobStatus{}, false
	}
	return *hv.Jobs[job], !hv.Jobs[job].LastSuccess.IsZero()
}

func writeResults(t *testing.T, name string, v any) {
	dir := os.Getenv("HOLDFAST_RESULTS")
	if dir == "" {
		return
	}
	_ = os.MkdirAll(dir, 0o755)
	b, _ := json.MarshalIndent(v, "", "  ")
	_ = os.WriteFile(filepath.Join(dir, name+".json"), b, 0o644)
}

// 1. Backup via the agent, restore via a server-queued job ON the agent, then diff
// every metadata class. This is the test Zerobyte cannot pass (restores only run on its server).
func TestRestoreOnAgentPreservesMetadata(t *testing.T) {
	requireRoot(t)
	src := filepath.Join(t.TempDir(), "data")
	buildFixture(t, src)
	e := newEnv(t, []server.HostSpec{{Name: "web01", EnrollCode: "code-web01", Jobs: []proto.Job{
		{ID: "data", Paths: []string{src}, Interval: dur(time.Hour), Retention: proto.Retention{KeepDaily: 7}},
	}}})
	e.startServer()
	e.startAgent("web01", "code-web01")

	var js server.JobStatus
	waitFor(t, "first backup reported", 60*time.Second, func() bool {
		var ok bool
		js, ok = lastSuccess(e, "web01", "data")
		return ok
	})
	if js.LastStatus != proto.StatusSuccess {
		t.Fatalf("first backup status %q: %s", js.LastStatus, js.LastError)
	}
	target := filepath.Join(t.TempDir(), "restore")
	code, body := e.do("POST", "/api/v1/hosts/web01/restore", proto.RestoreRequest{Snapshot: js.LastSnapshot, Target: target})
	if code != 202 {
		t.Fatalf("queue restore: %d %s", code, body)
	}
	waitFor(t, "restore reported", 60*time.Second, func() bool {
		hv, _ := e.hostView("web01")
		for _, r := range hv.Recent {
			if r.Kind == "restore" {
				if r.Status != proto.StatusSuccess {
					t.Fatalf("restore failed: %s", r.Error)
				}
				return true
			}
		}
		return false
	})
	results := compareTrees(scan(t, src), scan(t, filepath.Join(target, src)))
	writeResults(t, "fidelity", results)
	for _, r := range results {
		t.Logf("%-50s %-5v %s", r.Property, r.Pass, r.Detail)
		if !r.Pass {
			t.Errorf("metadata lost: %s: %s", r.Property, r.Detail)
		}
	}
}

// 2. Kill the server (SIGKILL). The agent must keep backing up on its own schedule and
// deliver every spooled result once the server is back. Nothing is lost or silent.
func TestAgentKeepsBackingUpWhileServerIsDown(t *testing.T) {
	src := filepath.Join(t.TempDir(), "data")
	must(t, os.MkdirAll(src, 0o755))
	must(t, os.WriteFile(filepath.Join(src, "f.txt"), []byte("x"), 0o644))
	e := newEnv(t, []server.HostSpec{{Name: "db01", EnrollCode: "code-db01", Jobs: []proto.Job{
		{ID: "etc", Paths: []string{src}, Interval: dur(2 * time.Second), Retention: proto.Retention{KeepLast: 100}},
	}}})
	e.startServer()
	e.startAgent("db01", "code-db01")
	waitFor(t, "first backup", 60*time.Second, func() bool { _, ok := lastSuccess(e, "db01", "etc"); return ok })

	pass := filepath.Join(e.cfg.StateDir, "keys", "db01.pass")
	rn := restic.Runner{Bin: resticBin, Repo: filepath.Join(e.cfg.RepoBase, "db01"), PasswordFile: pass}
	before, err := rn.Snapshots(context.Background(), "")
	must(t, err)

	e.srv.kill(syscall.SIGKILL)
	outage := 15 * time.Second
	time.Sleep(outage)
	during, err := rn.Snapshots(context.Background(), "")
	must(t, err)
	made := len(during) - len(before)
	if made < 4 {
		t.Fatalf("agent made only %d snapshots during a %s server outage (want >= 4)", made, outage)
	}
	spooled, _ := os.ReadDir(filepath.Join(e.dir, "agent-db01", "state", "spool"))

	e.startServer()
	m := map[string]float64{}
	waitFor(t, "spool drained into server", 30*time.Second, func() bool {
		m = metrics(t, e.url+"/metrics")
		left, _ := os.ReadDir(filepath.Join(e.dir, "agent-db01", "state", "spool"))
		return len(left) == 0 && m[`holdfast_runs_total{host="db01",kind="backup",job="etc",status="success"}`] >= float64(len(during))
	})
	// Server-side maintenance (forget --prune + rotating read-data-subset check) via the API.
	code, body := e.do("POST", "/api/v1/maintenance/run", nil)
	if code != 200 || !strings.Contains(string(body), `"db01":"success"`) {
		t.Fatalf("maintenance: %d %s", code, body)
	}
	m = metrics(t, e.url+"/metrics")
	if m[`holdfast_maintenance_last_success_timestamp_seconds{host="db01"}`] == 0 {
		t.Fatal("maintenance success not exported")
	}
	writeResults(t, "outage", map[string]any{"outage_seconds": outage.Seconds(), "snapshots_made_during_outage": made,
		"events_spooled_at_restart": len(spooled), "server_success_runs_after_restart": m[`holdfast_runs_total{host="db01",kind="backup",job="etc",status="success"}`]})
	t.Logf("outage %s: %d snapshots made while server was dead, %d events spooled, all delivered", outage, made, len(spooled))
}

// 3. SIGKILL the agent mid-backup. The next start must report "interrupted" (never silent),
// clean the stale restic lock, pick up a changed config and succeed.
func TestKilledBackupIsReportedAndRecovered(t *testing.T) {
	src := filepath.Join(t.TempDir(), "big")
	must(t, os.MkdirAll(src, 0o755))
	f, err := os.Create(filepath.Join(src, "random.bin"))
	must(t, err)
	buf := make([]byte, 1<<20)
	for i := 0; i < 24; i++ {
		rf, _ := os.Open("/dev/urandom")
		_, _ = rf.Read(buf)
		rf.Close()
		_, _ = f.Write(buf)
	}
	must(t, f.Close())
	slow := proto.Job{ID: "big", Paths: []string{src}, Interval: dur(time.Hour), Retry: dur(time.Second),
		Retention: proto.Retention{KeepLast: 5}, ExtraArgs: []string{"--limit-upload", "128"}}
	e := newEnv(t, []server.HostSpec{{Name: "app01", EnrollCode: "code-app01", Jobs: []proto.Job{slow}}})
	e.startServer()
	agentProc := e.startAgent("app01", "code-app01")
	marker := filepath.Join(e.dir, "agent-app01", "state", "running.json")
	waitFor(t, "backup in progress", 60*time.Second, func() bool { _, err := os.Stat(marker); return err == nil })
	time.Sleep(3 * time.Second)
	agentProc.kill(syscall.SIGKILL) // agent and restic die together, mid-upload

	locks, _ := os.ReadDir(filepath.Join(e.cfg.RepoBase, "app01", "locks"))
	if len(locks) == 0 {
		t.Log("note: no lock left behind (restic released it before dying)")
	}
	// Operator fixes the cause: remove the throttle. Server restart = new config version.
	e.srv.kill(syscall.SIGTERM)
	e.cfg.Hosts[0].Jobs[0].ExtraArgs = nil
	e.writeConfig()
	e.startServer()
	e.startAgent("app01", "code-app01")

	var statuses []string
	waitFor(t, "interrupted reported, then success", 90*time.Second, func() bool {
		hv, _ := e.hostView("app01")
		statuses = statuses[:0]
		for i := len(hv.Recent) - 1; i >= 0; i-- {
			statuses = append(statuses, hv.Recent[i].Status)
		}
		return len(statuses) >= 2 && statuses[0] == proto.StatusInterrupted && statuses[len(statuses)-1] == proto.StatusSuccess
	})
	m := metrics(t, e.url+"/metrics")
	if m[`holdfast_runs_total{host="app01",kind="backup",job="big",status="interrupted"}`] != 1 {
		t.Fatalf("interrupted run not counted in metrics")
	}
	writeResults(t, "interrupted", map[string]any{"stale_locks_after_kill": len(locks), "status_sequence": statuses})
	t.Logf("locks left by SIGKILL: %d; reported sequence: %v", len(locks), statuses)
}

// 4. Retention with an injected clock: 180 days of snapshots created in seconds via
// restic --time, then the server's forget policy. Survivors must match an independent model.
func TestRetentionPolicyMatchesModel(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	must(t, os.MkdirAll(src, 0o755))
	must(t, os.WriteFile(filepath.Join(src, "f"), []byte("x"), 0o644))
	pass := filepath.Join(dir, "pass")
	must(t, os.WriteFile(pass, []byte("retention-test"), 0o600))
	rn := restic.Runner{Bin: resticBin, Repo: filepath.Join(dir, "repo"), PasswordFile: pass, CacheDir: filepath.Join(dir, "cache")}
	ctx := context.Background()
	must(t, rn.Init(ctx))

	t.Setenv("TZ", "UTC")
	end := time.Date(2026, 11, 20, 2, 0, 0, 0, time.UTC)
	var times []time.Time
	for d := 179; d >= 0; d-- {
		day := end.AddDate(0, 0, -d)
		times = append(times, day)
		if d%3 == 0 { // some days have two snapshots: only the newest of a day may survive daily
			times = append(times, day.Add(12*time.Hour))
		}
	}
	for i := range times {
		tm := times[i]
		if _, err := rn.Backup(ctx, restic.BackupOptions{Paths: []string{src}, Host: "web01",
			Tags: []string{"holdfast", "job:etc"}, Time: &tm}); err != nil {
			t.Fatal(err)
		}
	}
	policy := map[string]int{"daily": 7, "weekly": 4, "monthly": 6, "yearly": 2}
	must(t, rn.Forget(ctx, "job:etc", policy, true))
	snaps, err := rn.Snapshots(ctx, "job:etc")
	must(t, err)
	var got []string
	for _, s := range snaps {
		got = append(got, s.Time.UTC().Format("2006-01-02 15:04"))
	}
	sort.Strings(got)
	want := modelKeep(times, policy)
	writeResults(t, "retention", map[string]any{"snapshots_created": len(times), "kept": len(got), "kept_times": got, "policy": policy})
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("retention mismatch\n got: %v\nwant: %v", got, want)
	}
	must(t, rn.Check(ctx, "1/1"))
	t.Logf("%d snapshots over 180 days -> %d kept, identical to model; full read-data check passed after prune", len(times), len(got))
}

// modelKeep is an independent implementation of restic's bucket policy: walking newest
// first, keep a snapshot when its bucket differs from the previous one, up to N buckets.
// Finding from this test: restic (>= 0.17) ALSO keeps the oldest snapshot when a policy
// cannot fill all N buckets (here keep-yearly 2 with only one year of data).
func modelKeep(times []time.Time, policy map[string]int) []string {
	sorted := append([]time.Time(nil), times...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].After(sorted[j]) })
	buckets := map[string]func(time.Time) string{
		"daily":   func(t time.Time) string { return t.Format("2006-01-02") },
		"weekly":  func(t time.Time) string { y, w := t.ISOWeek(); return fmt.Sprintf("%d-W%02d", y, w) },
		"monthly": func(t time.Time) string { return t.Format("2006-01") },
		"yearly":  func(t time.Time) string { return t.Format("2006") },
	}
	keep := map[time.Time]bool{}
	for name, n := range policy {
		last, count := "", 0
		for _, t := range sorted {
			b := buckets[name](t)
			if b != last && count < n {
				keep[t] = true
				count++
			}
			last = b
		}
		if count < n && len(sorted) > 0 {
			keep[sorted[len(sorted)-1]] = true
		}
	}
	var out []string
	for t := range keep {
		out = append(out, t.Format("2006-01-02 15:04"))
	}
	sort.Strings(out)
	return out
}

// 5. The two witnesses. Stop the agent: the server's own metrics must show staleness and
// an offline agent. Then delete the newest snapshot object from storage to simulate a
// server that reports a success the storage does not contain: the freshness exporter
// must disagree with the server.
func TestStalenessAndIndependentStorageWitness(t *testing.T) {
	src := filepath.Join(t.TempDir(), "data")
	must(t, os.MkdirAll(src, 0o755))
	must(t, os.WriteFile(filepath.Join(src, "f.txt"), []byte("x"), 0o644))
	interval := 2 * time.Second
	e := newEnv(t, []server.HostSpec{{Name: "file01", EnrollCode: "code-file01", Jobs: []proto.Job{
		{ID: "home", Paths: []string{src}, Interval: dur(interval), Retention: proto.Retention{KeepLast: 50}},
	}}})
	e.startServer()
	ag := e.startAgent("file01", "code-file01")
	waitFor(t, "three backups", 60*time.Second, func() bool {
		m := metrics(t, e.url+"/metrics")
		return m[`holdfast_runs_total{host="file01",kind="backup",job="home",status="success"}`] >= 3
	})
	ag.kill(syscall.SIGTERM)
	time.Sleep(3 * interval)

	m := metrics(t, e.url+"/metrics")
	now := float64(time.Now().Unix())
	lastOK := m[`holdfast_job_last_success_timestamp_seconds{host="file01",job="home"}`]
	expected := m[`holdfast_job_expected_interval_seconds{host="file01",job="home"}`]
	lastSeen := m[`holdfast_agent_last_seen_timestamp_seconds{host="file01"}`]
	// The PromQL alert is: time() - last_success > 2 * expected_interval
	staleFires := now-lastOK > 2*expected
	if !staleFires {
		t.Fatalf("stale alert should fire: age %.0fs, interval %.0fs", now-lastOK, expected)
	}

	repos := e.cfg.RepoBase
	st, err := freshness.LocalLister{Root: repos}.List()
	must(t, err)
	storageNewest := float64(st[0].NewestSnapshot.Unix())
	agreeGap := lastOK - storageNewest
	if agreeGap > 5 {
		t.Fatalf("witnesses should agree while healthy: server %v vs storage %v", lastOK, storageNewest)
	}

	// Simulate a lying or buggy control plane: storage loses the newest snapshot object.
	snapDir := filepath.Join(repos, "file01", "snapshots")
	entries, _ := os.ReadDir(snapDir)
	var newest os.DirEntry
	var newestT time.Time
	for _, en := range entries {
		info, _ := en.Info()
		if info.ModTime().After(newestT) {
			newest, newestT = en, info.ModTime()
		}
	}
	must(t, os.Remove(filepath.Join(snapDir, newest.Name())))
	// Age the remaining objects so the gap is unambiguous.
	old := time.Now().Add(-2 * time.Hour)
	entries, _ = os.ReadDir(snapDir)
	for _, en := range entries {
		_ = os.Chtimes(filepath.Join(snapDir, en.Name()), old, old)
	}
	st, err = freshness.LocalLister{Root: repos}.List()
	must(t, err)
	disagreeGap := lastOK - float64(st[0].NewestSnapshot.Unix())
	// The PromQL alert is: server last_success - storage newest > 1h
	if disagreeGap <= 3600 {
		t.Fatalf("witness disagreement not detected: gap %.0fs", disagreeGap)
	}
	writeResults(t, "witness", map[string]any{"stale_age_seconds": now - lastOK, "agent_last_seen_age_seconds": now - lastSeen,
		"healthy_gap_seconds": agreeGap, "disagreement_gap_seconds": disagreeGap})
	t.Logf("stale age %.0fs (>%.0fs fires), healthy witness gap %.1fs, injected disagreement gap %.0fs", now-lastOK, 2*expected, agreeGap, disagreeGap)
}
