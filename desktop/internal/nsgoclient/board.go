package nsgoclient

import (
	"fmt"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/frostyard/frostui/workspaceui"
	"github.com/frostyard/pocketgo/desktop/internal/state"
	"github.com/frostyard/pocketgo/proto"
	"github.com/nsgo-dev/nsgo/core"
	"github.com/nsgo-dev/nsgo/ui"
)

func (c *client) board(cx *core.Scope) core.Node {
	d := &c.store.Data
	byState := core.NewMemoFunc(cx, func() map[proto.ThreadState][]proto.Thread {
		c.revision.Get()
		lanes := make(map[proto.ThreadState][]proto.Thread)
		for _, t := range d.Threads() {
			key := t.State
			switch key {
			case proto.ThreadStarting, proto.ThreadOpen, proto.ThreadBlocked, proto.ThreadDone, proto.ThreadClosed:
			default:
				key = ""
			}
			lanes[key] = append(lanes[key], t)
		}
		return lanes
	}, nil)
	section := func(name string, expanded bool, count func() int, visible func() bool, content func(*core.Scope) core.Node) core.Node {
		if visible == nil {
			visible = func() bool { return count() > 0 }
		}
		return core.Show{When: visible, Then: func(*core.Scope) core.Node {
			open := expanded
			if name == "Closed" {
				source := c.nav.visits[c.nav.index].dest.card
				for _, thread := range byState.GetUntracked()[proto.ThreadClosed] {
					if fmt.Sprintf("thread-%d", thread.ID) == source {
						open = true
						break
					}
				}
			}
			return workspaceui.Section{Title: name, Count: count, InitiallyOpen: open, Content: content}
		}}
	}
	nodes := []core.Node{core.Text{ContentFunc: func() string { c.revision.Get(); return d.BoardNote() }, Style: ui.TypeLabel, StyleFunc: ui.Colored(ui.ColorsOf(cx).Muted)}}
	openBoard := core.Show{When: func() bool { return len(byState.Get()[proto.ThreadClosed]) < len(d.Threads()) },
		Then: func(cx *core.Scope) core.Node { return c.threadLanes(cx, byState.Get) }}
	closed := func() []proto.Thread { return byState.Get()[proto.ThreadClosed] }
	nodes = append(nodes, openBoard, section("Closed", false, func() int { return len(closed()) }, nil, func(cx *core.Scope) core.Node { return c.threadCards(cx, closed) }))
	nodes = append(nodes, section("Plans", true, func() int { c.revision.Get(); return len(d.Plans()) }, nil, func(*core.Scope) core.Node {
		return core.Dynamic(func(*core.Scope) core.Node {
			c.revision.Get()
			var cards []core.Node
			for _, p := range d.Plans() {
				cards = append(cards, workspaceui.Card{Children: []core.Node{c.plan(p)}})
			}
			return workspaceui.Stack(cards...)
		})
	}), section("Schedules", true, func() int { c.revision.Get(); return len(d.Schedules().Schedules) }, nil, func(*core.Scope) core.Node {
		return core.Dynamic(func(*core.Scope) core.Node {
			c.revision.Get()
			var cards []core.Node
			for _, s := range d.Schedules().Schedules {
				r := state.ScheduleRowOf(s, time.Now())
				cards = append(cards, workspaceui.Card{Children: []core.Node{title(r.ID), body(strings.Join(nonempty(r.When, r.Next, r.Action, r.Last, r.Problem), " · "))}})
			}
			return workspaceui.Stack(cards...)
		})
	}))
	proposals := func() int { c.revision.Get(); return len(d.BoardProposals()) }
	nodes = append(nodes, section("Night shift", true, proposals, func() bool {
		return proposals() > 0 || c.store.LaunchUncertain() || c.store.Launching().Err != "" || c.launchNote() != ""
	}, c.nightShift))
	return core.Scroll{Style: ui.FillRest, Children: []core.Node{core.View{
		Style: core.Style{Width: core.Pct(100), MaxWidth: core.Pt(1560), AlignSelf: core.AlignCenter, Padding: core.All(16), Gap: 14}, Children: nodes,
	}}}
}

