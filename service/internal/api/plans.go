package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/frostyard/pocketgo/proto"
	"github.com/frostyard/pocketgo/service/internal/profiles"
	"github.com/frostyard/pocketgo/service/internal/router"
	"github.com/frostyard/pocketgo/service/internal/store"
	"github.com/frostyard/pocketgo/service/internal/team"
)

// newPlanThread records a plan and its plan thread for a team_delegate
// call with plan {start: true} (ADR-0019), under teamMu, in the
// transaction that counts the open plans (team.CheckPlanStart).
func (s *Server) newPlanThread(ctx context.Context, call TeamCall, key string, caller *profiles.Profile, lead profiles.Profile,
	models []proto.Model, d team.Delegation) (store.ThreadRow, error) {
	if err := ctx.Err(); err != nil {
		return store.ThreadRow{}, fmt.Errorf("the delegation was stopped: %w", err)
	}
	db := context.WithoutCancel(ctx)
	return s.changeThread(func() (store.ThreadRow, error) {
		row, _, _, err := s.store.CreatePlanThread(db, store.ThreadRow{Key: key, Title: team.Title(d), Agent: lead.ID,
			SupervisorSession: call.Session, Task: strings.TrimSpace(d.Task), TaskID: call.Task}, func(open int) error {
			return team.CheckPlanStart(caller, lead, func() error { return s.startable(lead, models) }, d, open)
		})
		return row, err
	})
}

// newTaskThread records the thread that delivers a plan's task for a
// team_delegate call with plan {id, task} (ADR-0019), under teamMu: the
// delegation's own rules (team.CheckDelegation, the desk's cap included),
// then the gate, in the transaction that opens the thread
// (team.CheckDelivery). The thread's task is the supervisor's text with
// the task as the owner approved it.
func (s *Server) newTaskThread(ctx context.Context, call TeamCall, key string, caller *profiles.Profile, target profiles.Profile,
	models []proto.Model, d team.Delegation) (store.ThreadRow, error) {
	id, task := d.Plan.ID, d.Plan.Task
	facts, err := s.store.PlanFacts(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return store.ThreadRow{}, fmt.Errorf("no plan P%d", id)
	} else if err != nil {
		return store.ThreadRow{}, err
	}
	starter, err := s.store.Session(ctx, facts.Plan.SupervisorSession)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return store.ThreadRow{}, err
	}
	if caller != nil && (err != nil || starter.Profile != caller.ID) {
		return store.ThreadRow{}, fmt.Errorf("plan P%d was started by another supervisor", id)
	}
	onDesk, err := s.store.OpenThreads(ctx)
	if err != nil {
		return store.ThreadRow{}, err
	}
	if err := team.CheckDelegation(caller, target, func() error { return s.startable(target, models) }, d, onDesk, s.deskCap); err != nil {
		return store.ThreadRow{}, err
	}
	text, title := strings.TrimSpace(d.Task), team.Title(d)
	if t := facts.Task(task); t != nil {
		text = team.PlanTaskText(d.Task, id, facts.Plan.Title, facts.Plan.Rev, t.Task)
		if strings.TrimSpace(d.Title) == "" {
			title = "P" + strconv.FormatInt(id, 10) + " " + t.ID + " · " + t.Title
		}
	}
	if err := ctx.Err(); err != nil {
		return store.ThreadRow{}, fmt.Errorf("the delegation was stopped: %w", err)
	}
	db := context.WithoutCancel(ctx)
	return s.changeThread(func() (store.ThreadRow, error) {
		row, _, err := s.store.CreateTaskThread(db, store.ThreadRow{Key: key, Title: title, Agent: target.ID, SupervisorSession: call.Session,
			Repository: d.Repository, Task: text, TaskID: call.Task}, id, task, func(f store.PlanFacts) error {
			return team.CheckDelivery(id, f.Plan.State, f.Gate(), f.Task(task), d)
		})
		return row, err
	})
}

