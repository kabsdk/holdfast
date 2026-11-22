package checks

import (
	"strings"
	"testing"

	"github.com/kabsdk/holdfast/internal/config"
)

// O101 (decision 039): an off-site retention that keeps less than primary in any window, or fewer of the newest.
func TestOffsiteShorter(t *testing.T) {
	d := func(n int, u config.Unit) *config.Duration { return &config.Duration{N: n, Unit: u} }
	primary := config.Retention{Daily: d(14, config.Days), Monthly: d(12, config.Months), KeepLast: 3}
	for _, tc := range []struct {
		name    string
		offsite config.Retention
		want    string // "" for no finding
	}{
		{"the same", primary, ""},
		{"longer, and a tier more", config.Retention{Daily: d(4, config.Weeks), Monthly: d(2, config.Years), Yearly: d(5, config.Years), KeepLast: 3}, ""},
		{"a shorter window", config.Retention{Daily: d(7, config.Days), Monthly: d(12, config.Months), KeepLast: 3}, "daily: 1 week, against 2 weeks"},
		{"a missing window", config.Retention{Daily: d(14, config.Days), KeepLast: 3}, "monthly: none, against 12 months"},
		{"fewer of the newest", config.Retention{Daily: d(14, config.Days), Monthly: d(12, config.Months), KeepLast: 1}, "the newest 1, against 3"},
	} {
		f := Offsite(tc.offsite, primary)
		switch {
		case tc.want == "" && f != nil:
			t.Errorf("%s: %s", tc.name, f.Message)
		case tc.want != "" && (f == nil || f.Code != "O101" || f.Severity != Warning || !strings.Contains(f.Message, tc.want)):
			t.Errorf("%s: got %+v, want %q", tc.name, f, tc.want)
		}
	}
}
