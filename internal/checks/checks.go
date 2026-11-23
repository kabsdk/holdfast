// Package checks is the checks engine (spec §4.5, decision 021): every check on a job's schedule and
// retention, each with a stable code. The UI, the API and the CLI all get these from the server.
//
// Errors block a save. Warnings must be accepted one by one, by code. Info is shown and never blocks.
// R101, R102 and R103 are estimates from the schedule and the windows. R104, a retention cut,
// is decided by restic on the real repository: the server computes both keep sets, and RetentionCut
// compares them.
package checks

import (
	"fmt"
	"slices"
	"sort"
	"time"

	"github.com/kabsdk/holdfast/internal/config"
)

// Severities.
const (
	Error   = "error"
	Warning = "warning"
	Info    = "info"
)

// Finding is one check that fired.
type Finding struct {
	Code     string `json:"code"`
	Severity string `json:"severity"`
	Path     string `json:"path"` // such as schedule or retention.weekly
	Message  string `json:"message"`
}

// History is what the server knows about a job's recent runs, for S002.
type History struct {
	RunTimes []time.Duration // the durations of the job's last runs, at most 20
}

// approximate lengths, for comparisons and estimates only
const (
	day   = 24 * time.Hour
	week  = 7 * day
	month = 30 * day
	year  = 365 * day
)

// Length is a retention window as a duration: weeks are 7 days, months 30, years 365.
func Length(d config.Duration) time.Duration {
	switch d.Unit {
	case config.Hours:
		return time.Duration(d.N) * time.Hour
	case config.Days:
		return time.Duration(d.N) * day
	case config.Weeks:
		return time.Duration(d.N) * week
	case config.Months:
		return time.Duration(d.N) * month
	case config.Years:
		return time.Duration(d.N) * year
	}
	return 0
}

type tier struct {
	name   string
	period time.Duration // 0 for "within": it keeps every snapshot in its window
	d      *config.Duration
}

func tiers(r config.Retention) []tier {
	return []tier{{"within", 0, r.Within}, {"hourly", time.Hour, r.Hourly}, {"daily", day, r.Daily},
		{"weekly", week, r.Weekly}, {"monthly", month, r.Monthly}, {"yearly", year, r.Yearly}}
}

func human(d time.Duration) string {
	n := func(v int64, unit string) string {
		if v == 1 {
			return "1 " + unit
		}
		return fmt.Sprintf("%d %ss", v, unit)
	}
	switch {
	case d >= year && d%year == 0:
		return n(int64(d/year), "year")
	case d >= month && d%month == 0:
		return n(int64(d/month), "month")
	case d >= week && d%week == 0:
		return n(int64(d/week), "week")
	case d >= day && d%day == 0:
		return n(int64(d/day), "day")
	case d >= time.Hour:
		return n(int64(d/time.Hour), "hour")
	}
	return n(int64(d/time.Minute), "minute")
}

