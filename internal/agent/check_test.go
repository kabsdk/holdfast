package agent

import (
	"context"
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

// checkRig is an agent with hourly backups, daily checks, a clock the test sets, and a restic whose check passes,
// finds the repository locked, finds damage, hangs or is killed, as the file mode says.
type checkRig struct {
	t    *testing.T
	a    *Agent
	now  *atomic.Int64
	mode string // the file the fake restic reads: ok, locked, damaged, hang or killed
	argv string // every restic command line, one per line
}

func newCheckRig(t *testing.T) *checkRig {
	t.Helper()
	hourly := schedule.Schedule{Every: &schedule.Every{N: 1, Unit: "hours", From: "00:00"}, Zone: "UTC"}
	r := &checkRig{t: t, now: &atomic.Int64{}}
	r.now.Store(at("2026-11-20 10:00:00")) // day 324: the date's part is 25/30
	r.a = testAgent(t, hourly, r.now)
	r.a.hc.Check = &proto.CheckConfig{CycleDays: 30}
	dir := t.TempDir()
	r.mode, r.argv = filepath.Join(dir, "mode"), filepath.Join(dir, "argv")
	r.set("ok")
	script := "#!/bin/sh\necho \"$*\" >> " + r.argv + "\n" +
		"case \"$*\" in *check*)\n" +
		"  case \"$(cat " + r.mode + ")\" in\n" +
		"    locked) echo 'unable to create lock in backend: repository is already locked by PID 7 on db01 by holdfast' >&2; exit 11 ;;\n" +
		"    damaged) echo 'error for pack 9f3c1a2b: ciphertext verification failed' >&2; echo 'Fatal: repository contains errors' >&2; exit 1 ;;\n" +
		"    hang) exec sleep 5 ;;\n" +
		"    killed) kill -9 $$ ;;\n" +
		"  esac ;;\n" +
		"esac\n" + fakeRestic[len("#!/bin/sh\n"):]
	bin := filepath.Join(dir, "restic")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	r.a.cfg.ResticBin = bin
	return r
}

func (r *checkRig) set(mode string) {
	if err := os.WriteFile(r.mode, []byte(mode), 0o644); err != nil {
		r.t.Fatal(err)
	}
}

// backup runs every job due at when, each followed by checkIfDue, as the work loop does.
func (r *checkRig) backup(when string) {
	r.t.Helper()
	r.now.Store(at(when))
	n := 0
	for {
		j, st, ok := r.a.due(r.a.now())
		if !ok {
			break
		}
		r.a.runScheduled(context.Background(), j, st)
		r.a.checkIfDue(context.Background(), st)
		n++
	}
	if n == 0 {
		r.t.Fatalf("no backup due at %s", when)
	}
}

func (r *checkRig) checks() []proto.Event {
	var out []proto.Event
	for _, ev := range spooled(r.t, r.a) {
		if ev.Kind == proto.KindCheck {
			out = append(out, ev)
		}
	}
	return out
}

// last is the newest check, by the order the agent spooled them.
func (r *checkRig) last() proto.Event {
	r.t.Helper()
	c := r.checks()
	if len(c) == 0 {
		r.t.Fatal("no check")
	}
	newest := c[0]
	for _, ev := range c {
		if ev.Finished.After(newest.Finished) || (ev.Finished.Equal(newest.Finished) && ev.ID > newest.ID) {
			newest = ev
		}
	}
	return newest
}

// subsets are the parts of the data restic was asked to read, in order.
func (r *checkRig) subsets() []string {
	b, _ := os.ReadFile(r.argv)
	var out []string
	for _, l := range strings.Split(string(b), "\n") {
		f := strings.Fields(l)
		for i := range f {
			if f[i] == "check" && i+2 < len(f) && f[i+1] == "--read-data-subset" {
				out = append(out, f[i+2])
			}
		}
	}
	return out
}

func (r *checkRig) count(want int, after string) {
	r.t.Helper()
	if n := len(r.checks()); n != want {
		r.t.Fatalf("checks after %s: %d, want %d: %+v", after, n, want, r.checks())
	}
}

func (r *checkRig) detail(ev proto.Event, want map[string]string) {
	r.t.Helper()
	for k, v := range want {
		if ev.Detail[k] != v {
			r.t.Fatalf("detail %s = %q, want %q: %+v", k, ev.Detail[k], v, ev)
		}
	}
}

// TestCheckRetriedAfterALock: a check that finds the repository locked is retried after the first backup an hour
// later, on the same part of the data; the retry that passes clears it, and does not move the daily check (decision
// 060).
func TestCheckRetriedAfterALock(t *testing.T) {
	r := newCheckRig(t)
	r.set("locked")
	r.backup("2026-11-20 10:00:00")
	r.count(1, "the first backup")
	failed := r.last()
	if failed.Status != proto.StatusFailed {
		t.Fatalf("a locked check must fail: %+v", failed)
	}
	r.detail(failed, map[string]string{"reason": "locked", "read_data_subset": "25/30", "next_check": "2026-11-20T11:00:00Z", "next_is_retry": "true"})

	r.set("ok")
	r.backup("2026-11-20 11:00:00")
	r.count(2, "the backup an hour later")
	retry := r.last()
	if retry.Status != proto.StatusSuccess {
		t.Fatalf("the retry must pass: %+v", retry)
	}
	r.detail(retry, map[string]string{"retry": "1", "retry_of": "2026-11-20T10:00:00Z", "read_data_subset": "25/30"})
	if _, ok := retry.Detail["next_check"]; ok {
		t.Fatalf("a check that passed says nothing of the next: %+v", retry.Detail)
	}

	// The retry did not move the daily check: it is due 23 hours after the one at 10:00, and reads the next part.
	r.backup("2026-11-20 12:00:00")
	r.backup("2026-11-21 08:00:00")
	r.count(2, "the day's backups")
	r.backup("2026-11-21 09:00:00")
	r.count(3, "23 hours after the daily check")
	if s := r.subsets(); s[len(s)-1] != "26/30" {
		t.Fatalf("after 25/30 passed, the daily check must read 26/30: %v", s)
	}
}

// TestCheckLockedBacksOff: a repository that stays locked is retried at most 3 times, 1, 2 and 4 hours apart; then
// the daily check comes next, and the failure says so.
func TestCheckLockedBacksOff(t *testing.T) {
	r := newCheckRig(t)
	r.set("locked")
	r.backup("2026-11-20 10:00:00")
	r.backup("2026-11-20 11:00:00")
	r.count(2, "the first retry")
	r.detail(r.last(), map[string]string{"retry": "1", "next_check": "2026-11-20T13:00:00Z", "next_is_retry": "true"})
	r.backup("2026-11-20 12:00:00")
	r.count(2, "an hour after the first retry")
	r.backup("2026-11-20 13:00:00")
	r.count(3, "two hours after the first retry")
	r.detail(r.last(), map[string]string{"retry": "2", "next_check": "2026-11-20T17:00:00Z", "next_is_retry": "true"})
	for _, h := range []string{"14", "15", "16"} {
		r.backup("2026-11-20 " + h + ":00:00")
	}
	r.count(3, "less than four hours after the second retry")
	r.backup("2026-11-20 17:00:00")
	r.count(4, "four hours after the second retry")
	r.detail(r.last(), map[string]string{"retry": "3", "next_check": "2026-11-21T09:00:00Z", "next_is_retry": "false"})
	r.backup("2026-11-20 18:00:00")
	r.backup("2026-11-21 08:00:00")
	r.count(4, "the third retry, before the daily check")
}

// TestCheckDamageRetriedOnceOnTheSamePart: a check that finds damage is retried once, on the same part of the data,
// and fails again; the next daily check reads that part too, not the date's, and only a pass moves on.
func TestCheckDamageRetriedOnceOnTheSamePart(t *testing.T) {
	r := newCheckRig(t)
	r.set("damaged")
	r.backup("2026-11-20 10:00:00")
	r.detail(r.last(), map[string]string{"reason": "damaged", "next_is_retry": "true", "next_check": "2026-11-20T11:00:00Z"})
	r.backup("2026-11-20 11:00:00")
	r.count(2, "the retry")
	r.detail(r.last(), map[string]string{"reason": "damaged", "retry": "1", "next_check": "2026-11-21T09:00:00Z", "next_is_retry": "false"})
	r.backup("2026-11-20 12:00:00")
	r.backup("2026-11-20 13:00:00")
	r.count(2, "the one retry an error gets")

	r.set("ok")
	r.backup("2026-11-21 09:00:00") // day 325, whose own part is 26/30
	r.count(3, "the daily check")
	r.backup("2026-11-22 08:00:00")
	r.count(4, "the next daily check")
	if got, want := strings.Join(r.subsets(), " "), "25/30 25/30 25/30 26/30"; got != want {
		t.Fatalf("parts read: %s, want %s", got, want)
	}
}

// TestCheckTimeoutAndKillAreNotRetried: a check that ran out of time or whose restic was killed would only cost as
// much again; the daily check comes next.
func TestCheckTimeoutAndKillAreNotRetried(t *testing.T) {
	for _, tc := range []struct{ mode, reason string }{{"hang", "timeout"}, {"killed", "killed"}} {
		t.Run(tc.mode, func(t *testing.T) {
			r := newCheckRig(t)
			if tc.mode == "hang" {
				r.a.runTimeout = func(schedule.Schedule) time.Duration { return 300 * time.Millisecond }
			}
			r.set(tc.mode)
			r.backup("2026-11-20 10:00:00")
			r.detail(r.last(), map[string]string{"reason": tc.reason, "next_is_retry": "false", "next_check": "2026-11-21T09:00:00Z"})
			r.set("ok")
			r.backup("2026-11-20 11:00:00")
			r.backup("2026-11-20 15:00:00")
			r.count(1, "the hours after")
		})
	}
}

// TestCheckRetrySurvivesRestarts: a retry that is due survives a restart, and an agent stopped during a retry neither
// retries at once when it starts again nor loses the retries left.
func TestCheckRetrySurvivesRestarts(t *testing.T) {
	r := newCheckRig(t)
	r.set("locked")
	r.backup("2026-11-20 10:00:00")
	restart := func() {
		t.Helper()
		b, err := New(r.a.cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
		if err != nil {
			t.Fatal(err)
		}
		b.now, b.hc, b.runTimeout = r.a.now, r.a.hc, r.a.runTimeout
		r.a = b
	}

	// Stopped during the first retry: the agent's context ends while restic hangs.
	r.set("hang")
	r.now.Store(at("2026-11-20 11:00:00"))
	j, st, ok := r.a.due(r.a.now())
	if !ok {
		t.Fatal("no backup due at 11:00")
	}
	r.a.runScheduled(context.Background(), j, st)
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.a.checkIfDue(ctx, st); close(done) }()
	waitFor(t, "the retry to start", 5*time.Second, func() bool { _, err := os.Stat(r.a.path("running.json")); return err == nil })
	stop()
	<-done
	r.count(1, "a retry stopped with the agent")

	restart()
	r.set("ok")
	r.backup("2026-11-20 12:00:00")
	r.count(1, "a restart an hour after the stopped retry")
	r.backup("2026-11-20 13:00:00") // the stopped retry was the first: the second waits two hours from its start
	r.count(2, "two hours after the stopped retry")
	r.detail(r.last(), map[string]string{"retry": "2", "retry_of": "2026-11-20T10:00:00Z"})
	if r.last().Status != proto.StatusSuccess {
		t.Fatalf("the retry after the restart must pass: %+v", r.last())
	}
}

// TestCheckOnRequestReadsThePendingPart: a check on request reads the part that has not passed, a pass clears the
// retry, and it does not move the daily check.
func TestCheckOnRequestReadsThePendingPart(t *testing.T) {
	r := newCheckRig(t)
	r.set("damaged")
	r.backup("2026-11-20 10:00:00")
	r.set("ok")
	r.now.Store(at("2026-11-20 10:30:00"))
	r.a.execCommand(context.Background(), proto.Command{ID: "c1", Kind: proto.CommandCheckNow})
	r.count(2, "a check on request")
	if c := r.last(); c.Status != proto.StatusSuccess || c.CommandID != "c1" || c.Detail["read_data_subset"] != "25/30" {
		t.Fatalf("the check on request must read the part that failed, and pass: %+v", c)
	}
	r.backup("2026-11-20 11:00:00")
	r.count(2, "the hour the retry would have been due")
	r.backup("2026-11-21 09:00:00")
	r.count(3, "23 hours after the daily check")
	if s := r.subsets(); s[len(s)-1] != "26/30" {
		t.Fatalf("the daily check after the pass must read the next part: %v", s)
	}
}

// TestCheckOnceForJobsSharingASlot: with two jobs in one slot, a retry runs after the first, not after each.
func TestCheckOnceForJobsSharingASlot(t *testing.T) {
	r := newCheckRig(t)
	k := r.a.hc.Jobs[0]
	k.ID = "k"
	r.a.hc.Jobs = append(r.a.hc.Jobs, k)
	r.set("locked")
	r.backup("2026-11-20 10:00:00")
	r.count(1, "two backups in one slot")
	r.backup("2026-11-20 11:00:00")
	r.count(2, "the retry slot, with two backups")
}

// TestCheckCycleChangeDropsThePart: a part that has not passed is dropped when the cycle changes under it (restic
// refuses 25/10); the check reads the date's part of the new cycle.
func TestCheckCycleChangeDropsThePart(t *testing.T) {
	r := newCheckRig(t)
	r.set("damaged")
	r.backup("2026-11-20 10:00:00")
	r.a.hc.Check.CycleDays = 10
	r.backup("2026-11-20 11:00:00")
	if got, want := strings.Join(r.subsets(), " "), "25/30 5/10"; got != want {
		t.Fatalf("parts read: %s, want %s", got, want)
	}
}

// TestCheckFileFromAnOlderAgent: check.json with last only, as agents before decision 060 wrote it, loads with
// nothing pending.
func TestCheckFileFromAnOlderAgent(t *testing.T) {
	r := newCheckRig(t)
	if err := os.WriteFile(r.a.path("check.json"), []byte(`{"last":"2026-11-20T09:30:00Z"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	b, err := New(r.a.cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if !b.check.Last.Equal(time.Date(2026, 11, 20, 9, 30, 0, 0, time.UTC)) || b.check.Part != "" || !b.check.RetryAt.IsZero() {
		t.Fatalf("check.json from an older agent: %+v", b.check)
	}
}
