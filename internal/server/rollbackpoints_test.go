package server

import (
	"testing"
	"time"

	"github.com/kabsdk/holdfast/internal/server/storage"
)

// The moments a rollback can undo: each of Holdfast's runs that deleted something is one, named after it; the rest
// part where they are quiet for 10 minutes, as not Holdfast's; an overwrite is never Holdfast's. Newest first, each to
// be rolled back to just before it.
func TestRollbackPoints(t *testing.T) {
	t0 := time.Date(2026, 11, 20, 3, 0, 0, 0, time.UTC)
	at := func(min int) time.Time { return t0.Add(time.Duration(min) * time.Minute) }
	const p = "repos/web01.example.com/"
	ch := func(key, kind string, min int, over bool) storage.Change {
		return storage.Change{Key: p + key, Kind: kind, At: at(min), Overwritten: over}
	}
	changes := []storage.Change{
		ch("snapshots/aaaa", "snapshots", 0, false), ch("data/00/01", "data", 2, false), // the daily run, 03:00–03:05
		ch("snapshots/bbbb", "snapshots", 60, false), ch("snapshots/cccc", "snapshots", 65, false), // behind Holdfast's back
		ch("index/i1", "index", 300, true),            // overwritten, during a Prune now: still not Holdfast's
		ch("snapshots/dddd", "snapshots", 301, false), // Prune now, by ada
	}
	wins := []runWindow{
		{at(0), at(1), "scheduled", "system"}, // its last change lands within a minute of its recorded end
		{at(299), at(310), "prune_now", "ada"},
	}
	pts := rollbackPoints(changes, wins, map[string]string{"aaaa": "system", "bbbb": "db", "cccc": "db"})
	if len(pts) != 4 {
		t.Fatalf("points: %+v", pts)
	}
	if p := pts[0]; p.By != "prune_now" || p.Actor != "ada" || p.Deleted["snapshots"] != 1 || !p.To.Equal(at(301).Add(-time.Second)) {
		t.Errorf("Prune now, newest: %+v", p)
	}
	if p := pts[1]; p.By != "" || p.Overwritten["index"] != 1 || !p.At.Equal(at(300)) {
		t.Errorf("the overwrite: %+v", p)
	}
	if p := pts[2]; p.By != "" || p.Deleted["snapshots"] != 2 || len(p.Jobs) != 1 || p.Jobs[0] != "db" || !p.At.Equal(at(60)) {
		t.Errorf("behind Holdfast's back, one moment: %+v", p)
	}
	if p := pts[3]; p.By != "scheduled" || p.Deleted["snapshots"] != 1 || p.Deleted["data"] != 1 || p.Snapshots[0] != "aaaa" {
		t.Errorf("the daily run: %+v", p)
	}
}
