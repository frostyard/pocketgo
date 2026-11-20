package api

import (
	"context"
	"errors"
	"fmt"
	"slices"
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
	// The worker starts, and the message is kept for it, one call at a
	// time: the brief that starts it is first in its queue (Codex review of
	// #29, round 3).
	s.workerStartMu.Lock()
	worker, started, err := s.workerOf(ctx, thread.ID)
	if err == nil {
		err = s.queueWorkerMessage(ctx, thread.ID, worker, workerMessagePrefix+call.key(), strings.TrimSpace(w.Message))
	}
	s.workerStartMu.Unlock()
	if err != nil {
		return "", err
	}
	row, err := s.store.Session(ctx, worker)
	if err != nil {
		return "", err
	}
	// It goes through the outbox, as a report does: one a failed run left
	// in the worker's queue goes again, and it counts as sent only once it
	// is in the worker's transcript (Codex review of #29, round 3).
	if done, err := s.sendReports(ctx, thread.ID); err != nil || !done {
		if err != nil {
			s.log.Warn("send to the worker", "thread", thread.ID, "err", err)
		}
		s.deliverLater()
	}
	return team.WorkerSent(thread.ID, worker, row.Provider+"/"+row.Model, started), nil
}

// workerOf is thread id's worker, which it starts when it has none.
// workerStartMu is held, so two first calls of one specialist, which PiG
// runs side by side, start one worker (Codex review of #29, round 2).
// started says this call started it.
func (s *Server) workerOf(ctx context.Context, id int64) (worker int64, started bool, err error) {
	thread, err := s.store.Thread(ctx, id)
	if err != nil {
		return 0, false, err
	}
	if thread.WorkerSession != 0 {
		return thread.WorkerSession, false, nil
	}
	if thread.State == store.ThreadClosed {
		return 0, false, fmt.Errorf("thread T%d is closed: its worker takes no work", id)
	}
	worker, err = s.startWorker(ctx, thread)
	return worker, err == nil, err
}

// workerMessagePrefix begins the key of a message to a worker among its
// thread's reports.
const workerMessagePrefix = "worker-msg:"

// isWorkerReport says whether a report's key is a message between a
// thread's worker and its specialist: it goes only while the thread is
// open.
func isWorkerReport(key string) bool {
	return strings.HasPrefix(key, workerReplyPrefix) || strings.HasPrefix(key, workerMessagePrefix)
}

