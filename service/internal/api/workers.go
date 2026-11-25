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
	// A closed thread's messages already in the transcript are noted
	// first, as a report notes them: the one this run acts on may reopen
	// it (ADR-0061; Codex review of #121, round 1).
	if thread.State == store.ThreadClosed {
		if thread, err = s.noteClosed(ctx, thread); err != nil {
			return "", err
		}
	}
	// A call run again whose message was kept answers as the first did,
	// whatever the thread is now: it sends nothing again (Codex review of
	// #29, round 6).
	if kept, err := s.store.ReportFiled(ctx, workerMessagePrefix+call.key()); err != nil {
		return "", err
	} else if kept && thread.WorkerSession != 0 {
		// The call that started the worker is the one whose message is
		// its first (Codex review of #29, round 7).
		first, err := s.store.FirstReportKey(ctx, thread.ID, thread.WorkerSession)
		if err != nil {
			return "", err
		}
		row, err := s.store.Session(ctx, thread.WorkerSession)
		if err != nil {
			return "", err
		}
		return team.WorkerSent(thread.ID, thread.WorkerSession, modelOf(row).String(), first == workerMessagePrefix+call.key()), nil
	}
	switch {
	case thread.ReviewPull != 0:
		return "", errors.New("a cross-review has no worker: it reads its change set and files its verdict")
	case thread.IsPlanThread():
		// A plan's lead plans, and builds nothing (ADR-0020): the role is
		// checked here too, not only by the tools it is offered.
		return "", fmt.Errorf("a plan's lead has no worker: it plans P%d, and builds nothing", thread.Plan)
	case thread.State == store.ThreadClosed:
		return "", fmt.Errorf("thread T%d is closed: its worker takes no work", thread.ID)
	}
	if err := team.CheckWorkerMessage(w); err != nil {
		return "", err
	}
	// The worker starts, and the message is kept for it, one call at a
	// time: the brief that starts it is first in its queue (Codex review of
	// #29, round 3).
	key, text := workerMessagePrefix+call.key(), strings.TrimSpace(w.Message)
	s.workerStartMu.Lock()
	worker, model, started, err := s.workerOf(ctx, thread.ID, key, text)
	if err == nil && !started {
		// A worker this call started has its brief, this message, kept
		// with it already (Codex review of #29, round 5).
		err = s.queueWorkerMessage(ctx, thread.ID, worker, key, text)
	}
	s.workerStartMu.Unlock()
	if err != nil {
		return "", err
	}
	// It goes through the outbox, as a report does: one a failed run left
	// in the worker's queue goes again, and it counts as sent only once it
	// is in the worker's transcript (Codex review of #29, round 3). Its
	// delivery is asked for before anything else can fail: it is kept
	// (rounds 4 and 5).
	if done, err := s.sendReports(ctx, thread.ID); err != nil || !done {
		if err != nil {
			s.log.Warn("send to the worker", "thread", thread.ID, "err", err)
		}
		s.deliverLater()
	}
	// The model is the one this call knows, never one read after it: a
	// call and a run of it again answer the same (Codex review of #29,
	// rounds 7 and 8).
	return team.WorkerSent(thread.ID, worker, model.String(), started), nil
}

// modelOf is the model a session row runs.
func modelOf(row store.SessionRow) proto.ModelRef {
	return proto.ModelRef{Provider: row.Provider, ID: row.Model}
}

// workerModelRuns refuses a worker's recorded model when the service
// cannot run it now, as a start is refused (protocol rule 47); its session
// is never routed again (Codex review of #29, rounds 7 and 8).
func (s *Server) workerModelRuns(ctx context.Context, ref proto.ModelRef) error {
	models, err := s.engine.Models(ctx)
	if err != nil {
		return err
	}
	for _, m := range models {
		if m.ModelRef == ref {
			return nil
		}
	}
	return fmt.Errorf("your worker runs %s, which pocketd cannot run now. Stop, and tell the owner in your report", ref)
}

