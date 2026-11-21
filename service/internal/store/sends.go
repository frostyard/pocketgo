package store

import (
	"context"
	"database/sql"
	"errors"
)

// A supervisor's message to a thread it delegated (ADR-0032) is a row of
// the outbox, the reports table, to the thread's specialist session: the
// delivery passes place it as they place a worker's reply, and a shift's
// round waits for it as for anything owed to the specialist (Owed).

// FileSend keeps a supervisor's message, row, to its thread's specialist
// session, once by its key: a call run again after a restart finds the
// row its first run kept, whatever the thread is now. A new one is kept
// only while the thread is open, done or blocked, and its specialist's
// session is row's: a thread that closed or never started is ErrConflict,
// and nothing is kept.
func (s *Store) FileSend(ctx context.Context, row ReportRow) (ReportRow, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ReportRow{}, err
	}
	defer func() { _ = tx.Rollback() }()
	kept, err := scanReport(tx.QueryRowContext(ctx, `SELECT `+reportColumns+` FROM reports WHERE key = ?`, row.Key).Scan)
	if err == nil {
		return kept, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return ReportRow{}, err
	}
	var state string
	var session int64
	if err := tx.QueryRowContext(ctx, `SELECT state, session FROM threads WHERE id = ?`, row.ThreadID).Scan(&state, &session); errors.Is(err, sql.ErrNoRows) {
		return ReportRow{}, ErrNotFound
	} else if err != nil {
		return ReportRow{}, err
	}
	if (state != ThreadOpen && state != ThreadDone && state != ThreadBlocked) || session == 0 || session != row.SessionID {
		return ReportRow{}, ErrConflict
	}
	// Its text says it unblocks a blocked thread (team.SendText), so its
	// delivery may (ADR-0037).
	if _, err := tx.ExecContext(ctx, `INSERT INTO reports (key, thread_id, session_id, text, created_at, unblocks) VALUES (?, ?, ?, ?, ?, 1)`,
		row.Key, row.ThreadID, row.SessionID, row.Text, s.millis()); err != nil {
		return ReportRow{}, err
	}
	kept, err = scanReport(tx.QueryRowContext(ctx, `SELECT `+reportColumns+` FROM reports WHERE key = ?`, row.Key).Scan)
	if err != nil {
		return ReportRow{}, err
	}
	return kept, tx.Commit()
}

// SendNoted is what the noting of a supervisor's message changed
// (SendDelivered).
type SendNoted struct {
	// Reopened says its thread went back to open, from done or blocked.
	Reopened bool
	// Unblocked says it went from blocked (ADR-0037).
	Unblocked bool
	// Withdrawn are the thread's asks the noting withdrew, as it left
	// them, oldest first.
	Withdrawn []AskRow
}

// SendDelivered notes a supervisor's message, key, in its specialist's
// transcript, and takes its thread, id, from done or blocked back to open
// in the same transaction, so the thread's next report is expected
// (ADR-0032, ADR-0037). A blocked thread's open asks are withdrawn with
// it: they wait for no answer now, the message's row says it unblocked
// the thread, and each ask names the message. An answer that committed
// first stands. Only the noting that changes the message reopens the
// thread, once, and a thread open or closed by then keeps its state.
// thread is as the transaction leaves it.
func (s *Store) SendDelivered(ctx context.Context, key string, id int64) (thread ThreadRow, change SendNoted, err error) {
	now := s.millis()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ThreadRow{}, SendNoted{}, err
	}
	defer func() { _ = tx.Rollback() }()
	noted, err := changed(tx.ExecContext(ctx, `UPDATE reports SET sent_at = ? WHERE key = ? AND thread_id = ? AND sent_at IS NULL`, now, key, id))
	if err != nil {
		return ThreadRow{}, SendNoted{}, err
	}
	if noted {
		if change, err = sendReopens(ctx, tx, key, id, now); err != nil {
			return ThreadRow{}, SendNoted{}, err
		}
	}
	thread, err = scanThread(tx.QueryRowContext(ctx, `SELECT `+threadColumns+` FROM threads WHERE id = ?`, id).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return ThreadRow{}, SendNoted{}, ErrNotFound
	} else if err != nil {
		return ThreadRow{}, SendNoted{}, err
	}
	return thread, change, tx.Commit()
}

