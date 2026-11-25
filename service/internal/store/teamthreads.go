package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/frostyard/pocketgo/service/internal/team"
)

// ThreadsRows are what team_threads reads (ADR-0072), from one read
// transaction: the board the desktop's work board draws, and the threads
// closed in the window.
type ThreadsRows struct {
	// Threads are every thread not closed and every closed one whose
	// record changed at the window's start or later, the most recently
	// changed first, and Shifts their shifts, by thread, without their
	// rounds: the board shows a shift's state and round alone, and a read
	// reads its own thread's rounds (TeamThread).
	Threads []ThreadRow
	Shifts  map[int64]ShiftRow
	// OpenAsks counts the open asks of the threads not closed, by thread:
	// all the list and its movers read of asks, never every open ask in the
	// database (Codex review of #175, round 5).
	OpenAsks map[int64]int
	// Pulls are those threads' pull requests, oldest first, and Reviews
	// the newest verdict of each one's change set, by pull request.
	Pulls   []PullRow
	Reviews map[int64]ReviewRow
	// ReviewPulls are the pull requests the cross-reviews among Threads
	// review, by id.
	ReviewPulls map[int64]PullRow
	// Plans are the plans of the plan threads not closed, by id.
	Plans map[int64]PlanFacts
	// Desk counts the threads on the desk, as its cap does (deskCount).
	Desk int
}

// ThreadWholeRows are one thread as team_threads' read reads it, with
// what its mover reads (team.Board.NextMover) and nothing else of the
// board.
type ThreadWholeRows struct {
	Thread ThreadRow
	// Shift is its shift, nil for none.
	Shift *ShiftRow
	// Asks are every ask of it, oldest first.
	Asks []AskRow
	// Pulls are every pull request it links, oldest first, and Reviews the
	// newest verdict of each one's change set.
	Pulls   []PullRow
	Reviews map[int64]ReviewRow
	// ReviewPull is the pull request a cross-review reviews, nil for any
	// other thread.
	ReviewPull *PullRow
	// Plan is the plan it works for, nil for none.
	Plan *PlanFacts
	// Sessions are its delegating, specialist's and worker's sessions, by
	// id, those recorded.
	Sessions map[int64]SessionRow
	// TaskThreads are the threads of the delivered tasks of the approved
	// or building plan that a plan's own thread not closed leads, with
	// TaskShifts their shifts without rounds, TaskAsks their open asks and
	// TaskPulls their pull requests: who moves such a thread follows its
	// tasks' threads. Empty for any other thread, a closed one's mover
	// being nobody.
	TaskThreads []ThreadRow
	TaskShifts  map[int64]ShiftRow
	TaskAsks    []AskRow
	TaskPulls   []PullRow
}

// TeamThreads reads ThreadsRows in one read transaction, so a thread, its
// shift, its asks and its pull requests are seen as one commit left them.
// since is the window's start: a closed thread whose record changed then
// or later is read too.
func (s *Store) TeamThreads(ctx context.Context, since time.Time) (ThreadsRows, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return ThreadsRows{}, err
	}
	defer func() { _ = tx.Rollback() }()
	return threadsBoardIn(ctx, tx, since)
}

