package server

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/kabsdk/holdfast/internal/config"
)

// A job in the API may leave things out (plan-m10 item 4).
func TestMergeJob(t *testing.T) {
	def := config.DefaultJob()

	// Only paths: everything else from the default job.
	j, err := mergeJob(def, json.RawMessage(`{"paths": ["/etc"]}`), true)
	if err != nil || !slices.Equal(j.Paths, []string{"/etc"}) || j.Schedule.Daily == nil || !j.Schedule.Spread.Enabled ||
		j.Retention.Yearly == nil || j.Retention.KeepLast != config.DefaultKeepLast || j.Engine != config.EngineRestic {
		t.Fatalf("paths only: %+v %v", j, err)
	}

	// A new job given a schedule form takes that schedule whole: the default's 20-minute spread would be too much
	// for every 1m (S001).
	fresh, err := mergeJob(def, json.RawMessage(`{"paths": ["/etc"], "schedule": {"every": {"n": 1, "unit": "minutes", "from": "00:00"}}}`), true)
	if err != nil || fresh.Schedule.Every == nil || fresh.Schedule.Daily != nil || fresh.Schedule.Spread.Enabled {
		t.Fatalf("a new job with a schedule: %+v %v", fresh.Schedule, err)
	}

	// An existing job given a schedule form replaces the form and keeps its spread and zone.
	base := j
	base.Schedule.Zone = "Europe/Copenhagen"
	j, err = mergeJob(base, json.RawMessage(`{"schedule": {"weekly": {"days": ["sun"], "at": "03:00"}}}`), false)
	if err != nil || j.Schedule.Daily != nil || j.Schedule.Weekly == nil || j.Schedule.Zone != "Europe/Copenhagen" || !j.Schedule.Spread.Enabled ||
		!slices.Equal(j.Paths, []string{"/etc"}) {
		t.Fatalf("a new form: %+v %v", j.Schedule, err)
	}

	// A spread alone changes the spread only.
	j, err = mergeJob(base, json.RawMessage(`{"schedule": {"spread": {"enabled": false, "minutes": 20}}}`), false)
	if err != nil || j.Schedule.Daily == nil || j.Schedule.Spread.Enabled {
		t.Fatalf("a spread alone: %+v %v", j.Schedule, err)
	}

	// A retention replaces the whole retention.
	j, err = mergeJob(base, json.RawMessage(`{"retention": {"daily": {"n": 30, "unit": "days"}, "keep_last": 3}}`), false)
	if err != nil || j.Retention.Daily == nil || j.Retention.Daily.N != 30 || j.Retention.Yearly != nil || j.Schedule.Daily == nil {
		t.Fatalf("a retention: %+v %v", j.Retention, err)
	}

	// A dump drops the paths, and paths drop the dump.
	j, err = mergeJob(base, json.RawMessage(`{"dump": "postgresql"}`), false)
	if err != nil || j.Dump != "postgresql" || len(j.Paths) != 0 {
		t.Fatalf("a dump: %+v %v", j, err)
	}
	j, err = mergeJob(j, json.RawMessage(`{"paths": ["/srv"]}`), false)
	if err != nil || j.Dump != "" || !slices.Equal(j.Paths, []string{"/srv"}) {
		t.Fatalf("paths after a dump: %+v %v", j, err)
	}

	// A field the API does not know is refused, not ignored.
	if _, err := mergeJob(base, json.RawMessage(`{"pathz": ["/etc"]}`), false); err == nil {
		t.Fatal("an unknown field was taken")
	}
}
