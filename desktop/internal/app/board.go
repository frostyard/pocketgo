package app

import (
	"github.com/egoist/mygo/ui"
	"github.com/frostyard/frostui"

	"github.com/frostyard/pocketgo/desktop/internal/state"
	"github.com/frostyard/pocketgo/proto"
)

// boardPane is the work board: the threads grouped by who moves them next
// (state.Data.Board), each group most recently changed first, the closed
// ones folded last.
func (a *App) boardPane(c *ui.Context) {
	t := c.Theme()
	d := &a.st.Data
	status, problem := d.ThreadsStatus()
	asksStatus, asksProblem := d.AsksStatus()
	pullsStatus, pullsProblem := d.PullsStatus()
	ui.Scroll(c).Grow(1).Fill().Background(t.Background).Children(func() {
		ui.Column(c).AlignItems(ui.Center).Padding(frostui.SpaceL, frostui.SpaceXL).Children(func() {
			ui.Column(c).MaxWidth(860).FillWidth().Gap(frostui.SpaceM).AlignItems(ui.Stretch).Children(func() {
				if status == state.ListFailed {
					a.listProblem(c, "Loading the threads failed: "+problem+". Threads may be missing or out of date.")
				}
				if asksStatus == state.ListFailed {
					a.listProblem(c, "Loading the questions failed: "+asksProblem+". A thread may wait for your answer and show in another group.")
				}
				if pullsStatus == state.ListFailed {
					a.listProblem(c, "Loading the pull requests failed: "+pullsProblem+". A thread may wait for your review and show in another group.")
				}
				threads := d.Threads()
				switch {
				case status == state.ListLoading && len(threads) == 0:
					frostui.Loading(c, "Loading the threads…")
					return
				case status == state.ListLoaded && len(threads) == 0:
					frostui.Empty(c, "view-grid", "No threads yet",
						"When a supervisor delegates work to a specialist, its thread shows here, grouped by who moves it next.")
					return
				}
				for _, g := range d.Board() {
					if len(g.Threads) > 0 {
						a.boardGroup(c, g)
					}
				}
			})
		})
	})
}

// moverTitle names a group of the board, with its icon.
func moverTitle(m state.Mover) (title, icon string) {
	switch m {
	case state.MoverOwner:
		return "Waits for you", "mail-unread"
	case state.MoverSpecialist:
		return "With the specialists", "system-users"
	case state.MoverSupervisor:
		return "With the supervisors", "avatar-default"
	default:
		return "Closed", "object-select"
	}
}

// boardGroup draws one group of the board; the closed one is folded.
func (a *App) boardGroup(c *ui.Context, g state.BoardGroup) {
	title, icon := moverTitle(g.Mover)
	folded := g.Mover == state.MoverNobody && !a.closedOpen
	label := title + ", " + plural(len(g.Threads), "thread", "threads")
	ui.Column(c.Key("group:" + title)).Gap(frostui.SpaceS).AlignItems(ui.Stretch).Role(ui.RoleGroup).Label(label).Children(func() {
		header := func() {
			frostui.Icon(c, icon)
			frostui.Strong(c, title)
			frostui.Caption(c, " "+itoa(len(g.Threads)))
		}
		if g.Mover == state.MoverNobody {
			ui.Row(c).Children(func() {
				if ui.ButtonBase(c).Row().Gap(frostui.SpaceXS).AlignItems(ui.Center).Cursor(ui.CursorPointer).
					Label(title).Expanded(!folded).Children(func() {
					frostui.Icon(c, chevron(!folded))
					header()
				}).Clicked() {
					a.closedOpen = !a.closedOpen
				}
			})
		} else {
			ui.Row(c).Gap(frostui.SpaceXS).AlignItems(ui.Center).Children(header)
		}
		if folded {
			return
		}
		for _, th := range g.Threads {
			a.threadCard(c, th, g.Mover)
		}
	})
}

