package api

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"os"
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
const worktreeTimeout = 5 * time.Minute

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
		thread, err = s.newThread(db, call, key, d)
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
		if current, terr := s.store.Thread(db, thread.ID); terr != nil || current.Session != 0 {
			briefed := terr != nil
			if !briefed {
				sent, serr := s.engine.Submitted(db, current.Session, "thread:"+strconv.FormatInt(thread.ID, 10)+":brief")
				briefed = serr != nil || sent
			}
			if briefed {
				return "", fmt.Errorf("thread T%d may have started; its specialist's reports will say: %s", thread.ID, why)
			}
		}
		if err := s.failThread(db, thread.ID, why); err != nil {
			s.log.Warn("close a thread that could not start", "thread", thread.ID, "err", err)
			s.failLater(thread.ID, why)
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

// failLater closes a thread that could not start in the next delivery
// pass, when closing it now failed: it never holds a place on the desk
// for good. A restart forgets it; the owner closes such a thread.
func (s *Server) failLater(id int64, why string) {
	s.sessionMu.Lock()
	if s.failing == nil {
		s.failing = map[int64]string{}
	}
	s.failing[id] = why
	s.sessionMu.Unlock()
	s.deliverLater()
}

// newThread checks a delegation against the rules and records its thread,
// under teamMu, so two delegations never both take the desk's last place.
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
	if err := team.CheckDelegation(caller, target, s.startable(target, models), d, onDesk, s.deskCap); err != nil {
		return store.ThreadRow{}, err
	}
	return s.changeThread(func() (store.ThreadRow, error) {
		row, _, err := s.store.CreateThread(ctx, store.ThreadRow{Key: key, Title: team.Title(d), Agent: target.ID,
			SupervisorSession: call.Session, Repository: d.Repository, Task: strings.TrimSpace(d.Task)})
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
	sessionKey := "thread:" + strconv.FormatInt(thread.ID, 10)
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
			_, branch = team.Worktree(made.Cwd, thread.Agent, team.Slug(thread.Title, thread.ID))
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
	id, err := s.engine.CreateSession(db, sessionKey, spec)
	if err != nil {
		return store.ThreadRow{}, err
	}
	if err := s.recordSession(db, sessionKey, id, spec, profile, teamCreator); err != nil {
		return store.ThreadRow{}, err
	}
	if s.beforeAttach != nil {
		s.beforeAttach(thread.ID)
	}
	supervisor := "The supervisor"
	if p, err := s.callerProfile(db, thread.SupervisorSession); err == nil && p != nil {
		supervisor = p.Name
	}
	return s.attachBriefOpen(ctx, thread, id, cwd, branch, supervisor)
}

// attachBriefOpen gives the thread its session, sends the brief and opens
// the thread, all under workMu: a close comes before the attachment, and
// the specialist gets no brief, or after the opening, as the close of any
// open thread. A closed thread never gets a brief, also in a run again.
func (s *Server) attachBriefOpen(ctx context.Context, thread store.ThreadRow, id int64, cwd, branch, supervisor string) (store.ThreadRow, error) {
	db := context.WithoutCancel(ctx)
	s.workMu.Lock()
	defer s.workMu.Unlock()
	attached, err := s.store.AttachThread(db, thread.ID, id, cwd, branch)
	if err != nil {
		return store.ThreadRow{}, err
	}
	s.publishThread(attached)
	switch {
	case attached.Session != id || attached.State == store.ThreadClosed:
		return store.ThreadRow{}, fmt.Errorf("thread T%d was closed while it started", thread.ID)
	case attached.State == store.ThreadStarting && ctx.Err() != nil:
		return store.ThreadRow{}, fmt.Errorf("the delegation was stopped: %w", ctx.Err())
	}
	brief := team.Brief(supervisor, thread.ID, thread.Title, thread.Task, attached.Cwd, attached.Branch)
	if _, err := s.engine.Send(db, id, "thread:"+strconv.FormatInt(thread.ID, 10)+":brief", brief); err != nil {
		return store.ThreadRow{}, fmt.Errorf("send the brief: %w", err)
	}
	// The specialist has its brief and works: a thread that fails to
	// open stays starting, never closes, and its first report, or the
	// call run again, moves it on.
	opened, err := s.openThread(db, thread.ID)
	if err != nil {
		s.log.Warn("open thread", "thread", thread.ID, "err", err)
		return attached, nil
	}
	s.publishThread(opened)
	return opened, nil
}

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
	if err := team.CheckTarget(target, s.startable(target, models)); err != nil {
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
	// Making the worktree takes a while: the session starts from the
	// profile as it is now, checked again, with the same checkout.
	was := target.Repositories[thread.Repository]
	if target, err = s.profiles.Find(thread.Agent); err != nil {
		return SessionSpec{}, "", "", err
	}
	if err := team.CheckTarget(target, s.startable(target, models)); err != nil {
		return SessionSpec{}, "", "", err
	}
	if err := team.CheckDestination(target, thread.Repository); err != nil {
		return SessionSpec{}, "", "", err
	}
	if target.Repositories[thread.Repository] != was {
		return SessionSpec{}, "", "", fmt.Errorf("%s's checkout of %s changed while its worktree was made", target.ID, thread.Repository)
	}
	if thread.Repository == "" && target.Start() != cwd {
		return SessionSpec{}, "", "", fmt.Errorf("%s's home changed while the thread started", target.ID)
	}
	choice, err := router.Choose(router.Request{Profile: &target}, models)
	if err != nil {
		return SessionSpec{}, "", "", err
	}
	return SessionSpec{Title: "T" + strconv.FormatInt(thread.ID, 10) + " · " + thread.Title, Cwd: cwd, Model: choice.Model,
		Profile: &target, Route: string(choice.Rule), Thread: thread.ID}, cwd, branch, nil
}

// threadFolder is where a thread works: a new worktree of its repository,
// owned by the thread's random owner, or the specialist's home, else its
// workspace.
// The git that makes the worktree ends with ctx, or after worktreeTimeout.
func (s *Server) threadFolder(ctx context.Context, thread store.ThreadRow, target profiles.Profile) (cwd, branch string, err error) {
	if thread.Repository != "" {
		checkout := target.Repositories[thread.Repository]
		cwd, branch = team.Worktree(checkout, target.ID, team.Slug(thread.Title, thread.ID))
		gitCtx, cancel := context.WithTimeout(ctx, worktreeTimeout)
		defer cancel()
		if err := worktree.Add(gitCtx, s.engine.Git, checkout, cwd, branch, thread.Owner); err != nil {
			return "", "", fmt.Errorf("make the worktree: %w", err)
		}
	} else {
		cwd = target.Start()
	}
	if info, err := os.Stat(cwd); err != nil || !info.IsDir() {
		return "", "", fmt.Errorf("%s is not a folder", cwd)
	}
	return cwd, branch, nil
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
	key := call.key()
	asks := make([]store.AskRow, len(r.Asks))
	for i, a := range r.Asks {
		options := make([]string, len(a.Options))
		for j, o := range a.Options {
			options[j] = strings.TrimSpace(o)
		}
		asks[i] = store.AskRow{Key: key + ":ask:" + strconv.Itoa(i), SessionID: call.Session, Question: strings.TrimSpace(a.Question), Options: options}
	}
	name := s.agentName(thread.Agent)
	text := func(ids []int64) string { return team.ReportText(thread.ID, name, r.Status, r.Message, ids) }
	s.workMu.Lock()
	updated, filed, message, err := s.store.FileReport(ctx, key, thread.ID, team.ThreadState(r.Status), strings.TrimSpace(r.Message), asks, text)
	if err == nil {
		for _, row := range filed {
			s.publishAsk(row)
		}
		s.publishThread(updated)
	}
	s.workMu.Unlock()
	if errors.Is(err, store.ErrConflict) {
		return "", fmt.Errorf("thread T%d is closed and takes no more reports", thread.ID)
	} else if err != nil {
		return "", err
	}
	// The report is filed; sending it to the supervisor is the service's,
	// in the order the thread's reports were filed, and one that cannot go
	// now is tried again until it does.
	if done, err := s.sendReports(ctx, message.ThreadID); err != nil || !done {
		if err != nil {
			s.log.Warn("send report", "thread", thread.ID, "err", err)
		}
		s.deliverLater()
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
	if _, done, err := s.store.LookupRequest(ctx, call.key(), "closeThread:"+strconv.FormatInt(id, 10)); err != nil {
		return "", err
	} else if done {
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
	if _, err := s.changeThread(func() (store.ThreadRow, error) { return s.store.CloseThreadAs(ctx, call.key(), id) }); err != nil {
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

// deliverAll sends what has not reached its session: each thread's
// reports in the order they were filed, up to the first that has not,
// and every answer, so one session that takes no message holds back no
// other; and it closes the threads whose close failed (failLater). done
// is false while something has not reached its session.
func (s *Server) deliverAll(ctx context.Context) (done bool, err error) {
	var failures []error
	done = true
	s.sessionMu.Lock()
	failing := maps.Clone(s.failing)
	s.sessionMu.Unlock()
	for id, why := range failing {
		if err := s.failThread(ctx, id, why); err != nil {
			failures = append(failures, fmt.Errorf("close thread T%d: %w", id, err))
			continue
		}
		s.sessionMu.Lock()
		delete(s.failing, id)
		s.sessionMu.Unlock()
	}
	if reports, err := s.store.UnsentReports(ctx); err != nil {
		failures = append(failures, err)
	} else {
		held := map[int64]bool{}
		for _, row := range reports {
			if held[row.ThreadID] {
				continue // an older report of its thread has not reached the supervisor
			}
			placed, err := s.sendReport(ctx, row)
			if err != nil {
				failures = append(failures, fmt.Errorf("report %s: %w", row.Key, err))
			}
			if !placed {
				held[row.ThreadID], done = true, false
			}
		}
	}
	if answers, err := s.store.UndeliveredAnswers(ctx); err != nil {
		failures = append(failures, err)
	} else {
		for _, row := range answers {
			placed, err := s.deliver(ctx, row)
			if err != nil {
				failures = append(failures, fmt.Errorf("answer to A%d: %w", row.ID, err))
			}
			done = done && placed
		}
	}
	return done && len(failures) == 0, errors.Join(failures...)
}

// sendReports sends thread's unsent reports in the order they were filed,
// up to the first that has not reached the supervisor, so it never gets an
// older report after a newer one. done says all of them reached it.
func (s *Server) sendReports(ctx context.Context, thread int64) (done bool, err error) {
	reports, err := s.store.UnsentReportsOf(ctx, thread)
	if err != nil {
		return false, err
	}
	for _, row := range reports {
		if placed, err := s.sendReport(ctx, row); err != nil || !placed {
			return false, err
		}
	}
	return true, nil
}

// sendReport brings a report's message to the supervisor's session
// (place), and notes it sent once it is in the transcript.
func (s *Server) sendReport(ctx context.Context, row store.ReportRow) (bool, error) {
	if row.SentAt != nil {
		return true, nil
	}
	placed, err := s.place(ctx, outgoing{session: row.SessionID, key: row.Key + ":report", text: row.Text, submission: row.Submission, attempt: row.Attempt},
		func(sub int64) error { return s.store.ReportSubmitted(ctx, row.Key, sub) },
		func() error { return s.store.ReportWithdrawn(ctx, row.Key) })
	if err != nil || !placed {
		return false, err
	}
	return true, s.store.ReportSent(ctx, row.Key)
}

// deliver brings an answer to its asking session (place), and notes it
// delivered once it is in the transcript.
func (s *Server) deliver(ctx context.Context, row store.AskRow) (bool, error) {
	key := "ask:" + strconv.FormatInt(row.ID, 10) + ":answer"
	placed, err := s.place(ctx, outgoing{session: row.SessionID, key: key, text: team.AnswerText(row.ID, row.Question, row.Answer),
		submission: row.Submission, attempt: row.Attempt},
		func(sub int64) error { return s.store.AnswerSubmitted(ctx, row.ID, sub) },
		func() error { return s.store.AnswerWithdrawn(ctx, row.ID) })
	if err != nil || !placed {
		return false, err
	}
	return true, s.store.AnswerDelivered(ctx, row.ID)
}

// outgoing is a message pocketd owes a session: an answer or a report.
type outgoing struct {
	session    int64
	key, text  string
	submission int64
	attempt    int
}

// place brings m into its session's transcript and says whether it is
// there. A submission is not delivery: a busy session queues it, and an
// abort withdraws what it queued. So a message counts as delivered only
// once its entry is written; one still queued is looked at again in the
// next pass, and one withdrawn is submitted again, under a key of its own
// (an attempt's), since its first key is spent.
func (s *Server) place(ctx context.Context, m outgoing, submitted func(int64) error, withdrawn func() error) (bool, error) {
	if m.submission != 0 {
		where, err := s.engine.Placed(ctx, m.submission)
		if err != nil {
			return false, err
		}
		switch where {
		case PlacementPlaced:
			return true, nil
		case PlacementQueued:
			return false, nil
		}
		if err := withdrawn(); err != nil {
			return false, err
		}
		m.attempt++
	}
	key := m.key
	if m.attempt > 0 {
		key += ":" + strconv.Itoa(m.attempt)
	}
	sub, err := s.engine.Send(ctx, m.session, key, m.text)
	if err != nil {
		return false, err
	}
	if err := submitted(sub); err != nil {
		return false, err
	}
	where, err := s.engine.Placed(ctx, sub)
	return where == PlacementPlaced, err
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
	if s.stopping() {
		return
	}
	done, err := s.deliverAll(context.Background())
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
	closed, err := s.changeThread(func() (store.ThreadRow, error) {
		return s.store.CloseThreadAs(ctx, requestKey(r, request.RequestID), id)
	})
	switch {
	case errors.Is(err, store.ErrRequestReused):
		reused(w)
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, proto.ErrorCodeNotFound, fmt.Sprintf("no thread %d", id))
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
	s.workMu.Lock()
	row, replayed, err := s.store.AnswerAsk(ctx, requestKey(r, request.RequestID), id, text, deviceOf(r).Name)
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
	if placed, err := s.deliver(ctx, row); err != nil || !placed {
		if err != nil {
			s.log.Warn("send answer", "ask", row.ID, "err", err)
		}
		s.deliverLater()
	}
	writeJSON(w, http.StatusOK, wireAsk(row))
}
