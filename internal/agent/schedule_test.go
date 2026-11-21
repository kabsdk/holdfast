package agent

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
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
