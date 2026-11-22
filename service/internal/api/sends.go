package api

import (
	"context"
	"errors"
	"fmt"

	"github.com/frostyard/pocketgo/service/internal/store"
	"github.com/frostyard/pocketgo/service/internal/team"
)

// TeamSend runs team_send for session call.Session (ADR-0032): a
// supervisor's message to the specialist of a thread a session of its own
// profile delegated, open, done or blocked. The message is kept in the
// outbox once by the call's key, and placed in the specialist's session as
// the owner's answers are: sent once, followed until it is in the
// transcript, and kept across restarts; a busy specialist's queue holds it
// for its run. Once there, it takes a done thread back to open, and a
// blocked one too, withdrawing its open asks (ADR-0037). A call run again
// after a restart finds the message its first run kept, whatever the
// profiles and the thread say by then. The answer says whether the message
// is in the specialist's transcript, or waits, and what its delivery
// unblocked.
func (s *Server) TeamSend(ctx context.Context, call TeamCall, m team.Send) (string, error) {
	ctx = context.WithoutCancel(ctx)
	key := team.SendKey(call.Session, call.key())
	if row, err := s.store.Report(ctx, key); err == nil {
		if done, err := s.sendReports(ctx, row.ThreadID); err != nil || !done {
			if err != nil {
				s.log.Warn("send a supervisor's message", "thread", row.ThreadID, "err", err)
			}
			s.deliverLater()
		}
		return s.sentText(ctx, call.Session, key)
	} else if !errors.Is(err, store.ErrNotFound) {
		// This call may be one run again after its message was kept: the
		// answer says neither that it was nor that it failed, so the
		// supervisor sends no second copy under a new call (Codex review of
		// #56, round 2).
		s.log.Warn("look up a supervisor's message", "err", err)
		return team.SentText(team.SentReceipt{Thread: m.Thread, Agent: "the specialist", State: team.SendUnknown}), nil
	}
	caller, err := s.callerProfile(ctx, call.Session)
	if err != nil {
		return "", err
	}
	if err := team.CheckSend(caller, m); err != nil {
		return "", err
	}
	thread, err := s.store.Thread(ctx, m.Thread)
	if errors.Is(err, store.ErrNotFound) {
		return "", fmt.Errorf("no thread T%d", m.Thread)
	} else if err != nil {
		return "", err
	}
	supervisor := caller.Name
	if supervisor == "" {
		supervisor = caller.ID
	}
	// A closed thread takes one that reopens it (ADR-0061), checked under
	// its cleanup lock, held until the message is kept, so its worktree
	// does not go from under it. A thread that moved between the read and
	// the keeping is read and checked again, once.
	if err := s.fileSend(ctx, caller.ID, supervisor, key, thread, m.Text); err != nil {
		return "", err
	}
	// Sending it is the outbox's, in the order the thread's messages to
	// its specialist were kept; one that cannot go now is tried again
	// until it does.
	if done, err := s.sendReports(ctx, thread.ID); err != nil || !done {
		if err != nil {
			s.log.Warn("send a supervisor's message", "thread", thread.ID, "err", err)
		}
		s.deliverLater()
	}
	return s.sentText(ctx, call.Session, key)
}

// fileSend checks thread for a message of the supervisor's text from a
// session of profile caller, and keeps it under key, written for the
// thread's state: a closed thread's reopens it (ADR-0061). It holds a
// closed thread's cleanup lock from the look at its folder to the keeping,
// and outboxMu, which every close holds as it withdraws and drops its
// thread's messages, over the keeping: a message kept for a thread not
// closed is withdrawn and dropped by a close after it. A thread that moved
// between the read and the keeping is read and checked again, once.
func (s *Server) fileSend(ctx context.Context, caller, supervisor, key string, thread store.ThreadRow, text string) error {
	for try := 1; ; try++ {
		err := s.fileSendOnce(ctx, caller, supervisor, key, thread, text)
		if !errors.Is(err, store.ErrConflict) {
			return err
		}
		now, rerr := s.store.Thread(ctx, thread.ID)
		if rerr != nil || try == 2 || now.State == thread.State {
			return fmt.Errorf("thread T%d takes no message now", thread.ID)
		}
		thread = now
	}
}

