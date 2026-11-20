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
// call with plan {start: true} (ADR-0020), under teamMu, in the
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
// team_delegate call with plan {id, task} (ADR-0020), under teamMu: the
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
	for again := true; ; again = false {
		onDesk, err := s.store.OpenThreads(ctx)
		if err != nil {
			return store.ThreadRow{}, err
		}
		if s.afterDeskCount != nil {
			s.afterDeskCount()
		}
		err = team.CheckDelegation(caller, target, func() error { return s.startable(target, models) }, d, onDesk, s.deskCap)
		if err == nil {
			break
		}
		if !errors.Is(err, team.ErrDeskFull) {
			return store.ThreadRow{}, err
		}
		// The next close on the desk tells the supervisor that a place is
		// free for this plan's ready tasks (store.freedPlace). The hold
		// counts the desk again: a close since the count freed a place,
		// which this delivery takes (Codex review of #25, round 4).
		held, herr := s.holdIfFull(context.WithoutCancel(ctx), id, s.deskCap, func(f store.PlanFacts) error {
			if err := team.CheckDelivery(id, f.Plan.State, f.Gate(), f.Task(task), d); err != nil {
				return ruleError{err}
			}
			return nil
		})
		switch {
		case herr != nil && errors.Is(herr, store.ErrNotFound):
			return store.ThreadRow{}, fmt.Errorf("no plan P%d", id)
		case herr != nil && isRule(herr):
			// The task would not go with a free place either: that is
			// the refusal (Codex review of #25, round 5).
			return store.ThreadRow{}, herr
		case herr != nil:
			// No hold was recorded, so no close would say a place is
			// free: the caller tries again, never waits for that.
			return store.ThreadRow{}, fmt.Errorf("the desk is full, and pocketd could not note this plan for the next free place: %w; try again", herr)
		case held || !again:
			return store.ThreadRow{}, err
		}
	}
	if s.afterPlanRead != nil {
		s.afterPlanRead()
	}
	if err := ctx.Err(); err != nil {
		return store.ThreadRow{}, fmt.Errorf("the delegation was stopped: %w", err)
	}
	db := context.WithoutCancel(ctx)
	return s.changeThread(func() (store.ThreadRow, error) {
		row, _, err := s.store.CreateTaskThread(db, store.ThreadRow{Key: key, Agent: target.ID, SupervisorSession: call.Session,
			Repository: d.Repository, TaskID: call.Task}, id, task, func(f store.PlanFacts) error {
			return team.CheckDelivery(id, f.Plan.State, f.Gate(), f.Task(task), d)
		}, func(f store.PlanFacts) (string, string) {
			// The brief is the task as approved, from the facts the gate
			// checked: never from an earlier read.
			t := f.Task(task)
			title := team.Title(d)
			if strings.TrimSpace(d.Title) == "" {
				title = "P" + strconv.FormatInt(id, 10) + " " + t.ID + " · " + t.Title
			}
			return title, team.PlanTaskText(d.Task, id, f.Plan.Title, f.Plan.Rev, f.Plan.Text, t.Task)
		})
		return row, err
	})
}

// readTaskPulls reads again on GitHub each pull request of thread id
// that has not merged, when the thread's close would settle a task of a
// live plan (store.threadClosed): a pull request merged since the last
// round makes the task done, never ready again (Codex review of #25,
// rounds 5 and 6). It reads and records each one's state alone: the
// ready-for-human label keeps to its own path. checked are the pull
// requests it read, nil when the close settles no task; an error, as one
// GitHub cannot answer, refuses the close, which is tried again.
func (s *Server) readTaskPulls(ctx context.Context, id int64) (map[int64]bool, error) {
	thread, err := s.store.Thread(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil // the close says so
	} else if err != nil {
		return nil, err
	}
	if thread.State == store.ThreadClosed || thread.Plan == 0 || thread.PlanTask == "" {
		return nil, nil
	}
	facts, err := s.store.PlanFacts(ctx, thread.Plan)
	if err != nil {
		return nil, err
	}
	task := facts.Task(thread.PlanTask)
	if team.PlanEnded(facts.Plan.State) || task == nil || task.State != team.TaskDelegated || task.Thread != id {
		return nil, nil // the close settles no task: an ended plan's, or an older delivery's
	}
	pulls, err := s.store.PullsOfThread(ctx, id)
	if err != nil {
		return nil, err
	}
	checked := map[int64]bool{}
	for _, p := range pulls {
		if p.State == store.PullMerged {
			continue
		}
		if err := s.readPullState(ctx, p); err != nil {
			return nil, fmt.Errorf("read %s on GitHub before T%d closes, so its task %s is settled as the pull request is: %w", p.Ref(), id, thread.PlanTask, err)
		}
		checked[p.ID] = true
	}
	return checked, nil
}

