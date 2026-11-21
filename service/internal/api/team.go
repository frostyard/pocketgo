package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/frostyard/pocketgo/proto"
	"github.com/frostyard/pocketgo/service/internal/profiles"
	"github.com/frostyard/pocketgo/service/internal/router"
	"github.com/frostyard/pocketgo/service/internal/store"
	"github.com/frostyard/pocketgo/service/internal/team"
	"github.com/frostyard/pocketgo/service/internal/worktree"
)

// TeamCall names one call of a team tool (ADR-0014): the calling session,
// the call's tool task and the call's id. Its key makes the call
// idempotent: a run of the same task after a restart finds what the first
// run did, while a later call that reuses the id, as a model may, is a
// call of its own.
type TeamCall struct {
	Session int64
	Task    int64
	CallID  string
}

func (c TeamCall) key() string {
	return "team:" + strconv.FormatInt(c.Session, 10) + ":" + strconv.FormatInt(c.Task, 10) + ":" + c.CallID
}

// teamCreator is the created_by of a session pocketd started for a thread.
const teamCreator = "team"

// SetDeskCap sets how many threads not closed the desk holds (ADR-0014).
func (s *Server) SetDeskCap(n int) { s.deskCap = n }

// callerProfile is the profile session id started from: nil for a session
// without one.
func (s *Server) callerProfile(ctx context.Context, id int64) (*profiles.Profile, error) {
	row, err := s.store.Session(ctx, id)
	if err != nil {
		return nil, err
	}
	if row.Profile == "" || s.profiles == nil {
		return nil, nil
	}
	p, err := s.profiles.Find(row.Profile)
	if err != nil {
		return nil, fmt.Errorf("this session's profile %s: %w", row.Profile, err)
	}
	return &p, nil
}

// startable says why no session can start from p now, nil when one can:
// its tools, a private home, or a model no sign-in serves.
func (s *Server) startable(p profiles.Profile, models []proto.Model) error {
	if err := s.checkTools(p); err != nil {
		return err
	}
	choice, err := router.Choose(router.Request{Profile: &p}, models)
	if err != nil {
		return err
	}
	// A standing profile's threshold is below its model's window
	// (ADR-0026), as GET /v1/profiles checks it.
	return profiles.CheckCompactAt(p, router.Window(models, choice.Model))
}

// roster is a supervisor's part of its session's instructions: whom it can
// delegate to now (team.Roster).
func (s *Server) roster(models []proto.Model) (string, error) {
	valid, _, err := s.profiles.Load()
	if err != nil {
		return "", err
	}
	problem := ""
	list, err := s.workerList()
	if err != nil {
		problem = err.Error()
	}
	return team.Roster(valid, func(p profiles.Profile) error { return s.startable(p, models) }, s.deskCap) + "\n\n" + team.WorkerRoster(list, problem), nil
}

// workerList is the owner's worker list (ADR-0022): the default one when
// pocketd has no profiles.
func (s *Server) workerList() (profiles.Workers, error) {
	if s.profiles == nil {
		return profiles.Workers{Models: profiles.DefaultWorkers, Default: true}, nil
	}
	return s.profiles.Workers()
}

// askedWorker resolves the worker model the supervisor names in d for the
// owner (ADR-0022): zero for none. A plan's lead builds nothing, so a plan's
// start takes none.
func (s *Server) askedWorker(d team.Delegation) (proto.ModelRef, error) {
	if d.Worker == "" {
		return proto.ModelRef{}, nil
	}
	if d.Plan != nil && d.Plan.Start {
		return proto.ModelRef{}, errors.New("a plan's lead has no worker: name worker when you deliver the plan's tasks")
	}
	list, err := s.workerList()
	if err != nil {
		return proto.ModelRef{}, fmt.Errorf("the owner's worker list cannot be read, so no worker model can be asked for: %w", err)
	}
	return router.AskedWorker(d.Worker, list)
}

// worktreeTimeout bounds the git that makes a thread's worktree: a hook or
// a filter of the checkout that stalls does not hold a place on the desk.
// checkTimeout bounds the git that checks it again under the team's locks,
// which every report, answer and close waits for.
const (
	worktreeTimeout = 5 * time.Minute
	checkTimeout    = 30 * time.Second
)

// changeThread runs one change of a thread and publishes the row it leaves,
// under workMu, so thread events leave in the order the rows changed.
func (s *Server) changeThread(change func() (store.ThreadRow, error)) (store.ThreadRow, error) {
	s.workMu.Lock()
	defer s.workMu.Unlock()
	row, err := change()
	if err == nil {
		s.publishThread(row)
	}
	return row, err
}

// TeamDelegate runs team_delegate for session call.Session: it checks the
// rules, records the thread, makes its folder, starts the specialist's
// session and sends it the brief, all in code. The text is what the calling
// agent sees; so is an error, a refusal or a failure. ctx is the call's: an
// abort stops the start before the specialist's session exists, or before
// its brief goes; the records are written whatever ctx does.
func (s *Server) TeamDelegate(ctx context.Context, call TeamCall, d team.Delegation) (string, error) {
	db := context.WithoutCancel(ctx)
	key := call.key()
	thread, err := s.store.ThreadByKey(db, key)
	if errors.Is(err, store.ErrNotFound) {
		thread, err = s.newThread(ctx, call, key, d)
	}
	if err != nil {
		return "", err
	}
	switch {
	case thread.State == store.ThreadClosed && thread.Session == 0:
		why := thread.Report
		if why == "" {
			why = "it was closed before it started"
		}
		return "", fmt.Errorf("thread T%d could not start: %s", thread.ID, why)
	case thread.State == store.ThreadClosed:
		return "", fmt.Errorf("thread T%d is closed", thread.ID)
	}
	started, err := s.startThread(ctx, thread)
	if err != nil {
		s.log.Warn("start thread", "thread", thread.ID, "err", err)
		if ctx.Err() != nil && s.stopping() {
			// pocketd stops, and PiG runs the call again when it starts:
			// the thread stays starting, to finish then.
			return "", fmt.Errorf("thread T%d starts again when pocketd does: %w", thread.ID, err)
		}
		why := team.Clip(err.Error(), team.MaxFailure)
		// A thread whose specialist may have its brief stays on the desk:
		// its reports move it on. Only one whose brief surely did not go
		// leaves it, with why.
		current, terr := s.store.Thread(db, thread.ID)
		may, merr := s.mayHaveBrief(db, current)
		if terr != nil || merr != nil || may {
			// A delivery pass follows its brief, or closes the thread once
			// the brief is known never to have gone (followThread).
			s.deliverLater()
			return "", fmt.Errorf("thread T%d may have started; its specialist's reports will say: %s", thread.ID, why)
		}
		if err := s.failThread(db, thread.ID, why); err != nil {
			// The next delivery pass closes it once this call's task
			// ended (followThread), after a restart too.
			s.log.Warn("close a thread that could not start", "thread", thread.ID, "err", err)
			s.deliverLater()
		}
		return "", fmt.Errorf("thread T%d could not start: %s", thread.ID, why)
	}
	return s.delegatedText(started), nil
}

// failThread closes a thread that could not start, with why.
func (s *Server) failThread(ctx context.Context, id int64, why string) error {
	_, err := s.changeThread(func() (store.ThreadRow, error) { return s.closeFailed(ctx, id, why) })
	if err == nil {
		// Its place on the desk frees: a review may wait for it (Codex
		// review of #26, round 10).
		s.reviewSoon()
	}
	return err
}

// mayHaveBrief says whether thread's specialist may have its brief: the
// thread has a session, and a brief submission was recorded, or the
// durable store has one of its first key. An error says the store could
// not tell: the caller neither sends nor closes until it can.
func (s *Server) mayHaveBrief(ctx context.Context, thread store.ThreadRow) (bool, error) {
	if thread.Session == 0 {
		return false, nil
	}
	if thread.BriefSubmission != 0 || thread.BriefAttempt > 0 || thread.Briefed {
		return true, nil
	}
	where, err := s.engine.Placed(ctx, thread.Session, briefKey(thread))
	if err != nil {
		return false, err
	}
	return where != PlacementAbsent, nil
}

// briefKey is the key of thread's brief in its specialist's session: by
// the thread's random owner, never its row number (AGENTS.md: a key that
// crosses the two databases).
func briefKey(thread store.ThreadRow) string { return "thread:" + thread.Owner + ":brief" }

// newThread checks a delegation against the rules and records its thread,
// under teamMu, so two delegations never both take the desk's last place.
// The checks take the call's context, so an abort stops them; the record
// is written whatever it does, once the call was not aborted before it.
func (s *Server) newThread(ctx context.Context, call TeamCall, key string, d team.Delegation) (store.ThreadRow, error) {
	if s.profiles == nil {
		return store.ThreadRow{}, errors.New("pocketd has no profiles, so no agent takes a delegation")
	}
	if d.Plan != nil {
		if err := team.CheckPlanRef(*d.Plan); err != nil {
			return store.ThreadRow{}, err
		}
	}
	asked, err := s.askedWorker(d)
	if err != nil {
		return store.ThreadRow{}, err
	}
	// A shift runs a thread in rounds (ADR-0024): a plan's thread runs
	// none.
	var shift *team.ShiftPlan
	if d.Shift != nil {
		if d.Plan != nil {
			return store.ThreadRow{}, errors.New("a plan's thread runs no shift: delegate the shift's work as its own thread")
		}
		checked, err := team.CheckShift(*d.Shift)
		if err != nil {
			return store.ThreadRow{}, err
		}
		shift = &checked
	}
	caller, err := s.callerProfile(ctx, call.Session)
	if err != nil {
		return store.ThreadRow{}, err
	}
	models, err := s.engine.Models(ctx)
	if err != nil {
		return store.ThreadRow{}, err
	}
	target, err := s.profiles.Find(d.Agent)
	if err != nil {
		roster, rerr := s.roster(models)
		if rerr != nil {
			return store.ThreadRow{}, err
		}
		return store.ThreadRow{}, fmt.Errorf("%w. %s", err, roster)
	}
	s.teamMu.Lock()
	defer s.teamMu.Unlock()
	switch {
	case d.Plan != nil && d.Plan.Start:
		return s.newPlanThread(ctx, call, key, caller, target, models, d)
	case d.Plan != nil:
		return s.newTaskThread(ctx, call, key, caller, target, models, d, asked)
	}
	onDesk, err := s.store.OpenThreads(ctx)
	if err != nil {
		return store.ThreadRow{}, err
	}
	if err := team.CheckDelegation(caller, target, func() error { return s.startable(target, models) }, d, onDesk, s.deskCap); err != nil {
		return store.ThreadRow{}, err
	}
	if err := ctx.Err(); err != nil {
		return store.ThreadRow{}, fmt.Errorf("the delegation was stopped: %w", err)
	}
	db := context.WithoutCancel(ctx)
	return s.changeThread(func() (store.ThreadRow, error) {
		row := store.ThreadRow{Key: key, Title: team.Title(d), Agent: target.ID,
			SupervisorSession: call.Session, Repository: d.Repository, Task: strings.TrimSpace(d.Task), TaskID: call.Task, WorkerAsked: asked}
		if shift != nil {
			created, _, err := s.store.CreateShiftThread(db, row, store.ShiftRow{Stop: shift.Stop, MaxRounds: shift.MaxRounds,
				Deadline: shift.Deadline(s.now()), CI: shift.CI})
			if err == nil {
				// A pass sets the shift's deadline wake, whatever its brief
				// does (Codex review of #36).
				s.shiftSoon()
			}
			return created, err
		}
		created, _, err := s.store.CreateThread(db, row)
		return created, err
	})
}

