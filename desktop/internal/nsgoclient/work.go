package nsgoclient

import (
	"fmt"
	"reflect"
	"strings"

	"github.com/frostyard/frostui/workspaceui"
	"github.com/frostyard/pocketgo/desktop/internal/state"
	"github.com/frostyard/pocketgo/proto"
	"github.com/nsgo-dev/nsgo/core"
	"github.com/nsgo-dev/nsgo/icons/lucide"
	"github.com/nsgo-dev/nsgo/ui"
)

// listProblem says a list failed to load and what may be missing for it,
// with a way to load it again: a view never shows a list it could not
// load as empty (the kit's standard for an app, rule 6).
func (c *client) listProblem(what, problem, missing string, retry func()) core.Node {
	text := "Loading " + what + " failed: " + problem
	if missing != "" {
		text += ". " + missing
	}
	return c.problemBanner(workspaceui.Danger, text, retry)
}

// problemBanner says what went wrong, with Try Again when there is a way
// to.
func (c *client) problemBanner(tone workspaceui.Tone, text string, retry func()) core.Node {
	b := workspaceui.Banner{Tone: tone, Icon: lucide.CircleAlert, Text: text}
	if tone == workspaceui.Warning {
		b.Icon = lucide.TriangleAlert
	}
	if retry != nil {
		b.Action, b.OnAction = "Try Again", func() { retry(); c.changed() }
	}
	return b
}

func title(s string) core.Node { return core.Text{Content: s, Style: ui.TypeTitle} }
func body(s string) core.Node  { return core.Text{Content: s, Selectable: core.SelectableOn} }

// caption is a small quiet line about what stands over it.
func (c *client) caption(s string) core.Node {
	return core.Text{Content: s, Style: ui.TypeCaption, StyleFunc: ui.Colored(ui.ColorsOf(c.cx).Muted), Selectable: core.SelectableOn}
}

// captionFunc is a caption that follows what say reads.
func (c *client) captionFunc(say func() string) core.Node {
	return core.Text{ContentFunc: say, Style: ui.TypeCaption, StyleFunc: ui.Colored(ui.ColorsOf(c.cx).Muted), Selectable: core.SelectableOn}
}

// note is a quiet sentence: what a section says with nothing in it.
func (c *client) note(s string) core.Node {
	return core.Text{Content: s, StyleFunc: ui.Colored(ui.ColorsOf(c.cx).Muted), Selectable: core.SelectableOn}
}

// A target can arrive before its card loads, or while it is already mounted.
func (c *client) cardAnchor(id string, child core.Node) core.Node {
	return core.Func(func(cx *core.Scope) core.Node {
		ref := &core.Ref{}
		reveal := func() {
			if c.noticeCard.GetUntracked() != id {
				return
			}
			if _, mounted := ref.Bounds(); !mounted {
				return
			}
			ref.Reveal(core.RevealOptions{Vertical: core.ScrollAlignStart})
			c.noticeCard.Set("")
		}
		cx.Effect(func() {
			if c.noticeCard.Get() == id {
				cx.Post(reveal)
			}
		})
		return core.View{Ref: ref, TestID: id, Children: []core.Node{child}, OnLayout: func(core.Bounds) {
			if c.noticeCard.GetUntracked() == id {
				cx.Post(reveal)
			}
		}}
	})
}

