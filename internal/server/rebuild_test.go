package server

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	"filippo.io/age"

	"github.com/kabsdk/holdfast/internal/proto"
	"github.com/kabsdk/holdfast/internal/server/export"
	"github.com/kabsdk/holdfast/internal/server/storage"
	"github.com/kabsdk/holdfast/internal/server/store"
)

func serverProblem(ps []Problem, code string) *Problem {
	for i := range ps {
		if ps[i].Code == code {
			return &ps[i]
		}
	}
	return nil
}

// The nightly export (decision 040): due at 00:30 in the default zone, at once after a missed one, an
// hour after a failed one; written encrypted to meta/<date>.age; the newest 30 kept; a failure shown and audited.
func TestNightlyExport(t *testing.T) {
	id, _ := age.GenerateX25519Identity()
	zone, _ := time.LoadLocation("Europe/Copenhagen")
	now := time.Date(2026, 11, 22, 10, 0, 0, 0, zone)
	b := &storage.MemBucket{}
	s := newTestServerWith(t, func(d *Deps) { d.Bucket = b; d.Now = func() time.Time { return now } })
	ctx := context.Background()
	if _, err := s.AddHost(ctx, root, AddHostInput{FQDN: "web01.example.com", JobID: "system", Job: job("/etc")}); err != nil {
		t.Fatal(err)
	}

	// Without a recipient there is no export, and the server says so.
	var ve *ValidationError
	if _, err := s.ExportNow(ctx, root); !errors.As(err, &ve) {
		t.Fatalf("export without a recipient: %v", err)
	}
	if p := serverProblem(s.serverProblems(ctx), "export"); p == nil || p.Severity != "warning" {
		t.Fatalf("problems without a recipient: %+v", s.serverProblems(ctx))
	}

	s.set.Escrow.AgeRecipient = id.Recipient().String()
	st, _ := s.st.Q().ExportState(ctx)
	if due := s.exportDueAt(st, now); !due.Equal(now) {
		t.Fatalf("never exported: due %v, want now", due)
	}
	rec, err := s.ExportNow(ctx, root)
	if err != nil || rec.Name != "2026-11-22.age" || rec.Bytes == 0 || rec.Error != "" {
		t.Fatalf("export now: %+v %v", rec, err)
	}
	c, err := export.Open(b.Meta["2026-11-22.age"], id)
	if err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for _, tb := range c.Tables["public"] {
		names = append(names, tb.Name)
	}
	if len(c.Manifest.Schemas) != 1 || c.Manifest.Schemas[0].Version != store.LatestVersion() || !slices.Contains(names, "hosts") || slices.Contains(names, "sessions") {
		t.Fatalf("export: %+v, tables %v", c.Manifest, names)
	}
	st, _ = s.st.Q().ExportState(ctx)
	next := time.Date(2026, 11, 23, 0, 30, 0, 0, zone)
	if due := s.exportDueAt(st, now); !due.Equal(next) {
		t.Fatalf("after an export at 10:00: due %v, want %v", due, next)
	}
	if ps := s.serverProblems(ctx); len(ps) != 0 {
		t.Fatalf("problems after an export: %+v", ps)
	}

	// Night after night: the newest 30 stay in the bucket.
	for i := 1; i <= 32; i++ {
		now = time.Date(2026, 11, 22+i, 0, 31, 0, 0, zone)
		st, _ := s.st.Q().ExportState(ctx)
		if due := s.exportDueAt(st, now); due.After(now) {
			t.Fatalf("night %d: not due at %v (due %v)", i, now, due)
		}
		if _, err := s.export(ctx, systemActor); err != nil {
			t.Fatal(err)
		}
	}
	kept, _ := b.ListMeta(ctx)
	if len(kept) != ExportKeep || kept[len(kept)-1] != storage.MetaName(now) {
		t.Fatalf("kept %d exports, newest %s", len(kept), kept[len(kept)-1])
	}

	// A failure: shown, audited, and tried again an hour later.
	b.MetaErr = errors.New("writing meta/x: AccessDenied")
	now = now.Add(24 * time.Hour)
	failed, err := s.ExportNow(ctx, root)
	if err == nil || failed.Error == "" || failed.ID == 0 {
		t.Fatalf("a failed export: %+v %v", failed, err)
	}
	if p := serverProblem(s.serverProblems(ctx), "export"); p == nil || p.Severity != "error" {
		t.Fatalf("problems after a failure: %+v", s.serverProblems(ctx))
	}
	st, _ = s.st.Q().ExportState(ctx)
	if due := s.exportDueAt(st, now); !due.Equal(failed.StartedAt.Add(exportRetry)) {
		t.Fatalf("after a failure: due %v, want an hour later", due)
	}
	if st.LastGood == nil || st.LastGood.StartedAt.After(failed.StartedAt) {
		t.Fatalf("last good: %+v", st.LastGood)
	}
	audit, _ := s.st.Q().ListAudit(ctx, "", 200)
	var written, failures int
	for _, a := range audit {
		switch a.Action {
		case "export.written":
			written++
		case "export.failed":
			failures++
		}
	}
	if written != 33 || failures != 1 {
		t.Fatalf("audit: %d written, %d failed", written, failures)
	}
}

