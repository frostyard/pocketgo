package nsgoclient

import (
	"fmt"
	"reflect"

	"github.com/frostyard/frostui/workspaceui"
	"github.com/frostyard/pocketgo/desktop/internal/state"
	"github.com/frostyard/pocketgo/proto"
	"github.com/nsgo-dev/nsgo/core"
	"github.com/nsgo-dev/nsgo/icons/lucide"
	"github.com/nsgo-dev/nsgo/ui"
)

// What a session says beside its transcript: its header's usage and
// state, its question's card, its workers and its changes. The rules are
// state's; this draws them, as the MyGo client's session view does.

// usageRoom is the least width of a window whose header shows what the
// session used: under it the title keeps the room, and the inspector's
// Usage group says it.
const usageRoom core.Points = 720

// muted is small print in the muted color.
func muted(cx *core.Scope, text func() string) core.Text {
	return core.Text{ContentFunc: text, Style: ui.TypeCaption, StyleFunc: ui.Colored(ui.ColorsOf(cx).Muted), Selectable: core.SelectableOn}
}

// contentHeader is the content pane's header: the place's name, and over a
// session its title, what it used (state.UsageLine) and its state.
func (c *client) contentHeader(cx *core.Scope) core.Node {
	d := &c.store.Data
	open := func() (proto.Session, bool) {
		if c.page.Get() != "Session" {
			return proto.Session{}, false
		}
		c.revision.Get()
		s, _ := d.Session(c.session.Get())
		return s, true
	}
	roomy := core.NewMemo(cx, func() bool { return cx.Env().Window().Width >= usageRoom })
	usage := func() string {
		s, ok := open()
		if !ok || !roomy.Get() {
			return ""
		}
		return state.UsageLine(s.Usage)
	}
	return core.View{TestID: "session-header", Style: core.Style{Direction: core.Row, AlignItems: core.AlignCenter, Gap: workspaceui.SpaceS, FlexShrink: 1, MinWidth: core.Pt(0)}, Children: []core.Node{
		core.Text{MaxLines: 1, Style: ui.TypeTitle.Merge(core.Style{FlexShrink: 1, MinWidth: core.Pt(0)}), ContentFunc: func() string {
			if s, ok := open(); ok {
				return sessionName(s)
			}
			return c.page.Get()
		}},
		core.Show{When: func() bool { return usage() != "" }, Then: func(cx *core.Scope) core.Node {
			line := muted(cx, usage)
			line.MaxLines, line.Style = 1, line.Style.Merge(core.Style{FlexShrink: 0})
			return line
		}},
		core.Show{When: func() bool { _, ok := open(); return ok }, Then: func(*core.Scope) core.Node {
			current := func() proto.SessionState { c.revision.Get(); return d.StateOf(c.session.Get()) }
			return ui.Badge{TextFunc: func() string { text, _ := stateMark(current()); return text },
				VariantFunc: func() ui.BadgeVariant { _, variant := stateMark(current()); return variant }}
		}},
	}}
}

// stateMark is a session's state as its badge says it: the word, in the
// accent while it runs and in the warning tone while it waits for the
// owner.
func stateMark(s proto.SessionState) (string, ui.BadgeVariant) {
	switch s {
	case proto.SessionWaiting:
		return "waiting", ui.BadgeWarn
	case proto.SessionRunning:
		return "running", ui.BadgeAccent
	}
	return "idle", ui.BadgeNeutral
}

// questionRow is a session's question that waits for the owner, in its
// transcript (ADR-0048): who waits, the question, that the message box
// below takes any other answer and when the question is skipped if none
// comes (state.QuestionCue), and that an answer is on its way. Its
// options and Skip are in the footer, which answers it.
func (c *client) questionRow(cx *core.Scope, row func() state.Row) core.Node {
	question := func() proto.Question {
		if q := row().Question; q != nil {
			return *q
		}
		return proto.Question{}
	}
	colors := ui.ColorsOf(cx)
	return core.View{Role: core.RoleGroup, Label: "Question for you", Children: []core.Node{workspaceui.Card{Children: []core.Node{
		workspaceui.Row(ui.Icon(lucide.MessageCircleQuestionMark, 16, colors.Accent), core.Text{Style: ui.TypeLabel.Merge(core.Style{FlexShrink: 1}), ContentFunc: func() string {
			c.revision.Get()
			return c.store.Data.SessionAgent(question().SessionID) + " waits for your answer"
		}}),
		core.Text{ContentFunc: func() string { return question().Question }, Style: ui.TypeTitle, Selectable: core.SelectableOn},
		muted(cx, func() string { return state.QuestionCue(question(), cx.WallTime()) }),
		core.Show{When: func() bool { c.revision.Get(); return c.store.AnsweringQuestion(question().ID).Busy }, Then: func(cx *core.Scope) core.Node {
			return muted(cx, func() string { return "Sending your answer…" })
		}},
	}}}}
}

