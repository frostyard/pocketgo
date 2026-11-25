package nsgoclient

import (
	"fmt"
	"path"
	"reflect"
	"slices"
	"strings"

	"github.com/frostyard/frostui/workspaceui"
	"github.com/frostyard/pocketgo/desktop/internal/state"
	"github.com/frostyard/pocketgo/proto"
	"github.com/nsgo-dev/nsgo/core"
	"github.com/nsgo-dev/nsgo/icons/lucide"
	"github.com/nsgo-dev/nsgo/ui"
)

// board is the work board: the threads grouped by who moves them next
// (state.Data.Board), as the MyGo client groups them, each group a section
// with its count and the closed one folded, then the schedules and the
// night shift.
func (c *client) board(cx *core.Scope) core.Node {
	d := &c.store.Data
	groups := core.NewMemoFunc(cx, func() []state.BoardGroup { c.revision.Get(); return d.Board() }, nil)
	section := func(name string, expanded bool, count func() int, visible func() bool, content func(*core.Scope) core.Node) core.Node {
		if visible == nil {
			visible = func() bool { return count() > 0 }
		}
		return core.Show{When: visible, Then: func(*core.Scope) core.Node {
			return workspaceui.Section{Title: name, Count: count, InitiallyOpen: expanded, Content: content}
		}}
	}
	nodes := []core.Node{core.Text{ContentFunc: func() string { c.revision.Get(); return d.BoardNote() }, Style: ui.TypeLabel, StyleFunc: ui.Colored(ui.ColorsOf(cx).Muted)}}
	// What the board could not load, and a board with no thread: loading,
	// or none yet. It says there is none only once the threads loaded.
	nodes = append(nodes, core.Dynamic(func(*core.Scope) core.Node {
		c.revision.Get()
		var out []core.Node
		for _, list := range []struct {
			status        func() (state.ListStatus, string)
			what, missing string
		}{
			{d.ThreadsStatus, "the threads", "Threads may be missing or out of date."},
			{d.AsksStatus, "the asks", "A thread may wait for your answer and show in another group."},
			{d.PullsStatus, "the pull requests", "A thread may wait for your review and show in another group."},
		} {
			if status, problem := list.status(); status == state.ListFailed {
				out = append(out, c.listProblem(list.what, problem, list.missing, c.store.Refresh))
			}
		}
		if status, _ := d.ThreadsStatus(); len(d.Threads()) == 0 {
			switch status {
			case state.ListLoading:
				out = append(out, body("Loading the threads…"))
			case state.ListLoaded:
				out = append(out, core.View{Style: core.Style{Height: core.Pt(180)}, Children: []core.Node{workspaceui.EmptyState{Icon: lucide.Columns3, Title: "No threads yet",
					Hint: "When a supervisor delegates work to a specialist, its thread shows here, grouped by who moves it next."}}})
			}
		}
		return core.Group(out)
	}))
	// A plan that waits for the owner and has no open thread to show on
	// (state.PlansOffTheBoard) keeps a card of its own, before the groups.
	nodes = append(nodes, c.plansOffTheBoard(cx))
	// A group shows while it holds a thread, as the MyGo client's. The
	// closed one starts folded, unless the owner comes back to a card of
	// it.
	for i, m := range state.Movers {
		items := func() []proto.Thread { return groups.Get()[i].Threads }
		name, id := moverTitle(m)
		nodes = append(nodes, core.Show{When: func() bool { return len(items()) > 0 }, Then: func(*core.Scope) core.Node {
			open := m != state.MoverNobody
			if !open {
				source := c.nav.visits[c.nav.index].dest.card
				for _, thread := range groups.GetUntracked()[i].Threads {
					if fmt.Sprintf("thread-%d", thread.ID) == source {
						open = true
						break
					}
				}
			}
			return core.View{TestID: "board-group-" + id, Children: []core.Node{workspaceui.Section{Title: name, Count: func() int { return len(items()) }, InitiallyOpen: open,
				Content: func(cx *core.Scope) core.Node { return c.threadCards(cx, items) }}}}
		}})
	}
	// The schedules and the proposals always show: with none, each says
	// so, and what adds one.
	always := func() bool { return true }
	schedules := section("Schedules", true, func() int { c.revision.Get(); return len(d.Schedules().Schedules) }, always, c.schedules)
	night := section("Night shift", true, func() int { c.revision.Get(); return len(d.BoardProposals()) }, always, c.nightShift)
	// A desktop's window draws the threads, then the schedules and the
	// night shift. A compact window, a phone's, keeps a section that holds
	// something above the threads, and puts one that holds nothing after
	// them, where it takes none of the screen the threads need
	// (state.BoardOrder).
	type order struct{ compact, proposals, schedules bool }
	parts := core.NewMemo(cx, func() order {
		c.revision.Get()
		if !c.compact() {
			return order{}
		}
		proposals, _ := d.ProposalsStatus()
		listed, _ := d.SchedulesStatus()
		return order{compact: true,
			proposals: state.NightShiftHolds(len(d.BoardProposals()), proposals, state.ProposalsSay(c.store.LaunchUncertain(), c.store.Launching(), c.store.Launched(), timeNow())),
			schedules: state.NightShiftHolds(len(d.Schedules().Schedules), listed, d.Schedules().Problem != "")}
	})
	head, threads := nodes[:2], nodes[2:]
	ordered := core.Dynamic(func(*core.Scope) core.Node {
		o := parts.Get()
		out := append([]core.Node(nil), head...)
		if !o.compact {
			return core.Group(append(append(out, threads...), schedules, night))
		}
		for _, part := range state.BoardOrder(true, o.proposals, o.schedules) {
			switch part {
			case state.PartProposals:
				out = append(out, night)
			case state.PartSchedules:
				out = append(out, schedules)
			case state.PartThreads:
				out = append(out, threads...)
			}
		}
		return core.Group(out)
	})
	// A card is as wide as the page: the board keeps the kit's measure for
	// a page, as the Inbox does.
	return workspaceui.Page(core.View{TestID: "board", Style: core.Style{Gap: workspaceui.SpaceM}, Children: []core.Node{ordered}})
}