// fileSendOnce is one try of fileSend, for thread as read.
func (s *Server) fileSendOnce(ctx context.Context, caller, supervisor, key string, thread store.ThreadRow, text string) error {
	closed := thread.State == store.ThreadClosed
	if closed {
		defer s.lockClean(thread.ID)()
		// Read again under the lock, its messages already in the transcript
		// noted first: a close or a reopen may have come (Codex review of
		// #121, round 4).
		var err error
		if thread, err = s.noteClosed(ctx, thread); err != nil {
			return err
		}
		if thread.State != store.ThreadClosed {
			return store.ErrConflict
		}
	}
	target, err := s.sendTarget(ctx, caller, thread)
	if err != nil {
		return err
	}
	if closed && target.Own && !target.Review {
		if target.Reopen, err = s.reopenTarget(ctx, thread); err != nil {
			return err
		}
	}
	if err := team.CheckSendTarget(target); err != nil {
		return err
	}
	row := store.ReportRow{Key: key, ThreadID: thread.ID, SessionID: thread.Session, Text: team.SendText(thread.ID, supervisor, text)}
	if closed {
		refusal, failure := s.reopenRefused(ctx, thread, team.FromSupervisor)
		if failure != nil {
			return failure
		}
		if refusal != nil {
			return refusal
		}
		row.Text = team.SendTextToClosed(thread.ID, supervisor, text)
	}
	// The thread's messages already in the transcript are noted first,
	// under outboxMu held through the keeping: one may have reopened it,
	// and the text written for a closed thread is then refused as written
	// for another state, and written again (Codex review of #121, round 2).
	s.outboxMu.Lock()
	defer s.outboxMu.Unlock()
	if err := s.noteSendsPlaced(ctx, thread.ID); err != nil {
		return err
	}
	_, err = s.store.FileSendTo(ctx, row, closed)
	return err
}

// sendTarget is what team.CheckSendTarget reads of thread for a message
// from a session of profile caller: Own when a session of that profile
// delegated it, as team_close checks.
func (s *Server) sendTarget(ctx context.Context, caller string, thread store.ThreadRow) (team.SendTarget, error) {
	target := team.SendTarget{ID: thread.ID, State: thread.State, Review: thread.ReviewPull != 0}
	delegator, err := s.store.Session(ctx, thread.SupervisorSession)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return team.SendTarget{}, err
	}
	target.Own = err == nil && delegator.Profile == caller
	return target, nil
}

