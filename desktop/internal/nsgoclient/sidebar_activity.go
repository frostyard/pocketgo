package nsgoclient

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/frostyard/frostui/workspaceui"
	"github.com/frostyard/pocketgo/desktop/internal/state"
	"github.com/nsgo-dev/nsgo/core"
	"github.com/nsgo-dev/nsgo/icons/lucide"
	"github.com/nsgo-dev/nsgo/ui"
)

// GroupRow counts every session, including workers and sessions beyond the
// sidebar's display limit. Keep both totals visible when some work is waiting.
func agentActivity(group state.GroupRow) (core.Node, string) {
	var marks []core.Node
	var labels []string
	add := func(n int, state string, icon func() *core.SVG, color func(ui.Colors) core.Color, variant ui.BadgeVariant) {
		if n == 0 {
			return
		}
		label := fmt.Sprintf("%d %s", n, state)
		labels = append(labels, label)
		marks = append(marks, core.Func(func(cx *core.Scope) core.Node {
			ink := func() core.Color { return color(ui.ColorsOf(cx)) }
			// The count is NSGo's badge, and its icon says which count it
			// is to an eye that does not tell the 2 colors apart.
			return ui.Tooltip{Text: label, Trigger: core.View{
				Style:    core.Style{Direction: core.Row, AlignItems: core.AlignCenter, Gap: workspaceui.SpaceXS},
				Children: []core.Node{ui.Icon(icon, 12, ink), ui.Badge{Text: strconv.Itoa(n), Variant: variant}},
			}}
		}))
	}
	add(group.Running, "running", lucide.Activity, ui.Colors.Accent, ui.BadgeAccent)
	add(group.Waiting, "waiting", lucide.CirclePause, ui.Colors.Warn, ui.BadgeWarn)
	if len(marks) == 0 {
		return nil, ""
	}
	return core.View{Style: core.Style{Direction: core.Row, Gap: workspaceui.SpaceS, AlignItems: core.AlignCenter, FlexShrink: 0}, Children: marks}, strings.Join(labels, ", ")
}