// answerCheck says, at the transcript's end, that an answer whose reply
// was lost is checked before its words count as refused.
func (c *client) answerCheck(id int64) core.Node {
	return core.Show{When: func() bool { c.revision.Get(); return c.store.CheckingAnswer(id) }, Then: func(cx *core.Scope) core.Node {
		return muted(cx, func() string { return "Checking whether pocketd took your answer…" })
	}}
}

// senderName names the supervisor's session that sent a message: who
// speaks in it (state.Data.SessionAgent), else Supervisor.
func (c *client) senderName(id int64) string {
	if s, ok := c.store.Data.Session(id); ok && (s.Profile != "" || s.Title != "") {
		return c.store.Data.SessionAgent(id)
	}
	return "Supervisor"
}

// supervisorMark heads a message a supervisor sent (ADR-0032): its name,
// which opens its session, and a supervisor badge.
func (c *client) supervisorMark(session int64) core.Node {
	name := func() string { c.revision.Get(); return c.senderName(session) }
	return workspaceui.Row(ui.Button{TextFunc: name, LabelFunc: func() string { return "Open the session of " + name() + ", the supervisor" },
		Variant: ui.ButtonGhost, Size: ui.SizeSmall, OnPress: func() { c.openSession(session) }}, ui.Badge{Text: "supervisor", Variant: ui.BadgeAccent})
}

// noteRow is a quiet line of the transcript, as a stopped run: its words
// between two lines, apart from what the agent and the owner said.
func noteRow(cx *core.Scope, text func() string) core.Node {
	colors := ui.ColorsOf(cx)
	line := core.View{Style: core.Style{FlexGrow: 1, FlexBasis: core.Pt(0), Borders: core.Edges{Top: 1}}, StyleFunc: func() core.Style { return core.Style{BorderColor: colors.Border()} }}
	words := muted(cx, text)
	words.Style = words.Style.Merge(core.Style{FlexShrink: 1})
	return core.View{TestID: "transcript-note", Style: core.Style{Direction: core.Row, AlignItems: core.AlignCenter, Gap: workspaceui.SpaceS}, Children: []core.Node{line, words, line}}
}

