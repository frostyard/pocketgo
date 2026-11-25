package team

import (
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/frostyard/pocketgo/service/internal/profiles"
)

func hours(h float64) *float64 { return &h }

// An add names a pull request as owner/name#number, a known condition,
// and hours above 0 and at most MaxWatchHours, or none for the default; a
// limit given as 0 is refused (ADR-0073).
func TestCheckWatch(t *testing.T) {
	plan, err := CheckWatch(WatchCall{Action: WatchAdd, Pull: "frostyard/example#12", Until: UntilChecks})
	if err != nil || plan != (WatchPlan{Repository: "frostyard/example", Number: 12, Until: UntilChecks, Hours: DefaultWatchHours}) {
		t.Fatalf("a plain add: %+v %v", plan, err)
	}
	if plan, err := CheckWatch(WatchCall{Action: WatchAdd, Pull: " frostyard/example#3 ", Until: UntilMerged, Hours: hours(MaxWatchHours)}); err != nil || plan.Hours != MaxWatchHours || plan.Number != 3 {
		t.Fatalf("the most hours: %+v %v", plan, err)
	}
	if plan, err := CheckWatch(WatchCall{Action: WatchAdd, Pull: "a/b#1", Until: UntilClosed, Hours: hours(0.5)}); err != nil || plan.Hours != 0.5 {
		t.Fatalf("half an hour: %+v %v", plan, err)
	}
	for name, c := range map[string]WatchCall{
		"no pull request":     {Until: UntilChecks},
		"no number":           {Pull: "frostyard/example", Until: UntilChecks},
		"no repository":       {Pull: "#12", Until: UntilChecks},
		"no owner":            {Pull: "example#12", Until: UntilChecks},
		"a number of 0":       {Pull: "frostyard/example#0", Until: UntilChecks},
		"a leading zero":      {Pull: "frostyard/example#012", Until: UntilChecks},
		"a sign":              {Pull: "frostyard/example#+12", Until: UntilChecks},
		"a path":              {Pull: "frostyard/example/x#12", Until: UntilChecks},
		"a URL":               {Pull: "https://github.com/frostyard/example/pull/12", Until: UntilChecks},
		"no condition":        {Pull: "frostyard/example#12"},
		"an unknown one":      {Pull: "frostyard/example#12", Until: "green"},
		"0 hours":             {Pull: "frostyard/example#12", Until: UntilChecks, Hours: hours(0)},
		"negative hours":      {Pull: "frostyard/example#12", Until: UntilChecks, Hours: hours(-1)},
		"too many hours":      {Pull: "frostyard/example#12", Until: UntilChecks, Hours: hours(MaxWatchHours + 0.5)},
		"hours that are none": {Pull: "frostyard/example#12", Until: UntilChecks, Hours: hours(math.NaN())},
	} {
		c.Action = WatchAdd
		if plan, err := CheckWatch(c); err == nil {
			t.Errorf("%s: %+v", name, plan)
		}
	}
	at := time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC)
	if got := (WatchPlan{Hours: 1.5}).Deadline(at); !got.Equal(at.Add(90 * time.Minute)) {
		t.Fatalf("the deadline %v", got)
	}
}

