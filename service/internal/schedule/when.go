// Package schedule holds the rules of scheduled messages (ADR-0030): when
// one goes out, in the words the owner and the supervisor write, its limits,
// and the texts pocketd writes. It is pi-pocket's when.ts in Go: clock times
// are wall-clock times in a time zone, so `every day 19:00` stays at 19:00
// across daylight saving changes.
package schedule

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	// Zone names resolve on a host with no zone database, as a minimal
	// image: pocketd carries Go's own (ADR-0030).
	_ "time/tzdata"
)

// The bounds of a schedule (ADR-0030).
const (
	// MinRepeat is the shortest repeat: a runaway loop of model calls
	// costs money.
	MinRepeat = 10 * time.Minute
	// MaxAhead is how far ahead a time that goes out once may be.
	MaxAhead = 366 * 24 * time.Hour
)

// Help says what Parse reads.
const Help = "say when: in 30m, 7:00, 7pm, tomorrow 9:00, fri 17:30, or every 2h, every day 19:00, every weekday 8:00, every mon,thu 9:00"

// Clock is a wall-clock time of day.
type Clock struct{ Hour, Minute int }

func (c Clock) String() string { return fmt.Sprintf("%02d:%02d", c.Hour, c.Minute) }

// Repeat is how a schedule repeats: every Every, or at At on Days (every
// day when Days is nil).
type Repeat struct {
	Every time.Duration
	At    Clock
	Days  []time.Weekday
}

// Spec is when a schedule goes out, as Parse read it: once (In from now,
// or at At on a day), or on Repeat. It needs no clock: First gives its
// first time from a moment.
type Spec struct {
	// In is the span of `in 30m`; 0 otherwise.
	In time.Duration
	// At and Day are `7:00` (Day -1, the next time it comes), `today 9:00`
	// (Day 0), `tomorrow 9:00` (Day 1), or `fri 17:30` (Days, Day -1).
	At   *Clock
	Day  int
	Days []time.Weekday
	// Repeat is set for a schedule that repeats.
	Repeat *Repeat
}

// Once says the schedule goes out one time.
func (s Spec) Once() bool { return s.Repeat == nil }

var (
	dayNames  = []string{"sunday", "monday", "tuesday", "wednesday", "thursday", "friday", "saturday"}
	weekdays  = []time.Weekday{time.Monday, time.Tuesday, time.Wednesday, time.Thursday, time.Friday}
	weekend   = []time.Weekday{time.Sunday, time.Saturday}
	clockForm = regexp.MustCompile(`^(\d{1,2})(?::(\d{2}))?(am|pm)?$`)
	joined    = regexp.MustCompile(`^(\d+(?:\.\d+)?)([a-z]+)$`)
	number    = regexp.MustCompile(`^\d+(?:\.\d+)?$`)
	units     = map[string]time.Duration{
		"m": time.Minute, "min": time.Minute, "mins": time.Minute, "minute": time.Minute, "minutes": time.Minute,
		"h": time.Hour, "hr": time.Hour, "hrs": time.Hour, "hour": time.Hour, "hours": time.Hour,
		"d": 24 * time.Hour, "day": 24 * time.Hour, "days": 24 * time.Hour,
	}
)

// Canonical is a when as Parse reads it: its words in lowercase, one space
// apart. Two whens with the same canonical form are one when.
func Canonical(when string) string { return strings.Join(strings.Fields(strings.ToLower(when)), " ") }

// Parse reads when a schedule goes out, in the words people write: `in
// 30m`, `7:00`, `7pm`, `today 9:00`, `tomorrow 9am`, `fri 17:30`, `every
// 2h`, `every day 19:00`, `every weekday 8:00`, `every weekend 10:00`,
// `every mon,thu 9:00`. A repeat is MinRepeat apart at least, and a time
// that goes out once MaxAhead ahead at most; a word that is no part of a
// time is refused.
func Parse(text string) (Spec, error) {
	words := strings.Fields(strings.ToLower(text))
	if len(words) == 0 {
		return Spec{}, errors.New(Help)
	}
	spec, used, err := parseWords(words)
	if err != nil {
		return Spec{}, err
	}
	if used < len(words) {
		return Spec{}, fmt.Errorf("%q is not part of a time. %s", strings.Join(words[used:], " "), Help)
	}
	return spec, nil
}

