package api

import (
	"context"
	"errors"
	"fmt"

	"github.com/frostyard/pocketgo/service/internal/store"
	"github.com/frostyard/pocketgo/service/internal/team"
)

// A closed thread goes back to open when a message pocketd follows by its
// key reaches its specialist's session (ADR-0061): the owner's message,
// followed from when PiG took it (store.CompleteOwnerMessage), and a
// supervisor's team_send kept while the thread was closed. Delivered means
// Engine.Placed found it in the transcript, never that Send returned, as
// for a supervisor's message that unblocks a thread (ADR-0037).

// noteOwnerMessages notes each of thread's owner's messages that PiG placed
// in its specialist's transcript, and stops following each that never got
// there, under outboxMu, which the caller holds: a report, a close or a
// stop that follows reads the state they leave. A placed one takes a
// closed thread back to open, with its event, under workMu
// (ownerMessagePlaced). done says none is left on its way.
func (s *Server) noteOwnerMessages(ctx context.Context, thread int64) (done bool, err error) {
	return s.noteOwner(ctx, thread, func(ctx context.Context, m store.OwnerMessage) error {
		s.workMu.Lock()
		defer s.workMu.Unlock()
		return s.ownerMessagePlaced(ctx, m)
	})
}

// noteOwnerMessagesLocked is noteOwnerMessages with workMu held, as a close
// holds it.
func (s *Server) noteOwnerMessagesLocked(ctx context.Context, thread int64) (done bool, err error) {
	return s.noteOwner(ctx, thread, s.ownerMessagePlaced)
}

// noteOwner looks up each of thread's owner's messages pocketd follows, by
// PiG's record of its request key, and notes each placed one with placed.
// done is false while one waits in a closed thread's queue: its noting
// would reopen the thread, and the delivery pass looks again. One queued
// for a thread not closed changes nothing when placed, so it holds no pass
// back: the close that would make it matter notes it first, and asks for a
// pass.
func (s *Server) noteOwner(ctx context.Context, thread int64, placed func(context.Context, store.OwnerMessage) error) (done bool, err error) {
	messages, err := s.store.OwnerMessagesOf(ctx, thread)
	if err != nil || len(messages) == 0 {
		return err == nil, err
	}
	row, err := s.store.Thread(ctx, thread)
	if err != nil {
		return false, err
	}
	// A closed plan's thread never reopens: its messages are not followed
	// further, and hold neither a pass nor its worktree (Codex review of
	// #121, round 4). They go on to its session as PiG has them.
	if row.State == store.ThreadClosed && row.Plan != 0 {
		for _, m := range messages {
			if err := s.store.DropOwnerMessage(ctx, m.Key); err != nil {
				return false, err
			}
		}
		return true, nil
	}
	done = true
	for _, m := range messages {
		where, err := s.engine.Placed(ctx, m.SessionID, m.Key)
		if err != nil {
			return false, err
		}
		switch where {
		case PlacementPlaced:
			// A reopen the close's worker stop holds back waits for it
			// (store.ErrWorkerStopping).
			if err := placed(ctx, m); errors.Is(err, store.ErrWorkerStopping) {
				done = false
			} else if err != nil {
				return false, err
			}
		case PlacementQueued:
			done = done && row.State != store.ThreadClosed
		default:
			// Withdrawn by an abort, or gone from PiG: it never reached the
			// specialist, and reopens nothing.
			if err := s.store.DropOwnerMessage(ctx, m.Key); err != nil {
				return false, err
			}
		}
	}
	return done, nil
}

// ownerMessagePlaced notes the owner's message m in the transcript, and
// publishes its thread when the noting reopened it. workMu is held.
func (s *Server) ownerMessagePlaced(ctx context.Context, m store.OwnerMessage) error {
	thread, reopened, err := s.store.OwnerMessagePlaced(ctx, m.Key)
	if err != nil {
		return err
	}
	if reopened {
		s.publishReopened(ctx, thread)
	}
	return nil
}

// noteAllOwnerMessages is noteOwnerMessages for every thread with an
// owner's message pocketd follows, under outboxMu per thread: the delivery
// pass's. done says none is left on its way.
func (s *Server) noteAllOwnerMessages(ctx context.Context) (done bool, err error) {
	threads, err := s.store.ThreadsWithOwnerMessages(ctx)
	if err != nil {
		return false, err
	}
	done = true
	var failures []error
	for _, thread := range threads {
		s.outboxMu.Lock()
		noted, err := s.noteOwnerMessages(ctx, thread)
		s.outboxMu.Unlock()
		if err != nil {
			failures = append(failures, fmt.Errorf("the owner's messages to thread T%d: %w", thread, err))
		}
		done = done && noted
	}
	return done && len(failures) == 0, errors.Join(failures...)
}

// successorOf is where closed thread row's work goes on now: the first of
// its successors that is not closed, following at most team.MaxSuccessors
// of them, with its specialist's name; 0 for none. A read that fails names
// none: the refusal says what to do without it.
func (s *Server) successorOf(ctx context.Context, row store.ThreadRow) (int64, string) {
	next := row.Successor
	for i := 0; i < team.MaxSuccessors && next != 0 && next > row.ID; i++ {
		t, err := s.store.Thread(ctx, next)
		if err != nil {
			return 0, ""
		}
		if t.State != store.ThreadClosed {
			return t.ID, s.agentName(t.Agent)
		}
		if t.Successor <= t.ID {
			break
		}
		next = t.Successor
	}
	return 0, ""
}

