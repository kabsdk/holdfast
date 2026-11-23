package server

import (
	"context"
	"testing"

	"github.com/kabsdk/holdfast/internal/proto"
)

// Removing never deletes data (spec §3.5).
func TestRemoveJobAndHost(t *testing.T) {
	s, ctx := newTestServer(t), context.Background()
	q := s.st.Q()

	// A host that never enrolled is retired at once.
	if _, err := s.AddHost(ctx, root, AddHostInput{FQDN: "new01.example.com", JobID: "system", Job: job("/etc")}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RemoveHost(ctx, root, "new01.example.com", RemoveInput{}); err != nil {
		t.Fatal(err)
	}
	if h, _ := q.GetHost(ctx, "new01.example.com"); h.State != "retired" {
		t.Fatalf("a host that never enrolled: %s", h.State)
	}

	// An enrolled host: its agent gets the empty config first, and only then is the host retired.
	const fqdn = "web01.example.com"
	res, err := s.AddHost(ctx, root, AddHostInput{FQDN: fqdn, JobID: "system", Job: job("/etc")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetJob(ctx, root, fqdn, "www", SetJobInput{Job: *job("/var/www")}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RemoveJob(ctx, root, fqdn, "www", RemoveInput{}); err != nil {
		t.Fatal(err)
	}
	if h, _ := s.Host(ctx, fqdn); len(h.Jobs) != 1 {
		t.Fatalf("the job must be gone from the config: %+v", h.Jobs)
	}
	if err := s.Enroll(ctx, proto.EnrollRequest{FQDN: fqdn, Code: res.Enrollment.Code}, certOf(fqdn)); err != nil {
		t.Fatal(err)
	}
	first, err := s.Poll(ctx, fqdn, proto.PollRequest{}, 0)
	if err != nil || first.Config == nil {
		t.Fatalf("first poll: %+v %v", first, err)
	}
	pw := first.Credentials.RepoPassword
	running := proto.PollRequest{Applied: proto.Applied{Revision: first.Config.Revision, SHA256: first.Config.SHA256}, CredentialsVersion: first.Credentials.Version}
	if _, err := s.RemoveHost(ctx, root, fqdn, RemoveInput{Message: "decommissioned rack 4"}); err != nil {
		t.Fatal(err)
	}
	empty, err := s.Poll(ctx, fqdn, running, 0)
	if err != nil || empty.Config == nil || len(empty.Config.Jobs) != 0 || empty.Config.SHA256 == "" {
		t.Fatalf("after removal the agent gets an empty config: %+v %v", empty.Config, err)
	}
	if h, _ := q.GetHost(ctx, fqdn); h.State != "enrolled" || h.RemovedAt == nil {
		t.Fatalf("not retired before the agent applied it: %+v", h)
	}
	if v, _ := s.Host(ctx, fqdn); len(v.Problems) != 1 || v.Problems[0].Code != "removing" {
		t.Fatalf("a host being removed says so: %+v", v.Problems)
	}
	running.Applied = proto.Applied{Revision: empty.Config.Revision, SHA256: empty.Config.SHA256}
	if _, err := s.Poll(ctx, fqdn, running, 0); err != nil {
		t.Fatal(err)
	}
	if h, _ := q.GetHost(ctx, fqdn); h.State != "retired" {
		t.Fatalf("retired once the agent runs the empty config: %s", h.State)
	}
	if _, err := q.AgentByCertificate(ctx, certOf(fqdn)); err == nil {
		t.Fatal("a retired host's agent certificate must stop working")
	}

	// Adding it again brings it back, with the same repository password.
	again, err := s.AddHost(ctx, root, AddHostInput{FQDN: fqdn, JobID: "system", Job: job("/etc")})
	if err != nil {
		t.Fatal(err)
	}
	if h, _ := q.GetHost(ctx, fqdn); h.State != "awaiting_enrollment" || h.RemovedAt != nil {
		t.Fatalf("back after removal: %+v", h)
	}
	if err := s.Enroll(ctx, proto.EnrollRequest{FQDN: fqdn, Code: again.Enrollment.Code}, certOf(fqdn)); err != nil {
		t.Fatal(err)
	}
	if p, _ := s.Poll(ctx, fqdn, proto.PollRequest{}, 0); p.Credentials == nil || p.Credentials.RepoPassword != pw {
		t.Fatal("the repository password must stay, or the old snapshots could not be read")
	}
}
