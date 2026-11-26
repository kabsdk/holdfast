package agent

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kabsdk/holdfast/internal/proto"
	"github.com/kabsdk/holdfast/internal/schedule"
)

// testAgent is an agent with a fake restic and a clock the test controls.
func testAgent(t *testing.T, sch schedule.Schedule, now *atomic.Int64) *Agent {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "restic")
	if err := os.WriteFile(bin, []byte(fakeRestic), 0o755); err != nil {
		t.Fatal(err)
	}
	a, err := New(Config{Server: "http://unused", FQDN: "web01.example.com", StateDir: filepath.Join(dir, "state"),
		CredentialsDir: filepath.Join(dir, "creds"), ResticBin: bin}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	a.now = func() time.Time { return time.Unix(0, now.Load()).UTC() }
	hc := proto.HostConfig{FQDN: "web01.example.com", Repository: "s3:http://unused/b/repos/web01.example.com",
		Jobs: []proto.JobConfig{{ID: "j", Engine: "restic", Paths: []string{dir}, Schedule: sch}}}
	hc.SHA256 = hc.ComputeSHA256()
	a.hc = &hc
	return a
}

func at(s string) int64 {
	t, err := time.Parse("2006-01-02 15:04:05", s)
	if err != nil {
		panic(err)
	}
	return t.UnixNano()
}

func spooled(t *testing.T, a *Agent) []proto.Event {
	t.Helper()
	entries, _ := os.ReadDir(a.path("spool"))
	var out []proto.Event
	for _, e := range entries {
		b, _ := os.ReadFile(filepath.Join(a.path("spool"), e.Name()))
		var ev proto.Event
		if json.Unmarshal(b, &ev) == nil {
			out = append(out, ev)
		}
	}
	return out
}

func TestFirstRunCatchUpOnceAndPersistentSpread(t *testing.T) {
	daily := schedule.Schedule{Daily: &schedule.Daily{Times: []schedule.ClockTime{"02:00"}}, Spread: schedule.Spread{Enabled: true, Minutes: 30}, Zone: "UTC"}
	var now atomic.Int64
	now.Store(at("2026-11-20 10:00:00"))
	a := testAgent(t, daily, &now)

	// A job that never ran is due at once: the first backup right after enrollment.
	j, st, ok := a.due(a.now())
	if !ok {
		t.Fatal("a job that never ran must be due at once")
	}
	a.runScheduled(context.Background(), j, st)
	if got := a.sched["j"].LastSlot; !got.Equal(a.now()) {
		t.Fatalf("after the first run the schedule starts from now, got last slot %v", got)
	}

	// The next slot is tomorrow 02:00 plus a spread in [0, 30 min), drawn once and kept over a restart.
	if _, _, ok := a.due(time.Unix(0, at("2026-11-21 01:59:00"))); ok {
		t.Fatal("not due before the next slot")
	}
	planned := *a.sched["j"]
	if !planned.NextSlot.Equal(time.Unix(0, at("2026-11-21 02:00:00"))) || planned.NextDelayS < 0 || planned.NextDelayS >= 30*60 {
		t.Fatalf("planned %v + %ds", planned.NextSlot, planned.NextDelayS)
	}
	reloaded, err := New(a.cfg, a.log)
	if err != nil {
		t.Fatal(err)
	}
	if got := reloaded.sched["j"]; got.NextDelayS != planned.NextDelayS || !got.NextSlot.Equal(planned.NextSlot) {
		t.Fatalf("the spread changed over a restart: %+v, then %+v", planned, got)
	}

	// Down for three days: one catch-up run, not three; no skipped events for slots missed while down.
	now.Store(at("2026-11-24 09:30:00"))
	j, st, ok = a.due(a.now())
	if !ok {
		t.Fatal("a missed slot must be due at once after a restart")
	}
	before := len(spooled(t, a))
	a.runScheduled(context.Background(), j, st)
	evs := spooled(t, a)[before:]
	if len(evs) != 1 || evs[0].Status != proto.StatusSuccess {
		t.Fatalf("catch-up should report one successful run, got %+v", evs)
	}
	if got := a.sched["j"].LastSlot; !got.Equal(time.Unix(0, at("2026-11-24 02:00:00"))) {
		t.Fatalf("after catching up, the last handled slot is today's, got %v", got)
	}
	if _, _, ok := a.due(a.now()); ok {
		t.Fatal("caught up: nothing else is due until tomorrow")
	}
}

