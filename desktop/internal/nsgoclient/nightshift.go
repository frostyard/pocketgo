package nsgoclient

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/frostyard/frostui/workspaceui"
	"github.com/frostyard/pocketgo/desktop/internal/state"
	"github.com/frostyard/pocketgo/proto"
	"github.com/nsgo-dev/nsgo/core"
	"github.com/nsgo-dev/nsgo/icons/lucide"
	"github.com/nsgo-dev/nsgo/ui"
)

// The night shift on the work board (ADR-0034): the proposals, which the
// owner picks from and launches until an end, and the schedules. state
// holds every rule (the picks, the end, which proposals show); these views
// only draw them.

// schedules lists the schedules: the owner's, then the supervisors' own,
// each with how it repeats, its next time, its last run and its problem.
func (c *client) schedules(*core.Scope) core.Node {
	d := &c.store.Data
	return core.Dynamic(func(*core.Scope) core.Node {
		c.revision.Get()
		status, problem := d.SchedulesStatus()
		list := d.Schedules()
		var out []core.Node
		if status == state.ListFailed {
			out = append(out, c.listProblem("the schedules", problem, "Schedules may be missing or out of date.", c.store.Refresh))
		}
		if list.Problem != "" {
			out = append(out, c.problemBanner(workspaceui.Warning, "None of your schedules file goes out: "+list.Problem, nil))
		}
		switch {
		case len(list.Schedules) > 0:
			for _, s := range list.Schedules {
				out = append(out, c.scheduleRow(state.ScheduleRowOf(s, timeNow())))
			}
		case status == state.ListLoading:
			out = append(out, body("Loading the schedules…"))
		case status == state.ListLoaded:
			out = append(out, c.note("No schedule. Your schedules file, a supervisor's team_schedule and a private agent's schedule add them."))
		}
		return workspaceui.Stack(out...)
	})
}

// scheduleRow is one schedule: its id, how it repeats and its next time
// on a line, then what it sends, what its last time did, and why it does
// not go out.
func (c *client) scheduleRow(row state.ScheduleRow) core.Node {
	colors := ui.ColorsOf(c.cx)
	head := []core.Node{title(row.ID), core.Text{Content: row.When, Selectable: core.SelectableOn,
		Style: ui.TypeCaption.Merge(core.Style{FlexGrow: 1, FlexShrink: 1}), StyleFunc: ui.Colored(colors.Muted)}}
	if row.Next != "" {
		head = append(head, c.caption(row.Next))
	}
	out := []core.Node{workspaceui.Row(head...), core.Text{Content: row.Action, MaxLines: 2, Selectable: core.SelectableOn}}
	if row.Last != "" {
		out = append(out, c.caption(row.Last))
	}
	if row.Problem != "" {
		out = append(out, core.Text{Content: row.Problem, Selectable: core.SelectableOn, StyleFunc: ui.Colored(colors.Warn)})
	}
	return workspaceui.Card{Children: out}
}

// timeNow is the client's clock, in its zone: the store computes a
// launch's end from it, and the board says its times by it. It is the wall
// clock; the suite sets pocketd's test server's (TestMain).
var timeNow = time.Now

// launchPlan is what a press of Launch picks would send now, and what
// the end chooser says of it.
type launchPlan struct {
	until string
	picks []int64
	end   time.Time
	// says is the end ("Ends tomorrow 06:45"), or why there is none: no
	// clock time written (ok false), or one that is no clock time (ok
	// false, warn true).
	says     string
	ok, warn bool
}

