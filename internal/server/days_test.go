package server

import (
	"testing"
	"time"

	"github.com/kabsdk/holdfast/internal/server/store"
)

// The strip of days: each backup on the day it finished in the zone asked for, the worst run sets the day, a skipped
// run and another job's run are left out, and a day without a run is there, empty; marked Before when it ended before
// the job's first backup, as every day of a job that has none is.
func TestDayStrips(t *testing.T) {
	cph, err := time.LoadLocation("Europe/Copenhagen")
	if err != nil {
		t.Skip("no time zone data:", err)
	}
	from := time.Date(2026, 11, 22, 0, 0, 0, 0, cph)
	at := func(day, hour int) time.Time { return time.Date(2026, 11, day, hour, 5, 0, 0, cph) }
	outs := []store.Outcome{
		{JobID: "system", Status: "success", Finished: at(22, 1)},
		// 23:30 UTC on the 22nd is 00:30 on the 23rd in Copenhagen.
		{JobID: "system", Status: "failed", Finished: time.Date(2026, 11, 22, 23, 30, 0, 0, time.UTC)},
		{JobID: "system", Status: "success", Finished: at(23, 2)},
		{JobID: "logs", Status: "warning", Finished: at(23, 3)},
		{JobID: "logs", Status: "success", Finished: at(23, 4)},
		{JobID: "logs", Status: "skipped", Finished: at(24, 4)},
		{JobID: "logs", Status: "interrupted", Finished: at(24, 5)},
		{JobID: "gone", Status: "success", Finished: at(24, 6)},
	}
	first := map[string]time.Time{"system": at(1, 1), "logs": at(23, 3)}
	got := dayStrips([]string{"system", "logs", "new"}, outs, first, from, 3)
	if got.from != "2026-11-22" || got.to != "2026-11-24" || len(got.jobs) != 3 {
		t.Fatalf("range %s to %s, %d jobs", got.from, got.to, len(got.jobs))
	}
	want := map[string][]DayRuns{
		"system": {
			{Date: "2026-11-22", Runs: 1, Success: 1, Worst: "success"},
			{Date: "2026-11-23", Runs: 2, Success: 1, Failed: 1, Worst: "failed"},
			{Date: "2026-11-24"},
		},
		"logs": {
			{Date: "2026-11-22", Before: true},
			{Date: "2026-11-23", Runs: 2, Success: 1, Warning: 1, Worst: "warning"},
			{Date: "2026-11-24", Runs: 1, Failed: 1, Worst: "failed"},
		},
		"new": {
			{Date: "2026-11-22", Before: true},
			{Date: "2026-11-23", Before: true},
			{Date: "2026-11-24", Before: true},
		},
	}
	for _, j := range got.jobs {
		for i, d := range j.Days {
			if d != want[j.JobID][i] {
				t.Errorf("%s day %d: got %+v, want %+v", j.JobID, i, d, want[j.JobID][i])
			}
		}
	}
}
