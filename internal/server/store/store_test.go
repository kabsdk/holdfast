package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/kabsdk/holdfast/internal/config"
	"github.com/kabsdk/holdfast/internal/server/creds"
)

// testStore creates a fresh database for one test on the server named by HOLDFAST_TEST_PG
// (an admin DSN), migrates it, and drops it afterwards. Without the variable the test is skipped.
func testStore(t *testing.T) *Store {
	t.Helper()
	s := emptyTestStore(t)
	ctx := context.Background()
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.Migrate(ctx); err != nil { // idempotent
		t.Fatal(err)
	}
	return s
}

// emptyTestStore creates a fresh database with no table, as a rebuilt server's is.
func emptyTestStore(t *testing.T) *Store {
	t.Helper()
	admin := os.Getenv("HOLDFAST_TEST_PG")
	if admin == "" {
		t.Skip("HOLDFAST_TEST_PG is not set (make pg-up prints one)")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, admin)
	if err != nil {
		t.Fatal(err)
	}
	name := "hf_test_" + strings.ToLower(creds.NewToken()[:12])
	if _, err := conn.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	cfg, _ := pgx.ParseConfig(admin)
	dsn := fmt.Sprintf("postgres://%s:%s@%s:%d/%s?sslmode=disable", cfg.User, cfg.Password, cfg.Host, cfg.Port, name)
	s, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		s.Close()
		_, _ = conn.Exec(ctx, "DROP DATABASE "+name+" WITH (FORCE)")
		_ = conn.Close(ctx)
	})
	return s
}

func docWith(hosts ...string) config.Document {
	d := config.New()
	for _, h := range hosts {
		d.Hosts[h] = config.Host{Jobs: map[string]config.Job{}}
	}
	return d
}

func TestRevisionsAreLinearAndImmutable(t *testing.T) {
	s, ctx := testStore(t), context.Background()
	q := s.Q()
	if _, err := q.Head(ctx); !errors.Is(err, ErrNotFound) {
		t.Fatalf("empty store head: %v", err)
	}
	r1, err := q.SaveRevision(ctx, SaveRevision{Base: 0, Actor: "local:root", Source: "cli", Message: "first", Config: docWith("web01.example.com"),
		HostSHAs: map[string]string{"web01.example.com": "aaa"}})
	if err != nil || r1.Number != 1 {
		t.Fatalf("first revision: %v %v", r1, err)
	}
	// A second save from the same base loses: optimistic concurrency.
	if _, err := q.SaveRevision(ctx, SaveRevision{Base: 0, Actor: "local:alice", Source: "cli", Message: "stale", Config: docWith()}); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale save: %v", err)
	}
	// Revision 2 adds db01 and leaves web01 unchanged: only db01 gets a host_desired row.
	if _, err := q.SaveRevision(ctx, SaveRevision{Base: 1, Actor: "local:root", Source: "cli", Message: "add db01", Config: docWith("web01.example.com", "db01.example.com"),
		HostSHAs: map[string]string{"web01.example.com": "aaa", "db01.example.com": "bbb"}}); err != nil {
		t.Fatal(err)
	}
	if rev, sha, _ := q.HostDesired(ctx, "web01.example.com"); rev != 1 || sha != "aaa" {
		t.Fatalf("web01 desired = %d %s, want 1 aaa (unchanged in revision 2)", rev, sha)
	}
	// Revision 3 removes web01.
	if _, err := q.SaveRevision(ctx, SaveRevision{Base: 2, Actor: "local:root", Source: "cli", Message: "remove web01", Config: docWith("db01.example.com"),
		HostSHAs: map[string]string{"db01.example.com": "bbb"}}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := q.HostDesired(ctx, "web01.example.com"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("removed host should have no desired config, got %v", err)
	}
	head, err := q.Head(ctx)
	if err != nil || head.Number != 3 || len(head.Config.Hosts) != 1 {
		t.Fatalf("head: %+v %v", head, err)
	}
	// The stored bytes hash to the stored sha256.
	r2, _ := q.GetRevision(ctx, 2)
	sum, _ := r2.Config.SHA256()
	if fmt.Sprintf("%x", sum) != r2.SHA256 {
		t.Fatal("stored config does not hash to its stored sha256")
	}
	if _, err := s.pool.Exec(ctx, `UPDATE revisions SET message = 'tampered' WHERE number = 1`); err == nil {
		t.Fatal("revisions accepted an UPDATE")
	}
	if _, err := s.pool.Exec(ctx, `DELETE FROM revisions WHERE number = 1`); err == nil {
		t.Fatal("revisions accepted a DELETE")
	}
	list, _ := q.ListRevisions(ctx, 10)
	if len(list) != 3 || list[0].Number != 3 {
		t.Fatalf("list: %+v", list)
	}
}