func parseWords(words []string) (Spec, int, error) {
	word := func(i int) string {
		if i < len(words) {
			return words[i]
		}
		return ""
	}
	switch words[0] {
	case "every":
		if span, used, ok := duration(words[1:]); ok {
			if span < MinRepeat {
				return Spec{}, 0, fmt.Errorf("a schedule repeats at most every %d minutes", int(MinRepeat/time.Minute))
			}
			// "every 2 days 8:00" would repeat every two days from now, not
			// at 8:00.
			if next := word(1 + used); next == "at" || clockTime(next) != nil {
				return Spec{}, 0, errors.New("repeat after a time (every 2 days) or at a clock time (every day 8:00), not both")
			}
			return Spec{Day: -1, Repeat: &Repeat{Every: span}}, 1 + used, nil
		}
		days, every := dayList(word(1))
		at := 1
		if days != nil || every {
			at = 2
		}
		clock := clockTime(word(at))
		if clock == nil {
			return Spec{}, 0, errors.New(Help)
		}
		return Spec{Day: -1, Repeat: &Repeat{At: *clock, Days: days}}, at + 1, nil
	case "in":
		span, used, ok := duration(words[1:])
		if !ok {
			return Spec{}, 0, errors.New(Help)
		}
		if span > MaxAhead {
			return Spec{}, 0, fmt.Errorf("a schedule goes out at most %d days ahead", int(MaxAhead/(24*time.Hour)))
		}
		return Spec{In: span, Day: -1}, 1 + used, nil
	case "today", "tomorrow":
		clock := clockTime(word(1))
		if clock == nil {
			return Spec{}, 0, errors.New(Help)
		}
		day := 0
		if words[0] == "tomorrow" {
			day = 1
		}
		return Spec{At: clock, Day: day}, 2, nil
	}
	if days, _ := dayList(words[0]); len(days) == 1 {
		clock := clockTime(word(1))
		if clock == nil {
			return Spec{}, 0, errors.New(Help)
		}
		return Spec{At: clock, Day: -1, Days: days}, 2, nil
	}
	clock := clockTime(words[0])
	if clock == nil {
		return Spec{}, 0, errors.New(Help)
	}
	return Spec{At: clock, Day: -1}, 1, nil
}

// clockTime reads `7:30`, `19:05`, `7pm`, `7:30am`; nil for anything else,
// a bare number included.
func clockTime(word string) *Clock {
	m := clockForm.FindStringSubmatch(word)
	if m == nil || (m[2] == "" && m[3] == "") {
		return nil
	}
	hour, _ := strconv.Atoi(m[1])
	minute := 0
	if m[2] != "" {
		minute, _ = strconv.Atoi(m[2])
	}
	if m[3] != "" {
		if hour < 1 || hour > 12 {
			return nil
		}
		hour %= 12
		if m[3] == "pm" {
			hour += 12
		}
	}
	if hour > 23 || minute > 59 {
		return nil
	}
	return &Clock{Hour: hour, Minute: minute}
}

// dayList reads `mon`, `mon,wed,fri`, `weekday(s)` and `weekend(s)` as days
// of the week, in order, and `day` or `daily` as every day (every); nil and
// false for anything else. A day is named by its first three letters on.
func dayList(word string) (days []time.Weekday, every bool) {
	switch word {
	case "day", "daily":
		return nil, true
	case "weekday", "weekdays":
		return slices.Clone(weekdays), false
	case "weekend", "weekends":
		return slices.Clone(weekend), false
	}
	if word == "" {
		return nil, false
	}
	for _, name := range strings.Split(word, ",") {
		day := dayOfWeek(name)
		if day < 0 {
			return nil, false
		}
		if !slices.Contains(days, time.Weekday(day)) {
			days = append(days, time.Weekday(day))
		}
	}
	slices.Sort(days)
	return days, false
}

