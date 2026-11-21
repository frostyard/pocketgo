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
// for its run. Once there, it takes a done thread back to open. A call run
// again after a restart finds the message its first run kept, whatever the
// profiles and the thread say by then. The answer says whether the message
// is in the specialist's transcript, or waits.
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
	target, err := s.sendTarget(ctx, caller.ID, thread)
	if err != nil {
		return "", err
	}
	if err := team.CheckSendTarget(target); err != nil {
		return "", err
	}
	supervisor := caller.Name
	if supervisor == "" {
		supervisor = caller.ID
	}
	// Kept under outboxMu, which every close holds as it withdraws and
	// drops its thread's messages: the store keeps it only while the
	// thread is open, done or blocked, so none is kept after a close.
	s.outboxMu.Lock()
	_, err = s.store.FileSend(ctx, store.ReportRow{Key: key, ThreadID: thread.ID, SessionID: thread.Session,
		Text: team.SendText(thread.ID, supervisor, m.Text)})
	s.outboxMu.Unlock()
	if errors.Is(err, store.ErrConflict) {
		// The thread moved since it was read: refused as it is now.
		if now, rerr := s.store.Thread(ctx, thread.ID); rerr == nil {
			target.State = now.State
			if cerr := team.CheckSendTarget(target); cerr != nil {
				return "", cerr
			}
		}
		return "", fmt.Errorf("thread T%d takes no message now", thread.ID)
	} else if err != nil {
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
// shows its entry in the transcript, with the thread's state now; else
// queued. It names where the specialist's reports go, its thread's
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
	receipt := team.SentReceipt{Thread: row.ThreadID, Agent: "the specialist", State: team.SendUnconfirmed}
	placed, err := s.sendPlaced(ctx, row)
	if err != nil {
		s.log.Warn("look up a supervisor's message", "thread", row.ThreadID, "err", err)
	} else if placed && row.SentAt == nil {
		if _, err := s.sendDelivered(ctx, row); err != nil {
			s.log.Warn("note a supervisor's message delivered", "thread", row.ThreadID, "err", err)
		}
	}
	thread, terr := s.store.Thread(ctx, row.ThreadID)
	if terr != nil {
		s.log.Warn("read a supervisor's message's thread", "thread", row.ThreadID, "err", terr)
		return team.SentText(receipt), nil
	}
	receipt.Agent, receipt.Delegator, receipt.Caller = s.agentName(thread.Agent), thread.SupervisorSession, caller
	switch {
	case err != nil:
	case placed:
		receipt.State, receipt.ThreadState = team.SendDelivered, thread.State
	case row.SentAt != nil || thread.State == store.ThreadClosed:
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
// transcript, and publishes its thread when that took it from done back
// to open, in one change under workMu (ADR-0032). outboxMu is held.
func (s *Server) sendDelivered(ctx context.Context, row store.ReportRow) (reopened bool, err error) {
	s.workMu.Lock()
	defer s.workMu.Unlock()
	return s.sendDeliveredLocked(ctx, row)
}

// sendDeliveredLocked is sendDelivered with workMu held.
func (s *Server) sendDeliveredLocked(ctx context.Context, row store.ReportRow) (reopened bool, err error) {
	thread, reopened, err := s.store.SendDelivered(ctx, row.Key, row.ThreadID)
	if err == nil && reopened {
		s.publishThread(thread)
	}
	return reopened, err
}

// noteSendsPlaced notes each of thread's supervisor's messages that PiG
// placed in its specialist's transcript and pocketd has not noted yet
// (sendDelivered), under outboxMu, which the caller holds until the
// report that follows is filed: the report came after those messages, so
// a done thread they reopen takes the report's state, never the other way
// round, and a message placed after the report reopens it once noted
// (ADR-0032).
func (s *Server) noteSendsPlaced(ctx context.Context, thread int64) error {
	return s.noteSends(ctx, thread, s.sendDelivered)
}

// noteSendsPlacedLocked is noteSendsPlaced with workMu held, as a close
// holds it.
func (s *Server) noteSendsPlacedLocked(ctx context.Context, thread int64) error {
	return s.noteSends(ctx, thread, s.sendDeliveredLocked)
}

// noteSends notes each of thread's supervisor's messages PiG placed and
// pocketd has not noted, with note.
func (s *Server) noteSends(ctx context.Context, thread int64, note func(context.Context, store.ReportRow) (bool, error)) error {
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
		if _, err := note(ctx, row); err != nil {
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
//     still queued are withdrawn first. lead is that thread, 0 for none;
//     withdrew says a withdrawal took effect.
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

// droppedAtClose says whether the outbox message of key goes only while
// its thread is open: a worker's reply or a message to the worker
// (ADR-0021), a shift's own message (ADR-0024), and a supervisor's message
// (ADR-0032). A close withdraws each still queued before it commits, and
// drops each not placed after it.
func droppedAtClose(key string) bool {
	return isWorkerReport(key) || isShiftMessage(key) || team.IsSendKey(key)
}
