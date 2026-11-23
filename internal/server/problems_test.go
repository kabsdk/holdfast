package server

import (
	"testing"
	"time"

	"github.com/kabsdk/holdfast/internal/server/store"
)

// The storage witness's problem (plan-m10 item 1): raised for a job whose reported snapshot the bucket did not hold,
// and only for that job.
func TestProblemNotInBucket(t *testing.T) {
	now := time.Now()
	no, yes := false, true
	status := &store.JobStatus{LastStatus: "success", LastSuccessAt: &now, LastSnapshotID: "0123456789abcdef"}
	v := HostView{Host: store.Host{FQDN: "web01.example.com", State: "enrolled"}, Jobs: []JobView{
		{ID: "home", Status: status, InBucket: &no},
		{ID: "etc", Status: status, InBucket: &yes},
		{ID: "var", Status: status},
	}}
	var got []Problem
	for _, p := range problems(v, now, false, true) {
		if p.Code == "not_in_bucket" {
			got = append(got, p)
		}
	}
	if len(got) != 1 || got[0].JobID != "home" || got[0].Severity != "error" {
		t.Fatalf("not_in_bucket: %+v", got)
	}
}