// delegatedText is team_delegate's answer once the thread started.
func (s *Server) delegatedText(started store.ThreadRow) string {
	name := s.agentName(started.Agent)
	where := "in " + started.Cwd
	if started.Branch != "" {
		where += " on the branch " + started.Branch
	}
	switch {
	case started.IsPlanThread():
		return fmt.Sprintf("Started plan P%d, led by %s in thread T%d: %s. %s plans %s, in session %d. The owner approves the plan in their Inbox; pocketd then tells you its ready tasks.",
			started.Plan, name, started.ID, started.Title, name, where, started.Session)
	case started.Plan != 0:
		return fmt.Sprintf("Delivered task %s of plan P%d to %s as thread T%d: %s. %s works %s, in session %d. Reports come to you as messages that start with [T%d · %s · <status>].",
			started.PlanTask, started.Plan, name, started.ID, started.Title, name, where, started.Session, started.ID, name)
	}
	return fmt.Sprintf("Delegated to %s as thread T%d: %s. %s works %s, in session %d. Reports come to you as messages that start with [T%d · %s · <status>].",
		name, started.ID, started.Title, name, where, started.Session, started.ID, name)
}

// TeamPlan runs team_plan for session call.Session: the lead of a plan, in
// its plan thread, reads the plan, writes it, drafts its tasks, asks for
// the owner's approval, or consults other specialists (ADR-0019). Every
// change is checked in code and recorded with the call's key, in one
// transaction: the call run again answers as it did.
func (s *Server) TeamPlan(ctx context.Context, call TeamCall, c team.PlanCall) (string, error) {
	ctx = context.WithoutCancel(ctx)
	thread, err := s.store.ThreadOfSession(ctx, call.Session)
	if errors.Is(err, store.ErrNotFound) || err == nil && !thread.IsPlanThread() {
		return "", errors.New("team_plan is for a plan's lead, in its plan thread, and this session is none")
	} else if err != nil {
		return "", err
	}
	plan, key := thread.Plan, call.key()
	if c.Action != team.PlanRead {
		if result, found, err := s.store.PlanCallResult(ctx, key); err != nil {
			return "", err
		} else if found {
			s.deliverLater() // a consult call run again starts its consults
			return result, nil
		}
	}
	if err := team.CheckPlanCall(c); err != nil {
		return "", err
	}
	var change func() (store.PlanFacts, string, bool, error)
	switch c.Action {
	case team.PlanRead:
		facts, err := s.store.PlanFacts(ctx, plan)
		if err != nil {
			return "", err
		}
		return s.planText(facts), nil
	case team.PlanWrite:
		change = func() (store.PlanFacts, string, bool, error) {
			return s.store.WritePlan(ctx, key, plan, c.Text, c.Asks, func(f store.PlanFacts) error {
				if err := team.CheckDraft(plan, f.Plan.State, c.Action); err != nil {
					return err
				}
				if err := team.CheckWrite(plan, f.Plan.Rev, c.Rev, c.Text); err != nil {
					return err
				}
				for _, id := range c.Asks {
					switch a := f.Ask(id); {
					case a == nil:
						return fmt.Errorf("A%d is no ask of plan P%d", id, plan)
					case a.State == store.AskOpen:
						return fmt.Errorf("A%d waits for the owner's answer: write it in once it comes", id)
					}
				}
				return nil
			}, func(f store.PlanFacts) string {
				return fmt.Sprintf("Wrote plan P%d: it is at rev %s now, and %s.", plan, team.ShortRev(f.Plan.Rev), team.Waiting(f.Gate()))
			})
		}
	case team.PlanTasks:
		if s.profiles == nil {
			return "", errors.New("pocketd has no profiles, so no task has an agent")
		}
		tasks := team.CleanTasks(c.Tasks)
		if err := team.CheckGraph(tasks, s.profiles.Find); err != nil {
			return "", err
		}
		change = func() (store.PlanFacts, string, bool, error) {
			return s.store.DraftGraph(ctx, key, plan, tasks, func(f store.PlanFacts) error {
				if err := team.CheckDraft(plan, f.Plan.State, c.Action); err != nil {
					return err
				}
				if f.Plan.Rev == "" {
					return fmt.Errorf("plan P%d has no text: write it first with team_plan write", plan)
				}
				return nil
			}, func(f store.PlanFacts) string {
				return fmt.Sprintf("Drafted tasks version %d of plan P%d, for its rev %s: %d tasks.", f.Graph.Version, plan, team.ShortRev(f.Graph.Rev), len(f.Graph.Tasks))
			})
		}
	case team.PlanReady:
		change = func() (store.PlanFacts, string, bool, error) {
			return s.store.ReadyPlan(ctx, key, plan, func(f store.PlanFacts) error {
				return team.CheckReady(plan, f.Plan.State, f.Plan.Rev, f.DraftRev(), f.Gate())
			}, func(f store.PlanFacts) string {
				return fmt.Sprintf("Plan P%d waits for the owner's approval in their Inbox, at rev %s with tasks version %d. Any write, task draft or ask takes it back to drafting.",
					plan, team.ShortRev(f.Plan.Rev), f.Graph.Version)
			})
		}
	case team.PlanConsult:
		change, err = s.consultChange(ctx, key, plan, c)
		if err != nil {
			return "", err
		}
	}
	s.workMu.Lock()
	_, result, _, err := change()
	if err == nil {
		s.publishPlan(ctx, plan)
	}
	s.workMu.Unlock()
	if err != nil {
		return "", err
	}
	if c.Action == team.PlanConsult {
		// The consults start in a delivery pass, which a start that failed
		// for now tries again; their deadline wakes one, too.
		s.deliverLater()
	}
	return result, nil
}