// startThread finishes a thread's start, each step idempotent, so a run
// after a restart picks up where the first stopped: the specialist's
// session (with its folder, made only for a new session), the session's
// attachment to the thread, the brief, then the thread's opening. A
// session made already is finished from what the engine recorded, without
// its profile. A new one starts only from a profile that still takes the
// delegation. A thread closed while it started gets no brief.
func (s *Server) startThread(ctx context.Context, thread store.ThreadRow) (store.ThreadRow, error) {
	db := context.WithoutCancel(ctx)
	// The conversation is keyed by the thread's random owner, never its
	// row number, which a work database restored from a backup gives out
	// again: only a run of the same delegation finds it.
	sessionKey := "thread:" + thread.Owner
	made, exists, err := s.engine.Created(db, sessionKey)
	if err != nil {
		return store.ThreadRow{}, err
	}
	var spec SessionSpec
	var profile, cwd, branch string
	if exists {
		spec = SessionSpec{Title: made.Title, Cwd: made.Cwd, Model: made.Model, Thinking: made.Thinking, Route: made.Route, Review: thread.ReviewPull != 0}
		profile, cwd = made.Profile, made.Cwd
		if thread.Repository != "" && thread.ReviewPull == 0 {
			_, branch = team.Worktree(made.Cwd, thread.Agent, team.Slug(thread.Title, thread.ID, thread.Owner))
		}
	} else {
		if spec, cwd, branch, err = s.newSpecialist(ctx, thread); err != nil {
			return store.ThreadRow{}, err
		}
		profile = spec.Profile.ID
		if err := ctx.Err(); err != nil {
			return store.ThreadRow{}, fmt.Errorf("the delegation was stopped: %w", err)
		}
	}
	if s.beforeAttach != nil {
		s.beforeAttach(thread.ID)
	}
	supervisor := "The supervisor"
	if p, err := s.callerProfile(db, thread.SupervisorSession); err == nil && p != nil {
		supervisor = p.Name
	}
	brief := team.Brief(supervisor, thread.ID, thread.Title, thread.Task, cwd, branch)
	if thread.IsPlanThread() {
		brief = team.PlanBrief(supervisor, thread.ID, thread.Plan, thread.Title, thread.Task, cwd)
	}
	// A shift's brief is its round 1, and says how its rounds go
	// (ADR-0024).
	if sh, err := s.store.Shift(db, thread.ID); err == nil {
		brief += "\n\n" + team.ShiftRules(team.ShiftPlan{Stop: sh.Stop, MaxRounds: sh.MaxRounds, CI: sh.CI}, sh.Deadline)
	} else if !errors.Is(err, store.ErrNotFound) {
		return store.ThreadRow{}, err
	}
	if thread.ReviewPull != 0 {
		// A review's brief is pocketd's, read from the worktree before the
		// locks (ADR-0019); one kept already wins, as a delegation's does.
		if brief, err = s.reviewBrief(ctx, thread, cwd); err != nil {
			return store.ThreadRow{}, err
		}
	}
	return s.attachBriefOpen(ctx, thread, sessionKey, spec, profile, cwd, branch, brief)
}

// attachBriefOpen starts the specialist's session, gives the thread its
// session and brief, sends the brief and opens the thread, all under
// outboxMu and workMu, from a fresh read of the thread: a close comes
// before, and no session starts and no brief goes, or after the opening,
// as the close of any open thread. A closed thread never gets a session or
// a brief, also in a run again. The brief is followed until it is in the
// specialist's transcript (placeBrief): a queue that holds it opens the
// thread, and an abort that withdraws it has it sent again.
func (s *Server) attachBriefOpen(ctx context.Context, thread store.ThreadRow, sessionKey string, spec SessionSpec, profile, cwd, branch, newBrief string) (store.ThreadRow, error) {
	db := context.WithoutCancel(ctx)
	s.outboxMu.Lock()
	defer s.outboxMu.Unlock()
	s.workMu.Lock()
	defer s.workMu.Unlock()
	current, err := s.store.Thread(db, thread.ID)
	if err != nil {
		return store.ThreadRow{}, err
	}
	if current.State == store.ThreadClosed {
		return store.ThreadRow{}, fmt.Errorf("thread T%d was closed while it started", thread.ID)
	}
	// A review's pull request is checked again as stored now, under
	// workMu: making its worktree and reading its base took time, and a
	// pull request closed, a draft or moved meanwhile starts no session;
	// the next pass ends the review (Codex review of #26, round 11). Once
	// the brief pinned its base, the stored merge base must be known and
	// the same: a read that failed meanwhile left it unknown, which backs
	// no brief, and the next pass reads it again (round 12).
	if current.ReviewPull != 0 {
		p, err := s.store.PullByID(db, current.ReviewPull)
		if err != nil {
			return store.ThreadRow{}, err
		}
		if p.State != store.PullOpen || p.Draft || current.Key != team.ReviewKey(p.Repository, p.Number, p.Base, p.HeadSHA) ||
			(current.ReviewBase != "" && p.MergeBase != "" && p.MergeBase != current.ReviewBase) {
			return store.ThreadRow{}, fmt.Errorf("%s moved while review thread T%d started: its review ends", p.Ref(), current.ID)
		}
		if current.ReviewBase != "" && p.MergeBase != current.ReviewBase {
			return store.ThreadRow{}, fmt.Errorf("%s's merge base is not known now: review thread T%d waits for a read that names it", p.Ref(), current.ID)
		}
	}
	// checked says the specialist's folder was checked as it is now, right
	// here: a brief sent below needs no check of its own.
	checked := true
	if spec.Profile == nil {
		// A session made by an earlier run, whose brief has not gone: its
		// folder is checked as it is now, as a new one's is, without its
		// profile: a worktree from the checkout its name was made from, a
		// home as a folder.
		may, err := s.mayHaveBrief(db, current)
		if err != nil {
			return store.ThreadRow{}, err
		}
		if may {
			// Its brief went, or may have: one sent again is checked
			// first (placeBrief).
			checked = false
		} else if err := s.recheckFolder(ctx, thread, cwd, branch); err != nil {
			return store.ThreadRow{}, err
		}
	}
	create := db
	if spec.Profile != nil {
		// A new session: the wait for the locks may have been long. An
		// abort meanwhile starts nothing, and the session starts from the
		// profile as it is now, checked again with the same folder.
		if err := ctx.Err(); err != nil {
			return store.ThreadRow{}, fmt.Errorf("the delegation was stopped: %w", err)
		}
		if spec, err = s.recheckSpecialist(ctx, thread, spec); err != nil {
			return store.ThreadRow{}, err
		}
		create = ctx
	}
	id, err := s.engine.CreateSession(create, sessionKey, spec)
	if err != nil {
		return store.ThreadRow{}, err
	}
	// A session made by an earlier run comes back with the tools it had:
	// before its brief, a review's has team_verdict and any other's has
	// not, a specialist's made before ADR-0019 included (Codex review of
	// #26, round 2).
	if _, err := s.engine.SetVerdictTool(db, id, thread.ReviewPull != 0); err != nil {
		return store.ThreadRow{}, fmt.Errorf("set the session's team_verdict: %w", err)
	}
	brief := newBrief
	if current.Brief != "" {
		brief = current.Brief // as an earlier run kept it
	}
	// The session and its thread's attachment commit together, before
	// either is published: the desktop never shows a specialist session
	// whose thread does not know it.
	title := spec.Title
	if title == "" {
		title = "Session " + strconv.FormatInt(id, 10)
	}
	attached, err := s.store.RecordThreadSession(db, sessionKey, store.SessionRow{
		ID: id, Title: title, Cwd: spec.Cwd, Provider: spec.Model.Provider, Model: spec.Model.ID,
		Thinking: spec.Thinking, Profile: profile, Route: spec.Route, CreatedBy: teamCreator,
	}, thread.ID, cwd, branch, brief)
	if err != nil {
		return store.ThreadRow{}, err
	}
	s.publishSession(id, true)
	s.publishThread(attached)
	if profile != "" {
		s.lessonsSince(ctx, profile, spec.Lessons)
	}
	// The watch starts before the brief goes, so the brief's entry and the
	// specialist's first reply reach the event stream. A thread whose
	// session cannot be watched gets no brief (and closes).
	if err := s.engine.Watch(db, id); err != nil {
		return store.ThreadRow{}, fmt.Errorf("watch the specialist's session: %w", err)
	}
	switch {
	case attached.Session != id || attached.State == store.ThreadClosed:
		return store.ThreadRow{}, fmt.Errorf("thread T%d was closed while it started", thread.ID)
	case attached.State == store.ThreadStarting && ctx.Err() != nil:
		return store.ThreadRow{}, fmt.Errorf("the delegation was stopped: %w", ctx.Err())
	}
	// The brief's first submission takes the call's context: a
	// delegation aborted meanwhile sends none.
	placed, err := s.placeBrief(db, attached, ctx, checked)
	var expired shiftExpired
	if errors.As(err, &expired) {
		if _, closeErr := s.expireBrief(db, thread.ID, expired); closeErr != nil {
			return store.ThreadRow{}, errors.Join(fmt.Errorf("send the brief: %w", err), closeErr)
		}
		// Its place on the desk is free (Codex review of #36).
		s.reviewSoon()
		return store.ThreadRow{}, fmt.Errorf("thread T%d closed: %w", thread.ID, err)
	}
	if err != nil {
		return store.ThreadRow{}, fmt.Errorf("send the brief: %w", err)
	}
	if !placed {
		s.deliverLater()
	}
	// The specialist has its brief and works: a thread that fails to
	// open stays starting, never closes, and a delivery pass opens it
	// (followThread), or its first report moves it on.
	opened, err := s.openThread(db, thread.ID)
	if err != nil {
		s.log.Warn("open thread", "thread", thread.ID, "err", err)
		s.deliverLater()
		return attached, nil
	}
	s.publishThread(opened)
	return opened, nil
}

