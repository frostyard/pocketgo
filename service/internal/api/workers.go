package api

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/frostyard/pocketgo/proto"
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
	if err := s.sendWorker(ctx, thread.ID, worker, call.key()+":worker", strings.TrimSpace(w.Message)); err != nil {
		return "", err
	}
	return team.WorkerSent(thread.ID, worker, row.Provider+"/"+row.Model, started), nil
}

// sendWorker sends text to thread's worker, keyed by key, under outboxMu,
// which a close holds as it commits: a thread that closed refuses it, so
// no run starts after a close (Codex review of #29, round 1). The worker
// is watched first, every time: a watch an earlier call could not start
// starts now, so its replies reach the specialist (Watch is idempotent).
func (s *Server) sendWorker(ctx context.Context, thread, worker int64, key, text string) error {
	s.outboxMu.Lock()
	defer s.outboxMu.Unlock()
	current, err := s.store.Thread(ctx, thread)
	if err != nil {
		return err
	}
	if current.State == store.ThreadClosed || current.WorkerSession != worker {
		return fmt.Errorf("thread T%d is closed: its worker takes no work", thread)
	}
	if err := s.engine.Watch(ctx, worker); err != nil {
		return fmt.Errorf("watch the worker's session: %w", err)
	}
	if _, err := s.engine.Send(ctx, worker, key, text); err != nil {
		return fmt.Errorf("send to the worker: %w", err)
	}
	return nil
}

// startWorker starts thread's worker: on the worker model the router
// chooses for the thread's specialist's profile (router.Request.Worker),
// in the thread's folder, with the profile's coding tools. Its session and
// the thread's naming of it commit together (store.RecordWorkerSession).
func (s *Server) startWorker(ctx context.Context, thread store.ThreadRow) (int64, error) {
	if thread.Session == 0 || thread.Cwd == "" {
		return 0, fmt.Errorf("thread T%d has no folder yet", thread.ID)
	}
	key := workerKey(thread)
	spec := SessionSpec{Title: "T" + strconv.FormatInt(thread.ID, 10) + " · worker", Cwd: thread.Cwd, Thread: thread.ID, Worker: true}
	// A worker an earlier run made is recorded as it was made, whatever
	// the profile and the sign-ins say now (Codex review of #29, round 1).
	made, exists, err := s.engine.Created(ctx, key)
	if err != nil {
		return 0, err
	}
	if exists {
		spec.Model, spec.Route = made.Model, made.Route
	} else {
		// Its tools are its specialist's profile's: a profile pocketd
		// cannot read now starts no worker, rather than one with every
		// tool (Codex review of #29, round 1).
		if s.profiles == nil {
			return 0, errors.New("pocketd reads no profiles, so it cannot tell your worker's tools")
		}
		p, err := s.profiles.Find(thread.Agent)
		if err != nil {
			return 0, fmt.Errorf("pocketd cannot read your profile, so it cannot tell your worker's tools: %w", err)
		}
		profile := &p
		models, err := s.engine.WorkerModels(ctx)
		if err != nil {
			return 0, err
		}
		choice, err := router.Choose(router.Request{Worker: true, Profile: profile}, models)
		if err != nil {
			return 0, fmt.Errorf("%w. Stop, and tell the owner in your report", err)
		}
		spec.Model, spec.Route, spec.WorkerTools = choice.Model, string(choice.Rule), profile.Tools
	}
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
	return id, nil
}

