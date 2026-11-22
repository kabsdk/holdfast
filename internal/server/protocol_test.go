package server

import (
	"context"
	"errors"
	"testing"

	"github.com/kabsdk/holdfast/internal/proto"
)

// Plan-m5 item 1: an agent older than the server accepts is told so, still counts as seen, and shows as a
// problem; an agent before M5 sends no protocol and speaks 1.
func TestAgentProtocol(t *testing.T) {
	s, ctx := newTestServerWith(t, func(d *Deps) { d.MinProtocol = 2 }), context.Background()
	const fqdn = "web01.example.com"
	res, err := s.AddHost(ctx, root, AddHostInput{FQDN: fqdn, JobID: "system", Job: job("/etc")})
	if err != nil {
		t.Fatal(err)
	}
	var old *AgentTooOldError
	if _, err := s.Enroll(ctx, proto.EnrollRequest{FQDN: fqdn, Code: res.Enrollment.Code}); !errors.As(err, &old) || old.Agent != 1 {
		t.Fatalf("an agent before M5 enrolling with a server that needs 2: %v", err)
	}
	if _, err := s.Enroll(ctx, proto.EnrollRequest{FQDN: fqdn, Code: res.Enrollment.Code, Facts: proto.Facts{Protocol: 2}}); err != nil {
		t.Fatal(err)
	}

	// A poll at protocol 2 is answered, with the range.
	p, err := s.Poll(ctx, fqdn, proto.PollRequest{Facts: proto.Facts{Protocol: 2, AgentVersion: "new"}}, 0)
	if err != nil || p.Protocol == nil || p.Protocol.Min != 2 || p.Config == nil {
		t.Fatalf("poll at protocol 2: %+v %v", p.Protocol, err)
	}
	if v, _ := s.Host(ctx, fqdn); v.AgentTooOld || hasProblem(v, "agent_too_old") {
		t.Fatalf("a current agent is not too old: %+v", v.Problems)
	}

	// The same agent, downgraded: refused, but seen.
	if _, err := s.Poll(ctx, fqdn, proto.PollRequest{Facts: proto.Facts{AgentVersion: "old"}}, 0); !errors.As(err, &old) || old.Range.Min != 2 {
		t.Fatalf("poll at protocol 1: %v", err)
	}
	v, _ := s.Host(ctx, fqdn)
	if !v.AgentTooOld || !hasProblem(v, "agent_too_old") || v.Agent.AgentVersion != "old" || v.Agent.Protocol != 0 {
		t.Fatalf("too old: %+v %+v", v.Agent, v.Problems)
	}
}