// moverTitle names a group of the board, as the MyGo client names it, and
// gives the id its view takes.
func moverTitle(m state.Mover) (title, id string) {
	switch m {
	case state.MoverOwner:
		return "Waits for you", "owner"
	case state.MoverSpecialist:
		return "With the specialists", "specialists"
	case state.MoverSupervisor:
		return "With the supervisors", "supervisors"
	}
	return "Closed", "closed"
}

// cardFolds is what the owner opened on a thread's card: the whole of its
// report, its details and its plan. They outlast a change of the thread.
type cardFolds struct{ report, details, plan *core.Signal[bool] }

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
			src := c.nav.visits[c.nav.index].dest
			folds := cardFolds{report: core.NewSignal(cx, false), plan: core.NewSignal(cx, false),
				details: core.NewSignal(cx, src.details && src.card == fmt.Sprintf("thread-%d", core.Untracked(cx, item).ID))}
			// Other sessions can be busy while the owner reads a report.
			thread := core.NewMemoFunc(cx, item, func(a, b proto.Thread) bool { return reflect.DeepEqual(a, b) })
			return core.Dynamic(func(cx *core.Scope) core.Node {
				return c.cardAnchor(fmt.Sprintf("thread-%d", thread.Get().ID), c.threadCard(cx, thread.Get(), folds))
			})
		})
	// The pages are NSGo's pagination, beside which of the group's threads
	// the page shows.
	pager := core.Show{When: func() bool { return len(items()) > pageSize }, Then: func(*core.Scope) core.Node {
		return workspaceui.Row(
			ui.Pagination{Label: "Pages", PreviousLabel: "Previous", NextLabel: "Next", Compact: func() bool { return true },
				Page: func() int { return offset()/pageSize + 1 }, TotalPages: func() int { return (len(items()) + pageSize - 1) / pageSize },
				OnChange: func(n int) { page.Set(n - 1) }},
			core.Text{Style: ui.TypeCaption, ContentFunc: func() string {
				n := len(items())
				start := offset()
				return fmt.Sprintf("%d–%d of %d", start+1, min(start+pageSize, n), n)
			}})
	}}
	return workspaceui.Stack(rows, pager)
}