// readPullState reads pull request p on GitHub and records its state,
// under its pull lock, as a round's read does, without its label's sync.
func (s *Server) readPullState(ctx context.Context, p store.PullRow) error {
	unlock, err := s.lockPulls(ctx, []string{p.Ref()})
	if err != nil {
		return err
	}
	defer unlock()
	read, err := s.readPull(ctx, p.Repository, p.Number)
	if err != nil {
		return err
	}
	_, err = s.recordPull(ctx, p.ID, read)
	return err
}

// errPullsMoved is a task's close that found a pull request it had not
// read linked since: it reads again.
var errPullsMoved = errors.New("a pull request was linked to the thread while it closed")

// closeTaskThread closes thread id for request key with close
// (closeThread). A close that settles a plan task reads its pull requests
// first (readTaskPulls), and commits only while every one not merged that
// the thread links then, under workMu, was among those read: one linked
// meanwhile, by a report, has it read again (Codex review of #25, round
// 6). settled says the request committed already: nothing is read.
//
// ctx bounds the reads: an aborted call closes nothing; the close itself
// runs whatever ctx does once it began.
func (s *Server) closeTaskThread(ctx context.Context, id int64, key string, settled bool, close func() (store.ThreadRow, error)) (store.ThreadRow, error) {
	read, ctx := ctx, context.WithoutCancel(ctx)
	for try := 1; ; try++ {
		var checked map[int64]bool
		if !settled {
			var err error
			if checked, err = s.readTaskPulls(read, id); err != nil {
				return store.ThreadRow{}, err
			}
			if err := read.Err(); err != nil {
				return store.ThreadRow{}, fmt.Errorf("the close was stopped: %w", err)
			}
		}
		row, err := s.closeThread(ctx, id, key, read.Err, func() (store.ThreadRow, error) {
			if checked != nil {
				pulls, err := s.store.PullsOfThread(ctx, id)
				if err != nil {
					return store.ThreadRow{}, err
				}
				for _, p := range pulls {
					if p.State != store.PullMerged && !checked[p.ID] {
						return store.ThreadRow{}, errPullsMoved
					}
				}
			}
			return close()
		})
		if errors.Is(err, errPullsMoved) && try < 3 {
			continue
		}
		return row, err
	}
}

// isRule says whether err is a team rule's refusal rather than a
// failure: a store error is no rule.
func isRule(err error) bool {
	var rule ruleError
	return errors.As(err, &rule)
}

// ruleError marks a team rule's refusal, as check functions return it to
// the store.
type ruleError struct{ error }

