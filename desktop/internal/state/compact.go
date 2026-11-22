package state

import (
	"strconv"
	"time"

	"github.com/frostyard/pocketgo/proto"
)

// A compact window (Compact), an iPhone's, goes between its places by a
// bar of tabs at the bottom (ADR-0044): the Inbox, with what waits there,
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

// BoardPart is a part of the work board, as BoardOrder orders them.
type BoardPart int

const (
	// PartProposals and PartSchedules are the night shift's sections
	// (ADR-0034).
	PartProposals BoardPart = iota
	PartSchedules
	// PartThreads are the threads, grouped by who moves them next.
	PartThreads
)

// NightShiftHolds reports whether a night shift's section of the work
// board holds anything beyond its note that it is empty: an entry, the
// failure of its list's last fetch, or more, which the section says (a
// launch whose answer was lost, for the proposals; the problem of the
// owner's schedules file, for the schedules). While its list loads, it
// holds nothing yet.
func NightShiftHolds(entries int, status ListStatus, more bool) bool {
	return entries > 0 || status == ListFailed || more
}

// ProposalsSay reports whether the board's Proposals section says more
// than its proposals (NightShiftHolds' more), as the owner waits on a
// launch: a launch whose answer was lost, which it offers again
// (uncertain); a launch in flight, or its refusal (launching); or the
// launch it confirms, until its end at now (launched). The last proposal
// may have left the board with any of them (Codex review of #90, round
// 1).
func ProposalsSay(uncertain bool, launching Pending, launched *proto.Launch, now time.Time) bool {
	return uncertain || launching.Busy || launching.Err != "" || (launched != nil && launched.End.After(now))
}

// BoardOrder is the order the work board draws its parts in, given
// whether its Proposals and Schedules sections hold anything
// (NightShiftHolds). A desktop's window draws the night shift first, then
// the threads, whatever they hold. A compact window, a phone's, keeps a
// section that holds something above the threads, and puts one that holds
// nothing after them, where it takes none of the screen the threads need
// (the owner's choice of 2026-11-21, within ADR-0044).
func BoardOrder(compact, proposals, schedules bool) []BoardPart {
	if !compact {
		return []BoardPart{PartProposals, PartSchedules, PartThreads}
	}
	var above, below []BoardPart
	for _, s := range []struct {
		part  BoardPart
		holds bool
	}{{PartProposals, proposals}, {PartSchedules, schedules}} {
		if s.holds {
			above = append(above, s.part)
		} else {
			below = append(below, s.part)
		}
	}
	return append(append(above, PartThreads), below...)
}
