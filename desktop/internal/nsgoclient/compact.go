package nsgoclient

import (
	"fmt"

	"github.com/frostyard/frostui/workspaceui"
	"github.com/frostyard/pocketgo/desktop/internal/state"
	"github.com/nsgo-dev/nsgo/core"
	"github.com/nsgo-dev/nsgo/icons/lucide"
	"github.com/nsgo-dev/nsgo/ui"
)

// A compact window, an iPhone's and an iPad's in Split View, goes between
// its places by a bar of tabs at the bottom (ADR-0044): the Inbox, the
// work board, the sessions and More. A tab shows its own view; a session
// shows in its place, with a way back to it and no bar. The rules are
// state's (state.Compact, TabOf, BackFrom, TabBarShows, InboxBadge), as
// the MyGo client's compact window took them.

// The compact window's own places, beside the desktop's.
const (
	pageSessions = "Sessions"
	pageMore     = "More"
)

// goos is the platform the client runs on, as state's rules name it: by
// Go's names. A test's environment decides it, as a device's does.
func (c *client) goos() string {
	switch os := c.cx.Env().OS(); os {
	case core.OSMacOS:
		return "darwin"
	case core.OSWeb:
		return "js"
	default:
		return string(os)
	}
}

// compact reports whether the window takes the compact layout.
func (c *client) compact() bool {
	return state.Compact(c.goos(), float32(c.cx.Env().Window().Width))
}

// placeOf is state's name for a page of the client.
func placeOf(page string) string {
	switch page {
	case "Work board":
		return state.PlaceBoard
	case "Search":
		return state.PlaceSearch
	case "Files":
		return state.PlaceFiles
	case "Terminal":
		return state.PlaceTerminal
	case pageSessions:
		return state.PlaceSessions
	case pageMore:
		return state.PlaceMore
	}
	return state.PlaceInbox
}

// pageOf is the client's page for a place of state.
func pageOf(place string) string {
	switch place {
	case state.PlaceBoard:
		return "Work board"
	case state.PlaceSearch:
		return "Search"
	case state.PlaceFiles:
		return "Files"
	case state.PlaceSessions:
		return pageSessions
	case state.PlaceMore:
		return pageMore
	}
	return "Inbox"
}

// tabOf is the tab a page shows under: its own, or for a page of no tab
// (a session, New session, Activity) the tab it was opened over.
func (c *client) tabOf(page string) state.Tab {
	switch page {
	case "New session":
		return state.TabSessions
	case "Activity":
		return state.TabMore
	}
	if tab, ok := state.TabOf(placeOf(page)); ok && page != "Session" {
		return tab
	}
	return c.overTab
}

// compactWorkspace is the compact window: the bar of tabs, each tab's own
// view under a top bar, and a session over the tab it was opened from.
func (c *client) compactWorkspace(cx *core.Scope, showSettings func()) core.Node {
	icons := map[state.Tab]func() *core.SVG{state.TabInbox: lucide.Inbox, state.TabBoard: lucide.Columns3, state.TabSessions: lucide.MessageSquare, state.TabMore: lucide.Ellipsis}
	var tabs []ui.ShellTab
	for _, tab := range state.Tabs {
		shell := ui.ShellTab{Text: tab.Title(), Icon: icons[tab], TestID: "tab-" + tab.Place(),
			Content: func(cx *core.Scope) core.Node { return c.compactView(cx, tab, showSettings) }}
		if tab == state.TabInbox {
			shell.Badge = func() core.TabBadge {
				c.revision.Get()
				return core.TabBadge{Text: state.InboxBadge(c.store.Data.Inbox())}
			}
		}
		tabs = append(tabs, shell)
	}
	return ui.TabShell{Label: "Places", TestID: "tab-bar", Tabs: tabs,
		Selected: func() int { return int(c.tabOf(c.page.Get())) },
		OnChange: func(i int) { c.goTo(destination{page: pageOf(state.Tabs[i].Place())}) },
		// A session's composer takes the bottom of the screen.
		BarHidden: func() bool { return !state.TabBarShows(c.page.Get() == "Session") }}
}

