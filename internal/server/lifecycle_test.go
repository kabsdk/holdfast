package server

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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
	metrics := func() string {
		rr := httptest.NewRecorder()
		s.AgentHandler().ServeHTTP(rr, httptest.NewRequest("GET", "/metrics", nil))
		return rr.Body.String()
	}
	now := time.Now().UTC()
	if _, err := s.Events(ctx, fqdn, []proto.Event{{ID: "check-1", Kind: proto.KindCheck, Status: proto.StatusSuccess, Started: now, Finished: now}}); err != nil {
		t.Fatal(err)
	}
	agentSeries := []string{
		`holdfast_agent_last_seen_timestamp_seconds{host="web01.example.com"}`,
		`holdfast_agent_config_in_sync{host="web01.example.com"}`,
		`holdfast_maintenance_last_success_timestamp_seconds{host="web01.example.com",kind="check"}`,
	}
	for _, want := range agentSeries {
		if !strings.Contains(metrics(), want) {
			t.Fatalf("an enrolled host's metrics lack %s", want)
		}
	}
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
	// Its agent was stopped on purpose: the metrics say nothing about it, so HoldfastAgentOffline and
	// HoldfastMaintenanceStale do not fire for it. The host itself is still there, retired.
	m := metrics()
	for _, gone := range agentSeries {
		if strings.Contains(m, gone) {
			t.Errorf("a retired host's metrics still have %s", gone)
		}
	}
	if !strings.Contains(m, `holdfast_host_state{host="web01.example.com",state="retired"} 1`) {
		t.Error("a retired host's state must stay in the metrics")
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
