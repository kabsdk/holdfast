package store

import (
	"context"
	"testing"
	"time"
)

// TestDeletionMarks: Holdfast's own deletions get tombstones that never count as disappeared, even when a
// refresh comes between the mark and the forget; and a snapshot that had disappeared outside Holdfast
// before keeps being reported, even if Holdfast meant to delete it too.
func TestDeletionMarks(t *testing.T) {
	s, ctx := testStore(t), context.Background()
	q := s.Q()
	const fqdn = "web01.example.com"
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	r, _ := q.SaveRevision(ctx, SaveRevision{Base: 0, Actor: "local:root", Source: "cli", Message: "add", Config: docWith(fqdn)})
	must(t, q.CreateHost(ctx, fqdn, r.Number, now))
	snap := func(id string) Snapshot {
		return Snapshot{FQDN: fqdn, ID: id, ShortID: id[:8], Time: now, JobID: "data"}
	}
	x, y, z := "xxxxxxxx1", "yyyyyyyy2", "zzzzzzzz3"
	must2(t, func() (int, int, error) {
		return q.SyncSnapshots(ctx, fqdn, []Snapshot{snap(x), snap(y), snap(z)}, now)
	})

	// x disappears outside Holdfast.
	if _, gone, err := q.SyncSnapshots(ctx, fqdn, []Snapshot{snap(y), snap(z)}, now); err != nil || gone != 1 {
		t.Fatalf("x disappeared: gone=%d %v", gone, err)
	}
	// Holdfast forgets x (a deletion asked for earlier) and y; a refresh runs between the mark and the forget.
	must(t, q.MarkDeleting(ctx, fqdn, []string{x, y}))
	if _, gone, err := q.SyncSnapshots(ctx, fqdn, []Snapshot{snap(z)}, now); err != nil || gone != 0 {
		t.Fatalf("a snapshot Holdfast is deleting must not count as gone: gone=%d %v", gone, err)
	}
	must(t, q.MarkDeleted(ctx, fqdn, []string{x, y}, now))

	counts, err := q.CountCatalogue(ctx)
	must(t, err)
	if c := counts[fqdn]; c.Disappeared != 1 || c.Snapshots != 1 {
		t.Fatalf("x still disappeared, y deleted by Holdfast, z there: %+v", c)
	}
	for id, want := range map[string]bool{x: false, y: true} {
		sn, err := q.GetSnapshot(ctx, fqdn, id)
		if err != nil || sn.GoneAt == nil || sn.DeletedByUs != want {
			t.Fatalf("%s: deleted_by_holdfast=%v, want %v (%v)", id, sn.DeletedByUs, want, err)
		}
	}
}

func must2(t *testing.T, f func() (int, int, error)) {
	t.Helper()
	if _, _, err := f(); err != nil {
		t.Fatal(err)
	}
}