// Each action takes only its own fields.
func TestCheckWatchCall(t *testing.T) {
	for name, c := range map[string]WatchCall{
		"add":    {Action: WatchAdd, Pull: "a/b#1", Until: UntilChecks, Hours: hours(2)},
		"list":   {Action: WatchList},
		"cancel": {Action: WatchCancel, ID: "W3"},
	} {
		if err := CheckWatchCall(c); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	for name, c := range map[string]WatchCall{
		"an unknown action":   {Action: "wait"},
		"no action":           {},
		"an add with an id":   {Action: WatchAdd, Pull: "a/b#1", Until: UntilChecks, ID: "W1"},
		"a list with a pull":  {Action: WatchList, Pull: "a/b#1"},
		"a list with an id":   {Action: WatchList, ID: "W1"},
		"a cancel with hours": {Action: WatchCancel, ID: "W1", Hours: hours(1)},
	} {
		if err := CheckWatchCall(c); err == nil {
			t.Errorf("%s passed", name)
		}
	}
	if n, err := ParseWatchID("W3"); err != nil || n != 3 {
		t.Fatalf("W3: %d %v", n, err)
	}
	if n, err := ParseWatchID("w12"); err != nil || n != 12 {
		t.Fatalf("w12: %d %v", n, err)
	}
	for _, id := range []string{"", "W", "WW3", "S3", "W0", "W-1", "W03", "3.0"} {
		if _, err := ParseWatchID(id); err == nil {
			t.Errorf("%q passed", id)
		}
	}
	if WatchID(7) != "W7" {
		t.Fatal(WatchID(7))
	}
}

// team_watch is every session's that gets team_memory but a shift's
// specialist: a supervisor's, a thread's specialist, a plan's lead and a
// session the owner talks in; never a review's, a worker's, a consult's,
// a gather's agent's, a private profile's or a session with no profile.
func TestMayWatch(t *testing.T) {
	agent := &profiles.Profile{ID: "vera", Role: profiles.RoleAgent}
	supervisor := &profiles.Profile{ID: "odrade", Role: profiles.RoleSupervisor}
	private := &profiles.Profile{ID: "chani", Role: profiles.RoleAgent, Private: true}
	for name, tc := range map[string]struct {
		p    *profiles.Profile
		role SessionRole
		may  bool
	}{
		"a supervisor":            {supervisor, SessionRole{}, true},
		"an owner's session":      {agent, SessionRole{}, true},
		"a thread's specialist":   {agent, SessionRole{Thread: true}, true},
		"a plan's lead":           {agent, SessionRole{Thread: true, Plan: true}, true},
		"a shift's specialist":    {agent, SessionRole{Thread: true, Shift: true}, false},
		"a review":                {agent, SessionRole{Thread: true, Review: true}, false},
		"a worker":                {nil, SessionRole{Thread: true, Worker: true}, false},
		"a consult":               {agent, SessionRole{Consult: true}, false},
		"a gather's agent":        {agent, SessionRole{Gather: true}, false},
		"a private agent":         {private, SessionRole{}, false},
		"a private supervisor":    {&profiles.Profile{ID: "leto", Role: profiles.RoleSupervisor, Private: true}, SessionRole{}, false},
		"a session of no profile": {nil, SessionRole{}, false},
	} {
		if got := MayWatch(tc.p, tc.role); got != tc.may {
			t.Errorf("%s: %v", name, got)
		}
		if err := CheckWatchCaller(tc.p, tc.role); (err == nil) != tc.may {
			t.Errorf("%s: the caller check %v", name, err)
		}
	}
	if err := CheckWatchCaller(agent, SessionRole{Thread: true, Shift: true}); err == nil || !strings.Contains(err.Error(), "shift") {
		t.Fatalf("a shift's refusal %v", err)
	}
}

// A watch ends on what pocketd read, in code: checks once CI at the head
// is anything but pending, or as unreachable once the pull request is not
// open; merged on a merge, unreachable on a close without one; closed on
// either. Anything else waits, a CI pocketd could not read included.
func TestWatchDecide(t *testing.T) {
	open := WatchSeen{Open: true}
	pending := WatchSeen{Open: true, CIRead: true, CI: "pending"}
	for name, tc := range map[string]struct {
		until string
		seen  WatchSeen
		want  string
	}{
		"checks pending":             {UntilChecks, pending, ""},
		"checks unread":              {UntilChecks, open, ""},
		"checks green":               {UntilChecks, WatchSeen{Open: true, CIRead: true, CI: "green", CIDone: true}, WatchMet},
		"checks failing":             {UntilChecks, WatchSeen{Open: true, CIRead: true, CI: "failing", CIDone: true}, WatchMet},
		"checks none":                {UntilChecks, WatchSeen{Open: true, CIRead: true, CI: "none", CIDone: true}, WatchMet},
		"checks on a merge":          {UntilChecks, WatchSeen{Merged: true}, WatchUnreachable},
		"checks on a close":          {UntilChecks, WatchSeen{}, WatchUnreachable},
		"merged while open":          {UntilMerged, pending, ""},
		"merged on a merge":          {UntilMerged, WatchSeen{Merged: true}, WatchMet},
		"merged on a close":          {UntilMerged, WatchSeen{}, WatchUnreachable},
		"closed while open":          {UntilClosed, open, ""},
		"closed on a merge":          {UntilClosed, WatchSeen{Merged: true}, WatchMet},
		"closed on a close":          {UntilClosed, WatchSeen{}, WatchMet},
		"an unknown condition waits": {"green", WatchSeen{Merged: true}, ""},
	} {
		if got := WatchDecide(tc.until, tc.seen); got != tc.want {
			t.Errorf("%s: %q, want %q", name, got, tc.want)
		}
	}
	if !WatchNeedsCI(UntilChecks, true) || WatchNeedsCI(UntilChecks, false) || WatchNeedsCI(UntilMerged, true) || WatchNeedsCI(UntilClosed, true) {
		t.Fatal("only checks reads CI, on an open pull request")
	}
}

// The texts name the watch, the pull request, what it waited for and what
// pocketd saw: CI with the failing checks' names, the state, or how long
// it waited and why it could not read at the end.
func TestWatchTexts(t *testing.T) {
	deadline := time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC)
	added := WatchAddedText(3, "frostyard/example#12", UntilChecks, 24, deadline, 5*time.Minute)
	for _, want := range []string{"Watching W3: frostyard/example#12 until its checks are done", "24 hours", "2026-11-25 10:00 UTC", "every 5m", "End your run now"} {
		if !strings.Contains(added, want) {
			t.Errorf("added lacks %q: %s", want, added)
		}
	}
	if got := WatchAddedText(3, "a/b#1", UntilMerged, 1, deadline, 0); !strings.Contains(got, "1 hour,") || !strings.Contains(got, "until it merges") {
		t.Errorf("one hour: %s", got)
	}
	failing := WatchSeen{Open: true, Title: "Fix CI", Head: strings.Repeat("a", 40), CIRead: true, CI: "failing", CIDone: true, Failing: []string{"lint", "verify"}}
	note := WatchNoteText(3, "frostyard/example#12", UntilChecks, WatchMet, &failing, "", time.Hour)
	if note != "[W3 · frostyard/example#12 · checks done] frostyard/example#12 (Fix CI): CI at aaaaaaaaaaaa failing: lint, verify. "+
		"The watch ended; add another with team_watch to go on waiting." {
		t.Errorf("checks done: %s", note)
	}
	none := WatchSeen{Open: true, Head: strings.Repeat("b", 40), CIRead: true, CI: "none", CIDone: true}
	if got := WatchNoteText(1, "a/b#1", UntilChecks, WatchMet, &none, "", time.Hour); !strings.Contains(got, "CI at bbbbbbbbbbbb ran nothing.") {
		t.Errorf("no CI: %s", got)
	}
	merged := WatchSeen{Merged: true, Title: "Fix CI"}
	for name, tc := range map[string]struct {
		until, outcome string
		seen           WatchSeen
		want           []string
	}{
		"merged":                 {UntilMerged, WatchMet, merged, []string{"[W3 · a/b#1 · merged]", "It merged."}},
		"closed by a merge":      {UntilClosed, WatchMet, merged, []string{"[W3 · a/b#1 · closed]", "It merged."}},
		"closed without a merge": {UntilClosed, WatchMet, WatchSeen{}, []string{"[W3 · a/b#1 · closed]", "closed without merging."}},
		"never merges":           {UntilMerged, WatchUnreachable, WatchSeen{}, []string{"[W3 · a/b#1 · closed without merging]", "unless someone reopens it"}},
		"checks after a merge":   {UntilChecks, WatchUnreachable, merged, []string{"[W3 · a/b#1 · merged]", "no checks to wait for"}},
		"still pending":          {UntilChecks, WatchExpired, WatchSeen{Open: true, Head: strings.Repeat("c", 40), CIRead: true, CI: "pending"}, []string{"time is up", "watched it for 6h for its checks to be done", "still open, CI at cccccccccccc pending"}},
	} {
		got := WatchNoteText(3, "a/b#1", tc.until, tc.outcome, &tc.seen, "", 6*time.Hour+10*time.Second)
		for _, want := range tc.want {
			if !strings.Contains(got, want) {
				t.Errorf("%s lacks %q: %s", name, want, got)
			}
		}
	}
	if got := WatchNoteText(3, "a/b#1", UntilMerged, WatchExpired, nil, "gh: HTTP 502", 2*time.Hour); !strings.Contains(got, "could not read it at the end: gh: HTTP 502.") {
		t.Errorf("an unread end: %s", got)
	}
	if got := WatchNoteText(3, "a/b#1", UntilMerged, WatchUnchecked, nil, "", 0); !strings.Contains(got, "-pr-every 0") {
		t.Errorf("unchecked: %s", got)
	}
	if got := WatchAtOnceText("a/b#1", UntilChecks, WatchMet, WatchSeen{Open: true, Head: strings.Repeat("d", 40), CIRead: true, CI: "green", CIDone: true}); !strings.Contains(got, "is there already: CI at dddddddddddd green") || !strings.Contains(got, "no message follows") {
		t.Errorf("at once: %s", got)
	}
	if got := WatchAtOnceText("a/b#1", UntilMerged, WatchUnreachable, WatchSeen{}); !strings.Contains(got, "a/b#1 was closed without merging") {
		t.Errorf("never at once: %s", got)
	}
	list := WatchListText([]WatchLine{{ID: 2, Ref: "a/b#1", Until: UntilChecks, Live: true, Deadline: deadline},
		{ID: 1, Ref: "a/b#9", Until: UntilMerged, Outcome: WatchThreadClosed, Ended: deadline}})
	for _, want := range []string{"W2 a/b#1 until its checks are done: watching, until 2026-11-25 10:00 UTC", "W1 a/b#9 until it merges: ended 2026-11-25 10:00 UTC, its thread closed"} {
		if !strings.Contains(list, want) {
			t.Errorf("the list lacks %q: %s", want, list)
		}
	}
	if WatchListText(nil) != "This session has no watches." {
		t.Error(WatchListText(nil))
	}
	if got := WatchCancelledText(2, false, WatchMet); got != "W2 had ended already: the condition held." {
		t.Error(got)
	}
	for _, guide := range []string{WatchGuide, WatchRoster} {
		if !strings.Contains(guide, "team_watch") {
			t.Errorf("a guide does not name the tool: %s", guide)
		}
	}
	if !strings.Contains(WatchGuide, "until checks") || !strings.Contains(WatchRoster, "until merged") {
		t.Error("the guides name the wrong conditions")
	}
	// Only the live watch's answer promises a message: the guides say to
	// end the run on "Watching W…" alone, and an answer at once says no
	// message follows (Codex review of #173, round 2).
	for _, guide := range []string{WatchGuide, WatchRoster} {
		if !strings.Contains(guide, "Watching") || !strings.Contains(guide, "act on it now") {
			t.Errorf("a guide does not tell the live answer from the others: %s", guide)
		}
	}
	if !strings.HasPrefix(added, "Watching W") || strings.HasPrefix(WatchAtOnceText("a/b#1", UntilMerged, WatchMet, merged), "Watching") {
		t.Error("the live answer and the answer at once are not told apart by their start")
	}
	// CI pocketd could not read says why, bounded, in what it saw.
	unread := WatchSeen{Open: true, CIError: "read the checks: gh: HTTP 403: Resource not accessible. " + strings.Repeat("x", 500)}
	if got := WatchSeenText(unread); !strings.HasPrefix(got, "open, CI unread: read the checks: gh: HTTP 403") || len(got) > 260 {
		t.Errorf("unread CI: %q", got)
	}
	if got := WatchNoteText(3, "a/b#1", UntilChecks, WatchExpired, &unread, "", time.Hour); !strings.Contains(got, "it is still open, CI unread: read the checks: gh: HTTP 403") {
		t.Errorf("an expiry with unread CI: %s", got)
	}
}