func (r ruleError) Unwrap() error { return r.error }

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
// the owner's approval, or consults other specialists (ADR-0020). Every
// change is checked in code and recorded with the call's key, in one
// transaction: the call run again answers as it did.
func (s *Server) TeamPlan(ctx context.Context, call TeamCall, c team.PlanCall) (string, error) {
	live, ctx := ctx, context.WithoutCancel(ctx)
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
		agents, err := s.planAgents(ctx, facts.Plan.Lead)
		if err != nil {
			return "", err
		}
		// In pages, each whole in one tool result (team.Pages), all of one
		// snapshot of the read: a later page names it.
		whole := s.planText(facts) + "\n\n" + agents
		snapshot := team.Snapshot(whole)
		pages := team.Pages(whole)
		page := max(c.Page, 1)
		switch {
		case page > 1 && c.Snapshot != snapshot:
			return "", fmt.Errorf("plan P%d's read changed since its page 1 (snapshot %s): read again from page 1", plan, c.Snapshot)
		case page > len(pages):
			return "", fmt.Errorf("plan P%d's read has %d pages", plan, len(pages))
		}
		head := fmt.Sprintf("Page %d of %d of plan P%d's read, at rev %s, snapshot %s.\n\n", page, len(pages), plan, team.ShortRev(facts.Plan.Rev), snapshot)
		tail := ""
		if page < len(pages) {
			tail = fmt.Sprintf("\n\n(The read goes on: team_plan read with page %d and snapshot %s.)", page+1, snapshot)
		}
		return head + pages[page-1] + tail, nil
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
						return fmt.Errorf("ask A%d is no ask of plan P%d", id, plan)
					case a.State == store.AskOpen:
						return fmt.Errorf("ask A%d waits for the owner's answer: write it in once it comes", id)
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
	// An abort while the call waited for the lock records nothing new: a
	// call recorded already answers as it did (Codex review of #25, round
	// 8).
	if err := live.Err(); err != nil {
		result, found, ferr := s.store.PlanCallResult(ctx, key)
		s.workMu.Unlock()
		switch {
		case ferr != nil:
			return "", ferr
		case found:
			return result, nil
		}
		return "", fmt.Errorf("the call was stopped: %w", err)
	}
	facts, result, replayed, err := change()
	if err == nil && !replayed {
		// The facts the change's transaction read: no read after it can
		// lose the event (Codex review of #25, round 3).
		s.publishPlanFacts(facts)
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
				return fmt.Errorf("ask A%d is no ask of plan P%d", c.Ask, plan)
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

// planAgents names whom the plan's tasks can go to and its consults can
// ask (team.PlanAgents), from the profiles and the models as they are now.
func (s *Server) planAgents(ctx context.Context, lead string) (string, error) {
	if s.profiles == nil {
		return "pocketd has no profiles, so no agent takes a task.", nil
	}
	models, err := s.engine.Models(ctx)
	if err != nil {
		return "", err
	}
	valid, _, err := s.profiles.Load()
	if err != nil {
		return "", err
	}
	return team.PlanAgents(valid, func(p profiles.Profile) error { return s.startable(p, models) }, lead), nil
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
			// Each check, so a lead that drafts the graph again from a read
			// keeps them (Codex review of #25, round 1).
			for _, line := range t.Acceptance {
				fmt.Fprintf(&b, "  - acceptance: %s\n", line)
			}
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
			on := ""
			if k.AskID != 0 {
				on = fmt.Sprintf(", on A%d", k.AskID)
			}
			fmt.Fprintf(&b, "- K%d, %s%s, asked %q: %s", k.ID, k.Agent, on, k.Question, k.State)
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
// lead in the same transaction (ADR-0020).
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
			return fmt.Errorf("consult K%d filed its finding already: a consult answers once", c.ID)
		case c.State != team.ConsultRunning:
			return fmt.Errorf("consult K%d is %s, and takes no finding", c.ID, c.State)
		case !s.now().Before(c.Deadline()):
			// Past its deadline, a consult takes no finding, whether or
			// not a pass ended it yet.
			return fmt.Errorf("consult K%d had %d minutes, and they are over: it takes no finding", c.ID, int(team.ConsultLimit/time.Minute))
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
	// One consult that fails to end holds back no start of another (Codex
	// review of #25, round 2): its error joins the others.
	var failures []error
	if err := s.expireConsults(ctx); err != nil {
		failures = append(failures, err)
	}
	open, err := s.openConsults(ctx)
	if err != nil {
		return false, errors.Join(append(failures, err)...)
	}
	done = true
	now := s.now()
	for _, c := range open {
		if c.State != team.ConsultStarting || !now.Before(c.Deadline()) {
			continue // past its deadline, it ends rather than starts
		}
		started, err := s.startConsult(ctx, c.ID)
		if err != nil {
			failures = append(failures, fmt.Errorf("consult K%d: %w", c.ID, err))
		}
		done = done && started
	}
	// The deadlines of the consults this pass read: a consult that ended
	// meanwhile costs a pass at most, and no second read can fail and
	// leave a running consult without its timer (Codex review of #25,
	// round 3).
	s.armConsults(open)
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
// A new session starts from the profile as it is under the team's locks
// (recheckConsult); an expert that no longer takes a consult fails it.
// started says the consult no longer waits to start.
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
		// A session an earlier run made on a model that now serves
		// workers alone gets no question: a key would run it (ADR-0021),
		// as a thread's session gets no brief (startThread).
		if onKey, err := s.engine.APIKey(made.Model.Provider); err != nil {
			return false, err
		} else if onKey {
			return true, s.endConsult(ctx, c, team.ConsultFailed,
				team.Clip(fmt.Sprintf("it could not start: its session runs %s, which serves threads' workers alone", made.Model), team.MaxFailure))
		}
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
	if s.afterConsultSpec != nil {
		s.afterConsultSpec()
	}
	facts, err := s.store.PlanFacts(ctx, c.PlanID)
	if err != nil {
		return false, err
	}
	lead := s.agentName(facts.Plan.Lead)
	// The question quotes the plan as the transaction that files it reads
	// it: the expert has no other way to read the plan.
	question := func(f store.PlanFacts) string {
		askQuestion := ""
		if a := f.Ask(c.AskID); a != nil {
			askQuestion = a.Question
		}
		return team.ConsultQuestion(lead, c.ID, c.PlanID, f.Plan.Title, c.Question, c.AskID, askQuestion, f.Plan.Rev, f.Plan.Text)
	}
	// A session made by an earlier run keeps the profile it was made
	// from, as recorded, whatever the profile folder says now (Codex
	// review of #25, round 2).
	profile := ""
	if exists {
		profile = made.Profile
	}
	_, session, spec, err := s.createConsult(ctx, id, key, spec, !exists, profile, question)
	var gone consultGone
	switch {
	case errors.As(err, &gone):
		return true, s.endConsult(ctx, c, team.ConsultFailed, team.Clip("it could not start: "+gone.Error(), team.MaxFailure))
	case err != nil:
		return false, err
	case session == 0:
		return true, nil // it no longer starts: its plan ended meanwhile
	}
	if spec.Profile != nil {
		profile = spec.Profile.ID
	}
	if profile != "" {
		s.lessonsSince(ctx, profile, spec.Lessons)
	}
	// The watch starts before the question goes, so its entry and the
	// expert's answer reach the event stream; the question waits for a
	// watch that works (sendOldestNote).
	if err := s.engine.Watch(ctx, session); err != nil {
		return true, fmt.Errorf("watch the consult's session: %w", err)
	}
	return true, nil
}

// createConsult creates consult id's session from spec and records it with
// its question, under outboxMu and workMu, from a fresh read of the
// consult: one its plan's end stopped meanwhile starts no session (0). A
// new session (fresh) starts from its expert's profile as it is now
// (recheckConsult), and a rule that no longer holds is consultGone.
// profile is the profile a session made by an earlier run was made from.
func (s *Server) createConsult(ctx context.Context, id int64, key string, spec SessionSpec, fresh bool, profile string, question func(store.PlanFacts) string) (store.ConsultRow, int64, SessionSpec, error) {
	s.outboxMu.Lock()
	defer s.outboxMu.Unlock()
	s.workMu.Lock()
	defer s.workMu.Unlock()
	c, err := s.store.Consult(ctx, id)
	if err != nil || c.State != team.ConsultStarting {
		return c, 0, spec, err
	}
	if fresh {
		if spec, err = s.recheckConsult(ctx, c, spec); err != nil {
			return c, 0, spec, err
		}
		// Past its deadline, right before the session would start, a
		// consult starts none: its end comes at the next pass (Codex
		// review of #25, round 5).
		if !s.now().Before(c.Deadline()) {
			return c, 0, spec, nil
		}
	}
	session, err := s.engine.CreateSession(ctx, key, spec)
	if err != nil {
		return c, 0, spec, err
	}
	if spec.Profile != nil {
		profile = spec.Profile.ID
	}
	title := spec.Title
	if title == "" {
		title = "Session " + strconv.FormatInt(session, 10)
	}
	recorded, err := s.store.RecordConsultSession(ctx, key, store.SessionRow{ID: session, Title: title, Cwd: spec.Cwd, Provider: spec.Model.Provider,
		Model: spec.Model.ID, Thinking: spec.Thinking, Profile: profile, Route: spec.Route, CreatedBy: teamCreator}, id, question)
	if err != nil {
		return c, 0, spec, err
	}
	s.publishSession(session, true)
	s.publishPlan(ctx, recorded.PlanID)
	return recorded, session, spec, nil
}

// recheckConsult reads consult c's expert's profile again, right before
// its new session starts, under the team's locks: it must still take a
// consult (team.CheckTarget), in the folder spec was made for; the
// session starts from it, with its tools, the router's model and its
// lessons as they are now (Codex review of #25, round 1). A rule that no
// longer holds is consultGone.
func (s *Server) recheckConsult(ctx context.Context, c store.ConsultRow, spec SessionSpec) (SessionSpec, error) {
	if s.profiles == nil {
		return SessionSpec{}, consultGone{errors.New("pocketd has no profiles")}
	}
	// The slow check first: the folder as it is now, not as consultSpec
	// saw it, with a deadline of its own, since the team's locks are held
	// (Codex review of #25, rounds 2 and 3).
	check, cancel := s.withTimeout(ctx, checkTimeout)
	defer cancel()
	dir, err := s.engine.Dir(check, spec.Cwd)
	if err != nil {
		return SessionSpec{}, fmt.Errorf("look at %s: %w", spec.Cwd, err)
	}
	if !dir {
		return SessionSpec{}, consultGone{fmt.Errorf("%s is not a folder", spec.Cwd)}
	}
	// Then the lessons, the models and the profile, read last, right
	// before the session starts from them: nothing is read after.
	lessons, err := s.store.ActiveLessons(ctx, c.Agent)
	if err != nil {
		return SessionSpec{}, err
	}
	models, err := s.engine.Models(ctx)
	if err != nil {
		return SessionSpec{}, err
	}
	// A read that failed may work at the next pass: only a profile that is
	// gone or invalid fails the consult (Codex review of #25, round 8).
	p, err := s.profiles.Find(c.Agent)
	if profiles.Unreadable(err) {
		return SessionSpec{}, fmt.Errorf("read %s's profile: %w", c.Agent, err)
	} else if err != nil {
		return SessionSpec{}, consultGone{err}
	}
	if err := team.CheckTarget(p, func() error { return s.startable(p, models) }); err != nil {
		return SessionSpec{}, consultGone{err}
	}
	if p.Start() != spec.Cwd {
		return SessionSpec{}, consultGone{fmt.Errorf("%s's home changed while the consult started", p.ID)}
	}
	choice, err := router.Choose(router.Request{Profile: &p}, models)
	if err != nil {
		return SessionSpec{}, consultGone{err}
	}
	spec.Profile, spec.Model, spec.Route, spec.Lessons = &p, choice.Model, string(choice.Rule), sectionOf(lessons)
	return spec, nil
}

// consultSpec is what consult c's new session starts from: its expert's
// profile, checked again now, the router's model, and its home, else its
// workspace, a folder as the sandbox sees it. A rule that no longer holds
// is consultGone; a check that could not tell is another error.
func (s *Server) consultSpec(ctx context.Context, c store.ConsultRow) (SessionSpec, error) {
	if s.profiles == nil {
		return SessionSpec{}, consultGone{errors.New("pocketd has no profiles")}
	}
	// A read that failed may work at the next pass: only a profile that is
	// gone or invalid fails the consult (Codex review of #25, round 8).
	p, err := s.profiles.Find(c.Agent)
	if profiles.Unreadable(err) {
		return SessionSpec{}, fmt.Errorf("read %s's profile: %w", c.Agent, err)
	} else if err != nil {
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
// publishes its plan. Its question, still queued, is withdrawn first,
// under outboxMu, so the expert never spends a run on a consult that takes
// no finding; a consult whose withdrawal fails does not end now, and a
// later pass tries again.
func (s *Server) endConsult(ctx context.Context, c store.ConsultRow, state, why string) error {
	text := team.ConsultEndNote(c.ID, s.agentName(c.Agent), c.PlanID, why)
	s.outboxMu.Lock()
	defer s.outboxMu.Unlock()
	if _, err := s.withdrawQuestion(ctx, c); err != nil {
		return fmt.Errorf("withdraw the consult's queued question: %w", err)
	}
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

// withdrawQuestion withdraws every attempt of consult c's question that is
// still queued in its session (a no-op for one placed or gone), and says
// whether one was. outboxMu is held, so no pass places it meanwhile.
func (s *Server) withdrawQuestion(ctx context.Context, c store.ConsultRow) (bool, error) {
	note, err := s.store.Note(ctx, "consult:"+c.Owner+":question")
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	if note.SentAt != nil {
		return false, nil
	}
	changed := false
	for attempt := 0; attempt <= note.Attempt; attempt++ {
		withdrawn, err := s.engine.Withdraw(ctx, note.SessionID, attemptKey(note.Key, attempt))
		changed = changed || withdrawn
		if err != nil {
			return changed, err
		}
	}
	return changed, nil
}

// withdrawQuestions withdraws the queued questions of plan's open
// consults (withdrawQuestion), as closing its thread stops them.
func (s *Server) withdrawQuestions(ctx context.Context, plan int64) (bool, error) {
	facts, err := s.store.PlanFacts(ctx, plan)
	if err != nil {
		return false, err
	}
	changed := false
	for _, c := range facts.Consults {
		if !team.ConsultOpen(c.State) {
			continue
		}
		withdrawn, err := s.withdrawQuestion(ctx, c)
		changed = changed || withdrawn
		if err != nil {
			return changed, err
		}
	}
	return changed, nil
}

// questionWaits says whether session is a consult's whose question is not
// in its transcript yet, nor dropped: no other message goes before it.
func (s *Server) questionWaits(ctx context.Context, session int64) (bool, store.ConsultRow, error) {
	c, err := s.store.ConsultOfSession(ctx, session)
	if errors.Is(err, store.ErrNotFound) {
		return false, c, nil
	} else if err != nil {
		return false, c, err
	}
	note, err := s.store.Note(ctx, "consult:"+c.Owner+":question")
	if errors.Is(err, store.ErrNotFound) {
		return false, c, nil
	} else if err != nil {
		return false, c, err
	}
	return note.SentAt == nil && note.DroppedAt == nil, c, nil
}

// expireConsults fails each consult with no finding past its deadline
// (team.ConsultLimit), by the server's clock.
func (s *Server) expireConsults(ctx context.Context) error {
	open, err := s.openConsults(ctx)
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
func (s *Server) armConsults(open []store.ConsultRow) {
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
		// Past its deadline, a consult takes no finding: its question
		// waits for the consult's end, which drops it, and never starts a
		// run (Codex review of #25, round 2).
		if !s.now().Before(c.Deadline()) {
			return false, true, nil
		}
		// The question goes only to a watched session, so its entry and
		// the expert's answer reach the event stream; a watch that fails
		// holds it until one works.
		if err := s.engine.Watch(ctx, row.SessionID); err != nil {
			return false, true, fmt.Errorf("watch the consult's session: %w", err)
		}
	}
	var check func(context.Context) error
	if row.ConsultID != 0 {
		// The deadline again, right before a new submission: the watch and
		// the placement lookups may have outlasted it (Codex review of
		// #25, round 3).
		check = func(context.Context) error {
			c, err := s.store.Consult(ctx, row.ConsultID)
			if err != nil {
				return err
			}
			if c.State != team.ConsultRunning || !s.now().Before(c.Deadline()) {
				return fmt.Errorf("consult K%d takes no finding now: its question waits for its end", c.ID)
			}
			return nil
		}
	}
	placed, err = s.place(ctx, outgoing{session: row.SessionID, key: row.Key, text: row.Text, submission: row.Submission, attempt: row.Attempt, check: check},
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
//
// A read that fails leaves the plan to publish (planRetry): a delivery
// pass publishes it as it is then, so a change never goes without its
// event (Codex review of #25, round 3).
func (s *Server) publishPlan(ctx context.Context, id int64) {
	facts, err := s.planRead(context.WithoutCancel(ctx), id)
	if err != nil {
		s.log.Warn("read a plan to publish", "plan", id, "err", err)
		s.planRetry[id] = true
		s.deliverLater()
		return
	}
	delete(s.planRetry, id)
	s.publishPlanFacts(facts)
}

// republishPlans publishes each plan whose event a failed read held back
// (publishPlan), under workMu; done says none is left.
func (s *Server) republishPlans(ctx context.Context) (done bool) {
	s.workMu.Lock()
	defer s.workMu.Unlock()
	for id := range s.planRetry {
		if facts, err := s.planRead(ctx, id); err == nil {
			delete(s.planRetry, id)
			s.publishPlanFacts(facts)
		}
	}
	return len(s.planRetry) == 0
}

// publishPlanFacts publishes a plan as facts read it, and its thread once
// the plan is done. workMu is held.
func (s *Server) publishPlanFacts(facts store.PlanFacts) {
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
		s.publishPlanFacts(facts)
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