// closeThread closes thread id with close (the owner's or a
// supervisor's), under outboxMu and workMu. A brief still queued is
// withdrawn first, so the specialist of a closed thread never starts its
// work; a close whose withdrawal fails does not happen, and the caller
// tries again. A brief already in the transcript stays. Until the close
// commits the thread is open, and a delivery pass may send its brief
// again; under the locks none does between the two steps.
func (s *Server) closeThread(ctx context.Context, id int64, key string, admit func() error, close func() (store.ThreadRow, error)) (store.ThreadRow, error) {
	if s.beforeCloseLocks != nil {
		s.beforeCloseLocks()
	}
	row, err := s.closeThreadLocked(ctx, id, key, admit, close)
	if err == nil && row.State == store.ThreadClosed {
		// Its worker stops, outside the team's locks: PiG's abort waits
		// for the run (ADR-0021).
		if row.WorkerSession != 0 {
			s.stopWorker(ctx, row.WorkerSession)
		}
		// A place on the desk freed: the review pass starts what waited
		// for one, after the team's locks (Codex review of #26, round 9).
		// A review the owner closed after its brief went has a run that
		// goes on: the pass, which scans only open reviews, stops it as
		// endReview stops one, and tries again until it can (round 12).
		if row.ReviewPull != 0 && row.Session != 0 {
			s.stopReviewSoon(row.Session)
		} else {
			s.reviewSoon()
		}
	}
	return row, err
}

// closeThreadLocked is closeThread's close, under outboxMu and workMu.
// admit, when set, says whether a close whose request id is not claimed
// yet may still begin: a close stopped while it waited for the locks
// does nothing (Codex review of #25, round 8).
func (s *Server) closeThreadLocked(ctx context.Context, id int64, key string, admit func() error, close func() (store.ThreadRow, error)) (store.ThreadRow, error) {
	s.outboxMu.Lock()
	defer s.outboxMu.Unlock()
	s.workMu.Lock()
	defer s.workMu.Unlock()
	// A request id used for another request is refused before anything
	// else, also for a thread that does not exist (protocol rule 5); a
	// fresh one stays unclaimed when the thread does not exist.
	kind := "closeThread:" + strconv.FormatInt(id, 10)
	_, claimed, err := s.store.LookupRequest(ctx, key, kind)
	if err != nil {
		return store.ThreadRow{}, err
	}
	// Before its first change: a close reserved already finishes whatever
	// its call does now.
	if !claimed && admit != nil {
		if err := admit(); err != nil {
			return store.ThreadRow{}, fmt.Errorf("the close was stopped: %w", err)
		}
	}
	row, err := s.store.Thread(ctx, id)
	if err != nil {
		return store.ThreadRow{}, err
	}
	// The close's request key is reserved first, atomically: one another
	// request holds is refused here, and the queue is untouched. A
	// reservation this close made and whose close fails with nothing done
	// is released, so the id is free again (protocol rule 5); one an
	// earlier try left unreleased is this close's own.
	s.releaseMu.Lock()
	_, pending := s.unreleased[key]
	delete(s.unreleased, key)
	s.releaseMu.Unlock()
	_, existed, err := s.store.ClaimRequest(ctx, key, kind)
	if err != nil {
		// A reservation still to free stays to free (Codex review of #36).
		if pending {
			s.releaseMu.Lock()
			s.unreleased[key] = kind
			s.releaseMu.Unlock()
		}
		return store.ThreadRow{}, err
	}
	reserved := existed && !pending
	// changed says whether a withdrawal took effect: then the close has
	// done something, and keeps its id for its retry.
	changed := false
	release := func() error {
		if reserved || changed {
			return nil
		}
		if err := s.releaseRequest(ctx, key, kind); err != nil {
			// A delivery pass frees it, and never one completed since.
			s.releaseMu.Lock()
			s.unreleased[key] = kind
			s.releaseMu.Unlock()
			s.deliverLater()
			return fmt.Errorf("%w: %w", errRequestKept, err)
		}
		return nil
	}
	// Every attempt of the brief the work database knows is withdrawn
	// while queued, Briefed or not: PiG's queue, not the work row, says
	// whether one waits (a no-op for one placed or gone).
	if row.State != store.ThreadClosed && row.Session != 0 && row.Brief != "" {
		for attempt := 0; attempt <= row.BriefAttempt; attempt++ {
			withdrawn, err := s.engine.Withdraw(ctx, row.Session, attemptKey(briefKey(row), attempt))
			changed = changed || withdrawn
			if err != nil {
				return store.ThreadRow{}, errors.Join(fmt.Errorf("withdraw the queued brief: %w", err), release())
			}
		}
	}
	// A plan's own thread stops its plan's consults as it closes: their
	// questions still queued are withdrawn first, as the brief is.
	if row.State != store.ThreadClosed && row.IsPlanThread() {
		withdrawn, err := s.withdrawQuestions(ctx, row.Plan)
		changed = changed || withdrawn
		if err != nil {
			return store.ThreadRow{}, errors.Join(fmt.Errorf("withdraw the consults' queued questions: %w", err), release())
		}
	}
	// Its worker's replies still queued are withdrawn before the close
	// commits, as its brief is (ADR-0021; Codex review of #29, round 2).
	if row.State != store.ThreadClosed {
		withdrew, err := s.withdrawWorkerReplies(ctx, row.ID)
		changed = changed || withdrew
		if err != nil {
			return store.ThreadRow{}, errors.Join(err, release())
		}
	}
	// The supervisor's messages the close must settle first (ADR-0032):
	// those placed are noted, so the close reads the state they leave, and
	// a plan lead's the close may end are withdrawn.
	lead := int64(0)
	if row.State != store.ThreadClosed {
		var withdrew bool
		lead, withdrew, err = s.sendsBeforeClose(ctx, row)
		changed = changed || withdrew
		if err != nil {
			s.sendsAfterClose(ctx, lead)
			return store.ThreadRow{}, errors.Join(err, release())
		}
	}
	closed, err := close()
	if err != nil {
		s.sendsAfterClose(ctx, lead)
		return store.ThreadRow{}, errors.Join(err, release())
	}
	s.publishThread(closed)
	// They go to no one; one this cannot note now, the delivery pass
	// drops (sendReport).
	if err := s.dropWorkerReplies(ctx, closed.ID); err != nil {
		s.log.Warn("drop a closed thread's worker replies", "thread", closed.ID, "err", err)
		s.deliverLater()
	}
	s.sendsAfterClose(ctx, lead)
	return closed, nil
}

// errRequestKept is a failed close whose request id could not be freed:
// the service frees it later (releasePending), and the client says so.
var errRequestKept = errors.New("free the request id")

// stopping says whether the server is stopping (Stop).
func (s *Server) stopping() bool {
	s.sessionMu.Lock()
	defer s.sessionMu.Unlock()
	return s.stopped
}

// newSpecialist is what a thread's new specialist session starts from:
// its profile, checked again now (team.CheckTarget, team.CheckDestination),
// the router's model, and its folder, made now.
func (s *Server) newSpecialist(ctx context.Context, thread store.ThreadRow) (SessionSpec, string, string, error) {
	if s.profiles == nil {
		return SessionSpec{}, "", "", errors.New("pocketd has no profiles, so the specialist cannot start")
	}
	target, err := s.profiles.Find(thread.Agent)
	if err != nil {
		return SessionSpec{}, "", "", err
	}
	models, err := s.engine.Models(ctx)
	if err != nil {
		return SessionSpec{}, "", "", err
	}
	if thread.ReviewPull != 0 {
		// A cross-review pocketd started (ADR-0019).
		spec, cwd, err := s.reviewSpecialist(ctx, thread, target, models)
		return spec, cwd, "", err
	}
	if err := team.CheckTarget(target, func() error { return s.startable(target, models) }); err != nil {
		return SessionSpec{}, "", "", err
	}
	if err := team.CheckDestination(target, thread.Repository); err != nil {
		return SessionSpec{}, "", "", err
	}
	cwd, branch, err := s.threadFolder(ctx, thread, target)
	if err != nil {
		return SessionSpec{}, "", "", err
	}
	if s.afterFolder != nil {
		s.afterFolder()
	}
	choice, err := router.Choose(router.Request{Profile: &target}, models)
	if err != nil {
		return SessionSpec{}, "", "", err
	}
	spec := SessionSpec{Title: "T" + strconv.FormatInt(thread.ID, 10) + " · " + thread.Title, Cwd: cwd, Model: choice.Model,
		Profile: &target, Route: string(choice.Rule), Thread: thread.ID}
	if thread.IsPlanThread() {
		spec.Plan = thread.Plan
	}
	return spec, cwd, branch, nil
}

