package store

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"time"

	"github.com/frostyard/pocketgo/service/internal/team"
)

// ShiftRow is a thread's shift (ADR-0024): its rules, its round and state,
// and each round's verdict.
type ShiftRow struct {
	ThreadID  int64
	Stop      string
	MaxRounds int
	// Deadline is zero for none.
	Deadline time.Time
	CI       bool
	// State is team.ShiftRunning, ShiftSummary, ShiftPaused or ShiftEnded;
	// Reason why it stopped or paused (a team.Shift… reason), Why in
	// words.
	State, Reason, Why string
	// Round is the round under way, from 1: the brief is round 1.
	Round int
	// Failures counts the rounds in a row whose done saw Fingerprint's
	// checks fail.
	Failures    int
	Fingerprint string
	CreatedAt   time.Time
	UpdatedAt   time.Time
	EndedAt     *time.Time
	// Rounds are the verdicts of the rounds that ended, oldest first.
	Rounds []ShiftRound
}

// ShiftRound is one round's verdict, the specialist's STATUS line.
type ShiftRound struct {
	N       int
	Verdict string
	Reason  string
	At      time.Time
}

const shiftColumns = `thread_id, stop, max_rounds, deadline, ci, state, reason, why, round, failures, fingerprint, created_at, updated_at, ended_at`

func scanShift(scan func(...any) error) (ShiftRow, error) {
	var row ShiftRow
	var deadline, ended sql.NullInt64
	var created, updated int64
	if err := scan(&row.ThreadID, &row.Stop, &row.MaxRounds, &deadline, &row.CI, &row.State, &row.Reason, &row.Why, &row.Round,
		&row.Failures, &row.Fingerprint, &created, &updated, &ended); err != nil {
		return ShiftRow{}, err
	}
	if deadline.Valid {
		row.Deadline = fromMillis(deadline.Int64)
	}
	if ended.Valid {
		t := fromMillis(ended.Int64)
		row.EndedAt = &t
	}
	row.CreatedAt, row.UpdatedAt = fromMillis(created), fromMillis(updated)
	return row, nil
}

// loadShift reads thread's shift with its rounds.
func loadShift(ctx context.Context, q queryer, thread int64) (ShiftRow, error) {
	row, err := scanShift(q.QueryRowContext(ctx, `SELECT `+shiftColumns+` FROM shifts WHERE thread_id = ?`, thread).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return ShiftRow{}, ErrNotFound
	} else if err != nil {
		return ShiftRow{}, err
	}
	row.Rounds, err = queryRows(ctx, q, func(scan func(...any) error) (ShiftRound, error) {
		var r ShiftRound
		var at int64
		err := scan(&r.N, &r.Verdict, &r.Reason, &at)
		r.At = fromMillis(at)
		return r, err
	}, `SELECT n, verdict, reason, at FROM shift_rounds WHERE thread_id = ? ORDER BY n`, thread)
	return row, err
}

// Shift is thread's shift, ErrNotFound for a thread with none.
func (s *Store) Shift(ctx context.Context, thread int64) (ShiftRow, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return ShiftRow{}, err
	}
	defer func() { _ = tx.Rollback() }()
	return loadShift(ctx, tx, thread)
}