// TeamThread reads thread id whole in one read transaction: its asks,
// pull requests, plan, shift with its rounds and sessions, and only what
// its mover reads, never the rest of the board (Codex review of #175,
// round 2). needsTasks says, from those rows, whether its mover reads its
// plan's delivered tasks' threads (team.Board.ReachesTaskMover): only
// then are they read, in the same transaction (round 4). ErrNotFound says
// there is no thread id.
func (s *Store) TeamThread(ctx context.Context, id int64, needsTasks func(ThreadWholeRows) bool) (ThreadWholeRows, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return ThreadWholeRows{}, err
	}
	defer func() { _ = tx.Rollback() }()
	var w ThreadWholeRows
	w.Thread, err = scanThread(tx.QueryRowContext(ctx, `SELECT `+threadColumns+` FROM threads WHERE id = ?`, id).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return ThreadWholeRows{}, ErrNotFound
	} else if err != nil {
		return ThreadWholeRows{}, err
	}
	switch sh, err := loadShift(ctx, tx, id); {
	case errors.Is(err, ErrNotFound):
	case err != nil:
		return ThreadWholeRows{}, err
	default:
		w.Shift = &sh
	}
	if w.Asks, err = queryRows(ctx, tx, scanAsk, `SELECT `+askColumns+` FROM asks WHERE thread_id = ? ORDER BY id`, id); err != nil {
		return ThreadWholeRows{}, err
	}
	if w.Pulls, err = queryRows(ctx, tx, scanPull, `SELECT `+pullColumns+` FROM pull_requests WHERE thread_id = ? ORDER BY id`, id); err != nil {
		return ThreadWholeRows{}, err
	}
	if w.Reviews, err = reviewsAtChangeSets(ctx, tx, w.Pulls); err != nil {
		return ThreadWholeRows{}, err
	}
	if w.Thread.ReviewPull != 0 {
		switch p, err := scanPull(tx.QueryRowContext(ctx, `SELECT `+pullColumns+` FROM pull_requests WHERE id = ?`, w.Thread.ReviewPull).Scan); {
		case errors.Is(err, sql.ErrNoRows):
		case err != nil:
			return ThreadWholeRows{}, err
		default:
			w.ReviewPull = &p
		}
	}
	if w.Thread.Plan != 0 {
		switch facts, err := loadPlanFacts(ctx, tx, w.Thread.Plan); {
		case errors.Is(err, ErrNotFound):
		case err != nil:
			return ThreadWholeRows{}, err
		default:
			w.Plan = &facts
		}
	}
	w.Sessions = map[int64]SessionRow{}
	for _, session := range []int64{w.Thread.SupervisorSession, w.Thread.Session, w.Thread.WorkerSession} {
		if session == 0 {
			continue
		}
		row, err := scanSession(tx.QueryRowContext(ctx, `SELECT `+sessionColumns+` FROM sessions WHERE id = ?`, session).Scan)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		} else if err != nil {
			return ThreadWholeRows{}, err
		}
		w.Sessions[session] = row
	}
	w.TaskShifts = map[int64]ShiftRow{}
	if needsTasks != nil && needsTasks(w) {
		if err := taskThreadsIn(ctx, tx, &w); err != nil {
			return ThreadWholeRows{}, err
		}
	}
	return w, nil
}

// taskThreadsIn reads into w the delivered tasks' threads of the approved
// or building plan its thread leads, when it is a plan's own thread not
// closed, with their shifts, open asks and pull requests: what
// team.Board.NextMover reads of them (state.Data.taskMover). Any other
// thread needs none of them.
func taskThreadsIn(ctx context.Context, tx *sql.Tx, w *ThreadWholeRows) error {
	f := w.Plan
	if w.Thread.State == ThreadClosed || !w.Thread.IsPlanThread() || f == nil || f.Graph == nil ||
		(f.Plan.State != team.PlanApproved && f.Plan.State != team.PlanBuilding) {
		return nil
	}
	for _, task := range f.Graph.Tasks {
		if task.State != team.TaskDelegated || task.Thread == 0 {
			continue
		}
		t, err := scanThread(tx.QueryRowContext(ctx, `SELECT `+threadColumns+` FROM threads WHERE id = ?`, task.Thread).Scan)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		} else if err != nil {
			return err
		}
		w.TaskThreads = append(w.TaskThreads, t)
		switch sh, err := scanShift(tx.QueryRowContext(ctx, `SELECT `+shiftColumns+` FROM shifts WHERE thread_id = ?`, t.ID).Scan); {
		case errors.Is(err, sql.ErrNoRows):
		case err != nil:
			return err
		default:
			w.TaskShifts[t.ID] = sh
		}
		asks, err := queryRows(ctx, tx, scanAsk, `SELECT `+askColumns+` FROM asks WHERE thread_id = ? AND state = ? ORDER BY id`, t.ID, AskOpen)
		if err != nil {
			return err
		}
		w.TaskAsks = append(w.TaskAsks, asks...)
		pulls, err := queryRows(ctx, tx, scanPull, `SELECT `+pullColumns+` FROM pull_requests WHERE thread_id = ? ORDER BY id`, t.ID)
		if err != nil {
			return err
		}
		w.TaskPulls = append(w.TaskPulls, pulls...)
	}
	return nil
}

