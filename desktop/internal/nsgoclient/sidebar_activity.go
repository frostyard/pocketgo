package nsgoclient

import (
	"fmt"
	"strconv"
	"strings"

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
	add := func(n int, state string, icon func() *core.SVG, color func(ui.Colors) core.Color) {
		if n == 0 {
			return
		}
		label := fmt.Sprintf("%d %s", n, state)
		labels = append(labels, label)
		marks = append(marks, core.Func(func(cx *core.Scope) core.Node {
			ink := func() core.Color { return color(ui.ColorsOf(cx)) }
			return ui.Tooltip{Text: label, Trigger: core.View{
				Style:     core.Style{Direction: core.Row, AlignItems: core.AlignCenter, Gap: 3, Padding: core.Symmetric(2, 5), CornerRadius: 8},
				StyleFunc: func() core.Style { return core.Style{Background: ui.Alpha(ink(), 0.12)} },
				Children:  []core.Node{ui.Icon(icon, 12, ink), ui.Text(strconv.Itoa(n), 11, ui.Semibold, ink)},
			}}
		}))
	}
	add(group.Running, "running", lucide.Activity, ui.Colors.Accent)
	add(group.Waiting, "waiting", lucide.CirclePause, ui.Colors.Warn)
	if len(marks) == 0 {
		return nil, ""
	}
	return core.View{Style: core.Style{Direction: core.Row, Gap: 4, AlignItems: core.AlignCenter, FlexShrink: 0}, Children: marks}, strings.Join(labels, ", ")
}