// consultChange is the change of a team_plan consult call: its consults,
// checked in the transaction that records them (team.CheckDraft,
// team.CheckConsult).
func (s *Server) consultChange(ctx context.Context, key string, plan int64, c team.PlanCall) (func() (store.PlanFacts, string, bool, error), error) {
	if s.profiles == nil {
		return nil, errors.New("pocketd has no profiles, so no agent can be consulted")
	}
	models, err := s.engine.Models(ctx)
	if err != nil {
		return nil, err
	}
	rows := make([]store.ConsultRow, len(c.Agents))
	for i, agent := range c.Agents {
		rows[i] = store.ConsultRow{Key: key + ":" + strconv.Itoa(i), Agent: agent, Question: strings.TrimSpace(c.Question), AskID: c.Ask}
	}
	return func() (store.PlanFacts, string, bool, error) {
		return s.store.AddConsults(ctx, key, plan, rows, func(f store.PlanFacts) error {
			if err := team.CheckDraft(plan, f.Plan.State, c.Action); err != nil {
				return err
			}
			if c.Ask != 0 && f.Ask(c.Ask) == nil {
				return fmt.Errorf("A%d is no ask of plan P%d", c.Ask, plan)
			}
			running := 0
			for _, k := range f.Consults {
				if team.ConsultOpen(k.State) {
					running++
				}
			}
			return team.CheckConsult(f.Plan.Lead, c.Agents, s.profiles.Find, func(p profiles.Profile) error { return s.startable(p, models) },
				running, len(f.Consults))
		}, func(f store.PlanFacts) string {
			var ids []string
			for _, k := range f.Consults {
				if strings.HasPrefix(k.Key, key+":") {
					ids = append(ids, fmt.Sprintf("K%d (%s)", k.ID, k.Agent))
				}
			}
			return fmt.Sprintf("Consulting %s on plan P%d. Each finding comes to this session as a message; a consult with none %d minutes after this call ends.",
				strings.Join(ids, ", "), plan, int(team.ConsultLimit/time.Minute))
		})
	}, nil
}

