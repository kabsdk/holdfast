// Package schedule evaluates Holdfast's structured schedules (spec §4.1).
//
// The agent uses it to decide when to run. The server uses it for the values it shows and exports:
// the next runs, the longest gap, the worst-case data loss and the stale threshold. Both must agree, so
// this is the only implementation.
//
// Wall-clock rules (plan-m1 Q6): a time that does not exist on a given day (spring forward) runs at
// the next valid minute; a time that occurs twice (fall back) runs once, at its first occurrence.
package schedule

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Schedule is one of three forms, plus an optional random start spread.
type Schedule struct {
	Every  *Every  `json:"every,omitempty"`
	Daily  *Daily  `json:"daily,omitempty"`
	Weekly *Weekly `json:"weekly,omitempty"`
	Spread Spread  `json:"spread"`
	// Zone is an IANA time zone name. Empty means the fleet default, which the caller resolves.
	Zone string `json:"zone,omitempty"`
}

// Every runs every N minutes or hours, starting again from From each day.
type Every struct {
	N    int       `json:"n"`
	Unit string    `json:"unit"` // "minutes" or "hours"
	From ClockTime `json:"from"`
}

// Daily runs at one or more times each day.
type Daily struct {
	Times []ClockTime `json:"times"`
}

// Weekly runs at one time on the chosen days.
type Weekly struct {
	Days []string  `json:"days"` // "mon" … "sun"
	At   ClockTime `json:"at"`
}

// Spread delays each run by a random number of minutes in [0, Minutes). The number is kept while
// the spread is switched off.
type Spread struct {
	Enabled bool `json:"enabled"`
	Minutes int  `json:"minutes"`
}

// ClockTime is a wall-clock time of day, "HH:MM", from 00:00 to 23:59.
type ClockTime string

// Minutes returns the time of day as minutes after midnight.
func (c ClockTime) Minutes() (int, error) {
	h, m, ok := strings.Cut(string(c), ":")
	if !ok || len(h) != 2 || len(m) != 2 {
		return 0, fmt.Errorf("time %q is not HH:MM", string(c))
	}
	hh, err1 := strconv.Atoi(h)
	mm, err2 := strconv.Atoi(m)
	if err1 != nil || err2 != nil || hh < 0 || hh > 23 || mm < 0 || mm > 59 {
		return 0, fmt.Errorf("time %q is not a valid time of day", string(c))
	}
	return hh*60 + mm, nil
}

var weekdays = map[string]time.Weekday{
	"mon": time.Monday, "tue": time.Tuesday, "wed": time.Wednesday, "thu": time.Thursday,
	"fri": time.Friday, "sat": time.Saturday, "sun": time.Sunday,
}

// ErrInvalid wraps every structural problem Validate reports.
var ErrInvalid = errors.New("invalid schedule")

func invalid(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, a...))
}

// Validate checks the schedule's structure. The checks engine (M3) reports a failure as S003.
func (s Schedule) Validate() error {
	forms := 0
	for _, set := range []bool{s.Every != nil, s.Daily != nil, s.Weekly != nil} {
		if set {
			forms++
		}
	}
	if forms != 1 {
		return invalid("exactly one of every, daily or weekly must be set (got %d)", forms)
	}
	switch {
	case s.Every != nil:
		e := s.Every
		switch e.Unit {
		case "minutes":
			if e.N < 1 || e.N > 1440 {
				return invalid("every %d minutes: the number must be between 1 and 1440", e.N)
			}
		case "hours":
			if e.N < 1 || e.N > 24 {
				return invalid("every %d hours: the number must be between 1 and 24", e.N)
			}
		default:
			return invalid("every: unit must be minutes or hours, not %q", e.Unit)
		}
		if _, err := e.From.Minutes(); err != nil {
			return invalid("every: start time: %v", err)
		}
	case s.Daily != nil:
		if len(s.Daily.Times) == 0 {
			return invalid("daily: at least one time is needed")
		}
		seen := map[int]bool{}
		for _, t := range s.Daily.Times {
			m, err := t.Minutes()
			if err != nil {
				return invalid("daily: %v", err)
			}
			if seen[m] {
				return invalid("daily: time %s is listed twice", t)
			}
			seen[m] = true
		}
	case s.Weekly != nil:
		if len(s.Weekly.Days) == 0 {
			return invalid("weekly: at least one day is needed")
		}
		seen := map[string]bool{}
		for _, d := range s.Weekly.Days {
			if _, ok := weekdays[d]; !ok {
				return invalid("weekly: day %q is not one of mon, tue, wed, thu, fri, sat, sun", d)
			}
			if seen[d] {
				return invalid("weekly: day %s is listed twice", d)
			}
			seen[d] = true
		}
		if _, err := s.Weekly.At.Minutes(); err != nil {
			return invalid("weekly: %v", err)
		}
	}
	if s.Spread.Minutes < 0 {
		return invalid("spread: minutes cannot be negative")
	}
	if s.Zone != "" {
		if _, err := time.LoadLocation(s.Zone); err != nil {
			return invalid("zone %q: %v", s.Zone, err)
		}
	}
	return nil
}

