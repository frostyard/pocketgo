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

// Every review starts with its kind and source. Keep text labels alongside
// the themed glyphs so the categories also read clearly without color.
func inboxHeader(cx *core.Scope, kind string, icon func() *core.SVG, tint func() core.Color, source func() string) core.Node {
	colors := ui.ColorsOf(cx)
	mark := core.View{Style: core.Style{Width: core.Pt(26), Height: core.Pt(26), CornerRadius: 7, AlignItems: core.AlignCenter, JustifyContent: core.JustifyCenter},
		StyleFunc: func() core.Style { return core.Style{Background: ui.Mix(colors.Raised(), tint(), 0.14)} },
		Children:  []core.Node{ui.Icon(icon, 16, tint)}}
	return workspaceui.Row(mark, core.Text{Content: kind, Style: ui.TypeLabel}, core.Text{ContentFunc: source, Style: ui.TypeCaption.Merge(core.Style{FlexShrink: 1}), StyleFunc: ui.Colored(colors.Muted)})
}

func (c *client) openInboxConversation(key string, session int64) ui.Button {
	return ui.Button{Text: "Open conversation", Icon: lucide.MessageSquare, Size: ui.SizeSmall, Variant: ui.ButtonOutline, OnPress: func() { c.rememberCard(key); c.openSession(session) }}
}

func (c *client) questionCard(cx *core.Scope, q proto.Question) core.Node {
	key := "question:" + q.ID
	return c.cardAnchor(key, workspaceui.Card{Children: []core.Node{
		inboxHeader(cx, "Session question", lucide.MessageCircleQuestionMark, ui.ColorsOf(cx).Accent, func() string { return c.store.Data.QuestionNotice(q).Subtitle }),
		core.Text{Content: q.Question, Selectable: core.SelectableOn, Style: ui.TypeTitle},
		workspaceui.Row(c.openInboxConversation(key, q.SessionID)),
	}})
}

func (c *client) approvalCard(cx *core.Scope, approval func() proto.Approval, _ func() int) core.Node {
	a := core.Untracked(cx, approval)
	key := state.ApprovalCard(a.ID)
	args := core.NewMemo(cx, func() string { return state.IndentArguments(approval().Arguments) })
	colors := ui.ColorsOf(cx)
	return c.cardAnchor(key, workspaceui.Card{Children: []core.Node{
		inboxHeader(cx, "Tool approval", lucide.ShieldCheck, colors.Warn, func() string { c.revision.Get(); return c.store.Data.ApprovalNotice(approval()).Subtitle }),
		core.Text{ContentFunc: func() string {
			a := approval()
			return strings.Join(nonempty(a.Tool, state.ToolSummary(a.Tool, a.Arguments)), " · ")
		}, Style: ui.TypeTitle, Selectable: core.SelectableOn},
		core.View{Style: core.Style{Padding: core.All(10), CornerRadius: 6}, StyleFunc: ui.Filled(colors.Inset), Children: []core.Node{
			core.Scroll{Style: core.Style{MaxHeight: core.Pt(160)}, Children: []core.Node{core.Text{ContentFunc: args.Get, Style: ui.TypeCode, Selectable: core.SelectableOn}}},
		}},
		c.approvalActions(a.ID, ui.IconButton{Icon: lucide.Copy, Label: "Copy arguments", OnPress: func() { c.cx.SetClipboard(args.Get()) }}, c.openInboxConversation(key, a.SessionID)),
	}})
}

func (c *client) inboxPlanCard(cx *core.Scope, p proto.Plan) core.Node {
	return c.cardAnchor(state.PlanCard(p.ID), workspaceui.Card{Children: []core.Node{
		inboxHeader(cx, "Plan review", lucide.ListTree, ui.ColorsOf(cx).Info, func() string { return fmt.Sprintf("P%d · %s", p.ID, c.store.Data.PlanNotice(p).Subtitle) }),
		c.plan(p),
	}})
}

func (c *client) lessonCard(cx *core.Scope, l proto.Lesson) core.Node {
	seen, forget := state.SeenText(l)
	kind := "New lesson"
	if forget {
		kind = "Forget lesson"
		seen = l.Text
	} else if l.Change != nil {
		kind = "Revise lesson"
	}
	decision := "Approve lesson"
	if forget {
		decision = "Approve removal"
	} else if l.Change != nil {
		decision = "Approve revision"
	}
	out := []core.Node{inboxHeader(cx, kind, lucide.BookOpen, ui.ColorsOf(cx).Up, func() string { return fmt.Sprintf("L%d · %s", l.ID, c.store.Data.AgentName(l.Agent)) })}
	if l.Change != nil && !forget {
		out = append(out, core.Text{Content: "Current lesson", Style: ui.TypeCaption}, body(l.Text), core.Text{Content: "Proposed revision", Style: ui.TypeCaption})
	}
	out = append(out, body(seen), workspaceui.Row(ui.Button{Text: decision, Variant: ui.ButtonTinted, OnPress: func() { c.store.DecideLesson(l, proto.LessonApprove); c.changed() }}, c.button("Discard proposal", func() { c.store.DecideLesson(l, proto.LessonDiscard) })))
	if why := c.store.DecidingLesson(l.ID).Err; why != "" {
		out = append(out, body(why))
	}
	return c.cardAnchor(state.LessonCard(l.ID), workspaceui.Card{Children: out})
}
