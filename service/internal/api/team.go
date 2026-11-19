package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/frostyard/pocketgo/proto"
	"github.com/frostyard/pocketgo/service/internal/profiles"
	"github.com/frostyard/pocketgo/service/internal/router"
	"github.com/frostyard/pocketgo/service/internal/store"
	"github.com/frostyard/pocketgo/service/internal/team"
	"github.com/frostyard/pocketgo/service/internal/worktree"
)

// TeamCall names one call of a team tool (ADR-0014): the calling session
// and the call's id. Its key makes the call idempotent: a run of the same
// call after a restart finds what the first run did.
type TeamCall struct {
	Session int64
	CallID  string
}

func (c TeamCall) key() string { return "team:" + strconv.FormatInt(c.Session, 10) + ":" + c.CallID }

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

// TeamDelegate runs team_delegate for session call.Session: it checks the
// rules, records the thread, makes its folder, starts the specialist's
// session and sends it the brief, all in code. The text is what the calling
// agent sees; so is an error, a refusal or a failure.
func (s *Server) TeamDelegate(ctx context.Context, call TeamCall, d team.Delegation) (string, error) {
	ctx = context.WithoutCancel(ctx)
	key := call.key()
	thread, err := s.store.ThreadByKey(ctx, key)
	if errors.Is(err, store.ErrNotFound) {
		thread, err = s.newThread(ctx, call, key, d)
	}
	if err != nil {
		return "", err
	}
	if thread.State == store.ThreadClosed && thread.Session == 0 {
		return "", fmt.Errorf("thread T%d could not start: %s", thread.ID, thread.Report)
	}
	started, err := s.startThread(ctx, thread)
	if err != nil {
		s.log.Warn("start thread", "thread", thread.ID, "err", err)
		failed, ferr := s.store.FailThread(ctx, thread.ID, err.Error())
		if ferr != nil {
			return "", fmt.Errorf("thread T%d could not start: %w", thread.ID, err)
		}
		s.publishThread(failed)
		if failed.State == store.ThreadClosed && failed.Session == 0 {
			return "", fmt.Errorf("thread T%d could not start: %w", thread.ID, err)
		}
		return "", fmt.Errorf("thread T%d started in session %d, but: %w", thread.ID, failed.Session, err)
	}
	name := s.agentName(started.Agent)
	where := "in " + started.Cwd
	if started.Branch != "" {
		where += " on the branch " + started.Branch
	}
	return fmt.Sprintf("Delegated to %s as thread T%d: %s. %s works %s, in session %d. Reports come to you as messages that start with [T%d · %s · <status>].",
		name, started.ID, started.Title, name, where, started.Session, started.ID, name), nil
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
	if d.Repository == "" && target.Start() == "" {
		return store.ThreadRow{}, fmt.Errorf("%s has no home or workspace to work in: name one of its repositories", target.ID)
	}
	row, _, err := s.store.CreateThread(ctx, store.ThreadRow{Key: key, Title: team.Title(d), Agent: target.ID,
		SupervisorSession: call.Session, Repository: d.Repository, Task: strings.TrimSpace(d.Task)})
	if err != nil {
		return store.ThreadRow{}, err
	}
	s.publishThread(row)
	return row, nil
}

// startThread finishes a thread's start, each step idempotent, so a run
// after a restart picks up where the first stopped: the folder, the
// specialist's session, the thread's opening, then the brief. The thread
// opens before the brief goes, so the specialist's first report finds it.
func (s *Server) startThread(ctx context.Context, thread store.ThreadRow) (store.ThreadRow, error) {
	sessionKey := "thread:" + strconv.FormatInt(thread.ID, 10)
	target, err := s.profiles.Find(thread.Agent)
	if err != nil {
		return store.ThreadRow{}, err
	}
	cwd, branch := thread.Cwd, thread.Branch
	if thread.Session == 0 {
		cwd, branch, err = s.threadFolder(ctx, thread, target, sessionKey)
		if err != nil {
			return store.ThreadRow{}, err
		}
	}
	id, err := s.threadSession(ctx, thread, target, sessionKey, cwd)
	if err != nil {
		return store.ThreadRow{}, err
	}
	started, err := s.store.StartThread(ctx, thread.ID, id, cwd, branch)
	if err != nil {
		return store.ThreadRow{}, err
	}
	s.publishThread(started)
	supervisor := "The supervisor"
	if p, err := s.callerProfile(ctx, thread.SupervisorSession); err == nil && p != nil {
		supervisor = p.Name
	}
	brief := team.Brief(supervisor, thread.ID, thread.Title, thread.Task, started.Cwd, started.Branch)
	if _, err := s.engine.Send(ctx, id, sessionKey+":brief", brief); err != nil {
		return store.ThreadRow{}, fmt.Errorf("send the brief: %w", err)
	}
	return started, nil
}

