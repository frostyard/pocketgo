//go:build !js

package nsgoclient

import (
	"github.com/frostyard/pocketgo/desktop/internal/config"
	"github.com/frostyard/pocketgo/desktop/internal/state"
)

// The MyGo client kept its preferences in settings.json, in the config
// folder; this client keeps its own in its storage. A device that ran the
// MyGo client starts this one with what it kept: the service to sign in
// to, the pins, the seen marks, the notification switches, what has
// notified, and the zoom. The device token and the GitHub sign-in need no
// move: both clients keep them in the same keyring entries.

// movedKey marks a storage that took the MyGo client's settings, so that
// the move is made once: a later change of this client's preferences is
// not undone by the file, which this client never writes.
const movedKey = "settings-moved"

// moveSettings gives the client what the MyGo client's settings hold,
// once, and only to a storage with no sign-in of its own. The pane widths
// and the collapsed panes are not moved: the 2 windows' panes differ.
func (c *client) moveSettings(s config.Settings) {
	storage := c.cx.Storage()
	if _, done := storage.GetItem(movedKey); done {
		return
	}
	_ = storage.SetItem(movedKey, "1")
	if _, own := storage.GetItem("connected-server"); own {
		return
	}
	if _, own := storage.GetItem("last-server"); own {
		return
	}
	if s.Server != "" {
		_ = storage.SetItem("connected-server", s.Server)
	}
	if last := s.LastServer; last != "" {
		_ = storage.SetItem("last-server", last)
	} else if s.Server != "" {
		_ = storage.SetItem("last-server", s.Server)
	}
	if _, own := storage.GetItem("zoom"); !own && s.Zoom != 0 {
		c.setZoom(s.Zoom)
	}
	servers := map[string]bool{}
	for server := range s.Seen {
		servers[server] = true
	}
	for server := range s.Pinned {
		servers[server] = true
	}
	for server := range s.Notify {
		servers[server] = true
	}
	if s.Server != "" {
		servers[s.Server] = true
	}
	for server := range servers {
		n := s.Notify[server]
		p := preferences{Pinned: state.Pins(s.Pinned[server]), Seen: state.Seen(s.Seen[server]),
			Notify: state.NotifyPrefs{Off: n.Off, AsksOff: n.AsksOff, ApprovalsOff: n.ApprovalsOff, ReviewsOff: n.ReviewsOff, RunsOff: n.RunsOff}}
		// The file holds one session shown last, of the service signed in to.
		if server == s.Server {
			p.LastSession = s.LastSession
		}
		c.saveJSON("preferences/"+server, p)
	}
	for server, keys := range s.Notified {
		c.saveJSON("notified/"+server, keys)
	}
}
