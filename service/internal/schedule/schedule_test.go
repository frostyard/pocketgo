package schedule

import (
	"slices"
	"strings"
	"testing"
	"time"
)

// sunday is Sunday, October 4, 2026, 10:00 in Chicago (15:00 UTC).
var sunday = time.Date(2026, 10, 4, 15, 0, 0, 0, time.UTC)

func chicago(t *testing.T) *time.Location {
	t.Helper()
	zone, err := Zone("America/Chicago", time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	return zone
}

// Times read the way people write them, in their time zone, as pi-pocket
// reads them (ADR-0030).
func TestParseReadsTimesAsPeopleWriteThem(t *testing.T) {
	zone := chicago(t)
	for text, want := range map[string]struct {
		first  string
		repeat string
	}{
		"in 30m":                {"2026-10-04T15:30:00Z", ""},
		"in 2 hours":            {"2026-10-04T17:00:00Z", ""},
		"in 1.5h":               {"2026-10-04T16:30:00Z", ""},
		"7:00":                  {"2026-10-05T12:00:00Z", ""}, // passed today: tomorrow
		"7pm":                   {"2026-10-05T00:00:00Z", ""},
		"10:30am":               {"2026-10-04T15:30:00Z", ""},
		"tomorrow 9am":          {"2026-10-05T14:00:00Z", ""},
		"today 18:00":           {"2026-10-04T23:00:00Z", ""},
		"fri 17:30":             {"2026-10-09T22:30:00Z", ""},
		"Friday 17:30":          {"2026-10-09T22:30:00Z", ""},
		"every 2h":              {"2026-10-04T17:00:00Z", "every 2 hours"},
		"every 10 minutes":      {"2026-10-04T15:10:00Z", "every 10 minutes"},
		"every 2 days":          {"2026-10-06T15:00:00Z", "every 2 days"},
		"every day 19:00":       {"2026-10-05T00:00:00Z", "every day at 19:00"},
		"every daily 9:00":      {"2026-10-05T14:00:00Z", "every day at 09:00"},
		"every 8:00":            {"2026-10-05T13:00:00Z", "every day at 08:00"},
		"every weekday 8:00":    {"2026-10-05T13:00:00Z", "weekdays at 08:00"},
		"every weekends 10:00":  {"2026-10-10T15:00:00Z", "weekends at 10:00"},
		"every mon,thu 9:00":    {"2026-10-05T14:00:00Z", "mon, thu at 09:00"},
		"every thurs,tue 21:15": {"2026-10-07T02:15:00Z", "tue, thu at 21:15"},
		"  EVERY   Fri  7PM ":   {"2026-10-10T00:00:00Z", "fri at 19:00"},
	} {
		spec, err := Parse(text)
		if err != nil {
			t.Errorf("%q: %v", text, err)
			continue
		}
		first, err := spec.First(sunday, zone)
		if err != nil {
			t.Errorf("%q: %v", text, err)
			continue
		}
		repeat := ""
		if spec.Repeat != nil {
			repeat = spec.Repeat.String()
		}
		if got := first.UTC().Format(time.RFC3339); got != want.first || repeat != want.repeat || spec.Once() != (want.repeat == "") {
			t.Errorf("%q: first %s, %q; want %s, %q", text, got, repeat, want.first, want.repeat)
		}
	}
}

func TestParseRefusesWhatIsNoTime(t *testing.T) {
	for text, want := range map[string]string{
		"":                  "say when",
		"soon":              "say when",
		"every 5m":          "at most every 10 minutes",
		"every 9 minutes":   "at most every 10 minutes",
		"every 2 days 8:00": "not both",
		"every 2 days at 8": "not both",
		"in 400d":           "at most 366 days ahead",
		"in 0m":             "say when",
		"7":                 "say when",
		"25:00":             "say when",
		"13pm":              "say when",
		"7:60":              "say when",
		"every fr 9:00":     "say when",
		"in 30m check it":   `"check it" is not part of a time`,
		"every day 19:00 x": `"x" is not part of a time`,
		"tomorrow":          "say when",
		"every":             "say when",
		"in":                "say when",
		"in 1e3m":           "say when",
		"every NaN hours":   "say when",
	} {
		if _, err := Parse(text); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: %v, want an error with %q", text, err, want)
		}
	}
	// A today time that has passed goes nowhere.
	spec, err := Parse("today 9:00")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := spec.First(sunday, chicago(t)); err == nil || !strings.Contains(err.Error(), "09:00 today has passed") {
		t.Errorf("today 9:00 at 10:00: %v", err)
	}
}