func (c *client) threadLanes(cx *core.Scope, byState func() map[proto.ThreadState][]proto.Thread) core.Node {
	colors := ui.ColorsOf(cx)
	var columns []core.Node
	for _, lane := range []struct {
		key   proto.ThreadState
		title string
		color func() core.Color
	}{
		{proto.ThreadStarting, "Starting", colors.Muted},
		{proto.ThreadOpen, "In progress", colors.Accent},
		{proto.ThreadBlocked, "Blocked", colors.Warn},
		{proto.ThreadDone, "Done", colors.Up},
		{"", "Other", colors.Muted},
	} {
		items := func() []proto.Thread { return byState()[lane.key] }
		column := core.View{TestID: "board-lane-" + string(lane.key), Style: core.Style{MinWidth: core.Pt(0), Padding: core.All(8), CornerRadius: 10, BorderWidth: 1, Borders: core.Edges{Top: 3}},
			StyleFunc: func() core.Style {
				return core.Style{Background: colors.Inset(), BorderColor: colors.Border(), BorderColors: core.EdgeColors{Top: lane.color()}}
			},
			Children: []core.Node{workspaceui.Section{Title: lane.title, Count: func() int { return len(items()) }, InitiallyOpen: true, Content: func(cx *core.Scope) core.Node { return c.threadCards(cx, items) }}},
		}
		if lane.key == "" {
			columns = append(columns, core.Show{When: func() bool { return len(items()) > 0 }, Then: func(*core.Scope) core.Node { return column }})
		} else {
			columns = append(columns, column)
		}
	}
	return core.View{Style: core.Style{Display: core.DisplayGrid, GridTemplateColumns: "repeat(auto-fit, minmax(210px, 1fr))", Gap: 10}, Children: columns}
}

// Bound the native view count even when the closed archive has years of work.
func (c *client) threadCards(cx *core.Scope, items func() []proto.Thread) core.Node {
	const pageSize = 12
	page := core.NewSignal(cx, 0)
	cx.Effect(func() {
		key := c.noticeCard.Get()
		if key == "" {
			return
		}
		for i, t := range items() {
			if key == fmt.Sprintf("thread-%d", t.ID) {
				page.Set(i / pageSize)
				return
			}
		}
	})
	offset := func() int { return min(page.Get(), max(0, (len(items())-1)/pageSize)) * pageSize }
	rows := core.Each(func() []proto.Thread {
		threads := items()
		start := offset()
		return threads[start:min(start+pageSize, len(threads))]
	},
		func(t proto.Thread) int64 { return t.ID },
		func(cx *core.Scope, item func() proto.Thread, _ func() int) core.Node {
			report := core.NewSignal(cx, false)
			src := c.nav.visits[c.nav.index].dest
			details := core.NewSignal(cx, src.details && src.card == fmt.Sprintf("thread-%d", core.Untracked(cx, item).ID))
			// Other sessions can be busy while the owner reads a report.
			thread := core.NewMemoFunc(cx, item, func(a, b proto.Thread) bool { return reflect.DeepEqual(a, b) })
			return core.Dynamic(func(cx *core.Scope) core.Node {
				return c.cardAnchor(fmt.Sprintf("thread-%d", thread.Get().ID), c.threadCard(cx, thread.Get(), report, details))
			})
		})
	pager := core.Show{When: func() bool { return len(items()) > pageSize }, Then: func(*core.Scope) core.Node {
		return workspaceui.Row(ui.Button{Text: "Previous", Disabled: func() bool { return offset() == 0 }, OnPress: func() { page.Set(offset()/pageSize - 1) }},
			core.Text{ContentFunc: func() string {
				n := len(items())
				start := offset()
				return fmt.Sprintf("%d–%d of %d", start+1, min(start+pageSize, n), n)
			}},
			ui.Button{Text: "Next", Disabled: func() bool { return offset()+pageSize >= len(items()) }, OnPress: func() { page.Set(offset()/pageSize + 1) }})
	}}
	return workspaceui.Stack(rows, pager)
}

