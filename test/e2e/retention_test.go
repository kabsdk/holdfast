package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kabsdk/holdfast/internal/proto"
	"github.com/kabsdk/holdfast/internal/restic"
)

// history makes snapshots of src in a host's repository with restic --time, tagged as the job's, as if the
// agent had made them on those days. It returns their IDs in the order of times.
func history(t *testing.T, r restic.Runner, fqdn, jobID, src string, times []time.Time) []string {
	t.Helper()
	var ids []string
	for i := range times {
		res, err := r.Backup(context.Background(), restic.BackupOptions{Paths: []string{src}, Host: fqdn, Tags: []string{"job=" + jobID}, Time: &times[i]})
		if err != nil {
			t.Fatalf("restic backup --time %s: %v", times[i], err)
		}
		ids = append(ids, res.SnapshotID)
	}
	return ids
}

// reportSnapshots tells the server, as the host's agent, that it made these snapshots: the events the agent
// sends after each backup. The test made them with restic --time, which the agent never does.
func (e *env) reportSnapshots(a *agentProc, jobID string, ids []string) {
	e.t.Helper()
	now := time.Now()
	var evs []proto.Event
	for _, id := range ids {
		evs = append(evs, proto.Event{ID: "e2e-history-" + id[:16], Kind: proto.KindBackup, JobID: jobID, Status: "success", Started: now, Finished: now, SnapshotID: id})
	}
	body, _ := json.Marshal(proto.EventsRequest{Events: evs})
	resp, err := e.agentClient(a).Post(e.url+proto.Base+"/events", "application/json", bytes.NewReader(body))
	if err != nil || resp.StatusCode != 200 {
		e.t.Fatalf("reporting snapshots: %v %v", resp, err)
	}
	resp.Body.Close()
}

func ids(snaps []restic.Snapshot) []string {
	out := restic.IDs(snaps)
	slices.Sort(out)
	return out
}

type runResult struct {
	ID        int64    `json:"id"`
	Status    string   `json:"status"`
	Forgotten []string `json:"forgotten"`
	Pruned    bool     `json:"pruned"`
	Freed     string   `json:"freed"`
	Error     string   `json:"error"`
}

func (e *env) run(args ...string) runResult {
	e.t.Helper()
	var r runResult
	must(e.t, json.Unmarshal(e.cli(append(args, "--wait")...), &r))
	slices.Sort(r.Forgotten)
	return r
}

type pendingView struct {
	Forget  []string `json:"forget"`
	Pending []struct {
		ID         string    `json:"id"`
		Reason     string    `json:"reason"`
		KeptBy     int64     `json:"kept_by_revision"`
		PrunableAt time.Time `json:"prunable_at"`
	} `json:"pending"`
}

func (e *env) pending(fqdn string) pendingView {
	e.t.Helper()
	var p pendingView
	must(e.t, json.Unmarshal(e.cli("pending", fqdn), &p))
	slices.Sort(p.Forget)
	return p
}

func (p pendingView) ids() []string {
	var out []string
	for _, x := range p.Pending {
		out = append(out, x.ID)
	}
	slices.Sort(out)
	return out
}