// nightShift lists the proposals still proposed, in board order, each
// with Pick and Drop, then the end chooser and Launch picks (N), and
// where the launch stands.
func (c *client) nightShift(cx *core.Scope) core.Node {
	d := &c.store.Data
	hours := core.NewSignal(cx, c.launchHours)
	clock := core.NewSignal(cx, c.launchClock)
	busy := func() bool { c.revision.Get(); return c.store.Launching().Busy }
	plan := func() launchPlan {
		c.revision.Get()
		until, chooseErr := state.UntilOf(!hours.Get(), clock.Get())
		// What the press sends: a launch that failed goes again as it was.
		p := launchPlan{until: until, picks: c.store.PlannedPicks(until)}
		end, endErr := c.store.PlannedEnd(until)
		switch {
		case chooseErr != nil:
			p.says = capitalize(chooseErr.Error()) + "."
		case endErr != nil:
			p.says, p.warn = capitalize(endErr.Error())+".", true
		default:
			p.end, p.ok, p.says = end, true, "Ends "+state.ClockAt(end, timeNow())
		}
		return p
	}
	rows := core.Dynamic(func(*core.Scope) core.Node {
		c.revision.Get()
		status, problem := d.ProposalsStatus()
		list := d.BoardProposals()
		var out []core.Node
		if status == state.ListFailed {
			out = append(out, c.listProblem("the proposals", problem, "Proposals may be missing or out of date.", c.store.Refresh))
		}
		for _, p := range list {
			out = append(out, c.proposal(p, busy))
		}
		// A launch whose answer was lost is offered again though its picks
		// left the board: the section then holds its launch row.
		if len(list) == 0 && !c.store.LaunchUncertain() {
			switch status {
			case state.ListLoading:
				out = append(out, body("Loading the proposals…"))
			case state.ListLoaded:
				out = append(out, c.note("No proposal waits. When a gather asks the team for work, its proposals show here for you to pick and launch."))
			}
		}
		return core.Group(out)
	})
	controls := core.Show{When: func() bool {
		c.revision.Get()
		return len(d.BoardProposals()) > 0 || c.store.LaunchUncertain()
	}, Then: func(cx *core.Scope) core.Node {
		colors := ui.ColorsOf(cx)
		// A change of the end is the owner's: a launch that failed then
		// goes as a new one.
		chooser := ui.Tabs[bool]{Label: "Launch ends", Value: hours.Get, Options: []ui.Option[bool]{
			{Value: false, Label: "Until", Disabled: busy},
			{Value: true, Label: fmt.Sprintf("In %d hours", state.LaunchChoiceHours), Disabled: busy},
		}, OnChange: func(v bool) {
			c.launchHours = v
			hours.Set(v)
			c.store.LaunchEndEdited()
			c.changed()
		}}
		field := core.Show{When: func() bool { return !hours.Get() }, Then: func(*core.Scope) core.Node {
			return ui.Input{Label: "Until time", Placeholder: "HH:MM", Value: clock.Get, Disabled: busy, Style: core.Style{Width: core.Pt(untilWidth)},
				OnChange: func(s string) {
					c.launchClock = s
					clock.Set(s)
					c.store.LaunchEndEdited()
					c.changed()
				}}
		}}
		ends := core.Text{ContentFunc: func() string { return plan().says }, Selectable: core.SelectableOn,
			Style: ui.TypeCaption.Merge(core.Style{FlexGrow: 1, FlexShrink: 1, MinWidth: core.Pt(untilWidth)}),
			StyleFunc: func() core.Style {
				if plan().warn {
					return core.Style{Color: colors.Warn()}
				}
				return core.Style{Color: colors.Muted()}
			}}
		label := func() string { return fmt.Sprintf("Launch picks (%d)", len(plan().picks)) }
		launch := ui.Tooltip{Text: "Launch the picked proposals as shifts from the supervisor's standing session, until the end",
			Trigger: ui.Button{TextFunc: label, LabelFunc: label, Variant: ui.ButtonPrimary,
				Disabled: func() bool { p := plan(); return len(p.picks) == 0 || !p.ok || busy() },
				OnPress: func() {
					p := plan()
					if len(p.picks) == 0 || !p.ok {
						return
					}
					names := make([]string, len(p.picks))
					for i, id := range p.picks {
						names[i] = fmt.Sprintf("#%d", id)
					}
					c.confirm("Launch "+counted(len(p.picks), "proposal", "proposals")+"?",
						"Launch "+state.JoinAnd(names)+" as shifts from the supervisor's standing session, until "+state.ClockAt(p.end, timeNow())+".",
						"Launch", func() {
							// What the confirmation named, or nothing: a pick
							// the board took away meanwhile is not launched
							// unseen.
							if slices.Equal(p.picks, c.store.PlannedPicks(p.until)) {
								c.store.LaunchPicks(p.until)
							}
						})
				}}}
		return workspaceui.Card{Children: []core.Node{workspaceui.Row(chooser, field, ends, launch)}}
	}}
	feedback := core.Dynamic(func(*core.Scope) core.Node {
		c.revision.Get()
		var lines []core.Node
		launching := c.store.Launching()
		// A launch refused as its last pick left the board still says why.
		if launching.Busy {
			lines = append(lines, body("Launching "+counted(len(plan().picks), "proposal", "proposals")+"…"))
		} else if launching.Err != "" {
			lines = append(lines, c.problemBanner(workspaceui.Danger, launching.Err, nil))
		}
		if l := c.store.Launched(); l != nil && l.End.After(timeNow()) {
			lines = append(lines, workspaceui.Banner{Tone: workspaceui.Success, Icon: lucide.CircleCheck,
				Text: fmt.Sprintf("Launched L%d: %s until %s, from the supervisor's standing session.", l.ID, counted(len(l.Items), "proposal", "proposals"), state.ClockAt(l.End, timeNow()))})
		}
		return core.Group(lines)
	})
	return workspaceui.Stack(rows, controls, feedback)
}

// untilWidth is the room of the end chooser's clock time, HH:MM.
const untilWidth core.Points = 84

// proposal is one proposal: its number and title, who proposed it where
// and when, its task and its note, with Pick and Drop.
func (c *client) proposal(p proto.Proposal, launching func() bool) core.Node {
	name := fmt.Sprintf("#%d", p.ID)
	pick := "Pick"
	if c.store.Picked(p.ID) {
		pick = "Unpick"
	}
	dropping := c.store.Dropping(p.ID)
	children := []core.Node{title(name + " · " + p.Title), c.caption(c.store.Data.ProposalCaption(p, timeNow()))}
	for _, line := range nonempty(p.Task, p.Note) {
		children = append(children, body(line))
	}
	choose := c.button(pick, func() { c.store.TogglePick(p.ID) })
	choose.Label, choose.Disabled = pick+" "+name, launching
	drop := c.button("Drop", func() {
		c.destroy("Drop "+name+"?", p.Title, "Drop "+name, func() { c.store.DropProposal(p.ID, p.Rev) })
	})
	drop.Label, drop.Disabled = "Drop "+name, func() bool { return dropping.Busy || launching() }
	children = append(children, workspaceui.Row(choose, ui.Tooltip{Text: "Take " + name + " off the board", Trigger: drop}))
	if dropping.Busy {
		children = append(children, body("Dropping "+name+"…"))
	} else if dropping.Err != "" {
		children = append(children, c.problemBanner(workspaceui.Danger, "Dropping "+name+" failed: "+dropping.Err, nil))
	}
	return workspaceui.Card{Children: children}
}

// capitalize starts a sentence an error's text makes.
func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
