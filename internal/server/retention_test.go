package server

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kabsdk/holdfast/internal/config"
	"github.com/kabsdk/holdfast/internal/schedule"
	"github.com/kabsdk/holdfast/internal/server/store"
)

var (
	snapA = strings.Repeat("a", 64)
	snapB = strings.Repeat("b", 64)
	snapC = strings.Repeat("c", 64)
	snapD = strings.Repeat("d", 64) // not made by Holdfast
)

func snapJSON(id, t string) string {
	return `{"id":"` + id + `","short_id":"` + id[:8] + `","time":"` + t + `","hostname":"web01.example.com","tags":["job=data"]}`
}

// retentionRestic answers like restic for one job with four snapshots: "keep the newest 1" keeps C, and
// any other policy keeps all four. check exits with the number in check-exit; prune reports 1.500 KiB freed.
func retentionRestic(t *testing.T) (bin, log, dir string) {
	dir = t.TempDir()
	log = filepath.Join(dir, "log")
	a, b, c, d := snapJSON(snapA, "2026-11-17T02:00:00Z"), snapJSON(snapB, "2026-11-18T02:00:00Z"), snapJSON(snapC, "2026-11-20T02:00:00Z"), snapJSON(snapD, "2026-11-19T02:00:00Z")
	keep1 := `[{"host":"web01.example.com","keep":[` + c + `],"remove":[` + a + `,` + b + `,` + d + `]}]`
	keepAll := `[{"host":"web01.example.com","keep":[` + c + `,` + d + `,` + b + `,` + a + `],"remove":[]}]`
	script := `#!/bin/sh
echo "ARGS $*" >> ` + log + `
case "$*" in
*"cat config"*) exit 10 ;;
*"forget --dry-run"*"--keep-last 1") echo '` + keep1 + `' ;;
*"forget --dry-run"*) echo '` + keepAll + `' ;;
*" check") exit $(cat ` + dir + `/check-exit 2>/dev/null || echo 0) ;;
*" prune") printf 'total prune:           3 blobs / 1.500 KiB\nremaining:            10 blobs / 5.724 MiB\n' ;;
esac
exit 0
`
	bin = filepath.Join(dir, "restic")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, log, dir
}

