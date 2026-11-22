package server

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kabsdk/holdfast/internal/checks"
	"github.com/kabsdk/holdfast/internal/config"
	"github.com/kabsdk/holdfast/internal/gateway"
	"github.com/kabsdk/holdfast/internal/proto"
	"github.com/kabsdk/holdfast/internal/server/storage"
	"github.com/kabsdk/holdfast/internal/server/store"
)

func dumpJob(name string) config.Job {
	j := *job()
	j.Paths, j.Dump = nil, name
	return j
}

func codes(fs []checks.Finding) []string { return checks.Codes(fs) }

// enrolled adds a host, enrolls it, and polls once with the given facts.
func enrolled(t *testing.T, s *Server, fqdn string, f proto.Facts) {
	t.Helper()
	ctx := context.Background()
	res, err := s.AddHost(ctx, root, AddHostInput{FQDN: fqdn})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Enroll(ctx, proto.EnrollRequest{FQDN: fqdn, Code: res.Enrollment.Code, Facts: f}, certOf(fqdn)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Poll(ctx, fqdn, proto.PollRequest{Facts: f}, 0); err != nil {
		t.Fatal(err)
	}
}

// Decision 037: a dump job names a dump the host defines; an agent that does not know dumps cannot get one.
func TestDumpJobChecks(t *testing.T) {
	s, ctx := newTestServer(t), context.Background()
	j := dumpJob("postgresql")

	// Before any agent: noted, not blocking.
	res, err := s.AddHost(ctx, root, AddHostInput{FQDN: "new01.example.com", JobID: "pg", Job: &j})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(codes(res.Findings), "I002") {
		t.Fatalf("before any agent: %v", codes(res.Findings))
	}

	// An agent before M6.
	enrolled(t, s, "old01.example.com", proto.Facts{Protocol: 1})
	var ce *ChecksError
	if _, err := s.SetJob(ctx, root, "old01.example.com", "pg", SetJobInput{Job: j}); !errors.As(err, &ce) || !slices.Contains(codes(ce.Blocking), "D002") {
		t.Fatalf("a protocol 1 agent: %v", err)
	}

	// An agent with dumps: a name it lacks is D001; one it has is fine, and reaches the agent.
	enrolled(t, s, "db01.example.com", proto.Facts{Protocol: 2, Dumps: []string{"postgresql"}})
	if _, err := s.SetJob(ctx, root, "db01.example.com", "my", SetJobInput{Job: dumpJob("mysql")}); !errors.As(err, &ce) || !slices.Contains(codes(ce.Blocking), "D001") {
		t.Fatalf("an unknown dump: %v", err)
	}
	if _, err := s.SetJob(ctx, root, "db01.example.com", "pg", SetJobInput{Job: j}); err != nil {
		t.Fatal(err)
	}
	p, err := s.Poll(ctx, "db01.example.com", proto.PollRequest{Facts: proto.Facts{Protocol: 2, Dumps: []string{"postgresql"}}}, 0)
	if err != nil || p.Config == nil || len(p.Config.Jobs) != 1 || p.Config.Jobs[0].Dump != "postgresql" || len(p.Config.Jobs[0].Paths) != 0 {
		t.Fatalf("the agent's job: %+v %v", p.Config, err)
	}

	// Paths and a dump together are refused.
	both := dumpJob("postgresql")
	both.Paths = []string{"/etc"}
	var ve *ValidationError
	if _, err := s.SetJob(ctx, root, "db01.example.com", "both", SetJobInput{Job: both}); !errors.As(err, &ve) {
		t.Fatalf("paths and a dump: %v", err)
	}
}

// A dump goes into a new directory only; a restore onto another host needs the other host's opt-in and a new enough
// agent, its password reaches only that agent, once (plan-m6 §2.2), and that host may read the repository through the
// gateway until it reports the restore, and only read it (decision 041).
func TestRestoreDumpAndOntoAnotherHost(t *testing.T) {
	s, ctx := newTestServer(t), context.Background()
	q := s.st.Q()
	const a, b = "db01.example.com", "spare01.example.com"
	bounds := &proto.RestoreBounds{Root: "/var/restore", AllowOtherDirectories: true}
	enrolled(t, s, a, proto.Facts{Protocol: 2, RestoreBounds: &proto.RestoreBounds{Root: "/var/restore", AllowInPlace: true}})
	enrolled(t, s, b, proto.Facts{Protocol: 2, RestoreBounds: bounds})
	const dumpID, fileID = "aaaa000000000000000000000000000000000000000000000000000000000001", "aaaa000000000000000000000000000000000000000000000000000000000002"
	if _, _, err := q.SyncSnapshots(ctx, a, []store.Snapshot{
		{ID: dumpID, ShortID: dumpID[:8], Time: time.Now().Add(-time.Hour), Hostname: a, JobID: "pg", Tags: []string{"job=pg", "dump=postgresql"}, Paths: []string{"/postgresql.sql"}},
		{ID: fileID, ShortID: fileID[:8], Time: time.Now().Add(-time.Hour), Hostname: a, JobID: "etc", Tags: []string{"job=etc"}, Paths: []string{"/etc"}},
	}, time.Now()); err != nil {
		t.Fatal(err)
	}
	var ve *ValidationError
	if _, err := s.Restore(ctx, root, a, dumpID, RestoreInput{Target: proto.TargetInPlace}); !errors.As(err, &ve) {
		t.Fatalf("a dump in place: %v", err)
	}
	if _, err := s.Restore(ctx, root, a, dumpID, RestoreInput{}); err != nil {
		t.Fatalf("a dump into a new directory: %v", err)
	}

	for name, in := range map[string]RestoreInput{
		"in place":  {ToHost: b, Target: proto.TargetInPlace},
		"no opt-in": {ToHost: b},
	} {
		if _, err := s.Restore(ctx, root, a, fileID, in); !errors.As(err, &ve) {
			t.Errorf("%s: %v", name, err)
		}
	}
	editor := Actor{Name: "ed", Source: "cli"}
	if _, err := s.Restore(ctx, editor, a, fileID, RestoreInput{ToHost: b}); err == nil {
		t.Fatal("an editor restored onto another host")
	}
	if s.gatewayAccess(ctx, b, a) != gateway.NoAccess {
		t.Fatal("B may use A's repository before any restore")
	}

	// B opts in: the command is B's, sealed, B gets A's password once, and may read A's repository meanwhile.
	bounds.AllowOtherHosts = true
	if _, err := s.Poll(ctx, b, proto.PollRequest{Facts: proto.Facts{Protocol: 2, RestoreBounds: bounds}}, 0); err != nil {
		t.Fatal(err)
	}
	res, err := s.Restore(ctx, root, a, fileID, RestoreInput{ToHost: b, Message: "rebuild"})
	if err != nil {
		t.Fatal(err)
	}
	c, _ := q.GetCredentials(ctx, a)
	pw, _ := s.seal.Open(c.RepoPassword, a)
	queued, _ := q.PendingRestores(ctx)
	if !slices.ContainsFunc(queued, func(c store.Command) bool {
		return c.ID == res.Command && c.FQDN == b && len(c.Sealed) > 0 && !strings.Contains(string(c.Payload), pw)
	}) {
		t.Fatalf("the queued command: %+v", queued)
	}
	if s.gatewayAccess(ctx, b, a) != gateway.ReadOnly || s.gatewayAccess(ctx, a, b) != gateway.NoAccess {
		t.Fatal("during the restore, B may read A's repository, and A nothing of B's")
	}
	p, err := s.Poll(ctx, b, proto.PollRequest{Facts: proto.Facts{Protocol: 2, RestoreBounds: bounds}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	var got *proto.RestoreSource
	for _, cmd := range p.Commands {
		if cmd.ID == res.Command && cmd.Restore != nil {
			got = cmd.Restore.Source
		}
	}
	if got == nil || got.FQDN != a || got.RepoPassword != pw || got.S3AccessKeyID != "" || !strings.Contains(got.Repository, "/gateway/v1/"+a+"/") {
		t.Fatalf("B's restore source: %+v", got)
	}
	if _, err := s.Poll(ctx, b, proto.PollRequest{AckedCommands: []string{res.Command}, Facts: proto.Facts{Protocol: 2, RestoreBounds: bounds}}, 0); err != nil {
		t.Fatal(err)
	}
	var sealed []byte
	if err := s.st.Pool().QueryRow(ctx, `SELECT sealed FROM commands WHERE id = $1`, res.Command).Scan(&sealed); err != nil || sealed != nil {
		t.Fatalf("the secrets must be gone once B has them: %v %d bytes", err, len(sealed))
	}
	audit, _ := q.ListAudit(ctx, a, 20)
	if !slices.ContainsFunc(audit, func(e store.AuditEntry) bool { return e.Action == "restore.requested_elsewhere" }) {
		t.Fatal("A's history must say its snapshot was restored elsewhere")
	}
	// B reports the restore: its grant ends.
	now := time.Now()
	if _, err := s.Events(ctx, b, []proto.Event{{ID: "restored-1", Kind: proto.KindRestore, CommandID: res.Command, Status: proto.StatusSuccess, Started: now, Finished: now}}); err != nil {
		t.Fatal(err)
	}
	if s.gatewayAccess(ctx, b, a) != gateway.NoAccess {
		t.Fatal("B may still read A's repository after the restore")
	}
}

// Decision 038: the preview, its refusals, and a rollback on a host with no agent to pause.
func TestRollback(t *testing.T) {
	b := &storage.MemBucket{}
	s, ctx := newTestServerWith(t, func(d *Deps) { d.Bucket = b }), context.Background()
	q := s.st.Q()
	const fqdn = "web01.example.com"
	if _, err := s.AddHost(ctx, root, AddHostInput{FQDN: fqdn}); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := q.RecordStorageCheck(ctx, fqdn, storage.Report{CheckedAt: now, NoncurrentDays: 30}); err != nil {
		t.Fatal(err)
	}
	p := storage.Prefix(fqdn) + "/"
	b.Put(p+"config", now.Add(-72*time.Hour))
	b.Put(p+"snapshots/aaaa", now.Add(-48*time.Hour))
	b.Delete(p+"snapshots/aaaa", now.Add(-2*time.Hour)) // behind Holdfast's back
	b.Put(p+"snapshots/bbbb", now.Add(-time.Hour))

	pv, err := s.RollbackPreviewFor(ctx, fqdn, now.Add(-24*time.Hour))
	if err != nil || pv.Refused != "" {
		t.Fatalf("preview: %+v %v", pv, err)
	}
	if !slices.Equal(pv.SnapshotsBack, []string{"aaaa"}) || !slices.Equal(pv.SnapshotsSetAside, []string{"bbbb"}) || len(pv.SnapshotDeletions) != 1 {
		t.Fatalf("preview: %+v", pv)
	}
	for name, to := range map[string]time.Time{"in the future": now.Add(time.Hour), "past the lifecycle rule": now.AddDate(0, 0, -31)} {
		if pv, err := s.RollbackPreviewFor(ctx, fqdn, to); err != nil || pv.Refused == "" {
			t.Errorf("%s: %+v %v", name, pv.Refused, err)
		}
	}
	var ve *ValidationError
	if _, err := s.Rollback(ctx, root, fqdn, RollbackInput{To: now.Add(-24 * time.Hour), Confirm: "web01"}); !errors.As(err, &ve) {
		t.Fatalf("without the FQDN typed: %v", err)
	}

	// The host has no agent to pause: it runs at once.
	r, err := s.Rollback(ctx, root, fqdn, RollbackInput{To: now.Add(-24 * time.Hour), Confirm: fqdn, Message: "e2e"})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		list, _ := q.ListRollbacks(ctx, fqdn, 5)
		if len(list) > 0 && list[0].ID == r.ID && list[0].Status != "running" {
			if list[0].Status != "success" || list[0].BroughtBack != 1 || list[0].SetAside != 1 || list[0].CheckPassed == nil || !*list[0].CheckPassed {
				t.Fatalf("rollback: %+v", list[0])
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the rollback did not finish")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !slices.Equal(b.Copies, []string{p + "snapshots/aaaa@v1"}) || !slices.Equal(b.Markers, []string{p + "snapshots/bbbb"}) {
		t.Fatalf("what was done: copies %v, markers %v", b.Copies, b.Markers)
	}

	// After a rotation, a time before it is refused: the server no longer has that password.
	if err := q.Audit(ctx, store.AuditEntry{Actor: "root", Source: "cli", Action: "host.rotated", FQDN: fqdn}); err != nil {
		t.Fatal(err)
	}
	if pv, err := s.RollbackPreviewFor(ctx, fqdn, now.Add(-24*time.Hour)); err != nil || !strings.Contains(pv.Refused, "rotated") {
		t.Fatalf("before a rotation: %q %v", pv.Refused, err)
	}
}
