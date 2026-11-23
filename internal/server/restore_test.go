package server

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kabsdk/holdfast/internal/proto"
	"github.com/kabsdk/holdfast/internal/server/store"
)

// TestRestoreRequests: a restore is checked, and the host's own bounds as its agent
// reported them are honoured. It is queued for the agent and audited, and it is cancelled and reported
// when the host does not pick it up within 24 hours.
func TestRestoreRequests(t *testing.T) {
	clock := &testClock{t: time.Date(2026, 11, 23, 8, 0, 0, 0, time.UTC)}
	s := newTestServerWith(t, func(d *Deps) { d.Now = clock.Now })
	ctx := context.Background()
	const fqdn = "web01.example.com"
	res, err := s.AddHost(ctx, root, AddHostInput{FQDN: fqdn, JobID: "system", Job: job("/etc")})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Enroll(ctx, proto.EnrollRequest{FQDN: fqdn, Code: res.Enrollment.Code}, certOf(fqdn)); err != nil {
		t.Fatal(err)
	}
	const id = "851e6c7f6646ff2b9fbc2e56485eb34ce841d3f41c5b09981b8a46c0c0165d56"
	if _, _, err := s.st.Q().SyncSnapshots(ctx, fqdn, []store.Snapshot{{FQDN: fqdn, ID: id, ShortID: id[:8], Time: clock.Now(), Hostname: fqdn,
		JobID: "system", Paths: []string{"/etc"}}}, clock.Now()); err != nil {
		t.Fatal(err)
	}
	// The agent reports its bounds: only new directories.
	if _, err := s.Poll(ctx, fqdn, proto.PollRequest{Facts: proto.Facts{RestoreBounds: &proto.RestoreBounds{Root: "/var/restore"}}}, 0); err != nil {
		t.Fatal(err)
	}

	var ve *ValidationError
	for name, in := range map[string]RestoreInput{
		"an unknown target":                {Target: "elsewhere"},
		"a relative directory":             {Target: proto.TargetDirectory, Directory: "tmp/x"},
		"both left out and only":           {Exclude: []string{"/etc/a"}, Include: []string{"/etc/b"}},
		"a relative path left out":         {Exclude: []string{"etc/a"}},
		"in place, which the host refuses": {Target: proto.TargetInPlace},
		"another directory, refused too":   {Target: proto.TargetDirectory, Directory: "/srv/restore"},
	} {
		if _, err := s.Restore(ctx, root, fqdn, id[:8], in); !errors.As(err, &ve) {
			t.Errorf("%s: %v, want a validation error", name, err)
		}
	}
	if _, err := s.Restore(ctx, root, fqdn, "zzzz", RestoreInput{}); !errors.As(err, &ve) {
		t.Errorf("a malformed snapshot ID: %v", err)
	}

	r, err := s.Restore(ctx, root, fqdn, id[:8], RestoreInput{Exclude: []string{"/etc/shadow"}})
	if err != nil || r.Target != proto.TargetNew || !r.ExpiresAt.Equal(clock.Now().Add(RestoreExpiry)) {
		t.Fatalf("restore: %+v %v", r, err)
	}
	poll, err := s.Poll(ctx, fqdn, proto.PollRequest{CredentialsVersion: 1}, 0)
	if err != nil || len(poll.Commands) != 1 || poll.Commands[0].Restore == nil || poll.Commands[0].Restore.Snapshot != id ||
		poll.Commands[0].Restore.User != "root" || poll.Commands[0].Restore.Exclude[0] != "/etc/shadow" {
		t.Fatalf("the agent must get the restore: %+v %v", poll.Commands, err)
	}
	if v, _ := s.Restores(ctx, 10); len(v.Queued) != 1 {
		t.Fatalf("queued restores: %+v", v.Queued)
	}

	// The host never picks it up: after 24 hours it is cancelled, reported and audited.
	clock.Add(RestoreExpiry + time.Minute)
	s.expireCommands(ctx)
	if poll, _ := s.Poll(ctx, fqdn, proto.PollRequest{CredentialsVersion: 1}, 0); len(poll.Commands) != 0 {
		t.Fatalf("an expired restore was still sent: %+v", poll.Commands)
	}
	v, _ := s.Restores(ctx, 10)
	if len(v.Queued) != 0 || len(v.Done) != 1 || v.Done[0].Status != proto.StatusFailed || v.Done[0].Detail["snapshot"] != id {
		t.Fatalf("after expiry: %+v", v)
	}
	audit, _ := s.st.Q().ListAudit(ctx, fqdn, 100)
	seen := map[string]bool{}
	for _, a := range audit {
		seen[a.Action] = true
	}
	if !seen["restore.requested"] || !seen["restore.expired"] {
		t.Fatalf("audit: %v", seen)
	}
}
