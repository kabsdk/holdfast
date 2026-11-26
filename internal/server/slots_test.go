package server

import (
	"strings"
	"testing"
	"time"

	"github.com/kabsdk/holdfast/internal/schedule"
	"github.com/kabsdk/holdfast/internal/server/store"
)

// Each slot of the schedule gets the backup that ran for it, its retries counted and the last one's result kept; a
// slot with none is missed, or due when it is the newest; one before the job's first backup had nothing to miss.
func TestJobSlots(t *testing.T) {
	daily := schedule.Schedule{Daily: &schedule.Daily{Times: []schedule.ClockTime{"02:00"}}}
	at := func(day, h int) time.Time { return time.Date(2026, 11, day, h, 0, 0, 0, time.UTC) }
	run := func(day int, status string, fin int) store.SlotRun {
		return store.SlotRun{ScheduledFor: at(day, 2), Status: status, Finished: at(day, fin), SnapshotID: "snap" + status}
	}
	runs := []store.SlotRun{
		run(20, "success", 3),
		run(21, "failed", 3), run(21, "success", 4), // retried
		// none on the 22nd
		run(23, "skipped", 2),
		run(24, "warning", 3),
		run(25, "interrupted", 3),
		// none yet on the 26th
	}
	now := at(26, 12)
	got := jobSlots(daily, time.UTC, runs, at(19, 3), true, now, 7)
	var states []string
	for _, s := range got {
		states = append(states, s.State)
	}
	if want := "success,success,missed,skipped,warning,failed,due"; strings.Join(states, ",") != want {
		t.Fatalf("slots: %s, want %s", strings.Join(states, ","), want)
	}
	if !got[0].At.Equal(at(20, 2)) || got[1].Runs != 2 || got[1].SnapshotID != "snapsuccess" || got[2].Runs != 0 {
		t.Fatalf("the slots' times, runs and snapshots: %+v", got)
	}
	// A job whose first backup came on the 23rd had nothing to miss before it; one with none has had nothing yet.
	got = jobSlots(daily, time.UTC, runs[3:], at(23, 1), true, now, 7)
	if got[0].State != SlotBefore || got[2].State != SlotBefore || got[3].State != SlotSkipped {
		t.Fatalf("before the first backup: %+v", got)
	}
	if got := jobSlots(daily, time.UTC, nil, time.Time{}, false, now, 3); got[0].State != SlotBefore || got[2].State != SlotBefore {
		t.Fatalf("a job with no backup yet: %+v", got)
	}
	// A weekly job's 7 slots cover 7 weeks.
	weekly := schedule.Schedule{Weekly: &schedule.Weekly{Days: []string{"sun"}, At: "03:00"}}
	if got := jobSlots(weekly, time.UTC, nil, at(1, 0).AddDate(0, -3, 0), true, now, 7); len(got) != 7 || now.Sub(got[0].At) < 6*7*24*time.Hour {
		t.Fatalf("a weekly job's 7 slots: %+v", got)
	}
}