// queueWorkerMessage keeps text for thread's worker, by key, under
// outboxMu, which every close holds as it commits: a thread that closed
// refuses it, so no message to a worker is kept, or sent, after a close
// (Codex review of #29, round 1). The worker is watched first, every time:
// a watch an earlier call could not start starts now, so its replies reach
// the specialist (Watch is idempotent).
func (s *Server) queueWorkerMessage(ctx context.Context, thread, worker int64, key, text string) error {
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
	queued, err := s.store.QueueWorkerReport(ctx, store.ReportRow{Key: key, ThreadID: thread, SessionID: worker, Text: text})
	if err != nil {
		return err
	}
	if !queued {
		return fmt.Errorf("thread T%d is closed: its worker takes no work", thread)
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
// specialist, once by its entry (store.QueueWorkerReply). Whether the
// worker is at work is the transcript's own word, of the same revision as
// its entries (Codex review of #29, round 2).
func (s *Server) queueWorkerReplies(ctx context.Context, thread store.ThreadRow, worker int64) error {
	entries, live, err := s.engine.Transcript(ctx, worker)
	if err != nil {
		return err
	}
	// Only the runs after the cursor: each pass keeps a reply once, not
	// every reply before it again (Codex review of #29, round 3).
	replies, cursor := workerReplies(entries, live, thread.WorkerCursor)
	for _, r := range replies {
		if _, err := s.store.QueueWorkerReport(ctx, store.ReportRow{Key: workerReplyPrefix + thread.Owner + ":" + strconv.FormatInt(r.entry, 10),
			ThreadID: thread.ID, SessionID: thread.Session, Text: team.WorkerReplyText(thread.ID, r.text)}); err != nil {
			return err
		}
	}
	if cursor > thread.WorkerCursor {
		return s.store.SetWorkerCursor(ctx, thread.ID, cursor)
	}
	return nil
}

// workerReplyPrefix begins the key of a worker's reply among a thread's
// reports.
const workerReplyPrefix = "worker-reply:"

// workerReply is a reply a worker's run ended with, by the entry that
// names it.
type workerReply struct {
	entry int64
	text  string
}

// workerReplies are the replies of the runs a worker's transcript holds,
// in order. A run begins at a message and ends at the next, or at the
// transcript's end once the worker is idle (live.Busy false); one still
// at work has none yet. A run that ended with the model's entry, its stop
// for no tool, replies with that entry. Any other run that ended, as one
// stopped in a tool call or failed before the model answered, replies
// that it ended with none, by its message's entry; the last says the live
// failure (Codex review of #29, rounds 1 and 2).
//
// Only the runs begun after after count, those whose message's entry is
// later; cursor is the entry that began the last run that ended, or after.
func workerReplies(entries []proto.Entry, live proto.Live, after int64) (out []workerReply, cursor int64) {
	cursor = after
	start := -1
	end := func(stop int, last bool) {
		if start < 0 || entries[start].ID <= after {
			return
		}
		cursor = entries[start].ID
		final := entries[stop-1]
		switch {
		case stop-1 > start && final.Kind == proto.EntryAssistant && final.StopReason != stopToolUse:
			out = append(out, workerReply{entry: final.ID, text: replyText(final)})
		case last:
			why := strings.TrimSpace(live.Failure)
			if why == "" {
				why = "no reason was given"
			}
			out = append(out, workerReply{entry: entries[start].ID, text: "The worker's run ended with no reply: " + why + "."})
		default:
			out = append(out, workerReply{entry: entries[start].ID, text: "The worker's run ended with no reply."})
		}
	}
	for i, e := range entries {
		if e.Kind == proto.EntryUser {
			end(i, false)
			start = i
		}
	}
	if !live.Busy {
		end(len(entries), true)
	}
	return out, cursor
}

// stopToolUse is the stop reason of a model's entry that a tool call
// ends: its run goes on (PiG's ai.StopReasonToolUse).
const stopToolUse = "toolUse"

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

// withdrawWorkerReplies withdraws every attempt of thread's worker
// replies still queued, before its close commits: PiG places a queued
// message whatever pocketd's locks say, so one withdrawn after the commit
// may be in the transcript already (Codex review of #29, round 2). A
// withdrawal that fails leaves the close undone, for a retry; withdrew
// says one took effect. outboxMu is held.
func (s *Server) withdrawWorkerReplies(ctx context.Context, thread int64) (withdrew bool, err error) {
	rows, err := s.store.UnsentReportsOf(ctx, thread)
	if err != nil {
		return false, err
	}
	for _, row := range rows {
		if !isWorkerReport(row.Key) {
			continue
		}
		for attempt := 0; attempt <= row.Attempt; attempt++ {
			done, err := s.engine.Withdraw(ctx, row.SessionID, attemptKey(row.Key+":report", attempt))
			if err != nil {
				return withdrew, fmt.Errorf("withdraw the worker's reply: %w", err)
			}
			withdrew = withdrew || done
		}
	}
	return withdrew, nil
}

// dropWorkerReplies notes thread's worker replies not yet sent as sent,
// once its close committed: they go to no one. outboxMu is held.
func (s *Server) dropWorkerReplies(ctx context.Context, thread int64) error {
	rows, err := s.store.UnsentReportsOf(ctx, thread)
	if err != nil {
		return err
	}
	for _, row := range rows {
		if isWorkerReport(row.Key) {
			if err := s.store.ReportSent(ctx, row.Key); err != nil {
				return err
			}
		}
	}
	return nil
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
	scan := s.workerScanDue
	s.workerScanDue = false
	s.workerMu.Unlock()
	if scan {
		s.scanWorkers(ctx)
	}
	for _, worker := range due {
		thread, err := s.store.ThreadOfWorker(ctx, worker)
		switch {
		case errors.Is(err, store.ErrNotFound):
			continue // no thread's worker: nothing is owed
		case err != nil:
			s.workerDue(worker)
			continue
		}
		s.workerRunEnded(ctx, thread, worker)
	}
	s.workerMu.Lock()
	defer s.workerMu.Unlock()
	return len(s.workersDue) == 0 && !s.workerScanDue
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
		s.stopWorkerOnlySessions(s.passCtx)
		s.scanWorkers(s.passCtx)
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

// followWorker sends the replies of session's runs to its thread's
// specialist when session is a thread's worker. A look that fails, as one
// that is not "no such worker", is tried again by the delivery pass (Codex
// review of #29, round 2).
func (s *Server) followWorker(session int64) {
	thread, err := s.store.ThreadOfWorker(s.passCtx, session)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return
	case err != nil:
		s.log.Warn("look at a session's thread", "session", session, "err", err)
		s.workerDue(session)
		return
	}
	s.workerRunEnded(s.passCtx, thread, session)
}

// scanWorkers follows every thread's worker: an open thread's replies go,
// and a closed thread's worker stops. A list it cannot read is read again
// by the delivery pass (Codex review of #29, round 2).
func (s *Server) scanWorkers(ctx context.Context) {
	threads, err := s.store.ThreadsWithWorkers(ctx)
	if err != nil {
		s.log.Warn("read the threads' workers", "err", err)
		s.workerMu.Lock()
		s.workerScanDue = true
		s.workerMu.Unlock()
		s.deliverLater()
		return
	}
	for _, t := range threads {
		s.workerRunEnded(ctx, t, t.WorkerSession)
	}
}

// workerOnlySession says whether session row, which is no thread's worker,
// runs a model that serves workers alone: one the router names so
// (MAI-Code), or one pocketd runs on an API key, which the worker models
// list and the models do not (ADR-0021).
func (s *Server) workerOnlySession(ctx context.Context, row store.SessionRow) (bool, error) {
	if row.Route == string(router.RuleWorker) || row.Route == string(router.RuleWorkerDefault) || row.Provider == "" {
		return false, nil
	}
	ref := proto.ModelRef{Provider: row.Provider, ID: row.Model}
	if router.WorkerOnly(ref) {
		return true, nil
	}
	models, err := s.engine.Models(ctx)
	if err != nil {
		return false, err
	}
	if slices.ContainsFunc(models, func(m proto.Model) bool { return m.ModelRef == ref }) {
		return false, nil
	}
	workers, err := s.engine.WorkerModels(ctx)
	if err != nil {
		return false, err
	}
	return slices.ContainsFunc(workers, func(m proto.Model) bool { return m.ModelRef == ref }), nil
}

// stopWorkerOnlySessions stops, at the service's start, each session that
// is no thread's worker and runs a model that serves workers alone: PiG
// resumes a run it had before (Codex review of #29, round 3).
func (s *Server) stopWorkerOnlySessions(ctx context.Context) {
	rows, err := s.store.Sessions(ctx)
	if err != nil {
		s.log.Warn("read the sessions", "err", err)
		return
	}
	for _, row := range rows {
		only, err := s.workerOnlySession(ctx, row)
		if err != nil {
			s.log.Warn("read a session's model", "session", row.ID, "err", err)
			continue
		}
		if !only {
			continue
		}
		if busy, err := s.engine.Busy(ctx, row.ID); err == nil && busy {
			if err := s.engine.Abort(ctx, row.ID); err != nil {
				s.log.Warn("stop a session on a worker's model", "session", row.ID, "err", err)
			}
		}
	}
}