func (c *client) approvalActions(id string, extra ...core.Node) core.Node {
	reason := c.reasons[id]
	actions := []core.Node{
		ui.Button{Text: "Allow", Variant: ui.ButtonTinted, OnPress: func() { c.store.Decide(id, true, reason); c.changed() }},
		c.button("Deny", func() { c.store.Decide(id, false, reason) }),
	}
	return workspaceui.Stack(ui.Input{Label: "Decision reason", Placeholder: "Optional reason", Value: func() string { return reason }, OnChange: func(s string) { reason = s; c.reasons[id] = s }},
		workspaceui.Row(append(actions, extra...)...), core.Show{When: func() bool { c.revision.Get(); return c.store.Deciding(id).Err != "" }, Then: func(cx *core.Scope) core.Node {
			return core.Text{ContentFunc: func() string { c.revision.Get(); return c.store.Deciding(id).Err }, Selectable: core.SelectableOn, StyleFunc: ui.Colored(ui.ColorsOf(cx).Danger)}
		}})
}
func (c *client) inbox(cx *core.Scope) core.Node {
	d := &c.store.Data
	asks := core.Each(func() []proto.Ask { c.revision.Get(); return d.Asks() }, func(a proto.Ask) int64 { return a.ID },
		func(cx *core.Scope, ask func() proto.Ask, _ func() int) core.Node { return c.askCard(cx, ask) })
	approvals := core.Each(func() []proto.Approval { c.revision.Get(); return d.Approvals() }, func(a proto.Approval) string { return a.ID }, c.approvalCard)
	other := core.Dynamic(func(*core.Scope) core.Node {
		c.revision.Get()
		var out []core.Node
		loading := false
		// Each list that failed says what may wait unseen, in the Inbox's
		// order.
		for _, list := range []struct {
			status        func() (state.ListStatus, string)
			what, missing string
		}{
			{d.PlansStatus, "the plans", "Plans may wait for your approval that this list does not show."},
			{d.AsksStatus, "the asks", "Asks may wait for you that this list does not show."},
			{d.LessonsStatus, "the lessons", "Lessons may wait for you that this list does not show."},
			{d.PullsStatus, "the pull requests", "Pull requests may wait for your review that this list does not show."},
			{d.ApprovalsStatus, "the approvals", "Calls may wait for you that this list does not show."},
			{d.SessionsStatus, "the sessions", "A session may wait for your answer to its question that this list does not show."},
		} {
			status, problem := list.status()
			loading = loading || status == state.ListLoading
			if status == state.ListFailed {
				out = append(out, c.listProblem(list.what, problem, list.missing, c.store.Refresh))
			}
		}
		for _, r := range c.store.AnswerRefusals() {
			out = append(out, workspaceui.Card{Children: []core.Node{title(fmt.Sprintf("Answer to A%d refused", r.Ask)), body(r.Why), body(r.Answer), workspaceui.Row(c.button("Dismiss refused answer", func() { c.store.DismissAnswerRefusal(r.Ask) }))}})
		}
		for _, w := range c.store.WithdrawnDrafts() {
			out = append(out, workspaceui.Card{Children: []core.Node{title(fmt.Sprintf("A%d was withdrawn", w.Ask.ID)), body(w.Ask.Question), body(d.WithdrawalNote(w.Ask)), body(w.Draft), workspaceui.Row(c.button("Dismiss withdrawn draft", func() { c.store.DismissWithdrawnDraft(w.Ask.ID) }))}})
		}
		for _, q := range d.QuestionsWaiting() {
			out = append(out, c.questionCard(cx, q))
		}
		if len(out) == 0 && state.InboxCount(d.Inbox()) == 0 {
			if loading {
				return body("Loading inbox…")
			}
			out = append(out, title("You’re caught up"), body("Questions, approvals and reviews appear here when they need you."))
		}
		return core.Group(out)
	})
	reviews := core.Dynamic(func(*core.Scope) core.Node {
		c.revision.Get()
		var out []core.Node
		for _, p := range d.PlansToApprove() {
			out = append(out, c.inboxPlanCard(cx, p))
		}
		for _, l := range d.Lessons() {
			out = append(out, c.lessonCard(cx, l))
		}
		// Merge All Ready heads the pull requests it acts on.
		if all := c.mergeAll(); all != nil {
			out = append(out, all)
		}
		for _, p := range d.PullsToReview() {
			out = append(out, c.cardAnchor(state.PullCard(p.ID), c.pull(p)))
		}
		return core.Group(out)
	})
	note := core.Text{ContentFunc: func() string { c.revision.Get(); return state.InboxNote(d.Inbox()) }, Style: ui.TypeLabel, StyleFunc: ui.Colored(ui.ColorsOf(cx).Muted)}
	return workspaceui.Page(note, asks, other, approvals, reviews, c.lessonsInForce())
}

