package api

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/frostyard/pocketgo/proto"
	"github.com/frostyard/pocketgo/service/internal/store"
	"github.com/frostyard/pocketgo/service/internal/team"
)

// TeamThreads is team_threads (ADR-0071): every thread not closed, with
// who moves it next as the desktop's work board groups it, and the closed
// threads whose records changed in the last team.InboxWindow (list); or
// one thread whole (read). It is a supervisor's session's alone, which
// roleTools offers it and this checks again (team.CheckThreadsCaller). It
// reads pocketd's records alone, in one read transaction a call
// (store.TeamThreads; store.TeamThread, which reads one thread and what
// its mover reads alone), and runs no gh: each pull request is as pocketd
// last read it. It writes nothing, so a call run again reads again.
func (s *Server) TeamThreads(ctx context.Context, call TeamCall, c team.ThreadsCall) (string, error) {
	caller, err := s.callerProfile(ctx, call.Session)
	if err != nil {
		return "", err
	}
	if err := team.CheckThreadsCaller(caller); err != nil {
		return "", err
	}
	if err := team.CheckThreadsCall(c); err != nil {
		return "", err
	}
	now := s.now()
	if c.Action == team.ThreadsList {
		rows, err := s.store.TeamThreads(ctx, team.InboxSince(now))
		if err != nil {
			return "", err
		}
		return team.ThreadsText(s.threadsOf(rows, now, call.Session)), nil
	}
	// Its plan's delivered tasks' threads are read only when its mover reads
	// them (team.Board.ReachesTaskMover; Codex review of #175, round 4).
	whole, err := s.store.TeamThread(ctx, c.Thread, func(rows store.ThreadWholeRows) bool {
		thread, board := readBoard(rows)
		return board.ReachesTaskMover(thread)
	})
	if errors.Is(err, store.ErrNotFound) {
		return "", fmt.Errorf("there is no thread T%d", c.Thread)
	} else if err != nil {
		return "", err
	}
	view := s.threadsView(now, call.Session)
	thread, board := readBoard(whole)
	view.Board = board
	if whole.Thread.ReviewPull != 0 {
		pulls := map[int64]store.PullRow{}
		if whole.ReviewPull != nil {
			pulls[whole.ReviewPull.ID] = *whole.ReviewPull
		}
		view.Reviews[thread.ID] = team.ThreadReview{Pull: reviewedPull(pulls, whole.Thread.ReviewPull), Head: whole.Thread.ReviewHead}
	}
	w := team.ThreadWhole{Thread: thread, WorkerAsked: whole.Thread.WorkerAsked,
		Supervisor: threadSession(whole.Sessions, whole.Thread.SupervisorSession),
		Specialist: threadSession(whole.Sessions, whole.Thread.Session),
		Worker:     threadSession(whole.Sessions, whole.Thread.WorkerSession)}
	for _, row := range whole.Asks {
		w.Asks = append(w.Asks, team.InboxAsk{Ask: wireAsk(row), WithdrawnAt: row.WithdrawnAt})
	}
	w.Pulls = withReviews(whole.Pulls, whole.Reviews)
	if whole.Plan != nil {
		plan := wirePlan(*whole.Plan)
		w.Plan = &plan
	}
	return team.ThreadText(view, w), nil
}

