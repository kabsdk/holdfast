package schedule

import (
	"testing"
	"time"
)

func TestQuietWindow(t *testing.T) {
	cph := mustLoc(t, "Europe/Copenhagen")
	monday := time.Date(2026, 11, 23, 9, 0, 0, 0, cph)
	tuesday := monday.AddDate(0, 0, 1)
	daily := func(times ...ClockTime) Schedule { return Schedule{Daily: &Daily{Times: times}} }
	in := func(s Schedule) Placed { return Placed{Schedule: s, Loc: cph} }
	spread := func(s Schedule, m int) Schedule { s.Spread = Spread{Enabled: true, Minutes: m}; return s }

	for _, tc := range []struct {
		name       string
		day        time.Time
		runs       []Placed
		start, end string
		ok         bool
	}{
		{"one run a day: from that run to midnight", monday, []Placed{in(daily("01:00"))}, "Mon 01:00", "Tue 00:00", true},
		{"two runs: the longer stretch between them", monday, []Placed{in(daily("01:00", "13:00"))}, "Mon 01:00", "Mon 13:00", true},
		{"equal stretches: the earliest, after the spread",
			monday, []Placed{in(spread(Schedule{Every: &Every{N: 4, Unit: "hours", From: "00:00"}}, 30))}, "Mon 00:30", "Mon 04:00", true},
		{"a switched-off spread does not count",
			monday, []Placed{in(Schedule{Every: &Every{N: 4, Unit: "hours", From: "00:00"}, Spread: Spread{Minutes: 30}})}, "Mon 00:00", "Mon 04:00", true},
		{"weekly, on its day", monday, []Placed{in(Schedule{Weekly: &Weekly{Days: []string{"mon"}, At: "02:00"}})}, "Mon 02:00", "Tue 00:00", true},
		{"weekly, on another day: quiet all day", tuesday, []Placed{in(Schedule{Weekly: &Weekly{Days: []string{"mon"}, At: "02:00"}})}, "Tue 00:00", "Wed 00:00", true},
		{"yesterday's spread reaches past midnight", tuesday, []Placed{in(spread(daily("23:30"), 60))}, "Tue 00:30", "Tue 23:30", true},
		{"no jobs: quiet all day", monday, nil, "Mon 00:00", "Tue 00:00", true},
		{"runs in two zones", monday, []Placed{in(daily("01:00")), {Schedule: daily("06:00"), Loc: time.UTC}}, "Mon 07:00", "Tue 00:00", true},
		{"no quiet time at all", monday, []Placed{in(spread(Schedule{Every: &Every{N: 1, Unit: "minutes", From: "00:00"}}, 5))}, "Mon 00:00", "Tue 00:00", false},
	} {
		start, end, ok := QuietWindow(tc.day, cph, tc.runs)
		if got := [2]string{start.In(cph).Format("Mon 15:04"), end.In(cph).Format("Mon 15:04")}; got != [2]string{tc.start, tc.end} || ok != tc.ok {
			t.Errorf("%s: got %v ok=%v, want [%s %s] ok=%v", tc.name, got, ok, tc.start, tc.end, tc.ok)
		}
	}
}

func TestQuietWindowAcrossDaylightSaving(t *testing.T) {
	cph := mustLoc(t, "Europe/Copenhagen")
	runs := []Placed{{Schedule: Schedule{Daily: &Daily{Times: []ClockTime{"01:00"}}}, Loc: cph}}
	for _, tc := range []struct {
		day            time.Time
		dayLen, window time.Duration
	}{
		{time.Date(2026, 10, 25, 12, 0, 0, 0, cph), 25 * time.Hour, 24 * time.Hour}, // clocks go back at 03:00
		{time.Date(2026, 3, 29, 12, 0, 0, 0, cph), 23 * time.Hour, 22 * time.Hour},  // clocks go forward at 02:00
		{time.Date(2026, 11, 23, 12, 0, 0, 0, cph), 24 * time.Hour, 23 * time.Hour},
	} {
		ds, de := Day(tc.day, cph)
		start, end, ok := QuietWindow(tc.day, cph, runs)
		if de.Sub(ds) != tc.dayLen || end.Sub(start) != tc.window || !ok || !end.Equal(de) {
			t.Errorf("%s: day %v, window %v (%s to %s); want day %v, window %v ending at midnight",
				tc.day.Format("2006-01-02"), de.Sub(ds), end.Sub(start), start.In(cph), end.In(cph), tc.dayLen, tc.window)
		}
	}
}