// lessonsInForce lists each agent's lessons that hold now, after what
// waits and folded: they wait for nothing. Forget removes a lesson from
// its agent's sessions at once (ADR-0015).
func (c *client) lessonsInForce() core.Node {
	d := &c.store.Data
	count := func() int { c.revision.Get(); return len(d.LessonsInForce()) }
	return core.Show{When: func() bool { return count() > 0 }, Then: func(*core.Scope) core.Node {
		return workspaceui.Section{Title: "Lessons in force", Count: count, Content: func(*core.Scope) core.Node {
			return core.Dynamic(func(cx *core.Scope) core.Node {
				c.revision.Get()
				colors := ui.ColorsOf(cx)
				var rows []core.Node
				for _, lesson := range d.LessonsInForce() {
					pending := c.store.DecidingLesson(lesson.ID)
					forget := ui.Button{Text: "Forget", Label: fmt.Sprintf("Forget L%d", lesson.ID), Variant: ui.ButtonSecondary, Size: ui.SizeSmall,
						Disabled: func() bool { return pending.Busy }, OnPress: func() { c.store.DecideLesson(lesson, proto.LessonForget); c.changed() }}
					row := []core.Node{core.View{TestID: fmt.Sprintf("lesson-in-force-%d", lesson.ID), Style: core.Style{Direction: core.Row, AlignItems: core.AlignCenter, Gap: workspaceui.SpaceS}, Children: []core.Node{
						core.Text{Content: fmt.Sprintf("L%d · %s", lesson.ID, d.AgentName(lesson.Agent)), MaxLines: 1, Style: ui.TypeCaption.Merge(core.Style{FlexShrink: 0}), StyleFunc: ui.Colored(colors.Muted)},
						core.Text{Content: lesson.Text, Selectable: core.SelectableOn, Style: core.Style{FlexGrow: 1, FlexShrink: 1, MinWidth: core.Pt(0)}},
						ui.Tooltip{Text: "Remove this lesson from its agent's sessions now", Trigger: forget}}}}
					if pending.Err != "" {
						row = append(row, c.problemBanner(workspaceui.Danger, pending.Err, nil))
					}
					rows = append(rows, row...)
				}
				return workspaceui.Stack(rows...)
			})
		}}
	}}
}

// planHead says where a plan is: its number, its state, its revision, and
// what its gate waits for. It stands on the plan's own thread's card.
func (c *client) planHead(p proto.Plan) core.Node {
	row := []core.Node{ui.Icon(lucide.ListTree, 16, ui.ColorsOf(c.cx).Muted), core.Text{Content: fmt.Sprintf("Plan P%d", p.ID), Style: ui.TypeLabel}, planBadge(p.State)}
	if p.Rev != "" {
		row = append(row, c.caption("rev "+state.ShortRev(p.Rev)))
	}
	out := []core.Node{workspaceui.Row(row...)}
	if len(p.Waiting) > 0 {
		out = append(out, c.caption("Waiting: "+strings.Join(p.Waiting, ", ")))
	}
	return workspaceui.Stack(out...)
}

// planBadge shows a plan's state in its tone.
func planBadge(st proto.PlanState) core.Node {
	switch st {
	case proto.PlanReview:
		return ui.Badge{Text: "waits for you", Variant: ui.BadgeAccent}
	case proto.PlanDone:
		return ui.Badge{Text: "done", Variant: ui.BadgeSuccess}
	case proto.PlanDropped:
		return ui.Badge{Text: "dropped"}
	}
	return ui.Badge{Text: string(st), Variant: ui.BadgeAccent}
}

// taskBadge shows a plan task's state in its tone.
func taskBadge(st proto.TaskState) core.Node {
	switch st {
	case proto.TaskDone:
		return ui.Badge{Text: "done", Variant: ui.BadgeSuccess}
	case proto.TaskWaiting:
		return ui.Badge{Text: "waiting"}
	}
	return ui.Badge{Text: string(st), Variant: ui.BadgeAccent}
}

