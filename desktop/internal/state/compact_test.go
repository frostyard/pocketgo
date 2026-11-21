package state

import "testing"

// The compact window's tabs are the Inbox, the work board, the sessions
// and More, in that order, each with its own place; every other place
// belongs to none (ADR-0043).
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