func (c *client) threadCard(cx *core.Scope, t proto.Thread, report, details *core.Signal[bool]) core.Node {
	d := &c.store.Data
	muted := func(text string) core.Node {
		return core.Text{Content: text, Style: ui.TypeCaption, StyleFunc: ui.Colored(ui.ColorsOf(cx).Muted), Selectable: core.SelectableOn}
	}
	meta := nonempty(fmt.Sprintf("T%d", t.ID), t.Agent)
	head := []core.Node{title(t.Title), muted(strings.Join(meta, " · "))}
	if t.State == proto.ThreadClosed {
		head = append(head, c.threadDetails(cx, t, report)...)
		return workspaceui.Card{Children: head}
	}
	mover := core.NewMemo(cx, func() state.Mover { c.revision.Get(); return d.NextMover(t) })
	head = append(head, workspaceui.Row(ui.Badge{TextFunc: func() string {
		switch mover.Get() {
		case state.MoverOwner:
			return "Your move"
		case state.MoverSupervisor:
			return "Supervisor review"
		default:
			return "Specialist working"
		}
	}, VariantFunc: func() ui.BadgeVariant {
		if mover.Get() == state.MoverOwner {
			return ui.BadgeWarn
		}
		return ui.BadgeNeutral
	}}))
	asks := core.NewMemoFunc(cx, func() []proto.Ask { c.revision.Get(); return d.AsksOf(t.ID) }, func(a, b []proto.Ask) bool {
		return slices.EqualFunc(a, b, func(x, y proto.Ask) bool { return x.ID == y.ID })
	})
	head = append(head, core.Dynamic(func(*core.Scope) core.Node {
		var links []core.Node
		for _, a := range asks.Get() {
			links = append(links, ui.Button{Text: fmt.Sprintf("View ask A%d", a.ID), Variant: ui.ButtonTinted, OnPress: func() {
				c.rememberCard(fmt.Sprintf("thread-%d", t.ID))
				c.goTo(destination{page: "Inbox", card: state.AskCard(a.ID)})
			}})
		}
		if len(links) == 0 {
			return nil
		}
		return workspaceui.Row(links...)
	}))
	if t.Task != "" {
		head = append(head, core.Show{When: func() bool { return !details.Get() }, Then: func(*core.Scope) core.Node { return core.Text{Content: t.Task, MaxLines: 3} }})
	}
	head = append(head, ui.Collapsible{Text: fmt.Sprintf("Details for T%d", t.ID), Trigger: []core.Node{core.Text{Content: "Details", Style: ui.TypeLabel}}, Variant: ui.CollapsibleArrowRight,
		Open: details.Get, OnOpenChange: details.Set, Content: core.Show{When: details.Get, Then: func(cx *core.Scope) core.Node { return workspaceui.Stack(c.threadDetails(cx, t, report)...) }}})
	return workspaceui.Card{Children: head}
}

func (c *client) threadDetails(cx *core.Scope, t proto.Thread, report *core.Signal[bool]) []core.Node {
	d := &c.store.Data
	var out []core.Node
	if context := strings.Join(nonempty(t.Repository, t.Branch, state.ThreadLinks(t)), " · "); context != "" {
		out = append(out, body(context))
	}
	if t.Task != "" {
		out = append(out, body(t.Task))
	}
	usage := core.NewMemo(cx, func() string { c.revision.Get(); return state.UsageShort(d.ThreadUsage(t)) })
	out = append(out, core.Show{When: func() bool { return usage.Get() != "" }, Then: func(*core.Scope) core.Node {
		return core.Text{ContentFunc: usage.Get, Style: ui.TypeCaption, StyleFunc: ui.Colored(ui.ColorsOf(cx).Muted)}
	}})
	if t.Report != "" {
		out = append(out, ui.Collapsible{Text: "Report", Variant: ui.CollapsibleArrowLeft, Open: report.Get, OnOpenChange: report.Set,
			Content: core.Show{When: report.Get, Then: func(*core.Scope) core.Node { return c.markdown(func() string { return t.Report }, nil) }}})
	}
	if shift := t.Shift; shift != nil {
		for _, line := range nonempty(state.ShiftLine(*shift), shift.Stop) {
			out = append(out, body(line))
		}
		for _, round := range shift.Rounds {
			out = append(out, body(fmt.Sprintf("Round %d · %s · %s", round.N, round.Verdict, round.Reason)))
		}
	}
	out = append(out, core.Dynamic(func(*core.Scope) core.Node {
		c.revision.Get()
		var pulls []core.Node
		for _, p := range d.PullsOf(t.ID) {
			pulls = append(pulls, c.pull(p))
		}
		return core.Group(pulls)
	}))
	actions := []core.Node{}
	if t.Session != 0 {
		actions = append(actions, c.button("Specialist", func() {
			c.rememberCard(fmt.Sprintf("thread-%d", t.ID))
			c.nav.visits[c.nav.index].dest.details = true
			c.openSession(t.Session)
		}))
	}
	if t.Supervisor != 0 {
		actions = append(actions, c.button("Supervisor", func() {
			c.rememberCard(fmt.Sprintf("thread-%d", t.ID))
			c.nav.visits[c.nav.index].dest.details = true
			c.openSession(t.Supervisor)
		}))
	}
	if state.CanResumeShift(t) {
		actions = append(actions, c.button("Resume shift", func() { c.store.ResumeShift(t.ID) }))
	}
	if state.CanStopShift(t) {
		actions = append(actions, c.button("Stop shift", func() { c.confirm("Stop shift?", t.Title, "Stop", func() { c.store.StopShift(t.ID) }) }))
	}
	if t.State != proto.ThreadClosed {
		actions = append(actions, c.button("Close thread", func() {
			msg := t.Title
			if state.PlanThread(t) {
				msg += ". Closing this thread also drops its plan."
			}
			c.confirm("Close thread?", msg, "Close thread", func() { c.store.CloseThread(t.ID) })
		}))
	}

	if len(actions) > 0 {
		out = append(out, workspaceui.Row(actions...))
	}
	out = append(out, core.Dynamic(func(*core.Scope) core.Node {
		c.revision.Get()
		var errors []core.Node
		for _, line := range nonempty(c.store.Closing(t.ID).Err, c.store.ShiftChanging(t.ID).Err) {
			errors = append(errors, body(line))
		}
		return core.Group(errors)
	}))
	return out
}