// Location resolves the schedule's zone, using fallback when the schedule has none.
func (s Schedule) Location(fallback *time.Location) (*time.Location, error) {
	if s.Zone == "" {
		if fallback == nil {
			return time.UTC, nil
		}
		return fallback, nil
	}
	return time.LoadLocation(s.Zone)
}

// dayTimes returns the run times on a day of the given weekday, as sorted minutes after midnight.
// The schedule must be valid.
func (s Schedule) dayTimes(wd time.Weekday) []int {
	var out []int
	switch {
	case s.Every != nil:
		step := s.Every.N
		if s.Every.Unit == "hours" {
			step *= 60
		}
		from, _ := s.Every.From.Minutes()
		for m := from; m < 24*60; m += step {
			out = append(out, m)
		}
	case s.Daily != nil:
		for _, t := range s.Daily.Times {
			m, _ := t.Minutes()
			out = append(out, m)
		}
	case s.Weekly != nil:
		for _, d := range s.Weekly.Days {
			if weekdays[d] == wd {
				m, _ := s.Weekly.At.Minutes()
				out = append(out, m)
				break
			}
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// Slots returns the nominal run times t with after < t <= until, in order, without duplicates.
// Nominal means before any spread is added. The schedule must be valid.
func (s Schedule) Slots(after, until time.Time, loc *time.Location) []time.Time {
	if loc == nil {
		loc = time.UTC
	}
	first := after.In(loc)
	last := until.In(loc)
	day := time.Date(first.Year(), first.Month(), first.Day()-1, 0, 0, 0, 0, time.UTC)
	end := time.Date(last.Year(), last.Month(), last.Day()+1, 0, 0, 0, 0, time.UTC)
	var out []time.Time
	for ; !day.After(end); day = day.AddDate(0, 0, 1) {
		for _, m := range s.dayTimes(day.Weekday()) {
			t := WallTime(day.Year(), day.Month(), day.Day(), m/60, m%60, loc)
			if t.After(after) && !t.After(until) {
				out = append(out, t)
			}
		}
	}
	slices.SortFunc(out, func(a, b time.Time) int { return a.Compare(b) })
	return slices.CompactFunc(out, func(a, b time.Time) bool { return a.Equal(b) })
}

// Next returns the first nominal run time after t, and false if the schedule never runs.
func (s Schedule) Next(after time.Time, loc *time.Location) (time.Time, bool) {
	slots := s.Slots(after, after.AddDate(0, 0, 8), loc)
	if len(slots) == 0 {
		return time.Time{}, false
	}
	return slots[0], true
}

// NextN returns the next n nominal run times after t.
func (s Schedule) NextN(after time.Time, n int, loc *time.Location) []time.Time {
	var out []time.Time
	t := after
	for len(out) < n {
		next, ok := s.Next(t, loc)
		if !ok {
			break
		}
		out = append(out, next)
		t = next
	}
	return out
}

// LongestGap returns the longest time between two consecutive nominal runs. It is measured on wall
// clock time without daylight-saving changes, over two full weeks, so a weekly schedule's gap
// includes the wrap from its last day back to its first.
func (s Schedule) LongestGap() time.Duration {
	ref := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC) // a Monday
	slots := s.Slots(ref.Add(-time.Nanosecond), ref.AddDate(0, 0, 21), time.UTC)
	var longest time.Duration
	for i := 1; i < len(slots); i++ {
		if slots[i-1].Before(ref.AddDate(0, 0, 14)) {
			longest = max(longest, slots[i].Sub(slots[i-1]))
		}
	}
	return longest
}

// SpreadDuration is the spread when it is switched on, and zero otherwise.
func (s Schedule) SpreadDuration() time.Duration {
	if !s.Spread.Enabled {
		return 0
	}
	return time.Duration(s.Spread.Minutes) * time.Minute
}

// WorstCaseLoss is the longest gap plus the spread (spec §4.1).
func (s Schedule) WorstCaseLoss() time.Duration { return s.LongestGap() + s.SpreadDuration() }

// StaleAfter is the stale-alert threshold: 2 × the longest gap (spec §4.1).
func (s Schedule) StaleAfter() time.Duration { return 2 * s.LongestGap() }

// Placed is a schedule together with the zone its runs are in.
type Placed struct {
	Schedule Schedule
	Loc      *time.Location
}

// Day returns the start of the calendar day that contains t in loc, and the start of the next day.
// A day can be 23 or 25 hours long.
func Day(t time.Time, loc *time.Location) (start, end time.Time) {
	if loc == nil {
		loc = time.UTC
	}
	d := t.In(loc)
	return WallTime(d.Year(), d.Month(), d.Day(), 0, 0, loc), WallTime(d.Year(), d.Month(), d.Day()+1, 0, 0, loc)
}

// QuietWindow returns the longest quiet stretch of the calendar day that contains t, in loc. A stretch
// is quiet when no run of any of the schedules can start in it: it runs from the end of one run's
// spread to the next run's nominal start, and is cut off at the day's bounds. Ties go to the earliest
// stretch. A day without runs is quiet all day. ok is false when runs and their spreads leave no quiet
// time at all; start and end are then the day's bounds.
//
// The server places each host's daily restic check in the middle of this stretch (plan-m1 §10): check
// takes an exclusive lock, and a backup that meets it waits or fails.
func QuietWindow(t time.Time, loc *time.Location, runs []Placed) (start, end time.Time, ok bool) {
	dayStart, dayEnd := Day(t, loc)
	type span struct{ from, to time.Time }
	var busy []span
	for _, p := range runs {
		spread := p.Schedule.SpreadDuration()
		// A run that started before the day can still be inside its spread when the day begins.
		for _, s := range p.Schedule.Slots(dayStart.Add(-spread-time.Nanosecond), dayEnd, p.Loc) {
			from, to := s, s.Add(spread)
			if from.Before(dayStart) {
				from = dayStart
			}
			if to.After(dayEnd) {
				to = dayEnd
			}
			busy = append(busy, span{from, to})
		}
	}
	slices.SortFunc(busy, func(a, b span) int { return a.from.Compare(b.from) })
	cursor := dayStart
	consider := func(from, to time.Time) {
		if to.Sub(from) > end.Sub(start) {
			start, end, ok = from, to, true
		}
	}
	for _, b := range busy {
		if b.from.After(cursor) {
			consider(cursor, b.from)
		}
		if b.to.After(cursor) {
			cursor = b.to
		}
	}
	if dayEnd.After(cursor) {
		consider(cursor, dayEnd)
	}
	if !ok {
		return dayStart, dayEnd, false
	}
	return start, end, true
}

// WallTime returns the instant of a wall-clock time in loc. A time that does not exist that day
// (spring forward) becomes the next valid minute. A time that occurs twice (fall back) is the first
// occurrence.
func WallTime(y int, mo time.Month, d, h, mi int, loc *time.Location) time.Time {
	for k := 0; k <= 24*60; k++ {
		want := time.Date(y, mo, d, h, mi+k, 0, 0, time.UTC) // the wall clock we look for
		t := time.Date(y, mo, d, h, mi+k, 0, 0, loc)
		if !sameWall(t, want) {
			continue // this wall time does not exist in loc; try the next minute
		}
		// The wall time exists. If it also existed earlier (a repeated hour), take the earliest.
		for _, back := range []time.Duration{3 * time.Hour, 2 * time.Hour, 90 * time.Minute, time.Hour, 30 * time.Minute, 15 * time.Minute} {
			if e := t.Add(-back); sameWall(e, want) {
				return e
			}
		}
		return t
	}
	return time.Date(y, mo, d, h, mi, 0, 0, loc)
}

func sameWall(t, want time.Time) bool {
	ty, tm, td := t.Date()
	wy, wm, wd := want.Date()
	return ty == wy && tm == wm && td == wd && t.Hour() == want.Hour() && t.Minute() == want.Minute()
}