// Ported from the PoC's TestRetentionPolicyMatchesModel: 180 days of snapshots made with restic --time, then
// Holdfast's daily run. restic decides (decision 030): the survivors are exactly what restic's own dry run
// keeps, plus a snapshot Holdfast did not make, which it leaves alone (decision 032). After the prune, a full
// check --read-data passes, and the catalogue has tombstones, not disappearances.
func TestRetentionMatchesRestic(t *testing.T) {
	src := smallSource(t)
	e := newEnv(t)
	const fqdn = "ret01.example.com"
	code := e.addHost(fqdn, "data", "--path", src, "--daily", "02:00", "--keep-daily", "14d", "--keep-weekly", "8w", "--keep-monthly", "6mo", "--keep-last", "3")
	a := e.startAgent(fqdn, code)
	e.waitProgress(fqdn, "first_backup_done", 2*time.Minute)
	r := e.resticFor(e.escrow()[fqdn])

	base := time.Now().UTC().Truncate(24 * time.Hour).Add(-22 * time.Hour) // yesterday, 02:00
	var times []time.Time
	for d := 179; d >= 0; d-- {
		day := base.AddDate(0, 0, -d)
		times = append(times, day)
		if d%3 == 0 { // two snapshots on some days: only the newest of a day can be the daily one
			times = append(times, day.Add(10*time.Hour))
		}
	}
	made := history(t, r, fqdn, "data", src, times)
	e.reportSnapshots(a, "data", made)
	manual := history(t, r, fqdn, "data", src, []time.Time{base.AddDate(0, 0, -100).Add(-time.Hour)})[0] // someone ran restic by hand

	// restic's own answer, with the flags spec §4.2 maps the job's windows to.
	ctx := context.Background()
	keep, remove, err := r.ForgetPlan(ctx, "job=data", []string{"--keep-within-daily", "14d", "--keep-within-weekly", "56d", "--keep-within-monthly", "6m", "--keep-last", "3"})
	must(t, err)
	if !slices.Contains(ids(remove), manual) {
		t.Fatalf("the hand-made snapshot should be one the policy drops; pick another time for it")
	}
	want := append(ids(keep), manual)
	slices.Sort(want)
	wantForgotten := slices.DeleteFunc(ids(remove), func(id string) bool { return id == manual })

	// One policy has been in force, so nothing waits: the daily run forgets what restic drops, except the
	// hand-made snapshot.
	if p := e.pending(fqdn); len(p.Pending) != 0 || !slices.Equal(p.Forget, wantForgotten) {
		t.Fatalf("pending: %d pending, %d to forget, want 0 and %d", len(p.Pending), len(p.Forget), len(wantForgotten))
	}
	run := e.run("retention", "run", fqdn)
	if run.Status != "success" || !slices.Equal(run.Forgotten, wantForgotten) || !run.Pruned || run.Freed == "" {
		t.Fatalf("the daily run: %+v", run)
	}
	after, err := r.Snapshots(ctx, "job=data")
	must(t, err)
	if got := ids(after); !slices.Equal(got, want) {
		t.Fatalf("survivors differ from restic's own dry run: got %d, want %d", len(got), len(want))
	}
	must(t, r.Check(ctx, "1/1")) // a full read-data check after the prune

	// The catalogue: tombstones by Holdfast, nothing disappeared, and when the data is really gone.
	var rows []struct {
		ID          string     `json:"id"`
		GoneAt      *time.Time `json:"gone_at"`
		DeletedByUs bool       `json:"deleted_by_holdfast"`
		GoneBy      *time.Time `json:"really_gone_by"`
	}
	must(t, json.Unmarshal(e.cli("snapshot", "list", fqdn, "--all", "--refresh"), &rows))
	deleted := 0
	for _, s := range rows {
		if s.GoneAt != nil {
			if !s.DeletedByUs || s.GoneBy == nil || s.GoneBy.Sub(*s.GoneAt) < 29*24*time.Hour {
				t.Fatalf("a snapshot Holdfast deleted needs its tombstone and its really-gone date (30 days of S3 versions): %+v", s)
			}
			deleted++
		}
	}
	if deleted != len(wantForgotten) {
		t.Fatalf("%d tombstones, want %d", deleted, len(wantForgotten))
	}
	if h := e.hostRaw(fqdn); h["disappeared"].(float64) != 0 {
		t.Fatalf("snapshots Holdfast forgot must not count as disappeared: %v", h["disappeared"])
	}
	var prunes []struct {
		Kind   string     `json:"kind"`
		Pruned bool       `json:"pruned"`
		GoneBy *time.Time `json:"really_gone_by"`
	}
	must(t, json.Unmarshal(e.cli("prunes", fqdn), &prunes))
	if len(prunes) != 1 || prunes[0].Kind != "run_now" || !prunes[0].Pruned || prunes[0].GoneBy == nil {
		t.Fatalf("the list of prunes: %+v", prunes)
	}
	if m := metrics(t, e.url+"/metrics"); m[`holdfast_retention_last_run_failed{host="ret01.example.com"}`] != 0 ||
		m[`holdfast_retention_last_success_timestamp_seconds{host="ret01.example.com"}`] == 0 {
		t.Fatal("the retention metrics must show the run that went well")
	}
	t.Logf("%d snapshots over 180 days: restic keeps %d, Holdfast forgot %d and left the hand-made one; %s freed; check --read-data passed",
		len(made), len(keep), len(run.Forgotten), run.Freed)
}