// Job runs every check on one job. A job whose schedule is invalid gets S003 alone among the schedule
// checks, since the others need a valid schedule.
func Job(j config.Job, h History) []Finding {
	var out []Finding
	add := func(code, sev, path, format string, a ...any) {
		out = append(out, Finding{Code: code, Severity: sev, Path: path, Message: fmt.Sprintf(format, a...)})
	}
	r := j.Retention
	present := []tier{}
	for _, t := range tiers(r) {
		if t.d == nil {
			continue
		}
		if t.d.N < 1 {
			add("R005", Error, "retention."+t.name, "the %s tier has no duration", t.name)
			continue
		}
		present = append(present, t)
	}

	if err := j.Schedule.Validate(); err != nil {
		add("S003", Error, "schedule", "%v", err)
	} else {
		shortest, longest := j.Schedule.ShortestGap(), j.Schedule.LongestGap()
		if sp := j.Schedule.SpreadDuration(); sp > 0 && shortest > 0 && 2*sp >= shortest {
			add("S001", Error, "schedule.spread", "the spread (%s) is half the interval (%s) or more, so runs could start almost back to back", human(sp), human(shortest))
		}
		for _, t := range present {
			if t.period > 0 && t.period < shortest {
				add("R001", Error, "retention."+t.name, "the %s tier is finer than the schedule: it runs every %s at most", t.name, human(shortest))
			}
		}
		if n := len(h.RunTimes); n >= 5 && shortest > 0 {
			if p := p95(h.RunTimes); p >= shortest {
				add("S002", Warning, "schedule", "runs take up to %s (p95 of the last %d), but come every %s: some will be skipped", human(p.Round(time.Minute)), n, human(shortest))
			}
		}
		// R103 looks at the gap, not the gap plus the spread, so a daily job with a spread does not warn.
		// The message still gives the worst-case loss.
		if longest > day {
			add("R103", Warning, "schedule", "runs are up to %s apart, so the worst-case data loss is about %s", human(longest), human(j.Schedule.WorstCaseLoss().Round(time.Minute)))
		}
		oldest := time.Duration(r.KeepLast) * longest
		for _, t := range present {
			oldest = max(oldest, Length(*t.d))
		}
		if oldest < month {
			add("R101", Warning, "retention", "the oldest restore point is about %s old, less than 30 days", human(oldest))
		}
		if n := estimateKept(present, r.KeepLast, shortest); n > 1000 {
			add("R102", Warning, "retention", "about %d snapshots would be kept, more than 1000", n)
		}
	}

	for _, t := range present {
		if t.period > 0 && Length(*t.d) < t.period {
			add("R002", Error, "retention."+t.name, "the %s window (%s) is shorter than its own period (%s), so it keeps at most one", t.name, human(Length(*t.d)), human(t.period))
		}
	}
	for i, coarse := range present {
		for _, fine := range present[:i] {
			if Length(*coarse.d) <= Length(*fine.d) {
				add("R003", Error, "retention."+coarse.name, "the %s tier adds nothing: the finer %s tier already covers a window as long or longer (%s)",
					coarse.name, fine.name, human(Length(*fine.d)))
				break
			}
		}
	}
	switch {
	case len(present) == 0 && r.KeepLast == 0:
		add("R004", Error, "retention", "nothing is kept")
	case r.KeepLast == 0:
		add("R006", Warning, "retention.keep_last", "keep newest is 0: a long pause in backups could leave no snapshot at all")
	}
	if r.Yearly == nil {
		add("I001", Info, "retention.yearly", "there is no yearly tier")
	}
	Sort(out)
	return out
}

// Sort orders findings errors first, then warnings, then info, keeping the order within each.
func Sort(fs []Finding) {
	sort.SliceStable(fs, func(i, k int) bool { return rank(fs[i].Severity) < rank(fs[k].Severity) })
}

func rank(sev string) int {
	switch sev {
	case Error:
		return 0
	case Warning:
		return 1
	}
	return 2
}

func p95(d []time.Duration) time.Duration {
	s := slices.Clone(d)
	slices.Sort(s)
	return s[(len(s)*95+99)/100-1]
}

// estimateKept is about how many snapshots the tiers keep together: each tier keeps one per its period, or
// per run when that is coarser, over the part of its window that the finer tiers do not cover.
func estimateKept(present []tier, keepLast int, gap time.Duration) int {
	if gap <= 0 {
		return 0
	}
	total, covered := 0.0, time.Duration(0)
	for _, t := range present {
		w := Length(*t.d)
		if w <= covered {
			continue
		}
		total += float64(w-covered) / float64(max(t.period, gap))
		covered = w
	}
	return max(int(total), keepLast)
}

// Expect is what a job gets, estimated from its schedule and windows like R101 and R102: about how many
// restore points it keeps once it has run long enough, and how old the oldest is then.
type Expect struct {
	RestorePoints int           `json:"restore_points"`
	Oldest        time.Duration `json:"-"`
	OldestText    string        `json:"oldest"`
}