func waitRunDone(t *testing.T, s *Server, fqdn string, id int64) store.MaintenanceRun {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		runs, _ := s.st.Q().ListMaintenanceRuns(context.Background(), fqdn, 100)
		for _, r := range runs {
			if r.ID == id && r.Status != "running" {
				return r
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("run %d did not finish", id)
	return store.MaintenanceRun{}
}

func TestRetentionProtectionPruneAndDelete(t *testing.T) {
	var offset time.Duration
	s := newTestServer(t, func() time.Time { return time.Now().Add(offset) })
	bin, log, dir := retentionRestic(t)
	s.set.ResticBin = bin
	ctx := context.Background()
	const fqdn = "web01.example.com"
	keep := func(n int) *config.Job {
		j := job("/srv")
		j.Retention = config.Retention{KeepLast: n}
		return j
	}
	accept := []string{"R101", "R104"}
	if _, err := s.AddHost(ctx, root, AddHostInput{FQDN: fqdn, JobID: "data", Job: keep(10), Accept: accept}); err != nil {
		t.Fatal(err)
	}
	// Three snapshots an agent reported, and one it did not: D was made outside Holdfast.
	q := s.st.Q()
	var rows []store.Snapshot
	for i, id := range []string{snapA, snapB, snapC, snapD} {
		rows = append(rows, store.Snapshot{FQDN: fqdn, ID: id, ShortID: id[:8], Time: time.Now().Add(-time.Duration(4-i) * 24 * time.Hour), JobID: "data"})
		if id != snapD {
			if _, err := q.InsertEvent(ctx, store.Event{FQDN: fqdn, ID: "ev-" + id[:4], Kind: "backup", JobID: "data", Status: "success", SnapshotID: id,
				Started: time.Now(), Finished: time.Now()}); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, _, err := q.SyncSnapshots(ctx, fqdn, rows, time.Now()); err != nil {
		t.Fatal(err)
	}

	// A cut needs R104, decided from restic's dry runs.
	var ce *ChecksError
	if _, err := s.SetJob(ctx, root, fqdn, "data", SetJobInput{Job: *keep(1), Accept: []string{"R101"}}); !errors.As(err, &ce) || ce.Blocking[0].Code != "R104" {
		t.Fatalf("a cut must need R104: %v", err)
	}
	if _, err := s.SetJob(ctx, root, fqdn, "data", SetJobInput{Job: *keep(1), Accept: accept}); err != nil {
		t.Fatal(err)
	}
	plan, err := s.Pending(ctx, fqdn)
	if err != nil {
		t.Fatal(err)
	}
	pendingIDs := func(p RetentionPlan) []string {
		var ids []string
		for _, x := range p.Pending {
			ids = append(ids, x.ID)
		}
		slices.Sort(ids)
		return ids
	}
	if len(plan.Forget) != 0 || !slices.Equal(pendingIDs(plan), []string{snapA, snapB}) || plan.Pending[0].KeptBy != 1 {
		t.Fatalf("after the cut, A and B wait 7 days, kept by revision 1: %+v", plan)
	}
	if !slices.Equal(plan.Jobs[0].NotMadeByHoldfast, []string{snapD}) {
		t.Fatalf("D was not made by Holdfast: %+v", plan.Jobs[0])
	}

	// Keep longer brings revision 1's retention back: nothing is pending. Cut again: pending again.
	if res, err := s.KeepLonger(ctx, root, fqdn, "data", KeepLongerInput{Accept: accept}); err != nil || res.Revision != 3 {
		t.Fatalf("keep longer: %+v %v", res, err)
	}
	if plan, _ := s.Pending(ctx, fqdn); len(plan.Pending) != 0 {
		t.Fatalf("after keep longer nothing is pending: %+v", plan.Pending)
	}
	if _, err := s.SetJob(ctx, root, fqdn, "data", SetJobInput{Job: *keep(1), Accept: accept}); err != nil {
		t.Fatal(err)
	}

	// Within the 7 days, the daily run forgets nothing.
	id, err := s.beginRun(ctx, fqdn, store.RunNow, root)
	if err != nil {
		t.Fatal(err)
	}
	if run := s.runMaintenance(id, fqdn, store.RunNow, root, s.pickRetention(fqdn)); run.Status != "success" || len(run.Forgotten) != 0 || run.Pruned {
		t.Fatalf("within the 7 days: %+v", run)
	}

	// 8 days later, A and B go, D stays: it is not Holdfast's. Then the check passes and restic prunes.
	offset = 8 * 24 * time.Hour
	id, _ = s.beginRun(ctx, fqdn, store.RunNow, root)
	run := s.runMaintenance(id, fqdn, store.RunNow, root, s.pickRetention(fqdn))
	if run.Status != "success" || !slices.Equal(run.Forgotten, []string{snapA, snapB}) || !run.Pruned || run.Freed != "1.500 KiB" {
		t.Fatalf("after the 7 days: %+v", run)
	}
	b, _ := os.ReadFile(log)
	if !strings.Contains(string(b), "forget -- "+snapA+" "+snapB) || strings.Contains(string(b), "forget -- "+snapD) {
		t.Fatalf("restic forget must get exactly A and B:\n%s", b)
	}
	if i, j := strings.Index(string(b), "10m0s check\n"), strings.Index(string(b), "10m0s prune\n"); i < 0 || j < i {
		t.Fatalf("the check must come before the prune:\n%s", b)
	}
	counts, _ := q.CountCatalogue(ctx)
	if counts[fqdn].Disappeared != 0 {
		t.Fatal("snapshots Holdfast forgot must not count as disappeared")
	}
	list, _ := s.Snapshots(ctx, fqdn, SnapshotQuery{All: true})
	for _, sn := range list {
		if gone := sn.ID == snapA || sn.ID == snapB; gone != (sn.GoneAt != nil && sn.DeletedByUs) {
			t.Fatalf("tombstone of %s: %+v", sn.ShortID, sn)
		}
	}

	// Prune now and Delete now need the host's short name.
	var ve *ValidationError
	if _, err := s.PruneNow(ctx, root, fqdn, PruneNowInput{Confirm: "web02"}); !errors.As(err, &ve) {
		t.Fatalf("Prune now without the right name: %v", err)
	}
	if p, err := s.PruneNowPlan(ctx, fqdn); err != nil || p.Confirm != "web01" || len(p.Commands) != 3 {
		t.Fatalf("Prune now preview: %+v %v", p, err)
	}
	if _, err := s.DeleteSnapshot(ctx, root, fqdn, snapC[:8], DeleteSnapshotInput{Now: true, Message: "test"}); !errors.As(err, &ve) {
		t.Fatalf("Delete now without the name: %v", err)
	}
	// A deletion always says why (decision 055).
	if _, err := s.DeleteSnapshot(ctx, root, fqdn, snapC[:8], DeleteSnapshotInput{Message: "  "}); !errors.As(err, &ve) {
		t.Fatalf("a deletion without a reason: %v", err)
	}

	// A deletion waits 7 days and can be cancelled; Delete now forgets at once and leaves the prune for later.
	del, err := s.DeleteSnapshot(ctx, root, fqdn, snapC[:8], DeleteSnapshotInput{Message: "a password in it"})
	if err != nil || del.Status != "pending" || del.PrunableAt == nil {
		t.Fatalf("delete: %+v %v", del, err)
	}
	if _, err := s.DeleteSnapshot(ctx, root, fqdn, snapC[:8], DeleteSnapshotInput{Message: "again"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("asked twice: %v", err)
	}
	if plan, _ := s.Pending(ctx, fqdn); !slices.Equal(pendingIDs(plan), []string{snapC}) || plan.Pending[0].Reason != PendingDeletion ||
		plan.Pending[0].Message != "a password in it" {
		t.Fatalf("the deletion is pending, with its reason: %+v", plan.Pending)
	}
	if sn, err := s.st.Q().GetSnapshot(ctx, fqdn, snapC); err != nil || sn.DeletionReason != "a password in it" || sn.DeletionRequestedBy != root.Name {
		t.Fatalf("the snapshot keeps who asked and why: %+v %v", sn, err)
	}
	if err := s.CancelDeletion(ctx, root, fqdn, snapC[:8]); err != nil {
		t.Fatal(err)
	}
	if sn, _ := s.st.Q().GetSnapshot(ctx, fqdn, snapC); sn.DeletionReason != "" {
		t.Fatalf("a cancelled deletion leaves no reason: %+v", sn)
	}
	now, err := s.DeleteSnapshot(ctx, root, fqdn, snapC[:8], DeleteSnapshotInput{Now: true, Confirm: "web01", Message: "a password in it, now"})
	if err != nil {
		t.Fatal(err)
	}
	if run := waitRunDone(t, s, fqdn, now.Run); run.Status != "success" || !slices.Equal(run.Forgotten, []string{snapC}) || run.Pruned {
		t.Fatalf("Delete now: %+v", run)
	}

	// A failed check blocks the prune, and the prune stays due.
	if err := os.WriteFile(filepath.Join(dir, "check-exit"), []byte("1"), 0o644); err != nil {
		t.Fatal(err)
	}
	started, err := s.RunRetention(ctx, root, fqdn)
	if err != nil {
		t.Fatal(err)
	}
	if run := waitRunDone(t, s, fqdn, started.Run); run.Status != "failed" || run.Pruned || !strings.Contains(run.Error, "check failed") {
		t.Fatalf("a failed check must block the prune: %+v", run)
	}
	if h, _ := q.GetHost(ctx, fqdn); !h.PruneNeeded {
		t.Fatal("the prune must stay due")
	}
	prunes, _ := s.Prunes(ctx, fqdn, false, 100)
	if len(prunes) != 3 { // the prune after the 7 days, Delete now, and the failed one
		t.Fatalf("the list of prunes: %+v", prunes)
	}
}

// The daily run goes in the middle of the host's longest quiet stretch: for a backup at 02:00, from 02:00 to
// midnight, so at 13:00.
func TestRetentionSlot(t *testing.T) {
	s := newTestServer(t)
	h := config.Host{Jobs: map[string]config.Job{"data": {Schedule: schedule.Schedule{Daily: &schedule.Daily{Times: []schedule.ClockTime{"02:00"}}}}}}
	day := time.Date(2026, 11, 21, 10, 0, 0, 0, s.zone)
	slot, end := s.retentionSlot(day, h)
	if want := time.Date(2026, 11, 21, 13, 0, 0, 0, s.zone); !slot.Equal(want) || !end.Equal(time.Date(2026, 11, 22, 0, 0, 0, 0, s.zone)) {
		t.Fatalf("slot %v, end %v; want %v", slot, end, want)
	}
}