// With off-site copies the export holds the off-site tables too, and is written to both buckets; a failed off-site
// write leaves the export counted, and is its own problem.
func TestExportOffsite(t *testing.T) {
	id, _ := age.GenerateX25519Identity()
	s, off := newOffsitePair(t)
	ctx := context.Background()
	primary, offB := &storage.MemBucket{}, &storage.MemBucket{}
	s.bucket, off.bucket = primary, offB
	s.set.Escrow.AgeRecipient = id.Recipient().String()

	rec, err := s.ExportNow(ctx, root)
	if err != nil || !rec.Offsite || rec.OffsiteError != "" {
		t.Fatalf("export: %+v %v", rec, err)
	}
	if len(primary.Meta) != 1 || len(offB.Meta) != 1 {
		t.Fatalf("written: primary %d, off-site %d", len(primary.Meta), len(offB.Meta))
	}
	c, err := export.Open(offB.Meta[rec.Name], id)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Manifest.Schemas) != 2 || c.Manifest.Schemas[1].Name != OffsiteSchema || len(c.Tables[OffsiteSchema]) == 0 {
		t.Fatalf("schemas: %+v", c.Manifest.Schemas)
	}

	offB.MetaErr = errors.New("writing meta/x: AccessDenied")
	rec, err = s.ExportNow(ctx, root)
	if err != nil || rec.OffsiteError == "" {
		t.Fatalf("an off-site failure: %+v %v", rec, err)
	}
	ps := s.serverProblems(ctx)
	if serverProblem(ps, "export") != nil || serverProblem(ps, "export_offsite") == nil {
		t.Fatalf("problems: %+v", ps)
	}
	st, _ := s.st.Q().ExportState(ctx)
	if st.LastGood == nil || st.LastGood.ID != rec.ID || st.LastOffsite == nil || st.LastOffsite.ID == rec.ID {
		t.Fatalf("state: %+v", st)
	}
}

// After a restore from an export, an agent that runs a newer revision than the restored head keeps it: the server
// holds the config back, does not retire the host, and shows it; the first revision that touches the host is sent
// as usual.
func TestAgentAheadOfARestoredServer(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()
	const fqdn = "web01.example.com"
	enrolled(t, s, fqdn, proto.Facts{Protocol: proto.Protocol})
	head, err := s.st.Q().Head(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.st.Q().RecordRestoreFromExport(ctx, store.RestoreFromExport{RestoredAt: time.Now(), ExportName: "2026-11-22.age",
		ExportedAt: time.Now().Add(-time.Hour), Head: head.Number}); err != nil {
		t.Fatal(err)
	}
	ahead := proto.PollRequest{Facts: proto.Facts{Protocol: proto.Protocol}, Applied: proto.Applied{Revision: head.Number + 3, SHA256: "a-config-the-export-does-not-have"}}
	resp, err := s.Poll(ctx, fqdn, ahead, 0)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Config != nil {
		t.Fatalf("the config was sent to an agent ahead of the restored server: %+v", resp.Config)
	}
	v, _ := s.Host(ctx, fqdn)
	if v.AheadOfServer == nil || *v.AheadOfServer != head.Number || !hasProblem(v, "ahead") || hasProblem(v, "drift") || v.State == "retired" {
		t.Fatalf("host: ahead %v, state %s, problems %+v", v.AheadOfServer, v.State, v.Problems)
	}

	if _, err := s.SetJob(ctx, root, fqdn, "etc", SetJobInput{Job: *job("/etc")}); err != nil {
		t.Fatal(err)
	}
	resp, err = s.Poll(ctx, fqdn, ahead, 0)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Config == nil || resp.Config.Revision <= head.Number {
		t.Fatalf("after a change to the host: %+v", resp.Config)
	}
	if v, _ := s.Host(ctx, fqdn); v.AheadOfServer != nil || hasProblem(v, "ahead") {
		t.Fatalf("after a change to the host: ahead %v, problems %+v", v.AheadOfServer, v.Problems)
	}
}

// Importing a host is an admin's, and checks the escrow file before anything else.
func TestImportHostRefuses(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()
	const fqdn = "web01.example.com"
	file := func(f EscrowFile) string {
		f.Format = EscrowFormatVersion
		b, _ := json.Marshal(f)
		return string(b)
	}
	repo := s.loc.RepoURL(fqdn)
	var ve *ValidationError
	for name, in := range map[string]ImportHostInput{
		"not an escrow file": {FQDN: fqdn, EscrowFile: "hello"},
		"another host's":     {FQDN: fqdn, EscrowFile: file(EscrowFile{FQDN: "web02.example.com", Repository: s.loc.RepoURL("web02.example.com"), RepoPassword: "pw"})},
		"another repository": {FQDN: fqdn, EscrowFile: file(EscrowFile{FQDN: fqdn, Repository: "s3:https://elsewhere/b/repos/" + fqdn, RepoPassword: "pw"})},
		"no password":        {FQDN: fqdn, EscrowFile: file(EscrowFile{FQDN: fqdn, Repository: repo})},
		"a bad FQDN":         {FQDN: "Web01", EscrowFile: file(EscrowFile{FQDN: "Web01", Repository: repo, RepoPassword: "pw"})},
	} {
		if _, err := s.ImportHost(ctx, root, in); !errors.As(err, &ve) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if _, err := s.AddHost(ctx, root, AddHostInput{FQDN: fqdn}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ImportHost(ctx, root, ImportHostInput{FQDN: fqdn, EscrowFile: file(EscrowFile{FQDN: fqdn, Repository: repo, RepoPassword: "pw"})}); !errors.Is(err, ErrConflict) {
		t.Fatalf("a host the server knows: %v", err)
	}
}
