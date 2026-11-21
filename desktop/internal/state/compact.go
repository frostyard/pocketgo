package state

import "strconv"

// A compact window (Compact), an iPhone's, goes between its places by a
// bar of tabs at the bottom (ADR-0043): the Inbox, with what waits there,
// the work board, the sessions and More. A tab shows its own view; a
// session shows in its place, with a back button to it and no bar.

// The window's places, as the sidebar and the compact window's tabs name
// them.
const (
	PlaceInbox    = "inbox"
	PlaceBoard    = "board"
	PlaceTerminal = "terminal"
	// PlaceSessions and PlaceMore are a compact window's own: the
	// sidebar's sessions, and what a desktop's main menu and status bar
	// hold.
	PlaceSessions = "sessions"
	PlaceMore     = "more"
)

// Tab is a tab of a compact window's bar.
type Tab int

const (
	TabInbox Tab = iota
	TabBoard
	TabSessions
	TabMore
)

// Tabs are the bar's tabs, in its order.
var Tabs = []Tab{TabInbox, TabBoard, TabSessions, TabMore}

// Title names the tab, on its button and over its own view.
func (t Tab) Title() string {
	switch t {
	case TabBoard:
		return "Board"
	case TabSessions:
		return "Sessions"
	case TabMore:
		return "More"
	}
	return "Inbox"
}

// Place is the place the tab's own view shows.
func (t Tab) Place() string {
	switch t {
	case TabBoard:
		return PlaceBoard
	case TabSessions:
		return PlaceSessions
	case TabMore:
		return PlaceMore
	}
	return PlaceInbox
}

// TabOf is the tab whose own view place is: the tab a compact window
// chooses as it goes there, whatever way it went (its button, a key, a
// card's button). False for a place of no tab: the Terminal, which iOS
// lacks, and a session, which shows over the tab chosen.
func TabOf(place string) (Tab, bool) {
	for _, t := range Tabs {
		if t.Place() == place {
			return t, true
		}
	}
	return TabInbox, false
}

// BackTo is the place the back button of a session shown over tab t goes
// to: t's own view. One session at a time shows over a tab, so a session
// opened from another one goes back to the tab, not to the first.
func BackTo(t Tab) string { return t.Place() }

// TabBarShows reports whether the bar of tabs shows: over a tab's own
// view, and not over a session, whose composer takes the bottom of the
// screen, as a phone's conversations do.
func TabBarShows(session bool) bool { return !session }

// InboxBadgeMost is the largest count the Inbox tab's badge spells out.
const InboxBadgeMost = 99

// InboxBadge is the Inbox tab's badge for kinds (Data.Inbox): how many
// things wait for the owner, "99+" past InboxBadgeMost; "!" when none is
// counted and a list failed to load, so the Inbox may hold what the badge
// cannot count; "" when nothing waits, or nothing is known yet.
func InboxBadge(kinds []InboxKind) string {
	n, failed := 0, false
	for _, k := range kinds {
		n += k.Count
		failed = failed || k.Status == ListFailed
	}
	switch {
	case n > InboxBadgeMost:
		return strconv.Itoa(InboxBadgeMost) + "+"
	case n > 0:
		return strconv.Itoa(n)
	case failed:
		return "!"
	}
	return ""
}
