package store

import (
	"context"
	"testing"
	"time"
)

// An export loads into an empty database whole: every table but the skipped ones, with foreign keys in order, and
// each serial sequence past the rows loaded, so the next audit entry does not collide.
func TestExportLoadsIntoAnEmptyDatabase(t *testing.T) {
	ctx := context.Background()
	src := testStore(t)
	q := src.Q()
	now := time.Now().UTC().Truncate(time.Microsecond)
	rev, err := q.SaveRevision(ctx, SaveRevision{Actor: "ada", Source: "cli", Message: "add web01", Config: docWith("web01.example.com"),
		HostSHAs: map[string]string{"web01.example.com": "aa"}})
	must(t, err)
	must(t, q.CreateHost(ctx, "web01.example.com", rev.Number, now))
	must(t, q.PutCredentials(ctx, SealedCredentials{FQDN: "web01.example.com", Version: 1, Repository: "s3:https://s3.example/b/repos/web01.example.com",
		RepoPassword: []byte("sealed-password"), CreatedAt: now}))
	for i := 0; i < 3; i++ {
		must(t, q.Audit(ctx, AuditEntry{Actor: "ada", Source: "cli", Action: "host.added", FQDN: "web01.example.com", Revision: rev.Number}))
	}
	must(t, q.CreateSession(ctx, Session{TokenSHA256: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", Kind: "cli",
		Subject: "ada", Username: "ada", CreatedAt: now, ExpiresAt: now.Add(time.Hour)}))

	tables, version, err := src.ExportTables(ctx)
	must(t, err)
	if version != LatestVersion() {
		t.Fatalf("export version %d, latest %d", version, LatestVersion())
	}
	names := map[string]bool{}
	for _, tb := range tables {
		names[tb.Name] = true
	}
	for _, skipped := range []string{"sessions", "gateway_grants", "schema_migrations"} {
		if names[skipped] {
			t.Fatalf("the export holds %s", skipped)
		}
	}
	for _, kept := range []string{"revisions", "hosts", "host_credentials", "audit_log", "agents", "host_desired"} {
		if !names[kept] {
			t.Fatalf("the export lacks %s: %v", kept, names)
		}
	}

	dst := emptyTestStore(t)
	empty, err := dst.Empty(ctx)
	must(t, err)
	if !empty {
		t.Fatal("a new database is not empty")
	}
	must(t, dst.MigrateTo(ctx, version))
	must(t, dst.LoadTables(ctx, tables))
	must(t, dst.Migrate(ctx))
	if empty, _ := dst.Empty(ctx); empty {
		t.Fatal("the loaded database is empty")
	}

	d := dst.Q()
	h, err := d.GetHost(ctx, "web01.example.com")
	must(t, err)
	if h.FQDN != "web01.example.com" || !h.AddedAt.Equal(now) {
		t.Fatalf("host: %+v", h)
	}
	c, err := d.GetCredentials(ctx, "web01.example.com")
	must(t, err)
	if string(c.RepoPassword) != "sealed-password" || c.Version != 1 {
		t.Fatalf("credentials: %+v", c)
	}
	head, err := d.Head(ctx)
	must(t, err)
	if head.Number != rev.Number || head.SHA256 != rev.SHA256 {
		t.Fatalf("head %d %s, saved %d %s", head.Number, head.SHA256, rev.Number, rev.SHA256)
	}
	before, err := d.ListAudit(ctx, "", 100)
	must(t, err)
	must(t, d.Audit(ctx, AuditEntry{Actor: "holdfast-server", Source: "system", Action: "server.restored"}))
	after, err := d.ListAudit(ctx, "", 100)
	must(t, err)
	if len(after) != len(before)+1 {
		t.Fatalf("audit entries: %d before, %d after a new one", len(before), len(after))
	}

	// Loading again into a database that holds rows fails, and leaves it as it was.
	if err := dst.LoadTables(ctx, tables); err == nil {
		t.Fatal("loading twice succeeded")
	}
}