func dayOfWeek(name string) int {
	if len(name) < 3 {
		return -1
	}
	for i, day := range dayNames {
		if strings.HasPrefix(day, name) {
			return i
		}
	}
	return -1
}

// duration reads `30m`, `1.5h` or `2 hours` from the first one or two
// words, as whole minutes, one at least.
func duration(words []string) (time.Duration, int, bool) {
	if len(words) == 0 {
		return 0, 0, false
	}
	amount, unit, used := "", "", 0
	if m := joined.FindStringSubmatch(words[0]); m != nil {
		amount, unit, used = m[1], m[2], 1
	} else if len(words) > 1 && number.MatchString(words[0]) {
		amount, unit, used = words[0], words[1], 2
	} else {
		return 0, 0, false
	}
	per, ok := units[unit]
	if !ok {
		return 0, 0, false
	}
	n, err := strconv.ParseFloat(amount, 64)
	if err != nil {
		return 0, 0, false
	}
	minutes := int64(n*float64(per/time.Minute) + 0.5)
	// A span past a hundred years is no schedule; its minutes would also
	// overflow a duration.
	if minutes < 1 || minutes > 100*366*24*60 {
		return 0, 0, false
	}
	return time.Duration(minutes) * time.Minute, used, true
}

// First is the first time the schedule goes out after now, by the wall
// clock in zone. A `today` time that has passed is an error.
func (s Spec) First(now time.Time, zone *time.Location) (time.Time, error) {
	switch {
	case s.Repeat != nil:
		return s.Repeat.Next(now, zone), nil
	case s.In > 0:
		return now.Add(s.In), nil
	case s.At != nil && s.Day >= 0:
		y, m, d := now.In(zone).Date()
		at := Moment(y, m, d+s.Day, s.At.Hour, s.At.Minute, zone)
		if !at.After(now) {
			return time.Time{}, fmt.Errorf("%s today has passed", s.At)
		}
		return at, nil
	case s.At != nil:
		return nextClock(now, *s.At, s.Days, zone), nil
	}
	return time.Time{}, errors.New(Help)
}

// Next is the first time after after that the repeat goes out, by the wall
// clock in zone.
func (r Repeat) Next(after time.Time, zone *time.Location) time.Time {
	if r.Every > 0 {
		return after.Add(r.Every)
	}
	return nextClock(after, r.At, r.Days, zone)
}

// After is the first time after now that the repeat goes out, once its
// time due came: a repeat after a span keeps its beat from due, and one at
// a clock time is the next such time. No time before now goes out, so a
// repeat whose times a stop of pocketd missed goes out once, then on.
func (r Repeat) After(due, now time.Time, zone *time.Location) time.Time {
	if r.Every <= 0 {
		return nextClock(now, r.At, r.Days, zone)
	}
	n := int64(1)
	if now.After(due) {
		n += int64(now.Sub(due) / r.Every)
	}
	return due.Add(time.Duration(n) * r.Every)
}

// nextClock is the first moment after after when the wall clock in zone
// shows at on one of days (any day when nil). A day is the date asked for,
// never the date of the moment it resolves to: a time a gap at midnight
// moves into the next day goes out on its own day's night (Codex review of
// #51, round 2). The search starts the day before after's: that day's time,
// moved past a gap at midnight, may still be to come (round 3). Eight days
// ahead always reach the next of the days, even once today's time has
// passed.
func nextClock(after time.Time, at Clock, days []time.Weekday, zone *time.Location) time.Time {
	y, m, d := after.In(zone).Date()
	var moment time.Time
	for ahead := -1; ahead <= 8; ahead++ {
		day := time.Date(y, m, d+ahead, 12, 0, 0, 0, time.UTC).Weekday()
		moment = Moment(y, m, d+ahead, at.Hour, at.Minute, zone)
		if moment.After(after) && (days == nil || slices.Contains(days, day)) {
			return moment
		}
	}
	// Unreachable, since every day of the week comes within eight days: the
	// last day looked at, rather than a panic that would stop pocketd.
	return moment
}

