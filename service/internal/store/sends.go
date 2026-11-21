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
	if _, err := tx.ExecContext(ctx, `INSERT INTO reports (key, thread_id, session_id, text, created_at) VALUES (?, ?, ?, ?, ?)`,
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
// asks withdrawn (ADR-0037).
func sendReopens(ctx context.Context, tx *sql.Tx, key string, id, now int64) (SendNoted, error) {
	var state string
	if err := tx.QueryRowContext(ctx, `SELECT state FROM threads WHERE id = ?`, id).Scan(&state); errors.Is(err, sql.ErrNoRows) {
		return SendNoted{}, ErrNotFound
	} else if err != nil {
		return SendNoted{}, err
	}
	if state != ThreadDone && state != ThreadBlocked {
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

// AsksWithdrawnBy are the asks the supervisor's message of outbox key
// withdrew, oldest first (ADR-0037).
func (s *Store) AsksWithdrawnBy(ctx context.Context, key string) ([]AskRow, error) {
	if key == "" {
		return nil, nil
	}
	return queryRows(ctx, s.db, scanAsk, `SELECT `+askColumns+` FROM asks WHERE withdrawn_by = ? AND withdrawn_by != '' ORDER BY id`, key)
}
