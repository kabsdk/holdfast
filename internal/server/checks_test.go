package server

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/kabsdk/holdfast/internal/checks"
	"github.com/kabsdk/holdfast/internal/config"
	"github.com/kabsdk/holdfast/internal/schedule"
	"github.com/kabsdk/holdfast/internal/server/store"
)

func TestChecksOnSave(t *testing.T) {
	s, ctx := newTestServer(t), context.Background()
	const fqdn = "web01.example.com"

	// A job whose oldest restore point is 14 days old: R101 is a warning, so it must be accepted.
	short := job("/etc")
	short.Retention.Monthly = nil
	_, err := s.AddHost(ctx, root, AddHostInput{FQDN: fqdn, JobID: "system", Job: short})
	var ce *ChecksError
	if !errors.As(err, &ce) || !slices.Equal(checks.Codes(ce.Blocking), []string{"R101"}) {
		t.Fatalf("an unaccepted warning must block the save: %v", err)
	}
	if _, err := s.st.Q().GetHost(ctx, fqdn); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("a refused host was stored")
	}
	res, err := s.AddHost(ctx, root, AddHostInput{FQDN: fqdn, JobID: "system", Job: short, Accept: []string{"R101", "R102"}})
	if err != nil || !slices.Equal(res.Accepted, []string{"R101"}) {
		t.Fatalf("accepted: %+v %v", res, err)
	}
	rev, _ := s.st.Q().GetRevision(ctx, res.Revision)
	if !slices.Equal(rev.Accepted, []string{"R101"}) {
		t.Fatalf("the revision must record the accepted codes: %v", rev.Accepted)
	}
	audit, _ := s.st.Q().ListAudit(ctx, fqdn, 10)
	if !auditHas(audit, "host.add", "R101") {
		t.Fatalf("the audit log must record the accepted codes: %+v", audit)
	}

	// An error cannot be accepted.
	hourly := job("/srv")
	hourly.Retention.Hourly = &config.Duration{N: 48, Unit: config.Hours}
	if _, err := s.SetJob(ctx, root, fqdn, "srv", SetJobInput{Job: *hourly, Accept: []string{"R001"}}); !errors.As(err, &ce) || ce.Blocking[0].Code != "R001" {
		t.Fatalf("an error must block the save, accepted or not: %v", err)
	}

	// Checking saves nothing.
	head, _ := s.st.Q().Head(ctx)
	got, err := s.CheckJob(ctx, fqdn, "srv", JobCheckInput{Job: *hourly})
	if err != nil || len(got.Blocking) != 1 || got.Blocking[0].Code != "R001" {
		t.Fatalf("check: %+v %v", got, err)
	}
	if after, _ := s.st.Q().Head(ctx); after.Number != head.Number {
		t.Fatal("a check saved a revision")
	}

	// S002 comes from the job's last runs: five backups of 20 minutes on a 15-minute schedule.
	every := job("/var")
	every.Schedule = schedule.Schedule{Every: &schedule.Every{N: 15, Unit: "minutes", From: "00:00"}}
	if _, err := s.SetJob(ctx, root, fqdn, "system", SetJobInput{Job: *every}); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	for i := range 5 {
		start := now.Add(-time.Duration(i+1) * time.Hour)
		if _, err := s.st.Q().InsertEvent(ctx, store.Event{FQDN: fqdn, ID: fmt.Sprintf("ev%d", i), Kind: "backup", JobID: "system", Status: "success",
			Started: start, Finished: start.Add(20 * time.Minute)}); err != nil {
			t.Fatal(err)
		}
	}
	got, err = s.CheckJob(ctx, fqdn, "system", JobCheckInput{Job: *every})
	if err != nil || !slices.Contains(checks.Codes(got.Blocking), "S002") {
		t.Fatalf("S002 from the run history: %+v %v", got, err)
	}
	if _, err := s.SetJob(ctx, root, fqdn, "system", SetJobInput{Job: *every, Accept: []string{"S002"}}); err != nil {
		t.Fatal(err)
	}
}

func auditHas(list []store.AuditEntry, action, code string) bool {
	for _, a := range list {
		if a.Action != action {
			continue
		}
		if codes, ok := a.Detail["accepted_codes"].([]any); ok && slices.Contains(codes, any(code)) {
			return true
		}
	}
	return false
}
