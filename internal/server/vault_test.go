package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kabsdk/holdfast/internal/config"
	"github.com/kabsdk/holdfast/internal/proto"
	"github.com/kabsdk/holdfast/internal/server/creds"
	"github.com/kabsdk/holdfast/internal/server/storage"
	"github.com/kabsdk/holdfast/internal/server/store"
	"github.com/kabsdk/holdfast/internal/testpg"
)

// newVaultTestServer is a holdfast-server in the vault role, on a fresh database. Its source is not reached in
// these tests: the feed is applied directly.
func newVaultTestServer(t *testing.T) *Server {
	t.Helper()
	sealer, _ := creds.NewSealer(creds.Random(creds.KeySize))
	set := Settings{PublicURL: "https://vault.example.test", StateDir: t.TempDir(), ResticBin: fakeRestic(t), DefaultZone: "Europe/Copenhagen", NodeName: "vault",
		Storage: StorageSettings{Endpoint: "http://127.0.0.1:9000", Bucket: "holdfast-b"}, Maintenance: MaintenanceSettings{Check: CheckDaily, CheckCycleDays: 30},
		Role: RoleVault, Source: SourceSettings{Server: "https://holdfast.site-a.test:8443", TokenFile: "/nonexistent", Poll: time.Minute}}
	s, err := New(set, Deps{Store: testpg.Store(t), Sealer: sealer, Prober: storage.StaticProber{},
		Maintenance: storage.Key{AccessKeyID: "MAINTB", SecretAccessKey: "maint-b-secret"}, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// The source side (decision 039): a vault is registered with a read-only key and gets a token; its sync gets the
// hosts, jobs and passwords; the daily run holds back what it still wants for up to 7 days; and a restore from it
// waits for its grant, which reaches the host's agent sealed, once.
func TestVaultSourceSide(t *testing.T) {
	s, ctx := newTestServer(t), context.Background()
	q := s.st.Q()
	const fqdn = "web01.example.com"
	enrolled(t, s, fqdn, proto.Facts{Protocol: proto.Protocol, RestoreBounds: &proto.RestoreBounds{Root: "/var/restore"}})
	if _, err := s.SetJob(ctx, root, fqdn, "etc", SetJobInput{Job: *job("/etc")}); err != nil {
		t.Fatal(err)
	}

	key := S3KeyInput{AccessKeyID: "VAULTREAD", SecretAccessKey: "vault-read-secret"}
	var ve *ValidationError
	if _, err := s.AddVault(ctx, root, AddVaultInput{Name: "Site B", S3Key: key}); !errors.As(err, &ve) {
		t.Fatalf("a bad name: %v", err)
	}
	if _, err := s.AddVault(ctx, Actor{Name: "ed", Source: "cli"}, AddVaultInput{Name: "site-b", S3Key: key}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("a non-admin: %v", err)
	}
	reg, err := s.AddVault(ctx, root, AddVaultInput{Name: "site-b", S3Key: key})
	if err != nil || reg.Token == "" {
		t.Fatalf("add vault: %+v %v", reg, err)
	}
	if _, err := s.AddVault(ctx, root, AddVaultInput{Name: "site-b", S3Key: key}); !errors.Is(err, ErrConflict) {
		t.Fatalf("the same vault again: %v", err)
	}
	v, err := q.VaultByToken(ctx, creds.HashToken(reg.Token))
	if err != nil {
		t.Fatal(err)
	}
	if list, _ := s.Vaults(ctx, root); len(list) != 1 || strings.Contains(mustJSON(t, list), "vault-read-secret") {
		t.Fatalf("the vault list: %+v", list)
	}

	// The feed: the host, its job, its password, and the read-only key.
	feed, err := s.VaultSync(ctx, v, []byte(`{"version":"test","hosts":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	c, _ := q.GetCredentials(ctx, fqdn)
	pw, _ := s.seal.Open(c.RepoPassword, fqdn)
	if len(feed.Hosts) != 1 || feed.Hosts[0].FQDN != fqdn || feed.Hosts[0].RepoPassword != pw || len(feed.Hosts[0].Jobs) != 1 || feed.Hosts[0].Jobs[0].ID != "etc" ||
		feed.Source.S3AccessKeyID != "VAULTREAD" || feed.Source.S3SecretAccessKey != "vault-read-secret" || feed.Hosts[0].Jobs[0].Schedule.Zone != "Europe/Copenhagen" {
		t.Fatalf("the feed: %+v", feed)
	}

	// Two snapshots here; the vault has copied the old one and wants the new one.
	now := time.Now()
	const old, young, copyOfOld = "aaaa000000000000000000000000000000000000000000000000000000000001",
		"aaaa000000000000000000000000000000000000000000000000000000000002", "bbbb000000000000000000000000000000000000000000000000000000000001"
	if _, _, err := q.SyncSnapshots(ctx, fqdn, []store.Snapshot{
		{ID: old, ShortID: old[:8], Time: now.Add(-10 * 24 * time.Hour), Hostname: fqdn, JobID: "etc", Tags: []string{"job=etc"}, Paths: []string{"/etc"}},
		{ID: young, ShortID: young[:8], Time: now.Add(-time.Hour), Hostname: fqdn, JobID: "etc", Tags: []string{"job=etc"}, Paths: []string{"/etc"}},
	}, now); err != nil {
		t.Fatal(err)
	}
	rep := proto.VaultSync{Hosts: []proto.VaultHostReport{{FQDN: fqdn, Pending: []string{young, old},
		Copies: []proto.VaultCopy{{ID: copyOfOld, Original: old, Time: now.Add(-10 * 24 * time.Hour), JobID: "etc"}},
		Jobs:   []proto.VaultJobReport{{ID: "etc", Retention: job().Retention, Copies: 1}}}}}
	if _, err := s.VaultSync(ctx, v, []byte(mustJSON(t, rep))); err != nil {
		t.Fatal(err)
	}
	h, _ := s.Host(ctx, fqdn)
	if len(h.Offsite) != 1 || h.Offsite[0].Vault != "site-b" || h.Offsite[0].Copies != 1 || h.Offsite[0].Pending != 2 {
		t.Fatalf("the host's off-site view: %+v", h.Offsite)
	}

	// The hold: the young one stays, as pending; the old one, past 7 days, goes as retention says.
	jr := JobRetention{JobID: "etc", Forget: []string{old, young}}
	s.holdForVaults(ctx, fqdn, &jr, s.vaultsWant(ctx, fqdn), now)
	if !slices.Equal(jr.Forget, []string{old}) || len(jr.Pending) != 1 || jr.Pending[0].ID != young || jr.Pending[0].Reason != PendingOffsite {
		t.Fatalf("the hold: forget %v, pending %+v", jr.Forget, jr.Pending)
	}

	// A restore from the vault: never in place; then a request, which the vault's next sync sees.
	if _, err := s.Restore(ctx, root, fqdn, old, RestoreInput{FromVault: "site-b", Target: proto.TargetInPlace}); !errors.As(err, &ve) {
		t.Fatalf("in place from a vault: %v", err)
	}
	if _, err := s.Restore(ctx, root, fqdn, young, RestoreInput{FromVault: "site-b"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a snapshot the vault has no copy of: %v", err)
	}
	res, err := s.Restore(ctx, root, fqdn, old, RestoreInput{FromVault: "site-b", Message: "site A lost it"})
	if err != nil || res.Snapshot != copyOfOld {
		t.Fatalf("restore from the vault: %+v %v", res, err)
	}
	if rv, _ := s.Restores(ctx, 10); len(rv.WaitingForVault) != 1 || rv.WaitingForVault[0].ID != res.Command {
		t.Fatalf("waiting for the vault: %+v", rv.WaitingForVault)
	}
	feed, _ = s.VaultSync(ctx, v, []byte(mustJSON(t, rep)))
	if len(feed.Requests) != 1 || feed.Requests[0].Snapshot != copyOfOld {
		t.Fatalf("the request in the feed: %+v", feed.Requests)
	}

	// The grant: the host's agent gets site B's repository, password and key, sealed, once.
	g := proto.VaultGrant{ID: res.Command, Repository: "s3:http://site-b:9000/holdfast-b/repos/" + fqdn, RepoPassword: "b-password",
		S3AccessKeyID: "BREAD", S3SecretAccessKey: "b-read-secret"}
	if err := s.VaultGrant(ctx, v, g); err != nil {
		t.Fatal(err)
	}
	if err := s.VaultGrant(ctx, v, g); !errors.Is(err, ErrConflict) {
		t.Fatalf("a second grant: %v", err)
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
	if src == nil || src.FQDN != fqdn || src.RepoPassword != "b-password" || src.S3SecretAccessKey != "b-read-secret" {
		t.Fatalf("the agent's source: %+v", src)
	}

	// A refusal ends the restore in the host's history, so holdfast restore --wait ends too.
	res2, err := s.Restore(ctx, root, fqdn, copyOfOld[:8], RestoreInput{FromVault: "site-b"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.VaultGrant(ctx, v, proto.VaultGrant{ID: res2.Command, Refused: "no read-only key"}); err != nil {
		t.Fatal(err)
	}
	evs, _ := s.HostEvents(ctx, fqdn, 20)
	if !slices.ContainsFunc(evs, func(e store.Event) bool {
		return e.CommandID == res2.Command && e.Status == proto.StatusFailed && strings.Contains(e.Error, "no read-only key")
	}) {
		t.Fatalf("the refused restore's event: %+v", evs)
	}

	// Removing the vault: its token stops working, and nothing is held back for it.
	if err := s.RemoveVault(ctx, root, "site-b", RemoveInput{}); err != nil {
		t.Fatal(err)
	}
	if _, err := q.VaultByToken(ctx, creds.HashToken(reg.Token)); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("the removed vault's token: %v", err)
	}
	if len(s.vaultsWant(ctx, fqdn)) != 0 {
		t.Fatal("a removed vault still holds snapshots back")
	}
}

// A deletion an admin asked for is what a vault shows as deleted in primary; a forget by retention is not.
func TestDeletedOnRequestReachesTheFeed(t *testing.T) {
	s, ctx := newTestServer(t), context.Background()
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
	reg, err := s.AddVault(ctx, root, AddVaultInput{Name: "site-b", S3Key: S3KeyInput{AccessKeyID: "VAULTREAD", SecretAccessKey: "vault-read-secret"}})
	if err != nil {
		t.Fatal(err)
	}
	v, _ := q.VaultByToken(ctx, creds.HashToken(reg.Token))
	feed, err := s.VaultSync(ctx, v, []byte(`{"hosts":[]}`))
	if err != nil || len(feed.Hosts) != 1 || feed.Hosts[0].DeletedInPrimary[id].IsZero() {
		t.Fatalf("deleted in primary: %+v %v", feed.Hosts, err)
	}
}

// The feed's rules (decision 039): a new job's off-site retention starts as primary's; later changes in the source
// reach everything but the off-site retention and Exclude; a job or host the source no longer has is Retired.
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
		t.Fatalf("the vault's config does not validate: %v", p)
	}

	// The vault admin keeps longer and excludes; the source changes its paths and its retention.
	j.Retention, j.Excluded = longer, true
	doc.Hosts["web01.example.com"].Jobs["etc"] = j
	shorter := primary
	shorter.KeepLast = 1
	feed.Hosts[0].Jobs[0].Paths, feed.Hosts[0].Jobs[0].Retention = []string{"/etc", "/srv"}, shorter
	doc, changes = followFeed(doc, feed)
	j = doc.Hosts["web01.example.com"].Jobs["etc"]
	if len(changes) != 1 || !slices.Equal(j.Paths, []string{"/etc", "/srv"}) || j.Retention.Yearly == nil || !j.Excluded || j.Primary.KeepLast != 1 {
		t.Fatalf("the source's change: %v %+v", changes, j)
	}
	if _, again := followFeed(doc, feed); len(again) != 0 {
		t.Fatalf("an unchanged feed changes %v", again)
	}

	// The job goes in the source: Retired, kept. It comes back: not retired. The host goes: all its jobs retired.
	gone := proto.VaultFeed{Hosts: []proto.VaultHost{{FQDN: "web01.example.com"}}}
	doc, _ = followFeed(doc, gone)
	if !doc.Hosts["web01.example.com"].Jobs["etc"].Retired {
		t.Fatal("a job the source removed is not retired")
	}
	doc, _ = followFeed(doc, feed)
	if doc.Hosts["web01.example.com"].Jobs["etc"].Retired {
		t.Fatal("a job back in the source is still retired")
	}
	doc, _ = followFeed(doc, proto.VaultFeed{})
	if !doc.Hosts["web01.example.com"].Jobs["etc"].Retired {
		t.Fatal("a host the source removed keeps active jobs")
	}
}

// In the vault: hosts and jobs come from the source; only the off-site retention is the vault's, with O101 when it
// keeps less than primary; nothing is backed up, enrolled or restored there.
func TestVaultRole(t *testing.T) {
	s, ctx := newVaultTestServer(t), context.Background()
	const fqdn = "web01.example.com"
	primary := job().Retention
	feed := proto.VaultFeed{Source: proto.VaultSource{Endpoint: "http://site-a:9000", Bucket: "holdfast"},
		Hosts: []proto.VaultHost{{FQDN: fqdn, Repository: "s3:http://site-a:9000/holdfast/repos/" + fqdn, RepoPassword: "a-password",
			Jobs: []proto.VaultJob{{ID: "etc", Paths: []string{"/etc"}, Schedule: job().Schedule, Retention: primary}}}}}
	if err := s.applyFeed(ctx, feed); err != nil {
		t.Fatal(err)
	}
	h, err := s.Host(ctx, fqdn)
	if err != nil || h.State != "enrolled" || len(h.Jobs) != 1 || h.Jobs[0].Primary == nil || !h.RestoreKeyMissing {
		t.Fatalf("the copied host: %+v %v", h, err)
	}
	c, _ := s.st.Q().GetCredentials(ctx, fqdn)
	if pw, _ := s.seal.Open(c.RepoPassword, fqdn); pw == "" || pw == "a-password" || c.Repository != "s3:http://127.0.0.1:9000/holdfast-b/repos/"+fqdn {
		t.Fatalf("site B's own repository and password: %+v", c)
	}
	if head, _ := s.head(ctx, s.st.Q()); !slices.ContainsFunc(head.Config.Access.Bindings, func(b config.Binding) bool { return b.Group == "holdfast-vault-admins" }) {
		t.Fatalf("the vault's bindings: %+v", head.Config.Access.Bindings)
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
		"add vault": func() error { _, err := s.AddVault(ctx, root, AddVaultInput{Name: "x"}); return err }(),
	} {
		if !errors.Is(err, ErrConflict) {
			t.Errorf("%s in a vault: %v", name, err)
		}
	}

	// A job's save keeps the source's paths, whatever it is given; keeping less than primary is O101.
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
		t.Fatalf("removing a host the source still has: %v", err)
	}

	// The source removes the host: its jobs are retired, and the vault admin may remove it, which retires it at once.
	if err := s.applyFeed(ctx, proto.VaultFeed{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RemoveHost(ctx, root, fqdn, RemoveInput{}); err != nil {
		t.Fatal(err)
	}
	if h, _ := s.st.Q().GetHost(ctx, fqdn); h.State != "retired" {
		t.Fatalf("the removed host: %s", h.State)
	}
	// It comes back in the source: back in the vault, with its repository and password.
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

// The grant the vault gives: refused without a copy or a read-only key, else the repository, password and key.
func TestVaultGrants(t *testing.T) {
	s, ctx := newVaultTestServer(t), context.Background()
	const fqdn = "web01.example.com"
	if err := s.applyFeed(ctx, proto.VaultFeed{Hosts: []proto.VaultHost{{FQDN: fqdn, Jobs: []proto.VaultJob{{ID: "etc", Paths: []string{"/etc"}, Schedule: job().Schedule, Retention: job().Retention}}}}}); err != nil {
		t.Fatal(err)
	}
	const cp = "dddd000000000000000000000000000000000000000000000000000000000001"
	if _, _, err := s.st.Q().SyncSnapshots(ctx, fqdn, []store.Snapshot{{ID: cp, ShortID: cp[:8], Time: time.Now(), Hostname: fqdn, JobID: "etc", Original: "a1"}}, time.Now()); err != nil {
		t.Fatal(err)
	}
	ask := func(id string) proto.VaultGrant {
		g, _ := s.grantFor(ctx, proto.VaultRequest{ID: "r", FQDN: fqdn, Snapshot: id})
		return g
	}
	if g := ask(cp); !strings.Contains(g.Refused, "read-only key") {
		t.Fatalf("without a key: %+v", g)
	}
	if _, err := s.SetS3Key(ctx, root, fqdn, SetS3KeyInput{S3Key: S3KeyInput{AccessKeyID: "BREAD", SecretAccessKey: "b-read-secret"}}); err != nil {
		t.Fatal(err)
	}
	if g := ask("eeee0000"); !strings.Contains(g.Refused, "no longer") {
		t.Fatalf("an unknown copy: %+v", g)
	}
	if g := ask(cp); g.Refused != "" || g.S3AccessKeyID != "BREAD" || g.S3SecretAccessKey != "b-read-secret" || !strings.HasSuffix(g.Repository, "/holdfast-b/repos/"+fqdn) || g.RepoPassword == "" {
		t.Fatalf("the grant: %+v", g)
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