// boardCard is the kit's card (workspaceui.Card), which takes no border
// color, with the accent on its border while marked says so: a thread
// that waits for the owner.
func boardCard(cx *core.Scope, marked func() bool, children []core.Node) core.Node {
	colors := ui.ColorsOf(cx)
	return core.View{Style: core.Style{Padding: core.All(workspaceui.SpaceL), Gap: workspaceui.SpaceS, CornerRadius: ui.RadiusCard, BorderWidth: 1},
		StyleFunc: func() core.Style {
			border := ui.Mix(colors.Border(), colors.Text(), 0.12)
			if marked() {
				border = colors.Accent()
			}
			return core.Style{Background: colors.Raised(), BorderColor: border}
		}, Children: children}
}

// threadBadge shows a thread's state in its tone, in the MyGo client's
// words.
func threadBadge(st proto.ThreadState) core.Node {
	switch st {
	case proto.ThreadBlocked:
		return ui.Badge{Text: "blocked", Variant: ui.BadgeWarn}
	case proto.ThreadDone:
		return ui.Badge{Text: "done", Variant: ui.BadgeSuccess}
	case proto.ThreadClosed:
		return ui.Badge{Text: "closed"}
	}
	return ui.Badge{Text: string(st), Variant: ui.BadgeAccent}
}