// LiveShifts are the shifts pocketd drives: running or in their summary
// round, oldest first.
func (s *Store) LiveShifts(ctx context.Context) ([]ShiftRow, error) {
	ids, err := queryRows(ctx, s.db, func(scan func(...any) error) (int64, error) {
		var id int64
		return id, scan(&id)
	}, `SELECT thread_id FROM shifts WHERE state IN (?, ?) ORDER BY thread_id`, team.ShiftRunning, team.ShiftSummary)
	if err != nil {
		return nil, err
	}
	out := make([]ShiftRow, 0, len(ids))
	for _, id := range ids {
		row, err := s.Shift(ctx, id)
		if err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, nil
}

// CreateShiftThread is CreateThread for a delegation with a shift: the
// thread and its shift, running at round 1, are recorded in one
// transaction. A key recorded already returns its thread, as CreateThread
// does, with the shift it was made with.
func (s *Store) CreateShiftThread(ctx context.Context, row ThreadRow, shift ShiftRow) (ThreadRow, bool, error) {
	now := s.millis()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ThreadRow{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	got, existed, err := insertThread(ctx, tx, row, now)
	if err != nil {
		return ThreadRow{}, false, err
	}
	if !existed {
		var deadline any
		if !shift.Deadline.IsZero() {
			deadline = toMillis(shift.Deadline)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO shifts (thread_id, stop, max_rounds, deadline, ci, state, round, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, 1, ?, ?)`, got.ID, shift.Stop, shift.MaxRounds, deadline, shift.CI, team.ShiftRunning, now, now); err != nil {
			return ThreadRow{}, false, err
		}
	}
	return got, existed, tx.Commit()
}

// shiftChange runs change on thread's shift, read in the transaction, and
// reads it back: ErrConflict when want says the shift is not as the
// caller read it.
func (s *Store) shiftChange(ctx context.Context, thread int64, want func(ShiftRow) bool, change func(tx *sql.Tx, row ShiftRow, now int64) error) (ShiftRow, error) {
	now := s.millis()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ShiftRow{}, err
	}
	defer func() { _ = tx.Rollback() }()
	row, err := loadShift(ctx, tx, thread)
	if err != nil {
		return ShiftRow{}, err
	}
	if !want(row) {
		return row, ErrConflict
	}
	if err := change(tx, row, now); err != nil {
		return ShiftRow{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE shifts SET updated_at = ? WHERE thread_id = ?`, now, thread); err != nil {
		return ShiftRow{}, err
	}
	got, err := loadShift(ctx, tx, thread)
	if err != nil {
		return ShiftRow{}, err
	}
	return got, tx.Commit()
}

// queueShiftMessage keeps message for the thread's specialist in the
// outbox, in tx, while the thread is open.
func queueShiftMessage(ctx context.Context, tx *sql.Tx, message ReportRow, now int64) error {
	var state string
	if err := tx.QueryRowContext(ctx, `SELECT state FROM threads WHERE id = ?`, message.ThreadID).Scan(&state); err != nil {
		return err
	}
	if state == ThreadClosed {
		return ErrConflict
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO reports (key, thread_id, session_id, text, created_at) VALUES (?, ?, ?, ?, ?) ON CONFLICT (key) DO NOTHING`,
		message.Key, message.ThreadID, message.SessionID, message.Text, now)
	return err
}

// recordRound keeps round n's verdict, once.
func recordRound(ctx context.Context, tx *sql.Tx, thread int64, n int, verdict, reason string, now int64) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO shift_rounds (thread_id, n, verdict, reason, at) VALUES (?, ?, ?, ?, ?) ON CONFLICT DO NOTHING`,
		thread, n, verdict, reason, now)
	return err
}

// NextShiftRound ends round `from` of thread's running shift with its
// verdict, and starts the next: the round count, the CI failure streak and
// the next round's message, kept in the outbox, in one transaction.
// ErrConflict says the shift is no longer running at that round.
func (s *Store) NextShiftRound(ctx context.Context, thread int64, from int, verdict, reason string, failures int, fingerprint string, message ReportRow) (ShiftRow, error) {
	return s.shiftChange(ctx, thread, func(r ShiftRow) bool { return r.State == team.ShiftRunning && r.Round == from },
		func(tx *sql.Tx, _ ShiftRow, now int64) error {
			if err := recordRound(ctx, tx, thread, from, verdict, reason, now); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE shifts SET round = ?, failures = ?, fingerprint = ? WHERE thread_id = ?`, from+1, failures, fingerprint, thread); err != nil {
				return err
			}
			return queueShiftMessage(ctx, tx, message, now)
		})
}

// SummarizeShift ends round `from` of thread's running shift with its
// verdict and stops the shift for stop (a reason) and why: the shift
// moves to its summary round, whose message is kept in the outbox, in one
// transaction.
func (s *Store) SummarizeShift(ctx context.Context, thread int64, from int, verdict, reason, stop, why string, message ReportRow) (ShiftRow, error) {
	return s.shiftChange(ctx, thread, func(r ShiftRow) bool { return r.State == team.ShiftRunning && r.Round == from },
		func(tx *sql.Tx, _ ShiftRow, now int64) error {
			if verdict != "" {
				if err := recordRound(ctx, tx, thread, from, verdict, reason, now); err != nil {
					return err
				}
			}
			if _, err := tx.ExecContext(ctx, `UPDATE shifts SET state = ?, reason = ?, why = ? WHERE thread_id = ?`, team.ShiftSummary, stop, why, thread); err != nil {
				return err
			}
			return queueShiftMessage(ctx, tx, message, now)
		})
}

// EndShift ends thread's shift for reason and why, from any state in
// from. ErrConflict says it is in none of them.
func (s *Store) EndShift(ctx context.Context, thread int64, reason, why string, from ...string) (ShiftRow, error) {
	return s.shiftChange(ctx, thread, func(r ShiftRow) bool { return slices.Contains(from, r.State) }, func(tx *sql.Tx, _ ShiftRow, now int64) error {
		_, err := tx.ExecContext(ctx, `UPDATE shifts SET state = ?, reason = ?, why = ?, ended_at = ? WHERE thread_id = ?`, team.ShiftEnded, reason, why, now, thread)
		return err
	})
}

// PauseShift pauses thread's running shift for why: the owner wrote to its
// session. ErrConflict says it is not running.
func (s *Store) PauseShift(ctx context.Context, thread int64, why string) (ShiftRow, error) {
	return s.shiftChange(ctx, thread, func(r ShiftRow) bool { return r.State == team.ShiftRunning }, func(tx *sql.Tx, _ ShiftRow, _ int64) error {
		_, err := tx.ExecContext(ctx, `UPDATE shifts SET state = ?, reason = ?, why = ? WHERE thread_id = ?`, team.ShiftPaused, team.ShiftPerson, why, thread)
		return err
	})
}

// ResumeShift goes on with thread's shift at its next round, whose message
// is kept in the outbox, in one transaction: a paused shift, or one that
// ended for a reason ok takes, with rounds left. ErrConflict says it
// cannot.
func (s *Store) ResumeShift(ctx context.Context, thread int64, ok func(ShiftRow) bool, message func(ShiftRow) ReportRow) (ShiftRow, error) {
	return s.shiftChange(ctx, thread, ok, func(tx *sql.Tx, row ShiftRow, now int64) error {
		if _, err := tx.ExecContext(ctx, `UPDATE shifts SET state = ?, reason = '', why = '', ended_at = NULL, round = ? WHERE thread_id = ?`,
			team.ShiftRunning, row.Round+1, thread); err != nil {
			return err
		}
		next := row
		next.Round++
		return queueShiftMessage(ctx, tx, message(next), now)
	})
}

// shiftClosed ends the shift of a thread that closed, in the transaction
// that closes it (threadClosed).
func shiftClosed(ctx context.Context, tx *sql.Tx, thread int64, now int64) error {
	_, err := tx.ExecContext(ctx, `UPDATE shifts SET state = ?, reason = ?, why = ?, ended_at = ?, updated_at = ? WHERE thread_id = ? AND state != ?`,
		team.ShiftEnded, team.ShiftClosed, team.ShiftWhy(team.ShiftClosed, "", 0, 0), now, now, thread, team.ShiftEnded)
	return err
}

// Report is the outbox message key, ErrNotFound for none.
func (s *Store) Report(ctx context.Context, key string) (ReportRow, error) {
	row, err := scanReport(s.db.QueryRowContext(ctx, `SELECT `+reportColumns+` FROM reports WHERE key = ?`, key).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return ReportRow{}, ErrNotFound
	}
	return row, err
}
