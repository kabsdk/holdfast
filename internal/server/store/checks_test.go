package store

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// TestLastChecks is what HoldfastCheckFailed reads (decisions 059 and 060): the newest check that ended, passed or
// failed. A check stopped with the agent neither raises nor clears a failure, and a failure that may be damage stays
// an error when its retry meets a lock, until a check passes.
func TestLastChecks(t *testing.T) {
	s, ctx := testStore(t), context.Background()
	q := s.Q()
	const host = "db01.example.com"
	t0 := time.Date(2026, 11, 27, 18, 30, 0, 0, time.UTC)
	r, _ := q.SaveRevision(ctx, SaveRevision{Base: 0, Actor: "local:root", Source: "cli", Message: "add", Config: docWith(host)})
	must(t, q.CreateHost(ctx, host, r.Number, t0))
	n := 0
	check := func(status string, exit int) {
		t.Helper()
		n++
		at := t0.Add(time.Duration(n) * time.Hour)
		if _, err := q.InsertEvent(ctx, Event{FQDN: host, ID: fmt.Sprintf("c%d", n), Kind: "check", Status: status, Started: at, Finished: at, ResticExit: exit}); err != nil {
			t.Fatal(err)
		}
	}
	want := func(what string, failed bool, exit int, errored bool) {
		t.Helper()
		got, err := q.LastChecks(ctx)
		must(t, err)
		if c := got[host]; c.Failed != failed || c.ResticExit != exit || c.Errored != errored {
			t.Fatalf("%s: %+v, want failed=%v exit=%d errored=%v", what, c, failed, exit, errored)
		}
	}

	check("success", 0)
	want("a check that passed", false, 0, false)
	check("failed", 11)
	want("a check that met a lock", true, 11, false)
	check("interrupted", -1)
	want("then one stopped with the agent", true, 11, false)
	check("success", 0)
	want("then one that passed", false, 0, false)
	check("failed", 1)
	want("a check that found damage", true, 1, true)
	check("failed", 11)
	want("its retry, which met a lock", true, 11, true)
	check("interrupted", -1)
	want("then one stopped with the agent", true, 11, true)
	check("success", 0)
	want("then one that passed", false, 0, false)
}