// threadCard shows a thread, as the MyGo client's card does: who works on
// it and where, why it is in its group, its plan, the specialist's last
// report, its pull requests and the ways to its sessions; the owner may
// close it. A long brief in full and its shift's rounds stay in its
// details.
func (c *client) threadCard(cx *core.Scope, t proto.Thread, folds cardFolds) core.Node {
	d := &c.store.Data
	key, name := fmt.Sprintf("thread-%d", t.ID), fmt.Sprintf("T%d", t.ID)
	mover := core.NewMemo(cx, func() state.Mover { c.revision.Get(); return d.NextMover(t) })
	// Its title, with its state at the row's end.
	heading := []core.Node{core.Text{Content: t.Title, Style: ui.TypeTitle.Merge(core.Style{FlexGrow: 1, FlexShrink: 1, MinWidth: core.Pt(0)})}, threadBadge(t.State)}
	// A message to its closed specialist took it back to open (ADR-0061).
	if state.Reopened(t) {
		heading = append(heading, ui.Badge{Text: "reopened", Variant: ui.BadgeWarn})
	}
	// The agent by its name, where it works, and what the thread's
	// sessions used: the profiles and the sessions load beside the threads.
	head := []core.Node{core.View{Style: core.Style{Direction: core.Row, AlignItems: core.AlignCenter, Gap: workspaceui.SpaceS}, Children: heading},
		c.captionFunc(func() string {
			c.revision.Get()
			return strings.Join(nonempty(name, d.AgentName(t.Agent), threadWhere(t), state.UsageShort(d.ThreadUsage(t))), " · ")
		})}
	if t.Session != 0 {
		changes := core.NewMemo(cx, func() string {
			c.revision.Get()
			sc, ok := d.Changes(t.Session)
			return state.CardChanges(sc, ok)
		})
		// The session's details list the changes: the shell opens that
		// panel only at its own button.
		head = append(head, core.Show{When: func() bool { return changes.Get() != "" }, Then: func(*core.Scope) core.Node {
			return workspaceui.Row(ui.Tooltip{Text: "Open this thread's session: its details list what it changed",
				Trigger: ui.Button{Icon: lucide.FileDiff, TextFunc: changes.Get, LabelFunc: func() string { return "Changes: " + changes.Get() },
					Variant: ui.ButtonGhost, Size: ui.SizeSmall, OnPress: func() {
						c.rememberCard(key)
						c.openSession(t.Session)
						c.store.RefreshChanges(t.Session)
						c.changed()
					}}})
		}})
	}
	if t.Plan != 0 && t.PlanTask != "" {
		head = append(head, c.captionFunc(func() string {
			c.revision.Get()
			text := fmt.Sprintf("Task %s of plan P%d", t.PlanTask, t.Plan)
			if p, ok := d.Plan(t.Plan); ok {
				text += " · " + p.Title
			}
			return text
		}))
	}
	head = append(head, core.Text{ContentFunc: func() string { c.revision.Get(); return c.threadWhy(t, mover.Get()) },
		Selectable: core.SelectableOn, StyleFunc: ui.Colored(ui.ColorsOf(cx).Muted)})
	// The thread it carries on, and the one that carries it on.
	if links := state.ThreadLinks(t); links != "" {
		head = append(head, c.caption(links))
	}
	if shift := t.Shift; shift != nil {
		head = append(head, workspaceui.Row(shiftBadge(*shift), core.Text{Content: state.ShiftLine(*shift), Selectable: core.SelectableOn,
			Style: ui.TypeCaption.Merge(core.Style{FlexGrow: 1, FlexShrink: 1, MinWidth: core.Pt(0)}), StyleFunc: ui.Colored(ui.ColorsOf(cx).Muted)}))
	}
	asks := core.NewMemoFunc(cx, func() []proto.Ask { c.revision.Get(); return d.AsksOf(t.ID) }, func(a, b []proto.Ask) bool {
		return slices.EqualFunc(a, b, func(x, y proto.Ask) bool { return x.ID == y.ID })
	})
	// Open the Inbox goes to the thread's first ask. With more than one,
	// each has its own way there.
	head = append(head, core.Dynamic(func(*core.Scope) core.Node {
		var links []core.Node
		for _, a := range asks.Get() {
			links = append(links, ui.Button{Text: fmt.Sprintf("View ask A%d", a.ID), Variant: ui.ButtonTinted, OnPress: func() {
				c.rememberCard(key)
				c.goTo(destination{page: "Inbox", card: state.AskCard(a.ID)})
			}})
		}
		if len(links) < 2 {
			return nil
		}
		return workspaceui.Row(links...)
	}))
	// Its brief. A short one shows whole. One that folds as a report does
	// (state.ReportFolds) shows 3 lines, and all of it in the details,
	// which also hold what its shift stops at and what each round found.
	long := state.ReportFolds(t.Task)
	switch {
	case long:
		head = append(head, core.Show{When: func() bool { return !folds.details.Get() }, Then: func(*core.Scope) core.Node { return core.Text{Content: t.Task, MaxLines: 3} }})
	case t.Task != "":
		head = append(head, body(t.Task))
	}
	if details := threadDetails(t, long); len(details) > 0 {
		head = append(head, ui.Collapsible{Text: fmt.Sprintf("Details for T%d", t.ID), Trigger: []core.Node{core.Text{Content: "Details", Style: ui.TypeLabel}}, Variant: ui.CollapsibleArrowRight,
			Open: folds.details.Get, OnOpenChange: folds.details.Set, Content: core.Show{When: folds.details.Get, Then: func(*core.Scope) core.Node { return workspaceui.Stack(details...) }}})
	}
	if state.PlanThread(t) {
		head = append(head, c.planOnThread(cx, t, folds.plan))
	}
	if t.Report != "" {
		head = append(head, c.threadReport(cx, t, folds.report))
	}
	head = append(head, core.Dynamic(func(*core.Scope) core.Node {
		c.revision.Get()
		var pulls []core.Node
		for _, p := range d.PullsOf(t.ID) {
			pulls = append(pulls, c.pull(p))
		}
		return core.Group(pulls)
	}))
	head = append(head, c.threadActions(cx, t, mover.Get, folds), c.threadChanging(t))
	return boardCard(cx, func() bool { return mover.Get() == state.MoverOwner }, head)
}

// shiftBadge marks a thread's shift line (ADR-0024) in the tone of the
// shift's state: running, paused for the owner, ended, and ended done.
func shiftBadge(sh proto.Shift) core.Node {
	variant := ui.BadgeAccent
	switch sh.State {
	case proto.ShiftPaused:
		variant = ui.BadgeWarn
	case proto.ShiftEnded:
		variant = ui.BadgeNeutral
		if sh.Reason == "done" {
			variant = ui.BadgeSuccess
		}
	}
	return ui.Badge{Text: "shift", Variant: variant}
}

