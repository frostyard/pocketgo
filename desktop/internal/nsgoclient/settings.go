package nsgoclient

import (
	"strings"

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
		ui.SettingsItem{ID: "zoom", Title: "Zoom", Description: "Adjust the size of text and controls.", Keywords: []string{"display", "scale"}, Control: zoom})
}

func (c *client) connectionSettings() core.Node {
	return workspaceui.Stack(core.Text{Selectable: core.SelectableOn, ContentFunc: func() string {
		c.revision.Get()
		conn := c.store.Conn
		return strings.Join(nonempty(conn.Server, conn.Reason), " · ")
	}},
		workspaceui.Row(c.button("Reconnect", c.store.RetryNow), c.button("Disconnect", func() {
			server := c.store.Conn.Server
			c.confirm("Disconnect?", server, "Disconnect", func() { c.forget(server); c.store.SignOut(); c.reset() })
		})))
}

func (c *client) providerSettings(cx *core.Scope) core.Node {
	c.revision.Get()
	var out []core.Node
	if status, msg := c.store.Data.SignInsStatus(); msg != "" {
		out = append(out, body(msg))
	} else if status == state.ListLoading {
		out = append(out, body("Loading accounts…"))
	}
	for _, row := range c.store.Data.SignIns() {
		out = append(out, body(strings.Join(nonempty(row.Name, row.Account, string(row.State)), " · ")))
		if c.store.MaySignIn(row) {
			out = append(out, c.button("Sign in to "+row.Name, func() { c.store.StartSignIn(row.Provider) }))
		}
		if row.DeviceFlow && row.State != proto.SignInNone {
			out = append(out, c.button("Sign out of "+row.Name, func() {
				c.confirm("Sign out of "+row.Name+"?", row.Account, "Sign out", func() { c.store.SignOutOf(row.Provider) })
			}))
		}
	}
	if sheet := c.store.SignInSheet(); sheet.ProviderID != "" {
		out = append(out, title(sheet.Provider), body(strings.Join(nonempty(sheet.Flow.UserCode, sheet.Text), "\n")))
		if page, label, ok := state.DevicePage(sheet.ProviderID); ok && sheet.Phase == state.SheetCode {
			out = append(out, c.button("Copy code", func() { cx.SetClipboard(sheet.Flow.UserCode) }), c.button("Open "+label, func() { cx.OpenURL(page, nil) }))
		}
		out = append(out, c.button("Close sign-in", c.store.CancelSignIn))
	}

	if len(out) == 0 {
		return body("No provider accounts available.")
	}
	return workspaceui.Stack(out...)
}