// threadCard shows a thread: who works on it and where, why it is in its
// group, the specialist's last report, its pull requests, and the ways to
// its sessions; the owner may close it.
func (a *App) threadCard(c *ui.Context, th proto.Thread, m state.Mover) {
	t := c.Theme()
	key := "thread:" + itoa(int(th.ID))
	name := "T" + itoa(int(th.ID))
	agent := a.agentName(th.Agent)
	closing := a.st.Closing(th.ID)
	border := t.Border
	if m == state.MoverOwner {
		border = frostui.ToneColor(t, frostui.Accent)
	}
	ui.Column(c.Key(key)).Gap(frostui.SpaceS).Padding(frostui.SpaceM, frostui.SpaceL).AlignItems(ui.Stretch).
		Radius(10).Background(frostui.Panel(t)).BorderWidth(1, 1, 1, 1).BorderColor(border).
		Role(ui.RoleGroup).Label("Thread " + name).Children(func() {
		ui.Row(c).Gap(frostui.SpaceS).AlignItems(ui.Center).Children(func() {
			frostui.Strong(c, name+" · "+th.Title).Grow(1).SingleLine()
			threadPill(c, th.State)
		})
		where := "its folder is not set up yet"
		switch {
		case th.Repository != "":
			where = th.Repository
			if th.Branch != "" {
				where += " on " + th.Branch
			}
		case th.Cwd != "":
			where = "in " + shortPath(th.Cwd)
		}
		frostui.Caption(c, agent+" · "+where).SingleLine()
		if th.Plan != 0 && th.PlanTask != "" {
			a.taskLine(c, th)
		}
		frostui.Muted(c, a.threadWhy(th, m)).Wrap()
		if state.PlanThread(th) {
			a.planSummary(c, key, th)
		}
		if th.Report != "" {
			a.threadReport(c, key, th.Report)
		}
		for _, p := range a.st.Data.PullsOf(th.ID) {
			a.pullRow(c, p)
		}
		ui.Row(c).Gap(frostui.SpaceS).AlignItems(ui.Center).Wrap().Children(func() {
			if m == state.MoverOwner && len(a.st.Data.AsksOf(th.ID)) > 0 {
				if ui.PrimaryButton(c.Key(key+":inbox"), "Open the Inbox").Clicked() {
					a.goInbox()
				}
			}
			if th.Session != 0 {
				if ui.Button(c.Key(key+":session"), "Open "+agent+"'s Session").Clicked() {
					a.openSession(th.Session)
				}
			}
			if th.Supervisor != 0 {
				if ui.Button(c.Key(key+":supervisor"), "Open the Supervisor").Tooltip(a.sessionTitle(th.Supervisor)).Clicked() {
					a.openSession(th.Supervisor)
				}
			}
			ui.Spacer(c)
			if th.State != proto.ThreadClosed {
				if ui.Button(c.Key(key+":close"), "Close Thread").Disabled(closing.Busy).Clicked() {
					a.confirmClose(th, agent)
				}
			}
		})
		if closing.Busy {
			frostui.Loading(c, "Closing "+name+"…")
		}
		if closing.Err != "" {
			frostui.Problem(c, closing.Err).Wrap()
		}
	})
}

