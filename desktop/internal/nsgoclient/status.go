package nsgoclient

import (
	"strconv"

	"github.com/frostyard/frostui/workspaceui"
	"github.com/frostyard/pocketgo/desktop/internal/state"
	"github.com/nsgo-dev/nsgo/core"
	"github.com/nsgo-dev/nsgo/ui"
)

// The status bar is under the window's content, in every place: what
// waits for the owner and what runs, a GitHub sign-in the keyring did not
// keep, what the sessions used, and the service's version. It holds what
// the MyGo client's status bar holds, from the same rules.

// counted is "1 session waits" or "3 sessions wait".
func counted(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return strconv.Itoa(n) + " " + many
}

func (c *client) statusBar() core.Node {
	return core.Dynamic(func(cx *core.Scope) core.Node {
		c.revision.Get()
		colors := ui.ColorsOf(cx)
		d := &c.store.Data
		note := func(text string) core.Node {
			return core.Text{Content: text, MaxLines: 1, Style: ui.TypeCaption, StyleFunc: ui.Colored(colors.Muted)}
		}
		// What waits is a way to the Inbox.
		waits := func(n int, one, many string) core.Node {
			text := counted(n, one, many)
			return ui.Tooltip{Text: "Open the Inbox", Trigger: ui.Button{Text: text, Variant: ui.ButtonGhost, Size: ui.SizeSmall, OnPress: func() { c.navigate("Inbox") }}}
		}
		var start []core.Node
		waiting, running := d.Counts()
		if waiting > 0 {
			start = append(start, waits(waiting, "session waits", "sessions wait"))
		}
		if n := len(d.Asks()); n > 0 {
			start = append(start, waits(n, "ask waits", "asks wait"))
		}
		if n := len(d.Lessons()); n > 0 {
			start = append(start, waits(n, "lesson waits", "lessons wait"))
		}
		if n := len(d.PullsToReview()); n > 0 {
			start = append(start, waits(n, "pull request waits", "pull requests wait"))
		}
		if running > 0 {
			start = append(start, note(counted(running, "session runs", "sessions run")))
		}
		// A refreshed GitHub sign-in the keyring did not keep, whatever
		// the view: closing the window would lose it.
		if c.gh != nil && c.gh.CanSaveAgain() {
			start = append(start, ui.Tooltip{Text: "Save the GitHub sign-in again", Trigger: ui.Button{Text: "GitHub sign-in not saved", Variant: ui.ButtonDanger, Size: ui.SizeSmall, OnPress: c.saveGitHubAgain}})
		}
		var end []core.Node
		// A narrow window keeps what waits, and leaves the rest to Settings.
		if cx.Env().Window().Width >= 720 {
			if today, week, shown := c.store.UsageTotals(); shown {
				zone := d.UsageReport().Zone
				days := strconv.Itoa(state.WeekDays)
				end = append(end,
					ui.Tooltip{Text: today.Tooltip("Today", zone), Trigger: note(today.Line("Today"))},
					ui.Tooltip{Text: week.Tooltip("The last "+days+" days", zone), Trigger: note(week.Line(days + " days"))})
			}
			if h := c.store.Hello(); h != nil {
				end = append(end, ui.Tooltip{Text: "The service's version and this device's name", Trigger: note("pocketd " + h.Server + " · " + h.Device)})
			}
		}
		if len(start) == 0 && len(end) == 0 {
			return core.Group{}
		}
		children := append(append(start, ui.Spacer{}), end...)
		return core.View{TestID: "status-bar", Style: core.Style{Direction: core.Row, AlignItems: core.AlignCenter, Gap: workspaceui.SpaceM, FlexShrink: 0,
			Padding: core.Symmetric(workspaceui.SpaceXS, workspaceui.SpaceM), Borders: core.Edges{Top: 1}},
			StyleFunc: func() core.Style { return core.Style{BorderColor: colors.Border()} }, Children: children}
	})
}

// saveGitHubAgain saves a refreshed GitHub sign-in the keyring did not
// keep, and says how it went.
func (c *client) saveGitHubAgain() {
	c.gh.SaveAgain(func(err error) {
		if err != nil {
			c.showNotice(ui.ToastError, "GitHub sign-in not saved", "Saving the GitHub sign-in: "+err.Error())
			return
		}
		c.showNotice(ui.ToastSuccess, "Saved the GitHub sign-in", "")
		c.changed()
	})
}
