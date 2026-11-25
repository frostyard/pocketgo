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
			{d.AsksStatus, "the asks", "A thread may wait for your answer and not say so."},
			{d.PullsStatus, "the pull requests", "A thread may wait for your review and not say so."},
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
					Hint: "When a supervisor delegates work to a specialist, its thread shows here."}}})
			}
		}
		return core.Group(out)
	}))
	openBoard := core.Show{When: func() bool { return len(byState.Get()[proto.ThreadClosed]) < len(d.Threads()) },
		Then: func(cx *core.Scope) core.Node { return c.threadLanes(cx, byState.Get) }}
	closed := func() []proto.Thread { return byState.Get()[proto.ThreadClosed] }
	nodes = append(nodes, openBoard, section("Closed", false, func() int { return len(closed()) }, nil, func(cx *core.Scope) core.Node { return c.threadCards(cx, closed) }))
	nodes = append(nodes, section("Plans", true, func() int { c.revision.Get(); return len(d.Plans()) }, nil, func(*core.Scope) core.Node {
		return core.Dynamic(func(*core.Scope) core.Node {
			c.revision.Get()
			var cards []core.Node
			for _, p := range d.Plans() {
				cards = append(cards, core.View{TestID: fmt.Sprintf("plan-%d", p.ID), Children: []core.Node{workspaceui.Card{Children: []core.Node{c.planHead(p), c.plan(p)}}}})
			}
			return workspaceui.Stack(cards...)
		})
	}))
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
	return core.Scroll{Style: ui.FillRest, Children: []core.Node{core.View{TestID: "board",
		Style: core.Style{Width: core.Pct(100), MaxWidth: core.Pt(1560), AlignSelf: core.AlignCenter, Padding: core.All(workspaceui.SpaceL), Gap: 14}, Children: []core.Node{ordered},
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
		column := core.View{TestID: "board-lane-" + string(lane.key), Style: core.Style{MinWidth: core.Pt(0), Padding: core.All(workspaceui.SpaceS), CornerRadius: ui.RadiusInset, BorderWidth: 1, Borders: core.Edges{Top: 3}},
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
	// The pages are NSGo's pagination, beside which of the lane's threads
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

// threadCard shows a thread: who works on it and where, why it is where it
// is, and, in its details, the specialist's last report, its pull requests
// and the ways to its sessions; the owner may close it.
func (c *client) threadCard(cx *core.Scope, t proto.Thread, report, details *core.Signal[bool]) core.Node {
	d := &c.store.Data
	key, name := fmt.Sprintf("thread-%d", t.ID), fmt.Sprintf("T%d", t.ID)
	// The agent by its name, where it works, and what the thread's
	// sessions used: the profiles and the sessions load beside the threads.
	head := []core.Node{title(t.Title), c.captionFunc(func() string {
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
	mover := core.NewMemo(cx, func() state.Mover { c.revision.Get(); return d.NextMover(t) })
	if t.State != proto.ThreadClosed {
		badges := []core.Node{ui.Badge{TextFunc: func() string {
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
		}}}
		// A message to its closed specialist took it back to open
		// (ADR-0061).
		if state.Reopened(t) {
			badges = append(badges, ui.Badge{Text: "reopened", Variant: ui.BadgeWarn})
		}
		head = append(head, workspaceui.Row(badges...))
	}
	head = append(head, core.Text{ContentFunc: func() string { c.revision.Get(); return c.threadWhy(t, mover.Get()) },
		Selectable: core.SelectableOn, StyleFunc: ui.Colored(ui.ColorsOf(cx).Muted)})
	if state.PlanThread(t) {
		plan := core.NewMemoFunc(cx, func() *proto.Plan {
			c.revision.Get()
			if p, ok := d.Plan(t.Plan); ok {
				return &p
			}
			return nil
		}, func(a, b *proto.Plan) bool { return reflect.DeepEqual(a, b) })
		head = append(head, core.Dynamic(func(*core.Scope) core.Node {
			if p := plan.Get(); p != nil {
				return c.planHead(*p)
			}
			return c.caption(fmt.Sprintf("Plan P%d", t.Plan))
		}))
	}
	if t.State == proto.ThreadClosed {
		head = append(head, c.threadDetails(cx, t, report)...)
		return workspaceui.Card{Children: append(head, c.threadChanging(t))}
	}
	asks := core.NewMemoFunc(cx, func() []proto.Ask { c.revision.Get(); return d.AsksOf(t.ID) }, func(a, b []proto.Ask) bool {
		return slices.EqualFunc(a, b, func(x, y proto.Ask) bool { return x.ID == y.ID })
	})
	head = append(head, core.Dynamic(func(*core.Scope) core.Node {
		var links []core.Node
		for _, a := range asks.Get() {
			links = append(links, ui.Button{Text: fmt.Sprintf("View ask A%d", a.ID), Variant: ui.ButtonTinted, OnPress: func() {
				c.rememberCard(key)
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
	return workspaceui.Card{Children: append(head, c.threadChanging(t))}
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

func (c *client) threadDetails(cx *core.Scope, t proto.Thread, report *core.Signal[bool]) []core.Node {
	d := &c.store.Data
	name := fmt.Sprintf("T%d", t.ID)
	var out []core.Node
	if links := state.ThreadLinks(t); links != "" {
		out = append(out, body(links))
	}
	if t.Task != "" {
		out = append(out, body(t.Task))
	}
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
	shifting := func() bool { c.revision.Get(); return c.store.ShiftChanging(t.ID).Busy }
	closing := func() bool { c.revision.Get(); return c.store.Closing(t.ID).Busy }
	action := func(label string, busy func() bool, fn func()) core.Node {
		b := c.button(label, fn)
		b.Disabled = busy
		return b
	}
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
		actions = append(actions, action("Resume shift", shifting, func() { c.store.ResumeShift(t.ID) }))
	}
	if state.CanStopShift(t) {
		actions = append(actions, action("Stop shift", shifting, func() {
			c.destroy("Stop "+name+"'s shift?",
				"The shift ends: pocketd sends no more rounds, and the supervisor hears that you stopped it. "+
					"A turn the agent is in goes on to its end; abort its session to cut it short. "+
					"The thread stays on the desk, and you can resume the shift while it has rounds and time left.",
				"Stop the Shift", func() { c.store.StopShift(t.ID) })
		}))
	}
	if t.State != proto.ThreadClosed {
		actions = append(actions, action("Close thread", closing, func() { c.confirmClose(t) }))
	}

	if len(actions) > 0 {
		out = append(out, workspaceui.Row(actions...))
	}
	return out
}

// threadChanging says where the owner's change of a thread's shift and its
// close stand: on their way, or why pocketd refused. It stands on the
// card's face: a stop or a close moves the card to another lane, folded.
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
