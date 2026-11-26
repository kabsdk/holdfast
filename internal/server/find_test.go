package server

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// A find's options: days as YYYY-MM-DD in the zone asked for, from the start of the oldest to the end of the newest;
// a day that is not one, one after today, and an oldest after the newest are refused, and say so.
func TestFindOptions(t *testing.T) {
	cph, _ := time.LoadLocation("Europe/Copenhagen")
	s := &Server{now: func() time.Time { return time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC) }}
	o, err := s.findOptions(FindInput{IgnoreCase: true, Oldest: "2026-11-01", Newest: "2026-11-26", Zone: "Europe/Copenhagen"})
	if err != nil || !o.IgnoreCase || !o.Oldest.Equal(time.Date(2026, 11, 1, 0, 0, 0, 0, cph)) || !o.Newest.Equal(time.Date(2026, 11, 26, 23, 59, 59, 0, cph)) {
		t.Fatalf("options: %+v %v", o, err)
	}
	if o, err := s.findOptions(FindInput{}); err != nil || !o.Oldest.IsZero() || !o.Newest.IsZero() || o.IgnoreCase {
		t.Fatalf("no options: %+v %v", o, err)
	}
	for _, c := range []struct {
		in   FindInput
		path string
		msg  string
	}{
		{FindInput{Oldest: "1.11.2026"}, "oldest", "isn't a date"},
		{FindInput{Newest: "2026-02-30"}, "newest", "isn't a date"},
		{FindInput{Newest: "2026-11-27"}, "newest", "after today"},
		{FindInput{Oldest: "2026-11-20", Newest: "2026-11-10"}, "oldest", "From (2026-11-20) is after To (2026-11-10)."},
		{FindInput{Oldest: "2026-11-01", Zone: "Mars/Olympus"}, "tz", "not a time zone"},
	} {
		_, err := s.findOptions(c.in)
		var ve *ValidationError
		if !errors.As(err, &ve) || ve.Problems[0].Path != c.path || !strings.Contains(ve.Problems[0].Message, c.msg) {
			t.Errorf("%+v: %v, want %s: …%s…", c.in, err, c.path, c.msg)
		}
	}
}