// The bounds are in order, and the reads' budget stays under a third of
// GitHub's 5,000 requests an hour, however many pages CI has (Codex review
// of #173, round 2).
func TestWatchBounds(t *testing.T) {
	if DefaultWatchHours <= 0 || DefaultWatchHours > MaxWatchHours || MaxWatchesPerSession > MaxWatches || MaxWatchesListed <= 0 {
		t.Fatal("the bounds are out of order")
	}
	if WatchReadBudget != WatchReadsBurst+WatchReadsPerHour || WatchReadBudget*3 > 5000 {
		t.Fatalf("a budget of %d an hour", WatchReadBudget)
	}
}

// The reads' budget spends at most its burst at once, refills at its rate
// by the clock, and spends no more than WatchReadBudget in any hour,
// whatever the passes ask; a clock that goes back refills nothing (Codex
// review of #173, round 2).
func TestReadBudget(t *testing.T) {
	start := time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC)
	var b ReadBudget
	// Has spends nothing (Codex review of #173, round 3).
	for range 2 {
		if !b.Has(start, WatchReadsBurst) {
			t.Fatal("Has spent the budget")
		}
	}
	if b.Has(start, WatchReadsBurst+1) {
		t.Fatal("Has found more than the burst")
	}
	n := 0
	for b.Take(start) {
		n++
	}
	if b.Has(start, 1) {
		t.Fatal("an empty budget had a request")
	}
	if n != WatchReadsBurst {
		t.Fatalf("a burst of %d", n)
	}
	if b.Take(start.Add(-time.Hour)) {
		t.Fatal("a clock that went back refilled the budget")
	}
	// Every second for three hours, as many as it gives: each hour's
	// spending stays within the budget.
	spent := map[int]int{}
	at := start
	for i := 0; i < 3*3600; i++ {
		at = at.Add(time.Second)
		for b.Take(at) {
			spent[i/3600]++
		}
	}
	for hour, n := range spent {
		if n > WatchReadBudget || n < WatchReadsPerHour-1 {
			t.Errorf("hour %d spent %d", hour, n)
		}
	}
	// Any hour, not only those on the hour: the first hour with its burst.
	if n+spent[0] > WatchReadBudget+1 {
		t.Errorf("the first hour spent %d", n+spent[0])
	}
	// An empty budget holds a whole read of 21 requests after 63 seconds.
	if b.Has(at.Add(62*time.Second), 21) || !b.Has(at.Add(64*time.Second), 21) {
		t.Error("the budget did not build up for a whole read")
	}
}

