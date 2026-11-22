package storage

import (
	"slices"
	"testing"
	"time"
)

// Decision 038: each object goes back to its version at that time; locks/ is left alone.
func TestPlanRollback(t *testing.T) {
	const p = "repos/web01.example.com/"
	t0 := time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)
	at := func(h int) time.Time { return t0.Add(time.Duration(h) * time.Hour) }
	v := func(key, id string, h int, latest, marker bool) ObjectVersion {
		return ObjectVersion{Key: p + key, VersionID: id, LastModified: at(h), IsLatest: latest, IsDeleteMarker: marker}
	}
	versions := []ObjectVersion{
		v("config", "c1", 0, true, false),                                                    // unchanged
		v("keys/k1", "k1", 0, true, false),                                                   // unchanged
		v("snapshots/aaaa", "a1", 1, false, false), v("snapshots/aaaa", "a2", 5, true, true), // deleted at 5: comes back
		v("snapshots/bbbb", "b1", 6, true, false),                                 // made at 6: set aside
		v("index/i1", "i1", 1, false, false), v("index/i1", "i2", 7, true, false), // overwritten at 7: the old version comes back
		v("data/d1", "d1", 6, false, false), v("data/d1", "d2", 7, true, true), // made and deleted after: nothing to do
		v("locks/l1", "l1", 6, true, false),                                                   // a lock: left alone
		v("snapshots/cccc", "x1", 1, false, false), v("snapshots/cccc", "x2", 2, false, true), // gone before then: stays gone
		v("snapshots/cccc", "x3", 8, true, false), // and written again after: set aside
	}
	plan := PlanRollback(p, versions, at(4))
	back := map[string]string{}
	for _, s := range plan.BringBack {
		back[s.Key] = s.VersionID
	}
	if len(back) != 2 || back[p+"snapshots/aaaa"] != "a1" || back[p+"index/i1"] != "i1" {
		t.Fatalf("brought back: %+v", plan.BringBack)
	}
	var aside []string
	for _, s := range plan.SetAside {
		aside = append(aside, s.Key)
	}
	if !slices.Equal(aside, []string{p + "snapshots/bbbb", p + "snapshots/cccc"}) {
		t.Fatalf("set aside: %v", aside)
	}
	if plan.Unchanged != 3 { // config, keys/k1, data/d1
		t.Fatalf("unchanged: %d", plan.Unchanged)
	}
	if got := SnapshotIDs(plan.BringBack); !slices.Equal(got, []string{"aaaa"}) {
		t.Fatalf("snapshots back: %v", got)
	}
	if len(plan.Deletions) != 2 || !plan.Deletions[0].Equal(at(2)) || !plan.Deletions[1].Equal(at(5)) {
		t.Fatalf("snapshot deletions: %v", plan.Deletions)
	}

	// Rolling the rollback back: after it ran, the repository goes back to how it was just before.
	m := &MemBucket{Objects: map[string][]ObjectVersion{}}
	for _, x := range versions {
		x.IsLatest = false
		m.Objects[x.Key] = append(m.Objects[x.Key], x)
	}
	m.Clock = func() time.Time { return at(10) }
	for _, s := range plan.BringBack {
		_ = m.CopyVersion(nil, s.Key, s.VersionID)
	}
	for _, s := range plan.SetAside {
		_ = m.SetAside(nil, s.Key)
	}
	vs, _ := m.Versions(nil, "web01.example.com")
	// Just before it, aaaa was gone and i1, bbbb and cccc were their newer versions.
	undo := PlanRollback(p, vs, at(9))
	back = map[string]string{}
	for _, s := range undo.BringBack {
		back[s.Key] = s.VersionID
	}
	if len(back) != 3 || back[p+"index/i1"] != "i2" || back[p+"snapshots/bbbb"] != "b1" || back[p+"snapshots/cccc"] != "x3" ||
		len(undo.SetAside) != 1 || undo.SetAside[0].Key != p+"snapshots/aaaa" {
		t.Fatalf("undoing the rollback: back %+v, aside %+v", undo.BringBack, undo.SetAside)
	}
}
