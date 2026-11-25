package nsgoclient

import (
	"fmt"
	"runtime"
	"slices"

	"github.com/frostyard/frostui/workspaceui"
	"github.com/frostyard/pocketgo/desktop/internal/state"
	"github.com/frostyard/pocketgo/proto"
	"github.com/nsgo-dev/nsgo/base"
	"github.com/nsgo-dev/nsgo/core"
	"github.com/nsgo-dev/nsgo/desktop"
	"github.com/nsgo-dev/nsgo/ui"
)

// quits says the platform leaves quitting to the app: not a browser, whose
// page is the browser's, nor a phone or a tablet (state.QuitsItself).
func (c *client) quits() bool { return c.goos() != "js" && state.QuitsItself(c.goos()) }

func (c *client) setZoom(z float32) { z = state.Zoom(z); c.zoom.Set(z); c.saveJSON("zoom", z) }

// zoomNeed is the width the window keeps at any zoom: under it the kit's
// shell goes to one pane, so a zoom that would leave less is shown as the
// largest that fits (state.ZoomFit).
const zoomNeed = 720

// shownZoom is the zoom the window shows for the zoom saved.
func (c *client) shownZoom() float32 {
	return state.ZoomFit(c.zoom.Get(), float32(c.cx.Env().Window().Width), zoomNeed)
}

// zoomKeys bind the zoom's keys for the whole window, the sign-in screen
// included. A terminal keeps its own keys.
func (c *client) zoomKeys(cx *core.Scope) {
	bind := func(key string, steps int) {
		cx.OnKey(key, func() {
			if c.store.SignedIn() && c.page.Get() == "Terminal" {
				return
			}
			if steps == 0 {
				c.setZoom(1)
				return
			}
			c.setZoom(state.ZoomBy(c.zoom.Get(), steps))
		})
	}
	bind("mod+=", 1)
	bind("mod+shift+=", 1)
	bind("mod+-", -1)
	bind("mod+0", 0)
}

// about is what About PocketGo says.
func about() string {
	version := core.App().Version
	if version == "" {
		version = "a development build"
	}
	return fmt.Sprintf("Version %s, on %s/%s.\nThe client of pocketd, protocol version %d.\nhttps://github.com/frostyard/pocketgo · MIT", version, runtime.GOOS, runtime.GOARCH, proto.Version)
}