// threadsBoardIn reads ThreadsRows in tx, a read transaction.
func threadsBoardIn(ctx context.Context, tx *sql.Tx, since time.Time) (ThreadsRows, error) {
	at := toMillis(since)
	var rows ThreadsRows
	var err error
	if rows.Threads, err = queryRows(ctx, tx, scanThread, `SELECT `+threadColumns+` FROM threads WHERE state != 'closed' OR updated_at >= ?
		ORDER BY updated_at DESC, id DESC`, at); err != nil {
		return ThreadsRows{}, err
	}
	// The shifts of the threads read alone, never the whole history of
	// shifts and rounds (Codex review of #175, round 1).
	shifts, err := queryRows(ctx, tx, scanShift, `SELECT `+shiftColumns+` FROM shifts
		WHERE thread_id IN (SELECT id FROM threads WHERE state != 'closed' OR updated_at >= ?)`, at)
	if err != nil {
		return ThreadsRows{}, err
	}
	rows.Shifts = make(map[int64]ShiftRow, len(shifts))
	for _, sh := range shifts {
		rows.Shifts[sh.ThreadID] = sh
	}
	type count struct {
		thread int64
		n      int
	}
	counts, err := queryRows(ctx, tx, func(scan func(...any) error) (count, error) {
		var c count
		err := scan(&c.thread, &c.n)
		return c, err
	}, `SELECT thread_id, COUNT(*) FROM asks WHERE state = ? AND thread_id IN (SELECT id FROM threads WHERE state != 'closed') GROUP BY thread_id`, AskOpen)
	if err != nil {
		return ThreadsRows{}, err
	}
	rows.OpenAsks = make(map[int64]int, len(counts))
	for _, c := range counts {
		rows.OpenAsks[c.thread] = c.n
	}
	if rows.Pulls, err = queryRows(ctx, tx, scanPull, `SELECT `+pullColumns+` FROM pull_requests
		WHERE thread_id IN (SELECT id FROM threads WHERE state != 'closed' OR updated_at >= ?) ORDER BY id`, at); err != nil {
		return ThreadsRows{}, err
	}
	if rows.Reviews, err = reviewsAtChangeSets(ctx, tx, rows.Pulls); err != nil {
		return ThreadsRows{}, err
	}
	rows.ReviewPulls, rows.Plans = map[int64]PullRow{}, map[int64]PlanFacts{}
	for _, t := range rows.Threads {
		if t.ReviewPull != 0 {
			p, err := scanPull(tx.QueryRowContext(ctx, `SELECT `+pullColumns+` FROM pull_requests WHERE id = ?`, t.ReviewPull).Scan)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return ThreadsRows{}, err
			}
			if err == nil {
				rows.ReviewPulls[p.ID] = p
			}
		}
		if t.State != ThreadClosed && t.IsPlanThread() {
			facts, err := loadPlanFacts(ctx, tx, t.Plan)
			if err != nil && !errors.Is(err, ErrNotFound) {
				return ThreadsRows{}, err
			}
			if err == nil {
				rows.Plans[facts.Plan.ID] = facts
			}
		}
	}
	if rows.Desk, err = deskCount(ctx, tx); err != nil {
		return ThreadsRows{}, err
	}
	return rows, nil
}