// recheckSpecialist reads a new specialist's profile again, at the moment
// its session starts: it must still take the delegation (team.CheckTarget,
// team.CheckDestination), with the folder spec was made for, its checkout
// or its home; the session starts from it, with the router's model now.
func (s *Server) recheckSpecialist(ctx context.Context, thread store.ThreadRow, spec SessionSpec) (SessionSpec, error) {
	if s.profiles == nil {
		return SessionSpec{}, errors.New("pocketd has no profiles, so the specialist cannot start")
	}
	// The slow checks first: the worktree as it is now, not as it was
	// made (of its repository, on its branch, not locked), with a git
	// deadline of its own, since the team's locks are held.
	if thread.Repository != "" {
		target, err := s.profiles.Find(thread.Agent)
		if err != nil {
			return SessionSpec{}, err
		}
		_, checkout := threadCheckout(target, thread)
		path, branch := team.Worktree(checkout, target.ID, team.Slug(thread.Title, thread.ID, thread.Owner))
		if checkout == "" || path != spec.Cwd {
			return SessionSpec{}, fmt.Errorf("%s's checkout of %s changed while its worktree was made", target.ID, thread.Repository)
		}
		check, cancel := s.withTimeout(ctx, checkTimeout)
		defer cancel()
		if thread.ReviewPull != 0 {
			// Detached at the head it reviews (ADR-0019).
			if err := s.reviewGit.usable(check, checkout, path, thread.ReviewHead); err != nil {
				return SessionSpec{}, err
			}
		} else if err := worktree.Usable(check, s.worktreeTools(), checkout, path, branch); err != nil {
			return SessionSpec{}, err
		}
	} else if err := s.isFolder(ctx, spec.Cwd); err != nil {
		return SessionSpec{}, err
	}
	// The lessons in force, before the last checks: the section is written
	// from the profile that passes them, and nothing is read after.
	lessons, err := s.store.ActiveLessons(ctx, thread.Agent)
	if err != nil {
		return SessionSpec{}, err
	}
	// Then the models and the profile, read last, right before the session
	// starts from them: its rules, its folder as checked, its model on a
	// sign-in that is there now.
	models, err := s.engine.Models(ctx)
	if err != nil {
		return SessionSpec{}, err
	}
	target, err := s.profiles.Find(thread.Agent)
	if err != nil {
		return SessionSpec{}, err
	}
	startable := func() error { return s.startable(target, models) }
	if thread.ReviewPull != 0 {
		// The profile's own model does not run in a review, but its tools
		// are checked (ADR-0019; Codex review of #26, round 2).
		startable = func() error { return s.checkTools(target) }
	}
	if err := team.CheckTarget(target, startable); err != nil {
		return SessionSpec{}, err
	}
	repository, checkout := threadCheckout(target, thread)
	if err := team.CheckDestination(target, repository); err != nil {
		return SessionSpec{}, err
	}
	if thread.Repository != "" {
		if path, _ := team.Worktree(checkout, target.ID, team.Slug(thread.Title, thread.ID, thread.Owner)); path != spec.Cwd {
			return SessionSpec{}, fmt.Errorf("%s's checkout of %s changed while its worktree was checked", target.ID, thread.Repository)
		}
	} else if target.Start() != spec.Cwd {
		return SessionSpec{}, fmt.Errorf("%s's home changed while the thread started", target.ID)
	}
	if s.afterRecheck != nil {
		s.afterRecheck()
	}
	choice, err := router.Choose(router.Request{Profile: &target}, models)
	if thread.ReviewPull != 0 {
		choice, err = s.reviewChoice(ctx, thread, target, models)
	}
	if err != nil {
		return SessionSpec{}, err
	}
	spec.Profile, spec.Model, spec.Route = &target, choice.Model, string(choice.Rule)
	spec.Review = thread.ReviewPull != 0
	spec.Lessons = sectionOf(lessons)
	return spec, nil
}

// worktreeTools are the engine's sandboxed git and existence check.
func (s *Server) worktreeTools() worktree.Tools {
	return worktree.Tools{Git: s.engine.Git, Kind: s.engine.Kind, Read: s.engine.Read, Same: s.engine.Same, Each: s.engine.GitEach, MakeDir: s.engine.MakeDir}
}

// threadFolder is where a thread works: a new worktree of its repository,
// owned by the thread's random owner, or the specialist's home, else its
// workspace.
// The git that makes the worktree ends with ctx, or after worktreeTimeout.
func (s *Server) threadFolder(ctx context.Context, thread store.ThreadRow, target profiles.Profile) (cwd, branch string, err error) {
	if thread.Repository != "" {
		checkout := target.Repositories[thread.Repository]
		cwd, branch = team.Worktree(checkout, target.ID, team.Slug(thread.Title, thread.ID, thread.Owner))
		gitCtx, cancel := s.withTimeout(ctx, worktreeTimeout)
		defer cancel()
		if err := worktree.Add(gitCtx, s.worktreeTools(), checkout, cwd, branch); err != nil {
			return "", "", fmt.Errorf("make the worktree: %w", err)
		}
		return cwd, branch, nil // worktree.Add checked it, in the sandbox
	}
	cwd = target.Start()
	if err := s.isFolder(ctx, cwd); err != nil {
		return "", "", err
	}
	return cwd, branch, nil
}

// isFolder refuses a home that is not a folder as a command in the sandbox
// sees it (a link to one counts), bound by checkTimeout: the specialist
// works there, and the check may run under the team's locks.
func (s *Server) isFolder(ctx context.Context, path string) error {
	check, cancel := s.withTimeout(ctx, checkTimeout)
	defer cancel()
	dir, err := s.engine.Dir(check, path)
	if err != nil {
		return fmt.Errorf("look at %s: %w", path, err)
	}
	if !dir {
		return fmt.Errorf("%s is not a folder", path)
	}
	return nil
}

// folderChanged is a specialist's folder that is no longer as its thread
// names it (not its worktree, or not a folder): a brief never goes there.
// A check that could not tell is any other error, and is tried again.
type folderChanged struct{ error }

func (f folderChanged) Unwrap() error { return f.error }

// recheckFolder checks thread's folder cwd (on branch, for a worktree) as
// it is now, as a new one's is, without the thread's profile: a worktree
// from the checkout its name was made from, a home as a folder.
func (s *Server) recheckFolder(ctx context.Context, thread store.ThreadRow, cwd, branch string) error {
	check, cancel := s.withTimeout(ctx, checkTimeout)
	defer cancel()
	if thread.Repository == "" {
		dir, err := s.engine.Dir(check, cwd)
		if err != nil {
			return fmt.Errorf("look at %s: %w", cwd, err)
		}
		if !dir {
			return folderChanged{fmt.Errorf("%s is not a folder", cwd)}
		}
		return nil
	}
	suffix := "--" + thread.Agent + "-" + team.Slug(thread.Title, thread.ID, thread.Owner)
	if !strings.HasSuffix(cwd, suffix) {
		return folderChanged{fmt.Errorf("%s is not thread T%d's worktree", cwd, thread.ID)}
	}
	usable := func() error {
		return worktree.Usable(check, s.worktreeTools(), strings.TrimSuffix(cwd, suffix), cwd, branch)
	}
	if thread.ReviewPull != 0 {
		// A review's worktree is detached at its head, on no branch: its
		// recovery and a brief sent again check it so (Codex review of #26,
		// round 1).
		usable = func() error {
			return s.reviewGit.usable(check, strings.TrimSuffix(cwd, suffix), cwd, thread.ReviewHead)
		}
	}
	if err := usable(); err != nil {
		if worktree.Unsure(err) {
			return err
		}
		return folderChanged{err}
	}
	return nil
}

// agentName is a profile's display name, else its id.
func (s *Server) agentName(id string) string {
	if s.profiles != nil {
		if p, err := s.profiles.Find(id); err == nil && p.Name != "" {
			return p.Name
		}
	}
	return id
}