// workerOf is thread id's worker, which it starts when it has none.
// workerStartMu is held, so two first calls of one specialist, which PiG
// runs side by side, start one worker (Codex review of #29, round 2).
// started says this call started it.
//
// A worker it starts gets briefKey's briefText, kept with it.
func (s *Server) workerOf(ctx context.Context, id int64, briefKey, briefText string) (worker int64, model proto.ModelRef, started bool, err error) {
	thread, err := s.store.Thread(ctx, id)
	if err != nil {
		return 0, model, false, err
	}
	if thread.State == store.ThreadClosed {
		return 0, model, false, fmt.Errorf("thread T%d is closed: its worker takes no work", id)
	}
	// The worker works in the thread's folder as it is now: one removed,
	// or another checkout at its path, takes no work (Codex review of #29,
	// round 8). Each message is checked again before it goes (sendReport).
	if thread.Cwd != "" {
		if err := s.recheckFolder(ctx, thread, thread.Cwd, thread.Branch); err != nil {
			return 0, model, false, fmt.Errorf("your thread's folder is not as it was, so your worker takes no work: %w", err)
		}
	}
	if thread.WorkerSession != 0 {
		row, err := s.store.Session(ctx, thread.WorkerSession)
		if err != nil {
			return 0, model, false, err
		}
		if err := s.workerModelRuns(ctx, modelOf(row)); err != nil {
			return 0, model, false, err
		}
		return thread.WorkerSession, modelOf(row), false, nil
	}
	worker, model, err = s.startWorker(ctx, thread, store.ReportRow{Key: briefKey, Text: briefText})
	return worker, model, err == nil, err
}

// workerMessagePrefix begins the key of a message to a worker among its
// thread's reports.
const workerMessagePrefix = team.WorkerMessagePrefix

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
	queued, err := s.store.QueueWorkerReport(ctx, store.ReportRow{Key: key, ThreadID: thread, SessionID: worker, Text: text}, 0)
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
//
// brief, the message that starts it, is kept with it in the same
// transaction, so the brief is the first message it ever gets (Codex
// review of #29, round 4). The worker is watched once it is recorded, so
// no watch outlives a record a close refused; a watch that fails is
// logged, as the brief is kept and goes all the same: the next message,
// and the service's start, watch it again, and a run that ended before
// the watch is read then (round 5).
func (s *Server) startWorker(ctx context.Context, thread store.ThreadRow, brief store.ReportRow) (int64, proto.ModelRef, error) {
	id, spec, err := s.makeWorker(ctx, thread, brief)
	return id, spec.Model, err
}

