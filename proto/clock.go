package proto

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// NextClockTime is the first instant after now whose clock in zone reads
// clock, HH:MM (or H:MM) on the 24-hour clock: a launch's end chosen as a
// clock time (ADR-0034). The service and the desktop read it by this one
// policy (Codex review of #58, round 1):
//   - a clock time a change of daylight saving repeats has two instants,
//     and the first after now is the one;
//   - a clock time it skips is the instant the clock jumps over it, the
//     first after the gap.
func NextClockTime(clock string, now time.Time, zone *time.Location) (time.Time, error) {
	h, m, err := parseClock(clock)
	if err != nil {
		return time.Time{}, err
	}
	local := now.In(zone)
	for day := 0; day <= 2; day++ {
		for _, at := range clockInstants(local.Year(), local.Month(), local.Day()+day, h, m, zone) {
			if at.After(now) {
				return at, nil
			}
		}
	}
	return time.Time{}, fmt.Errorf("no %s comes after %s", clock, now.Format(time.RFC3339))
}

func parseClock(clock string) (int, int, error) {
	hh, mm, ok := strings.Cut(strings.TrimSpace(clock), ":")
	h, herr := strconv.Atoi(hh)
	m, merr := strconv.Atoi(mm)
	// Decimal digits alone: Atoi would take a sign (Codex review of #58,
	// round 2).
	if !ok || !digits(hh) || !digits(mm) || herr != nil || merr != nil || len(mm) != 2 || len(hh) < 1 || len(hh) > 2 || h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, 0, fmt.Errorf("%q is no clock time: write HH:MM on the 24-hour clock, as 07:30", clock)
	}
	return h, m, nil
}

func digits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != ""
}

// clockInstants are the instants, in order, of day (y, mo, d) in zone
// whose clock reads h:m: one, two in an hour a change of daylight saving
// repeats, or, for one it skips, the instant the clock jumps over it.
func clockInstants(y int, mo time.Month, d, h, m int, zone *time.Location) []time.Time {
	want := time.Date(y, mo, d, h, m, 0, 0, time.UTC)
	guess := time.Date(y, mo, d, h, m, 0, 0, zone)
	// Each instant that reads h:m is want less the zone's offset then: the
	// offsets around the day hold every one a single change gives.
	var out []time.Time
	for _, probe := range []time.Time{guess.Add(-3 * time.Hour), guess, guess.Add(3 * time.Hour)} {
		_, offset := probe.Zone()
		at := want.Add(-time.Duration(offset) * time.Second)
		if wall(at, zone).Equal(want) && !containsTime(out, at) {
			out = append(out, at.In(zone))
		}
	}
	if len(out) > 0 {
		sort.Slice(out, func(i, j int) bool { return out[i].Before(out[j]) })
		return out
	}
	// Skipped: the clock runs forward across the gap, so the first instant
	// whose clock reads h:m or later is where it jumps.
	lo, hi := guess.Add(-3*time.Hour), guess.Add(3*time.Hour)
	for hi.Sub(lo) > time.Second {
		mid := lo.Add(hi.Sub(lo) / 2).Truncate(time.Second)
		if wall(mid, zone).Before(want) {
			lo = mid
		} else {
			hi = mid
		}
	}
	return []time.Time{hi.In(zone)}
}

// wall is t's clock in zone, as the same reading in UTC.
func wall(t time.Time, zone *time.Location) time.Time {
	l := t.In(zone)
	return time.Date(l.Year(), l.Month(), l.Day(), l.Hour(), l.Minute(), l.Second(), 0, time.UTC)
}

func containsTime(list []time.Time, t time.Time) bool {
	for _, x := range list {
		if x.Equal(t) {
			return true
		}
	}
	return false
}
