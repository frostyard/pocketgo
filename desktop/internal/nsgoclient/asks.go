package nsgoclient

import (
	"fmt"
	"slices"
	"strings"

	"github.com/frostyard/pocketgo/desktop/internal/state"
	"github.com/frostyard/pocketgo/desktop/workspaceui"
	"github.com/frostyard/pocketgo/proto"
	"github.com/nsgo-dev/nsgo/core"
	"github.com/nsgo-dev/nsgo/icons/lucide"
	"github.com/nsgo-dev/nsgo/ui"
)

func (c *client) askCard(cx *core.Scope, ask func() proto.Ask) core.Node {
	id := core.Untracked(cx, ask).ID
	name := fmt.Sprintf("A%d", id)
	// The store keeps this exact draft if pocketd withdraws the ask mid-edit.
	saved := c.store.AnswerDraft(id)
	draft := core.NewSignal(cx, *saved)
	pending := core.NewMemo(cx, func() state.Pending { c.revision.Get(); return c.store.Answering(id) })
	busy := func() bool { return pending.Get().Busy }
	answer := func(text string) {
		if strings.TrimSpace(text) == "" || busy() {
			return
		}
		c.store.Answer(id, text)
		c.changed()
	}
	colors := ui.ColorsOf(cx)
	headerItems := []core.Node{ui.Icon(lucide.CircleQuestionMark, 16, colors.Accent), title(name + " asks you"),
		core.Text{ContentFunc: func() string { c.revision.Get(); return c.store.Data.AskNotice(ask()).Subtitle }, Style: ui.TypeCaption.Merge(core.Style{FlexGrow: 1, FlexShrink: 1, MinWidth: core.Pt(100)}), StyleFunc: ui.Colored(colors.Muted)}}
	if c.page.GetUntracked() != "Session" {
		headerItems = append(headerItems, ui.Button{Text: "Open session", Label: "Open session for " + name, Variant: ui.ButtonGhost, Disabled: func() bool { return ask().SessionID == 0 }, OnPress: func() { c.rememberCard(state.AskCard(id)); c.openSession(ask().SessionID) }})
	}
	header := workspaceui.Row(headerItems...)
	options := core.NewMemoFunc(cx, func() []string { return ask().Options }, slices.Equal)
	choices := core.Dynamic(func(*core.Scope) core.Node {
		var buttons []core.Node
		for _, s := range options.Get() {
			buttons = append(buttons, ui.Button{Text: s, Variant: ui.ButtonOutline, Disabled: busy, OnPress: func() { answer(s) }})
		}
		if len(buttons) == 0 {
			return nil
		}
		return workspaceui.Row(buttons...)
	})
	field := ui.Input{Label: "Answer to " + name, PlaceholderFunc: func() string {
		if len(options.Get()) > 0 {
			return "Or another answer"
		}
		return "Your answer"
	}, Value: draft.Get,
		OnChange: func(s string) { *saved = s; draft.Set(s) }, OnSubmit: answer, Disabled: busy, Style: core.Style{FlexGrow: 1, FlexShrink: 1, MinWidth: core.Pt(110)}}
	send := ui.Button{Text: "Answer", Label: "Send answer to " + name, Busy: busy, BusyLabel: "Sending…", Disabled: func() bool { return busy() || strings.TrimSpace(draft.Get()) == "" }, OnPress: func() { answer(draft.Get()) }}
	card := workspaceui.Card{Children: []core.Node{header, core.Text{ContentFunc: func() string { return ask().Question }, Selectable: core.SelectableOn}, choices, workspaceui.Row(field, send),
		core.Show{When: func() bool { return pending.Get().Err != "" }, Then: func(*core.Scope) core.Node {
			return core.Text{ContentFunc: func() string { return pending.Get().Err }, StyleFunc: ui.Colored(colors.Danger), Selectable: core.SelectableOn}
		}},
	}}
	return c.cardAnchor(state.AskCard(id), card)
}
