package app

import (
	"strings"

	"github.com/egoist/mygo/ui"
	"github.com/frostyard/frostui"

	"github.com/frostyard/pocketgo/desktop/internal/state"
	"github.com/frostyard/pocketgo/proto"
)

// planCard shows a plan that waits for the owner's approval (ADR-0020):
// its text, its tasks and its consults' findings, as the lead wrote them.
// Approve approves exactly the revision and the task graph the card shows
// (state.PlanSeen), after a confirmation.
func (a *App) planCard(c *ui.Context, p proto.Plan) {
	t := c.Theme()
	rev, graph, _ := state.PlanSeen(p)
	// The card, and its button, are the revision and the graph it shows:
	// a plan that changed meanwhile is another card, so a click on the
	// old one never approves the new plan.
	key := "plan:" + itoa(int(p.ID)) + ":" + rev + ":" + itoa(graph)
	name := "P" + itoa(int(p.ID))
	pending := a.st.Approving(p.ID)
	ui.Column(c.Key(key)).Gap(frostui.SpaceS).Padding(frostui.SpaceM, frostui.SpaceL).AlignItems(ui.Stretch).
		Radius(10).Background(frostui.Panel(t)).BorderWidth(1, 1, 1, 1).BorderColor(frostui.ToneColor(t, frostui.Accent)).
		Role(ui.RoleGroup).Label("Plan " + name).Children(func() {
		ui.Row(c).Gap(frostui.SpaceS).AlignItems(ui.Center).Children(func() {
			frostui.Icon(c, "view-list").TextColor(frostui.ToneColor(t, frostui.Accent))
			frostui.Strong(c, name+" · "+p.Title+" waits for your approval").Grow(1).SingleLine()
			frostui.Caption(c, "T"+itoa(int(p.Thread))+" · led by "+a.agentName(p.Lead)).SingleLine()
			if th, ok := a.st.Data.Thread(p.Thread); ok && th.Session != 0 {
				if ui.Button(c.Key(key+":session"), "Open the Lead's Session").Clicked() {
					a.openSession(th.Session)
				}
			}
		})
		frostui.Caption(c, "rev "+state.ShortRev(rev)+" · tasks version "+itoa(graph)+" · "+plural(len(p.Graph.Tasks), "task", "tasks")).SingleLine()
		a.planText(c, key, p.Text)
		a.planTasks(c, key, p, true)
		a.planConsults(c, p)
		ui.Row(c).Gap(frostui.SpaceS).AlignItems(ui.Center).Children(func() {
			ui.Spacer(c)
			if ui.PrimaryButton(c.Key(key+":approve"), "Approve Plan").Disabled(pending.Busy).
				Tooltip("Approve this revision and its tasks: the supervisor then delivers them").Clicked() {
				a.confirmApprove(p)
			}
		})
		if pending.Busy {
			frostui.Loading(c, "Approving "+name+"…")
		}
		if pending.Err != "" {
			frostui.Problem(c, pending.Err).Wrap()
		}
	})
}

// confirmApprove asks before the owner approves plan p, naming what the
// approval holds for: p as the card showed it.
func (a *App) confirmApprove(p proto.Plan) {
	name := "P" + itoa(int(p.ID))
	rev, graph, ok := state.PlanSeen(p)
	if !ok {
		return
	}
	a.dialogs.Ask(frostui.Confirm{
		Title: "Approve " + name + "?",
		Body: "You approve plan " + name + " at rev " + state.ShortRev(rev) + ", with tasks version " + itoa(graph) + " (" + plural(len(p.Graph.Tasks), "task", "tasks") + "). " +
			"pocketd tells the supervisor its ready tasks, and the plan no longer changes. If the plan changed since this card showed it, pocketd refuses.",
		Action: "Approve " + name, OnConfirm: func() { a.st.ApprovePlan(p) },
	})
}

// planText shows a plan's text; a long one shows its first 12 lines until
// the owner unfolds it.
func (a *App) planText(c *ui.Context, key, text string) {
	if text == "" {
		frostui.Muted(c, "The plan has no text yet.")
		return
	}
	open := a.reportOpen[key+":text"]
	folds := strings.Count(text, "\n") >= 12 || len([]rune(text)) > 1200
	ui.Column(c).Gap(frostui.SpaceXS).Padding(frostui.SpaceXS, frostui.SpaceS).Radius(6).Background(frostui.ColorsOf(c.Theme()).Code).
		AlignItems(ui.Stretch).Children(func() {
		shown := ui.Text(c, text).Wrap().Selectable()
		if folds && !open {
			shown.MaxLines(12).Ellipsis("…")
		}
		if folds {
			ui.Row(c).Children(func() {
				label := "Show the Whole Plan"
				if open {
					label = "Show Less"
				}
				if ui.Button(c.Key(key+":text"), label).Clicked() {
					a.reportOpen[key+":text"] = !open
				}
			})
		}
	})
}