// taskWhere says who does a plan's task, where, and after which. It names
// the profile the approval binds by its id too when its name differs: two
// profiles may share a name.
func (c *client) taskWhere(t proto.PlanTask) string {
	who := c.store.Data.AgentName(t.Agent)
	if who != t.Agent {
		who += " (" + t.Agent + ")"
	}
	where := who + " in its home"
	if t.Repository != "" {
		where = who + " in " + t.Repository
	}
	if len(t.Deps) != 0 {
		where += " · after " + state.JoinAnd(t.Deps)
	}
	return where
}

// planOnThread shows a plan on its own thread's card, where the MyGo
// client shows it: where the plan is (planHead), then each task with who
// does it, where and after which, its thread once it is delivered and its
// state. The plan's text and each task's checks, which only this client
// shows, stay folded behind Show the Plan. A plan the client does not hold
// is named.
func (c *client) planOnThread(cx *core.Scope, t proto.Thread, open *core.Signal[bool]) core.Node {
	d := &c.store.Data
	plan := core.NewMemoFunc(cx, func() *proto.Plan {
		c.revision.Get()
		if p, ok := d.Plan(t.Plan); ok {
			return &p
		}
		return nil
	}, func(a, b *proto.Plan) bool { return reflect.DeepEqual(a, b) })
	return core.Dynamic(func(*core.Scope) core.Node {
		p := plan.Get()
		if p == nil {
			return c.caption(fmt.Sprintf("Plan P%d", t.Plan))
		}
		return c.planSummary(*p, open)
	})
}

// planSummary is a plan as a card of the board shows it: its head, its
// tasks, and its text and checks behind Show the Plan.
func (c *client) planSummary(p proto.Plan, open *core.Signal[bool]) core.Node {
	out := []core.Node{c.planHead(p)}
	if p.Graph != nil {
		for _, t := range p.Graph.Tasks {
			row := []core.Node{core.Text{Content: t.ID + " · " + t.Title, Selectable: core.SelectableOn, Style: ui.TypeLabel.Merge(core.Style{FlexGrow: 1, FlexShrink: 1, MinWidth: core.Pt(0)})}}
			if t.Thread != 0 {
				row = append(row, c.caption(fmt.Sprintf("T%d", t.Thread)))
			}
			// A draft's tasks have no state yet: they wait for the
			// approval.
			if p.Graph.Approved {
				row = append(row, taskBadge(t.State))
			}
			out = append(out, core.View{TestID: fmt.Sprintf("plan-%d-task-%s", p.ID, t.ID), Style: core.Style{Gap: workspaceui.SpaceXS}, Children: []core.Node{
				core.View{Style: core.Style{Direction: core.Row, AlignItems: core.AlignCenter, Gap: workspaceui.SpaceS}, Children: row},
				c.caption(c.taskWhere(t))}})
		}
	}
	out = append(out, ui.Collapsible{Text: "Show the Plan", Variant: ui.CollapsibleArrowLeft, Open: open.Get, OnOpenChange: open.Set,
		Content: core.Show{When: open.Get, Then: func(*core.Scope) core.Node {
			var text []core.Node
			if p.Text == "" {
				text = append(text, c.note("The plan has no text yet."))
			} else {
				text = append(text, ui.TextView{Markdown: p.Text, Selectable: true})
			}
			if p.Graph != nil {
				for _, t := range p.Graph.Tasks {
					if len(t.Acceptance) == 0 {
						continue
					}
					text = append(text, core.Text{Content: t.ID + " · " + t.Title, Style: ui.TypeLabel})
					for _, check := range t.Acceptance {
						text = append(text, body("• "+check))
					}
				}
			}
			return workspaceui.Stack(text...)
		}}})
	return workspaceui.Stack(out...)
}

