package nsgoclient

import (
	"fmt"
	"runtime"

	"github.com/frostyard/pocketgo/desktop/internal/state"
	"github.com/nsgo-dev/nsgo/base"
	"github.com/nsgo-dev/nsgo/core"
	"github.com/nsgo-dev/nsgo/ui"
)

func (c *client) setZoom(z float32) { z = state.Zoom(z); c.zoom.Set(z); c.saveJSON("zoom", z) }
func (c *client) commands(cx *core.Scope, showSettings func()) (core.Node, func()) {
	open := core.NewSignal(cx, false)
	show := func() { open.Set(true) }
	commands := []base.CommandItem{
		{ID: "New session", Label: "New session", Shortcut: "mod+n"}, {ID: "Inbox", Label: "Inbox", Shortcut: "mod+1"}, {ID: "Work board", Label: "Work board", Shortcut: "mod+2"},
		{ID: "Search", Label: "Search all sessions"}, {ID: "Files", Label: "Browse files"}, {ID: "Terminal", Label: "Open terminal"}, {ID: "Activity", Label: "Activity log"}, {ID: "Settings", Label: "Settings", Shortcut: "mod+,"},
		{ID: "zoom-in", Label: "Zoom in", Shortcut: "mod+="}, {ID: "zoom-out", Label: "Zoom out", Shortcut: "mod+-"}, {ID: "zoom-reset", Label: "Actual size", Shortcut: "mod+0"}, {ID: "about", Label: "About PocketGo"}}
	run := func(cmd string) {
		open.Set(false)
		switch cmd {
		case "Settings":
			showSettings()
		case "zoom-in":
			c.setZoom(state.ZoomBy(c.zoom.Get(), 1))
		case "zoom-out":
			c.setZoom(state.ZoomBy(c.zoom.Get(), -1))
		case "zoom-reset":
			c.setZoom(1)
		case "about":
			cx.ShowAlert(core.AlertConfig{Title: "PocketGo", Message: fmt.Sprintf("NSGo client · %s/%s", runtime.GOOS, runtime.GOARCH), Actions: []core.AlertAction{{Label: "Close"}}}, nil)
		case "abort":
			if o := c.store.Data.Open(); c.page.Get() == "Session" && o != nil && o.Live.Busy {
				c.store.Abort(o.ID)
				c.changed()
			}
		default:
			c.navigate(cmd)
		}
	}
	for _, cmd := range commands {
		if cmd.Shortcut != "" {
			cx.OnKey(cmd.Shortcut, func() {
				if c.page.Get() != "Terminal" {
					run(cmd.ID)
				}
			})
		}
	}
	cx.OnKey("mod+k", show)
	cx.OnKey("mod+.", func() { run("abort") })
	cx.OnBackPress(func() bool { return c.move(-1) })
	return ui.CommandDialog{Open: open.Get, OnOpenChange: func(b bool, _ base.Reason) { open.Set(b) }, Command: ui.Command{Label: "Workspace commands", SearchLabel: "Find command", Items: func() []base.CommandItem {
		c.revision.Get()
		out := append([]base.CommandItem(nil), commands...)
		if o := c.store.Data.Open(); c.page.Get() == "Session" && o != nil && o.Live.Busy {
			out = append(out, base.CommandItem{ID: "abort", Label: "Stop run", Shortcut: "mod+."})
		}
		for _, s := range c.store.Data.Sessions() {
			out = append(out, base.CommandItem{ID: fmt.Sprintf("session/%d", s.ID), Label: sessionName(s), Group: "Sessions"})
		}
		return out
	}, OnSelect: run}}, show
}