// threadWhere says where a thread's agent works: its repository and
// branch, else the last 2 elements of its folder.
func threadWhere(t proto.Thread) string {
	switch {
	case t.Repository != "" && t.Branch != "":
		return t.Repository + " on " + t.Branch
	case t.Repository != "":
		return t.Repository
	case t.Cwd != "":
		return "in " + shortPath(t.Cwd)
	}
	return "its folder is not set up yet"
}

// shortPath is the last 2 elements of a path on the service host.
func shortPath(p string) string {
	dir, base := path.Split(path.Clean(p))
	parent := path.Base(dir)
	if parent == "/" || parent == "." || parent == "" {
		return base
	}
	return parent + "/" + base
}

// threadDetails is what a thread's card keeps folded: its brief in full,
// when the card cuts it (brief), and of its shift what it stops at and
// what each round found.
func threadDetails(t proto.Thread, brief bool) []core.Node {
	var out []core.Node
	if brief {
		out = append(out, body(t.Task))
	}
	if shift := t.Shift; shift != nil {
		if shift.Stop != "" {
			out = append(out, body(shift.Stop))
		}
		for _, round := range shift.Rounds {
			out = append(out, body(fmt.Sprintf("Round %d · %s · %s", round.N, round.Verdict, round.Reason)))
		}
	}
	return out
}

// threadReport shows the specialist's last report on its thread's card, as
// rendered Markdown. One that folds shows its head (state.ReportHead)
// until the owner unfolds it, as the MyGo client's does; a short one shows
// whole, with no button.
func (c *client) threadReport(cx *core.Scope, t proto.Thread, open *core.Signal[bool]) core.Node {
	head, more := state.ReportHead(t.Report)
	out := []core.Node{core.Dynamic(func(*core.Scope) core.Node {
		if more && !open.Get() {
			return c.markdownHead(func() string { return head })
		}
		return c.markdown(func() string { return t.Report }, nil)
	})}
	if more {
		label := func() string {
			if open.Get() {
				return "Show Less"
			}
			return "Show the Whole Report"
		}
		out = append(out, workspaceui.Row(ui.Button{TextFunc: label, LabelFunc: label, Variant: ui.ButtonSecondary, Size: ui.SizeSmall, OnPress: func() { open.Set(!open.Get()) }}))
	}
	return core.View{TestID: fmt.Sprintf("report-%d", t.ID), Style: core.Style{Padding: core.All(workspaceui.SpaceS), Gap: workspaceui.SpaceS, CornerRadius: ui.RadiusControl},
		StyleFunc: ui.Filled(ui.ColorsOf(cx).Inset), Children: out}
}

