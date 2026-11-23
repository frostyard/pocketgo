package team

import (
	"strings"
	"testing"
	"time"
)

// A launch takes 1 to 20 distinct proposals (ADR-0034).
func TestCheckLaunchRows(t *testing.T) {
	if err := CheckLaunchRows([]int64{3, 1, 2}); err != nil {
		t.Fatal(err)
	}
	many := make([]int64, MaxLaunchItems+1)
	for i := range many {
		many[i] = int64(i + 1)
	}
	for name, rows := range map[string][]int64{"none": nil, "too many": many, "a zero": {1, 0}, "twice": {2, 2}} {
		if err := CheckLaunchRows(rows); err == nil {
			t.Errorf("%s: taken", name)
		}
	}
}

// A launch ends after hours, at the next clock time, or at the next 06:45;
// its end is in the future and at most a day away (ADR-0034).
func TestLaunchEnd(t *testing.T) {
	zone, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip(err)
	}
	now := time.Date(2026, 11, 21, 22, 30, 0, 0, zone)
	for _, tc := range []struct {
		name      string
		now, want time.Time
		clock     bool
	}{
		{"evening default", time.Date(2026, 11, 21, 18, 0, 0, 0, zone), time.Date(2026, 11, 22, 6, 45, 0, 0, zone), true},
		{"early morning default", time.Date(2026, 11, 21, 5, 0, 0, 0, zone), time.Date(2026, 11, 21, 6, 45, 0, 0, zone), true},
		{"exact clock default", time.Date(2026, 11, 21, 6, 45, 0, 0, zone), time.Date(2026, 11, 22, 6, 45, 0, 0, zone), true},
		{"spring-forward default", time.Date(2027, 3, 13, 22, 0, 0, 0, zone), time.Date(2027, 3, 14, 6, 45, 0, 0, zone), true},
		{"fall-back evening default", time.Date(2026, 10, 31, 22, 0, 0, 0, zone), time.Date(2026, 11, 1, 6, 45, 0, 0, zone), true},
		{"fall-back clamped default", time.Date(2026, 10, 31, 7, 0, 0, 0, zone), time.Date(2026, 10, 31, 7, 0, 0, 0, zone).Add(24 * time.Hour), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := LaunchEnd(nil, "", tc.now, zone)
			if err != nil || !got.Equal(tc.want) {
				t.Fatalf("default end: %v %v; want %v", got, err, tc.want)
			}
			if tc.clock && (got.In(zone).Hour() != 6 || got.In(zone).Minute() != 45) {
				t.Errorf("default clock: %v; want 06:45", got.In(zone))
			}
			if err := CheckLaunchEnd(got, tc.now); err != nil {
				t.Errorf("default end refused: %v", err)
			}
		})
	}
	fallBackNow := time.Date(2026, 10, 31, 7, 0, 0, 0, zone)
	if got, err := LaunchEnd(nil, "06:45", fallBackNow, zone); err != nil || !got.Equal(time.Date(2026, 11, 1, 6, 45, 0, 0, zone)) {
		t.Errorf("explicit fall-back clock is not clamped: %v %v", got, err)
	}
	half := 1.5
	if got, err := LaunchEnd(&half, "", now, zone); err != nil || !got.Equal(now.Add(90*time.Minute)) {
		t.Errorf("hours: %v %v", got, err)
	}
	if got, err := LaunchEnd(nil, "6:00", now, zone); err != nil || !got.Equal(time.Date(2026, 11, 22, 6, 0, 0, 0, zone)) {
		t.Errorf("a clock time tomorrow: %v %v", got, err)
	}
	if got, err := LaunchEnd(nil, "23:15", now, zone); err != nil || !got.Equal(time.Date(2026, 11, 21, 23, 15, 0, 0, zone)) {
		t.Errorf("a clock time today: %v %v", got, err)
	}
	if got, err := LaunchEnd(nil, "22:30", now, zone); err != nil || !got.Equal(time.Date(2026, 11, 22, 22, 30, 0, 0, zone)) {
		t.Errorf("now's clock time: %v %v", got, err)
	}
	day := float64(MaxShiftHours + 1)
	zero := 0.0
	for name, tc := range map[string]struct {
		hours *float64
		until string
	}{"both": {&half, "6:00"}, "too long": {&day, ""}, "zero": {&zero, ""}, "no clock": {nil, "6pm"}, "bad minutes": {nil, "6:7"}, "25 hours": {nil, "25:00"}} {
		if _, err := LaunchEnd(tc.hours, tc.until, now, zone); err == nil {
			t.Errorf("%s: taken", name)
		}
	}
	if err := CheckLaunchEnd(now, now); err == nil {
		t.Error("an end that has come")
	}
	if err := CheckLaunchEnd(now.Add(25*time.Hour), now); err == nil {
		t.Error("an end more than a day away")
	}
	if err := CheckLaunchEnd(now.Add(24*time.Hour), now); err != nil {
		t.Errorf("an end a day away: %v", err)
	}
}

// A launch's proposals take at most its round budget together, and its
// items start within the at-once limit and the desk's reserve (ADR-0034).
func TestLaunchLimits(t *testing.T) {
	if err := CheckLaunchRounds([]int{100, 100}); err != nil {
		t.Errorf("the budget whole: %v", err)
	}
	if err := CheckLaunchRounds([]int{100, 100, 1}); err == nil || !strings.Contains(err.Error(), "201 rounds") {
		t.Errorf("over the budget: %v", err)
	}
	for _, tc := range []struct{ open, cap, running, want int }{
		{0, 12, 0, 3}, {8, 12, 0, 2}, {10, 12, 0, 0}, {11, 12, 0, 0}, {2, 12, 2, 1}, {3, 12, 3, 0}, {0, 2, 0, 0},
	} {
		if got := LaunchRoom(tc.open, tc.cap, tc.running); got != tc.want {
			t.Errorf("LaunchRoom(%d, %d, %d) = %d, want %d", tc.open, tc.cap, tc.running, got, tc.want)
		}
	}
}

// A launch's texts name each proposal and its end.
func TestLaunchTexts(t *testing.T) {
	lines := []LaunchLine{{ID: 3, Agent: "duncan", Title: "Fix the flaky test"}, {ID: 1, Agent: "vera", Title: "Drop dead code"}}
	note := LaunchNote(2, "Sun 22 Nov 06:00 EST", lines)
	if !strings.HasPrefix(note, "[launch L2] The owner launched 2 proposals from your board, until Sun 22 Nov 06:00 EST.") || !strings.Contains(note, "\n#3 duncan: Fix the flaky test\n#1 vera: Drop dead code") {
		t.Errorf("the note %q", note)
	}
	if text := LaunchedText(2, "Sun 22 Nov 06:00 EST", lines); !strings.HasPrefix(text, "Launch L2: 2 proposals start in this order") || !strings.Contains(text, "#1 vera: Drop dead code") ||
		!strings.HasSuffix(text, "Their reports go to your standing session.") {
		t.Errorf("the answer %q", text)
	}
}