func (c *client) commands(cx *core.Scope, showSettings func()) (core.Node, func()) {
	open := core.NewSignal(cx, false)
	show := func() { open.Set(true) }
	commands := []base.CommandItem{
		{ID: "New session", Label: "New session", Shortcut: "mod+n"}, {ID: "Inbox", Label: "Inbox", Shortcut: "mod+1"}, {ID: "Work board", Label: "Work board", Shortcut: "mod+2"},
		{ID: "Search", Label: "Search all sessions", Shortcut: "mod+shift+f"}, {ID: "Files", Label: "Browse files"}, {ID: "Terminal", Label: "Open terminal"}, {ID: "Activity", Label: "Activity log", Shortcut: "mod+j"}, {ID: "Settings", Label: "Settings", Shortcut: "mod+,"},
		{ID: "about", Label: "About PocketGo"}, {ID: "sign-out", Label: "Sign out", Group: "App"}}
	busy := func() bool {
		o := c.store.Data.Open()
		return c.page.Get() == "Session" && o != nil && o.Live.Busy
	}
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
			c.tell("PocketGo", about())
		case "sign-out":
			server := c.store.Conn.Server
			c.destroy("Sign out?", server, "Sign out", func() { c.forget(server); c.store.SignOut(); c.reset() })
		case "github-sign-in":
			if c.gh != nil && c.gh.CanOpenSignIn() {
				c.gh.BeginSignIn()
				showSettings()
			}
		case "github-sign-out":
			if c.gh != nil && c.gh.CanSignOut() {
				c.destroy("Sign out of GitHub?", "Remove the owner credential from the desktop keyring.", "Sign out", func() { c.gh.SignOut(c.result) })
			}
		case "quit":
			desktop.RequestQuit(c.cx)
		case "rename":
			// F2 renames the session shown.
			if c.page.Get() == "Session" {
				c.startRename(c.session.Get())
			}
		case "abort":
			if busy() {
				c.store.Abort(c.store.Data.Open().ID)
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
	// The palette's second key, for a place where an editor takes the first.
	cx.OnKey("mod+shift+p", show)
	if c.quits() {
		cx.OnKey("mod+q", func() { run("quit") })
	}
	cx.OnKey("mod+.", func() { run("abort") })
	cx.OnKey("f2", func() { run("rename") })
	cx.OnBackPress(func() bool { return c.move(-1) })
	return ui.CommandDialog{Style: core.Style{Background: workspaceui.ColorsOf(cx).Panel()}, Open: open.Get, OnOpenChange: func(b bool, _ base.Reason) { open.Set(b) }, Command: ui.Command{Label: "Workspace commands", SearchLabel: "Find command", Items: func() []base.CommandItem {
		c.revision.Get()
		out := append([]base.CommandItem(nil), commands...)
		// No Terminal where the platform has none (state.HasTerminal).
		if !state.HasTerminal(c.goos()) {
			out = slices.DeleteFunc(out, func(i base.CommandItem) bool { return i.ID == "Terminal" })
		}
		// Stop run is always listed, and says why it is off.
		stop := base.CommandItem{ID: "abort", Label: "Stop run", Shortcut: "mod+."}
		if !busy() {
			stop.Disabled, stop.Description = true, "No run works in the session shown"
		}
		out = append(out, stop)
		// Rename session is always listed, and says why it is off.
		rename := base.CommandItem{ID: "rename", Label: "Rename session", Shortcut: "f2"}
		switch {
		case c.page.Get() != "Session":
			rename.Disabled, rename.Description = true, "No session is shown"
		case !c.store.Data.Renamable(c.session.Get()):
			rename.Disabled, rename.Description = true, "pocketd names this session"
		case !c.store.MayRename(c.session.Get()):
			rename.Disabled, rename.Description = true, "A rename of it is on its way"
		}
		out = append(out, rename)
		// The zoom's commands say when the zoom is at its end.
		z := c.zoom.Get()
		in := base.CommandItem{ID: "zoom-in", Label: "Zoom in", Shortcut: "mod+="}
		if state.ZoomBy(z, 1) == z {
			in.Disabled, in.Description = true, "PocketGo is at its largest"
		}
		smaller := base.CommandItem{ID: "zoom-out", Label: "Zoom out", Shortcut: "mod+-"}
		if state.ZoomBy(z, -1) == z {
			smaller.Disabled, smaller.Description = true, "PocketGo is at its smallest"
		}
		actual := base.CommandItem{ID: "zoom-reset", Label: "Actual size", Shortcut: "mod+0", Disabled: z == 1}
		out = append(out, in, smaller, actual)
		if g := c.gh; g != nil {
			out = append(out, base.CommandItem{ID: "github-sign-in", Label: "Sign in to GitHub", Group: "GitHub", Disabled: !g.CanOpenSignIn() || g.Status().SignedIn},
				base.CommandItem{ID: "github-sign-out", Label: "Sign out of GitHub", Group: "GitHub", Disabled: !g.CanSignOut()})
		}
		if c.quits() {
			out = append(out, base.CommandItem{ID: "quit", Label: "Quit", Group: "App", Shortcut: "mod+q"})
		}
		for _, s := range c.store.Data.Sessions() {
			out = append(out, base.CommandItem{ID: fmt.Sprintf("session/%d", s.ID), Label: sessionName(s), Group: "Sessions"})
		}
		return out
	}, OnSelect: run}}, show
}
