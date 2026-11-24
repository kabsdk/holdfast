package server

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/kabsdk/holdfast/internal/config"
	"github.com/kabsdk/holdfast/internal/server/store"
)

// The host page's strip of days (spec §5.1a): for each job, each of the last days, how many backups ran and the
// worst of them. The server decides what "worst" is and fills in the days without a run, so the UI and the CLI only
// draw what it says.

// DayRuns is one job's backups on one day.
type DayRuns struct {
	Date    string `json:"date"` // YYYY-MM-DD, in the zone asked for
	Runs    int    `json:"runs"`
	Success int    `json:"success"`
	Warning int    `json:"warning"` // incomplete: some files could not be read
	Failed  int    `json:"failed"`  // failed or interrupted
	// Worst is the worst run of the day: failed, warning or success; empty on a day without a run.
	Worst string `json:"worst"`
	// Before says the day ended before the job's first backup: it had no run to miss.
	Before bool `json:"before,omitempty"`
}

// JobDays is one job's days, oldest first.
type JobDays struct {
	JobID string    `json:"job_id"`
	Days  []DayRuns `json:"days"`
}

// HostDays is the answer of GET /api/v1/agents/{fqdn}/days.
type HostDays struct {
	FQDN string    `json:"fqdn"`
	Zone string    `json:"zone"`
	From string    `json:"from"`
	To   string    `json:"to"`
	Jobs []JobDays `json:"jobs"`
}

// MaxDays is the longest strip of days the API gives.
const MaxDays = 92

// HostDays returns the last days of each of a host's jobs, today included, in a time zone (the server's when empty).
func (s *Server) HostDays(ctx context.Context, fqdn string, days int, zone string) (HostDays, error) {
	if days == 0 {
		days = 14
	}
	if days < 1 || days > MaxDays {
		return HostDays{}, &ValidationError{config.Problems{{Path: "days", Message: fmt.Sprintf("days is from 1 to %d", MaxDays)}}}
	}
	loc := time.Local
	if zone != "" {
		l, err := time.LoadLocation(zone)
		if err != nil {
			return HostDays{}, &ValidationError{config.Problems{{Path: "tz", Message: "not a time zone, such as Europe/Copenhagen: " + zone}}}
		}
		loc = l
	}
	h, err := s.Host(ctx, fqdn)
	if err != nil {
		return HostDays{}, err
	}
	now := s.now().In(loc)
	from := time.Date(now.Year(), now.Month(), now.Day()-(days-1), 0, 0, 0, 0, loc)
	q := s.st.Q()
	outs, err := q.BackupOutcomes(ctx, fqdn, from)
	if err != nil {
		return HostDays{}, err
	}
	first, err := q.FirstBackups(ctx, fqdn)
	if err != nil {
		return HostDays{}, err
	}
	ids := make([]string, 0, len(h.Jobs))
	for _, j := range h.Jobs {
		ids = append(ids, j.ID)
	}
	jobs := dayStrips(ids, outs, first, from, days)
	return HostDays{FQDN: fqdn, Zone: loc.String(), From: jobs.from, To: jobs.to, Jobs: jobs.jobs}, nil
}

type strips struct {
	from, to string
	jobs     []JobDays
}

// dayStrips puts each backup on the day it finished, in from's zone, for each job named; a run of another job is
// left out. A skipped run is not a run. A day that ended before a job's first backup, or every day of a job that has
// none, is marked Before.
func dayStrips(jobIDs []string, outs []store.Outcome, first map[string]time.Time, from time.Time, days int) strips {
	loc := from.Location()
	dates := make([]string, days)
	index := map[string]int{}
	for i := range days {
		d := time.Date(from.Year(), from.Month(), from.Day()+i, 0, 0, 0, 0, loc).Format(time.DateOnly)
		dates[i] = d
		index[d] = i
	}
	jobs := make([]JobDays, len(jobIDs))
	byJob := map[string]int{}
	for i, id := range jobIDs {
		jobs[i] = JobDays{JobID: id, Days: make([]DayRuns, days)}
		for k, d := range dates {
			jobs[i].Days[k].Date = d
		}
		byJob[id] = i
	}
	for _, o := range outs {
		j, ok := byJob[o.JobID]
		if !ok {
			continue
		}
		k, ok := index[o.Finished.In(loc).Format(time.DateOnly)]
		if !ok {
			continue
		}
		d := &jobs[j].Days[k]
		switch o.Status {
		case "success":
			d.Success++
		case "warning":
			d.Warning++
		case "failed", "interrupted":
			d.Failed++
		default:
			continue
		}
		d.Runs++
	}
	for i := range jobs {
		f, ok := first[jobs[i].JobID]
		firstDay := ""
		if ok {
			firstDay = f.In(loc).Format(time.DateOnly)
		}
		for k := range jobs[i].Days {
			d := &jobs[i].Days[k]
			d.Before = d.Runs == 0 && (!ok || d.Date < firstDay)
			switch {
			case d.Failed > 0:
				d.Worst = "failed"
			case d.Warning > 0:
				d.Worst = "warning"
			case d.Success > 0:
				d.Worst = "success"
			}
		}
	}
	return strips{from: dates[0], to: dates[days-1], jobs: jobs}
}

func (s *Server) apiHostDays(w http.ResponseWriter, r *http.Request) {
	days := 0
	if v := r.URL.Query().Get("days"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			apiError(w, &ValidationError{config.Problems{{Path: "days", Message: "days is a number"}}})
			return
		}
		days = n
	}
	hd, err := s.HostDays(r.Context(), r.PathValue("fqdn"), days, r.URL.Query().Get("tz"))
	if err != nil {
		apiError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, hd)
}