// sentText is team_send's answer for the message of key, kept for
// caller, as it is now: delivered once PiG's record of one of its attempts
// shows its entry in the transcript, with the thread's state now and, when
// its own noting unblocked the thread, the asks it withdrew (ADR-0037);
// else queued. It names where the specialist's reports go, its thread's
// delegating session. The message is kept, so a read that fails says so,
// never a failure the supervisor would send again under another key
// (Codex review of #56, round 1); one its thread's close dropped is an
// error: it went to no one.
//
// It reads under outboxMu, as delivery and closes change the message and
// its thread under it: a message placed is noted first, so the thread's
// state it gives is the one its delivery leaves, and a message whose
// thread closed with no attempt placed is dropped, whatever the close has
// noted yet (Codex review of #56, round 2).
func (s *Server) sentText(ctx context.Context, caller int64, key string) (string, error) {
	s.outboxMu.Lock()
	defer s.outboxMu.Unlock()
	row, err := s.store.Report(ctx, key)
	if err != nil {
		s.log.Warn("read a supervisor's message", "err", err)
		return team.SentText(team.SentReceipt{State: team.SendUnconfirmed}), nil
	}
	// What the message's delivery may do is what it was kept to do: one kept
	// before migration 20 unblocks nothing (Codex review of #62, round 2).
	receipt := team.SentReceipt{Thread: row.ThreadID, Agent: "the specialist", State: team.SendUnconfirmed, Unblocks: row.Unblocks, Reopens: row.Reopens != 0}
	// noted is what this answer's own noting committed, with the thread as
	// it left it: the answer is built from it, never from a read after it,
	// which may fail (Codex review of #62, round 1).
	var noted *store.SendNoted
	var thread store.ThreadRow
	placed, err := s.sendPlaced(ctx, row)
	if err != nil {
		s.log.Warn("look up a supervisor's message", "thread", row.ThreadID, "err", err)
	} else if placed && row.SentAt == nil {
		after, change, nerr := store.ThreadRow{}, store.SendNoted{}, s.failSendNote
		if nerr == nil {
			after, change, nerr = s.sendDelivered(ctx, row)
		}
		if nerr != nil {
			// In the transcript, and not noted: what the noting does to the
			// thread waits for a later one, and the answer says so, never
			// the thread's state before it (Codex review of #62, round 3).
			s.log.Warn("note a supervisor's message delivered", "thread", row.ThreadID, "err", nerr)
			receipt.Unnoted = true
			s.deliverLater()
		} else {
			noted, thread = &change, after
		}
		if s.afterSendNoted != nil {
			s.afterSendNoted()
		}
	}
	if noted == nil {
		var terr error
		if thread, terr = s.store.Thread(ctx, row.ThreadID); terr != nil {
			s.log.Warn("read a supervisor's message's thread", "thread", row.ThreadID, "err", terr)
			return team.SentText(receipt), nil
		}
	}
	receipt.Agent, receipt.Delegator, receipt.Caller = s.agentName(thread.Agent), thread.SupervisorSession, caller
	switch {
	case err != nil:
	case placed:
		receipt.State, receipt.ThreadState = team.SendDelivered, thread.State
		// The unblocking is this message's own (ADR-0037): what its noting
		// here committed, else, for one noted before, its row and the asks
		// that name it, never another message's.
		switch {
		case noted != nil:
			receipt.Unblocked = noted.Unblocked
			for _, a := range noted.Withdrawn {
				receipt.Withdrawn = append(receipt.Withdrawn, team.WithdrawnAsk{ID: a.ID, Question: a.Question})
			}
		case row.UnblockedAt != nil:
			// A read bounded by what the answer names (Codex review of #62,
			// round 2).
			receipt.Unblocked = true
			withdrawn, total, err := s.store.AsksWithdrawnBy(ctx, row.Key, team.MaxWithdrawnNamed, team.MaxWithdrawnQuestion+1)
			if err != nil {
				s.log.Warn("read the asks a supervisor's message withdrew", "thread", row.ThreadID, "err", err)
				receipt.WithdrawnUnread = true
			}
			receipt.WithdrawnTotal = total
			for _, a := range withdrawn {
				receipt.Withdrawn = append(receipt.Withdrawn, team.WithdrawnAsk{ID: a.ID, Question: a.Question})
			}
		}
	case row.SentAt != nil || (thread.State == store.ThreadClosed && !reopensNow(row, thread)):
		return "", fmt.Errorf("thread T%d closed before the message reached its specialist: it went to no one", thread.ID)
	default:
		receipt.State = team.SendQueued
	}
	return team.SentText(receipt), nil
}

// sendPlaced says whether one attempt of a supervisor's message, row, is
// in its session's transcript, by PiG's record of its request key, the
// latest attempt first: what pocketd noted of it may lag.
func (s *Server) sendPlaced(ctx context.Context, row store.ReportRow) (bool, error) {
	for attempt := row.Attempt; attempt >= 0; attempt-- {
		where, err := s.engine.Placed(ctx, row.SessionID, attemptKey(row.Key+":report", attempt))
		if err != nil {
			return false, err
		}
		if where == PlacementPlaced {
			return true, nil
		}
	}
	return false, nil
}