// Estimate is Expect for a job. A job whose schedule is invalid gets nothing.
func Estimate(j config.Job) Expect {
	if j.Schedule.Validate() != nil {
		return Expect{}
	}
	var present []tier
	for _, t := range tiers(j.Retention) {
		if t.d != nil && t.d.N >= 1 {
			present = append(present, t)
		}
	}
	shortest, longest := j.Schedule.ShortestGap(), j.Schedule.LongestGap()
	oldest := time.Duration(j.Retention.KeepLast) * longest
	for _, t := range present {
		oldest = max(oldest, Length(*t.d))
	}
	return Expect{RestorePoints: estimateKept(present, j.Retention.KeepLast, shortest), Oldest: oldest, OldestText: human(oldest)}
}

// RetentionCut is R104 (spec §4.5, decision 030): restic decided what the old and the new policy keep, and
// the change is a cut when the new keep set is a strict subset of the old one. It returns nil otherwise.
func RetentionCut(oldKeep, newKeep []string) *Finding {
	inOld := map[string]bool{}
	for _, id := range oldKeep {
		inOld[id] = true
	}
	for _, id := range newKeep {
		if !inOld[id] {
			return nil // the new policy keeps something the old one would not: not a cut
		}
	}
	if len(newKeep) >= len(oldKeep) {
		return nil
	}
	return &Finding{Code: "R104", Severity: Warning, Path: "retention",
		Message: fmt.Sprintf("the change cuts retention: %d of today's %d kept snapshots would go, after the 7-day protection", len(oldKeep)-len(newKeep), len(oldKeep))}
}

// Shorter says where an off-site retention keeps less than primary: each window that is shorter, or missing, and
// the newest N if it is fewer. Empty when it keeps at least as much everywhere.
func Shorter(offsite, primary config.Retention) []string {
	var out []string
	pt, ot := tiers(primary), tiers(offsite)
	for i := range pt {
		if pt[i].d == nil {
			continue
		}
		p := Length(*pt[i].d)
		switch {
		case ot[i].d == nil:
			out = append(out, fmt.Sprintf("%s: none, against %s", pt[i].name, human(p)))
		case Length(*ot[i].d) < p:
			out = append(out, fmt.Sprintf("%s: %s, against %s", pt[i].name, human(Length(*ot[i].d)), human(p)))
		}
	}
	if offsite.KeepLast < primary.KeepLast {
		out = append(out, fmt.Sprintf("the newest %d, against %d", offsite.KeepLast, primary.KeepLast))
	}
	return out
}

// Offsite is O101 (decision 039): a vault's retention for a job keeps less than the job's primary retention in
// site A. The vault's admin decides site B's retention, so it is a warning to accept, not an error. nil otherwise.
func Offsite(offsite, primary config.Retention) *Finding {
	short := Shorter(offsite, primary)
	if len(short) == 0 {
		return nil
	}
	msg := "the off-site retention keeps less than primary"
	for i, s := range short {
		sep := "; "
		if i == 0 {
			sep = ": "
		}
		msg += sep + s
	}
	return &Finding{Code: "O101", Severity: Warning, Path: "retention", Message: msg}
}

// Blocking returns the errors, and the warnings not in accepted. A save goes ahead only when it is empty.
func Blocking(fs []Finding, accepted []string) []Finding {
	var out []Finding
	for _, f := range fs {
		if f.Severity == Error || (f.Severity == Warning && !slices.Contains(accepted, f.Code)) {
			out = append(out, f)
		}
	}
	return out
}

// Codes lists the findings' codes.
func Codes(fs []Finding) []string {
	out := make([]string, 0, len(fs))
	for _, f := range fs {
		out = append(out, f.Code)
	}
	return out
}

// Accepted returns the warning codes among fs that are in accepted: what a save records as accepted.
func Accepted(fs []Finding, accepted []string) []string {
	out := []string{}
	for _, f := range fs {
		if f.Severity == Warning && slices.Contains(accepted, f.Code) && !slices.Contains(out, f.Code) {
			out = append(out, f.Code)
		}
	}
	return out
}
