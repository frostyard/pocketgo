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
	// StopDue says the owner's stop still owes the abort of the runs of
	// the specialist or its worker: the driver aborts them until both are
	// idle (ADR-0024).
	StopDue   bool
	CreatedAt time.Time
	UpdatedAt time.Time
	EndedAt   *time.Time
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

const shiftColumns = `thread_id, stop, max_rounds, deadline, ci, state, reason, why, round, failures, fingerprint, stop_due, created_at, updated_at, ended_at`

func scanShift(scan func(...any) error) (ShiftRow, error) {
	var row ShiftRow
	var deadline, ended sql.NullInt64
	var created, updated int64
	if err := scan(&row.ThreadID, &row.Stop, &row.MaxRounds, &deadline, &row.CI, &row.State, &row.Reason, &row.Why, &row.Round,
		&row.Failures, &row.Fingerprint, &row.StopDue, &created, &updated, &ended); err != nil {
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
// round, or owing a stop's abort, oldest first.
func (s *Store) LiveShifts(ctx context.Context) ([]ShiftRow, error) {
	ids, err := queryRows(ctx, s.db, func(scan func(...any) error) (int64, error) {
		var id int64
		return id, scan(&id)
	}, `SELECT thread_id FROM shifts WHERE state IN (?, ?) OR stop_due != 0 ORDER BY thread_id`, team.ShiftRunning, team.ShiftSummary)
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

// ShiftClaim is the owner's request a change of a shift claims, in the
// transaction that changes it: the same request again changes nothing,
// and reads the shift as it is.
type ShiftClaim struct{ Key, Kind string }

// shiftChange runs change on thread's shift, read in the transaction, and
// reads it back: ErrConflict when want says the shift is not as the
// caller read it. With claim, a request claimed already changes nothing
// and returns the shift as it is (replayed); another request's key is
// ErrRequestReused.
func (s *Store) shiftChange(ctx context.Context, thread int64, claim *ShiftClaim, want func(ShiftRow) bool, change func(tx *sql.Tx, row ShiftRow, now int64) error) (row ShiftRow, replayed bool, err error) {
	now := s.millis()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ShiftRow{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	row, err = loadShift(ctx, tx, thread)
	if err != nil {
		return ShiftRow{}, false, err
	}
	if claim != nil {
		_, existed, err := claimRequest(ctx, tx, claim.Key, claim.Kind, now)
		if err != nil {
			return ShiftRow{}, false, err
		}
		if existed {
			return row, true, nil
		}
	}
	if !want(row) {
		return row, false, ErrConflict
	}
	if err := change(tx, row, now); err != nil {
		return ShiftRow{}, false, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE shifts SET updated_at = ? WHERE thread_id = ?`, now, thread); err != nil {
		return ShiftRow{}, false, err
	}
	got, err := loadShift(ctx, tx, thread)
	if err != nil {
		return ShiftRow{}, false, err
	}
	return got, false, tx.Commit()
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
	row, _, err := s.shiftChange(ctx, thread, nil, func(r ShiftRow) bool { return r.State == team.ShiftRunning && r.Round == from },
		func(tx *sql.Tx, _ ShiftRow, now int64) error {
			if err := recordRound(ctx, tx, thread, from, verdict, reason, now); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE shifts SET round = ?, failures = ?, fingerprint = ? WHERE thread_id = ?`, from+1, failures, fingerprint, thread); err != nil {
				return err
			}
			return queueShiftMessage(ctx, tx, message, now)
		})
	return row, err
}

// SummarizeShift ends round `from` of thread's running shift with its
// verdict and stops the shift for stop (a reason) and why, with the CI
// failure streak as it stands then: the shift moves to its summary round,
// whose message is kept in the outbox, in one transaction.
func (s *Store) SummarizeShift(ctx context.Context, thread int64, from int, verdict, reason string, failures int, fingerprint, stop, why string, message ReportRow) (ShiftRow, error) {
	row, _, err := s.shiftChange(ctx, thread, nil, func(r ShiftRow) bool { return r.State == team.ShiftRunning && r.Round == from },
		func(tx *sql.Tx, _ ShiftRow, now int64) error {
			if verdict != "" {
				if err := recordRound(ctx, tx, thread, from, verdict, reason, now); err != nil {
					return err
				}
			}
			if _, err := tx.ExecContext(ctx, `UPDATE shifts SET state = ?, reason = ?, why = ?, failures = ?, fingerprint = ? WHERE thread_id = ?`,
				team.ShiftSummary, stop, why, failures, fingerprint, thread); err != nil {
				return err
			}
			return queueShiftMessage(ctx, tx, message, now)
		})
	return row, err
}

// ShiftReport is the report pocketd files for its supervisor when a shift
// ends with none of the specialist's: its key, the thread's state and its
// text, as FileReport takes them.
type ShiftReport struct {
	Key, State, Message string
	Text                func(asks []int64) string
}

// EndShift ends thread's shift for reason and why, from any state in from,
// and files report, made from the shift as it ended, on its thread in the
// same transaction, so the supervisor is told once whatever stops pocketd
// (ADR-0024; Codex review of #36). With stopDue, the runs of the
// specialist and its worker are still to abort. With claim, the owner's
// request is claimed with the change.
func (s *Store) EndShift(ctx context.Context, thread int64, reason, why string, stopDue bool, claim *ShiftClaim, report func(ShiftRow) ShiftReport, from ...string) (ShiftRow, ThreadRow, ReportRow, bool, error) {
	var updated ThreadRow
	var message ReportRow
	row, replayed, err := s.shiftChange(ctx, thread, claim, func(r ShiftRow) bool { return slices.Contains(from, r.State) }, func(tx *sql.Tx, _ ShiftRow, now int64) error {
		if _, err := tx.ExecContext(ctx, `UPDATE shifts SET state = ?, reason = ?, why = ?, ended_at = ?, stop_due = ? WHERE thread_id = ?`,
			team.ShiftEnded, reason, why, now, stopDue, thread); err != nil {
			return err
		}
		ended, err := loadShift(ctx, tx, thread)
		if err != nil {
			return err
		}
		r := report(ended)
		updated, _, message, _, err = fileReport(ctx, tx, now, r.Key, thread, r.State, r.Message, nil, r.Text)
		return err
	})
	if replayed {
		updated, err = s.Thread(ctx, thread)
	}
	return row, updated, message, replayed, err
}

// StopSettled says the owner's stop of thread's shift owes no abort any
// more: the runs it stopped are over.
func (s *Store) StopSettled(ctx context.Context, thread int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE shifts SET stop_due = 0 WHERE thread_id = ?`, thread)
	return err
}

// ResumeShift goes on with thread's shift at its next round, whose message
// is kept in the outbox, with the owner's request claimed, in one
// transaction: a paused shift, or one that ended for a reason ok takes,
// with rounds left. A stop's abort still owed is dropped: the resumed run
// is no stop's. ErrConflict says it cannot.
func (s *Store) ResumeShift(ctx context.Context, thread int64, claim *ShiftClaim, ok func(ShiftRow) bool, message func(ShiftRow) ReportRow) (ShiftRow, bool, error) {
	return s.shiftChange(ctx, thread, claim, ok, func(tx *sql.Tx, row ShiftRow, now int64) error {
		if _, err := tx.ExecContext(ctx, `UPDATE shifts SET state = ?, reason = '', why = '', ended_at = NULL, stop_due = 0, round = ? WHERE thread_id = ?`,
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

// AnswerOwed says whether an answer to an ask of session waits to reach
// it: a shift's round that waits on it is not over (ADR-0024).
func (s *Store) AnswerOwed(ctx context.Context, session int64) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM asks WHERE session_id = ? AND state = ? AND delivered_at IS NULL`, session, AskAnswered).Scan(&n)
	return n > 0, err
}

// RecordSend records the owner's message to a session that took effect,
// as RecordRequest does, and with its first record, in the same
// transaction, pauses the running shift of thread, whose specialist's
// session it is (0 for none): a retry of the message pauses nothing again
// (ADR-0024; Codex review of #36). The owner's message also owes a stop
// nothing: the owner directs the session now, and no stop's abort reaches
// that run. paused says the shift paused.
func (s *Store) RecordSend(ctx context.Context, key, kind, result string, thread int64, why string) (paused bool, err error) {
	now := s.millis()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx, `INSERT INTO requests (key, kind, result, created_at) VALUES (?, ?, ?, ?) ON CONFLICT (key) DO NOTHING`, key, kind, result, now)
	if err != nil {
		return false, err
	}
	if n, err := res.RowsAffected(); err != nil {
		return false, err
	} else if n == 0 || thread == 0 {
		return false, tx.Commit()
	}
	if _, err := tx.ExecContext(ctx, `UPDATE shifts SET stop_due = 0 WHERE thread_id = ?`, thread); err != nil {
		return false, err
	}
	res, err = tx.ExecContext(ctx, `UPDATE shifts SET state = ?, reason = ?, why = ?, updated_at = ? WHERE thread_id = ? AND state = ?`,
		team.ShiftPaused, team.ShiftPerson, why, now, thread, team.ShiftRunning)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, tx.Commit()
}
