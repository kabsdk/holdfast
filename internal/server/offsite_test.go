package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kabsdk/holdfast/internal/config"
	"github.com/kabsdk/holdfast/internal/gateway"
	"github.com/kabsdk/holdfast/internal/proto"
	"github.com/kabsdk/holdfast/internal/server/creds"
	"github.com/kabsdk/holdfast/internal/server/storage"
	"github.com/kabsdk/holdfast/internal/server/store"
	"github.com/kabsdk/holdfast/internal/testpg"
)

// newOffsiteInstance is the off-site instance on a fresh database, not attached yet. The copier does not run in
// these tests: the feed is applied directly.
func newOffsiteInstance(t *testing.T) *Server {
	t.Helper()
	sealer, _ := creds.NewSealer(creds.Random(creds.KeySize))
	set := Settings{PublicURL: "https://holdfast.example.test", StateDir: t.TempDir(), ResticBin: fakeRestic(t), DefaultZone: "Europe/Copenhagen", NodeName: "test (off-site)",
		Storage: StorageSettings{Endpoint: "http://127.0.0.1:9000", Bucket: "holdfast-b"}, Maintenance: MaintenanceSettings{Check: CheckDaily, CheckCycleDays: 30},
		Role: RoleVault, Offsite: &OffsiteSettings{Poll: time.Minute}}
	s, err := New(set, Deps{Store: testpg.Store(t), Sealer: sealer, Prober: storage.StaticProber{},
		Maintenance: storage.Key{AccessKeyID: "MAINTB", SecretAccessKey: "maint-b-secret"}, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// newOffsitePair is a server with its off-site instance attached (decision 041).
func newOffsitePair(t *testing.T) (*Server, *Server) {
	t.Helper()
	s, off := newTestServer(t), newOffsiteInstance(t)
	ctx, stop := context.WithCancel(context.Background())
	t.Cleanup(stop)
	if err := s.AttachOffsite(ctx, off); err != nil {
		t.Fatal(err)
	}
	return s, off
}

// The server's side (decisions 039 and 041): the feed has the hosts, jobs, passwords, the copier's source and the
// bindings; the daily run holds back what is not copied yet for up to 7 days; and a restore from off-site is granted
// at once, reaching the host's agent sealed, with a grant to read its own copy through the gateway until it reports it.
func TestOffsiteServerSide(t *testing.T) {
	s, off := newOffsitePair(t)
	ctx := context.Background()
	q := s.st.Q()
	const fqdn = "web01.example.com"
	enrolled(t, s, fqdn, proto.Facts{Protocol: proto.Protocol, RestoreBounds: &proto.RestoreBounds{Root: "/var/restore"}})
	if _, err := s.SetJob(ctx, root, fqdn, "etc", SetJobInput{Job: *job("/etc")}); err != nil {
		t.Fatal(err)
	}

	// The feed: the host, its job and its password; where the copier reads; and the server's bindings.
	feed, err := s.offsiteSync(ctx, proto.VaultSync{Version: "test", Hosts: []proto.VaultHostReport{}})
	if err != nil {
		t.Fatal(err)
	}
	c, _ := q.GetCredentials(ctx, fqdn)
	pw, _ := s.seal.Open(c.RepoPassword, fqdn)
	if len(feed.Hosts) != 1 || feed.Hosts[0].FQDN != fqdn || feed.Hosts[0].RepoPassword != pw || len(feed.Hosts[0].Jobs) != 1 || feed.Hosts[0].Jobs[0].ID != "etc" ||
		feed.Hosts[0].Jobs[0].Schedule.Zone != "Europe/Copenhagen" || !strings.HasPrefix(feed.Source, "http://copier:") || !strings.HasSuffix(feed.Source, gateway.Base) ||
		len(feed.Access.Bindings) == 0 {
		t.Fatalf("the feed: %+v", feed)
	}
	if err := off.applyFeed(ctx, feed); err != nil {
		t.Fatal(err)
	}

	// Two snapshots here; the old one is copied off-site, the new one not yet.
	now := time.Now()
	const old, young, copyOfOld = "aaaa000000000000000000000000000000000000000000000000000000000001",
		"aaaa000000000000000000000000000000000000000000000000000000000002", "bbbb000000000000000000000000000000000000000000000000000000000001"
	if _, _, err := q.SyncSnapshots(ctx, fqdn, []store.Snapshot{
		{ID: old, ShortID: old[:8], Time: now.Add(-10 * 24 * time.Hour), Hostname: fqdn, JobID: "etc", Tags: []string{"job=etc"}, Paths: []string{"/etc"}},
		{ID: young, ShortID: young[:8], Time: now.Add(-time.Hour), Hostname: fqdn, JobID: "etc", Tags: []string{"job=etc"}, Paths: []string{"/etc"}},
	}, now); err != nil {
		t.Fatal(err)
	}
	if _, _, err := off.st.Q().SyncSnapshots(ctx, fqdn, []store.Snapshot{
		{ID: copyOfOld, ShortID: copyOfOld[:8], Time: now.Add(-10 * 24 * time.Hour), Hostname: fqdn, JobID: "etc", Tags: []string{"job=etc"}, Paths: []string{"/etc"}, Original: old},
	}, now); err != nil {
		t.Fatal(err)
	}
	rep, err := off.vaultReport(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.offsiteSync(ctx, rep); err != nil {
		t.Fatal(err)
	}
	h, _ := s.Host(ctx, fqdn)
	// What is wanted is worked out here, from the snapshots here and the copies there: the young one.
	if h.Offsite == nil || h.Offsite.Copies != 1 || h.Offsite.Pending != 1 {
		t.Fatalf("the host's off-site view: %+v", h.Offsite)
	}

	// The hold: the young one stays, as pending; the old one, past 7 days, goes as retention says.
	jr := JobRetention{JobID: "etc", Forget: []string{old, young}}
	s.holdForVaults(ctx, fqdn, &jr, s.vaultsWant(ctx, fqdn), now)
	if !slices.Equal(jr.Forget, []string{old}) || len(jr.Pending) != 1 || jr.Pending[0].ID != young || jr.Pending[0].Reason != PendingOffsite {
		t.Fatalf("the hold: forget %v, pending %+v", jr.Forget, jr.Pending)
	}

	// A restore from off-site: never in place; not of a snapshot with no copy; else queued at once.
	var ve *ValidationError
	if _, err := s.Restore(ctx, root, fqdn, old, RestoreInput{FromOffsite: true, Target: proto.TargetInPlace}); !errors.As(err, &ve) {
		t.Fatalf("in place from off-site: %v", err)
	}
	if _, err := s.Restore(ctx, root, fqdn, young, RestoreInput{FromOffsite: true}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a snapshot with no off-site copy: %v", err)
	}
	if s.offsiteAccess(ctx, fqdn, fqdn) != gateway.NoAccess {
		t.Fatal("a host reads its off-site copy before any restore")
	}
	res, err := s.Restore(ctx, root, fqdn, old, RestoreInput{FromOffsite: true, Message: "primary lost it"})
	if err != nil || res.Snapshot != copyOfOld {
		t.Fatalf("restore from off-site: %+v %v", res, err)
	}
	if s.offsiteAccess(ctx, fqdn, fqdn) != gateway.ReadOnly || s.offsiteAccess(ctx, "db01.example.com", fqdn) != gateway.NoAccess ||
		s.gatewayAccess(ctx, "db01.example.com", fqdn) != gateway.NoAccess {
		t.Fatal("during the restore, the host may read its own off-site copy, and nothing more")
	}
	p, err := s.Poll(ctx, fqdn, proto.PollRequest{Facts: proto.Facts{Protocol: proto.Protocol}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	var src *proto.RestoreSource
	for _, cmd := range p.Commands {
		if cmd.ID == res.Command && cmd.Restore != nil {
			src = cmd.Restore.Source
			if cmd.Restore.Snapshot != copyOfOld || cmd.Restore.Target != proto.TargetNew {
				t.Fatalf("the restore: %+v", cmd.Restore)
			}
		}
	}
	oc, _ := off.st.Q().GetCredentials(ctx, fqdn)
	offPW, _ := off.seal.Open(oc.RepoPassword, fqdn)
	if src == nil || src.FQDN != fqdn || !src.Offsite || src.RepoPassword != offPW || offPW == pw || !strings.Contains(src.Repository, gateway.OffsiteBase+fqdn+"/") {
		t.Fatalf("the agent's source: %+v", src)
	}
	if audit, _ := off.st.Q().ListAudit(ctx, fqdn, 20); !slices.ContainsFunc(audit, func(e store.AuditEntry) bool { return e.Action == "restore.granted" }) {
		t.Fatal("the off-site audit log does not have the grant")
	}
	// The restore is reported: the grant ends.
	if _, err := s.Events(ctx, fqdn, []proto.Event{{ID: "restored-1", Kind: proto.KindRestore, CommandID: res.Command, Status: proto.StatusSuccess, Started: now, Finished: now}}); err != nil {
		t.Fatal(err)
	}
	if s.offsiteAccess(ctx, fqdn, fqdn) != gateway.NoAccess {
		t.Fatal("the host may still read its off-site copy after the restore")
	}
	// A copy that is gone off-site is refused, and the refusal is in the off-site audit log.
	if _, _, err := off.st.Q().SyncSnapshots(ctx, fqdn, []store.Snapshot{}, now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Restore(ctx, root, fqdn, old, RestoreInput{FromOffsite: true}); !errors.Is(err, ErrConflict) {
		t.Fatalf("a copy that is gone off-site: %v", err)
	}
}

// Without offsite: nothing about off-site exists: no API, no view, no hold, no restore from it.
func TestNoOffsite(t *testing.T) {
	s, ctx := newTestServer(t), context.Background()
	const fqdn = "web01.example.com"
	enrolled(t, s, fqdn, proto.Facts{Protocol: proto.Protocol})
	if h, _ := s.Host(ctx, fqdn); h.Offsite != nil {
		t.Fatalf("an off-site view without off-site: %+v", h.Offsite)
	}
	if v, _ := s.SettingsFor(ctx, root); v.Offsite {
		t.Fatal("the settings say off-site is set up")
	}
	if len(s.vaultsWant(ctx, fqdn)) != 0 {
		t.Fatal("snapshots held back without off-site")
	}
	if _, err := s.Restore(ctx, root, fqdn, "aaaa0000", RestoreInput{FromOffsite: true}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a restore from off-site without off-site: %v", err)
	}
	if code, body := apiAs(t, s.APIHandler(false), root, "GET", "/api/v1/offsite/hosts", nil); code != http.StatusNotFound || !strings.Contains(string(body), "offsite:") {
		t.Fatalf("the off-site API without off-site: %d %s", code, body)
	}
}

// The off-site API is the off-site instance's, under /api/v1/offsite/, for the actor the server authenticated.
func TestOffsiteAPI(t *testing.T) {
	s, off := newOffsitePair(t)
	ctx := context.Background()
	const fqdn = "web01.example.com"
	enrolled(t, s, fqdn, proto.Facts{Protocol: proto.Protocol})
	if _, err := s.SetJob(ctx, root, fqdn, "etc", SetJobInput{Job: *job("/etc")}); err != nil {
		t.Fatal(err)
	}
	feed, err := s.offsiteSync(ctx, proto.VaultSync{})
	if err != nil {
		t.Fatal(err)
	}
	if err := off.applyFeed(ctx, feed); err != nil {
		t.Fatal(err)
	}
	h := s.APIHandler(false)
	var hosts []HostView
	code, body := apiAs(t, h, Actor{Name: "ada", Source: "ui", Groups: []string{"holdfast-admins"}}, "GET", "/api/v1/offsite/hosts", nil)
	if err := json.Unmarshal(body, &hosts); code != 200 || err != nil || len(hosts) != 1 || hosts[0].FQDN != fqdn || hosts[0].Jobs[0].Primary == nil {
		t.Fatalf("the off-site hosts: %d %s", code, body)
	}
	if code, _ := apiAs(t, h, Actor{Name: "nob", Source: "ui"}, "GET", "/api/v1/offsite/hosts/"+fqdn, nil); code != http.StatusNotFound {
		t.Fatalf("someone with no binding sees an off-site host: %d", code)
	}
	var set SettingsView
	if _, body := apiAs(t, h, root, "GET", "/api/v1/offsite/settings", nil); json.Unmarshal(body, &set) != nil || set.Role != RoleVault || !set.Offsite {
		t.Fatalf("the off-site settings: %s", body)
	}
	if v, _ := s.SettingsFor(ctx, root); !v.Offsite || v.Role == RoleVault {
		t.Fatalf("the server's settings: %+v", v)
	}
}

// A deletion an admin asked for is what the off-site pages show as deleted in primary; a forget by retention is not.
func TestDeletedOnRequestReachesTheFeed(t *testing.T) {
	s, _ := newOffsitePair(t)
	ctx := context.Background()
	q := s.st.Q()
	const fqdn = "web01.example.com"
	enrolled(t, s, fqdn, proto.Facts{Protocol: proto.Protocol})
	const id = "cccc000000000000000000000000000000000000000000000000000000000001"
	if _, _, err := q.SyncSnapshots(ctx, fqdn, []store.Snapshot{{ID: id, ShortID: id[:8], Time: time.Now(), Hostname: fqdn, JobID: "etc"}}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := q.MarkDeletedOnRequest(ctx, fqdn, []string{id}, time.Now()); err != nil {
		t.Fatal(err)
	}
	feed, err := s.offsiteSync(ctx, proto.VaultSync{})
	if err != nil || len(feed.Hosts) != 1 || feed.Hosts[0].DeletedInPrimary[id].IsZero() {
		t.Fatalf("deleted in primary: %+v %v", feed.Hosts, err)
	}
}

// The feed's rules (decision 039): a new job's off-site retention starts as primary's; later changes reach everything
// but the off-site retention and Exclude; a job or host the server no longer has is Retired; the bindings follow.
func TestFollowFeed(t *testing.T) {
	primary := job().Retention
	longer := primary
	longer.Yearly = &config.Duration{N: 5, Unit: config.Years}
	sched := job().Schedule
	feed := proto.VaultFeed{Hosts: []proto.VaultHost{{FQDN: "web01.example.com", Jobs: []proto.VaultJob{{ID: "etc", Paths: []string{"/etc"}, Schedule: sched, Retention: primary}}}}}
	doc, changes := followFeed(config.New(), feed)
	j := doc.Hosts["web01.example.com"].Jobs["etc"]
	if len(changes) != 2 || j.Retention.Yearly != nil || j.Primary == nil || j.Engine != config.EngineRestic || j.Retired {
		t.Fatalf("first sync: %v %+v", changes, j)
	}
	if p := doc.Validate(); len(p) > 0 {
		t.Fatalf("the off-site config does not validate: %v", p)
	}

	// Off-site keeps longer and excludes; primary changes its paths and its retention.
	j.Retention, j.Excluded = longer, true
	doc.Hosts["web01.example.com"].Jobs["etc"] = j
	shorter := primary
	shorter.KeepLast = 1
	feed.Hosts[0].Jobs[0].Paths, feed.Hosts[0].Jobs[0].Retention = []string{"/etc", "/srv"}, shorter
	doc, changes = followFeed(doc, feed)
	j = doc.Hosts["web01.example.com"].Jobs["etc"]
	if len(changes) != 1 || !slices.Equal(j.Paths, []string{"/etc", "/srv"}) || j.Retention.Yearly == nil || !j.Excluded || j.Primary.KeepLast != 1 {
		t.Fatalf("the server's change: %v %+v", changes, j)
	}
	if _, again := followFeed(doc, feed); len(again) != 0 {
		t.Fatalf("an unchanged feed changes %v", again)
	}

	// The bindings follow the server's.
	feed.Access.Bindings = []config.Binding{{Group: "ops", Role: "admin", Hosts: []string{"*"}}}
	doc, changes = followFeed(doc, feed)
	if len(changes) != 1 || !reflect.DeepEqual(doc.Access.Bindings, feed.Access.Bindings) {
		t.Fatalf("the bindings: %v %+v", changes, doc.Access.Bindings)
	}

	// The job goes: Retired, kept. It comes back: not retired. The host goes: all its jobs retired.
	gone := proto.VaultFeed{Access: feed.Access, Hosts: []proto.VaultHost{{FQDN: "web01.example.com"}}}
	doc, _ = followFeed(doc, gone)
	if !doc.Hosts["web01.example.com"].Jobs["etc"].Retired {
		t.Fatal("a job the server removed is not retired")
	}
	doc, _ = followFeed(doc, feed)
	if doc.Hosts["web01.example.com"].Jobs["etc"].Retired {
		t.Fatal("a job back on the server is still retired")
	}
	doc, _ = followFeed(doc, proto.VaultFeed{Access: feed.Access})
	if !doc.Hosts["web01.example.com"].Jobs["etc"].Retired {
		t.Fatal("a host the server removed keeps active jobs")
	}
}

// In the off-site instance: hosts and jobs follow the server; only the off-site retention and Exclude are its own,
// with O101 when it keeps less than primary; nothing is backed up, enrolled or restored there, and its bindings are
// the server's.
func TestOffsiteInstance(t *testing.T) {
	s, ctx := newOffsiteInstance(t), context.Background()
	const fqdn = "web01.example.com"
	primary := job().Retention
	bindings := append(config.DefaultAccess().Bindings, config.Binding{Group: "web-team", Role: "viewer", Hosts: []string{fqdn}})
	feed := proto.VaultFeed{Access: config.Access{Bindings: bindings},
		Hosts: []proto.VaultHost{{FQDN: fqdn, Repository: "s3:http://site-a:9000/holdfast/repos/" + fqdn, RepoPassword: "a-password",
			Jobs: []proto.VaultJob{{ID: "etc", Paths: []string{"/etc"}, Schedule: job().Schedule, Retention: primary}}}}}
	if err := s.applyFeed(ctx, feed); err != nil {
		t.Fatal(err)
	}
	h, err := s.Host(ctx, fqdn)
	if err != nil || h.State != "enrolled" || len(h.Jobs) != 1 || h.Jobs[0].Primary == nil {
		t.Fatalf("the copied host: %+v %v", h, err)
	}
	c, _ := s.st.Q().GetCredentials(ctx, fqdn)
	if pw, _ := s.seal.Open(c.RepoPassword, fqdn); pw == "" || pw == "a-password" || c.Repository != "s3:http://127.0.0.1:9000/holdfast-b/repos/"+fqdn {
		t.Fatalf("the off-site repository and password: %+v", c)
	}
	if head, _ := s.head(ctx, s.st.Q()); !reflect.DeepEqual(head.Config.Access.Bindings, bindings) {
		t.Fatalf("the off-site bindings: %+v", head.Config.Access.Bindings)
	}

	for name, err := range map[string]error{
		"add host": func() error { _, err := s.AddHost(ctx, root, AddHostInput{FQDN: "new.example.com"}); return err }(),
		"enroll":   func() error { _, err := s.IssueEnrollmentCode(ctx, root, fqdn); return err }(),
		"run now":  func() error { _, err := s.RunNow(ctx, root, fqdn, "etc"); return err }(),
		"restore":  func() error { _, err := s.Restore(ctx, root, fqdn, "abcdef12", RestoreInput{}); return err }(),
		"remove a live job": func() error {
			_, err := s.RemoveJob(ctx, root, fqdn, "etc", RemoveInput{})
			return err
		}(),
		"set the bindings": func() error { _, err := s.SetAccess(ctx, root, SetAccessInput{Bindings: bindings}); return err }(),
	} {
		if !errors.Is(err, ErrConflict) {
			t.Errorf("%s off-site: %v", name, err)
		}
	}

	// A job's save keeps the paths from primary, whatever it is given; keeping less than primary is O101.
	j := *job("/elsewhere")
	j.Retention.Monthly = nil
	var ce *ChecksError
	if _, err := s.SetJob(ctx, root, fqdn, "etc", SetJobInput{Job: j}); !errors.As(err, &ce) || !slices.Contains(codes(ce.Blocking), "O101") {
		t.Fatalf("a shorter off-site retention: %v", err)
	}
	if _, err := s.SetJob(ctx, root, fqdn, "etc", SetJobInput{Job: j, Accept: []string{"O101", "R101"}}); err != nil {
		t.Fatal(err)
	}
	h, _ = s.Host(ctx, fqdn)
	if !slices.Equal(h.Jobs[0].Paths, []string{"/etc"}) || h.Jobs[0].Retention.Monthly != nil {
		t.Fatalf("after the save: %+v", h.Jobs[0])
	}
	if _, err := s.MatchPrimary(ctx, root, fqdn, "etc", OffsiteInput{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetExcluded(ctx, root, fqdn, "etc", true, OffsiteInput{}); err != nil {
		t.Fatal(err)
	}
	h, _ = s.Host(ctx, fqdn)
	if h.Jobs[0].Retention.Monthly == nil || !h.Jobs[0].Excluded {
		t.Fatalf("after Match primary and Exclude: %+v", h.Jobs[0])
	}
	if _, err := s.RemoveHost(ctx, root, fqdn, RemoveInput{}); !errors.Is(err, ErrConflict) {
		t.Fatalf("removing a host the server still has: %v", err)
	}

	// The server removes the host: its jobs are retired, and an admin may remove it off-site, which retires it at once.
	if err := s.applyFeed(ctx, proto.VaultFeed{Access: feed.Access}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RemoveHost(ctx, root, fqdn, RemoveInput{}); err != nil {
		t.Fatal(err)
	}
	if h, _ := s.st.Q().GetHost(ctx, fqdn); h.State != "retired" {
		t.Fatalf("the removed host: %s", h.State)
	}
	// It comes back on the server: back off-site, with its repository and password.
	if err := s.applyFeed(ctx, feed); err != nil {
		t.Fatal(err)
	}
	c2, _ := s.st.Q().GetCredentials(ctx, fqdn)
	if h, _ := s.st.Q().GetHost(ctx, fqdn); h.State != "enrolled" || c2.Version != c.Version {
		t.Fatalf("the host back: %s, credentials %d then %d", h.State, c.Version, c2.Version)
	}

	// The report: the job, with its retention and the copy state.
	rep, err := s.vaultReport(ctx)
	if err != nil || len(rep.Hosts) != 1 || len(rep.Hosts[0].Jobs) != 1 || rep.Hosts[0].Retired {
		t.Fatalf("the report: %+v %v", rep, err)
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
