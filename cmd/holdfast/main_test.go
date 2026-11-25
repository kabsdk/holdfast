package main

import (
	"testing"
	"time"
)

// The status column of holdfast agent list and agent show: a retired host is not "agent offline", as its agent
// was stopped on purpose (spec §3.5).
func TestHostStatus(t *testing.T) {
	due := time.Date(2026, 12, 2, 12, 0, 0, 0, time.Local)
	done := due.Add(time.Hour)
	retired := func(d *time.Time, doneAt *time.Time) hostView {
		h := hostView{State: "retired"}
		if d != nil {
			h.Decommission = &struct {
				RequestedBy string     `json:"requested_by"`
				DueAt       time.Time  `json:"due_at"`
				DoneAt      *time.Time `json:"done_at"`
				Objects     int        `json:"objects_deleted"`
				GoneAt      *time.Time `json:"really_gone_at"`
				Error       string     `json:"error"`
			}{DueAt: *d, DoneAt: doneAt}
		}
		return h
	}
	for _, c := range []struct {
		name string
		h    hostView
		want string
	}{
		{"waiting to enroll", hostView{State: "awaiting_enrollment"}, "waiting for agent"},
		{"enrolled, offline", hostView{State: "enrolled"}, "agent offline"},
		{"enrolled, online", hostView{State: "enrolled", Online: true, InSync: true}, "ok"},
		{"retired", retired(nil, nil), "retired"},
		{"retired, decommission waiting", retired(&due, nil), "retired, deleted on 2026-12-02"},
		{"decommissioned", retired(&due, &done), "decommissioned"},
	} {
		if got := hostStatus(c.h); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}