// sendReopens is SendDelivered's change to thread id, in tx, once message
// key is noted: a done thread opens, and a blocked one opens with its open
// asks withdrawn (ADR-0037), when the message's text says so: one kept
// before migration 20 leaves a blocked thread blocked, as ADR-0032 had it
// (Codex review of #62, round 1).
func sendReopens(ctx context.Context, tx *sql.Tx, key string, id, now int64) (SendNoted, error) {
	var state string
	if err := tx.QueryRowContext(ctx, `SELECT state FROM threads WHERE id = ?`, id).Scan(&state); errors.Is(err, sql.ErrNoRows) {
		return SendNoted{}, ErrNotFound
	} else if err != nil {
		return SendNoted{}, err
	}
	var unblocks bool
	if err := tx.QueryRowContext(ctx, `SELECT unblocks FROM reports WHERE key = ?`, key).Scan(&unblocks); err != nil {
		return SendNoted{}, err
	}
	if state != ThreadDone && (state != ThreadBlocked || !unblocks) {
		return SendNoted{}, nil
	}
	if _, err := tx.ExecContext(ctx, `UPDATE threads SET state = ?, updated_at = MAX(updated_at, ?) WHERE id = ?`, ThreadOpen, now, id); err != nil {
		return SendNoted{}, err
	}
	if state == ThreadDone {
		return SendNoted{Reopened: true}, nil
	}
	if _, err := tx.ExecContext(ctx, `UPDATE reports SET unblocked_at = ? WHERE key = ?`, now, key); err != nil {
		return SendNoted{}, err
	}
	withdrawn, err := changed(tx.ExecContext(ctx, `UPDATE asks SET state = ?, withdrawn_by = ?, withdrawn_at = ? WHERE thread_id = ? AND state = ?`,
		AskWithdrawn, key, now, id, AskOpen))
	if err != nil {
		return SendNoted{}, err
	}
	change := SendNoted{Reopened: true, Unblocked: true}
	if !withdrawn {
		return change, nil
	}
	if change.Withdrawn, err = queryRows(ctx, tx, scanAsk, `SELECT `+askColumns+` FROM asks WHERE withdrawn_by = ? AND withdrawn_by != '' ORDER BY id`, key); err != nil {
		return SendNoted{}, err
	}
	// A withdrawal changes the gate of the plan whose thread asked, as an
	// answer does (ADR-0020): the plan changed with it, and one it clears
	// may let the supervisor deliver again.
	if _, err := tx.ExecContext(ctx, `UPDATE plans SET updated_at = ? WHERE thread_id = ?`, now, id); err != nil {
		return SendNoted{}, err
	}
	last := change.Withdrawn[len(change.Withdrawn)-1]
	return change, askCleared(ctx, tx, last.ID, true, now)
}

// WithdrawnAsk is an ask a supervisor's message withdrew, as team_send's
// answer names it: its number and the start of its question.
type WithdrawnAsk struct {
	ID       int64
	Question string
}

// AsksWithdrawnBy are the first limit asks the supervisor's message of
// outbox key withdrew, oldest first, each question cut to its first
// question characters, and how many it withdrew in all (ADR-0037): a read
// bounded however many asks the thread had, as team_send's answer names
// no more (Codex review of #62, round 2).
func (s *Store) AsksWithdrawnBy(ctx context.Context, key string, limit, question int) (asks []WithdrawnAsk, total int, err error) {
	if key == "" {
		return nil, 0, nil
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM asks WHERE withdrawn_by = ? AND withdrawn_by != ''`, key).Scan(&total); err != nil {
		return nil, 0, err
	}
	asks, err = queryRows(ctx, s.db, func(scan func(...any) error) (WithdrawnAsk, error) {
		var a WithdrawnAsk
		return a, scan(&a.ID, &a.Question)
	}, `SELECT id, substr(question, 1, ?) FROM asks WHERE withdrawn_by = ? AND withdrawn_by != '' ORDER BY id LIMIT ?`, question, key, limit)
	return asks, total, err
}