// reopenTarget is what the rules read of closed thread row (ADR-0061):
// its plan, whether its brief reached its specialist, by pocketd's note or
// PiG's record of its key, and where its work goes on.
func (s *Server) reopenTarget(ctx context.Context, row store.ThreadRow) (team.ReopenTarget, error) {
	t := team.ReopenTarget{ID: row.ID, Plan: row.Plan, PlanTask: row.PlanTask}
	t.Successor, t.SuccessorAgent = s.successorOf(ctx, row)
	switch err := s.briefFirst(ctx, row.ID); {
	case err == nil:
		t.Briefed = row.Brief != ""
	case errors.Is(err, errNoBriefYet):
	default:
		return team.ReopenTarget{}, err
	}
	return t, nil
}

// reopenRefused is the rule's refusal of a message to closed thread row
// from sender, nil when the message may go and reopen it: a plan's thread
// and an unbriefed one never reopen (team.CheckReopen), and one whose
// folder is no longer its own, a worktree pocketd removed included, is
// refused with why (team.FolderGone). failure is a check that could not
// tell, which refuses nothing. The caller holds the thread's cleanup lock
// (lockClean) from this look to the record of the message.
func (s *Server) reopenRefused(ctx context.Context, row store.ThreadRow, sender team.Sender) (refusal, failure error) {
	target, err := s.reopenTarget(ctx, row)
	if err != nil {
		return nil, err
	}
	if err := team.CheckReopen(target, sender); err != nil {
		return err, nil
	}
	var moved folderChanged
	switch err := s.recheckFolder(ctx, row, row.Cwd, row.Branch); {
	case errors.As(err, &moved):
		return team.FolderGone(target, sender, moved.Error()), nil
	case err != nil:
		return nil, fmt.Errorf("check thread T%d's folder: %w", row.ID, err)
	}
	return nil, nil
}

// noteClosed notes closed thread row's messages already in its
// specialist's transcript, under outboxMu, and reads the thread again: one
// may have reopened it.
func (s *Server) noteClosed(ctx context.Context, row store.ThreadRow) (store.ThreadRow, error) {
	s.outboxMu.Lock()
	err := s.noteSendsPlaced(ctx, row.ID)
	s.outboxMu.Unlock()
	if err != nil {
		return store.ThreadRow{}, err
	}
	return s.store.Thread(ctx, row.ID)
}

// closedSince is the refusal of the owner's message to thread, read now
// under outboxMu, when the thread closed after the send looked at it
// (checked false) and the message could not reopen it: the whole check of
// a closed thread's message (reopenRefused), its folder's included, which
// the cleanup lock the send holds keeps as it is, not as it should be
// (Codex review of #121, round 3).
func (s *Server) closedSince(ctx context.Context, thread int64, checked bool) (refusal, err error) {
	// Its messages already in the transcript are noted first: one may have
	// reopened it (Codex review of #121, round 4). outboxMu is held.
	if err := s.noteSendsPlaced(ctx, thread); err != nil {
		return nil, err
	}
	row, err := s.store.Thread(ctx, thread)
	if err != nil || checked || row.State != store.ThreadClosed {
		return nil, err
	}
	return s.reopenRefused(ctx, row, team.FromOwner)
}

// publishReopened publishes thread, which a message just reopened, with
// its pull requests: a client lists a closed thread's only while they are
// open, so one that read them while it was closed has no copy of the rest
// (Codex review of #121, round 2). workMu is held.
func (s *Server) publishReopened(ctx context.Context, thread store.ThreadRow) {
	s.publishThread(thread)
	if !s.publishPullsOf(ctx, thread.ID) {
		// A delivery pass publishes them (Codex review of #121, round 3).
		s.threadRetryMu.Lock()
		if s.pullsRetry == nil {
			s.pullsRetry = map[int64]bool{}
		}
		s.pullsRetry[thread.ID] = true
		s.threadRetryMu.Unlock()
		s.deliverLater()
	}
}

// publishPullsOf publishes thread's pull requests, and says whether it
// could read them. workMu is held.
func (s *Server) publishPullsOf(ctx context.Context, thread int64) bool {
	pulls, err := s.readPullsOf(ctx, thread)
	if err != nil {
		s.log.Warn("read a reopened thread's pull requests", "thread", thread, "err", err)
		return false
	}
	for _, p := range pulls {
		s.publishPull(p)
	}
	return true
}

// readPullsOf is store.PullsOfThread, which tests may fail (failRead).
func (s *Server) readPullsOf(ctx context.Context, thread int64) ([]store.PullRow, error) {
	if s.failRead != nil {
		if err := s.failRead("readPullsOf", thread); err != nil {
			return nil, err
		}
	}
	return s.store.PullsOfThread(ctx, thread)
}

// republishPulls publishes the pull requests of each reopened thread whose
// read failed (publishReopened), under workMu; done says none is left.
func (s *Server) republishPulls(ctx context.Context) (done bool) {
	s.threadRetryMu.Lock()
	ids := make([]int64, 0, len(s.pullsRetry))
	for id := range s.pullsRetry {
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
		if !s.publishPullsOf(ctx, id) {
			done = false
			continue
		}
		s.threadRetryMu.Lock()
		delete(s.pullsRetry, id)
		s.threadRetryMu.Unlock()
	}
	return done
}

// reportRefused is team_message's refusal on closed thread row: what its
// specialist can do instead (team.ReportRefused).
func (s *Server) reportRefused(ctx context.Context, row store.ThreadRow) error {
	target := team.ReopenTarget{ID: row.ID, Plan: row.Plan, PlanTask: row.PlanTask}
	target.Successor, target.SuccessorAgent = s.successorOf(ctx, row)
	return errors.New(team.ReportRefused(target))
}