// The specialist's brief and the supervisor's roster carry the guides,
// which name the live answer by its exact start: an answer at once about
// a repository named Watching starts with that word too (Codex review of
// #173, round 4).
func TestTheRosterNamesTeamWatch(t *testing.T) {
	roster := Roster(nil, func(profiles.Profile) error { return nil }, 3)
	if !strings.Contains(roster, WatchRoster) {
		t.Fatalf("the roster:\n%s", roster)
	}
	for name, text := range map[string]string{"the roster": roster, "the brief's guide": WatchGuide} {
		if !strings.Contains(text, `Only an answer that starts with "Watching W" promises a message`) {
			t.Errorf("%s does not name the live answer's exact start:\n%s", name, text)
		}
	}
	// The follow-up waits for a merge alone, never a close or an expiry
	// (Codex review of #173, round 5).
	if !strings.Contains(roster, "delegate the follow-up only when pocketd says it merged, never on a close or an expiry") {
		t.Errorf("the roster ties the follow-up to more than a merge:\n%s", roster)
	}
	if at := WatchAtOnceText("Watching/example#1", UntilMerged, WatchMet, WatchSeen{Merged: true}); strings.HasPrefix(at, "Watching W") {
		t.Fatalf("an answer at once starts as a live one: %s", at)
	}
}

