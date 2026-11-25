package nsgoclient

import (
	"github.com/frostyard/frostui/workspaceui"
	"github.com/frostyard/pocketgo/desktop/internal/state"
	"github.com/nsgo-dev/nsgo/base"
	"github.com/nsgo-dev/nsgo/core"
	"github.com/nsgo-dev/nsgo/icons/lucide"
	"github.com/nsgo-dev/nsgo/ui"
)

// The window's shell, as the apps on the kit share it (frostui's
// docs/specs/workspaceui.md, the standard for an app): the toolbar's
// actions are icon buttons in NSGo's toolbar, the palette is a field with
// its keys, and the navigation's foot says where the app is connected and
// leads to Settings.

// paletteLabel names the palette's field, and its icon button where the
// window has no room for the field.
const paletteLabel = "Search or run a command"

// fieldRoom is the least width of a window that shows the palette as a
// field.
const fieldRoom core.Points = 900

// toolbar is the toolbar's end: the way back and forward, Refresh, and
// the palette.
func (c *client) toolbar(cx *core.Scope, palette func()) core.Node {
	wide := core.NewMemo(cx, func() bool { return cx.Env().Window().Width >= fieldRoom })
	refresh := func() {
		c.store.Refresh()
		c.fileRefresh.Update(func(i int) int { return i + 1 })
		if c.page.Get() == "Files" {
			c.store.LoadFileRoots()
		}
		c.changed()
	}
	tools := ui.Toolbar{Label: "Window", Children: []core.Node{
		ui.ToolbarGroup{Label: "History", Children: []core.Node{
			ui.IconButton{Icon: lucide.ArrowLeft, Label: "Back", Disabled: func() bool { return !c.canMove(-1) }, OnPress: func() { c.move(-1) }},
			ui.IconButton{Icon: lucide.ArrowRight, Label: "Forward", Disabled: func() bool { return !c.canMove(1) }, OnPress: func() { c.move(1) }}}},
		ui.ToolbarSeparator{},
		ui.ToolbarGroup{Label: "Data", Children: []core.Node{ui.IconButton{Icon: lucide.RefreshCw, Label: "Refresh", OnPress: refresh}}}}}
	return core.Dynamic(func(*core.Scope) core.Node {
		var find core.Node = ui.IconButton{Icon: lucide.Search, Label: paletteLabel, OnPress: palette}
		if wide.Get() {
			find = ui.SidebarButton{Text: paletteLabel, Icon: lucide.Search, Shortcut: "mod+k", Variant: ui.SidebarButtonField, OnPress: palette}
		}
		return core.View{Style: core.Style{Direction: core.Row, AlignItems: core.AlignCenter, Gap: workspaceui.SpaceS, FlexShrink: 0}, Children: []core.Node{tools, find}}
	})
}

// sidebarFoot is the navigation's foot: the server and the state of the
// connection to it, and the way to Settings.
func (c *client) sidebarFoot(settings func()) core.Node {
	text := func() string {
		c.revision.Get()
		link := c.store.Conn
		switch {
		case link.Stopped:
			return "Connection stopped · " + link.Server
		case link.State == state.Reconnecting:
			return "Reconnecting · " + link.Server
		case link.State == state.Connecting:
			return "Connecting · " + link.Server
		default:
			return link.Server
		}
	}
	return core.Dynamic(func(*core.Scope) core.Node {
		c.revision.Get()
		tone := workspaceui.Success
		switch link := c.store.Conn; {
		case link.Stopped:
			tone = workspaceui.Danger
		case link.State == state.Reconnecting || link.State == state.Connecting:
			tone = workspaceui.Warning
		}
		return core.View{Style: core.Style{Direction: core.Row, AlignItems: core.AlignCenter, Width: core.Pct(100)}, Children: []core.Node{
			workspaceui.ConnectionItem{Tone: tone, Text: text}, ui.Spacer{},
			ui.IconButton{Icon: lucide.Settings, Label: "Settings", OnPress: settings}}}
	})
}

// question is a confirmation: what it asks, what its action is called, and
// what the action does.
type question struct {
	title, body, action string
	variant             ui.ButtonVariant
	fn                  func()
	// gen is the sign-in the question was asked under: an answer after
	// another sign-in does nothing.
	gen int
}

// asks holds the confirmations in the order they were asked. One shows at
// a time. A confirmation asked while another shows waits behind it and
// never takes its place: a merge's confirmation opens when its checks
// end, at no press of the owner's, and a question that changed under the
// pointer would be answered unread.
type asks struct {
	shown   *question
	waiting []*question
}

// add asks q, and returns the confirmation to show.
func (a *asks) add(q *question) *question {
	if a.shown == nil {
		a.shown = q
	} else {
		a.waiting = append(a.waiting, q)
	}
	return a.shown
}

// done ends q, which was answered or dismissed, and returns the
// confirmation to show: the next that waited, when q was the one shown.
func (a *asks) done(q *question) *question {
	if a.shown != q {
		return a.shown
	}
	a.shown = nil
	if len(a.waiting) > 0 {
		a.shown, a.waiting = a.waiting[0], a.waiting[1:]
	}
	return a.shown
}

func (a *asks) clear() { a.shown, a.waiting = nil, nil }

// confirm asks before an action that sends or changes something, and
// destroy before one that ends or removes it: NSGo's alert dialog on the
// kit's panel, Cancel first and quiet, and a destroying action in the
// danger variant. The keyboard starts on Cancel, so Return alone never
// runs the action.
func (c *client) confirm(title, body, action string, fn func()) {
	c.ask(&question{title: title, body: body, action: action, variant: ui.ButtonPrimary, fn: fn})
}

func (c *client) destroy(title, body, action string, fn func()) {
	c.ask(&question{title: title, body: body, action: action, variant: ui.ButtonDanger, fn: fn})
}

// tell says something that needs no answer but Close.
func (c *client) tell(title, body string) { c.ask(&question{title: title, body: body}) }

func (c *client) ask(q *question) {
	q.gen = c.generation
	c.asking.Set(c.asks.add(q))
}

// alert draws the confirmation shown.
func (c *client) alert(cx *core.Scope) core.Node {
	q := c.asking.Get()
	if q == nil {
		return nil
	}
	closed := func() { c.asking.Set(c.asks.done(q)) }
	actions := []ui.AlertAction{{Text: "Close", Variant: ui.ButtonSecondary, Role: core.AlertCancel, Default: true}}
	if q.action != "" {
		actions = []ui.AlertAction{
			{Text: "Cancel", Variant: ui.ButtonSecondary, Role: core.AlertCancel, Default: true},
			{Text: q.action, Variant: q.variant, OnPress: func() {
				closed()
				if q.gen == c.generation {
					q.fn()
					c.changed()
				}
			}}}
	}
	return ui.AlertDialog{Title: q.title, Description: q.body, StyleFunc: workspaceui.DialogPanel(cx), Open: func() bool { return true },
		OnOpenChange: func(open bool, _ base.Reason) {
			if !open {
				closed()
			}
		}, Actions: actions}
}