// threadActions are a thread's buttons, as the MyGo client's card has
// them: the Inbox, filled, while the owner moves the thread and the Inbox
// holds what waits (an ask of it, or its plan's approval), then its
// specialist's and its supervisor's sessions, and at the row's end its
// shift's changes and its close.
func (c *client) threadActions(cx *core.Scope, t proto.Thread, mover func() state.Mover, folds cardFolds) core.Node {
	d := &c.store.Data
	key, name := fmt.Sprintf("thread-%d", t.ID), fmt.Sprintf("T%d", t.ID)
	// The card of the Inbox the owner goes to: the thread's first ask, or
	// its plan that waits for an approval.
	waits := core.NewMemo(cx, func() string {
		c.revision.Get()
		if mover() != state.MoverOwner {
			return ""
		}
		if asks := d.AsksOf(t.ID); len(asks) > 0 {
			return state.AskCard(asks[0].ID)
		}
		if p, ok := d.Plan(t.Plan); ok && state.PlanThread(t) && state.ToApprove(p) {
			return state.PlanCard(p.ID)
		}
		return ""
	})
	// The way back from a session opens the details the owner had open.
	open := func(session int64) {
		c.rememberCard(key)
		c.nav.visits[c.nav.index].dest.details = folds.details.GetUntracked()
		c.openSession(session)
	}
	actions := []core.Node{core.Show{When: func() bool { return waits.Get() != "" }, Then: func(*core.Scope) core.Node {
		return ui.Button{Text: "Open the Inbox", OnPress: func() {
			c.rememberCard(key)
			c.goTo(destination{page: "Inbox", card: waits.Get()})
		}}
	}}}
	if t.Session != 0 {
		label := func() string { c.revision.Get(); return "Open " + d.AgentName(t.Agent) + "'s Session" }
		b := c.button("", func() { open(t.Session) })
		b.TextFunc, b.LabelFunc = label, label
		actions = append(actions, b)
	}
	if t.Supervisor != 0 {
		actions = append(actions, ui.Tooltip{TextFunc: func() string {
			c.revision.Get()
			if s, ok := d.Session(t.Supervisor); ok {
				return s.Title
			}
			return fmt.Sprintf("Session %d", t.Supervisor)
		}, Trigger: c.button("Open the Supervisor", func() { open(t.Supervisor) })})
	}
	actions = append(actions, core.View{Style: core.Style{FlexGrow: 1}})
	shifting := func() bool { c.revision.Get(); return c.store.ShiftChanging(t.ID).Busy }
	closing := func() bool { c.revision.Get(); return c.store.Closing(t.ID).Busy }
	action := func(label string, busy func() bool, fn func()) core.Node {
		b := c.button(label, fn)
		b.Disabled = busy
		return b
	}
	if state.CanResumeShift(t) {
		actions = append(actions, action("Resume Shift", shifting, func() { c.store.ResumeShift(t.ID) }))
	}
	if state.CanStopShift(t) {
		actions = append(actions, action("Stop Shift", shifting, func() {
			c.destroy("Stop "+name+"'s shift?",
				"The shift ends: pocketd sends no more rounds, and the supervisor hears that you stopped it. "+
					"A turn the agent is in goes on to its end; abort its session to cut it short. "+
					"The thread stays on the desk, and you can resume the shift while it has rounds and time left.",
				"Stop the Shift", func() { c.store.StopShift(t.ID) })
		}))
	}
	if t.State != proto.ThreadClosed {
		actions = append(actions, action("Close Thread", closing, func() { c.confirmClose(t) }))
	}
	return workspaceui.Row(actions...)
}

// threadChanging says where the owner's change of a thread's shift and its
// close stand: on their way, or why pocketd refused. A stop or a close
// moves the card to another group, which may be folded.
func (c *client) threadChanging(t proto.Thread) core.Node {
	name := fmt.Sprintf("T%d", t.ID)
	return core.Dynamic(func(*core.Scope) core.Node {
		c.revision.Get()
		var lines []core.Node
		if s := c.store.ShiftChanging(t.ID); s.Busy {
			lines = append(lines, body("Changing "+name+"'s shift…"))
		} else if s.Err != "" {
			lines = append(lines, body(s.Err))
		}
		s := c.store.Closing(t.ID)
		if s.Busy {
			lines = append(lines, body("Closing "+name+"…"))
		}
		if s.Err != "" {
			lines = append(lines, body(s.Err))
		}
		return core.Group(lines)
	})
}

// confirmClose asks before the owner closes a thread. A plan's own thread
// says that its close drops the plan, and names it.
func (c *client) confirmClose(t proto.Thread) {
	d := &c.store.Data
	name, agent := fmt.Sprintf("T%d", t.ID), d.AgentName(t.Agent)
	if state.PlanThread(t) {
		plan := fmt.Sprintf("P%d", t.Plan)
		if p, ok := d.Plan(t.Plan); ok {
			plan += " (" + p.Title + ")"
		}
		c.destroy("Close "+name+" and drop "+plan+"?",
			name+" is where "+agent+" plans "+plan+". Closing it drops the plan for good: a task of it not delivered yet can no longer be, "+
				"and its consults stop. The threads of its delivered tasks, its sessions and its asks stay.",
			"Close "+name+" and Drop the Plan", func() { c.store.CloseThread(t.ID) })
		return
	}
	c.destroy("Close "+name+"?",
		name+" leaves the desk: "+agent+" can no longer report on it, and a brief not started yet is withdrawn. "+
			"Its sessions, its open asks and its pull requests stay.",
		"Close "+name, func() { c.store.CloseThread(t.ID) })
}

