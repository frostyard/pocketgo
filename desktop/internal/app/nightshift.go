package app

import (
	"time"

	"github.com/egoist/mygo/ui"
	"github.com/frostyard/frostui"

	"github.com/frostyard/pocketgo/desktop/internal/state"
	"github.com/frostyard/pocketgo/proto"
)

// The night shift on the Work Board (ADR-0034): the Proposals section,
// where the owner picks proposals and launches them until an end, then the
// Schedules section. state holds every rule (the picks, the end, which
// proposals show); these views only draw them.

// endChoices are the end chooser's segments: in 4 hours, or until a clock
// time (launchEnd's values).
var endChoices = []string{"In 4 hours", "Until"}

const endUntil = 1

// boardSection draws one section of the board as a card: its icon, its
// title and its count, then what fn draws.
func boardSection(c *ui.Context, key, title, icon string, count int, fn func()) {
	t := c.Theme()
	ui.Column(c.Key(key)).Gap(frostui.SpaceS).Padding(frostui.SpaceM, frostui.SpaceL).AlignItems(ui.Stretch).
		Radius(10).Background(frostui.Panel(t)).BorderWidth(1, 1, 1, 1).BorderColor(t.Border).
		Role(ui.RoleGroup).Label(title).Children(func() {
		ui.Row(c).Gap(frostui.SpaceXS).AlignItems(ui.Center).Children(func() {
			frostui.Icon(c, icon)
			frostui.Strong(c, title)
			if count > 0 {
				frostui.Caption(c, " "+itoa(count))
			}
		})
		fn()
	})
}

// proposalsSection lists the proposals still proposed, in board order,
// each with a Pick checkbox and a Drop button, then the end chooser and
// Launch picks (N).
func (a *App) proposalsSection(c *ui.Context) {
	d := &a.st.Data
	status, problem := d.ProposalsStatus()
	list := d.BoardProposals()
	now := a.opts.Now()
	boardSection(c, "proposals", "Proposals", "view-list", len(list), func() {
		if status == state.ListFailed {
			a.listProblem(c, "Loading the proposals failed: "+problem+". Proposals may be missing or out of date.")
		}
		switch {
		case len(list) > 0:
			launching := a.st.Launching()
			for i, p := range list {
				if i > 0 {
					ui.Divider(c)
				}
				a.proposalRow(c, p, now, launching.Busy)
			}
			ui.Divider(c)
			a.launchRow(c, now, launching)
		case status == state.ListLoading:
			frostui.Loading(c, "Loading the proposals…")
		case status == state.ListLoaded:
			frostui.Muted(c, "No proposal waits. When a gather asks the team for work, its proposals show here for you to pick and launch.").Wrap()
		}
		if l := a.st.Launched(); l != nil && l.End.After(now) {
			frostui.Toned(c, "Launched L"+itoa(int(l.ID))+": "+plural(len(l.Items), "proposal", "proposals")+" until "+
				state.ClockAt(l.End, now)+", from the supervisor's standing session.", frostui.Success).Wrap()
		}
	})
}

// proposalRow is one proposal: its Pick checkbox, its title, who proposed
// it where and when, its note, and Drop.
func (a *App) proposalRow(c *ui.Context, p proto.Proposal, now time.Time, launching bool) {
	key := "proposal:" + itoa(int(p.ID))
	name := "#" + itoa(int(p.ID))
	dropping := a.st.Dropping(p.ID)
	ui.Column(c.Key(key)).Gap(frostui.SpaceXS).AlignItems(ui.Stretch).Children(func() {
		ui.Row(c).Gap(frostui.SpaceS).AlignItems(ui.Start).Children(func() {
			picked := a.st.Picked(p.ID)
			if ui.Checkbox(c.Key(key+":pick"), &picked, "").Label("Pick "+name).Disabled(launching).Padding(2, 0, 0, 0).Changed() {
				a.st.TogglePick(p.ID)
			}
			ui.Column(c).Grow(1).Gap(2).AlignItems(ui.Stretch).Children(func() {
				frostui.Strong(c, name+" · "+p.Title).Wrap()
				frostui.Caption(c, a.st.Data.ProposalCaption(p, now)).Wrap()
				if p.Note != "" {
					frostui.Muted(c, p.Note).Wrap()
				}
			})
			if ui.Button(c.Key(key+":drop"), "Drop").Label("Drop " + name).Tooltip("Take " + name + " off the board").
				Disabled(dropping.Busy || launching).Clicked() {
				a.st.DropProposal(p.ID, p.Rev)
			}
		})
		if dropping.Busy {
			frostui.Loading(c, "Dropping "+name+"…")
		} else if dropping.Err != "" {
			frostui.Problem(c, "Dropping "+name+" failed: "+dropping.Err).Wrap()
		}
	})
}