// The failing checks' names a watch's text lists are bounded all
// together, say how many were left out, and leave the outcome and the
// line that ends the text whole, in an answer at once and in a note
// (Codex review of #173, round 4).
func TestAWatchsTextBoundsItsFailingChecks(t *testing.T) {
	failing := make([]string, 600)
	for i := range failing {
		failing[i] = fmt.Sprintf("job-%03d-", i) + strings.Repeat("x", 120)
	}
	seen := WatchSeen{Open: true, Title: "Fix CI", Head: strings.Repeat("a", 40), CIRead: true, CI: "failing", CIDone: true, Failing: failing}
	listed := failingText(failing)
	if len(listed) > MaxWatchFailingBytes+len(" and 600 more") || !strings.HasPrefix(listed, "job-000-") {
		t.Fatalf("the list has %d bytes: %.200s", len(listed), listed)
	}
	shown := strings.Count(listed, "job-")
	if want := fmt.Sprintf(" and %d more", 600-shown); !strings.HasSuffix(listed, want) {
		t.Fatalf("the list does not end with %q: …%s", want, listed[len(listed)-60:])
	}
	at := WatchAtOnceText("a/b#1", UntilChecks, WatchMet, seen)
	note := WatchNoteText(3, "a/b#1", UntilChecks, WatchMet, &seen, "", time.Hour)
	for name, tc := range map[string]struct{ text, end string }{
		"an answer at once": {at, "No watch was kept, and no message follows."},
		"a note":            {note, "The watch ended; add another with team_watch to go on waiting."},
	} {
		if len(tc.text) > 2*MaxWatchFailingBytes || !strings.HasSuffix(tc.text, tc.end) || !strings.Contains(tc.text, "failing: job-000-") {
			t.Errorf("%s has %d bytes: %.120s … %s", name, len(tc.text), tc.text, tc.text[max(0, len(tc.text)-120):])
		}
	}
	if got := failingText([]string{"lint", "verify"}); got != "lint, verify" {
		t.Errorf("a short list %q", got)
	}
}