// TeamMessage runs team_message for session call.Session: it files the
// specialist's report on its thread with its asks, in one transaction and
// once per call (store.FileReport), and sends the report to the
// supervisor's session.
func (s *Server) TeamMessage(ctx context.Context, call TeamCall, r team.Report) (string, error) {
	// The call's own context bounds the GitHub reads before the report is
	// filed: an abort stops them, and files nothing. What follows them is
	// done whatever the call does.
	callCtx := ctx
	ctx = context.WithoutCancel(ctx)
	thread, err := s.store.ThreadOfSession(ctx, call.Session)
	if errors.Is(err, store.ErrNotFound) {
		return "", errors.New("team_message is for a thread's specialist, and this session works in no thread")
	} else if err != nil {
		return "", err
	}
	// A cross-review files its verdict and reports nothing else, whatever
	// tool list it has (ADR-0019; Codex review of #26, round 1).
	if thread.ReviewPull != 0 {
		return "", errors.New("a cross-review reports only with team_verdict")
	}
	if err := team.CheckReport(r); err != nil {
		return "", err
	}
	if thread.IsPlanThread() && len(r.PullRequests) > 0 {
		return "", fmt.Errorf("thread T%d plans P%d, and links no pull request: a plan's tasks open them, in threads of their own", thread.ID, thread.Plan)
	}
	if err := team.CheckPullRefs(r.PullRequests, thread.Repository); err != nil {
		return "", err
	}
	key := call.key()
	// A shift's specialist reports done or blocked only in its summary
	// round, when pocketd asks, with the status its stop asks for: one
	// report, never twinned (ADR-0024). Checked here before GitHub is
	// read, and again in the transaction that files it, which ends the
	// shift with its summary's report (store.FileReportWithPulls).
	var seen store.ShiftSeen
	if shift, err := s.store.Shift(ctx, thread.ID); err == nil {
		// The calling run, by its first entry: the one that files the
		// summary's report files no second final one (Codex review of
		// #36).
		run, _, err := s.engine.RunIdentity(ctx, call.Session)
		if err != nil {
			return "", err
		}
		seen = store.ShiftSeen{State: shift.State, Round: shift.Round, Run: run}
		if filed, err := s.store.ReportFiled(ctx, key); err != nil {
			return "", err
		} else if !filed {
			if err := team.CheckShiftReport(thread.ID, shift.State, shift.Reason, shift.Round, shift.MaxRounds, r.Status); err != nil {
				return "", err
			}
			// The summary's report answers the summary round's message,
			// once it is in the transcript: one filed before it, as by a
			// run the owner's message began, would leave it to ask for a
			// second (Codex review of #36).
			if final := r.Status == team.StatusDone || r.Status == team.StatusBlocked; final && shift.State == team.ShiftSummary {
				if _, placed, err := s.roundAnchor(ctx, thread, shift); err != nil {
					return "", err
				} else if !placed {
					return "", fmt.Errorf("thread T%d's shift asks for its report in the summary round's message, which has not reached you yet: end your turn, and report once it has", thread.ID)
				}
			}
		}
	} else if !errors.Is(err, store.ErrNotFound) {
		return "", err
	}
	// The pull requests are read on GitHub and checked before the report
	// is filed (ADR-0016); a report filed already, as by this call run
	// again after a restart, is not checked again.
	var pulls []store.PullRow
	var facts []team.PullFacts
	filing, unlockPulls := ctx, func() {}
	if filed, err := s.store.ReportFiled(ctx, key); err != nil {
		return "", err
	} else if !filed && thread.State == store.ThreadClosed {
		// A new report on a closed thread is refused before GitHub is
		// read; one that closes meanwhile is refused when it is filed.
		return "", fmt.Errorf("thread T%d is closed and takes no more reports", thread.ID)
	} else if !filed && len(r.PullRequests) > 0 {
		// Each pull request is held from its read to the report's record,
		// so no round reads it in between; the call's abort stops the
		// reads, and the filing too until it commits.
		refs := make([]string, 0, len(r.PullRequests))
		for _, ref := range r.PullRequests {
			repo, err := team.PullRepository(ref, thread.Repository)
			if err != nil {
				return "", err
			}
			refs = append(refs, repo+"#"+strconv.FormatInt(ref.Number, 10))
		}
		if unlockPulls, err = s.lockPulls(callCtx, refs); err != nil {
			return "", fmt.Errorf("the report was stopped: %w", err)
		}
		if pulls, facts, err = s.checkPulls(callCtx, thread, r.PullRequests); err != nil {
			unlockPulls()
			return "", err
		}
		filing = callCtx
	}
	asks := make([]store.AskRow, len(r.Asks))
	for i, a := range r.Asks {
		options := make([]string, len(a.Options))
		for j, o := range a.Options {
			options[j] = strings.TrimSpace(o)
		}
		asks[i] = store.AskRow{Key: key + ":ask:" + strconv.Itoa(i), SessionID: call.Session, Question: strings.TrimSpace(a.Question), Options: options}
	}
	name := s.agentName(thread.Agent)
	text := func(ids []int64, linked []store.PullRow) string {
		return team.ReportText(thread.ID, name, r.Status, r.Message, ids, pullNames(linked)...)
	}
	// The supervisor's messages this report came after are noted first,
	// under outboxMu held through the filing, so a done thread they reopen
	// takes the report's state after them, and one placed after the report
	// reopens it once noted (ADR-0032).
	s.outboxMu.Lock()
	if err := s.noteSendsPlaced(ctx, thread.ID); err != nil {
		s.outboxMu.Unlock()
		unlockPulls()
		return "", err
	}
	s.workMu.Lock()
	// The profile as it is now, right before the filing: the reads, or the
	// wait for workMu, may have been long.
	if err := s.recheckPulls(thread, facts); err != nil {
		s.workMu.Unlock()
		s.outboxMu.Unlock()
		unlockPulls()
		return "", err
	}
	updated, filed, message, linked, err := s.store.FileReportOnShift(filing, key, thread.ID, team.ThreadState(r.Status), strings.TrimSpace(r.Message), asks, pulls, seen, text)
	if err == nil {
		// Each ask's event, then the thread's, which publishes their plan
		// once for the whole report (Codex review of #25, round 5).
		for _, row := range filed {
			s.emitAsk(row)
		}
		for _, row := range linked {
			s.publishPull(row)
		}
		s.publishThread(updated)
	}
	s.workMu.Unlock()
	s.outboxMu.Unlock()
	unlockPulls()
	if errors.Is(err, store.ErrPullTaken) {
		return "", err
	} else if errors.Is(err, store.ErrConflict) {
		return "", fmt.Errorf("thread T%d is closed and takes no more reports", thread.ID)
	} else if err != nil {
		return "", err
	}
	// The report is filed; sending it to the supervisor is the service's,
	// in the order the thread's reports were filed, and one that cannot go
	// now is tried again until it does. It goes before any read of GitHub
	// below.
	if done, err := s.sendReports(ctx, message.ThreadID); err != nil || !done {
		if err != nil {
			s.log.Warn("send report", "thread", thread.ID, "err", err)
		}
		s.deliverLater()
	}
	if len(r.PullRequests) > 0 && updated.State != store.ThreadClosed {
		// A pull request reported merged frees the thread's place now, also
		// when the call runs again after a stop before it did.
		if err := s.closeIfMerged(ctx, thread.ID); err != nil {
			s.log.Warn("close a thread whose pull requests merged", "thread", thread.ID, "err", err)
		}
	}
	if len(linked) > 0 {
		// Its pull requests are read again with their merge base, their
		// labels brought in step, and reviewed now, not at the next round
		// (ADR-0019): a head reported again after a base branch reset is
		// backed by nothing (Codex review of #26, round 8). The review
		// pass reads them, off this tool call (round 9).
		ids := make([]int64, len(linked))
		for i, p := range linked {
			ids[i] = p.ID
		}
		s.reviewAfterRead(ids...)
	}
	ids := make([]int64, len(filed))
	for i, row := range filed {
		ids[i] = row.ID
	}
	if len(ids) == 0 {
		return fmt.Sprintf("Reported on thread T%d to the supervisor.", thread.ID), nil
	}
	names := make([]string, len(ids))
	for i, id := range ids {
		names[i] = "A" + strconv.FormatInt(id, 10)
	}
	return fmt.Sprintf("Reported on thread T%d to the supervisor. %s wait for the owner; each answer comes to this session as a message.",
		thread.ID, strings.Join(names, ", ")), nil
}

// TeamClose runs team_close for session call.Session: a supervisor closes
// a thread that a session of its own profile delegated. The close is
// recorded with the call's key, so the call run again after a restart
// answers as the first did, whatever the profiles say by then.
func (s *Server) TeamClose(ctx context.Context, call TeamCall, id int64) (string, error) {
	// The call's own context bounds a fresh close's reads on GitHub: an
	// abort stops them, and the close (Codex review of #25, round 7). A
	// close reserved already finishes whatever the call does.
	callCtx := ctx
	ctx = context.WithoutCancel(ctx)
	switch result, reserved, err := s.store.LookupRequest(ctx, call.key(), "closeThread:"+strconv.FormatInt(id, 10)); {
	case err != nil:
		return "", err
	case result == store.RequestClosed:
		return s.closedText(ctx, id)
	case reserved:
		// A close reserved and cut off before it committed: its checks
		// passed then, and it finishes now, a plan task's pull requests
		// read first, since one may have merged meanwhile (Codex review of
		// #25, round 6).
		if _, err := s.closeTaskThread(ctx, id, call.key(), false, func() (store.ThreadRow, error) { return s.store.CloseThreadAs(ctx, call.key(), id) }); err != nil {
			return "", err
		}
		return s.closedText(ctx, id)
	}
	caller, err := s.callerProfile(ctx, call.Session)
	if err != nil {
		return "", err
	}
	if caller == nil || caller.Role != profiles.RoleSupervisor {
		return "", errors.New("only a supervisor closes threads: this session's profile is not one")
	}
	thread, err := s.store.Thread(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return "", fmt.Errorf("no thread T%d", id)
	} else if err != nil {
		return "", err
	}
	// pocketd started the review, and ends it: no supervisor delegated it
	// (ADR-0019; Codex review of #26, round 5).
	if thread.ReviewPull != 0 {
		return "", fmt.Errorf("thread T%d is a cross-review pocketd runs and ends: no supervisor closes it", id)
	}
	delegator, err := s.store.Session(ctx, thread.SupervisorSession)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return "", err
	}
	if err != nil || delegator.Profile != caller.ID {
		return "", fmt.Errorf("thread T%d was delegated by another supervisor", id)
	}
	if _, err := s.closeTaskThread(callCtx, id, call.key(), false, func() (store.ThreadRow, error) { return s.store.CloseThreadAs(ctx, call.key(), id) }); err != nil {
		return "", err
	}
	return s.closedText(ctx, id)
}

// closedText is team_close's answer: it depends only on the thread, so a
// call run again answers as the first did.
func (s *Server) closedText(ctx context.Context, id int64) (string, error) {
	thread, err := s.store.Thread(ctx, id)
	if err != nil {
		return "", err
	}
	return team.ClosedText(id, thread.Plan, thread.IsPlanThread()), nil
}

// DeliverPending sends each report and each answer that has not reached
// its session yet (deliverAll). What has not reached it is tried again
// later (deliverLater), without a restart.
func (s *Server) DeliverPending(ctx context.Context) error {
	done, err := s.deliverAll(ctx)
	if err != nil || !done {
		s.deliverLater()
	}
	return err
}

// StartDelivery starts the first pass over what did not reach its
// session before a stop. The service calls it once at start, after
// engine.Resume (a submission may let PiG's scheduler run), and does not
// wait for it: a backlog never holds back serving.
func (s *Server) StartDelivery() {
	s.deliverLater()
	// The shifts go on from where a stop left them (ADR-0024).
	s.shiftSoon()
}

// deliverAll brings what has not reached its session there, under
// outboxMu: each followed thread's brief (and the close of a thread whose
// delegation ended before it started, followThread), each thread's
// reports in the order they were filed, up to the first that has not
// reached the supervisor, and every answer, so one session that takes no
// message holds back no other. done is false while something has not
// reached its session, or a thread still starts.
func (s *Server) deliverAll(ctx context.Context) (done bool, err error) {
	// outboxMu is taken per message (followThread, sendReports, deliver),
	// never for the whole pass: a close or an answer waits for one
	// message, not for a backlog.
	var failures []error
	done = true
	if released, err := s.releasePending(ctx); err != nil || !released {
		failures = append(failures, err)
		done = false
	}
	if refreshed, err := s.refreshLessons(ctx); err != nil || !refreshed {
		failures = append(failures, err)
		done = false
	}
	if err := s.standingPass(ctx); err != nil {
		failures = append(failures, err)
		done = false
	}
	// A review's brief is the review pass's alone, which reads its pull
	// request on GitHub before the brief goes, and follows it when the
	// review's run ends, at each round and at each report, close and
	// confirmation: this pass, which runs every second while anything is
	// left, never drives a review's GitHub reads (Codex review of #26,
	// rounds 10 and 11). A thread a failed start closed frees a place a
	// review may wait for: a review pass runs after the following.
	review := false
	if threads, err := s.store.ThreadsToFollow(ctx); err != nil {
		failures = append(failures, err)
	} else {
		for _, row := range threads {
			if row.ReviewPull != 0 {
				continue
			}
			settled, freed, err := s.followThread(ctx, row.ID)
			if err != nil {
				failures = append(failures, fmt.Errorf("thread T%d: %w", row.ID, err))
			}
			done = done && settled
			review = review || freed
		}
	}
	if review {
		s.reviewSoon()
	}
	// Workers whose replies or stop could not be done are read again
	// (ADR-0021).
	if !s.followDueWorkers(ctx) {
		done = false
	}
	if threads, err := s.store.ThreadsWithUnsentReports(ctx); err != nil {
		failures = append(failures, err)
	} else {
		for _, thread := range threads {
			sent, err := s.sendReports(ctx, thread)
			if err != nil {
				failures = append(failures, fmt.Errorf("reports of thread T%d: %w", thread, err))
			}
			done = done && sent
		}
	}
	if !s.republishPlans(ctx) {
		done = false
	}
	if !s.republishGathers(ctx) {
		done = false
	}
	if !s.republishThreads(ctx) {
		done = false
	}
	if started, err := s.startConsults(ctx); err != nil || !started {
		failures = append(failures, err)
		done = false
	}
	if sent, err := s.deliverNotes(ctx); err != nil || !sent {
		failures = append(failures, err)
		done = false
	}
	// The gathers' asks start, and end once over, after their briefs were
	// noted placed above (ADR-0033).
	if followed, err := s.followGathers(ctx); err != nil || !followed {
		failures = append(failures, err)
		done = false
	}
	if ids, err := s.store.UndeliveredAnswerIDs(ctx); err != nil {
		failures = append(failures, err)
	} else {
		for _, id := range ids {
			placed, err := s.deliver(ctx, id)
			if err != nil {
				failures = append(failures, fmt.Errorf("answer to A%d: %w", id, err))
			}
			done = done && placed
		}
	}
	return done && len(failures) == 0, errors.Join(failures...)
}

