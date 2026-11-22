package state

import (
	"slices"
	"testing"
	"time"

	"github.com/frostyard/pocketgo/proto"
)

// A night shift's section holds something with an entry, a failed fetch
// or more it says; empty or still loading, it holds nothing.
func TestANightShiftSectionHolds(t *testing.T) {
	for _, tc := range []struct {
		name    string
		entries int
		status  ListStatus
		more    bool
		want    bool
	}{
		{"empty", 0, ListLoaded, false, false},
		{"loading", 0, ListLoading, false, false},
		{"an entry", 1, ListLoaded, false, true},
		{"a failed fetch", 0, ListFailed, false, true},
		{"more it says", 0, ListLoaded, true, true},
	} {
		if got := NightShiftHolds(tc.entries, tc.status, tc.more); got != tc.want {
			t.Errorf("%s: NightShiftHolds = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// The Proposals section says more than its proposals while the owner
// waits on a launch: one whose answer was lost, one in flight, its
// refusal, or the launch it confirms until its end (Codex review of #90,
// round 1).
func TestWhatTheProposalsSay(t *testing.T) {
	now := time.Date(2026, 11, 21, 22, 0, 0, 0, time.UTC)
	running := &proto.Launch{ID: 1, End: now.Add(time.Hour)}
	ended := &proto.Launch{ID: 1, End: now.Add(-time.Minute)}
	for _, tc := range []struct {
		name      string
		uncertain bool
		launching Pending
		launched  *proto.Launch
		want      bool
	}{
		{"nothing", false, Pending{}, nil, false},
		{"a lost answer", true, Pending{}, nil, true},
		{"a launch in flight", false, Pending{Busy: true}, nil, true},
		{"a refusal", false, Pending{Err: "pocketd refused the launch"}, nil, true},
		{"a launch until its end", false, Pending{}, running, true},
		{"a launch past its end", false, Pending{}, ended, false},
	} {
		if got := ProposalsSay(tc.uncertain, tc.launching, tc.launched, now); got != tc.want {
			t.Errorf("%s: ProposalsSay = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// A desktop's board draws the night shift, then the threads, whatever it
// holds; a compact window's puts a section that holds nothing after the
// threads, and keeps one that holds something above them (the owner's
// choice of 2026-11-21).
func TestTheBoardsOrder(t *testing.T) {
	p, s, th := PartProposals, PartSchedules, PartThreads
	for _, tc := range []struct {
		compact, proposals, schedules bool
		want                          []BoardPart
	}{
		{false, false, false, []BoardPart{p, s, th}},
		{false, true, false, []BoardPart{p, s, th}},
		{false, true, true, []BoardPart{p, s, th}},
		{true, false, false, []BoardPart{th, p, s}},
		{true, true, false, []BoardPart{p, th, s}},
		{true, false, true, []BoardPart{s, th, p}},
		{true, true, true, []BoardPart{p, s, th}},
	} {
		if got := BoardOrder(tc.compact, tc.proposals, tc.schedules); !slices.Equal(got, tc.want) {
			t.Errorf("BoardOrder(%v, %v, %v) = %v, want %v", tc.compact, tc.proposals, tc.schedules, got, tc.want)
		}
	}
}

// The compact window's tabs are the Inbox, the work board, the sessions
// and More, in that order, each with its own place; every other place
// belongs to none (ADR-0044).
func TestEachTabHasItsPlace(t *testing.T) {
	want := []struct {
		tab          Tab
		title, place string
	}{
		{TabInbox, "Inbox", PlaceInbox},
		{TabBoard, "Board", PlaceBoard},
		{TabSessions, "Sessions", PlaceSessions},
		{TabMore, "More", PlaceMore},
	}
	if len(Tabs) != len(want) {
		t.Fatalf("tabs %v", Tabs)
	}
	for i, w := range want {
		tab := Tabs[i]
		if tab != w.tab || tab.Title() != w.title || tab.Place() != w.place {
			t.Errorf("tab %d is %v %q at %q, want %v %q at %q", i, tab, tab.Title(), tab.Place(), w.tab, w.title, w.place)
		}
		if got, ok := TabOf(w.place); !ok || got != w.tab {
			t.Errorf("TabOf(%q) = %v, %v", w.place, got, ok)
		}
	}
	for _, place := range []string{PlaceTerminal, "", "session"} {
		if got, ok := TabOf(place); ok {
			t.Errorf("TabOf(%q) = %v, want none", place, got)
		}
	}
}

// Back from a session goes to the own view of the tab it shows over,
// whatever showed before it; the bar of tabs shows only over a tab's own
// view.
func TestBackGoesToTheTabsOwnView(t *testing.T) {
	for _, tab := range Tabs {
		if got := BackTo(tab); got != tab.Place() {
			t.Errorf("BackTo(%v) = %q, want %q", tab, got, tab.Place())
		}
	}
	if !TabBarShows(false) || TabBarShows(true) {
		t.Errorf("the bar shows over a tab's view %v, over a session %v", TabBarShows(false), TabBarShows(true))
	}
}

// The Inbox tab's badge counts what waits in every list, up to 99+; it
// warns when nothing is counted and a list failed, and is empty when
// nothing waits or nothing loaded yet.
func TestTheInboxBadge(t *testing.T) {
	kinds := func(counts []int, statuses ...ListStatus) []InboxKind {
		out := make([]InboxKind, len(counts))
		for i, n := range counts {
			out[i] = InboxKind{List: "list", Count: n, Status: ListLoaded}
			if i < len(statuses) {
				out[i].Status = statuses[i]
			}
		}
		return out
	}
	for _, tc := range []struct {
		name  string
		kinds []InboxKind
		want  string
	}{
		{"nothing waits", kinds([]int{0, 0, 0}), ""},
		{"nothing loaded yet", kinds([]int{0, 0}, ListLoading, ListLoading), ""},
		{"counted across the lists", kinds([]int{1, 0, 2}), "3"},
		{"a failed list beside a count", kinds([]int{0, 2}, ListFailed), "2"},
		{"a failed list and nothing counted", kinds([]int{0, 0}, ListLoaded, ListFailed), "!"},
		{"at the most", kinds([]int{InboxBadgeMost}), "99"},
		{"past the most", kinds([]int{60, 40}), "99+"},
	} {
		if got := InboxBadge(tc.kinds); got != tc.want {
			t.Errorf("%s: InboxBadge = %q, want %q", tc.name, got, tc.want)
		}
	}
}