func TestSlotsDuringARunAreSkippedAndReported(t *testing.T) {
	every := schedule.Schedule{Every: &schedule.Every{N: 1, Unit: "minutes", From: "00:00"}, Zone: "UTC"}
	var now atomic.Int64
	now.Store(at("2026-11-20 10:00:00"))
	a := testAgent(t, every, &now)
	a.sched["j"] = &jobState{ScheduleSHA: scheduleSHA(every), LastSlot: time.Unix(0, at("2026-11-20 10:00:00")).UTC()}

	now.Store(at("2026-11-20 10:01:00"))
	j, st, ok := a.due(a.now())
	if !ok || !st.NextSlot.Equal(time.Unix(0, at("2026-11-20 10:01:00"))) {
		t.Fatalf("10:01 should be due, got ok=%v next=%v", ok, st.NextSlot)
	}
	// The fake restic takes "two and a half minutes": the clock moves while it runs.
	marker := filepath.Join(t.TempDir(), "ran")
	a.cfg.ResticBin = writeSlowRestic(t, marker)
	go func() {
		for {
			if _, err := os.Stat(marker); err == nil {
				now.Store(at("2026-11-20 10:03:30"))
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()
	a.runScheduled(context.Background(), j, st)
	var skipped []string
	for _, ev := range spooled(t, a) {
		if ev.Status == proto.StatusSkipped {
			skipped = append(skipped, ev.ScheduledFor.UTC().Format("15:04"))
		}
	}
	if len(skipped) != 2 || skipped[0] == skipped[1] {
		t.Fatalf("10:02 and 10:03 came due during the run and must be reported skipped, got %v", skipped)
	}
	if got := a.sched["j"].LastSlot; !got.Equal(time.Unix(0, at("2026-11-20 10:03:00"))) {
		t.Fatalf("last handled slot should be 10:03, got %v", got)
	}
}

// TestHungBackupIsStopped: a backup that runs past its timeout is stopped and reported as failed, so the
// host's other jobs can run (decision 030).
func TestHungBackupIsStopped(t *testing.T) {
	daily := schedule.Schedule{Daily: &schedule.Daily{Times: []schedule.ClockTime{"02:00"}}, Zone: "UTC"}
	if got := RunTimeout(daily); got != 24*time.Hour {
		t.Fatalf("a daily job's timeout: %s", got)
	}
	if got := RunTimeout(schedule.Schedule{Every: &schedule.Every{N: 15, Unit: "minutes", From: "00:00"}}); got != time.Hour {
		t.Fatalf("an every-15-minutes job's timeout is at least an hour, got %s", got)
	}
	var now atomic.Int64
	now.Store(at("2026-11-20 10:00:00"))
	a := testAgent(t, daily, &now)
	a.runTimeout = func(schedule.Schedule) time.Duration { return 300 * time.Millisecond }
	hung := filepath.Join(t.TempDir(), "restic")
	if err := os.WriteFile(hung, []byte("#!/bin/sh\ncase \"$*\" in *backup*) exec sleep 60 ;; esac\n"+fakeRestic[len("#!/bin/sh\n"):]), 0o755); err != nil {
		t.Fatal(err)
	}
	a.cfg.ResticBin = hung
	j, st, ok := a.due(a.now())
	if !ok {
		t.Fatal("due")
	}
	start := time.Now()
	a.runScheduled(context.Background(), j, st)
	if took := time.Since(start); took > 10*time.Second {
		t.Fatalf("the hung backup was not stopped in time: %s", took)
	}
	evs := spooled(t, a)
	if len(evs) != 1 || evs[0].Status != proto.StatusFailed || !strings.Contains(evs[0].Error, "was stopped") {
		t.Fatalf("a hung backup must be reported as failed and stopped, got %+v", evs)
	}
}

// TestDailyCheckAfterBackup: the agent checks its own repository after a backup, at most once a day, in
// its one-restic-at-a-time loop. A check on request runs at once, a failed check is reported, and with
// daily checks off only requested checks run (decision 030).
func TestDailyCheckAfterBackup(t *testing.T) {
	hourly := schedule.Schedule{Every: &schedule.Every{N: 1, Unit: "hours", From: "00:00"}, Zone: "UTC"}
	var now atomic.Int64
	now.Store(at("2026-11-20 10:00:00"))
	a := testAgent(t, hourly, &now)
	a.hc.Check = &proto.CheckConfig{CycleDays: 30}
	argv := filepath.Join(t.TempDir(), "argv")
	restic := func(extra string) string {
		p := filepath.Join(t.TempDir(), "restic")
		script := "#!/bin/sh\necho \"$*\" >> " + argv + "\n" + extra + fakeRestic[len("#!/bin/sh\n"):]
		if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	a.cfg.ResticBin = restic("")
	ctx := context.Background()
	backup := func(when string) {
		t.Helper()
		now.Store(at(when))
		j, st, ok := a.due(a.now())
		if !ok {
			t.Fatalf("no backup due at %s", when)
		}
		a.runScheduled(ctx, j, st)
		a.checkIfDue(ctx, st)
	}
	checks := func() []proto.Event {
		var out []proto.Event
		for _, ev := range spooled(t, a) {
			if ev.Kind == proto.KindCheck {
				out = append(out, ev)
			}
		}
		return out
	}

	backup("2026-11-20 10:00:00")
	if c := checks(); len(c) != 1 || c[0].Status != proto.StatusSuccess {
		t.Fatalf("the first backup must be followed by a check: %+v", c)
	}
	if b, _ := os.ReadFile(argv); !strings.Contains(string(b), "check --read-data-subset 25/30") { // day 324 of 2026
		t.Fatalf("restic was not asked to check today's subset:\n%s", b)
	}
	// The check says which part of the data it read.
	if c := checks(); c[0].Detail["read_data_subset"] != "25/30" {
		t.Fatalf("the check's report must name the part it read: %+v", c[0].Detail)
	}
	backup("2026-11-20 11:00:00")
	if n := len(checks()); n != 1 {
		t.Fatalf("a second check within the day: %d", n)
	}
	backup("2026-11-21 10:00:00")
	if n := len(checks()); n != 2 {
		t.Fatalf("a day later the backup must be followed by a check: %d", n)
	}

	a.cfg.ResticBin = restic("case \"$*\" in *check*) echo 'Fatal: repository contains errors' >&2; exit 1 ;; esac\n")
	now.Store(at("2026-11-21 12:00:00"))
	a.execCommand(ctx, proto.Command{ID: "c1", Kind: proto.CommandCheckNow})
	if c := checks(); len(c) != 3 || c[2].Status != proto.StatusFailed || c[2].CommandID != "c1" || !strings.Contains(c[2].Error, "repository contains errors") {
		t.Fatalf("a check on request that fails must be reported as failed: %+v", c)
	}

	a.hc.Check = nil
	a.cfg.ResticBin = restic("")
	backup("2026-11-23 10:00:00")
	if n := len(checks()); n != 3 {
		t.Fatalf("with daily checks off, a backup must not be followed by a check: %d", n)
	}
}

// TestChangedCredentialFilesAreRepaired: the credential files are Holdfast's own. If someone changes them
// on the host, the agent records it once in the host's history, and asks the server for them again by
// reporting version 0 (decision 032).
func TestChangedCredentialFilesAreRepaired(t *testing.T) {
	daily := schedule.Schedule{Every: &schedule.Every{N: 1, Unit: "hours", From: "00:00"}, Zone: "UTC"}
	var now atomic.Int64
	now.Store(at("2026-11-20 10:00:00"))
	a := testAgent(t, daily, &now)
	creds := proto.Credentials{Version: 3, RepoPassword: "repo-password"}
	if err := a.applyCredentials(creds); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	backup := func(when string) {
		t.Helper()
		now.Store(at(when))
		j, st, ok := a.due(a.now())
		if !ok {
			t.Fatalf("no backup due at %s", when)
		}
		a.runScheduled(ctx, j, st)
	}
	notices := func() int {
		n := 0
		for _, ev := range spooled(t, a) {
			if ev.Kind == proto.KindNotice {
				n++
			}
		}
		return n
	}
	backup("2026-11-20 10:00:00")
	if notices() != 0 || a.credVer != 3 {
		t.Fatalf("untouched files: %d notices, version %d", notices(), a.credVer)
	}
	pass := filepath.Join(a.cfg.CredentialsDir, "repo.pass")
	if err := os.WriteFile(pass, []byte("someone-elses-password"), 0o600); err != nil {
		t.Fatal(err)
	}
	backup("2026-11-20 11:00:00")
	backup("2026-11-20 12:00:00")
	if notices() != 1 || a.credVer != 0 {
		t.Fatalf("changed files: %d notices (want 1), version %d (want 0, so the server sends them again)", notices(), a.credVer)
	}
	if err := a.applyCredentials(creds); err != nil { // the server's answer
		t.Fatal(err)
	}
	backup("2026-11-20 13:00:00")
	if notices() != 1 || a.credVer != 3 {
		t.Fatalf("after the server sent them again: %d notices, version %d", notices(), a.credVer)
	}
	if b, _ := os.ReadFile(pass); string(b) != "repo-password" {
		t.Fatalf("the files were not repaired: %s", b)
	}
}

// writeSlowRestic is the fake restic, plus a marker file written during the backup so the test can
// move its clock while the run is in progress.
func writeSlowRestic(t *testing.T, marker string) string {
	p := filepath.Join(t.TempDir(), "restic")
	script := "#!/bin/sh\ncase \"$*\" in *backup*) touch " + marker + "; sleep 0.2 ;; esac\n" + fakeRestic[len("#!/bin/sh\n"):]
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}