func TestEnrollment(t *testing.T) {
	s, ctx := testStore(t), context.Background()
	q := s.Q()
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	r, _ := q.SaveRevision(ctx, SaveRevision{Base: 0, Actor: "local:root", Source: "cli", Message: "add", Config: docWith("web01.example.com")})
	if err := q.CreateHost(ctx, "web01.example.com", r.Number, now); err != nil {
		t.Fatal(err)
	}
	if err := q.CreateHost(ctx, "web01.example.com", r.Number, now); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate host: %v", err)
	}
	code1, code2 := creds.HashToken("C1"), creds.HashToken("C2")
	must(t, q.CreateEnrollmentCode(ctx, "00000000-0000-0000-0000-000000000001", "web01.example.com", code1, now, now.Add(15*time.Minute), "local:root"))
	must(t, q.CreateEnrollmentCode(ctx, "00000000-0000-0000-0000-000000000002", "web01.example.com", code2, now, now.Add(15*time.Minute), "local:root"))
	if err := q.RedeemEnrollmentCode(ctx, "web01.example.com", code1, now); !errors.Is(err, ErrCodeRevoked) {
		t.Fatalf("revoked code: %v", err)
	}
	if err := q.RedeemEnrollmentCode(ctx, "db01.example.com", code2, now); !errors.Is(err, ErrCodeInvalid) {
		t.Fatalf("code for another host: %v", err)
	}
	if err := q.RedeemEnrollmentCode(ctx, "web01.example.com", code2, now.Add(15*time.Minute)); !errors.Is(err, ErrCodeExpired) {
		t.Fatalf("expired code: %v", err)
	}
	must(t, q.RedeemEnrollmentCode(ctx, "web01.example.com", code2, now.Add(14*time.Minute)))
	if err := q.RedeemEnrollmentCode(ctx, "web01.example.com", code2, now.Add(14*time.Minute)); !errors.Is(err, ErrCodeUsed) {
		t.Fatalf("reused code: %v", err)
	}
	must(t, q.EnrollAgent(ctx, "web01.example.com", creds.HashToken("cert"), now))
	if f, err := q.AgentByCertificate(ctx, creds.HashToken("cert")); err != nil || f != "web01.example.com" {
		t.Fatalf("agent by certificate: %q %v", f, err)
	}
	if _, err := q.AgentByCertificate(ctx, creds.HashToken("other")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown certificate: %v", err)
	}
	h, _ := q.GetHost(ctx, "web01.example.com")
	if h.State != "enrolled" || h.ConnectedAt == nil {
		t.Fatalf("host after enrollment: %+v", h)
	}
}

