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

// SendDelivered notes a supervisor's message, key, in its specialist's
// transcript, and takes its thread, id, from done back to open in the same
// transaction, so the thread's next report is expected (ADR-0032): only the
// noting that changes the message reopens it, once, and a thread blocked,
// open or closed by then keeps its state. reopened says the thread opened;
// thread is as the transaction leaves it.
func (s *Store) SendDelivered(ctx context.Context, key string, id int64) (thread ThreadRow, reopened bool, err error) {
	now := s.millis()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ThreadRow{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	noted, err := changed(tx.ExecContext(ctx, `UPDATE reports SET sent_at = ? WHERE key = ? AND thread_id = ? AND sent_at IS NULL`, now, key, id))
	if err != nil {
		return ThreadRow{}, false, err
	}
	if noted {
		if reopened, err = changed(tx.ExecContext(ctx, `UPDATE threads SET state = ?, updated_at = MAX(updated_at, ?) WHERE id = ? AND state = ?`,
			ThreadOpen, now, id, ThreadDone)); err != nil {
			return ThreadRow{}, false, err
		}
	}
	thread, err = scanThread(tx.QueryRowContext(ctx, `SELECT `+threadColumns+` FROM threads WHERE id = ?`, id).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return ThreadRow{}, false, ErrNotFound
	} else if err != nil {
		return ThreadRow{}, false, err
	}
	return thread, reopened, tx.Commit()
}