// releasePending frees the reservations of closes and owner's messages
// whose release failed, under releaseMu, which a request of the same key
// takes before it claims it again: none is freed from under it. done says
// none is left.
func (s *Server) releasePending(ctx context.Context) (done bool, err error) {
	var failures []error
	// The uploads a stop left pending go, files and claims, outside
	// releaseMu, since each is dropped under its request's key (Codex
	// review of #49, rounds 1 and 2).
	uploadsLeft, err := s.settleLeftUploads(ctx)
	if err != nil {
		failures = append(failures, fmt.Errorf("settle the uploads a stop left: %w", err))
	}
	s.releaseMu.Lock()
	defer s.releaseMu.Unlock()
	if s.claimsDue {
		if left, err := s.store.LeftClaims(ctx, modelKindPrefix, s.startedAt); err != nil {
			failures = append(failures, fmt.Errorf("read the claims a stop left: %w", err))
		} else {
			for key, kind := range left {
				if _, ok := s.unreleased[key]; !ok {
					s.unreleased[key] = kind
				}
			}
			s.claimsDue = false
		}
	}
	for key, kind := range s.unreleased {
		// A change of a session's model is freed only once PiG has no
		// change of its key: one PiG has took effect (Codex review of #48,
		// round 1).
		if id, ok := modelKindSession(kind); ok {
			if _, in, err := s.engine.ModelChange(ctx, id, key); err != nil {
				failures = append(failures, fmt.Errorf("look up the model change of %s: %w", kind, err))
				continue
			} else if in {
				delete(s.unreleased, key)
				continue
			}
		}
		// An owner's message's claim is freed only once PiG says it has no
		// submission of its key: one PiG has went (Codex review of #36).
		// So is a Start Over's, once PiG has no reset of its key (ADR-0026).
		session, ok := strings.CutPrefix(kind, "send:")
		if !ok {
			session, ok = strings.CutPrefix(kind, "startOver:")
		}
		if ok {
			id, err := strconv.ParseInt(session, 10, 64)
			if err != nil {
				delete(s.unreleased, key)
				continue
			}
			if where, err := s.engine.Placed(ctx, id, key); err != nil {
				failures = append(failures, fmt.Errorf("look up the message of %s: %w", kind, err))
				continue
			} else if where != PlacementAbsent {
				delete(s.unreleased, key)
				continue
			}
		}
		if err := s.releaseRequest(ctx, key, kind); err != nil {
			failures = append(failures, fmt.Errorf("free the request id of %s: %w", kind, err))
			continue
		}
		delete(s.unreleased, key)
	}
	return len(s.unreleased) == 0 && !uploadsLeft, errors.Join(failures...)
}

// followThread settles a thread a pass follows. A starting one whose
// delegating call's task ended will not start: unless its specialist may
// have its brief, it closes. A brief kept and not in the specialist's
// transcript goes there (placeBrief). A thread still starting once its
// brief went opens, as attachBriefOpen would have, without the brief
// again. settled is false while the thread still starts or its brief has
// not arrived.
//
// The thread is read again under workMu, which it holds through the
// brief's sending: a close comes before, and no brief goes, or after.
// freed says it closed the thread, which frees a place on the desk.
func (s *Server) followThread(ctx context.Context, id int64) (settled, freed bool, err error) {
	s.outboxMu.Lock()
	defer s.outboxMu.Unlock()
	s.workMu.Lock()
	defer s.workMu.Unlock()
	row, err := s.store.Thread(ctx, id)
	if err != nil || row.State == store.ThreadClosed {
		return err == nil, false, err
	}
	if row.State == store.ThreadStarting && row.TaskID != 0 {
		ended, err := s.engine.TaskEnded(ctx, row.TaskID)
		if err != nil {
			return false, false, err
		}
		if !ended {
			return false, false, nil // its call is still at work
		}
		may, err := s.mayHaveBrief(ctx, row)
		if err != nil {
			return false, false, err
		}
		if !may {
			closed, err := s.closeFailed(ctx, row.ID, "its delegation ended before the thread started")
			if err == nil {
				s.publishThread(closed)
			}
			return err == nil, err == nil, err
		}
	}
	if row.Session == 0 || row.Brief == "" {
		return true, false, nil
	}
	placed := row.Briefed
	if !placed {
		placed, err = s.placeBrief(ctx, row, nil, false)
		var moved folderChanged
		var expired shiftExpired
		if errors.As(err, &expired) {
			closed, err := s.expireBrief(ctx, row.ID, expired)
			return err == nil, err == nil && closed.State == store.ThreadClosed, err
		}
		if errors.As(err, &moved) {
			// The brief never reached the specialist, and its folder is
			// no longer the one the brief names: the thread cannot start.
			closed, err := s.store.FailUnbriefed(ctx, row.ID, team.Clip("its folder changed before its brief reached the specialist: "+moved.Error(), team.MaxFailure))
			if err == nil {
				s.publishThread(closed)
			}
			return err == nil, err == nil && closed.State == store.ThreadClosed, err
		}
		if err != nil {
			return false, false, err
		}
	}
	if row.State != store.ThreadStarting {
		return placed, false, nil
	}
	// The brief went, or waits in the queue, and the opening failed to be
	// recorded: the thread opens now, so the owner may message its
	// specialist.
	opened, err := s.openThread(ctx, row.ID)
	if err != nil {
		return false, false, err
	}
	s.publishThread(opened)
	return placed, false, nil
}

// placeBrief brings thread's kept brief to its specialist (place), and
// notes it once it is in the transcript. send, when set, is the context
// of the brief's submission. Unless checked (the caller just checked it),
// the specialist's folder is checked again right before any submission:
// the brief names it, and the specialist starts its work there.
func (s *Server) placeBrief(ctx context.Context, row store.ThreadRow, send context.Context, checked bool) (bool, error) {
	// A shift's brief goes only before its deadline (briefInTime), checked
	// before each submission, a retry's included (Codex review of #36).
	check := func(ctx context.Context) error {
		if !checked {
			if err := s.recheckFolder(ctx, row, row.Cwd, row.Branch); err != nil {
				return err
			}
		}
		// A shift's brief goes only to an idle specialist, so PiG never
		// keeps it in a queue, and only before the deadline, checked last,
		// right before the submission, after every read: a folder's check
		// takes time (Codex review of #36).
		if _, err := s.store.Shift(ctx, row.ID); err == nil {
			if err := s.specialistIdle(ctx, row.Session); err != nil {
				return err
			}
		} else if !errors.Is(err, store.ErrNotFound) {
			return err
		}
		return s.briefInTime(ctx, row)
	}
	_, shiftErr := s.store.Shift(ctx, row.ID)
	if shiftErr != nil && !errors.Is(shiftErr, store.ErrNotFound) {
		return false, shiftErr
	}
	placed, err := s.place(ctx, outgoing{session: row.Session, key: briefKey(row), text: row.Brief, submission: row.BriefSubmission,
		attempt: row.BriefAttempt, send: send, check: check, idleOnly: shiftErr == nil},
		func(sub int64) error { return s.store.BriefSubmitted(ctx, row.ID, sub) },
		func(attempt int) (bool, error) { return s.store.BriefWithdrawn(ctx, row.ID, attempt) })
	if errors.Is(err, errSpecialistBusy) {
		return false, nil
	}
	if err != nil || !placed {
		return false, err
	}
	if err := s.store.Briefed(ctx, row.ID); err != nil {
		return true, err
	}
	// A shift's round 1 may be over already: a pass looks, whatever a
	// read of the thread's shift would say now (Codex review of #36).
	s.shiftSoon()
	return true, nil
}

// sendReports sends thread's unsent reports, read now, in the order they
// were filed, up to the first that has not reached the supervisor, so it
// never gets an older report after a newer one. done says all of them
// reached it.
func (s *Server) sendReports(ctx context.Context, thread int64) (done bool, err error) {
	// Each session takes its reports in order, whatever another takes: the
	// supervisor's reports and the specialist's worker replies wait for
	// their own session alone (Codex review of #29, round 1).
	sessions, err := s.store.UnsentReportSessions(ctx, thread)
	if err != nil {
		return false, err
	}
	done = true
	var failures []error
	for _, session := range sessions {
		drained, err := s.sendReportsTo(ctx, thread, session)
		if err != nil {
			failures = append(failures, err)
		}
		done = done && drained
	}
	return done && len(failures) == 0, errors.Join(failures...)
}

// sendReportsTo sends thread's unsent reports to session in order, while
// each is placed. outboxMu is held for one report at a time: read, place,
// note.
func (s *Server) sendReportsTo(ctx context.Context, thread, session int64) (done bool, err error) {
	for {
		placed, more, err := s.sendOldestReport(ctx, thread, session)
		if s.afterReport != nil {
			s.afterReport()
		}
		if err != nil || !placed {
			return false, err
		}
		if !more {
			return true, nil
		}
	}
}

// sendOldestReport sends thread's oldest unsent report to session, under
// outboxMu; more says one waited, and the next may.
func (s *Server) sendOldestReport(ctx context.Context, thread, session int64) (placed, more bool, err error) {
	s.outboxMu.Lock()
	defer s.outboxMu.Unlock()
	row, ok, err := s.store.OldestUnsentReport(ctx, thread, session)
	if err != nil || !ok {
		return err == nil, false, err
	}
	placed, err = s.sendReport(ctx, row)
	return placed, true, err
}