// threadWhy says why a thread is in its group.
func (a *App) threadWhy(th proto.Thread, m state.Mover) string {
	agent := a.agentName(th.Agent)
	supervisor := a.supervisorName(th.Supervisor)
	switch m {
	case state.MoverOwner:
		if p, ok := a.st.Data.Plan(th.Plan); ok && state.PlanThread(th) && state.ToApprove(p) && len(a.st.Data.AsksOf(th.ID)) == 0 {
			return "P" + itoa(int(p.ID)) + " waits for your approval."
		}
		asks := a.st.Data.AsksOf(th.ID)
		if p, ok := a.st.Data.Plan(th.Plan); ok && state.PlanThread(th) && len(asks) == 0 {
			if open := state.OpenAsksOf(p); len(open) > 0 {
				return "P" + itoa(int(p.ID)) + " waits for your answer: " + state.JoinAnd(open) + "."
			}
			if !state.ToApprove(p) {
				return "Its tasks wait for you: " + a.taskThreadNames(p) + "."
			}
		}
		if len(asks) > 0 {
			names := make([]string, len(asks))
			for i, ask := range asks {
				names[i] = "A" + itoa(int(ask.ID))
			}
			if len(asks) == 1 {
				return names[0] + " waits for your answer."
			}
			return state.JoinAnd(names) + " wait for your answers."
		}
		var refs []string
		for _, p := range a.st.Data.PullsOf(th.ID) {
			if state.ToReview(p) {
				refs = append(refs, state.PullRef(p))
			}
		}
		if len(refs) == 1 {
			return refs[0] + " waits for your review."
		}
		return state.JoinAnd(refs) + " wait for your review."
	case state.MoverSpecialist:
		if th.State == proto.ThreadStarting {
			return "pocketd sets it up for " + agent + "."
		}
		if p, ok := a.st.Data.Plan(th.Plan); ok && state.PlanThread(th) && (p.State == proto.PlanApproved || p.State == proto.PlanBuilding) {
			var at []string
			if p.Graph != nil {
				for _, task := range p.Graph.Tasks {
					if task.State == proto.TaskDelegated && task.Thread != 0 {
						at = append(at, task.ID+" in T"+itoa(int(task.Thread)))
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
		if p, ok := a.st.Data.Plan(th.Plan); ok && state.PlanThread(th) {
			if ready := state.ReadyTasks(p); len(ready) > 0 {
				return supervisor + " delivers its ready tasks: " + state.JoinAnd(ready) + "."
			}
			if p.State == proto.PlanApproved || p.State == proto.PlanBuilding {
				return "Its tasks wait for " + supervisor + ": " + a.taskThreadNames(p) + "."
			}
		}
		switch th.State {
		case proto.ThreadDone:
			return agent + " reported it done; " + supervisor + " reads the report."
		case proto.ThreadBlocked:
			return agent + " is blocked; " + supervisor + " reads the report."
		}
		return supervisor + " reads the report."
	default:
		return "Closed."
	}
}

// taskThreadNames names the threads of p's delivered tasks, as T8 and T9.
func (a *App) taskThreadNames(p proto.Plan) string {
	var names []string
	for _, id := range state.TaskThreads(p) {
		names = append(names, "T"+itoa(int(id)))
	}
	if len(names) == 0 {
		return "none delivered"
	}
	return state.JoinAnd(names)
}

// supervisorName names the session that delegated a thread: its profile's
// name, else its title.
func (a *App) supervisorName(id int64) string {
	if s, ok := a.st.Data.Session(id); ok {
		if s.Profile != "" {
			return a.agentName(s.Profile)
		}
		return s.Title
	}
	return "the supervisor"
}

// sessionTitle is a session's title, or its id.
func (a *App) sessionTitle(id int64) string {
	if s, ok := a.st.Data.Session(id); ok {
		return s.Title
	}
	return "Session " + itoa(int(id))
}

// threadReport shows the specialist's last report: one that folds
// (state.ReportFolds) shows its first four lines until the owner unfolds
// it.
func (a *App) threadReport(c *ui.Context, key, report string) {
	open := a.reportOpen[key]
	folds := state.ReportFolds(report)
	ui.Column(c).Gap(frostui.SpaceXS).Padding(frostui.SpaceXS, frostui.SpaceS).Radius(6).Background(frostui.ColorsOf(c.Theme()).Code).
		AlignItems(ui.Stretch).Children(func() {
		text := ui.Text(c, report).Wrap().Selectable()
		// Only a report the owner can unfold is cut: a short one shows
		// whole, however it wraps.
		if folds && !open {
			text.MaxLines(4).Ellipsis("…")
		}
		if folds {
			ui.Row(c).Children(func() {
				label := "Show the Whole Report"
				if open {
					label = "Show Less"
				}
				if ui.Button(c.Key(key+":report"), label).Clicked() {
					a.reportOpen[key] = !open
				}
			})
		}
	})
}

// pullRow shows a linked pull request on a thread's card.
func (a *App) pullRow(c *ui.Context, p proto.PullRequest) {
	key := "pull:" + itoa(int(p.ID))
	ui.Row(c.Key(key)).Gap(frostui.SpaceS).AlignItems(ui.Center).Children(func() {
		frostui.Icon(c, "insert-link")
		frostui.Strong(c, state.PullRef(p)).SingleLine()
		frostui.Muted(c, p.Title).SingleLine().Grow(1)
		if p.Ready {
			frostui.Pill(c, "ready-for-human", frostui.Success).Tooltip(readyTooltip(p))
		}
		pullPill(c, p)
		a.openOnGitHub(c, key, p, false)
	})
	if line := a.reviewLine(p); line != "" {
		frostui.Caption(c, line).Wrap()
	}
}

// readyTooltip says what the ready-for-human pill shows: the label as
// GitHub has it, and what pocketd last read of its review and CI
// (state.ReadyBacked), as past reads (ADR-0017).
func readyTooltip(p proto.PullRequest) string {
	text := "It has ready-for-human on GitHub."
	if state.ReadyBacked(p) {
		return text + " The newest cross-review at its head said ready, and CI was green when pocketd last read it."
	}
	return text + " pocketd has not read a ready cross-review at its head with green CI."
}

// reviewLine says what the newest cross-review at p's head found, and
// CI's state there (ADR-0017): "" when no verdict was filed at its head.
func (a *App) reviewLine(p proto.PullRequest) string {
	r := p.Review
	if r == nil {
		return ""
	}
	head := r.Head
	if len(head) > 12 {
		head = head[:12]
	}
	line := "Cross-review R" + itoa(int(r.ID)) + " by " + a.agentName(r.Agent) + " (" + r.Model.ID + "): " + string(r.Verdict) + " at " + head
	if p.Checks != "" {
		line += " · CI " + string(p.Checks)
	}
	if r.Verdict != proto.ReviewReady && len(r.Findings) > 0 {
		line += " · " + r.Findings[0]
	}
	return line
}

// openOnGitHub is a button that opens p on GitHub, at the link the client
// builds itself (state.PullURL); none when it cannot build one.
func (a *App) openOnGitHub(c *ui.Context, key string, p proto.PullRequest, primary bool) {
	link, ok := state.PullURL(p)
	if !ok {
		frostui.Caption(c, "no GitHub link")
		return
	}
	button := ui.Button
	if primary {
		button = ui.PrimaryButton
	}
	if button(c.Key(key+":github"), "Open on GitHub").Tooltip(link).Clicked() {
		c.OpenURLThen(link, func(err error) {
			if err != nil {
				a.toast("Opening " + link + " failed: " + err.Error())
			}
		})
	}
}

// pullPill shows a pull request's state in its tone.
func pullPill(c *ui.Context, p proto.PullRequest) {
	switch {
	case p.State == proto.PullMerged:
		frostui.Pill(c, "merged", frostui.Success)
	case p.State == proto.PullClosed:
		frostui.Pill(c, "closed", frostui.Neutral)
	case p.Draft:
		frostui.Pill(c, "draft", frostui.Neutral)
	default:
		frostui.Pill(c, "open", frostui.Accent)
	}
}

// threadPill shows a thread's state in its tone.
func threadPill(c *ui.Context, st proto.ThreadState) {
	switch st {
	case proto.ThreadBlocked:
		frostui.Pill(c, "blocked", frostui.Warning)
	case proto.ThreadDone:
		frostui.Pill(c, "done", frostui.Success)
	case proto.ThreadClosed:
		frostui.Pill(c, "closed", frostui.Neutral)
	case proto.ThreadStarting:
		frostui.Pill(c, "starting", frostui.Accent)
	default:
		frostui.Pill(c, string(st), frostui.Accent)
	}
}

// confirmClose asks before the owner closes a thread. A plan's own
// thread says that its close drops the plan (Codex review of #25, round
// 1).
func (a *App) confirmClose(th proto.Thread, agent string) {
	name := "T" + itoa(int(th.ID))
	id := th.ID
	if state.PlanThread(th) {
		plan := "P" + itoa(int(th.Plan))
		if p, ok := a.st.Data.Plan(th.Plan); ok {
			plan += " (" + p.Title + ")"
		}
		a.dialogs.Ask(frostui.Confirm{
			Title: "Close " + name + " and drop " + plan + "?",
			Body: name + " is where " + agent + " plans " + plan + ". Closing it drops the plan for good: a task of it not delivered yet can no longer be, " +
				"and its consults stop. The threads of its delivered tasks, its sessions and its questions stay.",
			Action: "Close " + name + " and Drop the Plan", Destructive: true, OnConfirm: func() { a.st.CloseThread(id) },
		})
		return
	}
	a.dialogs.Ask(frostui.Confirm{
		Title: "Close " + name + "?",
		Body: name + " leaves the desk: " + agent + " can no longer report on it, and a brief not started yet is withdrawn. " +
			"Its sessions, its open questions and its pull requests stay.",
		Action: "Close " + name, Destructive: true, OnConfirm: func() { a.st.CloseThread(id) },
	})
}

// pullCard shows a pull request that waits for the owner's review in the
// Inbox, with its thread and a way to it on GitHub.
func (a *App) pullCard(c *ui.Context, p proto.PullRequest) {
	t := c.Theme()
	key := "review:" + itoa(int(p.ID))
	ui.Column(c.Key(key)).Gap(frostui.SpaceS).Padding(frostui.SpaceM, frostui.SpaceL).AlignItems(ui.Stretch).
		Radius(10).Background(frostui.Panel(t)).BorderWidth(1, 1, 1, 1).BorderColor(frostui.ToneColor(t, frostui.Accent)).
		Role(ui.RoleGroup).Label("Pull request " + state.PullRef(p)).Children(func() {
		ui.Row(c).Gap(frostui.SpaceS).AlignItems(ui.Center).Children(func() {
			frostui.Icon(c, "insert-link").TextColor(frostui.ToneColor(t, frostui.Accent))
			frostui.Strong(c, state.PullRef(p)+" waits for your review").Grow(1).SingleLine()
			title := "Thread T" + itoa(int(p.Thread))
			th, ok := a.st.Data.Thread(p.Thread)
			if ok {
				title = "T" + itoa(int(th.ID)) + " · " + th.Title
			}
			frostui.Caption(c, title).SingleLine()
			if ok && th.Session != 0 {
				if ui.Button(c.Key(key+":session"), "Open Session").Clicked() {
					a.openSession(th.Session)
				}
			}
		})
		ui.Text(c, p.Title).Wrap().Selectable()
		if line := a.reviewLine(p); line != "" {
			frostui.Caption(c, line).Wrap()
		}
		ui.Row(c).Gap(frostui.SpaceS).AlignItems(ui.Center).Children(func() {
			frostui.Caption(c, p.Head+" → "+p.Base).SingleLine().Grow(1)
			if p.Ready {
				frostui.Pill(c, "ready-for-human", frostui.Success).Tooltip(readyTooltip(p))
			}
			a.openOnGitHub(c, key, p, true)
		})
		a.mergeRow(c, key, p)
	})
}