// makeWorker is startWorker's work; spec is the worker's as it was made.
func (s *Server) makeWorker(ctx context.Context, thread store.ThreadRow, brief store.ReportRow) (int64, SessionSpec, error) {
	if thread.Session == 0 || thread.Cwd == "" {
		return 0, SessionSpec{}, fmt.Errorf("thread T%d has no folder yet", thread.ID)
	}
	key := workerKey(thread)
	spec := SessionSpec{Title: "T" + strconv.FormatInt(thread.ID, 10) + " · worker", Cwd: thread.Cwd, Thread: thread.ID, Worker: true}
	// A worker an earlier run made is recorded as it was made, whatever
	// the profile and the sign-ins say now (Codex review of #29, round 1).
	made, exists, err := s.engine.Created(ctx, key)
	if err != nil {
		return 0, spec, err
	}
	if exists {
		// Its model is checked as a later message's is, never routed
		// again (Codex review of #29, round 8).
		if err := s.workerModelRuns(ctx, made.Model); err != nil {
			return 0, spec, err
		}
		spec.Model, spec.Route = made.Model, made.Route
	} else {
		// Its tools are its specialist's profile's: a profile pocketd
		// cannot read now starts no worker, rather than one with every
		// tool (Codex review of #29, round 1).
		if s.profiles == nil {
			return 0, spec, errors.New("pocketd reads no profiles, so it cannot tell your worker's tools")
		}
		p, err := s.profiles.Find(thread.Agent)
		if err != nil {
			return 0, spec, fmt.Errorf("pocketd cannot read your profile, so it cannot tell your worker's tools: %w", err)
		}
		// Its tools are checked as a start's are: a name the engine does
		// not know makes the profile one that starts nothing (Codex review
		// of #29, round 6).
		if err := s.checkTools(p); err != nil {
			return 0, spec, fmt.Errorf("your profile cannot start a worker: %w", err)
		}
		profile := &p
		models, err := s.engine.Models(ctx)
		if err != nil {
			return 0, spec, err
		}
		// The owner's list is read and checked at every fresh start, an
		// asked model's and a profile's included: a list that breaks a
		// rule starts no worker (ADR-0022; Codex review of #33).
		list, err := s.workerList()
		if err != nil {
			return 0, spec, fmt.Errorf("pocketd cannot read the owner's worker list: %w. Stop, and tell the owner in your report", err)
		}
		choice, err := router.Choose(router.Request{Worker: true, Profile: profile, WorkerAsked: thread.WorkerAsked, Workers: list}, models)
		if err != nil {
			return 0, spec, fmt.Errorf("%w. Stop, and tell the owner in your report", err)
		}
		spec.Model, spec.Route, spec.WorkerTools = choice.Model, string(choice.Rule), profile.Tools
	}
	id, err := s.engine.CreateSession(ctx, key, spec)
	if err != nil {
		return 0, spec, err
	}
	s.workMu.Lock()
	row, err := s.store.RecordWorkerSession(ctx, key, store.SessionRow{ID: id, Title: spec.Title, Cwd: spec.Cwd, Provider: spec.Model.Provider,
		Model: spec.Model.ID, Route: spec.Route, CreatedBy: teamCreator}, thread.ID, brief)
	if err == nil {
		s.publishThread(row)
	}
	s.workMu.Unlock()
	if errors.Is(err, store.ErrConflict) {
		return 0, spec, fmt.Errorf("thread T%d closed while its worker started", thread.ID)
	} else if err != nil {
		return 0, spec, err
	}
	s.publishSession(id, true)
	if err := s.engine.Watch(ctx, id); err != nil {
		s.log.Warn("watch the worker's session", "thread", thread.ID, "session", id, "err", err)
	}
	// The brief was visible to a delivery pass before the watch began, so
	// a run may have ended before it: the delivery pass watches the worker
	// again if its watch failed, then reads on from its cursor, so a run
	// that ended unwatched still replies (Codex review of #29, rounds 6
	// and 7).
	s.workerDue(id)
	return id, spec, nil
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
	// Only the runs after the cursor, read from there: each pass reads and
	// keeps a reply once, not every reply before it again (Codex review of
	// #29, rounds 3 and 4).
	entries, live, err := s.engine.TranscriptAfter(ctx, worker, thread.WorkerCursor)
	if err != nil {
		return err
	}
	replies, cursor := workerReplies(entries, live, thread.WorkerCursor)
	for _, r := range replies {
		if _, err := s.store.QueueWorkerReport(ctx, store.ReportRow{Key: workerReplyPrefix + thread.Owner + ":" + strconv.FormatInt(r.entry, 10),
			ThreadID: thread.ID, SessionID: thread.Session, Text: team.WorkerReplyText(thread.ID, r.text)}, r.run); err != nil {
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
const workerReplyPrefix = team.WorkerReplyPrefix

// workerReply is a reply a worker's run ended with, by the entry that
// names it, and the entry that began its run.
type workerReply struct {
	entry, run int64
	text       string
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
	out, cursor, _ = runReplies(entries, live, after, "worker")
	return out, cursor
}

// runReplies are the replies of the runs a transcript holds, read as
// workerReplies reads a worker's, with who naming the session in the
// replies pocketd writes for a run that ended with none: a worker's, or an
// agent's work session's (ADR-0074). consumed is the last entry of the
// last run that ended, or after: the entry before the message that began
// the next run, or the transcript's last once it is idle, never one of a
// run still at work. A work session reads on from there, so no entry of a
// run whose reply was kept is read again (Codex review of #174, round 5).
func runReplies(entries []proto.Entry, live proto.Live, after int64, who string) (out []workerReply, cursor, consumed int64) {
	cursor, consumed = after, after
	start := -1
	end := func(stop int, last bool) {
		if start < 0 || entries[start].ID <= after {
			return
		}
		cursor = entries[start].ID
		consumed = max(consumed, entries[stop-1].ID)
		// A compaction, which the wire names compaction (proto.EntryCompaction),
		// and an entry the protocol does not model, are no step of the run:
		// the reply is the last entry before them (Codex review of #29,
		// round 7; of #174, round 1).
		at := stop - 1
		for at > start && (entries[at].Kind == proto.EntryCompaction || entries[at].Kind == proto.EntryOther) {
			at--
		}
		final := entries[at]
		switch {
		case at > start && final.Kind == proto.EntryAssistant && final.StopReason != stopToolUse:
			out = append(out, workerReply{entry: final.ID, run: entries[start].ID, text: runReplyText(final, who)})
		case last:
			why := strings.TrimSpace(live.Failure)
			if why == "" {
				why = "no reason was given"
			}
			out = append(out, workerReply{entry: entries[start].ID, run: entries[start].ID, text: "The " + who + "'s run ended with no reply: " + why + "."})
		default:
			out = append(out, workerReply{entry: entries[start].ID, run: entries[start].ID, text: "The " + who + "'s run ended with no reply."})
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
	return out, cursor, consumed
}

// stopToolUse is the stop reason of a model's entry that a tool call
// ends: its run goes on (PiG's ai.StopReasonToolUse).
const stopToolUse = "toolUse"

// stopEnd is the stop reason of a model's entry that ends its response
// whole (PiG's ai.StopReasonStop): any other, a tool call, a length cut,
// an abort or an error, is no final response.
const stopEnd = "stop"

// cancelReport withdraws every attempt of report row still queued, and
// notes it sent: it goes to no one. outboxMu is held.
func (s *Server) cancelReport(ctx context.Context, row store.ReportRow) error {
	for attempt := 0; attempt <= row.Attempt; attempt++ {
		if _, err := s.engine.Withdraw(ctx, row.SessionID, attemptKey(row.Key+":report", attempt)); err != nil {
			return fmt.Errorf("withdraw the message: %w", err)
		}
	}
	return s.store.ReportSent(ctx, row.Key)
}

// withdrawWorkerReplies withdraws every attempt of thread's worker
// replies and messages still queued, of its shift's round messages (Codex
// review of #36), and of its supervisor's messages (ADR-0032), each one
// droppedAtClose, before its close commits: PiG places a queued
// message whatever pocketd's locks say, so one withdrawn after the commit
// may be in the transcript already (Codex review of #29, round 2). A
// withdrawal that fails leaves the close undone, for a retry; withdrew
// says one took effect. outboxMu is held.
func (s *Server) withdrawWorkerReplies(ctx context.Context, thread int64) (withdrew bool, err error) {
	return s.withdrawQueued(ctx, thread, func(row store.ReportRow) bool { return droppedAtClose(row.Key) })
}

// dropWorkerReplies notes thread's worker replies, shift messages and
// supervisor's messages not yet sent as sent (droppedAtClose), once its
// close committed: they go to no one. outboxMu is held.
func (s *Server) dropWorkerReplies(ctx context.Context, thread int64) error {
	rows, err := s.store.UnsentReportsOf(ctx, thread)
	if err != nil {
		return err
	}
	for _, row := range rows {
		if droppedAtClose(row.Key) {
			if err := s.store.ReportSent(ctx, row.Key); err != nil {
				return err
			}
		}
	}
	return nil
}

// replyText is a worker's reply's text (runReplyText).
func replyText(e proto.Entry) string { return runReplyText(e, "worker") }

// runReplyText is a reply's text, or what ended its run with none, who
// naming the session that ran it. A run stopped or failed mid-answer says
// so before what it had written: that is no finished report (Codex review
// of #29, round 6).
func runReplyText(e proto.Entry, who string) string {
	text := strings.TrimSpace(e.Text)
	// A stop at the model's output limit is cut short too (Codex review of
	// #29, round 7).
	if stopped := e.StopReason == "aborted" || e.StopReason == "error" || e.StopReason == "length" || e.Error != ""; stopped && text != "" {
		why := e.StopReason
		switch {
		case e.Error != "":
			why = e.Error
		case why == "length":
			why = "it reached the model's output limit"
		}
		return "The " + who + "'s run ended before it finished (" + why + "). What it had written:\n\n" + text
	}
	if text != "" {
		return text
	}
	if e.Error != "" {
		return "The " + who + "'s run failed: " + e.Error
	}
	if e.StopReason != "" {
		return "The " + who + "'s run ended (" + e.StopReason + ") with no reply."
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
		// Watched first (Watch is idempotent): a watch that failed is
		// tried again, and the read after it finds a run that ended
		// unwatched (Codex review of #29, round 6).
		if thread.State != store.ThreadClosed {
			if err := s.engine.Watch(ctx, worker); err != nil {
				s.log.Warn("watch the worker's session", "session", worker, "err", err)
				s.workerDue(worker)
				continue
			}
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
//
// No message reopens the thread until the stop is done (worker_stop_due):
// the run of the closed time stops before any work of the reopened thread
// reaches the worker, and the worker's cursor moves past that time, so its
// runs reply to no one (store.WorkerStopped; ADR-0061; Codex review of
// #121, rounds 2 to 6). Only a read that finds the worker's thread closed
// lets the stop go: a read that fails is tried again by the delivery pass
// (round 3), and a worker no closed thread names is owed nothing.
//
// One stop of a worker goes at a time, from its look at the thread to the
// note that it is done, and only a stop still owed goes: a second caller,
// as the run-ended follow beside the close, finds it done, or the thread
// reopened, and stops nothing of the reopened thread's work (Codex review
// of #121, round 7).
func (s *Server) stopWorker(ctx context.Context, worker int64) {
	defer s.workerStops.lock(strconv.FormatInt(worker, 10))()
	thread, err := s.readThreadOfWorker(ctx, worker)
	if s.afterStopRead != nil {
		s.afterStopRead(worker)
	}
	if errors.Is(err, store.ErrNotFound) {
		return
	} else if err != nil {
		s.log.Warn("read a worker's thread before its stop", "session", worker, "err", err)
		s.workerDue(worker)
		return
	}
	if thread.State != store.ThreadClosed || !thread.WorkerStopDue {
		return
	}
	busy, err := s.engine.Busy(ctx, worker)
	if err == nil && busy {
		err = s.engine.Abort(ctx, worker)
	}
	if err == nil {
		err = s.workerStopped(ctx, thread, worker)
	}
	if err != nil {
		s.log.Warn("stop a closed thread's worker", "session", worker, "err", err)
		s.workerDue(worker)
	}
}

// workerStopped notes the stop of closed thread's worker done, its
// cursor moved past the closed time's runs (store.WorkerStopped), and has
// a pass note the messages the stop held back. No work reaches the worker
// while the thread is closed, so its newest entry ends that time.
func (s *Server) workerStopped(ctx context.Context, thread store.ThreadRow, worker int64) error {
	entries, _, err := s.engine.TranscriptAfter(ctx, worker, thread.WorkerCursor)
	if err != nil {
		return err
	}
	floor := thread.WorkerCursor
	for _, e := range entries {
		floor = max(floor, e.ID)
	}
	if err := s.store.WorkerStopped(ctx, thread.ID, worker, floor); err != nil {
		return err
	}
	s.deliverLater()
	return nil
}

// readThreadOfWorker is store.ThreadOfWorker, which tests may fail
// (failRead).
func (s *Server) readThreadOfWorker(ctx context.Context, worker int64) (store.ThreadRow, error) {
	if s.failRead != nil {
		if err := s.failRead("readThreadOfWorker", worker); err != nil {
			return store.ThreadRow{}, err
		}
	}
	return s.store.ThreadOfWorker(ctx, worker)
}

// settleWorkers, at the service's start, sends each open thread's
// worker's replies that a stop left unsent, and stops each closed thread's
// worker a stop left running: a run that ended, or a close that committed,
// while pocketd was down has no end to follow (Codex review of #29, round
// 1). It runs off the start's path, as a delivery pass does, whose retries
// take what fails.
func (s *Server) settleWorkers() {
	run := func() { s.scanWorkers(s.passCtx) }
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
