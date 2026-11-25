package nsgoclient

import (
	"strings"
	"time"

	"github.com/frostyard/frostui/workspaceui"
	"github.com/frostyard/pocketgo/desktop/internal/state"
	"github.com/nsgo-dev/nsgo/core"
	"github.com/nsgo-dev/nsgo/icons/lucide"
	"github.com/nsgo-dev/nsgo/ui"
)

// The Activity place shows what the client did and what went wrong: each
// line with its time and its level, the newest last, and Copy for all of
// them. It holds the last activityMost lines.

// activityMost is how many lines the Activity place keeps.
const activityMost = 2000

// activityLine is one line of the Activity place.
type activityLine struct {
	at    time.Time
	level state.Level
	text  string
}

// logLine adds a line.
func (c *client) logLine(level state.Level, text string) {
	c.log = append(c.log, activityLine{at: time.Now(), level: level, text: text})
	if len(c.log) > activityMost {
		c.log = c.log[len(c.log)-activityMost:]
	}
}

// levelName is how a level reads in a copied line.
func levelName(l state.Level) string {
	switch l {
	case state.LevelWarn:
		return "warning"
	case state.LevelError:
		return "error"
	case state.LevelCommand:
		return "command"
	}
	return "info"
}

// activityText is the lines as text, for the clipboard.
func activityText(lines []activityLine) string {
	var b strings.Builder
	for _, l := range lines {
		b.WriteString(l.at.Format("15:04:05") + " " + levelName(l.level) + " " + l.text + "\n")
	}
	return b.String()
}

func (c *client) activity(cx *core.Scope) core.Node {
	return core.Dynamic(func(cx *core.Scope) core.Node {
		c.revision.Get()
		colors := ui.ColorsOf(cx)
		if len(c.log) == 0 {
			return workspaceui.EmptyState{Icon: lucide.Activity, Title: "No activity yet", Hint: "What the client does and what goes wrong shows here."}
		}
		rows := []core.Node{workspaceui.Row(ui.Button{Text: "Copy", Label: "Copy the activity", Icon: lucide.Copy, Variant: ui.ButtonSecondary, Size: ui.SizeSmall,
			OnPress: func() { cx.SetClipboard(activityText(c.log)) }})}
		for _, l := range c.log {
			ink := colors.Text
			var mark core.Node = core.View{Style: core.Style{Width: core.Pt(14), FlexShrink: 0}}
			switch l.level {
			case state.LevelWarn:
				ink, mark = colors.Warn, ui.Icon(lucide.TriangleAlert, 14, colors.Warn)
			case state.LevelError:
				ink, mark = colors.Danger, ui.Icon(lucide.CircleAlert, 14, colors.Danger)
			case state.LevelCommand:
				mark = ui.Icon(lucide.ChevronRight, 14, colors.Accent)
			}
			rows = append(rows, core.View{Label: levelName(l.level) + ": " + l.text, Style: core.Style{Direction: core.Row, AlignItems: core.AlignStart, Gap: workspaceui.SpaceS}, Children: []core.Node{
				core.Text{Content: l.at.Format("15:04:05"), Style: ui.TypeCode.Merge(core.Style{FlexShrink: 0}), StyleFunc: ui.Colored(colors.Muted)},
				mark,
				core.Text{Content: l.text, Selectable: core.SelectableOn, Style: core.Style{FlexGrow: 1, FlexShrink: 1, MinWidth: core.Pt(0)}, StyleFunc: ui.Colored(ink)}}})
		}
		return workspaceui.Page(rows...)
	})
}
