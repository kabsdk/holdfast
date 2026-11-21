package config

import (
	"fmt"
	"strconv"
)

// Unit is a retention duration unit.
type Unit string

// Retention units. restic durations have only years, months, days and hours, so weeks become days.
const (
	Hours  Unit = "hours"
	Days   Unit = "days"
	Weeks  Unit = "weeks"
	Months Unit = "months"
	Years  Unit = "years"
)

// Duration is a retention window such as {14, days}.
type Duration struct {
	N    int  `json:"n"`
	Unit Unit `json:"unit"`
}

// Restic returns the duration in restic's format ("14d", "8w" becomes "56d", "12m" for months).
func (d Duration) Restic() string {
	switch d.Unit {
	case Hours:
		return strconv.Itoa(d.N) + "h"
	case Days:
		return strconv.Itoa(d.N) + "d"
	case Weeks:
		return strconv.Itoa(d.N*7) + "d"
	case Months:
		return strconv.Itoa(d.N) + "m"
	case Years:
		return strconv.Itoa(d.N) + "y"
	}
	return ""
}

func (d Duration) validate() error {
	switch d.Unit {
	case Hours, Days, Weeks, Months, Years:
	default:
		return fmt.Errorf("unit %q is not one of hours, days, weeks, months, years", d.Unit)
	}
	if d.N < 1 {
		return fmt.Errorf("%d %s: the number must be at least 1", d.N, d.Unit)
	}
	return nil
}

// Retention is time windows only, plus "always keep the newest N" (spec §4.2, decision 018).
type Retention struct {
	Within   *Duration `json:"within,omitempty"`  // --keep-within
	Hourly   *Duration `json:"hourly,omitempty"`  // --keep-within-hourly
	Daily    *Duration `json:"daily,omitempty"`   // --keep-within-daily
	Weekly   *Duration `json:"weekly,omitempty"`  // --keep-within-weekly
	Monthly  *Duration `json:"monthly,omitempty"` // --keep-within-monthly
	Yearly   *Duration `json:"yearly,omitempty"`  // --keep-within-yearly
	KeepLast int       `json:"keep_last"`         // --keep-last, default 3
}

// DefaultKeepLast is the default for "always keep the newest N".
const DefaultKeepLast = 3

// tiers lists the windows with their restic flags, finest first.
func (r Retention) tiers() []struct {
	name string
	flag string
	d    *Duration
} {
	return []struct {
		name string
		flag string
		d    *Duration
	}{
		{"within", "--keep-within", r.Within},
		{"hourly", "--keep-within-hourly", r.Hourly},
		{"daily", "--keep-within-daily", r.Daily},
		{"weekly", "--keep-within-weekly", r.Weekly},
		{"monthly", "--keep-within-monthly", r.Monthly},
		{"yearly", "--keep-within-yearly", r.Yearly},
	}
}

// ForgetArgs returns the restic forget flags for this retention. Count-based flags are never used.
func (r Retention) ForgetArgs() []string {
	var out []string
	for _, t := range r.tiers() {
		if t.d != nil {
			out = append(out, t.flag, t.d.Restic())
		}
	}
	if r.KeepLast > 0 {
		out = append(out, "--keep-last", strconv.Itoa(r.KeepLast))
	}
	return out
}

func (r Retention) validate() []string {
	var out []string
	for _, t := range r.tiers() {
		if t.d != nil {
			if err := t.d.validate(); err != nil {
				out = append(out, t.name+": "+err.Error())
			}
		}
	}
	if r.KeepLast < 0 {
		out = append(out, "keep_last cannot be negative")
	}
	return out
}
