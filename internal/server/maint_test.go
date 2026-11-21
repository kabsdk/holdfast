package server

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/kabsdk/holdfast/internal/proto"
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
	s := newTestServerWith(t, func(d *Deps) { d.Provisioner, d.Prober, d.Now = nil, p, clock.Now })
	ctx := context.Background()
	const fqdn = "web01.example.com"
	res, err := s.AddHost(ctx, root, AddHostInput{FQDN: fqdn, JobID: "system", Job: job("/etc"), S3Key: &S3KeyInput{AccessKeyID: "HOSTKEY01", SecretAccessKey: "host-secret-01"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Enroll(ctx, proto.EnrollRequest{FQDN: fqdn, Code: res.Enrollment.Code}); err != nil {
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
}
