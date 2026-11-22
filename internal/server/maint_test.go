package server

import (
	"context"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kabsdk/holdfast/internal/proto"
	"github.com/kabsdk/holdfast/internal/restic"
	"github.com/kabsdk/holdfast/internal/server/storage"
)

type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) Now() time.Time  { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *testClock) Set(t time.Time) { c.mu.Lock(); defer c.mu.Unlock(); c.t = t }
func (c *testClock) Add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// TestChecksAndTheDailyStorageTest (decision 030):
//   - `holdfast check` tests the storage contract at once, and queues a repository check on the agent;
//   - the daily check config reaches the agent, and a settings change is not reported as drift;
//   - the server tests each host's storage once a day, not more often.
func TestChecksAndTheDailyStorageTest(t *testing.T) {
	clock := &testClock{t: time.Date(2026, 11, 23, 8, 0, 0, 0, time.UTC)}
	p := &testProber{}
	s := newTestServerWith(t, func(d *Deps) { d.Prober, d.Now = p, clock.Now })
	ctx := context.Background()
	const fqdn = "web01.example.com"
	res, err := s.AddHost(ctx, root, AddHostInput{FQDN: fqdn, JobID: "system", Job: job("/etc")})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Enroll(ctx, proto.EnrollRequest{FQDN: fqdn, Code: res.Enrollment.Code}, certOf(fqdn)); err != nil {
		t.Fatal(err)
	}
	poll, err := s.Poll(ctx, fqdn, proto.PollRequest{}, 0)
	if err != nil || poll.Config == nil || poll.Config.Check == nil || poll.Config.Check.CycleDays != 30 {
		t.Fatalf("the agent must get its daily check: %+v %v", poll.Config, err)
	}
	applied := proto.Applied{Revision: poll.Config.Revision, SHA256: poll.Config.SHA256}
	if _, err := s.Poll(ctx, fqdn, proto.PollRequest{Applied: applied, CredentialsVersion: 1}, 0); err != nil {
		t.Fatal(err)
	}
	if h, _ := s.Host(ctx, fqdn); !h.InSync {
		t.Fatal("not in sync after applying the config")
	}
	// Switching the daily check off changes what the agent should run, without a revision: the host is
	// out of sync until the agent applies it, and the poll sends it.
	s.set.Maintenance.Check = CheckOff
	if h, _ := s.Host(ctx, fqdn); h.InSync {
		t.Fatal("still in sync after the check settings changed")
	}
	poll, _ = s.Poll(ctx, fqdn, proto.PollRequest{Applied: applied, CredentialsVersion: 1}, 0)
	if poll.Config == nil || poll.Config.Check != nil {
		t.Fatalf("the agent must get the config without the daily check: %+v", poll.Config)
	}
	s.set.Maintenance.Check = CheckDaily

	// holdfast check: the storage at once, and a check_now command for the agent.
	before := p.called()
	out := s.RunChecks(ctx, root)
	if len(out) != 1 || out[0].Status != "requested" || out[0].Command == "" || p.called() != before+1 {
		t.Fatalf("holdfast check: %+v (storage tested %d times)", out, p.called()-before)
	}
	poll, _ = s.Poll(ctx, fqdn, proto.PollRequest{Applied: applied, CredentialsVersion: 1}, 0)
	if len(poll.Commands) != 1 || poll.Commands[0].Kind != proto.CommandCheckNow || poll.Commands[0].ID != out[0].Command {
		t.Fatalf("the agent must get the check command: %+v", poll.Commands)
	}
	p.fail(storage.CodeHostDelete)
	if out := s.RunChecks(ctx, root); out[0].Status != "failed" || !slices.Equal(out[0].Storage, []string{"B005"}) || out[0].Command == "" {
		t.Fatalf("holdfast check while B005 fails: %+v", out)
	}
	p.fail()

	// The daily storage test: not again within the day, then once.
	before = p.called()
	clock.Add(12 * time.Hour)
	s.checkStorageDue(ctx)
	if p.called() != before {
		t.Fatal("the storage was tested again within a day")
	}
	clock.Add(12 * time.Hour)
	s.checkStorageDue(ctx)
	s.checkStorageDue(ctx)
	if p.called() != before+1 {
		t.Fatalf("after a day the storage must be tested once, got %d", p.called()-before)
	}
	if h, _ := s.Host(ctx, fqdn); h.Storage == nil || !h.Storage.CheckedAt.Equal(clock.Now()) {
		t.Fatalf("the host view must show the latest storage test: %+v", h.Storage)
	}

	// Another restic than the pinned one is warned about, in the host view and in the metrics (decision 032).
	for _, tc := range []struct {
		version, metric string
		warned          bool
	}{{"0.18.0", `holdfast_agent_restic_pinned{host="web01.example.com"} 0`, true}, {restic.PinnedVersion, `holdfast_agent_restic_pinned{host="web01.example.com"} 1`, false}} {
		if _, err := s.Poll(ctx, fqdn, proto.PollRequest{Applied: applied, CredentialsVersion: 1, Facts: proto.Facts{ResticVersion: tc.version}}, 0); err != nil {
			t.Fatal(err)
		}
		h, _ := s.Host(ctx, fqdn)
		if (h.ResticWarning != "") != tc.warned {
			t.Errorf("restic %s: warning %q", tc.version, h.ResticWarning)
		}
		rec := httptest.NewRecorder()
		s.AgentHandler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
		if !strings.Contains(rec.Body.String(), tc.metric) {
			t.Errorf("restic %s: metrics lack %s", tc.version, tc.metric)
		}
	}
}