// compactView is a tab's section: the top bar, and the page shown under
// the tab.
func (c *client) compactView(cx *core.Scope, tab state.Tab, showSettings func()) core.Node {
	bar := core.Dynamic(func(cx *core.Scope) core.Node {
		page := c.page.Get()
		heading := tab.Title()
		var start, end []core.Node
		switch page {
		case "Session":
			c.revision.Get()
			s, _ := c.store.Data.Session(c.session.Get())
			heading = sessionName(s)
			back := state.BackFrom(tab, c.openedFrom)
			start = append(start, ui.Button{Text: state.BackTitle(tab, c.openedFrom), Label: "Back to " + state.BackTitle(tab, c.openedFrom), Icon: lucide.ChevronLeft, Variant: ui.ButtonGhost,
				OnPress: func() { c.goTo(destination{page: pageOf(back)}) }})
		case "New session", "Search", "Files", "Activity":
			heading = page
			start = append(start, ui.Button{Text: tab.Title(), Label: "Back to " + tab.Title(), Icon: lucide.ChevronLeft, Variant: ui.ButtonGhost,
				OnPress: func() { c.goTo(destination{page: pageOf(tab.Place())}) }})
		case pageSessions:
			end = append(end, ui.IconButton{Label: "New session", Icon: lucide.SquarePen, Variant: ui.IconButtonPrimary, OnPress: func() { c.navigate("New session") }})
		}
		children := append(start, core.Text{Content: heading, MaxLines: 1, Style: ui.TypeTitle.Merge(core.Style{FlexGrow: 1, FlexShrink: 1, MinWidth: core.Pt(0)})})
		return core.View{TestID: "compact-bar", Style: core.Style{Direction: core.Row, AlignItems: core.AlignCenter, Gap: workspaceui.SpaceS, FlexShrink: 0,
			Padding: core.Symmetric(workspaceui.SpaceS, workspaceui.SpaceM)}, Children: append(children, end...)}
	})
	content := core.Dynamic(func(cx *core.Scope) core.Node {
		page := c.page.Get()
		// A tab shows a page only while it is the page's tab: the shell
		// keeps every tab's section.
		if c.tabOf(page) != tab {
			return core.Group{}
		}
		switch page {
		case pageSessions:
			return c.compactSessions(cx)
		case pageMore:
			return c.compactMore(cx, showSettings)
		}
		return c.placeView(cx, page)
	})
	return core.View{Style: ui.FillRest, Children: []core.Node{bar, core.View{Style: ui.FillRest.Merge(core.Style{MinHeight: core.Pt(0)}), Children: []core.Node{content}}}}
}

// compactSessions is the Sessions tab's own view: the filter and the
// sessions, as the sidebar lists them.
func (c *client) compactSessions(cx *core.Scope) core.Node {
	return core.View{Style: ui.FillRest.Merge(core.Style{Gap: workspaceui.SpaceS, Padding: core.All(workspaceui.SpaceS)}), Children: []core.Node{
		workspaceui.SearchField{Placeholder: "Filter sessions", Value: c.filter.Get, OnChange: c.filter.Set},
		c.sidebarNotes(),
		core.Scroll{Style: ui.FillRest, Children: []core.Node{core.Dynamic(func(*core.Scope) core.Node {
			c.filter.Get()
			// The sidebar's sections after its places.
			return ui.Sidebar{Selected: func() string { return "" }, OnSelect: c.navigate, Sections: c.sections()[1:]}
		})}}}}
}

// compactMore is the More tab's own view: the places with no tab, what a
// desktop's status bar and palette hold, and the way out.
func (c *client) compactMore(cx *core.Scope, showSettings func()) core.Node {
	return core.Dynamic(func(cx *core.Scope) core.Node {
		c.revision.Get()
		colors := ui.ColorsOf(cx)
		row := func(text string, icon func() *core.SVG, run func()) core.Node {
			return ui.Button{Text: text, Icon: icon, Variant: ui.ButtonGhost, Style: core.Style{JustifyContent: core.JustifyStart}, OnPress: run}
		}
		note := func(text string) core.Node {
			return core.Text{Content: text, Style: ui.TypeCaption, StyleFunc: ui.Colored(colors.Muted)}
		}
		rows := []core.Node{}
		// A refreshed GitHub sign-in the keyring did not keep, from any view.
		if c.gh != nil && c.gh.CanSaveAgain() {
			rows = append(rows, workspaceui.Banner{Tone: workspaceui.Danger, Icon: lucide.CircleAlert, Text: "GitHub sign-in not saved", Action: "Save Again", OnAction: c.saveGitHubAgain})
		}
		rows = append(rows, row("Files", lucide.FolderOpen, func() { c.navigate("Files") }), row("Activity", lucide.Activity, func() { c.navigate("Activity") }),
			row("Settings", lucide.Settings, showSettings), row("About PocketGo", lucide.Info, func() { c.tell("PocketGo", about()) }))
		if today, week, shown := c.store.UsageTotals(); shown {
			rows = append(rows, note(today.Line("Today")), note(week.Line(fmt.Sprintf("%d days", state.WeekDays))))
		}
		conn := c.store.Conn
		rows = append(rows, note(conn.Server))
		if h := c.store.Hello(); h != nil {
			rows = append(rows, note("pocketd "+h.Server+" · "+h.Device))
		}
		rows = append(rows, row("Sign out", lucide.LogOut, func() {
			c.destroy("Sign out?", conn.Server, "Sign out", func() { c.forget(conn.Server); c.store.SignOut(); c.reset() })
		}))
		return workspaceui.Page(rows...)
	})
}

// holder is the GitHub sign-in as the system suspends the app and as the
// app comes back.
type holder interface {
	Suspend()
	Resume()
}

// holdWhileSuspended holds the GitHub sign-in's refresh while the system
// has the app in the background, where it suspends apps (ADR-0041,
// state.SavesOnSuspend): no refresh starts until the app is back, and one
// in progress gets its seconds to reach the keychain. The client keeps
// each preference as it changes, so the window has nothing to save then.
func (c *client) holdWhileSuspended(auth holder) {
	if !state.SavesOnSuspend(c.goos()) {
		return
	}
	held := false
	c.cx.Effect(func() {
		away := c.cx.Env().Lifecycle() == core.LifecycleBackground
		switch {
		case away && !held:
			held = true
			auth.Suspend()
		case !away && held:
			held = false
			auth.Resume()
		}
	})
}