func (c *client) launchNote() string {
	if launch := c.store.Launched(); launch != nil && launch.End.After(time.Now()) {
		return fmt.Sprintf("Launched %d proposals until %s", len(launch.Items), state.ClockAt(launch.End, time.Now()))
	}
	return ""
}

func (c *client) nightShift(cx *core.Scope) core.Node {
	until := core.NewSignal(cx, c.shiftEnd)
	rows := core.Dynamic(func(*core.Scope) core.Node {
		c.revision.Get()
		var cards []core.Node
		for _, p := range c.store.Data.BoardProposals() {
			label := "Pick"
			if c.store.Picked(p.ID) {
				label = "Unpick"
			}
			children := []core.Node{title(p.Title), body(p.Task), body(c.store.Data.ProposalCaption(p, time.Now())),
				workspaceui.Row(c.button(label, func() { c.store.TogglePick(p.ID) }), c.button("Drop proposal", func() { c.confirm("Drop proposal?", p.Title, "Drop", func() { c.store.DropProposal(p.ID, p.Rev) }) }))}
			if err := c.store.Dropping(p.ID).Err; err != "" {
				children = append(children, body(err))
			}
			cards = append(cards, workspaceui.Card{Children: children})
		}
		return core.Group(cards)
	})
	controls := core.Show{When: func() bool {
		c.revision.Get()
		return len(c.store.Data.BoardProposals()) > 0 || c.store.LaunchUncertain()
	}, Then: func(*core.Scope) core.Node {
		return workspaceui.Card{Children: []core.Node{ui.Input{Label: "Shift end", Placeholder: "Optional end time (HH:MM)", Value: until.Get, OnChange: func(s string) { c.shiftEnd = s; until.Set(s); c.store.LaunchEndEdited() }},
			workspaceui.Row(c.button("Launch selected", func() {
				end := until.Get()
				ids := c.store.PlannedPicks(end)
				stop, err := c.store.PlannedEnd(end)
				if err != nil {
					c.result(err)
					return
				}
				if len(ids) == 0 {
					return
				}
				c.confirm("Launch selected proposals?", fmt.Sprintf("Start work on %v until %s.", ids, state.ClockAt(stop, time.Now())), "Launch", func() {
					if slices.Equal(ids, c.store.PlannedPicks(end)) {
						c.store.LaunchPicks(end)
					}
				})
			}))}}
	}}
	feedback := core.Dynamic(func(*core.Scope) core.Node {
		c.revision.Get()
		var lines []core.Node
		for _, s := range nonempty(c.store.Launching().Err, c.launchNote()) {
			lines = append(lines, body(s))
		}
		return core.Group(lines)
	})
	return workspaceui.Stack(rows, controls, feedback)
}