// Clock times hold across daylight saving changes: a skipped time is the
// moment after the gap, a time that comes twice is the first, and a repeat
// stays at its wall-clock time (ADR-0030).
func TestClockTimesHoldAcrossDaylightSaving(t *testing.T) {
	zone := chicago(t)
	iso := func(t time.Time) string { return t.UTC().Format(time.RFC3339) }
	if got := iso(Moment(2026, time.March, 8, 2, 30, zone)); got != "2026-03-08T08:30:00Z" {
		t.Errorf("a skipped 2:30 is %s, want 3:30 CDT", got)
	}
	if got := iso(Moment(2026, time.November, 1, 1, 30, zone)); got != "2026-11-01T06:30:00Z" {
		t.Errorf("a 1:30 that comes twice is %s, want the first", got)
	}
	// Every day at 8:00, from the Saturday before clocks go back.
	daily := Repeat{At: Clock{8, 0}}
	saturday := daily.Next(time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC), zone)
	sundayAfter := daily.Next(saturday, zone)
	if iso(saturday) != "2026-10-31T13:00:00Z" || iso(sundayAfter) != "2026-11-01T14:00:00Z" {
		t.Errorf("8:00 across the change: %s, %s", iso(saturday), iso(sundayAfter))
	}
	// Every day at 1:30 goes out once on the night 1:30 comes twice.
	night := Repeat{At: Clock{1, 30}}
	first := night.Next(time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC), zone)
	second := night.Next(first, zone)
	if iso(first) != "2026-11-01T06:30:00Z" || iso(second) != "2026-11-02T07:30:00Z" {
		t.Errorf("1:30 across the change: %s, %s", iso(first), iso(second))
	}
	// Every day at 2:30 goes out at 3:30 on the night it is skipped, then
	// at 2:30 again.
	skipped := Repeat{At: Clock{2, 30}}
	gap := skipped.Next(time.Date(2026, 3, 7, 12, 0, 0, 0, time.UTC), zone)
	after := skipped.Next(gap, zone)
	if iso(gap) != "2026-03-08T08:30:00Z" || iso(after) != "2026-03-09T07:30:00Z" {
		t.Errorf("2:30 across the gap: %s, %s", iso(gap), iso(after))
	}
	// A repeat after a span keeps its beat from the time due, however late
	// the pass that sends it; one at a clock time goes on from now. Neither
	// sends a time before now.
	every := Repeat{Every: 2 * time.Hour}
	due := time.Date(2026, 10, 4, 15, 0, 0, 0, time.UTC)
	for late, want := range map[time.Duration]string{
		0:                             "2026-10-04T17:00:00Z",
		30 * time.Second:              "2026-10-04T17:00:00Z",
		2 * time.Hour:                 "2026-10-04T19:00:00Z",
		72*time.Hour + 30*time.Minute: "2026-10-07T17:00:00Z",
	} {
		if got := iso(every.After(due, due.Add(late), zone)); got != want {
			t.Errorf("every 2h, %v late: %s, want %s", late, got, want)
		}
	}
	if got := iso(daily.After(due, time.Date(2026, 10, 7, 14, 0, 0, 0, time.UTC), zone)); got != "2026-10-08T13:00:00Z" {
		t.Errorf("8:00 after a gap: %s", got)
	}
	// A weekday repeat reaches Monday from a Friday evening.
	weekday := Repeat{At: Clock{8, 0}, Days: slices.Clone(weekdays)}
	if got := iso(weekday.Next(time.Date(2026, 10, 9, 23, 0, 0, 0, time.UTC), zone)); got != "2026-10-12T13:00:00Z" {
		t.Errorf("weekdays at 8:00 after Friday: %s", got)
	}
}