// sendReport brings a report's message to the supervisor's session
// (place), and notes it sent once it is in the transcript. outboxMu is
// held.
func (s *Server) sendReport(ctx context.Context, row store.ReportRow) (bool, error) {
	if row.SentAt != nil {
		return true, nil
	}
	// A worker's reply is for its thread while it is open: one a close
	// overtook goes to no one (Codex review of #29, round 1). So is a
	// supervisor's message (ADR-0032).
	var check func(context.Context) error
	if droppedAtClose(row.Key) {
		thread, err := s.store.Thread(ctx, row.ThreadID)
		if err != nil {
			return false, err
		}
		if thread.State == store.ThreadClosed {
			return true, s.cancelReport(ctx, row)
		}
		// A supervisor's message goes to a watched specialist, so its
		// entry reaches the event stream, and only once the thread's brief
		// is in the specialist's transcript, as the owner's message does
		// (protocol rule 10): checked before each submission.
		if team.IsSendKey(row.Key) {
			check = func(ctx context.Context) error {
				if err := s.briefFirst(ctx, row.ThreadID); err != nil {
					return err
				}
				if err := s.engine.Watch(ctx, row.SessionID); err != nil {
					return fmt.Errorf("watch the specialist's session: %w", err)
				}
				return nil
			}
		}
		// A shift's message goes only while it begins the round under
		// way: one of a round that ended, or of a shift the owner paused
		// or stopped, goes to no one. The shift changes under outboxMu,
		// so it does not change between this read and the placement
		// (Codex review of #36).
		if isShiftMessage(row.Key) {
			sh, err := s.store.Shift(ctx, row.ThreadID)
			if err != nil {
				return false, err
			}
			if !shiftMessageCurrent(thread, sh, row.Key) {
				return true, s.cancelReport(ctx, row)
			}
			// A round's message is submitted, first or again, only before
			// the deadline, checked last before each submission: one in the
			// transcript began its round, which finishes, and the driver
			// stops a shift whose round never went (Codex review of #36).
			// It goes only to an idle specialist, so PiG never keeps it in
			// a queue (specialistIdle).
			round := strings.HasPrefix(row.Key, shiftRoundPrefix)
			check = func(ctx context.Context) error {
				if err := s.specialistIdle(ctx, row.SessionID); err != nil {
					return err
				}
				// The clock last, after every read (Codex review of #36).
				if round && !sh.Deadline.IsZero() && !s.now().Before(sh.Deadline) {
					return errRoundExpired
				}
				return nil
			}
		}
		// A message to a worker goes only to a watched worker, since PiG's
		// watch begins at the transcript's end, and only while the
		// thread's folder, where it works, is as it was: checked before
		// each submission, a retry's included (Codex review of #29, round
		// 8).
		if strings.HasPrefix(row.Key, workerMessagePrefix) {
			check = func(ctx context.Context) error {
				if err := s.engine.Watch(ctx, row.SessionID); err != nil {
					return fmt.Errorf("watch the worker's session: %w", err)
				}
				return s.recheckFolder(ctx, thread, thread.Cwd, thread.Branch)
			}
		}
	}
	placed, err := s.place(ctx, outgoing{session: row.SessionID, key: row.Key + ":report", text: row.Text, submission: row.Submission, attempt: row.Attempt, check: check,
		idleOnly: isShiftMessage(row.Key)},
		func(sub int64) error { return s.store.ReportSubmitted(ctx, row.Key, sub) },
		func(attempt int) (bool, error) { return s.store.ReportWithdrawn(ctx, row.Key, attempt) })
	if errors.Is(err, errRoundExpired) {
		s.shiftSoon()
		return false, nil
	}
	if errors.Is(err, errSpecialistBusy) || errors.Is(err, errNoBriefYet) {
		return false, nil
	}
	if err != nil || !placed {
		return false, err
	}
	if team.IsSendKey(row.Key) {
		// A supervisor's message in the transcript takes a done thread
		// back to open, with its event (ADR-0032).
		if _, err := s.sendDelivered(ctx, row); err != nil {
			return true, err
		}
	} else if err := s.store.ReportSent(ctx, row.Key); err != nil {
		return true, err
	}
	// A shift's round may be over already: its specialist may have ended
	// before the message was noted placed, a round's or the last one owed
	// (Codex review of #36).
	s.shiftSoon()
	return true, nil
}

// deliver brings ask id's answer, read now, to its asking session.
func (s *Server) deliver(ctx context.Context, id int64) (bool, error) {
	s.outboxMu.Lock()
	defer s.outboxMu.Unlock()
	row, err := s.store.Ask(ctx, id)
	if err != nil {
		return false, err
	}
	return s.deliverRow(ctx, row)
}

// deliverRow brings an answer to its asking session (place), and notes it
// delivered once it is in the transcript. outboxMu is held.
func (s *Server) deliverRow(ctx context.Context, row store.AskRow) (bool, error) {
	if row.Delivered {
		return true, nil
	}
	// Keyed by the ask's own key (its report call and place), never its
	// row number, which a restored work database gives out again: the
	// placement proves this answer, not an earlier occupant of the row.
	key := answerRequestKey(row)
	text := team.AnswerText(row.ID, row.Question, row.Answer)
	if thread, err := s.store.Thread(ctx, row.ThreadID); err != nil {
		return false, err
	} else if thread.IsPlanThread() {
		// The lead writes an answer into a plan that is not approved yet;
		// one approved no longer changes.
		plan, err := s.store.Plan(ctx, thread.Plan)
		if err != nil {
			return false, err
		}
		if plan.State == team.PlanDrafting || plan.State == team.PlanReview {
			text = team.PlanAnswerText(row.ID, row.Question, row.Answer)
		}
	}
	placed, err := s.place(ctx, outgoing{session: row.SessionID, key: key, text: text,
		submission: row.Submission, attempt: row.Attempt},
		func(sub int64) error { return s.store.AnswerSubmitted(ctx, row.ID, sub) },
		func(attempt int) (bool, error) { return s.store.AnswerWithdrawn(ctx, row.ID, attempt) })
	if err != nil || !placed {
		return false, err
	}
	if err := s.store.AnswerDelivered(ctx, row.ID); err != nil {
		return true, err
	}
	// A shift's round may wait on nothing else (Codex review of #36).
	s.shiftSoon()
	return true, nil
}

// answerRequestKey is the request key of row's answer: keyed by the ask's own
// key (its report call and place), never its row number, which a restored
// work database gives out again, and by the answer it carries.
func answerRequestKey(row store.AskRow) string { return row.Key + ":answer:" + digest(row.Answer) }

// digest is a short digest of text, for a key that names one content: an
// answer key names the answer it carries, so a placed key proves this
// answer, not another one a restored work database let the owner give.
func digest(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:6])
}

// outgoing is a message pocketd owes a session: a brief, an answer or a
// report. send, when set, is the context of its first submission.
type outgoing struct {
	session    int64
	key, text  string
	submission int64
	attempt    int
	send       context.Context
	// check, when set, runs right before a submission: one that fails
	// sends nothing.
	check func(context.Context) error
	// idleOnly submits it only while the session has no run, refused by
	// PiG in the transaction that would admit it (Engine.SendIdle): a
	// shift's own message never waits in a queue (Codex review of #36).
	idleOnly bool
}

// place brings m into its session's transcript and says whether it is
// there; outboxMu is held, so m is as the store has it. Where a message is
// comes from the durable store's record of its key (Engine.Placed), never
// a submission number, so the proof is for the message owed. A
// submission is not delivery: a busy session queues it, and an abort
// withdraws what it queued. So a message counts as delivered only once
// its entry is written. One still queued is looked at again in the next
// pass, unless its session is idle: a failed run left it queued, and PiG
// keeps such a queue until the next input, so pocketd withdraws it
// (Engine.Withdraw, only while it is still queued) and sends it again. One
// withdrawn is submitted again under a key of its own (an attempt's),
// since its first key is spent: a new input, whose run starts from its own
// entry, never a continue of the failed run. The withdrawal is noted only
// for the attempt the store still names. send is the context the first
// submission of a message takes, so a delegation's abort stops its brief.
func (s *Server) place(ctx context.Context, m outgoing, submitted func(int64) error, withdrawn func(int) (bool, error)) (bool, error) {
	key := attemptKey(m.key, m.attempt)
	where, err := s.engine.Placed(ctx, m.session, key)
	if err != nil {
		return false, err
	}
	switch where {
	case PlacementPlaced:
		return true, nil
	case PlacementQueued:
		stranded, err := s.stranded(ctx, m.session, key)
		if err != nil || !stranded {
			return false, err
		}
		fallthrough
	case PlacementWithdrawn:
		if changed, err := withdrawn(m.attempt); err != nil || !changed {
			return false, err // moved on meanwhile: the next pass reads it again
		}
		m.attempt++
		key = attemptKey(m.key, m.attempt)
	case PlacementAbsent:
		// An attempt the durable store does not have (one restored from
		// before it) is sent only if no earlier attempt still lives: a
		// placed one is the delivery, a queued one is followed.
		for earlier := m.attempt - 1; earlier >= 0; earlier-- {
			where, err := s.engine.Placed(ctx, m.session, attemptKey(m.key, earlier))
			if err != nil {
				return false, err
			}
			switch where {
			case PlacementPlaced:
				return true, nil
			case PlacementQueued:
				// Stranded on an idle session, it is withdrawn and the
				// unsent attempt goes; otherwise its run places it.
				if stranded, err := s.stranded(ctx, m.session, attemptKey(m.key, earlier)); err != nil || !stranded {
					return false, err
				}
			}
		}
	}
	if m.check != nil {
		// The check takes the submission's context when it has one: an
		// abort stops it, and the team's locks with it.
		check := ctx
		if m.send != nil {
			check = m.send
		}
		if err := m.check(check); err != nil {
			return false, err
		}
	}
	send := m.send
	if send == nil {
		send = ctx
	}
	submit := func(ctx context.Context, id int64, key, text string) (int64, error) {
		return s.engine.Send(ctx, id, key, text)
	}
	if m.idleOnly {
		submit = s.engine.SendIdle
	}
	sub, err := submit(send, m.session, key, m.text)
	if errors.Is(err, ErrSessionBusy) {
		return false, errSpecialistBusy
	}
	if err != nil {
		return false, err
	}
	if err := submitted(sub); err != nil {
		return false, err
	}
	where, err = s.engine.Placed(ctx, m.session, key)
	return where == PlacementPlaced, err
}

func attemptKey(key string, attempt int) string {
	if attempt > 0 {
		return key + ":" + strconv.Itoa(attempt)
	}
	return key
}

// stranded says whether the queued message of key waits on an idle
// session, and if so withdraws it: a busy session's run places its queue
// by itself. A message a run placed meanwhile is not withdrawn.
func (s *Server) stranded(ctx context.Context, session int64, key string) (bool, error) {
	busy, err := s.engine.Busy(ctx, session)
	if err != nil || busy {
		return false, err
	}
	return s.engine.Withdraw(ctx, session, key)
}

