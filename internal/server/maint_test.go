package server

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kabsdk/holdfast/internal/proto"
	"github.com/kabsdk/holdfast/internal/schedule"
)

type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) Now() time.Time  { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *testClock) Set(t time.Time) { c.mu.Lock(); defer c.mu.Unlock(); c.t = t }
func (c *testClock) At(s string) time.Time {
	cph, _ := time.LoadLocation("Europe/Copenhagen")
	t, err := time.ParseInLocation("2006-01-02 15:04:05.000", s, cph)
	if err != nil {
		panic(err)
	}
	c.Set(t)
	return t
}

type checkRun struct {
	fqdn, status, err string
	started           time.Time
}

func checkRuns(t *testing.T, s *Server) []checkRun {
	t.Helper()
	rows, err := s.st.Pool().Query(context.Background(), `SELECT fqdn, status, error, started_at FROM maintenance_runs WHERE kind = 'check' ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []checkRun
	for rows.Next() {
		var r checkRun
		if err := rows.Scan(&r.fqdn, &r.status, &r.err, &r.started); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

// TestDailyCheck: each host is checked once a day, in the middle of the day's longest quiet stretch, and
// never once that stretch has ended (plan-m1 §10).
func TestDailyCheck(t *testing.T) {
	clock := &testClock{}
	clock.At("2026-11-23 08:00:00.000") // a Monday
	s, ctx := newTestServer(t, clock.Now), context.Background()

	// Runs at 02:00 and 20:00: the longest quiet stretch is 02:00 to 20:00, so the check is at 11:00.
	j := job("/etc")
	j.Schedule = schedule.Schedule{Daily: &schedule.Daily{Times: []schedule.ClockTime{"02:00", "20:00"}}}
	res, err := s.AddHost(ctx, root, AddHostInput{FQDN: "web01.example.com", JobID: "system", Job: j})
	if err != nil {
		t.Fatal(err)
	}
	if h, _ := s.Host(ctx, "web01.example.com"); h.NextCheck != nil {
		t.Fatalf("a host that is not enrolled has no check planned: %v", h.NextCheck)
	}
	if _, err := s.Enroll(ctx, proto.EnrollRequest{FQDN: "web01.example.com", Code: res.Enrollment.Code}); err != nil {
		t.Fatal(err)
	}
	wantNext := func(when string) {
		t.Helper()
		h, err := s.Host(ctx, "web01.example.com")
		if err != nil || h.NextCheck == nil || h.NextCheck.In(clock.Now().Location()).Format("2006-01-02 15:04") != when {
			t.Fatalf("next check: got %v (%v), want %s", h.NextCheck, err, when)
		}
	}
	tick := func(at string) {
		t.Helper()
		clock.At(at)
		s.startDueChecks(ctx)
		s.checks.Wait()
	}
	wantNext("2026-11-23 11:00")

	tick("2026-11-23 10:59:00.000")
	if n := len(checkRuns(t, s)); n != 0 {
		t.Fatalf("a check started before its time: %d runs", n)
	}
	tick("2026-11-23 11:00:00.000")
	runs := checkRuns(t, s)
	if len(runs) != 1 || runs[0].status != "success" || !runs[0].started.Equal(clock.Now()) {
		t.Fatalf("the 11:00 check: %+v", runs)
	}
	tick("2026-11-23 11:05:00.000")
	if n := len(checkRuns(t, s)); n != 1 {
		t.Fatalf("a host was checked twice in a day: %d runs", n)
	}
	wantNext("2026-11-24 11:00")

	// The server was down all Tuesday's quiet stretch: no check runs into the 20:00 backup.
	tick("2026-11-24 20:30:00.000")
	if n := len(checkRuns(t, s)); n != 1 {
		t.Fatalf("a check started after the quiet stretch ended: %d runs", n)
	}
	wantNext("2026-11-25 11:00")

	// A check by hand counts as the day's check.
	clock.At("2026-11-25 09:00:00.000")
	if out := s.RunChecks(ctx, root); len(out) != 1 || out[0].Status != "success" {
		t.Fatalf("holdfast check: %+v", out)
	}
	tick("2026-11-25 11:00:00.000")
	if n := len(checkRuns(t, s)); n != 2 {
		t.Fatalf("after a check by hand: %d runs, want 2", n)
	}

	// One check per host at a time, whoever starts it.
	if !s.claimCheck("web01.example.com") {
		t.Fatal("claim")
	}
	if out := s.RunChecks(ctx, root); len(out) != 1 || out[0].Status != "skipped" {
		t.Fatalf("a second check of the same host: %+v", out)
	}
	s.releaseCheck("web01.example.com")

	// No free slot before the quiet stretch ends: skipped for the day, and recorded.
	for range parallelChecks {
		s.checkSlots <- struct{}{}
	}
	tick("2026-11-26 19:59:59.900")
	for range parallelChecks {
		<-s.checkSlots
	}
	runs = checkRuns(t, s)
	if last := runs[len(runs)-1]; last.status != "skipped" || !strings.Contains(last.err, "quiet stretch ended") {
		t.Fatalf("a check that found no slot: %+v", last)
	}
	tick("2026-11-26 19:59:59.950")
	if n := len(checkRuns(t, s)); n != len(runs) {
		t.Fatal("a skipped check was retried the same day")
	}

	audit, err := s.st.Q().ListAudit(ctx, "web01.example.com", 100)
	if err != nil {
		t.Fatal(err)
	}
	var system, byHand int
	for _, a := range audit {
		if a.Action == "maintenance.check" {
			switch a.Actor {
			case systemActor.Name:
				system++
			case root.Name:
				byHand++
			}
		}
	}
	if system != 2 || byHand != 1 {
		t.Fatalf("audited checks: %d by the server, %d by hand; want 2 and 1", system, byHand)
	}
}

// TestInterruptedCheckIsRecovered: a check the server never finished is marked interrupted at start-up,
// and restic unlock removes the stale lock it left (unlock only ever removes stale locks).
func TestInterruptedCheckIsRecovered(t *testing.T) {
	clock := &testClock{}
	start := clock.At("2026-11-23 08:00:00.000")
	s, ctx := newTestServer(t, clock.Now), context.Background()
	res, err := s.AddHost(ctx, root, AddHostInput{FQDN: "web01.example.com", JobID: "system", Job: job("/etc")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Enroll(ctx, proto.EnrollRequest{FQDN: "web01.example.com", Code: res.Enrollment.Code}); err != nil {
		t.Fatal(err)
	}
	q := s.st.Q()
	if _, err := q.StartMaintenance(ctx, "web01.example.com", "check", "1/30", "test", start.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	// A check this process started itself is running, not interrupted.
	if _, err := q.StartMaintenance(ctx, "web01.example.com", "check", "1/30", "test", start.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	argv := filepath.Join(t.TempDir(), "argv")
	s.set.ResticBin = filepath.Join(t.TempDir(), "restic")
	if err := os.WriteFile(s.set.ResticBin, []byte("#!/bin/sh\necho \"$*\" >> "+argv+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	s.recoverInterruptedChecks(ctx)
	runs := checkRuns(t, s)
	if len(runs) != 2 || runs[0].status != "interrupted" || runs[1].status != "" {
		t.Fatalf("after recovery: %+v", runs)
	}
	if b, _ := os.ReadFile(argv); strings.TrimSpace(string(b)) != "unlock" {
		t.Fatalf("restic was run as %q; want exactly one plain unlock", b)
	}
}
