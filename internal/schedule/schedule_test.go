package schedule

import (
	"errors"
	"testing"
	"time"
)

func mustLoc(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

func hhmm(times []time.Time, loc *time.Location) []string {
	var out []string
	for _, t := range times {
		out = append(out, t.In(loc).Format("Mon 02 15:04"))
	}
	return out
}

func eq(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestValidate(t *testing.T) {
	ok := []Schedule{
		{Daily: &Daily{Times: []ClockTime{"02:00", "14:00"}}},
		{Every: &Every{N: 4, Unit: "hours", From: "01:00"}, Spread: Spread{Enabled: true, Minutes: 20}},
		{Every: &Every{N: 15, Unit: "minutes", From: "00:00"}},
		{Weekly: &Weekly{Days: []string{"mon", "thu"}, At: "03:00"}, Zone: "Europe/Copenhagen"},
	}
	for i, s := range ok {
		if err := s.Validate(); err != nil {
			t.Errorf("valid schedule %d rejected: %v", i, err)
		}
	}
	bad := []Schedule{
		{},
		{Daily: &Daily{Times: []ClockTime{"02:00"}}, Weekly: &Weekly{Days: []string{"mon"}, At: "01:00"}},
		{Daily: &Daily{}},
		{Daily: &Daily{Times: []ClockTime{"24:00"}}},
		{Daily: &Daily{Times: []ClockTime{"2:00"}}},
		{Daily: &Daily{Times: []ClockTime{"02:00", "02:00"}}},
		{Every: &Every{N: 0, Unit: "hours", From: "00:00"}},
		{Every: &Every{N: 25, Unit: "hours", From: "00:00"}},
		{Every: &Every{N: 5, Unit: "days", From: "00:00"}},
		{Weekly: &Weekly{Days: []string{"monday"}, At: "03:00"}},
		{Weekly: &Weekly{Days: []string{"mon", "mon"}, At: "03:00"}},
		{Daily: &Daily{Times: []ClockTime{"02:00"}}, Spread: Spread{Minutes: -1}},
		{Daily: &Daily{Times: []ClockTime{"02:00"}}, Zone: "Mars/Olympus"},
	}
	for i, s := range bad {
		if err := s.Validate(); !errors.Is(err, ErrInvalid) {
			t.Errorf("invalid schedule %d accepted (err=%v)", i, err)
		}
	}
}

func TestNextRunsAndGaps(t *testing.T) {
	utc := time.UTC
	start := time.Date(2026, 11, 23, 10, 30, 0, 0, utc) // a Monday

	daily := Schedule{Daily: &Daily{Times: []ClockTime{"14:00", "02:00"}}}
	eq(t, hhmm(daily.NextN(start, 5, utc), utc), []string{"Mon 23 14:00", "Tue 24 02:00", "Tue 24 14:00", "Wed 25 02:00", "Wed 25 14:00"})
	if g := daily.LongestGap(); g != 12*time.Hour {
		t.Errorf("daily 02:00+14:00 longest gap = %v, want 12h", g)
	}

	every4 := Schedule{Every: &Every{N: 4, Unit: "hours", From: "01:00"}}
	eq(t, hhmm(every4.NextN(start, 4, utc), utc), []string{"Mon 23 13:00", "Mon 23 17:00", "Mon 23 21:00", "Tue 24 01:00"})
	if g := every4.LongestGap(); g != 4*time.Hour {
		t.Errorf("every 4 h from 01:00 longest gap = %v, want 4h", g)
	}
	// Every 5 h from 01:00: 01, 06, 11, 16, 21, then 01 again: the overnight gap is 4 h, the longest 5 h.
	every5 := Schedule{Every: &Every{N: 5, Unit: "hours", From: "01:00"}}
	if g := every5.LongestGap(); g != 5*time.Hour {
		t.Errorf("every 5 h from 01:00 longest gap = %v, want 5h", g)
	}
	// Every 7 h from 20:00 runs once a day: the gap is 24 h.
	every7late := Schedule{Every: &Every{N: 7, Unit: "hours", From: "20:00"}}
	if g := every7late.LongestGap(); g != 24*time.Hour {
		t.Errorf("every 7 h from 20:00 longest gap = %v, want 24h", g)
	}

	weekly := Schedule{Weekly: &Weekly{Days: []string{"thu", "mon"}, At: "03:00"}, Spread: Spread{Enabled: true, Minutes: 30}}
	eq(t, hhmm(weekly.NextN(start, 3, utc), utc), []string{"Thu 26 03:00", "Mon 30 03:00", "Thu 03 03:00"})
	if g := weekly.LongestGap(); g != 4*24*time.Hour {
		t.Errorf("weekly mon+thu longest gap = %v, want 96h", g)
	}
	if w := weekly.WorstCaseLoss(); w != 4*24*time.Hour+30*time.Minute {
		t.Errorf("worst-case loss = %v, want 96h30m", w)
	}
	if s := weekly.StaleAfter(); s != 8*24*time.Hour {
		t.Errorf("stale after = %v, want 192h", s)
	}
	weekly.Spread.Enabled = false
	if w := weekly.WorstCaseLoss(); w != 4*24*time.Hour {
		t.Errorf("worst-case loss with spread off = %v, want 96h (the minutes are kept but not used)", w)
	}
}

func TestDaylightSaving(t *testing.T) {
	cph := mustLoc(t, "Europe/Copenhagen")

	// 29 Mar 2026: 02:00 CET jumps to 03:00 CEST. 02:30 does not exist: run at 03:00, the next valid minute.
	got := WallTime(2026, time.March, 29, 2, 30, cph)
	if want := time.Date(2026, 3, 29, 1, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Errorf("02:30 on spring-forward day = %v, want %v (03:00 CEST)", got.UTC(), want)
	}

	// 25 Oct 2026: 03:00 CEST falls back to 02:00 CET. 02:30 happens twice: run at the first one (CEST).
	got = WallTime(2026, time.October, 25, 2, 30, cph)
	if want := time.Date(2026, 10, 25, 0, 30, 0, 0, time.UTC); !got.Equal(want) {
		t.Errorf("02:30 on fall-back day = %v, want %v (02:30 CEST, the first)", got.UTC(), want)
	}

	// Every 30 minutes across spring forward: 02:00 and 02:30 both become 03:00, which runs once.
	s := Schedule{Every: &Every{N: 30, Unit: "minutes", From: "00:00"}}
	slots := s.Slots(time.Date(2026, 3, 29, 0, 0, 0, 0, cph), time.Date(2026, 3, 29, 4, 0, 0, 0, cph), cph)
	eq(t, hhmm(slots, cph), []string{"Sun 29 00:30", "Sun 29 01:00", "Sun 29 01:30", "Sun 29 03:00", "Sun 29 03:30", "Sun 29 04:00"})

	// Across fall back, a daily 02:30 still runs exactly once that day.
	d := Schedule{Daily: &Daily{Times: []ClockTime{"02:30"}}}
	slots = d.Slots(time.Date(2026, 10, 24, 12, 0, 0, 0, cph), time.Date(2026, 10, 25, 12, 0, 0, 0, cph), cph)
	if len(slots) != 1 {
		t.Errorf("daily 02:30 on fall-back day ran %d times, want 1", len(slots))
	}
}

func TestLocation(t *testing.T) {
	cph := mustLoc(t, "Europe/Copenhagen")
	s := Schedule{Daily: &Daily{Times: []ClockTime{"02:00"}}}
	if loc, _ := s.Location(cph); loc != cph {
		t.Errorf("empty zone should use the fallback")
	}
	s.Zone = "UTC"
	if loc, _ := s.Location(cph); loc.String() != "UTC" {
		t.Errorf("explicit zone should win, got %v", loc)
	}
}