func TestEventsAreStoredOnce(t *testing.T) {
	s, ctx := testStore(t), context.Background()
	q := s.Q()
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	r, _ := q.SaveRevision(ctx, SaveRevision{Base: 0, Actor: "local:root", Source: "cli", Message: "add", Config: docWith("web01.example.com")})
	must(t, q.CreateHost(ctx, "web01.example.com", r.Number, now))
	ev := Event{FQDN: "web01.example.com", ID: "e1", Kind: "backup", JobID: "system", Status: "success", Started: now, Finished: now.Add(time.Minute), SnapshotID: "snap1"}
	for i := 0; i < 3; i++ {
		inserted, err := q.InsertEvent(ctx, ev)
		if err != nil || inserted != (i == 0) {
			t.Fatalf("delivery %d: inserted=%v err=%v", i, inserted, err)
		}
	}
	// The latest report wins, whatever the agent's clock says (decision 030). The agent sends reports in
	// the order its runs end. Here its clock had jumped back, and the failure must still show.
	_, _ = q.InsertEvent(ctx, Event{FQDN: "web01.example.com", ID: "e2", Kind: "backup", JobID: "system", Status: "failed", Started: now.Add(-time.Hour), Finished: now.Add(-time.Hour), Error: "boom"})
	st, _ := q.ListJobStatus(ctx)
	if len(st) != 1 || st[0].LastStatus != "failed" || st[0].LastError != "boom" || st[0].LastSnapshotID != "snap1" || !st[0].LastSuccessAt.Equal(now.Add(time.Minute)) {
		t.Fatalf("job status after a report dated earlier: %+v", st)
	}
	_, _ = q.InsertEvent(ctx, Event{FQDN: "web01.example.com", ID: "e3", Kind: "backup", JobID: "system", Status: "success", Started: now.Add(-50 * time.Minute), Finished: now.Add(-50 * time.Minute), SnapshotID: "snap3"})
	if st, _ = q.ListJobStatus(ctx); st[0].LastStatus != "success" || st[0].LastSnapshotID != "snap3" {
		t.Fatalf("the next success, also dated earlier, must show: %+v", st)
	}
	counters, _ := q.ListRunCounters(ctx)
	got := map[string]int64{}
	for _, c := range counters {
		got[c.Status] = c.Count
	}
	if got["success"] != 2 || got["failed"] != 1 {
		t.Fatalf("counters: %+v", counters)
	}
	h, _ := q.GetHost(ctx, "web01.example.com")
	if h.FirstBackupAt == nil {
		t.Fatal("first backup not recorded")
	}
}

// TestReportedInBucket is the storage witness's query: a reported snapshot is judged only by a read
// of the repository that started after the report arrived; one that was there and went later counts as held (it is
// disappeared instead).
func TestReportedInBucket(t *testing.T) {
	s, ctx := testStore(t), context.Background()
	q := s.Q()
	const fqdn = "web01.example.com"
	now := time.Now()
	r, _ := q.SaveRevision(ctx, SaveRevision{Base: 0, Actor: "local:root", Source: "cli", Message: "add", Config: docWith(fqdn)})
	must(t, q.CreateHost(ctx, fqdn, r.Number, now))
	snap := func(id string) Snapshot {
		return Snapshot{FQDN: fqdn, ID: id, ShortID: id[:4], Time: now.Add(-time.Hour), JobID: "home"}
	}
	_, _, err := q.SyncSnapshots(ctx, fqdn, []Snapshot{snap("aaaa1")}, now.Add(-time.Minute))
	must(t, err)
	_, err = q.InsertEvent(ctx, Event{FQDN: fqdn, ID: "e1", Kind: "backup", JobID: "home", Status: "success", Started: now, Finished: now, SnapshotID: "bbbb2"})
	must(t, err)
	got := func() []ReportedSnapshot {
		t.Helper()
		out, err := q.ReportedInBucket(ctx)
		must(t, err)
		return out
	}
	// The last read started before the report: nothing is judged yet.
	if out := got(); len(out) != 0 {
		t.Fatalf("judged by a read from before the report: %+v", out)
	}
	// A read after the report, without the snapshot: not in the bucket.
	_, _, err = q.SyncSnapshots(ctx, fqdn, []Snapshot{snap("aaaa1")}, now.Add(time.Minute))
	must(t, err)
	if out := got(); len(out) != 1 || out[0].JobID != "home" || out[0].SnapshotID != "bbbb2" || out[0].InBucket {
		t.Fatalf("a reported snapshot the read did not find: %+v", out)
	}
	// Found by the next read, and still held after it goes: that is a disappearance, not this.
	_, _, err = q.SyncSnapshots(ctx, fqdn, []Snapshot{snap("aaaa1"), snap("bbbb2")}, now.Add(2*time.Minute))
	must(t, err)
	if out := got(); len(out) != 1 || !out[0].InBucket {
		t.Fatalf("a reported snapshot the read found: %+v", out)
	}
	_, gone, err := q.SyncSnapshots(ctx, fqdn, []Snapshot{snap("aaaa1")}, now.Add(3*time.Minute))
	must(t, err)
	if out := got(); gone != 1 || len(out) != 1 || !out[0].InBucket {
		t.Fatalf("a reported snapshot that went later: gone %d, %+v", gone, out)
	}
}

