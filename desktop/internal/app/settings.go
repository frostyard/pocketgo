package app

import (
	"runtime"

	"github.com/egoist/mygo/ui"
	"github.com/frostyard/frostui"

	"github.com/frostyard/pocketgo/desktop/internal/state"
)

// prefs is the Settings window's state: frostui's layout, and the zoom it
// shows in words. Ported from frostyard/pocket, with Appearance's Zoom
// alone, as the owner asked.
type prefs struct {
	layout frostui.Settings
	zoom   string
	// notifyOn and notifyKinds are the Notifications page's switches, as
	// the settings have them for the service signed in to (ADR-0049).
	notifyOn    bool
	notifyKinds [len(state.NoticeKinds)]bool
}

// The panes' least widths: the zoom never goes past what fits both side
// by side, with the line between them (fitZoom).
const (
	sidebarMin = 220
	mainMin    = 420
	panesNeed  = sidebarMin + mainMin + 1
)

// The Settings window's size, and its margin from the window's edges at
// a zoom that leaves less room.
const (
	settingsWidth  = 760
	settingsHeight = 540
	settingsMargin = 16
)

// openSettings opens the Settings window at page (empty for the first).
func (a *App) openSettings(page string) { a.prefs.layout.Show(page) }

// applyZoom zooms the window to the zoom saved, or to the largest level
// below it at which the session list and the session still fit side by
// side (state.ZoomFit), and records the zoom shown. The window's size
// comes in DIPs at the zoom the last frame set, which holds until set
// again, so its width at 100% is that size times the zoom shown; the zoom
// is set once a frame.
func (a *App) applyZoom(c *ui.Context) {
	applied := a.shownZoom
	if applied <= 0 {
		applied = 1
	}
	w, _ := c.Size()
	a.shownZoom = state.ZoomFit(a.zoom(), w*applied, panesNeed)
	c.SetZoom(a.shownZoom)
}

// settingsDialog draws the Settings window while it is open: frostui's
// layout in a window no larger than the zoomed window leaves room for,
// which takes the zoom keys too, since a window's shortcuts wait behind a
// dialog (Codex review of #39).
func (a *App) settingsDialog(c *ui.Context) {
	l := &a.prefs.layout
	if !l.Open {
		return
	}
	a.prefs.zoom = state.ZoomLabel(a.zoom())
	subtitle := "Text, icons and spacing together (" + cmdKey() + " +, " + cmdKey() + " -, " + cmdKey() + " 0)"
	if a.shownZoom < a.zoom() {
		subtitle += ". Shown at " + state.ZoomLabel(a.shownZoom) + ": the window is too narrow for more"
	}
	pages := []frostui.SettingsPage{
		{ID: "appearance", Title: "Appearance", Icon: "preferences-desktop-theme", Keywords: "zoom scale font text size",
			Groups: []frostui.SettingsGroup{{Settings: []frostui.Setting{
				{Title: "Zoom", Subtitle: subtitle,
					Choice: &a.prefs.zoom, Options: state.ZoomLabels(), Keywords: "scale font text size bigger smaller",
					OnChange: func() {
						if z, ok := state.ZoomOfLabel(a.prefs.zoom); ok {
							a.setZoom(z)
						}
					}},
			}}}},
	}
	// Notifications, where the platform shows them (state.Notifies).
	if state.Notifies(a.opts.Platform) {
		pages = append(pages, a.notificationsPage())
	}
	w, h := c.Size()
	ui.Modal(c, &l.Open, func() {
		a.zoomKeysIn(c)
		width := min(settingsWidth, w-2*settingsMargin)
		if a.compact {
			width = dialogWidth(c, settingsWidth)
		}
		ui.Column(c).Width(width).Height(min(settingsHeight, h-2*settingsMargin)).
			AlignItems(ui.Stretch).Role(ui.RoleDialog).Label("Settings").Children(func() {
			ui.Row(c).AlignItems(ui.Center).Padding(0, 0, frostui.SpaceM, 0).Children(func() {
				ui.Text(c, "Settings").FontSize(18).Bold().Grow(1)
				if frostui.IconButton(c, "window-close", "Close").Clicked() {
					l.Open = false
				}
			})
			frostui.RowDivider(c)
			ui.Column(c).Grow(1).AlignItems(ui.Stretch).Children(func() { l.View(c, pages) })
		})
	})
}