// readBoard is one thread read whole as the protocol has it, with its
// shift, and the board its mover reads: the thread, its open asks, its
// pull requests and its plan, and the delivered tasks' threads read with
// it (store.TeamThread), with their open asks and pull requests.
func readBoard(rows store.ThreadWholeRows) (proto.Thread, team.Board) {
	thread := wireThread(rows.Thread)
	if rows.Shift != nil {
		thread.Shift = wireShift(*rows.Shift)
	}
	board := team.Board{Threads: map[int64]proto.Thread{thread.ID: thread}, Plans: map[int64]proto.Plan{}}
	for _, row := range rows.Asks {
		if row.State == store.AskOpen {
			board.Asks = append(board.Asks, wireAsk(row))
		}
	}
	board.Pulls = withReviews(rows.Pulls, rows.Reviews)
	if rows.Plan != nil {
		plan := wirePlan(*rows.Plan)
		board.Plans[plan.ID] = plan
	}
	for _, row := range rows.TaskThreads {
		task := wireThread(row)
		if sh, ok := rows.TaskShifts[row.ID]; ok {
			task.Shift = wireShift(sh)
		}
		board.Threads[row.ID] = task
	}
	for _, row := range rows.TaskAsks {
		board.Asks = append(board.Asks, wireAsk(row))
	}
	for _, row := range rows.TaskPulls {
		board.Pulls = append(board.Pulls, wireRow(row))
	}
	return thread, board
}

// threadsView is an empty team_threads view, read at now for the calling
// session caller, with how fresh pocketd's pull requests are.
func (s *Server) threadsView(now time.Time, caller int64) team.Threads {
	t := team.Threads{Now: now, Zone: s.zone, Caller: caller, DeskCap: s.deskCap, Reviews: map[int64]team.ThreadReview{},
		Board: team.Board{Threads: map[int64]proto.Thread{}, Plans: map[int64]proto.Plan{}}}
	s.sessionMu.Lock()
	t.PullsStarted, t.PullEvery = s.pullsStarted, s.pullEvery
	s.sessionMu.Unlock()
	return t
}

// threadsOf is what team_threads' list shows of rows, read at now for the
// calling session caller.
func (s *Server) threadsOf(rows store.ThreadsRows, now time.Time, caller int64) team.Threads {
	t := s.threadsView(now, caller)
	t.Desk = rows.Desk
	for _, row := range rows.Threads {
		thread := wireThread(row)
		if sh, ok := rows.Shifts[row.ID]; ok {
			thread.Shift = wireShift(sh)
		}
		t.Board.Threads[row.ID] = thread
		if row.ReviewPull != 0 {
			// A cross-review pocketd started, open or closed: the list shows
			// an open one apart and leaves a closed one out.
			t.Reviews[row.ID] = team.ThreadReview{Pull: reviewedPull(rows.ReviewPulls, row.ReviewPull), Head: row.ReviewHead}
		}
	}
	for _, row := range rows.Asks {
		t.Board.Asks = append(t.Board.Asks, wireAsk(row))
	}
	t.Board.Pulls = withReviews(rows.Pulls, rows.Reviews)
	for id, facts := range rows.Plans {
		t.Board.Plans[id] = wirePlan(facts)
	}
	return t
}

// withReviews are pulls as the protocol has them, each with its verdict
// as the database held them together.
func withReviews(pulls []store.PullRow, reviews map[int64]store.ReviewRow) []proto.PullRequest {
	out := make([]proto.PullRequest, 0, len(pulls))
	for _, row := range pulls {
		pull := wireRow(row)
		if review, ok := reviews[row.ID]; ok {
			pull = withReview(pull, review)
		}
		out = append(out, pull)
	}
	return out
}

// reviewedPull names pull request id, which a cross-review reviews, as
// owner/name#number, or by its record's number when pocketd no longer has
// it.
func reviewedPull(pulls map[int64]store.PullRow, id int64) string {
	if p, ok := pulls[id]; ok {
		return p.Ref()
	}
	return fmt.Sprintf("pull request record %d, which pocketd no longer has", id)
}

// threadSession is session id as team_threads names it, its ID 0 for
// none and its profile and model empty for one not recorded.
func threadSession(sessions map[int64]store.SessionRow, id int64) team.ThreadSession {
	if id == 0 {
		return team.ThreadSession{}
	}
	row := sessions[id]
	return team.ThreadSession{ID: id, Profile: row.Profile, Model: proto.ModelRef{Provider: row.Provider, ID: row.Model}}
}
