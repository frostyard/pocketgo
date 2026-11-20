package api

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/frostyard/pocketgo/proto"
	"github.com/frostyard/pocketgo/service/internal/profiles"
	"github.com/frostyard/pocketgo/service/internal/router"
	"github.com/frostyard/pocketgo/service/internal/store"
	"github.com/frostyard/pocketgo/service/internal/team"
)

// A thread's worker (ADR-0021): its specialist gives it work with
// team_worker, on a cheaper model, in the thread's folder, and its replies
// come back to the specialist once each, through the outbox.

// workerKey is the request key of thread's worker session: by the thread's
// random owner, never its row number (AGENTS.md: a key that crosses the two
// databases).
func workerKey(thread store.ThreadRow) string { return "worker:" + thread.Owner }

// TeamWorker runs team_worker for session call.Session: a thread's
// specialist sends w's message to its thread's worker, which the first call
// starts. The message is keyed by the call, so a call run again after a
// restart sends nothing twice.
func (s *Server) TeamWorker(ctx context.Context, call TeamCall, w team.WorkerMessage) (string, error) {
	thread, err := s.store.ThreadOfSession(ctx, call.Session)
	if errors.Is(err, store.ErrNotFound) {
		return "", errors.New("team_worker is for a thread's specialist")
	} else if err != nil {
		return "", err
	}
	switch {
	case thread.ReviewPull != 0:
		return "", errors.New("a cross-review has no worker: it reads its change set and files its verdict")
	case thread.State == store.ThreadClosed:
		return "", fmt.Errorf("thread T%d is closed: its worker takes no work", thread.ID)
	}
	if err := team.CheckWorkerMessage(w); err != nil {
		return "", err
	}
	worker, started := thread.WorkerSession, false
	if worker == 0 {
		if worker, err = s.startWorker(ctx, thread); err != nil {
			return "", err
		}
		started = true
	}
	row, err := s.store.Session(ctx, worker)
	if err != nil {
		return "", err
	}
	if _, err := s.engine.Send(ctx, worker, call.key()+":worker", strings.TrimSpace(w.Message)); err != nil {
		return "", fmt.Errorf("send to the worker: %w", err)
	}
	return team.WorkerSent(thread.ID, worker, row.Provider+"/"+row.Model, started), nil
}

// startWorker starts thread's worker: on the worker model the router
// chooses for the thread's specialist's profile (router.Request.Worker),
// in the thread's folder, with the profile's coding tools. Its session and
// the thread's naming of it commit together (store.RecordWorkerSession).
func (s *Server) startWorker(ctx context.Context, thread store.ThreadRow) (int64, error) {
	if thread.Session == 0 || thread.Cwd == "" {
		return 0, fmt.Errorf("thread T%d has no folder yet", thread.ID)
	}
	var profile *profiles.Profile
	if s.profiles != nil {
		if p, err := s.profiles.Find(thread.Agent); err == nil {
			profile = &p
		}
	}
	models, err := s.engine.WorkerModels(ctx)
	if err != nil {
		return 0, err
	}
	choice, err := router.Choose(router.Request{Worker: true, Profile: profile}, models)
	if err != nil {
		return 0, fmt.Errorf("%w. Stop, and tell the owner in your report", err)
	}
	spec := SessionSpec{Title: "T" + strconv.FormatInt(thread.ID, 10) + " · worker", Cwd: thread.Cwd, Model: choice.Model,
		Route: string(choice.Rule), Thread: thread.ID, Worker: true}
	if profile != nil {
		spec.WorkerTools = profile.Tools
	}
	key := workerKey(thread)
	id, err := s.engine.CreateSession(ctx, key, spec)
	if err != nil {
		return 0, err
	}
	s.workMu.Lock()
	row, err := s.store.RecordWorkerSession(ctx, key, store.SessionRow{ID: id, Title: spec.Title, Cwd: spec.Cwd, Provider: spec.Model.Provider,
		Model: spec.Model.ID, Route: spec.Route, CreatedBy: teamCreator}, thread.ID)
	if err == nil {
		s.publishThread(row)
	}
	s.workMu.Unlock()
	if errors.Is(err, store.ErrConflict) {
		return 0, fmt.Errorf("thread T%d closed while its worker started", thread.ID)
	} else if err != nil {
		return 0, err
	}
	s.publishSession(id, true)
	// The watch starts before the brief goes, so the worker's entries
	// reach the event stream.
	if err := s.engine.Watch(ctx, id); err != nil {
		return 0, fmt.Errorf("watch the worker's session: %w", err)
	}
	return id, nil
}