// Moment is the moment the wall clock in zone shows a date and time (days
// past a month's end roll over). A time that comes twice, as clocks go
// back, is the first one; a time skipped as clocks go forward is the moment
// after the gap, so 2:30 on that night is 3:30.
func Moment(year int, month time.Month, day, hour, minute int, zone *time.Location) time.Time {
	wanted := time.Date(year, month, day, hour, minute, 0, 0, time.UTC)
	offset := func(t time.Time) time.Duration {
		_, seconds := t.In(zone).Zone()
		return time.Duration(seconds) * time.Second
	}
	// Offsets run from -12 to +14 hours, so the moment lies between 14 hours
	// before the wall time read as UTC and 12 after. The zone's offsets just
	// outside that span, before and after it, are those of either side of
	// any daylight saving change within it, however far the zone is from
	// UTC (Codex review of #51, round 1): the same, unless a change is near.
	candidates := []time.Time{
		wanted.Add(-offset(wanted.Add(-15 * time.Hour))),
		wanted.Add(-offset(wanted.Add(13 * time.Hour))),
	}
	if candidates[1].Before(candidates[0]) {
		candidates[0], candidates[1] = candidates[1], candidates[0]
	}
	for _, c := range candidates {
		local := c.In(zone)
		if time.Date(local.Year(), local.Month(), local.Day(), local.Hour(), local.Minute(), 0, 0, time.UTC).Equal(wanted) {
			return c
		}
	}
	return candidates[1]
}

// String says how a repeat goes out: "every 2 hours", "every day at
// 19:00", "weekdays at 08:00", "mon, thu at 09:00".
func (r Repeat) String() string {
	if r.Every > 0 {
		minutes := int(r.Every / time.Minute)
		switch {
		case minutes%1440 == 0 && minutes == 1440:
			return "every day"
		case minutes%1440 == 0:
			return fmt.Sprintf("every %d days", minutes/1440)
		case minutes == 60:
			return "every hour"
		case minutes%60 == 0:
			return fmt.Sprintf("every %d hours", minutes/60)
		}
		return fmt.Sprintf("every %d minutes", minutes)
	}
	switch {
	case r.Days == nil:
		return "every day at " + r.At.String()
	case slices.Equal(r.Days, weekdays):
		return "weekdays at " + r.At.String()
	case slices.Equal(r.Days, weekend):
		return "weekends at " + r.At.String()
	}
	names := make([]string, len(r.Days))
	for i, d := range r.Days {
		names[i] = dayNames[d][:3]
	}
	return strings.Join(names, ", ") + " at " + r.At.String()
}

// Zone is the time zone a schedule's clock times are in: name, an IANA
// zone, or service, the service's own, for "".
func Zone(name string, service *time.Location) (*time.Location, error) {
	if name == "" {
		return service, nil
	}
	// Go reads "Local" as the host's zone, which a restart may change: a
	// zone named is an IANA zone, and the service's is named by none (Codex
	// review of #51, round 2).
	if name == "Local" {
		return nil, errors.New(`the zone "Local" is no IANA time zone: leave the zone out for the service's`)
	}
	zone, err := time.LoadLocation(name)
	if err != nil {
		return nil, fmt.Errorf("the zone %q is no time zone pocketd knows", name)
	}
	return zone, nil
}

// When formats t as people read it in zone: "Fri 20 Nov 19:00 CET".
func When(t time.Time, zone *time.Location) string {
	return t.In(zone).Format("Mon 2 Jan 15:04 MST")
}