// threadWhy says why a thread is where it is, given who moves it next.
func (c *client) threadWhy(th proto.Thread, m state.Mover) string {
	d := &c.store.Data
	agent := d.AgentName(th.Agent)
	supervisor := c.supervisorName(th.Supervisor)
	switch m {
	case state.MoverOwner:
		asks := d.AsksOf(th.ID)
		if p, ok := d.Plan(th.Plan); ok && state.PlanThread(th) && len(asks) == 0 {
			if state.ToApprove(p) {
				return fmt.Sprintf("P%d waits for your approval.", p.ID)
			}
			if open := state.OpenAsksOf(p); len(open) > 0 {
				return fmt.Sprintf("P%d waits for your answer: %s.", p.ID, state.JoinAnd(open))
			}
			return "Its tasks wait for you: " + threadNames(d.TaskThreadsMovedBy(p, state.MoverOwner)) + "."
		}
		if len(asks) > 0 {
			names := make([]string, len(asks))
			for i, ask := range asks {
				names[i] = fmt.Sprintf("A%d", ask.ID)
			}
			if len(asks) == 1 {
				return names[0] + " waits for your answer."
			}
			return state.JoinAnd(names) + " wait for your answers."
		}
		var refs []string
		for _, p := range d.PullsOf(th.ID) {
			if state.ToReview(p) {
				refs = append(refs, state.PullRef(p))
			}
		}
		if len(refs) == 0 && state.ShiftWaitsForOwner(th) {
			// It names only the actions the card offers: a pause in the
			// last round leaves no round to resume.
			switch {
			case state.CanResumeShift(th):
				return "Its shift is paused for your message: resume or stop it."
			case state.CanStopShift(th):
				return "Its shift is paused for your message in its last round: stop it."
			}
			return "Its shift is paused for your message."
		}
		if len(refs) == 1 {
			return refs[0] + " waits for your review."
		}
		return state.JoinAnd(refs) + " wait for your review."
	case state.MoverSpecialist:
		if th.State == proto.ThreadStarting {
			return "pocketd sets it up for " + agent + "."
		}
		if p, ok := d.Plan(th.Plan); ok && state.PlanThread(th) && (p.State == proto.PlanApproved || p.State == proto.PlanBuilding) {
			var at []string
			if p.Graph != nil {
				for _, task := range p.Graph.Tasks {
					if task.State == proto.TaskDelegated && task.Thread != 0 {
						at = append(at, fmt.Sprintf("%s in T%d", task.ID, task.Thread))
					}
				}
			}
			if len(at) == 0 {
				return "Its tasks are at work."
			}
			return "Its tasks are at work: " + state.JoinAnd(at) + "."
		}
		if state.PlanThread(th) {
			return agent + " plans it."
		}
		return agent + " works on it."
	case state.MoverSupervisor:
		if p, ok := d.Plan(th.Plan); ok && state.PlanThread(th) {
			if ready := state.ReadyTasks(p); len(ready) > 0 {
				return supervisor + " delivers its ready tasks: " + state.JoinAnd(ready) + "."
			}
			if p.State == proto.PlanApproved || p.State == proto.PlanBuilding {
				return "Its tasks wait for " + supervisor + ": " + threadNames(d.TaskThreadsMovedBy(p, state.MoverSupervisor)) + "."
			}
		}
		switch th.State {
		case proto.ThreadDone:
			return agent + " reported it done; " + supervisor + " reads the report."
		case proto.ThreadBlocked:
			return agent + " is blocked; " + supervisor + " reads the report."
		}
		return supervisor + " reads the report."
	}
	return "Closed."
}

// threadNames names threads, as T8 and T9.
func threadNames(ids []int64) string {
	names := make([]string, len(ids))
	for i, id := range ids {
		names[i] = fmt.Sprintf("T%d", id)
	}
	if len(names) == 0 {
		return "none"
	}
	return state.JoinAnd(names)
}

// supervisorName names the session that delegated a thread: its profile's
// name, else its title.
func (c *client) supervisorName(id int64) string {
	if s, ok := c.store.Data.Session(id); ok {
		if s.Profile != "" {
			return c.store.Data.AgentName(s.Profile)
		}
		return s.Title
	}
	return "the supervisor"
}