// The daylight saving rule holds in a zone far from UTC, and in one whose
// clocks move half an hour (Codex review of #51, round 1).
func TestClockTimesHoldFarFromUTC(t *testing.T) {
	iso := func(t time.Time) string { return t.UTC().Format(time.RFC3339) }
	for name, tc := range map[string]struct {
		zone       string
		y          int
		m          time.Month
		d, h, min  int
		want, says string
	}{
		"a skipped 2:30 in Auckland":       {"Pacific/Auckland", 2026, time.September, 27, 2, 30, "2026-09-26T14:30:00Z", "03:30 NZDT"},
		"a doubled 2:30 in Auckland":       {"Pacific/Auckland", 2026, time.April, 5, 2, 30, "2026-04-04T13:30:00Z", "02:30 NZDT"},
		"a skipped 2:15 on Lord Howe":      {"Australia/Lord_Howe", 2026, time.October, 4, 2, 15, "2026-10-03T15:45:00Z", "02:45 +11"},
		"a doubled 1:45 on Lord Howe":      {"Australia/Lord_Howe", 2026, time.April, 5, 1, 45, "2026-04-04T14:45:00Z", "01:45 +11"},
		"an ordinary time in Kiritimati":   {"Pacific/Kiritimati", 2026, time.March, 1, 9, 0, "2026-02-28T19:00:00Z", "09:00 +14"},
		"a skipped 2:30 in Chicago, still": {"America/Chicago", 2026, time.March, 8, 2, 30, "2026-03-08T08:30:00Z", "03:30 CDT"},
	} {
		zone, err := Zone(tc.zone, time.UTC)
		if err != nil {
			t.Fatal(err)
		}
		got := Moment(tc.y, tc.m, tc.d, tc.h, tc.min, zone)
		if iso(got) != tc.want || got.In(zone).Format("15:04 MST") != tc.says {
			t.Errorf("%s: %s (%s), want %s (%s)", name, iso(got), got.In(zone).Format("15:04 MST"), tc.want, tc.says)
		}
	}
	// Every quarter hour of 2026, in zones on both sides of UTC: the moment
	// shows the time asked for, at its first coming, or, in a gap, the time
	// asked for moved on by the gap, as 2:30 is 3:30.
	offset := func(t time.Time, zone *time.Location) time.Duration {
		_, seconds := t.In(zone).Zone()
		return time.Duration(seconds) * time.Second
	}
	wall := func(t time.Time, zone *time.Location) time.Time {
		l := t.In(zone)
		return time.Date(l.Year(), l.Month(), l.Day(), l.Hour(), l.Minute(), 0, 0, time.UTC)
	}
	for _, name := range []string{"Pacific/Auckland", "Australia/Lord_Howe", "Pacific/Chatham", "America/Chicago", "America/Santiago", "Europe/Berlin", "Asia/Tehran", "Pacific/Kiritimati", "Pacific/Pago_Pago"} {
		zone, err := Zone(name, time.UTC)
		if err != nil {
			t.Fatal(err)
		}
		for at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC); at.Year() == 2026; at = at.Add(15 * time.Minute) {
			got := Moment(at.Year(), at.Month(), at.Day(), at.Hour(), at.Minute(), zone)
			switch shown := wall(got, zone); {
			case shown.Equal(at):
				if wall(got.Add(-time.Hour), zone).Equal(at) || wall(got.Add(-30*time.Minute), zone).Equal(at) {
					t.Errorf("%s %s: %s is its second coming", name, at.Format("2006-01-02 15:04"), iso(got))
				}
			case shown.After(at) && shown.Sub(at) == offset(got, zone)-offset(got.Add(-3*time.Hour), zone):
				// A gap: the time moved on by it.
			default:
				t.Errorf("%s %s: %s shows %s", name, at.Format("2006-01-02 15:04"), iso(got), shown.Format("2006-01-02 15:04"))
			}
		}
	}
}

