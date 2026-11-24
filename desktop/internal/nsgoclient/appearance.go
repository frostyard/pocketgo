package nsgoclient

import "github.com/nsgo-dev/nsgo/core"

// The Theme setting is this device's: the client's storage keeps it under
// appearanceKey as "System", "Light" or "Dark". No entry follows the system.
const appearanceKey = "appearance"

// restoreAppearance shows the saved choice. It runs after workspaceui.Apply
// and before the first view is built.
func (c *client) restoreAppearance() {
	switch mode, _ := c.cx.Storage().GetItem(appearanceKey); mode {
	case "Dark":
		c.cx.SetAppearance(core.AppearanceDark)
	case "Light":
		c.cx.SetAppearance(core.AppearanceLight)
	}
}

// saveAppearance keeps the choice Settings made. The kit's control has
// already shown it.
func (c *client) saveAppearance(mode core.Appearance) {
	name := "System"
	switch mode {
	case core.AppearanceLight:
		name = "Light"
	case core.AppearanceDark:
		name = "Dark"
	}
	c.result(c.cx.Storage().SetItem(appearanceKey, name))
}