// TestRepositoryKeys (decision 032): the keys of a repository Holdfast made are known; a later one is
// foreign until acknowledged; a repository made again after the old one was lost starts over, so its new
// key is not flagged.
func TestRepositoryKeys(t *testing.T) {
	s, ctx := testStore(t), context.Background()
	q := s.Q()
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	r, _ := q.SaveRevision(ctx, SaveRevision{Base: 0, Actor: "local:root", Source: "cli", Message: "add", Config: docWith("web01.example.com")})
	must(t, q.CreateHost(ctx, "web01.example.com", r.Number, now))
	if again, err := q.ResetRepositoryKeys(ctx, "web01.example.com", []string{"k1"}); err != nil || again {
		t.Fatalf("first init: again=%v %v", again, err)
	}
	if foreign, fresh, _ := q.RecordRepositoryKeys(ctx, "web01.example.com", []string{"k1", "k2"}); len(foreign) != 1 || len(fresh) != 1 {
		t.Fatalf("a key added outside: foreign %v, fresh %v", foreign, fresh)
	}
	if _, fresh, _ := q.RecordRepositoryKeys(ctx, "web01.example.com", []string{"k1", "k2"}); len(fresh) != 0 {
		t.Fatalf("the same foreign key is reported as new twice: %v", fresh)
	}
	if again, err := q.ResetRepositoryKeys(ctx, "web01.example.com", []string{"k9"}); err != nil || !again {
		t.Fatalf("init again: again=%v %v", again, err)
	}
	if foreign, _, _ := q.RecordRepositoryKeys(ctx, "web01.example.com", []string{"k9"}); len(foreign) != 0 {
		t.Fatalf("the key of a repository Holdfast made again is flagged: %v", foreign)
	}
}

func TestAuditIsAppendOnly(t *testing.T) {
	s, ctx := testStore(t), context.Background()
	q := s.Q()
	must(t, q.Audit(ctx, AuditEntry{Actor: "local:root", Source: "cli", Action: "host.add", FQDN: "web01.example.com", Detail: map[string]any{"revision": 1}}))
	if _, err := s.pool.Exec(ctx, `DELETE FROM audit_log`); err == nil {
		t.Fatal("audit_log accepted a DELETE")
	}
	list, _ := q.ListAudit(ctx, "web01.example.com", 10)
	if len(list) != 1 || list[0].Action != "host.add" {
		t.Fatalf("audit list: %+v", list)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// A job's size is one measurement: a job restic could not measure keeps its last, and a job without snapshots, or one
// the host no longer has, has none.
func TestJobSizes(t *testing.T) {
	s, ctx := testStore(t), context.Background()
	q := s.Q()
	const fqdn = "web01.example.com"
	sizes := func() map[string]JobSize {
		t.Helper()
		all, err := q.ListJobSizes(ctx)
		must(t, err)
		out := map[string]JobSize{}
		for _, z := range all {
			out[z.FQDN+"/"+z.JobID] = z
		}
		return out
	}
	t1 := time.Date(2026, 11, 26, 3, 0, 0, 0, time.UTC)
	must(t, q.RecordJobSizes(ctx, fqdn, map[string]int64{"home": 100, "etc": 200}, nil, t1))
	must(t, q.RecordJobSizes(ctx, "db01.example.com", map[string]int64{"home": 7}, nil, t1))
	t2 := t1.Add(24 * time.Hour)
	must(t, q.RecordJobSizes(ctx, fqdn, map[string]int64{"home": 150}, []string{"etc"}, t2))
	got := sizes()
	if h, e := got[fqdn+"/home"], got[fqdn+"/etc"]; h.Bytes != 150 || !h.MeasuredAt.Equal(t2) || e.Bytes != 200 || !e.MeasuredAt.Equal(t1) {
		t.Fatalf("a measured job takes its new size, one that failed keeps its last: %+v", got)
	}
	must(t, q.RecordJobSizes(ctx, fqdn, map[string]int64{"home": 160}, nil, t2.Add(24*time.Hour)))
	got = sizes()
	if _, ok := got[fqdn+"/etc"]; ok || got[fqdn+"/home"].Bytes != 160 || got["db01.example.com/home"].Bytes != 7 {
		t.Fatalf("a job without a size this time has none, and other hosts' stay: %+v", got)
	}
}