// workerRunEnded follows a worker's run that ended: each reply its runs
// ended with goes to its thread's specialist, once, keyed by the reply's
// entry, through the outbox. A run that a queued message followed at once
// leaves no idle moment between them, so the whole transcript is read,
// not only its end (Codex review of #29, round 1). What cannot be read or
// kept now is tried again by the delivery pass (workersDue). A closed
// thread's worker gets its run stopped instead: a message queued before
// the close may have started one.
func (s *Server) workerRunEnded(ctx context.Context, thread store.ThreadRow, worker int64) {
	if thread.State == store.ThreadClosed {
		s.stopWorker(ctx, worker)
		return
	}
	if thread.Session == 0 {
		return
	}
	if err := s.queueWorkerReplies(ctx, thread, worker); err != nil {
		s.log.Warn("keep the worker's replies", "thread", thread.ID, "session", worker, "err", err)
		s.workerDue(worker)
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

// queueWorkerReplies keeps each reply of worker's transcript for thread's
// specialist, once by its entry (store.QueueWorkerReply).
func (s *Server) queueWorkerReplies(ctx context.Context, thread store.ThreadRow, worker int64) error {
	busy, err := s.engine.Busy(ctx, worker)
	if err != nil {
		return err
	}
	entries, live, err := s.engine.Transcript(ctx, worker)
	if err != nil {
		return err
	}
	for _, r := range workerReplies(entries, live, !busy) {
		if _, err := s.store.QueueWorkerReply(ctx, store.ReportRow{Key: workerReplyPrefix + thread.Owner + ":" + strconv.FormatInt(r.entry, 10),
			ThreadID: thread.ID, SessionID: thread.Session, Text: team.WorkerReplyText(thread.ID, r.text)}); err != nil {
			return err
		}
	}
	return nil
}

// workerReplyPrefix begins the key of a worker's reply among a thread's
// reports.
const workerReplyPrefix = "worker-reply:"

// cancelReport withdraws every attempt of report row still queued, and
// notes it sent: it goes to no one. outboxMu is held.
func (s *Server) cancelReport(ctx context.Context, row store.ReportRow) error {
	for attempt := 0; attempt <= row.Attempt; attempt++ {
		if _, err := s.engine.Withdraw(ctx, row.SessionID, attemptKey(row.Key+":report", attempt)); err != nil {
			return fmt.Errorf("withdraw the worker's reply: %w", err)
		}
	}
	return s.store.ReportSent(ctx, row.Key)
}

// cancelWorkerReplies cancels thread's worker replies not yet sent: its
// close overtook them (Codex review of #29, round 1). outboxMu is held.
func (s *Server) cancelWorkerReplies(ctx context.Context, thread int64) error {
	rows, err := s.store.UnsentReportsOf(ctx, thread)
	if err != nil {
		return err
	}
	for _, row := range rows {
		if strings.HasPrefix(row.Key, workerReplyPrefix) {
			if err := s.cancelReport(ctx, row); err != nil {
				return err
			}
		}
	}
	return nil
}

// workerReply is a reply a worker's run ended with, by the entry that
// names it.
type workerReply struct {
	entry int64
	text  string
}

// workerReplies are the replies a worker's transcript holds, in order:
//   - a model's entry that the next message follows, which ended its run;
//   - the last entry, when it is the model's, the worker is idle, and the
//     model stopped for no tool;
//   - a message that the next message follows, or that ends an idle
//     worker's transcript, with no reply of the model's after it: its run
//     ended with none, as a failure before the model answered; the last
//     one says the live failure (Codex review of #29, round 1).
func workerReplies(entries []proto.Entry, live proto.Live, idle bool) []workerReply {
	var out []workerReply
	for i, e := range entries {
		last := i == len(entries)-1
		nextUser := !last && entries[i+1].Kind == proto.EntryUser
		switch e.Kind {
		case proto.EntryAssistant:
			if nextUser || (last && idle && e.StopReason != stopToolUse) {
				out = append(out, workerReply{entry: e.ID, text: replyText(e)})
			}
		case proto.EntryUser:
			switch {
			case nextUser:
				out = append(out, workerReply{entry: e.ID, text: "The worker's run ended with no reply."})
			case last && idle:
				why := strings.TrimSpace(live.Failure)
				if why == "" {
					why = "no reason was given"
				}
				out = append(out, workerReply{entry: e.ID, text: "The worker's run ended with no reply: " + why + "."})
			}
		}
	}
	return out
}

// stopToolUse is the stop reason of a model's entry that a tool call
// ends: its run goes on (PiG's ai.StopReasonToolUse).
const stopToolUse = "toolUse"

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

// workerDue keeps worker for the delivery pass to read again.
func (s *Server) workerDue(worker int64) {
	s.workerMu.Lock()
	if s.workersDue == nil {
		s.workersDue = map[int64]bool{}
	}
	s.workersDue[worker] = true
	s.workerMu.Unlock()
	s.deliverLater()
}

// followDueWorkers reads again each worker whose replies, or whose stop,
// could not be done: done says none is left.
func (s *Server) followDueWorkers(ctx context.Context) bool {
	s.workerMu.Lock()
	due := make([]int64, 0, len(s.workersDue))
	for worker := range s.workersDue {
		due = append(due, worker)
	}
	s.workersDue = nil
	s.workerMu.Unlock()
	for _, worker := range due {
		thread, err := s.store.ThreadOfWorker(ctx, worker)
		if err != nil {
			s.workerDue(worker)
			continue
		}
		s.workerRunEnded(ctx, thread, worker)
	}
	s.workerMu.Lock()
	defer s.workerMu.Unlock()
	return len(s.workersDue) == 0
}

// stopWorker stops worker's run, if one goes: its thread closed. A stop,
// or a look at the worker, that fails is tried again by the delivery pass
// (Codex review of #29, round 1); the run's reply reaches no one.
func (s *Server) stopWorker(ctx context.Context, worker int64) {
	busy, err := s.engine.Busy(ctx, worker)
	if err == nil && busy {
		err = s.engine.Abort(ctx, worker)
	}
	if err != nil {
		s.log.Warn("stop a closed thread's worker", "session", worker, "err", err)
		s.workerDue(worker)
	}
}

// settleWorkers, at the service's start, sends each open thread's
// worker's replies that a stop left unsent, and stops each closed thread's
// worker a stop left running: a run that ended, or a close that committed,
// while pocketd was down has no end to follow (Codex review of #29, round
// 1). It runs off the start's path, as a delivery pass does, whose retries
// take what fails.
func (s *Server) settleWorkers() {
	run := func() {
		ctx := s.passCtx
		threads, err := s.store.ThreadsWithWorkers(ctx)
		if err != nil {
			s.log.Warn("read the threads' workers", "err", err)
			return
		}
		for _, t := range threads {
			s.workerRunEnded(ctx, t, t.WorkerSession)
		}
	}
	if !s.reviewAsync {
		run()
		return
	}
	s.sessionMu.Lock()
	if s.stopped {
		s.sessionMu.Unlock()
		return
	}
	s.passes.Add(1)
	s.sessionMu.Unlock()
	go func() {
		defer s.passes.Done()
		run()
	}()
}
