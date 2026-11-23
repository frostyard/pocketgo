package proto

import (
	"testing"
	"time"
)

// A clock time is its next instant after now: today's or tomorrow's; one
// daylight saving skips is the instant the clock jumps over it; one it
// repeats is the first of its instants after now (Codex review of #58,
// round 1).
func TestNextClockTime(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip(err)
	}
	at := func(s string) time.Time {
		v, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	for name, tc := range map[string]struct {
		clock string
		now   time.Time
		want  time.Time
	}{
		"later today":     {"23:15", at("2026-11-21T22:30:00-05:00"), at("2026-11-21T23:15:00-05:00")},
		"tomorrow":        {"6:00", at("2026-11-21T22:30:00-05:00"), at("2026-11-22T06:00:00-05:00")},
		"now's own":       {"22:30", at("2026-11-21T22:30:00-05:00"), at("2026-11-22T22:30:00-05:00")},
		"skipped":         {"02:30", at("2027-03-14T01:45:00-05:00"), at("2027-03-14T03:00:00-04:00")},
		"after the gap":   {"03:30", at("2027-03-14T01:45:00-05:00"), at("2027-03-14T03:30:00-04:00")},
		"repeated, first": {"01:30", at("2026-11-01T00:45:00-04:00"), at("2026-11-01T01:30:00-04:00")},
		"repeated, again": {"01:30", at("2026-11-01T01:40:00-04:00"), at("2026-11-01T01:30:00-05:00")},
		"after both":      {"01:30", at("2026-11-01T01:40:00-05:00"), at("2026-11-02T01:30:00-05:00")},
	} {
		got, err := NextClockTime(tc.clock, tc.now, ny)
		if err != nil || !got.Equal(tc.want) {
			t.Errorf("%s: %v %v, want %v", name, got, err, tc.want)
		}
	}
	for _, bad := range []string{"", "6pm", "6:7", "24:00", "7:60", "007:00", "7:+5", "-0:30", "+7:05"} {
		if _, err := NextClockTime(bad, at("2026-11-21T22:30:00-05:00"), ny); err == nil {
			t.Errorf("%q: taken", bad)
		}
	}
}

func TestDefaultLaunchUntil(t *testing.T) {
	h, m, err := parseClock(DefaultLaunchUntil)
	if err != nil || h != 6 || m != 45 {
		t.Fatalf("default launch clock: %d:%02d, error %v; want 6:45", h, m, err)
	}
}