// The 7-day protection, with the server's clock moved 8 days:
//   - a cut needs R104, and what it drops waits 7 days, kept by the older revision;
//   - Keep longer undoes it;
//   - 8 days later the daily run forgets exactly what waited;
//   - Delete snapshot waits too, can be cancelled, and Delete now needs the host's short name;
//   - Prune now forgets what the current policy drops, at once, after the preview and the short name;
//   - a snapshot Holdfast did not make is never forgotten;
//   - a server without -test refuses the test clock.
func TestRetentionCutWaitsSevenDays(t *testing.T) {
	src := smallSource(t)
	e := newEnv(t)
	const fqdn = "cut01.example.com"
	long := []string{"--path", src, "--daily", "02:00", "--keep-daily", "30d", "--keep-monthly", "12mo"}
	short := []string{"--path", src, "--daily", "02:00", "--keep-daily", "7d", "--keep-monthly", "12mo"}
	code := e.addHost(fqdn, "data", long...)
	a := e.startAgent(fqdn, code)
	e.waitProgress(fqdn, "first_backup_done", 2*time.Minute)
	r := e.resticFor(e.escrow()[fqdn])
	base := time.Now().UTC().Truncate(24 * time.Hour).Add(-22 * time.Hour)
	var times []time.Time
	for d := 20; d >= 1; d-- {
		times = append(times, base.AddDate(0, 0, -d))
	}
	e.reportSnapshots(a, "data", history(t, r, fqdn, "data", src, times))
	manual := history(t, r, fqdn, "data", src, []time.Time{base.AddDate(0, 0, -15).Add(-time.Hour)})[0]
	e.cli("snapshot", "list", fqdn, "--refresh")

	// The cut needs R104, from restic's dry runs.
	set := func(args []string, accept ...string) ([]byte, error) {
		return e.cliErr(append(append([]string{"job", "set", fqdn, "data", testAccept}, accept...), args...)...)
	}
	if out, err := set(short); err == nil || !strings.Contains(string(out), "R104") {
		t.Fatalf("a cut must need R104: %v %s", err, out)
	}
	if out, err := set(short, "--accept=R104"); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	p := e.pending(fqdn)
	cut := p.ids()
	if len(cut) == 0 || len(p.Forget) != 0 {
		t.Fatalf("the cut must leave snapshots waiting and none to forget: %+v", p)
	}
	for _, x := range p.Pending {
		// Revision 1 added the host, revision 2 its job.
		if x.Reason != "retention_cut" || x.KeptBy != 2 || time.Until(x.PrunableAt) < 7*24*time.Hour-time.Hour || time.Until(x.PrunableAt) > 7*24*time.Hour+time.Hour {
			t.Fatalf("a pending snapshot is kept by revision 2 for 7 days: %+v", x)
		}
	}
	if run := e.run("retention", "run", fqdn); run.Status != "success" || len(run.Forgotten) != 0 {
		t.Fatalf("within the 7 days the daily run forgets nothing: %+v", run)
	}

	// Keep longer brings the old retention back; the cut again leaves the same snapshots waiting.
	e.cli("job", "keep-longer", fqdn, "data", testAccept)
	if p := e.pending(fqdn); len(p.Pending) != 0 {
		t.Fatalf("after keep longer nothing waits: %+v", p)
	}
	if out, err := set(short, "--accept=R104"); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	if p := e.pending(fqdn); !slices.Equal(p.ids(), cut) || p.Pending[0].KeptBy != 4 {
		t.Fatalf("after the second cut, revision 4 keeps the same snapshots: %+v", p)
	}

	// The test clock needs -test.
	out, err := exec.Command("env", "HOLDFAST_TEST_NOW_OFFSET=192h", filepath.Join(binDir, "holdfast-server"), "-config", e.cfgPath).CombinedOutput()
	if err == nil || !strings.Contains(string(out), "-test") {
		t.Fatalf("a server without -test must refuse the test clock: %v %s", err, out)
	}

	// 8 days later: exactly what waited is forgotten, and pruned.
	e.restartServer([]string{"HOLDFAST_TEST_NOW_OFFSET=192h"}, "-test")
	if p := e.pending(fqdn); len(p.Pending) != 0 || !slices.Equal(p.Forget, cut) {
		t.Fatalf("after 8 days nothing waits and the cut is due: %+v", p)
	}
	if run := e.run("retention", "run", fqdn); run.Status != "success" || !slices.Equal(run.Forgotten, cut) || !run.Pruned {
		t.Fatalf("after 8 days: %+v", run)
	}

	// Delete snapshot: it waits, it can be cancelled, and Delete now needs the short name.
	survivors, err := r.Snapshots(context.Background(), "job=data")
	must(t, err)
	slices.SortFunc(survivors, func(x, y restic.Snapshot) int { return x.Time.Compare(y.Time) })
	victim := survivors[len(survivors)-2].ID // not the newest, not the hand-made one (it is older than every daily survivor)
	if victim == manual {
		victim = survivors[len(survivors)-3].ID
	}
	var del struct {
		Status     string    `json:"status"`
		PrunableAt time.Time `json:"prunable_at"`
	}
	must(t, json.Unmarshal(e.cli("snapshot", "delete", fqdn, victim[:12], "-m", "a password in it"), &del))
	if del.Status != "pending" || del.PrunableAt.Sub(time.Now()) < 14*24*time.Hour { // the server's clock is 8 days ahead
		t.Fatalf("delete: %+v", del)
	}
	if p := e.pending(fqdn); !slices.Equal(p.ids(), []string{victim}) || p.Pending[0].Reason != "deletion" {
		t.Fatalf("the deletion waits: %+v", p)
	}
	e.cli("snapshot", "delete", fqdn, victim[:12], "--cancel")
	if p := e.pending(fqdn); len(p.Pending) != 0 {
		t.Fatalf("cancelled: %+v", p)
	}
	if out, err := e.cliErr("snapshot", "delete", fqdn, victim[:12], "--now", "--confirm", "cut02", "--reason", "e2e"); err == nil || !strings.Contains(string(out), "cut01") {
		t.Fatalf("Delete now with the wrong name: %v %s", err, out)
	}
	if run := e.run("snapshot", "delete", fqdn, victim[:12], "--now", "--confirm", "cut01", "--reason", "e2e"); run.Status != "success" || !slices.Equal(run.Forgotten, []string{victim}) || run.Pruned {
		t.Fatalf("Delete now forgets at once and leaves the prune for later: %+v", run)
	}

	// Prune now: the preview, the wrong name, then the current policy's drops at once.
	if out, err := set([]string{"--path", src, "--daily", "02:00", "--keep-daily", "2d", "--keep-monthly", "12mo"}, "--accept=R104"); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	var pre struct {
		Confirm   string `json:"confirm"`
		Count     int    `json:"count"`
		Snapshots []struct {
			ID string `json:"id"`
		} `json:"snapshots"`
		Commands []string `json:"commands"`
	}
	must(t, json.Unmarshal(e.cli("prune-now", fqdn), &pre))
	var preIDs []string
	for _, s := range pre.Snapshots {
		preIDs = append(preIDs, s.ID)
	}
	slices.Sort(preIDs)
	if pre.Confirm != "cut01" || pre.Count == 0 || len(pre.Commands) != 3 || !strings.Contains(pre.Commands[0], "forget -- ") || slices.Contains(preIDs, manual) {
		t.Fatalf("the Prune now preview: %+v", pre)
	}
	if out, err := e.cliErr("prune-now", fqdn, "--confirm", "cut"); err == nil {
		t.Fatalf("Prune now with the wrong name: %s", out)
	}
	if run := e.run("prune-now", fqdn, "--confirm", "cut01", "-m", "disk full"); run.Status != "success" || !slices.Equal(run.Forgotten, preIDs) || !run.Pruned {
		t.Fatalf("Prune now: %+v, preview %v", run, preIDs)
	}

	// The hand-made snapshot is still there, and every step is in the audit log.
	left, err := r.Snapshots(context.Background(), "job=data")
	must(t, err)
	if !slices.Contains(ids(left), manual) {
		t.Fatal("Holdfast forgot a snapshot it did not make")
	}
	var audit []auditRow
	must(t, json.Unmarshal(e.cli("audit", "--host", fqdn), &audit))
	for _, action := range []string{"retention.run_requested", "retention.forgot", "repository.pruned", "snapshot.delete_requested", "snapshot.delete_cancelled",
		"snapshot.delete_now", "snapshot.deleted", "retention.prune_now", "retention.prune_now_forgot"} {
		if !slices.ContainsFunc(audit, func(a auditRow) bool { return a.Action == action }) {
			t.Errorf("no %s in the audit log", action)
		}
	}
}