// sendDelivered notes a supervisor's message, row, in its specialist's
// transcript, and publishes what that changed in one change under workMu
// (ADR-0032, ADR-0037): each ask a blocked thread's unblocking withdrew,
// then the thread, from done or blocked back to open. outboxMu is held,
// taken before workMu.
// thread is as the noting left it.
func (s *Server) sendDelivered(ctx context.Context, row store.ReportRow) (store.ThreadRow, store.SendNoted, error) {
	s.workMu.Lock()
	defer s.workMu.Unlock()
	return s.sendDeliveredLocked(ctx, row)
}

// sendDeliveredLocked is sendDelivered with workMu held.
func (s *Server) sendDeliveredLocked(ctx context.Context, row store.ReportRow) (store.ThreadRow, store.SendNoted, error) {
	thread, change, err := s.store.SendDelivered(ctx, row.Key, row.ThreadID)
	if err != nil {
		return thread, change, err
	}
	// Each ask's event, then the thread's, which publishes their plan once
	// (as TeamMessage does): a withdrawn ask leaves the owner's Inbox.
	for _, ask := range change.Withdrawn {
		s.emitAsk(ask)
	}
	switch {
	case change.FromClosed:
		s.publishReopened(ctx, thread)
	case change.Reopened:
		s.publishThread(thread)
	}
	if len(change.Withdrawn) > 0 && thread.IsPlanThread() {
		s.deliverLater() // a withdrawal may have cleared the plan's gate (store.askCleared)
	}
	return thread, change, nil
}

// noteSendsPlaced notes each of thread's supervisor's messages that PiG
// placed in its specialist's transcript and pocketd has not noted yet
// (sendDelivered), under outboxMu, which the caller holds until the
// report that follows is filed: the report came after those messages, so
// a done thread they reopen takes the report's state, never the other way
// round, and a message placed after the report reopens it once noted
// (ADR-0032).
//
// The owner's messages to the thread's specialist are noted first the same
// way (noteOwnerMessages): one placed reopens a closed thread (ADR-0061).
func (s *Server) noteSendsPlaced(ctx context.Context, thread int64) error {
	if _, err := s.noteOwnerMessages(ctx, thread); err != nil {
		return err
	}
	return s.noteSends(ctx, thread, s.sendDelivered)
}

// noteSendsPlacedLocked is noteSendsPlaced with workMu held, as a close
// holds it.
func (s *Server) noteSendsPlacedLocked(ctx context.Context, thread int64) error {
	if _, err := s.noteOwnerMessagesLocked(ctx, thread); err != nil {
		return err
	}
	return s.noteSends(ctx, thread, s.sendDeliveredLocked)
}

// noteSends notes each of thread's supervisor's messages PiG placed and
// pocketd has not noted, with note.
func (s *Server) noteSends(ctx context.Context, thread int64, note func(context.Context, store.ReportRow) (store.ThreadRow, store.SendNoted, error)) error {
	rows, err := s.store.UnsentReportsOf(ctx, thread)
	if err != nil {
		return err
	}
	for _, row := range rows {
		if !team.IsSendKey(row.Key) {
			continue
		}
		placed, err := s.sendPlaced(ctx, row)
		if err != nil {
			return err
		}
		if !placed {
			continue
		}
		// A reopen the close's worker stop holds back waits for it.
		if _, _, err := note(ctx, row); err != nil && !errors.Is(err, store.ErrWorkerStopping) {
			return err
		}
	}
	return nil
}

// sendsBeforeClose settles the supervisor's messages a close of row must
// see to first, under outboxMu and workMu, after the thread's own queued
// messages were withdrawn (ADR-0032):
//   - Each of row's messages PiG placed is noted, and reopens a done
//     thread, so the close reads the state it leaves: a plan task that a
//     message placed after its report is not done at its close (Codex
//     review of #56, round 1).
//   - A plan task's close may end its plan, which closes the lead's thread
//     in the same transaction (store.threadClosed): the lead's messages
//     still queued are withdrawn first, and those PiG placed already are
//     noted, so a blocked lead's asks are withdrawn as its specialist was
//     told, before its close (ADR-0037; Codex review of #62, round 1).
//     lead is that thread, 0 for none; withdrew says a withdrawal took
//     effect.
//
// The caller hands lead to sendsAfterClose, whatever the close did.
func (s *Server) sendsBeforeClose(ctx context.Context, row store.ThreadRow) (lead int64, withdrew bool, err error) {
	if lead, err = s.leadAtClose(ctx, row); err != nil {
		return 0, false, err
	}
	if lead != 0 {
		if withdrew, err = s.withdrawQueued(ctx, lead, func(r store.ReportRow) bool { return team.IsSendKey(r.Key) }); err != nil {
			return lead, withdrew, err
		}
		if err = s.noteSendsPlacedLocked(ctx, lead); err != nil {
			return lead, withdrew, err
		}
	}
	return lead, withdrew, s.noteSendsPlacedLocked(ctx, row.ID)
}