// planText is team_plan read's answer: the plan's state and gate, its
// text, its graph and its consults.
func (s *Server) planText(f store.PlanFacts) string {
	var b strings.Builder
	p := f.Plan
	fmt.Fprintf(&b, "Plan P%d (%s), led by %s in thread T%d: %s; %s.\n", p.ID, p.Title, s.agentName(p.Lead), p.ThreadID, p.State, team.Waiting(f.Gate()))
	if p.Rev == "" {
		b.WriteString("It has no text yet: write it with team_plan write and no rev.\n")
	} else {
		fmt.Fprintf(&b, "rev: %s\n\n%s\n", p.Rev, p.Text)
	}
	if f.Graph != nil {
		state := "a draft"
		if f.Graph.State == store.GraphApproved {
			state = "approved"
		}
		fmt.Fprintf(&b, "\nTasks version %d (%s, for rev %s):\n", f.Graph.Version, state, team.ShortRev(f.Graph.Rev))
		for _, t := range f.Graph.Tasks {
			where := t.Agent + "'s home"
			if t.Repository != "" {
				where = t.Repository
			}
			deps := ""
			if len(t.Deps) > 0 {
				deps = ", after " + strings.Join(t.Deps, ", ")
			}
			fmt.Fprintf(&b, "- %s: %s, by %s in %s%s; %s\n", t.ID, t.Title, t.Agent, where, deps, t.State)
		}
	}
	if len(f.Asks) > 0 {
		b.WriteString("\nAsks:\n")
		for _, a := range f.Asks {
			switch {
			case a.State == store.AskOpen:
				fmt.Fprintf(&b, "- A%d %q: waits for the owner\n", a.ID, a.Question)
			case a.Written:
				fmt.Fprintf(&b, "- A%d %q: %s (written)\n", a.ID, a.Question, a.Answer)
			default:
				fmt.Fprintf(&b, "- A%d %q: %s (not written yet)\n", a.ID, a.Question, a.Answer)
			}
		}
	}
	if len(f.Consults) > 0 {
		b.WriteString("\nConsults:\n")
		for _, k := range f.Consults {
			fmt.Fprintf(&b, "- K%d, %s: %s", k.ID, k.Agent, k.State)
			switch {
			case k.State == team.ConsultAnswered:
				fmt.Fprintf(&b, ", %s: %s", k.Stance, k.Summary)
			case k.Why != "":
				fmt.Fprintf(&b, " (%s)", k.Why)
			}
			b.WriteString("\n")
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// TeamFinding runs team_finding for session call.Session: a consult's one
// finding, recorded once with the call's key, and noted for the plan's
// lead in the same transaction (ADR-0019).
func (s *Server) TeamFinding(ctx context.Context, call TeamCall, f team.Finding) (string, error) {
	ctx = context.WithoutCancel(ctx)
	consult, err := s.store.ConsultOfSession(ctx, call.Session)
	if errors.Is(err, store.ErrNotFound) {
		return "", errors.New("team_finding is for a consult's session, and this session is none")
	} else if err != nil {
		return "", err
	}
	if err := team.CheckFinding(f); err != nil {
		return "", err
	}
	name := s.agentName(consult.Agent)
	s.workMu.Lock()
	got, result, _, err := s.store.FileFinding(ctx, call.key(), call.Session, f, func(c store.ConsultRow, facts store.PlanFacts) error {
		switch {
		case c.State == team.ConsultAnswered:
			return fmt.Errorf("K%d filed its finding already: a consult answers once", c.ID)
		case c.State != team.ConsultRunning:
			return fmt.Errorf("K%d is %s, and takes no finding", c.ID, c.State)
		case facts.Plan.State != team.PlanDrafting && facts.Plan.State != team.PlanReview:
			return fmt.Errorf("plan P%d is %s, and takes no finding", facts.Plan.ID, facts.Plan.State)
		}
		return nil
	}, func(c store.ConsultRow) string {
		return team.FindingNote(c.ID, name, c.PlanID, c.AskID, c.Stance, c.Summary)
	}, fmt.Sprintf("Filed your finding as K%d: it goes to the plan's lead. Your work on plan P%d is done.", consult.ID, consult.PlanID))
	if err == nil {
		s.publishPlan(ctx, got.PlanID)
	}
	s.workMu.Unlock()
	if err != nil {
		return "", err
	}
	s.deliverLater()
	return result, nil
}

// startConsults starts each consult that waits to (startConsult), and ends
// each one past its deadline (expireConsults); done says none is left to
// start now.
func (s *Server) startConsults(ctx context.Context) (done bool, err error) {
	if err := s.expireConsults(ctx); err != nil {
		return false, err
	}
	open, err := s.store.OpenConsults(ctx)
	if err != nil {
		return false, err
	}
	var failures []error
	done = true
	for _, c := range open {
		if c.State != team.ConsultStarting {
			continue
		}
		started, err := s.startConsult(ctx, c.ID)
		if err != nil {
			failures = append(failures, fmt.Errorf("consult K%d: %w", c.ID, err))
		}
		done = done && started
	}
	s.armConsults(ctx)
	return done && len(failures) == 0, errors.Join(failures...)
}

// consultGone is why a consult's session cannot start: its expert no
// longer takes one. The consult fails, and its lead is told.
type consultGone struct{ error }

// startConsult starts consult id's session, from its expert's profile in
// its home, else its workspace, with read and team_finding alone, and
// files its question, in the transaction that records the session
// (store.RecordConsultSession). The session is keyed by the consult's
// random owner, so a start run again after a restart finds the one made.
// An expert that no longer takes a consult fails it. started says the
// consult no longer waits to start.
func (s *Server) startConsult(ctx context.Context, id int64) (bool, error) {
	c, err := s.store.Consult(ctx, id)
	if err != nil || c.State != team.ConsultStarting {
		return err == nil, err
	}
	key := "consult:" + c.Owner
	made, exists, err := s.engine.Created(ctx, key)
	if err != nil {
		return false, err
	}
	var spec SessionSpec
	if exists {
		spec = SessionSpec{Title: made.Title, Cwd: made.Cwd, Model: made.Model, Thinking: made.Thinking, Route: made.Route, Consult: c.ID}
		if made.Profile != "" && s.profiles != nil {
			if p, err := s.profiles.Find(made.Profile); err == nil {
				spec.Profile = &p
			}
		}
	} else if spec, err = s.consultSpec(ctx, c); err != nil {
		var gone consultGone
		if errors.As(err, &gone) {
			return true, s.endConsult(ctx, c, team.ConsultFailed, team.Clip("it could not start: "+gone.Error(), team.MaxFailure))
		}
		return false, err
	}
	facts, err := s.store.PlanFacts(ctx, c.PlanID)
	if err != nil {
		return false, err
	}
	askQuestion := ""
	if a := facts.Ask(c.AskID); a != nil {
		askQuestion = a.Question
	}
	question := team.ConsultQuestion(s.agentName(facts.Plan.Lead), c.ID, c.PlanID, facts.Plan.Title, c.Question, c.AskID, askQuestion)
	s.outboxMu.Lock()
	s.workMu.Lock()
	// The consult read again under the locks: one its plan's end stopped
	// meanwhile starts no session.
	if c, err = s.store.Consult(ctx, id); err != nil || c.State != team.ConsultStarting {
		s.workMu.Unlock()
		s.outboxMu.Unlock()
		return err == nil, err
	}
	session, err := s.engine.CreateSession(ctx, key, spec)
	if err != nil {
		s.workMu.Unlock()
		s.outboxMu.Unlock()
		return false, err
	}
	profile := ""
	if spec.Profile != nil {
		profile = spec.Profile.ID
	} else if exists {
		profile = made.Profile
	}
	title := spec.Title
	if title == "" {
		title = "Session " + strconv.FormatInt(session, 10)
	}
	recorded, err := s.store.RecordConsultSession(ctx, key, store.SessionRow{ID: session, Title: title, Cwd: spec.Cwd, Provider: spec.Model.Provider,
		Model: spec.Model.ID, Thinking: spec.Thinking, Profile: profile, Route: spec.Route, CreatedBy: teamCreator}, id, question)
	if err == nil {
		s.publishSession(session, true)
		s.publishPlan(ctx, recorded.PlanID)
	}
	s.workMu.Unlock()
	s.outboxMu.Unlock()
	if err != nil {
		return false, err
	}
	if profile != "" {
		s.lessonsSince(ctx, profile, spec.Lessons)
	}
	// The watch starts before the question goes, so its entry and the
	// expert's answer reach the event stream.
	if err := s.engine.Watch(ctx, session); err != nil {
		return true, fmt.Errorf("watch the consult's session: %w", err)
	}
	return true, nil
}

// consultSpec is what consult c's new session starts from: its expert's
// profile, checked again now, the router's model, and its home, else its
// workspace, a folder as the sandbox sees it. A rule that no longer holds
// is consultGone; a check that could not tell is another error.
func (s *Server) consultSpec(ctx context.Context, c store.ConsultRow) (SessionSpec, error) {
	if s.profiles == nil {
		return SessionSpec{}, consultGone{errors.New("pocketd has no profiles")}
	}
	p, err := s.profiles.Find(c.Agent)
	if err != nil {
		return SessionSpec{}, consultGone{err}
	}
	models, err := s.engine.Models(ctx)
	if err != nil {
		return SessionSpec{}, err
	}
	if err := team.CheckTarget(p, func() error { return s.startable(p, models) }); err != nil {
		return SessionSpec{}, consultGone{err}
	}
	cwd := p.Start()
	if cwd == "" {
		return SessionSpec{}, consultGone{fmt.Errorf("%s has no home or workspace to answer in", p.ID)}
	}
	check, cancel := s.withTimeout(ctx, checkTimeout)
	defer cancel()
	dir, err := s.engine.Dir(check, cwd)
	if err != nil {
		return SessionSpec{}, fmt.Errorf("look at %s: %w", cwd, err)
	}
	if !dir {
		return SessionSpec{}, consultGone{fmt.Errorf("%s is not a folder", cwd)}
	}
	choice, err := router.Choose(router.Request{Profile: &p}, models)
	if err != nil {
		return SessionSpec{}, consultGone{err}
	}
	lessons, err := s.store.ActiveLessons(ctx, p.ID)
	if err != nil {
		return SessionSpec{}, err
	}
	return SessionSpec{Title: fmt.Sprintf("K%d · consult on P%d", c.ID, c.PlanID), Cwd: cwd, Model: choice.Model, Profile: &p,
		Route: string(choice.Rule), Consult: c.ID, Lessons: sectionOf(lessons)}, nil
}

// endConsult ends consult c without a finding (store.EndConsult), and
// publishes its plan.
func (s *Server) endConsult(ctx context.Context, c store.ConsultRow, state, why string) error {
	text := team.ConsultEndNote(c.ID, s.agentName(c.Agent), c.PlanID, why)
	s.workMu.Lock()
	ended, did, err := s.store.EndConsult(ctx, c.ID, state, why, text)
	if err == nil && did {
		s.publishPlan(ctx, ended.PlanID)
	}
	s.workMu.Unlock()
	if did {
		s.deliverLater() // its note to the lead
	}
	return err
}

// expireConsults fails each consult with no finding past its deadline
// (team.ConsultLimit), by the server's clock.
func (s *Server) expireConsults(ctx context.Context) error {
	open, err := s.store.OpenConsults(ctx)
	if err != nil {
		return err
	}
	now := s.now()
	var failures []error
	for _, c := range open {
		if now.Before(c.Deadline()) {
			continue
		}
		why := fmt.Sprintf("no finding %d minutes after the call", int(team.ConsultLimit/time.Minute))
		if err := s.endConsult(ctx, c, team.ConsultFailed, why); err != nil {
			failures = append(failures, fmt.Errorf("consult K%d: %w", c.ID, err))
		}
	}
	return errors.Join(failures...)
}

// armConsults has a delivery pass run at the earliest deadline of a
// consult that is open, so it fails on time, without a pass every second.
func (s *Server) armConsults(ctx context.Context) {
	open, err := s.store.OpenConsults(ctx)
	if err != nil {
		s.log.Warn("read the open consults", "err", err)
		return
	}
	var next time.Time
	for _, c := range open {
		if next.IsZero() || c.Deadline().Before(next) {
			next = c.Deadline()
		}
	}
	s.sessionMu.Lock()
	defer s.sessionMu.Unlock()
	if s.consultCancel != nil {
		s.consultCancel()
		s.consultCancel = nil
	}
	if s.stopped || next.IsZero() {
		return
	}
	s.consultCancel = s.consultTimer(max(next.Sub(s.now()), 0)+time.Second, s.deliverLater)
}

// sendNotes sends session's notes, read now, oldest first, up to the first
// that has not reached it: a session never gets an older note after a
// newer one. A consult's question whose consult no longer runs is
// dropped. done says every one went.
func (s *Server) sendNotes(ctx context.Context, session int64) (done bool, err error) {
	for {
		placed, more, err := s.sendOldestNote(ctx, session)
		if err != nil || !placed {
			return false, err
		}
		if !more {
			return true, nil
		}
	}
}

// sendOldestNote sends session's oldest note, under outboxMu; more says
// one waited, and the next may.
func (s *Server) sendOldestNote(ctx context.Context, session int64) (placed, more bool, err error) {
	s.outboxMu.Lock()
	defer s.outboxMu.Unlock()
	row, ok, err := s.store.OldestNote(ctx, session)
	if err != nil || !ok {
		return err == nil, false, err
	}
	if row.ConsultID != 0 {
		c, err := s.store.Consult(ctx, row.ConsultID)
		if err != nil {
			return false, true, err
		}
		if c.State != team.ConsultRunning {
			return true, true, s.store.DropNote(ctx, row.Key)
		}
	}
	placed, err = s.place(ctx, outgoing{session: row.SessionID, key: row.Key, text: row.Text, submission: row.Submission, attempt: row.Attempt},
		func(sub int64) error { return s.store.NoteSubmitted(ctx, row.Key, sub) },
		func(attempt int) (bool, error) { return s.store.NoteWithdrawn(ctx, row.Key, attempt) })
	if err != nil || !placed {
		return false, true, err
	}
	return true, true, s.store.NoteSent(ctx, row.Key)
}

// deliverNotes sends every session's notes (sendNotes), so one session
// that takes no message holds back no other.
func (s *Server) deliverNotes(ctx context.Context) (done bool, err error) {
	sessions, err := s.store.SessionsWithNotes(ctx)
	if err != nil {
		return false, err
	}
	var failures []error
	done = true
	for _, session := range sessions {
		sent, err := s.sendNotes(ctx, session)
		if err != nil {
			failures = append(failures, fmt.Errorf("notes to session %d: %w", session, err))
		}
		done = done && sent
	}
	return done && len(failures) == 0, errors.Join(failures...)
}

// publishPlan publishes plan id as it is now, and its thread once the
// plan is done (a task thread's close closed it). workMu is held, so plan
// events leave in the order the plans changed.
func (s *Server) publishPlan(ctx context.Context, id int64) {
	facts, err := s.store.PlanFacts(context.WithoutCancel(ctx), id)
	if err != nil {
		s.log.Warn("read a plan to publish", "plan", id, "err", err)
		return
	}
	plan := wirePlan(facts)
	s.emit(proto.Event{Type: proto.EventPlan, Plan: &plan})
	if facts.Plan.State == team.PlanDone {
		thread := wireThread(facts.Thread)
		s.emit(proto.Event{Type: proto.EventThread, Thread: &thread})
	}
}

func wirePlan(f store.PlanFacts) proto.Plan {
	p := f.Plan
	out := proto.Plan{ID: p.ID, Thread: p.ThreadID, Supervisor: p.SupervisorSession, Lead: p.Lead, Title: p.Title, State: proto.PlanState(p.State),
		Text: p.Text, Rev: p.Rev, Waiting: f.Gate(), ApprovedBy: p.ApprovedBy, ApprovedAt: p.ApprovedAt, CreatedAt: p.CreatedAt, UpdatedAt: p.UpdatedAt}
	if f.Graph != nil {
		g := &proto.PlanGraph{Version: f.Graph.Version, Rev: f.Graph.Rev, Approved: f.Graph.State == store.GraphApproved, Tasks: []proto.PlanTask{}}
		for _, t := range f.Graph.Tasks {
			task := proto.PlanTask{ID: t.ID, Title: t.Title, Agent: t.Agent, Repository: t.Repository, Acceptance: slices.Clone(t.Acceptance),
				State: proto.TaskState(t.State), Thread: t.Thread}
			if len(t.Deps) > 0 {
				task.Deps = slices.Clone(t.Deps)
			}
			g.Tasks = append(g.Tasks, task)
		}
		out.Graph = g
	}
	for _, c := range f.Consults {
		out.Consults = append(out.Consults, proto.Consult{ID: c.ID, Agent: c.Agent, Question: c.Question, Ask: c.AskID, State: proto.ConsultState(c.State),
			Session: c.Session, Stance: c.Stance, Summary: c.Summary, Why: c.Why, CreatedAt: c.CreatedAt, EndedAt: c.EndedAt})
	}
	return out
}

// plans lists every plan, the most recently changed first.
func (s *Server) plans(w http.ResponseWriter, r *http.Request) {
	rows, err := s.store.Plans(r.Context())
	if err != nil {
		s.internal(w, "list plans", err)
		return
	}
	out := make([]proto.Plan, 0, len(rows))
	for _, row := range rows {
		out = append(out, wirePlan(row))
	}
	writeJSON(w, http.StatusOK, out)
}

// approvePlan approves a plan for the owner, once, at the revision and
// graph version they saw (team.CheckApprove), in the transaction that
// claims the request id (store.ApprovePlan). The supervisor's note of the
// ready tasks is filed with it, and sent.
func (s *Server) approvePlan(w http.ResponseWriter, r *http.Request) {
	id, ok := pathIDOf(w, r, "plan")
	if !ok {
		return
	}
	var request proto.ApprovePlan
	if !decode(w, r, &request) || !validRequestID(w, request.RequestID) {
		return
	}
	if len(request.Rev) != 64 || request.Graph <= 0 {
		writeError(w, http.StatusBadRequest, proto.ErrorCodeBadRequest, "an approval names the plan's rev and its graph's version, as seen")
		return
	}
	ctx := context.WithoutCancel(r.Context())
	key := requestKey(r, request.RequestID)
	defer s.keys.lock(key)()
	s.workMu.Lock()
	facts, replayed, err := s.store.ApprovePlan(ctx, key, id, deviceOf(r).Name, func(f store.PlanFacts) error {
		version := 0
		if f.Graph != nil && f.Graph.State == store.GraphDraft {
			version = f.Graph.Version
		}
		return team.CheckApprove(id, f.Plan.State, f.Plan.Rev, version, f.Gate(), request.Rev, request.Graph)
	})
	if err == nil && !replayed {
		s.publishPlan(ctx, id)
	}
	s.workMu.Unlock()
	switch {
	case errors.Is(err, store.ErrRequestReused):
		reused(w)
		return
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, proto.ErrorCodeNotFound, fmt.Sprintf("no plan %d", id))
		return
	case errors.Is(err, store.ErrConflict):
		writeError(w, http.StatusConflict, proto.ErrorCodeConflict, err.Error())
		return
	case err != nil:
		s.internal(w, "approve plan", err)
		return
	}
	// The note is filed; bringing it to the supervisor is the service's.
	if done, err := s.sendNotes(ctx, facts.Plan.SupervisorSession); err != nil || !done {
		if err != nil {
			s.log.Warn("send the plan's notes", "plan", id, "err", err)
		}
		s.deliverLater()
	}
	writeJSON(w, http.StatusOK, wirePlan(facts))
}