// workersGroup is the inspector's Workers group. A worker's session
// shows the way back to its specialist, and its own title and model; a
// specialist's shows a card for each of its workers, with its model and
// its state, and folds those past the third behind "+N more"
// (state.Data.WorkerCards).
func (c *client) workersGroup(cx *core.Scope, id int64) core.Node {
	d := &c.store.Data
	expanded := core.NewSignal(cx, false)
	type workers struct {
		header state.WorkerHeader
		worker bool
		cards  state.WorkerCards
	}
	shown := core.NewMemoFunc(cx, func() workers {
		c.revision.Get()
		header, worker := d.WorkerHeaderOf(id)
		return workers{header, worker, d.WorkerCards(id, expanded.Get())}
	}, func(a, b workers) bool { return reflect.DeepEqual(a, b) })
	fits := core.Style{FlexShrink: 1, MinWidth: core.Pt(0)}
	return core.Dynamic(func(cx *core.Scope) core.Node {
		v := shown.Get()
		var nodes []core.Node
		if v.worker {
			if h := v.header; h.Parent != 0 {
				nodes = append(nodes, workspaceui.Row(ui.Button{Text: "Back to specialist", Icon: lucide.ArrowLeft, Label: "Back to the specialist's session, " + h.ParentTitle,
					Variant: ui.ButtonSecondary, Size: ui.SizeSmall, OnPress: func() { c.openSession(h.Parent) }}, muted(cx, func() string { return h.ParentTitle })))
			}
			self := []core.Node{muted(cx, func() string { return "Worker:" }), core.Text{Content: v.header.Title, Style: ui.TypeLabel.Merge(fits), Selectable: core.SelectableOn}}
			if name := state.ModelName(v.header.Model); name != "" {
				self = append(self, ui.Tooltip{Text: v.header.Model.String(), Trigger: muted(cx, func() string { return name })})
			}
			nodes = append(nodes, core.View{TestID: "worker-header", Children: []core.Node{workspaceui.Row(self...)}})
		}
		for _, card := range v.cards.Cards {
			tooltip := card.Title
			name := state.ModelName(card.Model)
			if name != "" {
				tooltip += " · " + card.Model.String()
			}
			word, variant := stateMark(card.State)
			row := []core.Node{ui.Tooltip{Text: tooltip, Style: fits, Trigger: ui.Button{Text: card.Title, Label: state.WorkerCardLabel(card), Variant: ui.ButtonSecondary, Size: ui.SizeSmall,
				OnPress: func() { c.openSession(card.ID) }}}}
			if name != "" {
				row = append(row, muted(cx, func() string { return name }))
			}
			nodes = append(nodes, workspaceui.Row(append(row, ui.Badge{Text: word, Variant: variant})...))
		}
		if n := v.cards.More; n > 0 {
			nodes = append(nodes, workspaceui.Row(ui.Button{Text: state.MoreWorkersLabel(n), Label: fmt.Sprintf("Show %d more workers", n), Variant: ui.ButtonGhost, Size: ui.SizeSmall,
				OnPress: func() { expanded.Set(true) }}))
		}
		if v.cards.Fewer {
			nodes = append(nodes, workspaceui.Row(ui.Button{Text: "Show fewer", Label: "Show fewer workers", Variant: ui.ButtonGhost, Size: ui.SizeSmall,
				OnPress: func() { expanded.Set(false) }}))
		}
		if len(nodes) == 0 {
			return nil
		}
		return workspaceui.InspectorGroup("Workers", nodes...)
	})
}

// changesGroup is the inspector's Changes group (ADR-0053): what the
// session changed in its git checkout, with Refresh, where the changes
// are counted from, each file with where its change is and its lines
// added and removed in their tones, and the diff of the file chosen. A
// session whose folder is in no checkout shows none.
func (c *client) changesGroup(cx *core.Scope, id int64) core.Node {
	d := &c.store.Data
	type changes struct {
		files  state.SessionChanges
		known  bool
		diff   state.ShownDiff
		diffed bool
	}
	shown := core.NewMemoFunc(cx, func() changes {
		c.revision.Get()
		files, known := d.Changes(id)
		diff, diffed := d.Diff()
		if diff.Session != id {
			diff, diffed = state.ShownDiff{}, false
		}
		return changes{files, known, diff, diffed}
	}, func(a, b changes) bool { return reflect.DeepEqual(a, b) })
	colors := ui.ColorsOf(cx)
	return core.Dynamic(func(cx *core.Scope) core.Node {
		v := shown.Get()
		sc, ch := v.files, v.files.Changes
		summary := state.ChangesSummary(sc, v.known)
		if summary == "" {
			if v.known && sc.Status == state.ListLoaded {
				return nil
			}
			summary = "Reading changes…"
		}
		out := []core.Node{workspaceui.Row(core.Text{Content: summary, Style: ui.TypeLabel.Merge(core.Style{FlexGrow: 1, FlexShrink: 1, MinWidth: core.Pt(0)}), Selectable: core.SelectableOn},
			ui.Button{Text: "Refresh", Label: "Refresh the changes", Icon: lucide.RefreshCw, Variant: ui.ButtonSecondary, Size: ui.SizeSmall,
				Disabled: func() bool { c.revision.Get(); return c.store.ChangesLoading(id) }, OnPress: func() { c.store.RefreshChanges(id); c.changed() }})}
		// A failed read says why first; what an earlier one listed shows
		// under it, if any did (state.ChangesSummary).
		if sc.Status == state.ListFailed {
			out = append(out, core.Text{Content: "Reading the changes failed: " + sc.Err, StyleFunc: ui.Colored(colors.Danger), Selectable: core.SelectableOn})
		}
		if ch.Repository {
			out = append(out, muted(cx, func() string { return state.BaseNote(ch) }))
			// No change is said of a read that succeeded alone: after a
			// failure, what the folder holds is not known.
			if len(ch.Files) == 0 && sc.Status == state.ListLoaded {
				out = append(out, muted(cx, func() string { return "Nothing changed since then." }))
			}
			for _, f := range ch.Files {
				out = append(out, c.changedFile(cx, id, f))
			}
			if more := state.MoreFiles(ch); more != "" {
				out = append(out, muted(cx, func() string { return more }))
			}
		}
		if v.diffed {
			out = append(out, c.shownDiff(cx, v.diff)...)
		}
		return workspaceui.InspectorGroup("Changes", out...)
	})
}

