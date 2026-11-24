package server

import (
	"strings"
	"testing"
	"time"
)

func TestPGMajor(t *testing.T) {
	for v, want := range map[string]int{"16.3": 16, "18.6": 18, "18beta1": 18, "9.6.24": 9, "": 0, "x": 0} {
		if got := pgMajor(v); got != want {
			t.Errorf("pgMajor(%q) = %d, want %d", v, got, want)
		}
	}
}

// A restore's new database is named after the original and the dump's time in the server's zone, within PostgreSQL's
// 63 bytes (decision 055).
func TestPGRestoreName(t *testing.T) {
	cph, err := time.LoadLocation("Europe/Copenhagen")
	if err != nil {
		t.Skip("no time zone data")
	}
	s := &Server{zone: cph}
	at := time.Date(2026, 11, 24, 23, 30, 0, 0, time.UTC)
	if got := s.pgRestoreName("authelia", at); got != "authelia-restore-20261125-0030" {
		t.Errorf("name: %s", got)
	}
	long := strings.Repeat("d", 63)
	got := s.pgRestoreName(long, at)
	if len(got) != 63 || !strings.HasSuffix(got, "-restore-20261125-0030") {
		t.Errorf("a long name must be cut to 63 bytes, keeping the suffix: %s (%d)", got, len(got))
	}
}