// launchRow is the end chooser, the end it names, and Launch picks (N),
// with where the launch stands.
func (a *App) launchRow(c *ui.Context, now time.Time, launching state.Pending) {
	picks := len(a.st.Picks())
	until, chooseErr := state.UntilOf(a.launchEnd == endUntil, a.launchClock)
	end, endErr := a.st.PlannedEnd(until)
	ui.Row(c.Key("launch")).Gap(frostui.SpaceS).AlignItems(ui.Center).Wrap().Children(func() {
		ui.Segmented(c.Key("launch:end"), &a.launchEnd, endChoices...).Label("Launch ends").Disabled(launching.Busy)
		if a.launchEnd == endUntil {
			ui.TextInput(c.Key("launch:clock"), &a.launchClock).Label("Until time").Placeholder("HH:MM").Width(84).Disabled(launching.Busy)
		}
		switch {
		case chooseErr != nil:
			frostui.Caption(c, capitalize(chooseErr.Error())+".").Wrap().Grow(1)
		case endErr != nil:
			frostui.Toned(c, capitalize(endErr.Error())+".", frostui.Warning).Wrap().Grow(1)
		default:
			frostui.Caption(c, "Ends "+state.ClockAt(end, now)).Grow(1)
		}
		label := "Launch picks (" + itoa(picks) + ")"
		if ui.PrimaryButton(c.Key("launch:go"), label).Label(label).
			Tooltip("Launch the picked proposals as shifts from the supervisor's standing session, until the end").
			Disabled(picks == 0 || chooseErr != nil || endErr != nil || launching.Busy).Clicked() {
			a.st.LaunchPicks(until)
		}
	})
	if launching.Busy {
		frostui.Loading(c, "Launching "+plural(picks, "proposal", "proposals")+"…")
	} else if launching.Err != "" {
		frostui.Problem(c, launching.Err).Wrap()
	}
}

// schedulesSection lists the schedules: the owner's, then the
// supervisors' own, each with how it repeats, its next time, its last run
// and its problem.
func (a *App) schedulesSection(c *ui.Context) {
	d := &a.st.Data
	status, problem := d.SchedulesStatus()
	list := d.Schedules()
	now := a.opts.Now()
	boardSection(c, "schedules", "Schedules", "alarm", len(list.Schedules), func() {
		if status == state.ListFailed {
			a.listProblem(c, "Loading the schedules failed: "+problem+". Schedules may be missing or out of date.")
		}
		if list.Problem != "" {
			frostui.Toned(c, "None of your schedules file goes out: "+list.Problem, frostui.Warning).Wrap()
		}
		switch {
		case len(list.Schedules) > 0:
			for i, s := range list.Schedules {
				if i > 0 {
					ui.Divider(c)
				}
				scheduleRow(c, state.ScheduleRowOf(s, now))
			}
		case status == state.ListLoading:
			frostui.Loading(c, "Loading the schedules…")
		case status == state.ListLoaded:
			frostui.Muted(c, "No schedule. Your schedules file and a supervisor's team_schedule add them.").Wrap()
		}
	})
}

// scheduleRow is one schedule.
func scheduleRow(c *ui.Context, row state.ScheduleRow) {
	ui.Column(c.Key("schedule:" + row.ID)).Gap(2).AlignItems(ui.Stretch).Children(func() {
		ui.Row(c).Gap(frostui.SpaceS).AlignItems(ui.Center).Children(func() {
			frostui.Strong(c, row.ID).SingleLine()
			frostui.Caption(c, row.When).SingleLine().Grow(1)
			if row.Next != "" {
				frostui.Caption(c, row.Next).SingleLine()
			}
		})
		frostui.Muted(c, row.Action).MaxLines(2).Ellipsis("…").Wrap()
		if row.Last != "" {
			frostui.Caption(c, row.Last).Wrap()
		}
		if row.Problem != "" {
			frostui.Toned(c, row.Problem, frostui.Warning).Wrap()
		}
	})
}