func TestZone(t *testing.T) {
	service := time.FixedZone("service", 3600)
	if zone, err := Zone("", service); err != nil || zone != service {
		t.Errorf("no zone is the service's: %v %v", zone, err)
	}
	if zone, err := Zone("Europe/Berlin", service); err != nil || zone.String() != "Europe/Berlin" {
		t.Errorf("Europe/Berlin: %v %v", zone, err)
	}
	for _, name := range []string{"Mars/Olympus", "../etc/passwd", "Europe/Berlin "} {
		if _, err := Zone(name, service); err == nil {
			t.Errorf("%q is a zone", name)
		}
	}
}

func TestCheckText(t *testing.T) {
	if text, err := CheckText("  look at the deploy \n"); err != nil || text != "look at the deploy" {
		t.Errorf("%q %v", text, err)
	}
	if _, err := CheckText(" \n\t"); err == nil {
		t.Error("an empty message is taken")
	}
	if _, err := CheckText(strings.Repeat("é", MaxText)); err != nil {
		t.Errorf("%d characters: %v", MaxText, err)
	}
	if _, err := CheckText(strings.Repeat("é", MaxText+1)); err == nil {
		t.Errorf("%d characters are taken", MaxText+1)
	}
	if _, err := CheckText("bad \xff"); err == nil {
		t.Error("text that is no UTF-8 is taken")
	}
}

func TestCheckCallAndIDs(t *testing.T) {
	for _, c := range []Call{{Action: "add", When: "in 30m", Message: "x"}, {Action: "list"}, {Action: "cancel", ID: "S3"}} {
		if err := CheckCall(c); err != nil {
			t.Errorf("%+v: %v", c, err)
		}
	}
	for _, c := range []Call{{Action: "add", When: "in 30m"}, {Action: "add", Message: "x"}, {Action: "add", When: "in 30m", Message: "x", ID: "S1"},
		{Action: "list", ID: "S1"}, {Action: "cancel"}, {Action: "cancel", ID: "S1", When: "in 30m"}, {Action: "remove"}} {
		if err := CheckCall(c); err == nil {
			t.Errorf("%+v is taken", c)
		}
	}
	for id, want := range map[string]int64{"S3": 3, "s12": 12, "7": 7} {
		if n, err := ParseID(id); err != nil || n != want {
			t.Errorf("%q: %d %v", id, n, err)
		}
	}
	for _, id := range []string{"", "S", "S0", "S-1", "S03", "T3", "S3x"} {
		if _, err := ParseID(id); err == nil {
			t.Errorf("%q is an id", id)
		}
	}
	if ID(4) != "S4" {
		t.Error(ID(4))
	}
}

// The texts pocketd writes depend only on the schedule, so a call run
// again answers as the first did.
func TestScheduleTexts(t *testing.T) {
	zone := chicago(t)
	first := time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)
	if got := AddedText("S3", "every day 19:00", first, zone); !strings.Contains(got, "Scheduled S3: every day at 19:00 (America/Chicago).") ||
		!strings.Contains(got, `"[scheduled]"`) {
		t.Errorf("added: %q", got)
	}
	if got := AddedText("S4", "in 30m", first, zone); !strings.Contains(got, "Scheduled S4: once, Sun 4 Oct 19:00 CDT.") {
		t.Errorf("added once: %q", got)
	}
	if Message("look again") != "[scheduled] look again" {
		t.Error(Message("look again"))
	}
	line := Line{ID: "evening", Owners: true, When: "every day at 19:00", Zone: zone, Next: first, Skipped: 2, Text: "gather"}
	if got := line.String(); got != "evening (the owner's): every day at 19:00, next Sun 4 Oct 19:00 CDT, skipped 2 times while its last message waited · gather" {
		t.Errorf("line: %q", got)
	}
	if got := ListText(nil); got != "Nothing is scheduled for this session." {
		t.Error(got)
	}
	if CancelledText("S3", false) != "Cancelled S3: it sends nothing more." || !strings.Contains(CancelledText("S3", true), "went out already") {
		t.Error("cancel texts")
	}
}
