package nsgoclient

import (
	"fmt"
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
		return core.View{Ref: ref, Children: []core.Node{child}, OnLayout: func(core.Bounds) {
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
		for _, p := range d.PullsToReview() {
			out = append(out, c.cardAnchor(state.PullCard(p.ID), c.pull(p)))
		}
		return core.Group(append(out, c.mergeAll()))
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
func (c *client) plan(p proto.Plan) core.Node {
	out := []core.Node{title(p.Title), ui.TextView{Markdown: p.Text, Selectable: true}}
	if p.Graph != nil {
		for _, t := range p.Graph.Tasks {
			assignment := t.Agent
			if t.Repository != "" {
				assignment += " · " + t.Repository
			}
			if len(t.Deps) != 0 {
				assignment += " · after " + state.JoinAnd(t.Deps)
			}
			out = append(out, title(t.ID+" · "+t.Title), body(strings.Join(nonempty(assignment, string(t.State)), " · ")))
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