// threadFolder is where a thread works: a new worktree of its repository,
// or the specialist's home. A session made for it already keeps its folder.
func (s *Server) threadFolder(ctx context.Context, thread store.ThreadRow, target profiles.Profile, sessionKey string) (cwd, branch string, err error) {
	slug := team.Slug(thread.Title, thread.ID)
	if made, exists, err := s.engine.Created(ctx, sessionKey); err != nil {
		return "", "", err
	} else if exists {
		if thread.Repository != "" {
			_, branch = team.Worktree(made.Cwd, thread.Agent, slug)
		}
		return made.Cwd, branch, nil
	}
	if thread.Repository != "" {
		checkout, ok := target.Repositories[thread.Repository]
		if !ok {
			return "", "", fmt.Errorf("%s no longer names the repository %s", target.ID, thread.Repository)
		}
		cwd, branch = team.Worktree(checkout, target.ID, slug)
		if err := worktree.Add(ctx, s.engine.Git, checkout, cwd, branch); err != nil {
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

// threadSession starts the specialist's session for a thread, from its
// profile with the router's model, and records it as createSession does.
func (s *Server) threadSession(ctx context.Context, thread store.ThreadRow, target profiles.Profile, key, cwd string) (int64, error) {
	made, exists, err := s.engine.Created(ctx, key)
	if err != nil {
		return 0, err
	}
	var spec SessionSpec
	profile := target.ID
	if exists {
		spec = SessionSpec{Title: made.Title, Cwd: made.Cwd, Model: made.Model, Thinking: made.Thinking, Route: made.Route}
		profile = made.Profile
	} else {
		models, err := s.engine.Models(ctx)
		if err != nil {
			return 0, err
		}
		choice, err := router.Choose(router.Request{Profile: &target}, models)
		if err != nil {
			return 0, err
		}
		spec = SessionSpec{Title: "T" + strconv.FormatInt(thread.ID, 10) + " · " + thread.Title, Cwd: cwd, Model: choice.Model,
			Profile: &target, Route: string(choice.Rule), Thread: thread.ID}
	}
	id, err := s.engine.CreateSession(ctx, key, spec)
	if err != nil {
		return 0, err
	}
	return id, s.recordSession(ctx, key, id, spec, profile, teamCreator)
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
// specialist's report on its thread, puts its asks in the owner's Inbox,
// and sends the report to the supervisor's session.
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
	if thread.State == store.ThreadClosed {
		return "", fmt.Errorf("thread T%d is closed and takes no more reports", thread.ID)
	}
	key := call.key()
	var asks []int64
	for i, a := range r.Asks {
		options := make([]string, len(a.Options))
		for j, o := range a.Options {
			options[j] = strings.TrimSpace(o)
		}
		row, err := s.store.CreateAsk(ctx, store.AskRow{Key: key + ":ask:" + strconv.Itoa(i), ThreadID: thread.ID, SessionID: call.Session,
			Question: strings.TrimSpace(a.Question), Options: options})
		if err != nil {
			return "", err
		}
		s.publishAsk(row)
		asks = append(asks, row.ID)
	}
	updated, err := s.store.ReportThread(ctx, thread.ID, team.ThreadState(r.Status), strings.TrimSpace(r.Message))
	if errors.Is(err, store.ErrConflict) {
		return "", fmt.Errorf("thread T%d is closed and takes no more reports", thread.ID)
	} else if err != nil {
		return "", err
	}
	s.publishThread(updated)
	text := team.ReportText(thread.ID, s.agentName(thread.Agent), r.Status, r.Message, asks)
	if _, err := s.engine.Send(ctx, thread.SupervisorSession, key+":report", text); err != nil {
		return "", fmt.Errorf("send the report to the supervisor: %w", err)
	}
	if len(asks) == 0 {
		return fmt.Sprintf("Reported on thread T%d to the supervisor.", thread.ID), nil
	}
	ids := make([]string, len(asks))
	for i, id := range asks {
		ids[i] = "A" + strconv.FormatInt(id, 10)
	}
	return fmt.Sprintf("Reported on thread T%d to the supervisor. %s wait for the owner; each answer comes to this session as a message.",
		thread.ID, strings.Join(ids, ", ")), nil
}

// TeamClose runs team_close for session call.Session: a supervisor closes
// a thread that a session of its own profile delegated.
func (s *Server) TeamClose(ctx context.Context, call TeamCall, id int64) (string, error) {
	ctx = context.WithoutCancel(ctx)
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
	closed, err := s.closeThread(ctx, id)
	if err != nil {
		return "", err
	}
	onDesk, err := s.store.OpenThreads(ctx)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("Closed thread T%d. %d of the desk's %d places are taken.", closed.ID, onDesk, s.deskCap), nil
}

func (s *Server) closeThread(ctx context.Context, id int64) (store.ThreadRow, error) {
	closed, err := s.store.CloseThread(ctx, id)
	if err != nil {
		return store.ThreadRow{}, err
	}
	s.publishThread(closed)
	return closed, nil
}

// DeliverAnswers sends each answer that has not reached its asking session
// yet; the service calls it once at start, after WatchAll. An answer the
// owner gave just before a stop arrives after the restart.
func (s *Server) DeliverAnswers(ctx context.Context) error {
	waiting, err := s.store.UndeliveredAnswers(ctx)
	if err != nil {
		return err
	}
	for _, row := range waiting {
		if err := s.deliver(ctx, row); err != nil {
			return fmt.Errorf("answer to A%d: %w", row.ID, err)
		}
	}
	return nil
}

// deliver sends an answer to its asking session, once: the submission's
// key is the ask's.
func (s *Server) deliver(ctx context.Context, row store.AskRow) error {
	key := "ask:" + strconv.FormatInt(row.ID, 10) + ":answer"
	if _, err := s.engine.Send(ctx, row.SessionID, key, team.AnswerText(row.ID, row.Question, row.Answer)); err != nil {
		return err
	}
	return s.store.AnswerDelivered(ctx, row.ID)
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
// changes nothing.
func (s *Server) ownerCloseThread(w http.ResponseWriter, r *http.Request) {
	id, ok := pathIDOf(w, r, "thread")
	if !ok {
		return
	}
	var request proto.CloseThread
	if !decode(w, r, &request) || !validRequestID(w, request.RequestID) {
		return
	}
	if _, err := s.store.Thread(r.Context(), id); errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, proto.ErrorCodeNotFound, fmt.Sprintf("no thread %d", id))
		return
	} else if err != nil {
		s.internal(w, "read thread", err)
		return
	}
	key := requestKey(r, request.RequestID)
	kind := "closeThread:" + strconv.FormatInt(id, 10)
	if _, _, err := s.store.LookupRequest(r.Context(), key, kind); errors.Is(err, store.ErrRequestReused) {
		reused(w)
		return
	} else if err != nil {
		s.internal(w, "read request", err)
		return
	}
	ctx := context.WithoutCancel(r.Context())
	closed, err := s.closeThread(ctx, id)
	if err != nil {
		s.internal(w, "close thread", err)
		return
	}
	if err := s.store.RecordRequest(ctx, key, kind, "closed"); err != nil {
		s.internal(w, "record request", err)
		return
	}
	writeJSON(w, http.StatusOK, wireThread(closed))
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
// A retry with the same request id sends it again, which the submission's
// key makes a no-op once it went.
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
	row, replayed, err := s.store.AnswerAsk(ctx, requestKey(r, request.RequestID), id, text, deviceOf(r).Name)
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
	if !replayed {
		s.publishAsk(row)
	}
	if err := s.deliver(ctx, row); err != nil {
		s.internal(w, "send the answer (retry with the same requestId)", err)
		return
	}
	writeJSON(w, http.StatusOK, wireAsk(row))
}