// changedFile is a changed file: its path, which shows its diff, then
// where its change is and its lines.
func (c *client) changedFile(cx *core.Scope, id int64, f proto.ChangedFile) core.Node {
	name := state.FileName(f)
	var path core.Node = core.Text{Content: name, Style: ui.TypeCode, Selectable: core.SelectableOn}
	// A repository inside the checkout has no diff to show (state.CanDiff).
	if state.CanDiff(f) {
		// A link's text wraps, so a long path stays inside a narrow panel.
		path = ui.Link{Text: name, Label: name + ", " + state.FileState(f) + ", " + state.FileCounts(f),
			Style: core.Style{FontFamily: ui.Mono, MaxWidth: core.Pct(100)}, OnPress: func() { c.store.ShowDiff(id, f.Path); c.changed() }}
	}
	where := muted(cx, func() string { return state.FileState(f) })
	where.Style = where.Style.Merge(core.Style{FlexGrow: 1, FlexShrink: 1, MinWidth: core.Pt(0)})
	return core.View{TestID: "changed-file", Style: core.Style{Gap: workspaceui.SpaceXS}, Children: []core.Node{path,
		core.View{Style: core.Style{Direction: core.Row, AlignItems: core.AlignCenter, Gap: workspaceui.SpaceS}, Children: []core.Node{where, fileLines(cx, f)}}}}
}

// fileLines are a file's lines, added in the success tone and removed in
// the danger tone, or why there are none.
func fileLines(cx *core.Scope, f proto.ChangedFile) core.Node {
	if f.Binary || f.Uncounted {
		return muted(cx, func() string { return state.FileCounts(f) })
	}
	colors := ui.ColorsOf(cx)
	return core.View{Style: core.Style{Direction: core.Row, Gap: workspaceui.SpaceXS, FlexShrink: 0}, Children: []core.Node{
		core.Text{Content: fmt.Sprintf("+%d", f.Added), Style: ui.TypeCode, StyleFunc: ui.Colored(colors.Up)},
		core.Text{Content: fmt.Sprintf("−%d", f.Removed), Style: ui.TypeCode, StyleFunc: ui.Colored(colors.Danger)},
	}}
}

// shownDiff is the diff of the file chosen: its path, that pocketd cut
// it, why reading it failed, and its lines.
func (c *client) shownDiff(cx *core.Scope, diff state.ShownDiff) []core.Node {
	colors := ui.ColorsOf(cx)
	out := []core.Node{core.Text{Content: diff.Path, Style: ui.TypeCode.Merge(core.Style{FontWeight: ui.Semibold}), Selectable: core.SelectableOn}}
	if cut := state.CutNote(diff.Diff); cut != "" {
		out = append(out, core.Text{Content: cut, Style: ui.TypeCaption, StyleFunc: ui.Colored(colors.Warn), Selectable: core.SelectableOn})
	}
	if diff.Err != "" {
		out = append(out, core.Text{Content: "Reading the diff failed: " + diff.Err, StyleFunc: ui.Colored(colors.Danger), Selectable: core.SelectableOn})
	}
	switch none := state.NoDiff(diff.Diff); {
	case diff.Status == state.ListLoading:
		out = append(out, muted(cx, func() string { return "Reading the diff…" }))
	case diff.Status == state.ListFailed && diff.Diff.Diff == "":
	case none != "":
		out = append(out, muted(cx, func() string { return none }))
	default:
		out = append(out, core.Text{Content: diff.Diff.Diff, Style: ui.TypeCode, Selectable: core.SelectableOn})
	}
	return append(out, workspaceui.Row(c.button("Close diff", c.store.Data.CloseDiff)))
}