// plansOffTheBoard shows each plan that waits for the owner and has no
// open thread's card to show on (state.PlansOffTheBoard): its thread is
// not among the threads the client holds, or is closed. Each keeps a card
// that says what waits and where its thread is, with the way to the
// Inbox, so no plan waits unseen.
func (c *client) plansOffTheBoard(cx *core.Scope) core.Node {
	d := &c.store.Data
	plans := core.NewMemoFunc(cx, func() []proto.Plan { c.revision.Get(); return d.PlansOffTheBoard() },
		func(a, b []proto.Plan) bool { return reflect.DeepEqual(a, b) })
	return core.Show{When: func() bool { return len(plans.Get()) > 0 }, Then: func(*core.Scope) core.Node {
		return workspaceui.Section{Title: "Plans with no open thread", Count: func() int { return len(plans.Get()) }, InitiallyOpen: true, Content: func(*core.Scope) core.Node {
			return workspaceui.Stack(core.Each(plans.Get, func(p proto.Plan) int64 { return p.ID },
				func(cx *core.Scope, item func() proto.Plan, _ func() int) core.Node {
					open := core.NewSignal(cx, false)
					return core.Dynamic(func(cx *core.Scope) core.Node {
						p := item()
						waits := fmt.Sprintf("P%d waits for your approval.", p.ID)
						target := state.PlanCard(p.ID)
						if !state.ToApprove(p) {
							waits, target = fmt.Sprintf("P%d waits for your answer: %s.", p.ID, state.JoinAnd(state.OpenAsksOf(p))), ""
						}
						where := fmt.Sprintf("Its thread, T%d, is not among the threads loaded.", p.Thread)
						if _, ok := d.Thread(p.Thread); ok {
							where = fmt.Sprintf("Its thread, T%d, is closed.", p.Thread)
						}
						return core.View{TestID: fmt.Sprintf("plan-%d", p.ID), Children: []core.Node{boardCard(cx, func() bool { return true }, []core.Node{
							title(p.Title),
							core.Text{Content: waits + " " + where, Selectable: core.SelectableOn, StyleFunc: ui.Colored(ui.ColorsOf(cx).Muted)},
							c.planSummary(p, open),
							workspaceui.Row(ui.Button{Text: "Open the Inbox", OnPress: func() { c.goTo(destination{page: "Inbox", card: target}) }}),
						})}}
					})
				}))
		}}
	}}
}

// plan shows a plan's text and its tasks: who does each, where, after
// which, its thread once it is delivered and its state, then the checks
// its work must pass.
func (c *client) plan(p proto.Plan) core.Node {
	out := []core.Node{title(p.Title), ui.TextView{Markdown: p.Text, Selectable: true}}
	if p.Graph != nil {
		for _, t := range p.Graph.Tasks {
			head := []core.Node{core.Text{Content: t.ID + " · " + t.Title, Style: ui.TypeTitle.Merge(core.Style{FlexGrow: 1, FlexShrink: 1})}}
			if t.Thread != 0 {
				head = append(head, c.caption(fmt.Sprintf("T%d", t.Thread)))
			}
			// A draft's tasks have no state yet: they wait for the
			// approval.
			if p.Graph.Approved {
				head = append(head, taskBadge(t.State))
			}
			out = append(out, workspaceui.Row(head...), body(c.taskWhere(t)))
			for _, check := range t.Acceptance {
				out = append(out, body("• "+check))
			}
		}
	}
	if state.ToApprove(p) {
		out = append(out, workspaceui.Row(c.button("Approve plan", func() {
			c.confirm("Approve "+p.Title+"?", fmt.Sprintf("Approve revision %s, tasks version %d (%d tasks), and start its ready tasks.", state.ShortRev(p.Rev), p.Graph.Version, len(p.Graph.Tasks)), "Approve", func() { c.store.ApprovePlan(p) })
		})))
		if why := c.store.Approving(p.ID).Err; why != "" {
			out = append(out, body(why))
		}
	}
	return workspaceui.Stack(out...)
}
