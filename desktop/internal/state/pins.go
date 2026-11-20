package state

import "slices"

// Pins are the sessions the owner pinned, by id, in the order pinned. The
// sidebar lists them in a section of their own, first after the places
// (the owner's choice: pocket lists them after the team), and Recent no
// longer lists them; each still shows in its agent's group. The desktop
// keeps them in its settings file, per service, as it keeps the seen
// marks: pocketd knows nothing of them, so another desktop has its own.
type Pins []int64

// Has reports whether session id is pinned.
func (p Pins) Has(id int64) bool { return slices.Contains(p, id) }

// Toggle returns the pins with session id pinned, after the others, or
// unpinned when it was pinned: every time the pins name it, as a settings
// file written by hand may, since the sidebar shows it once (Codex review
// of #47, round 1). A new slice, since the sidebar may still read p.
func (p Pins) Toggle(id int64) Pins {
	if p.Has(id) {
		return slices.DeleteFunc(slices.Clone(p), func(pin int64) bool { return pin == id })
	}
	return append(slices.Clone(p), id)
}

// PinOf is the session row k pins or unpins, from its right-click menu: a
// session's row its session, nested under the session that started it or
// not; false for a place's row or a group's, which pin nothing.
func PinOf(k SidebarKey) (int64, bool) {
	if k.Kind != SidebarSession || k.Session == 0 {
		return 0, false
	}
	return k.Session, true
}