// zoomKeysIn runs the zoom keys pressed in c's scope, as in a dialog,
// where the window's shortcuts wait.
func (a *App) zoomKeysIn(c *ui.Context) {
	for _, cmd := range a.zoomKeys.Commands() {
		for _, b := range cmd.Bindings {
			if c.Shortcut(b.Mods, b.Key) {
				a.zoomKeys.Run(cmd.ID)
			}
		}
	}
}

// cmdKey names the key the zoom keys take: Cmd on macOS, Ctrl elsewhere.
func cmdKey() string {
	if runtime.GOOS == "darwin" {
		return "⌘"
	}
	return "Ctrl"
}

// zoom is the zoom saved: the level nearest to it, 100% when none is.
// The window shows it, or less when it does not fit (applyZoom).
func (a *App) zoom() float32 { return state.Zoom(a.settings.Zoom) }

// setZoom saves the level nearest to z as the zoom.
func (a *App) setZoom(z float32) {
	saved := state.ZoomSaved(z)
	if saved == a.settings.Zoom {
		return
	}
	a.settings.Zoom = saved
	_ = a.saveSettings()
}

// zoomBy zooms steps levels in (out when negative), within the levels.
func (a *App) zoomBy(steps int) { a.setZoom(state.Zoom(state.ZoomBy(a.settings.Zoom, steps))) }

// settingsCommand opens Settings.
func (a *App) settingsCommand() frostui.Command {
	return frostui.Command{ID: "settings", Title: "Settings…", Group: "App", Icon: "preferences-system",
		Bindings: []frostui.KeyBinding{{Mods: ui.Cmd, Key: ui.KeyComma, Scope: frostui.Everywhere}},
		Run: func() {
			// A compact window's Settings are rows of its More tab.
			if a.compact {
				a.goPlaceNamed(state.PlaceMore)
				return
			}
			a.openSettings("")
		}}
}

// zoomCommands zoom the window, with the keys a browser zooms with: Cmd
// = (and Cmd + with Shift), Cmd - and Cmd 0, Ctrl off macOS. They work on
// the sign-in screen too (zoomKeys), and in Settings (zoomKeysIn).
func (a *App) zoomCommands() []frostui.Command {
	return []frostui.Command{
		{ID: "zoom-in", Title: "Zoom In", Group: "View", Icon: "zoom-in",
			Bindings: []frostui.KeyBinding{
				{Mods: ui.Cmd, Key: ui.KeyEqual, Scope: frostui.Everywhere},
				{Mods: ui.Cmd | ui.Shift, Key: ui.KeyEqual, Scope: frostui.Everywhere},
			},
			Enabled: func() (bool, string) {
				if a.zoom() >= state.ZoomLevels[len(state.ZoomLevels)-1] {
					return false, "PocketGo is at its largest"
				}
				return true, ""
			},
			Run: func() { a.zoomBy(1) }},
		{ID: "zoom-out", Title: "Zoom Out", Group: "View", Icon: "zoom-out",
			Bindings: []frostui.KeyBinding{{Mods: ui.Cmd, Key: ui.KeyMinus, Scope: frostui.Everywhere}},
			Enabled: func() (bool, string) {
				if a.zoom() <= state.ZoomLevels[0] {
					return false, "PocketGo is at its smallest"
				}
				return true, ""
			},
			Run: func() { a.zoomBy(-1) }},
		{ID: "zoom-reset", Title: "Actual Size", Group: "View", Icon: "zoom-original",
			Bindings: []frostui.KeyBinding{{Mods: ui.Cmd, Key: ui.Key0, Scope: frostui.Everywhere}},
			Enabled: func() (bool, string) {
				if a.zoom() == 1 {
					return false, "PocketGo is at 100%"
				}
				return true, ""
			},
			Run: func() { a.setZoom(1) }},
	}
}
