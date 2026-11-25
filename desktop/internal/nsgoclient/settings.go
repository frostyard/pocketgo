package nsgoclient

import (
	"strings"
	"time"

	"github.com/frostyard/frostui/workspaceui"
	"github.com/frostyard/pocketgo/desktop/internal/state"
	"github.com/frostyard/pocketgo/proto"
	"github.com/nsgo-dev/nsgo/base"
	"github.com/nsgo-dev/nsgo/core"
	"github.com/nsgo-dev/nsgo/icons/lucide"
	"github.com/nsgo-dev/nsgo/ui"
)

func (c *client) settingsDialog(cx *core.Scope, open *core.Signal[bool]) core.Node {
	return ui.SettingsDialog{Label: "Settings", Open: open.Get, OnOpenChange: func(v bool, _ base.Reason) { open.Set(v) },
		Content: func(narrow func() bool) core.Node {
			return ui.Settings{Title: "Settings", Narrow: narrow, SearchLabel: "Search settings", SearchPlaceholder: "Search",
				ResultsTitle: "Search results", EmptyText: "No matching settings", CloseLabel: "Close settings", OnClose: func() { open.Set(false) },
				Categories: []ui.SettingsCategory{
					{ID: "general", Title: "General", Icon: lucide.Settings, Groups: []ui.SettingsGroup{{Items: c.generalSettings(cx)}}},
					{ID: "connection", Title: "Connection", Icon: lucide.Network, Groups: []ui.SettingsGroup{{Items: []ui.SettingsItem{
						{ID: "server", Title: "Pocketd server", Keywords: []string{"service", "URL", "reconnect", "disconnect"}, Stacked: true, Control: c.connectionSettings()},
					}}}},
					{ID: "accounts", Title: "Accounts", Icon: lucide.Users, Groups: []ui.SettingsGroup{{Items: []ui.SettingsItem{
						{ID: "providers", Title: "Provider accounts", Keywords: []string{"sign in", "sign out", "device code", "copilot"}, Stacked: true, Control: core.Dynamic(c.providerSettings)},
						{ID: "github", Title: "GitHub", Keywords: []string{"sign in", "sign out", "merge", "credentials"}, Stacked: true, Control: core.Dynamic(func(*core.Scope) core.Node { c.revision.Get(); return c.githubSettings() })},
						{ID: "usage", Title: "Usage", Keywords: []string{"tokens", "today", "week"}, Stacked: true, Control: core.Dynamic(func(*core.Scope) core.Node {
							c.revision.Get()
							if today, week, ok := c.store.UsageTotals(); ok {
								return workspaceui.Stack(body(today.Line("Today")), body(week.Line("This week")))
							}
							return body("Usage totals are unavailable.")
						})},
					}}}},
					{ID: "notifications", Title: "Notifications", Icon: lucide.Bell, Groups: c.notificationSettings(cx)},
				}}
		}}
}

func (c *client) generalSettings(cx *core.Scope) []ui.SettingsItem {
	var rows []ui.SettingsItem
	if version := core.App().Version; version != "" {
		rows = append(rows, ui.SettingsItem{ID: "version", Title: "PocketGo", Description: "App version", Control: body(version)})
	}
	field := core.Style{Width: core.Pt(140)}
	theme := workspaceui.AppearanceChoice{Value: cx.Env().Appearance, OnChange: c.saveAppearance}
	var sizes []ui.Option[string]
	for _, s := range state.ZoomLabels() {
		sizes = append(sizes, ui.Option[string]{Value: s, Label: s})
	}
	zoom := ui.Select[string]{Label: "Zoom", Style: field, Options: sizes, Value: func() string { return state.ZoomLabel(c.zoom.Get()) }, OnChange: func(s string) {
		if z, ok := state.ZoomOfLabel(s); ok {
			c.setZoom(z)
		}
	}}
	return append(rows, ui.SettingsItem{ID: "theme", Title: "Theme", Description: "Choose light or dark, or follow the system.", Keywords: []string{"appearance"}, Control: theme},
		ui.SettingsItem{ID: "zoom", Title: "Zoom", Description: zoomNote(c.zoom.Get(), c.shownZoom()), Keywords: []string{"display", "scale"}, Control: zoom})
}

func (c *client) connectionSettings() core.Node {
	return workspaceui.Stack(core.Text{Selectable: core.SelectableOn, ContentFunc: func() string {
		c.revision.Get()
		conn := c.store.Conn
		return strings.Join(nonempty(conn.Server, conn.Reason), " · ")
	}},
		workspaceui.Row(c.button("Reconnect", c.store.RetryNow), c.button("Disconnect", func() {
			server := c.store.Conn.Server
			c.destroy("Disconnect?", server, "Disconnect", func() { c.forget(server); c.store.SignOut(); c.reset() })
		})))
}

