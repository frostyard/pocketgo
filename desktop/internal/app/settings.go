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
}

// openSettings opens the Settings window at page (empty for the first).
func (a *App) openSettings(page string) { a.prefs.layout.Show(page) }

// settingsDialog draws the Settings window while it is open.
func (a *App) settingsDialog(c *ui.Context) {
	if !a.prefs.layout.Open {
		return
	}
	a.prefs.zoom = state.ZoomLabel(a.zoom())
	a.prefs.layout.Dialog(c, "Settings", []frostui.SettingsPage{
		{ID: "appearance", Title: "Appearance", Icon: "preferences-desktop-theme", Keywords: "zoom scale font text size",
			Groups: []frostui.SettingsGroup{{Settings: []frostui.Setting{
				{Title: "Zoom", Subtitle: "Text, icons and spacing together (" + cmdKey() + " +, " + cmdKey() + " -, " + cmdKey() + " 0)",
					Choice: &a.prefs.zoom, Options: state.ZoomLabels(), Keywords: "scale font text size bigger smaller",
					OnChange: func() {
						if z, ok := state.ZoomOfLabel(a.prefs.zoom); ok {
							a.setZoom(z)
						}
					}},
			}}}},
	})
}

// cmdKey names the key the zoom keys take: Cmd on macOS, Ctrl elsewhere.
func cmdKey() string {
	if runtime.GOOS == "darwin" {
		return "⌘"
	}
	return "Ctrl"
}

// zoom is the window's zoom: the level nearest to the one saved, 100%
// when none is.
func (a *App) zoom() float32 { return state.Zoom(a.settings.Zoom) }

// setZoom zooms the window to the level nearest to z and saves it.
func (a *App) setZoom(z float32) {
	saved := state.ZoomSaved(z)
	if saved == a.settings.Zoom {
		return
	}
	a.settings.Zoom = saved
	_ = a.saveSettings()
}

// zoomBy zooms steps levels in (out when negative), within the levels.
func (a *App) zoomBy(steps int) {
	a.settings.Zoom = state.ZoomBy(a.settings.Zoom, steps)
	_ = a.saveSettings()
}

// settingsCommands open Settings and zoom the window, with the keys a
// browser zooms with: Cmd = (and Cmd + with Shift), Cmd - and Cmd 0, Ctrl
// off macOS.
func (a *App) settingsCommands() []frostui.Command {
	return []frostui.Command{
		{ID: "settings", Title: "Settings…", Group: "App", Icon: "preferences-system",
			Bindings: []frostui.KeyBinding{{Mods: ui.Cmd, Key: ui.KeyComma, Scope: frostui.Everywhere}},
			Run:      func() { a.openSettings("") }},
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