// planTasks lists a plan's tasks: who does each, where, after which, and
// its state; with acceptance, the checks its work must pass.
func (a *App) planTasks(c *ui.Context, key string, p proto.Plan, acceptance bool) {
	if p.Graph == nil {
		return
	}
	for _, task := range p.Graph.Tasks {
		ui.Column(c.Key(key + ":task:" + task.ID)).Gap(frostui.SpaceXS).AlignItems(ui.Stretch).Children(func() {
			ui.Row(c).Gap(frostui.SpaceS).AlignItems(ui.Center).Children(func() {
				// The owner reads all of a task before approving it: its
				// title and its assignment wrap (Codex review of #25,
				// round 4).
				frostui.Strong(c, task.ID+" · "+task.Title).Grow(1).Wrap()
				if task.Thread != 0 {
					frostui.Caption(c, "T"+itoa(int(task.Thread))).SingleLine()
				}
				// A draft's tasks have no state yet: they wait for the
				// approval.
				if p.Graph.Approved {
					taskPill(c, task.State)
				}
			})
			where := a.agentName(task.Agent) + " in its home"
			if task.Repository != "" {
				where = a.agentName(task.Agent) + " in " + task.Repository
			}
			if len(task.Deps) > 0 {
				where += " · after " + state.JoinAnd(task.Deps)
			}
			frostui.Caption(c, where).Wrap()
			if acceptance {
				for _, line := range task.Acceptance {
					frostui.Muted(c, "• "+line).Wrap()
				}
			}
		})
	}
}

// planConsults lists a plan's consults: what each was asked, on which of
// the plan's asks, and what it found (Codex review of #25, round 5).
func (a *App) planConsults(c *ui.Context, p proto.Plan) {
	for _, k := range p.Consults {
		asked := "K" + itoa(int(k.ID)) + " · " + a.agentName(k.Agent)
		if k.Ask != 0 {
			asked += ", on A" + itoa(int(k.Ask))
		}
		frostui.Caption(c, asked+": “"+k.Question+"”").Wrap()
		var found string
		switch {
		case k.State == proto.ConsultAnswered:
			found = k.Stance + " · " + k.Summary
		case k.Why != "":
			found = string(k.State) + " · " + k.Why
		default:
			found = string(k.State)
		}
		frostui.Muted(c, found).Wrap()
	}
}

// planSummary shows, on its plan thread's card, where a plan is: its
// state, what its gate waits for, and its tasks.
func (a *App) planSummary(c *ui.Context, key string, th proto.Thread) {
	p, ok := a.st.Data.Plan(th.Plan)
	if !ok {
		frostui.Caption(c, "Plan P"+itoa(int(th.Plan))).SingleLine()
		return
	}
	ui.Row(c).Gap(frostui.SpaceS).AlignItems(ui.Center).Children(func() {
		frostui.Icon(c, "view-list")
		frostui.Strong(c, "Plan P"+itoa(int(p.ID))).SingleLine()
		planPill(c, p.State)
		if p.Rev != "" {
			frostui.Caption(c, "rev "+state.ShortRev(p.Rev)).SingleLine()
		}
	})
	if len(p.Waiting) > 0 {
		frostui.Caption(c, "Waiting: "+strings.Join(p.Waiting, ", ")).Wrap()
	}
	a.planTasks(c, key+":plan", p, false)
}

// taskLine names, on a task thread's card, the plan task it delivers.
func (a *App) taskLine(c *ui.Context, th proto.Thread) {
	text := "Task " + th.PlanTask + " of plan P" + itoa(int(th.Plan))
	if p, ok := a.st.Data.Plan(th.Plan); ok {
		text += " · " + p.Title
	}
	frostui.Caption(c, text).SingleLine()
}

// planPill shows a plan's state in its tone.
func planPill(c *ui.Context, st proto.PlanState) {
	switch st {
	case proto.PlanReview:
		frostui.Pill(c, "waits for you", frostui.Accent)
	case proto.PlanDone:
		frostui.Pill(c, "done", frostui.Success)
	case proto.PlanDropped:
		frostui.Pill(c, "dropped", frostui.Neutral)
	default:
		frostui.Pill(c, string(st), frostui.Accent)
	}
}

// taskPill shows a plan task's state in its tone.
func taskPill(c *ui.Context, st proto.TaskState) {
	switch st {
	case proto.TaskDone:
		frostui.Pill(c, "done", frostui.Success)
	case proto.TaskWaiting:
		frostui.Pill(c, "waiting", frostui.Neutral)
	default:
		frostui.Pill(c, string(st), frostui.Accent)
	}
}