// providerSettings are pocketd's sign-ins to its model providers: each
// with what it is signed in as, Sign in… or Sign out, and the sheet of a
// device flow the owner started.
func (c *client) providerSettings(cx *core.Scope) core.Node {
	c.revision.Get()
	colors := ui.ColorsOf(cx)
	d := &c.store.Data
	var out []core.Node
	status, problem := d.SignInsStatus()
	switch {
	case status == state.ListFailed:
		out = append(out, c.listProblem("the sign-ins", problem, "", c.store.LoadSignIns))
	case status == state.ListLoading && len(d.SignIns()) == 0:
		out = append(out, body("Loading the sign-ins…"))
	}
	for _, row := range d.SignIns() {
		var action core.Node = core.Group{}
		switch {
		case !row.DeviceFlow:
		case row.State == proto.SignInSignedIn:
			busy := c.store.SigningOut(row.Provider).Busy
			action = ui.Button{Text: "Sign out", Label: "Sign out of " + row.Name, Variant: ui.ButtonSecondary, Size: ui.SizeSmall, Disabled: func() bool { return busy }, OnPress: func() {
				c.destroy("Sign pocketd out of "+row.Name+"?", "pocketd forgets its "+row.Name+" sign-in, and New session no longer offers its models. "+
					"A session on one of them fails its next call until you sign in again.", "Sign out of "+row.Name, func() { c.store.SignOutOf(row.Provider) })
			}}
		case c.store.MaySignIn(row):
			action = ui.Button{Text: "Sign in…", Label: "Sign in to " + row.Name, Variant: ui.ButtonSecondary, Size: ui.SizeSmall, OnPress: func() { c.store.StartSignIn(row.Provider); c.changed() }}
		}
		out = append(out, core.View{TestID: "sign-in-" + row.Provider, Style: core.Style{Direction: core.Row, AlignItems: core.AlignCenter, Gap: workspaceui.SpaceS}, Children: []core.Node{
			core.View{Style: core.Style{FlexGrow: 1, FlexShrink: 1, MinWidth: core.Pt(0)}, Children: []core.Node{
				core.Text{Content: row.Name, Style: ui.TypeLabel},
				core.Text{Content: state.SignInCaption(row), Style: ui.TypeCaption, StyleFunc: ui.Colored(colors.Muted)}}},
			action}})
		if why := c.store.SigningOut(row.Provider).Err; why != "" {
			out = append(out, c.problemBanner(workspaceui.Danger, why, nil))
		}
	}
	if sheet := c.signInSheet(cx); sheet != nil {
		out = append(out, sheet)
	}
	if len(out) == 0 {
		return body("No provider accounts available.")
	}
	return workspaceui.Stack(out...)
}

// signInSheet is the device flow the owner started: the code, the
// provider's device page, the wait with the code's time left, and how it
// ended, with Try Again. Cancel and Close cancel the flow on pocketd
// while it waits. nil while none runs.
func (c *client) signInSheet(cx *core.Scope) core.Node {
	v := c.store.SignInSheet()
	if v.Phase == state.SheetClosed {
		return nil
	}
	colors := ui.ColorsOf(cx)
	page, shown, _ := state.DevicePage(v.ProviderID)
	rows := []core.Node{title("Sign in to " + v.Provider)}
	end := c.button("Cancel", c.store.CancelSignIn)
	switch v.Phase {
	case state.SheetStarting:
		rows = append(rows, body("Asking GitHub for a code…"))
	case state.SheetCode:
		rows = append(rows, core.Text{Content: "Enter this code at GitHub's device page, and approve the sign-in there. pocketd keeps it; PocketGo never sees a token.", StyleFunc: ui.Colored(colors.Muted)},
			workspaceui.Row(core.Text{Content: v.Flow.UserCode, Label: "Code " + v.Flow.UserCode, Selectable: core.SelectableOn, Style: ui.TypeCode.Merge(workspaceui.Heading(1))},
				c.button("Copy code", func() { cx.SetClipboard(v.Flow.UserCode) })))
		if page != "" {
			rows = append(rows, workspaceui.Row(ui.Tooltip{Text: page, Trigger: ui.Button{Text: "Open " + shown, OnPress: func() { cx.OpenURL(page, nil) }}}),
				core.Text{Content: "On another computer, go to " + page + " and enter the code.", Selectable: core.SelectableOn, Style: ui.TypeCaption, StyleFunc: ui.Colored(colors.Muted)})
		}
		rows = append(rows, core.Text{Content: "Waiting for approval… " + state.FormatLeft(v.Left) + " left", StyleFunc: ui.Colored(colors.Muted)})
		// The time left counts down.
		cx.AfterFunc(time.Second, c.changed)
		if v.Text != "" {
			rows = append(rows, c.problemBanner(workspaceui.Danger, v.Text, nil))
		}
	case state.SheetSignedIn:
		rows = append(rows, workspaceui.Row(ui.Icon(lucide.Check, 16, colors.Up), body(v.Text)))
		end = c.button("Done", c.store.CancelSignIn)
	case state.SheetFailed:
		rows = append(rows, c.problemBanner(workspaceui.Danger, v.Text, func() { c.store.StartSignIn(v.ProviderID) }))
		end = c.button("Close", c.store.CancelSignIn)
	}
	return workspaceui.Card{Children: append(rows, workspaceui.Row(end))}
}

// zoomNote is what Settings says of the zoom: that a window too narrow
// for the zoom saved shows a smaller one.
func zoomNote(saved, shown float32) string {
	note := "Adjust the size of text and controls."
	if shown < saved {
		note += " Shown at " + state.ZoomLabel(shown) + ": the window is too narrow for more."
	}
	return note
}
