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
	_, err := router.Choose(router.Request{Profile: &p}, models)
	return err
}

// roster is a supervisor's part of its session's instructions: whom it can
// delegate to now (team.Roster).
func (s *Server) roster(models []proto.Model) (string, error) {
	valid, _, err := s.profiles.Load()
	if err != nil {
		return "", err
	}
	return team.Roster(valid, func(p profiles.Profile) error { return s.startable(p, models) }, s.deskCap), nil
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
	name := s.agentName(started.Agent)
	where := "in " + started.Cwd
	if started.Branch != "" {
		where += " on the branch " + started.Branch
	}
	return fmt.Sprintf("Delegated to %s as thread T%d: %s. %s works %s, in session %d. Reports come to you as messages that start with [T%d · %s · <status>].",
		name, started.ID, started.Title, name, where, started.Session, started.ID, name), nil
}

// failThread closes a thread that could not start, with why.
func (s *Server) failThread(ctx context.Context, id int64, why string) error {
	_, err := s.changeThread(func() (store.ThreadRow, error) { return s.closeFailed(ctx, id, why) })
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
		row, _, err := s.store.CreateThread(db, store.ThreadRow{Key: key, Title: team.Title(d), Agent: target.ID,
			SupervisorSession: call.Session, Repository: d.Repository, Task: strings.TrimSpace(d.Task), TaskID: call.Task})
		return row, err
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
		spec = SessionSpec{Title: made.Title, Cwd: made.Cwd, Model: made.Model, Thinking: made.Thinking, Route: made.Route}
		profile, cwd = made.Profile, made.Cwd
		if thread.Repository != "" {
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
	return s.attachBriefOpen(ctx, thread, sessionKey, spec, profile, cwd, branch, supervisor)
}

// attachBriefOpen starts the specialist's session, gives the thread its
// session and brief, sends the brief and opens the thread, all under
// outboxMu and workMu, from a fresh read of the thread: a close comes
// before, and no session starts and no brief goes, or after the opening,
// as the close of any open thread. A closed thread never gets a session or
// a brief, also in a run again. The brief is followed until it is in the
// specialist's transcript (placeBrief): a queue that holds it opens the
// thread, and an abort that withdraws it has it sent again.
func (s *Server) attachBriefOpen(ctx context.Context, thread store.ThreadRow, sessionKey string, spec SessionSpec, profile, cwd, branch, supervisor string) (store.ThreadRow, error) {
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
	brief := team.Brief(supervisor, thread.ID, thread.Title, thread.Task, cwd, branch)
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
func (s *Server) closeThread(ctx context.Context, id int64, key string, close func() (store.ThreadRow, error)) (store.ThreadRow, error) {
	s.outboxMu.Lock()
	defer s.outboxMu.Unlock()
	s.workMu.Lock()
	defer s.workMu.Unlock()
	// A request id used for another request is refused before anything
	// else, also for a thread that does not exist (protocol rule 5); a
	// fresh one stays unclaimed when the thread does not exist.
	kind := "closeThread:" + strconv.FormatInt(id, 10)
	if _, _, err := s.store.LookupRequest(ctx, key, kind); err != nil {
		return store.ThreadRow{}, err
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
	_, existed, err := s.store.ClaimRequest(ctx, key, kind)
	if err != nil {
		return store.ThreadRow{}, err
	}
	_, pending := s.unreleased[key]
	delete(s.unreleased, key)
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
			s.unreleased[key] = kind
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
	closed, err := close()
	if err != nil {
		return store.ThreadRow{}, errors.Join(err, release())
	}
	s.publishThread(closed)
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
	return SessionSpec{Title: "T" + strconv.FormatInt(thread.ID, 10) + " · " + thread.Title, Cwd: cwd, Model: choice.Model,
		Profile: &target, Route: string(choice.Rule), Thread: thread.ID}, cwd, branch, nil
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
		checkout := target.Repositories[thread.Repository]
		path, branch := team.Worktree(checkout, target.ID, team.Slug(thread.Title, thread.ID, thread.Owner))
		if checkout == "" || path != spec.Cwd {
			return SessionSpec{}, fmt.Errorf("%s's checkout of %s changed while its worktree was made", target.ID, thread.Repository)
		}
		check, cancel := s.withTimeout(ctx, checkTimeout)
		defer cancel()
		if err := worktree.Usable(check, s.worktreeTools(), checkout, path, branch); err != nil {
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
	if err := team.CheckTarget(target, func() error { return s.startable(target, models) }); err != nil {
		return SessionSpec{}, err
	}
	if err := team.CheckDestination(target, thread.Repository); err != nil {
		return SessionSpec{}, err
	}
	if thread.Repository != "" {
		if path, _ := team.Worktree(target.Repositories[thread.Repository], target.ID, team.Slug(thread.Title, thread.ID, thread.Owner)); path != spec.Cwd {
			return SessionSpec{}, fmt.Errorf("%s's checkout of %s changed while its worktree was checked", target.ID, thread.Repository)
		}
	} else if target.Start() != spec.Cwd {
		return SessionSpec{}, fmt.Errorf("%s's home changed while the thread started", target.ID)
	}
	if s.afterRecheck != nil {
		s.afterRecheck()
	}
	choice, err := router.Choose(router.Request{Profile: &target}, models)
	if err != nil {
		return SessionSpec{}, err
	}
	spec.Profile, spec.Model, spec.Route = &target, choice.Model, string(choice.Rule)
	spec.Lessons = sectionOf(lessons)
	return spec, nil
}

// worktreeTools are the engine's sandboxed git and existence check.
func (s *Server) worktreeTools() worktree.Tools {
	return worktree.Tools{Git: s.engine.Git, Kind: s.engine.Kind, Read: s.engine.Read, Same: s.engine.Same}
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
	if err := worktree.Usable(check, s.worktreeTools(), strings.TrimSuffix(cwd, suffix), cwd, branch); err != nil {
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
	if err := team.CheckReport(r); err != nil {
		return "", err
	}
	if err := team.CheckPullRefs(r.PullRequests, thread.Repository); err != nil {
		return "", err
	}
	key := call.key()
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
	s.workMu.Lock()
	// The profile as it is now, right before the filing: the reads, or the
	// wait for workMu, may have been long.
	if err := s.recheckPulls(thread, facts); err != nil {
		s.workMu.Unlock()
		unlockPulls()
		return "", err
	}
	updated, filed, message, linked, err := s.store.FileReportWithPulls(filing, key, thread.ID, team.ThreadState(r.Status), strings.TrimSpace(r.Message), asks, pulls, text)
	if err == nil {
		for _, row := range filed {
			s.publishAsk(row)
		}
		for _, row := range linked {
			s.publishPull(row)
		}
		s.publishThread(updated)
	}
	s.workMu.Unlock()
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
	ctx = context.WithoutCancel(ctx)
	switch result, reserved, err := s.store.LookupRequest(ctx, call.key(), "closeThread:"+strconv.FormatInt(id, 10)); {
	case err != nil:
		return "", err
	case result == store.RequestClosed:
		return s.closedText(ctx, id)
	case reserved:
		// A close reserved and cut off before it committed: its checks
		// passed then, and it finishes now.
		if _, err := s.closeThread(ctx, id, call.key(), func() (store.ThreadRow, error) { return s.store.CloseThreadAs(ctx, call.key(), id) }); err != nil {
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
	delegator, err := s.store.Session(ctx, thread.SupervisorSession)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return "", err
	}
	if err != nil || delegator.Profile != caller.ID {
		return "", fmt.Errorf("thread T%d was delegated by another supervisor", id)
	}
	if _, err := s.closeThread(ctx, id, call.key(), func() (store.ThreadRow, error) { return s.store.CloseThreadAs(ctx, call.key(), id) }); err != nil {
		return "", err
	}
	return s.closedText(ctx, id)
}

// closedText is team_close's answer: it depends only on the thread, so a
// call run again answers as the first did.
func (s *Server) closedText(_ context.Context, id int64) (string, error) {
	return fmt.Sprintf("Closed thread T%d: its place on the desk is free.", id), nil
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
func (s *Server) StartDelivery() { s.deliverLater() }

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
	if threads, err := s.store.ThreadsToFollow(ctx); err != nil {
		failures = append(failures, err)
	} else {
		for _, row := range threads {
			settled, err := s.followThread(ctx, row.ID)
			if err != nil {
				failures = append(failures, fmt.Errorf("thread T%d: %w", row.ID, err))
			}
			done = done && settled
		}
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

// releasePending frees the close reservations whose release failed
// (closeThread), under workMu, so no close of the same key runs between.
// done says none is left.
func (s *Server) releasePending(ctx context.Context) (done bool, err error) {
	s.workMu.Lock()
	defer s.workMu.Unlock()
	var failures []error
	for key, kind := range s.unreleased {
		if err := s.releaseRequest(ctx, key, kind); err != nil {
			failures = append(failures, fmt.Errorf("free the request id of %s: %w", kind, err))
			continue
		}
		delete(s.unreleased, key)
	}
	return len(s.unreleased) == 0, errors.Join(failures...)
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
func (s *Server) followThread(ctx context.Context, id int64) (settled bool, err error) {
	s.outboxMu.Lock()
	defer s.outboxMu.Unlock()
	s.workMu.Lock()
	defer s.workMu.Unlock()
	row, err := s.store.Thread(ctx, id)
	if err != nil || row.State == store.ThreadClosed {
		return err == nil, err
	}
	if row.State == store.ThreadStarting && row.TaskID != 0 {
		ended, err := s.engine.TaskEnded(ctx, row.TaskID)
		if err != nil {
			return false, err
		}
		if !ended {
			return false, nil // its call is still at work
		}
		may, err := s.mayHaveBrief(ctx, row)
		if err != nil {
			return false, err
		}
		if !may {
			closed, err := s.closeFailed(ctx, row.ID, "its delegation ended before the thread started")
			if err == nil {
				s.publishThread(closed)
			}
			return err == nil, err
		}
	}
	if row.Session == 0 || row.Brief == "" {
		return true, nil
	}
	placed := row.Briefed
	if !placed {
		placed, err = s.placeBrief(ctx, row, nil, false)
		var moved folderChanged
		if errors.As(err, &moved) {
			// The brief never reached the specialist, and its folder is
			// no longer the one the brief names: the thread cannot start.
			closed, err := s.store.FailUnbriefed(ctx, row.ID, team.Clip("its folder changed before its brief reached the specialist: "+moved.Error(), team.MaxFailure))
			if err == nil {
				s.publishThread(closed)
			}
			return err == nil, err
		}
		if err != nil {
			return false, err
		}
	}
	if row.State != store.ThreadStarting {
		return placed, nil
	}
	// The brief went, or waits in the queue, and the opening failed to be
	// recorded: the thread opens now, so the owner may message its
	// specialist.
	opened, err := s.openThread(ctx, row.ID)
	if err != nil {
		return false, err
	}
	s.publishThread(opened)
	return placed, nil
}

// placeBrief brings thread's kept brief to its specialist (place), and
// notes it once it is in the transcript. send, when set, is the context
// of the brief's submission. Unless checked (the caller just checked it),
// the specialist's folder is checked again right before any submission:
// the brief names it, and the specialist starts its work there.
func (s *Server) placeBrief(ctx context.Context, row store.ThreadRow, send context.Context, checked bool) (bool, error) {
	var check func(context.Context) error
	if !checked {
		check = func(ctx context.Context) error { return s.recheckFolder(ctx, row, row.Cwd, row.Branch) }
	}
	placed, err := s.place(ctx, outgoing{session: row.Session, key: briefKey(row), text: row.Brief, submission: row.BriefSubmission,
		attempt: row.BriefAttempt, send: send, check: check},
		func(sub int64) error { return s.store.BriefSubmitted(ctx, row.ID, sub) },
		func(attempt int) (bool, error) { return s.store.BriefWithdrawn(ctx, row.ID, attempt) })
	if err != nil || !placed {
		return false, err
	}
	return true, s.store.Briefed(ctx, row.ID)
}

// sendReports sends thread's unsent reports, read now, in the order they
// were filed, up to the first that has not reached the supervisor, so it
// never gets an older report after a newer one. done says all of them
// reached it.
func (s *Server) sendReports(ctx context.Context, thread int64) (done bool, err error) {
	// outboxMu is held for one report at a time: read, place, note.
	for {
		placed, more, err := s.sendOldestReport(ctx, thread)
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

// sendOldestReport sends thread's oldest unsent report, under outboxMu;
// more says one waited, and the next may.
func (s *Server) sendOldestReport(ctx context.Context, thread int64) (placed, more bool, err error) {
	s.outboxMu.Lock()
	defer s.outboxMu.Unlock()
	row, ok, err := s.store.OldestUnsentReport(ctx, thread)
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
	placed, err := s.place(ctx, outgoing{session: row.SessionID, key: row.Key + ":report", text: row.Text, submission: row.Submission, attempt: row.Attempt},
		func(sub int64) error { return s.store.ReportSubmitted(ctx, row.Key, sub) },
		func(attempt int) (bool, error) { return s.store.ReportWithdrawn(ctx, row.Key, attempt) })
	if err != nil || !placed {
		return false, err
	}
	return true, s.store.ReportSent(ctx, row.Key)
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
	key := row.Key + ":answer:" + digest(row.Answer)
	placed, err := s.place(ctx, outgoing{session: row.SessionID, key: key, text: team.AnswerText(row.ID, row.Question, row.Answer),
		submission: row.Submission, attempt: row.Attempt},
		func(sub int64) error { return s.store.AnswerSubmitted(ctx, row.ID, sub) },
		func(attempt int) (bool, error) { return s.store.AnswerWithdrawn(ctx, row.ID, attempt) })
	if err != nil || !placed {
		return false, err
	}
	return true, s.store.AnswerDelivered(ctx, row.ID)
}

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
	sub, err := s.engine.Send(send, m.session, key, m.text)
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

func (s *Server) publishThread(row store.ThreadRow) {
	thread := wireThread(row)
	s.emit(proto.Event{Type: proto.EventThread, Thread: &thread})
}

func (s *Server) publishAsk(row store.AskRow) {
	ask := wireAsk(row)
	s.emit(proto.Event{Type: proto.EventAsk, Ask: &ask})
}

func wireThread(row store.ThreadRow) proto.Thread {
	return proto.Thread{ID: row.ID, Title: row.Title, Agent: row.Agent, Supervisor: row.SupervisorSession, Session: row.Session,
		Repository: row.Repository, Cwd: row.Cwd, Branch: row.Branch, Task: row.Task, State: proto.ThreadState(row.State),
		Report: row.Report, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt}
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
	rows, err := s.store.Threads(r.Context())
	if err != nil {
		s.internal(w, "list threads", err)
		return
	}
	out := make([]proto.Thread, 0, len(rows))
	for _, row := range rows {
		out = append(out, wireThread(row))
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
	closed, err := s.closeThread(ctx, id, key, func() (store.ThreadRow, error) { return s.store.CloseThreadAs(ctx, key, id) })
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
		writeJSON(w, http.StatusOK, wireThread(closed))
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