// deliverLater runs a pass over what did not reach its session after
// retryDelay. One pass waits or runs at a time; one asked for meanwhile
// runs after it, and a pass that leaves something behind runs again,
// until everything went or the server stopped.
func (s *Server) deliverLater() {
	s.sessionMu.Lock()
	defer s.sessionMu.Unlock()
	if s.stopped {
		return
	}
	if s.delivering {
		s.again = true
		return
	}
	s.delivering = true
	s.retry(s.deliverPass)
}

func (s *Server) deliverPass() {
	s.sessionMu.Lock()
	if s.stopped {
		s.sessionMu.Unlock()
		return
	}
	s.passes.Add(1)
	s.sessionMu.Unlock()
	defer s.passes.Done()
	done, err := s.deliverAll(s.passCtx)
	if err != nil {
		s.log.Warn("send reports and answers", "err", err)
	}
	s.sessionMu.Lock()
	s.delivering = false
	next := !done || s.again
	s.again = false
	s.sessionMu.Unlock()
	if next {
		s.deliverLater()
	}
}

// publishThread publishes a thread, and the plan it works for, which its
// change may have changed (ADR-0020). Any thread's close may have filed a
// note for a supervisor (store.threadClosed: its plan's, or one whose
// task the desk's cap held back): a delivery pass brings it, whichever
// path closed the thread (Codex review of #25, round 4).
func (s *Server) publishThread(row store.ThreadRow) {
	// A shift that cannot be read now holds the thread's event back, as a
	// plan's does: one with no shift would tell a client it has none. A
	// delivery pass publishes it as it is then (Codex review of #36).
	thread := wireThread(row)
	if sh, err := s.store.Shift(context.Background(), row.ID); err == nil {
		thread.Shift = wireShift(sh)
	} else if !errors.Is(err, store.ErrNotFound) {
		s.log.Warn("read a thread's shift to publish", "thread", row.ID, "err", err)
		s.threadRetryMu.Lock()
		s.threadRetry[row.ID] = true
		s.threadRetryMu.Unlock()
		s.deliverLater()
		thread = proto.Thread{}
	}
	if thread.ID != 0 {
		s.threadRetryMu.Lock()
		delete(s.threadRetry, row.ID)
		s.threadRetryMu.Unlock()
		s.emit(proto.Event{Type: proto.EventThread, Thread: &thread})
	}
	if row.Plan != 0 {
		s.publishPlan(context.Background(), row.Plan)
	}
	if row.State == store.ThreadClosed {
		s.deliverLater()
	}
}

// republishThreads publishes each thread whose event a failed read of its
// shift held back (publishThread), as it is now, under workMu; done says
// none is left.
func (s *Server) republishThreads(ctx context.Context) (done bool) {
	s.threadRetryMu.Lock()
	ids := make([]int64, 0, len(s.threadRetry))
	for id := range s.threadRetry {
		ids = append(ids, id)
	}
	s.threadRetryMu.Unlock()
	if len(ids) == 0 {
		return true
	}
	s.workMu.Lock()
	defer s.workMu.Unlock()
	done = true
	for _, id := range ids {
		row, sh, err := s.store.ThreadWithShift(ctx, id)
		if err != nil {
			done = false
			continue
		}
		thread := wireThread(row)
		if sh != nil {
			thread.Shift = wireShift(*sh)
		}
		s.threadRetryMu.Lock()
		delete(s.threadRetry, id)
		s.threadRetryMu.Unlock()
		s.emit(proto.Event{Type: proto.EventThread, Thread: &thread})
	}
	return done
}

// emitAsk publishes an ask's event alone; its plan goes with its report's
// thread (TeamMessage) or by publishAsk.
func (s *Server) emitAsk(row store.AskRow) {
	ask := wireAsk(row)
	s.emit(proto.Event{Type: proto.EventAsk, Ask: &ask})
}

// publishAsk publishes an ask, and the plan whose gate it holds when it is
// a plan thread's.
func (s *Server) publishAsk(row store.AskRow) {
	s.emitAsk(row)
	if row.Plan != 0 {
		// A plan whose read fails here goes at the next pass (publishPlan).
		s.publishPlan(context.Background(), row.Plan)
		if row.State == store.AskAnswered {
			s.deliverLater() // its answer may have cleared the plan's gate (store.askCleared)
		}
	}
}

func wireThread(row store.ThreadRow) proto.Thread {
	return proto.Thread{ID: row.ID, Title: row.Title, Agent: row.Agent, Supervisor: row.SupervisorSession, Session: row.Session,
		Repository: row.Repository, Cwd: row.Cwd, Branch: row.Branch, Task: row.Task, State: proto.ThreadState(row.State),
		Report: row.Report, Plan: row.Plan, PlanTask: row.PlanTask, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt}
}

func wireAsk(row store.AskRow) proto.Ask {
	out := proto.Ask{ID: row.ID, Thread: row.ThreadID, SessionID: row.SessionID, Question: row.Question, State: proto.AskState(row.State),
		Answer: row.Answer, AnsweredBy: row.AnsweredBy, CreatedAt: row.CreatedAt, AnsweredAt: row.AnsweredAt}
	if len(row.Options) > 0 {
		out.Options = slices.Clone(row.Options)
	}
	return out
}

// threads lists every thread, the most recently changed first.
func (s *Server) threads(w http.ResponseWriter, r *http.Request) {
	// Each thread with its shift, read in one transaction (Codex review of
	// #36).
	rows, shifts, err := s.store.ThreadsWithShifts(r.Context())
	if err != nil {
		s.internal(w, "list threads", err)
		return
	}
	out := make([]proto.Thread, 0, len(rows))
	for _, row := range rows {
		thread := wireThread(row)
		if sh, ok := shifts[row.ID]; ok {
			thread.Shift = wireShift(sh)
		}
		out = append(out, thread)
	}
	writeJSON(w, http.StatusOK, out)
}

// ownerCloseThread closes a thread for the owner; closing a closed one
// changes nothing. The request id is claimed in the transaction that
// closes (store.CloseThreadAs).
func (s *Server) ownerCloseThread(w http.ResponseWriter, r *http.Request) {
	id, ok := pathIDOf(w, r, "thread")
	if !ok {
		return
	}
	var request proto.CloseThread
	if !decode(w, r, &request) || !validRequestID(w, request.RequestID) {
		return
	}
	ctx := context.WithoutCancel(r.Context())
	key := requestKey(r, request.RequestID)
	defer s.keys.lock(key)()
	// The request id first: one used for another request is conflict,
	// and does no work, no read on GitHub included (Codex review of #25,
	// round 6). A close that committed answers as it did.
	result, existed, err := s.store.LookupRequest(ctx, key, "closeThread:"+strconv.FormatInt(id, 10))
	if errors.Is(err, store.ErrRequestReused) {
		reused(w)
		return
	} else if err != nil {
		s.internal(w, "close thread", err)
		return
	}
	// A plan task's pull requests as GitHub has them now, before the
	// close settles its task (closeTaskThread).
	closed, err := s.closeTaskThread(ctx, id, key, existed && result == store.RequestClosed, func() (store.ThreadRow, error) { return s.store.CloseThreadAs(ctx, key, id) })
	switch {
	case errors.Is(err, store.ErrRequestReused):
		reused(w)
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, proto.ErrorCodeNotFound, fmt.Sprintf("no thread %d", id))
	case errors.Is(err, errRequestKept):
		s.log.Error("close thread", "err", err)
		writeError(w, http.StatusInternalServerError, proto.ErrorCodeInternal,
			"close thread failed, and its requestId could not be freed yet: the service frees it later; retry this close with the same requestId")
	case err != nil:
		s.internal(w, "close thread", err)
	default:
		// The closed thread with its shift, read in one transaction: a shift
		// that cannot be read fails the answer, and the close tried again
		// answers whole, never a thread with no shift (Codex review of #36).
		row, sh, err := s.store.ThreadWithShift(ctx, closed.ID)
		if err != nil {
			s.internal(w, "read the closed thread", err)
			return
		}
		thread := wireThread(row)
		if sh != nil {
			thread.Shift = wireShift(*sh)
		}
		writeJSON(w, http.StatusOK, thread)
	}
}

// asks lists the asks that wait for the owner, the oldest first.
func (s *Server) asks(w http.ResponseWriter, r *http.Request) {
	rows, err := s.store.OpenAsks(r.Context())
	if err != nil {
		s.internal(w, "list asks", err)
		return
	}
	out := make([]proto.Ask, 0, len(rows))
	for _, row := range rows {
		out = append(out, wireAsk(row))
	}
	writeJSON(w, http.StatusOK, out)
}

// answer answers an ask once and sends the answer to the asking session.
// Ask events leave in the order the asks changed (workMu).
func (s *Server) answer(w http.ResponseWriter, r *http.Request) {
	id, ok := pathIDOf(w, r, "ask")
	if !ok {
		return
	}
	var request proto.AnswerAsk
	if !decode(w, r, &request) || !validRequestID(w, request.RequestID) {
		return
	}
	text := strings.TrimSpace(request.Answer)
	if text == "" || utf8.RuneCountInString(text) > proto.MaxAnswer {
		writeError(w, http.StatusBadRequest, proto.ErrorCodeBadRequest, fmt.Sprintf("answer must be 1 to %d characters and not only spaces", proto.MaxAnswer))
		return
	}
	ctx := context.WithoutCancel(r.Context())
	key := requestKey(r, request.RequestID)
	defer s.keys.lock(key)()
	s.workMu.Lock()
	row, replayed, err := s.store.AnswerAsk(ctx, key, id, text, deviceOf(r).Name)
	if err == nil && !replayed {
		s.publishAsk(row)
	}
	s.workMu.Unlock()
	switch {
	case errors.Is(err, store.ErrRequestReused):
		reused(w)
		return
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, proto.ErrorCodeNotFound, fmt.Sprintf("no ask %d", id))
		return
	case errors.Is(err, store.ErrConflict):
		writeError(w, http.StatusConflict, proto.ErrorCodeConflict, fmt.Sprintf("ask %d is already answered", id))
		return
	case err != nil:
		s.internal(w, "answer ask", err)
		return
	}
	// The answer is the owner's; bringing it to the session is the
	// service's, until it is in the transcript.
	if placed, err := s.deliver(ctx, row.ID); err != nil || !placed {
		if err != nil {
			s.log.Warn("send answer", "ask", row.ID, "err", err)
		}
		s.deliverLater()
	}
	writeJSON(w, http.StatusOK, wireAsk(row))
}