// workerRunEnded follows a worker's run that ended: its last reply goes to
// its thread's specialist, once, keyed by the reply's entry, through the
// outbox. A closed thread's worker gets its run stopped instead: a message
// queued before the close may have started one.
func (s *Server) workerRunEnded(ctx context.Context, thread store.ThreadRow, worker int64) {
	if thread.State == store.ThreadClosed {
		s.stopWorker(ctx, worker)
		return
	}
	if thread.Session == 0 {
		return
	}
	entries, _, err := s.engine.Transcript(ctx, worker)
	if err != nil {
		s.log.Warn("read the worker's reply", "thread", thread.ID, "session", worker, "err", err)
		return
	}
	last, ok := lastReply(entries)
	if !ok {
		return
	}
	queued, err := s.store.QueueWorkerReply(ctx, store.ReportRow{Key: "worker-reply:" + thread.Owner + ":" + strconv.FormatInt(last.ID, 10),
		ThreadID: thread.ID, SessionID: thread.Session, Text: team.WorkerReplyText(thread.ID, replyText(last))})
	if err != nil {
		s.log.Warn("keep the worker's reply", "thread", thread.ID, "err", err)
		return
	}
	if !queued {
		return
	}
	// Sent now, as a report is; one that cannot go now is tried again
	// until it does.
	if done, err := s.sendReports(ctx, thread.ID); err != nil || !done {
		if err != nil {
			s.log.Warn("send the worker's reply", "thread", thread.ID, "err", err)
		}
		s.deliverLater()
	}
}

// lastReply is a transcript's last entry when it is the model's: the reply
// a run ended with. A transcript that ends otherwise has no reply yet.
func lastReply(entries []proto.Entry) (proto.Entry, bool) {
	if len(entries) == 0 || entries[len(entries)-1].Kind != proto.EntryAssistant {
		return proto.Entry{}, false
	}
	return entries[len(entries)-1], true
}

// replyText is a reply's text, or what ended its run with none.
func replyText(e proto.Entry) string {
	if text := strings.TrimSpace(e.Text); text != "" {
		return text
	}
	if e.Error != "" {
		return "The worker's run failed: " + e.Error
	}
	if e.StopReason != "" {
		return "The worker's run ended (" + e.StopReason + ") with no reply."
	}
	return ""
}

// stopWorker stops worker's run, if one goes: its thread closed. A stop
// that fails is logged; the run's reply then reaches no one.
func (s *Server) stopWorker(ctx context.Context, worker int64) {
	busy, err := s.engine.Busy(ctx, worker)
	if err != nil || !busy {
		if err != nil {
			s.log.Warn("look at a closed thread's worker", "session", worker, "err", err)
		}
		return
	}
	if err := s.engine.Abort(ctx, worker); err != nil {
		s.log.Warn("stop a closed thread's worker", "session", worker, "err", err)
	}
}

// settleWorkers sends each open thread's worker's last reply that a stop
// left unsent, at the service's start: a run that ended while pocketd was
// down has no end to follow.
func (s *Server) settleWorkers(ctx context.Context) {
	threads, err := s.store.ThreadsOnDesk(ctx)
	if err != nil {
		s.log.Warn("read the threads' workers", "err", err)
		return
	}
	for _, t := range threads {
		if t.WorkerSession == 0 {
			continue
		}
		if busy, err := s.engine.Busy(ctx, t.WorkerSession); err != nil || busy {
			continue
		}
		s.workerRunEnded(ctx, t, t.WorkerSession)
	}
}
