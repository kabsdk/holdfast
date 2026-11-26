package server

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/kabsdk/holdfast/internal/config"
	"github.com/kabsdk/holdfast/internal/schedule"
	"github.com/kabsdk/holdfast/internal/server/store"
)

// The agent page's last backups (spec §5.1a): for each job, the last slots its schedule called for, and how each
// went. A slot is served by the scheduled backup the agent ran for it, retries included; failing that, by a backup run
// outside the schedule while it was the newest slot, such as the job's first backup or one run by hand. One with no
// backup is missed, so a weekly job's 7 slots cover 7 weeks and a missed one shows. The server decides what each
// slot's state is, so the UI and the CLI only draw what it says.

// Slot states. A backup's own statuses, success, warning and failed, say how it went; the rest say why there is none.
const (
	SlotMissed  = "missed"  // the slot passed, and no backup ran for it
	SlotSkipped = "skipped" // the slot came while the job's previous run was still going
	SlotDue     = "due"     // the newest slot, whose backup has not been reported yet: it may still run
	SlotBefore  = "before"  // the slot came before the job's first backup: it had nothing to miss
)

// Slot is one nominal run time of a job's schedule, and the backup that ran for it.
type Slot struct {
	At    time.Time `json:"at"`    // the nominal time, before any spread
	State string    `json:"state"` // success, warning, failed, missed, skipped, due or before
	// The backup that ran for the slot, the last when it was retried: when it ended, how many runs it took, and its
	// snapshot.
	Finished   *time.Time `json:"finished,omitempty"`
	Runs       int        `json:"runs,omitempty"`
	SnapshotID string     `json:"snapshot_id,omitempty"`
	Error      string     `json:"error,omitempty"`
	// Unscheduled says the backup ran outside the schedule, while this was the newest slot: the job's first backup,
	// or one run by hand, in place of the slot's own.
	Unscheduled bool `json:"unscheduled,omitempty"`
}

// JobSlots is one job's last slots, oldest first.
type JobSlots struct {
	JobID string `json:"job_id"`
	Slots []Slot `json:"slots"`
}

// HostSlots is the answer of GET /api/v1/agents/{agent}/slots.
type HostSlots struct {
	FQDN string     `json:"fqdn"`
	Jobs []JobSlots `json:"jobs"`
}

// MaxSlots is the most slots the API gives per job.
const MaxSlots = 30

// slotLookback is how far back the slots are looked for: a weekly job's 30 slots, and a week more.
const slotLookback = (MaxSlots + 7) * 7 * 24 * time.Hour

// HostSlots returns the last n slots of each of a host's jobs, by each job's schedule as it is now.
func (s *Server) HostSlots(ctx context.Context, fqdn string, n int) (HostSlots, error) {
	if n == 0 {
		n = 7
	}
	if n < 1 || n > MaxSlots {
		return HostSlots{}, &ValidationError{config.Problems{{Path: "count", Message: fmt.Sprintf("count is from 1 to %d", MaxSlots)}}}
	}
	h, err := s.Host(ctx, fqdn)
	if err != nil {
		return HostSlots{}, err
	}
	now := s.now()
	q := s.st.Q()
	runs, err := q.SlotRuns(ctx, fqdn, now.Add(-slotLookback))
	if err != nil {
		return HostSlots{}, err
	}
	first, err := q.FirstBackups(ctx, fqdn)
	if err != nil {
		return HostSlots{}, err
	}
	head, err := s.head(ctx, q)
	if err != nil {
		return HostSlots{}, err
	}
	hc := head.Config.Hosts[h.FQDN]
	out := HostSlots{FQDN: h.FQDN, Jobs: make([]JobSlots, 0, len(h.Jobs))}
	for _, jv := range h.Jobs {
		j, ok := hc.Jobs[jv.ID]
		if !ok {
			continue // an off-site job that primary removed: it has no schedule here
		}
		loc, _ := j.Schedule.Location(s.zone)
		f, ok := first[jv.ID]
		out.Jobs = append(out.Jobs, JobSlots{JobID: jv.ID, Slots: jobSlots(j.Schedule, loc, runs[jv.ID], f, ok, now, n)})
	}
	return out, nil
}

// jobSlots puts each run on the slot it was for, and says what became of each of the last n slots up to now.
func jobSlots(sch schedule.Schedule, loc *time.Location, runs []store.SlotRun, first time.Time, hasFirst bool, now time.Time, n int) []Slot {
	all := sch.Slots(now.Add(-slotLookback), now, loc)
	if len(all) > n {
		all = all[len(all)-n:]
	}
	by := map[int64][]store.SlotRun{}
	var unscheduled []store.SlotRun
	for _, r := range runs {
		if r.ScheduledFor.IsZero() {
			unscheduled = append(unscheduled, r)
			continue
		}
		by[r.ScheduledFor.Unix()] = append(by[r.ScheduledFor.Unix()], r)
	}
	out := make([]Slot, 0, len(all))
	for i, at := range all {
		sl := Slot{At: at}
		rs := by[at.Unix()]
		var last *store.SlotRun
		for k := range rs {
			if rs[k].Status != "skipped" {
				sl.Runs++
				last = &rs[k]
			}
		}
		if last == nil {
			// None of its own: a backup run outside the schedule while this was the newest slot stands in for it.
			end := now
			if i+1 < len(all) {
				end = all[i+1]
			}
			for k := range unscheduled {
				if u := &unscheduled[k]; !u.Started.Before(at) && u.Started.Before(end) && u.Status != "skipped" {
					sl.Runs++
					last, sl.Unscheduled = u, true
				}
			}
		}
		switch {
		case last != nil:
			fin := last.Finished
			sl.Finished, sl.SnapshotID, sl.Error = &fin, last.SnapshotID, last.Error
			sl.State = last.Status
			if sl.State == "interrupted" {
				sl.State = "failed"
			}
		case len(rs) > 0:
			sl.State = SlotSkipped
		case !hasFirst || at.Before(first):
			sl.State = SlotBefore
		case i == len(all)-1:
			sl.State = SlotDue // the newest slot: its backup may still be waiting for its spread, or running
		default:
			sl.State = SlotMissed
		}
		out = append(out, sl)
	}
	return out
}

func (s *Server) apiHostSlots(w http.ResponseWriter, r *http.Request) {
	n := 0
	if v := r.URL.Query().Get("count"); v != "" {
		k, err := strconv.Atoi(v)
		if err != nil {
			apiError(w, &ValidationError{config.Problems{{Path: "count", Message: "count is a number"}}})
			return
		}
		n = k
	}
	hs, err := s.HostSlots(r.Context(), r.PathValue("fqdn"), n)
	if err != nil {
		apiError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, hs)
}
