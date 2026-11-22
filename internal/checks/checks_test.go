package checks

import (
	"slices"
	"testing"
	"time"

	"github.com/kabsdk/holdfast/internal/config"
	"github.com/kabsdk/holdfast/internal/schedule"
)

func d(n int, u config.Unit) *config.Duration { return &config.Duration{N: n, Unit: u} }

var (
	daily2 = schedule.Schedule{Daily: &schedule.Daily{Times: []schedule.ClockTime{"02:00"}}}
	good   = config.Retention{Daily: d(14, config.Days), Weekly: d(8, config.Weeks), Monthly: d(12, config.Months), Yearly: d(2, config.Years), KeepLast: 3}
	every  = func(n int, unit string) schedule.Schedule {
		return schedule.Schedule{Every: &schedule.Every{N: n, Unit: unit, From: "00:00"}}
	}
	weekly1 = schedule.Schedule{Weekly: &schedule.Weekly{Days: []string{"sun"}, At: "03:00"}}
)

func TestJobChecks(t *testing.T) {
	for _, tc := range []struct {
		name string
		job  config.Job
		h    History
		want []string
	}{
		{"a sound daily job", config.Job{Schedule: daily2, Retention: good}, History{}, nil},
		{"a daily job with a spread does not lose more than a day's worth (plan-m4 Q12)", config.Job{Schedule: func() schedule.Schedule {
			s := daily2
			s.Spread = schedule.Spread{Enabled: true, Minutes: 20}
			return s
		}(), Retention: good}, History{}, nil},
		{"an hourly tier on a daily schedule", config.Job{Schedule: daily2, Retention: config.Retention{Hourly: d(48, config.Hours), Daily: d(14, config.Days), Yearly: d(1, config.Years), KeepLast: 3}}, History{}, []string{"R001"}},
		{"a weekly window of 3 days", config.Job{Schedule: daily2, Retention: config.Retention{Daily: d(60, config.Days), Weekly: d(3, config.Days), Yearly: d(1, config.Years), KeepLast: 3}}, History{}, []string{"R002", "R003"}},
		{"a weekly tier shorter than the daily one", config.Job{Schedule: daily2, Retention: config.Retention{Daily: d(30, config.Days), Weekly: d(2, config.Weeks), Yearly: d(1, config.Years), KeepLast: 3}}, History{}, []string{"R003"}},
		{"nothing kept", config.Job{Schedule: daily2, Retention: config.Retention{}}, History{}, []string{"R004", "I001", "R101"}},
		{"a tier with no duration", config.Job{Schedule: daily2, Retention: config.Retention{Daily: d(0, config.Days), Yearly: d(1, config.Years), KeepLast: 3}}, History{}, []string{"R005"}},
		{"a spread of half the interval", config.Job{Schedule: func() schedule.Schedule {
			s := every(1, "hours")
			s.Spread = schedule.Spread{Enabled: true, Minutes: 30}
			return s
		}(), Retention: good}, History{}, []string{"S001"}},
		{"an invalid schedule", config.Job{Schedule: schedule.Schedule{}, Retention: good}, History{}, []string{"S003"}},
		{"runs longer than the interval", config.Job{Schedule: every(15, "minutes"), Retention: good},
			History{RunTimes: []time.Duration{20 * time.Minute, 21 * time.Minute, 19 * time.Minute, 22 * time.Minute, 20 * time.Minute}}, []string{"S002"}},
		{"too few runs to judge", config.Job{Schedule: every(15, "minutes"), Retention: good},
			History{RunTimes: []time.Duration{20 * time.Minute, 21 * time.Minute}}, nil},
		{"keep newest is 0", config.Job{Schedule: daily2, Retention: func() config.Retention { r := good; r.KeepLast = 0; return r }()}, History{}, []string{"R006"}},
		{"a week of history only", config.Job{Schedule: daily2, Retention: config.Retention{Within: d(7, config.Days), KeepLast: 3}}, History{}, []string{"R101", "I001"}},
		{"thousands of snapshots", config.Job{Schedule: every(5, "minutes"), Retention: config.Retention{Within: d(30, config.Days), Yearly: d(1, config.Years), KeepLast: 3}}, History{}, []string{"R102"}},
		{"a weekly schedule loses up to a week", config.Job{Schedule: weekly1, Retention: config.Retention{Weekly: d(8, config.Weeks), Monthly: d(12, config.Months), Yearly: d(2, config.Years), KeepLast: 3}}, History{}, []string{"R103"}},
		{"a daily tier on a weekly schedule", config.Job{Schedule: weekly1, Retention: good}, History{}, []string{"R001", "R103"}},
	} {
		got := Codes(Job(tc.job, tc.h))
		want := tc.want
		if want == nil {
			want = []string{}
		}
		slices.Sort(got)
		slices.Sort(want)
		if !slices.Equal(got, want) {
			t.Errorf("%s: got %v, want %v (%+v)", tc.name, got, want, Job(tc.job, tc.h))
		}
	}
}

func TestRetentionCut(t *testing.T) {
	if f := RetentionCut([]string{"a", "b", "c"}, []string{"a", "b"}); f == nil || f.Code != "R104" {
		t.Fatalf("a strict subset is a cut: %+v", f)
	}
	if f := RetentionCut([]string{"a", "b", "c"}, []string{"a", "b", "d"}); f != nil {
		t.Fatalf("keeping something new is not a cut: %+v", f)
	}
	if f := RetentionCut([]string{"a", "b"}, []string{"a", "b"}); f != nil {
		t.Fatalf("the same keep set is not a cut: %+v", f)
	}
}

func TestBlockingAndAccepted(t *testing.T) {
	fs := []Finding{{Code: "R001", Severity: Error}, {Code: "R102", Severity: Warning}, {Code: "R104", Severity: Warning}, {Code: "I001", Severity: Info}}
	if got := Codes(Blocking(fs, []string{"R102", "R001"})); !slices.Equal(got, []string{"R001", "R104"}) {
		t.Fatalf("an error cannot be accepted, and an unaccepted warning blocks: %v", got)
	}
	if got := Accepted(fs, []string{"R102", "R001", "X999"}); !slices.Equal(got, []string{"R102"}) {
		t.Fatalf("only warnings that fired can be recorded as accepted: %v", got)
	}
}