// leadAtClose is the lead's thread of row's plan when a close of row, a
// plan task's thread, may end the plan: every other task of its approved
// graph is done, and the lead's thread is not closed. 0 otherwise.
func (s *Server) leadAtClose(ctx context.Context, row store.ThreadRow) (int64, error) {
	if row.Plan == 0 || row.PlanTask == "" {
		return 0, nil
	}
	facts, err := s.store.PlanFacts(ctx, row.Plan)
	if err != nil {
		return 0, err
	}
	if facts.Thread.State == store.ThreadClosed || facts.Graph == nil || facts.Graph.State != store.GraphApproved {
		return 0, nil
	}
	for _, t := range facts.Graph.Tasks {
		if t.ID != row.PlanTask && t.State != team.TaskDone {
			return 0, nil
		}
	}
	return facts.Thread.ID, nil
}

// sendsAfterClose follows a close that withdrew lead's queued messages
// (sendsBeforeClose): a lead's thread the close closed drops them, and
// one still open gets them again from a delivery pass. outboxMu is held.
func (s *Server) sendsAfterClose(ctx context.Context, lead int64) {
	if lead == 0 {
		return
	}
	thread, err := s.store.Thread(ctx, lead)
	if err == nil && thread.State == store.ThreadClosed {
		err = s.dropWorkerReplies(ctx, lead)
	}
	if err != nil {
		s.log.Warn("settle a plan lead's messages after a close", "thread", lead, "err", err)
	}
	s.deliverLater()
}

// errNoBriefYet holds a supervisor's message back while its thread's
// brief is not in the specialist's transcript: the brief comes first, as
// it does before the owner's message (protocol rule 10).
var errNoBriefYet = errors.New("the thread's specialist does not have its brief yet")

// briefFirst is errNoBriefYet while thread id's brief is not in its
// specialist's transcript, by PiG's record of its attempts when pocketd
// has not noted it yet.
func (s *Server) briefFirst(ctx context.Context, id int64) error {
	thread, err := s.store.Thread(ctx, id)
	if err != nil {
		return err
	}
	if thread.Briefed || thread.Brief == "" {
		return nil
	}
	for attempt := thread.BriefAttempt; attempt >= 0; attempt-- {
		where, err := s.engine.Placed(ctx, thread.Session, attemptKey(briefKey(thread), attempt))
		if err != nil {
			return err
		}
		if where == PlacementPlaced {
			return nil
		}
	}
	return errNoBriefYet
}

// reopensNow says whether row, a supervisor's message kept while its
// thread was closed, still reopens thread, which is closed: no reopen came
// since it was kept, so no close after one dropped it (ADR-0061).
func reopensNow(row store.ReportRow, thread store.ThreadRow) bool {
	return row.Reopens != 0 && row.Reopens == thread.Reopened+1
}

// droppedAtClose says whether the outbox message of key goes only while
// its thread is open: a worker's reply or a message to the worker
// (ADR-0021), a shift's own message (ADR-0024), and a supervisor's message
// (ADR-0032), by team.DroppedAtClose. A close withdraws each still queued
// before it commits, and drops each not placed in the transaction that
// closes the thread (store.threadClosed), so none goes after it, also once
// a message reopens the thread (ADR-0061).
func droppedAtClose(key string) bool {
	return team.DroppedAtClose(key)
}
