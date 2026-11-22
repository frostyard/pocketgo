package store

import (
	"context"
	"database/sql"
	"errors"
)

// A closed thread goes back to open when a message pocketd follows by its
// key reaches its specialist's session (ADR-0061): the owner's message,
// followed in owner_messages from when PiG took it, and a supervisor's
// message kept while the thread was closed (reports.reopens). The noting
// that finds it placed reopens the thread, in its own transaction, once.

// OwnerMessage is the owner's message to a thread's specialist, followed
// by its request key until pocketd notes it placed or gone.
type OwnerMessage struct {
	Key       string
	ThreadID  int64
	SessionID int64
}

// reopenThread takes closed thread id back to open, in tx, and says
// whether it did: its reopened count goes up, reopened_at is now, and
// merged_at_reopen keeps how many of its pull requests had merged, so
// those merges never close it again (team.MergedOut). A plan's thread,
// whose plan settled it at its close, stays closed, and so does a thread
// not closed.
func reopenThread(ctx context.Context, tx *sql.Tx, id, now int64) (bool, error) {
	return changed(tx.ExecContext(ctx, `UPDATE threads SET state = ?, reopened = reopened + 1, reopened_at = ?,
		merged_at_reopen = (SELECT COUNT(*) FROM pull_requests p WHERE p.thread_id = threads.id AND p.state = ?),
		updated_at = MAX(updated_at, ?) WHERE id = ? AND state = ? AND plan = 0`, ThreadOpen, now, PullMerged, now, id, ThreadClosed))
}

// CompleteOwnerMessage completes the request of the owner's message, key,
// with result, as CompleteRequest does, and follows the message to
// thread's specialist session, in one transaction (ADR-0061): PiG has
// taken it, so pocketd notes it once placed. thread is 0 for a session
// that is no thread's specialist's, which follows nothing.
func (s *Store) CompleteOwnerMessage(ctx context.Context, key, result string, thread, session int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `UPDATE requests SET result = ? WHERE key = ?`, result, key); err != nil {
		return err
	}
	if thread != 0 {
		if _, err := tx.ExecContext(ctx, `INSERT INTO owner_messages (key, thread_id, session_id, created_at) VALUES (?, ?, ?, ?)
			ON CONFLICT (key) DO NOTHING`, key, thread, session, s.millis()); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// OwnerMessagesOf are the owner's messages to thread's specialist that
// pocketd follows, in the order they were taken.
func (s *Store) OwnerMessagesOf(ctx context.Context, thread int64) ([]OwnerMessage, error) {
	return queryRows(ctx, s.db, func(scan func(...any) error) (OwnerMessage, error) {
		var m OwnerMessage
		err := scan(&m.Key, &m.ThreadID, &m.SessionID)
		return m, err
	}, `SELECT key, thread_id, session_id FROM owner_messages WHERE thread_id = ? ORDER BY rowid`, thread)
}

// ThreadsWithOwnerMessages are the threads with an owner's message
// pocketd follows, by its oldest one.
func (s *Store) ThreadsWithOwnerMessages(ctx context.Context) ([]int64, error) {
	return queryRows(ctx, s.db, func(scan func(...any) error) (int64, error) {
		var id int64
		err := scan(&id)
		return id, err
	}, `SELECT thread_id FROM owner_messages GROUP BY thread_id ORDER BY MIN(rowid)`)
}

// OwnerMessagePlaced notes the owner's message, key, in its specialist's
// transcript, and stops following it, in one transaction that takes its
// thread from closed back to open (reopenThread): the specialist was given
// work, so its report is expected. Only the noting that ends the follow
// reopens the thread, once. thread is as the transaction leaves it, zero
// when the message was noted already.
func (s *Store) OwnerMessagePlaced(ctx context.Context, key string) (thread ThreadRow, reopened bool, err error) {
	now := s.millis()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ThreadRow{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	var id int64
	err = tx.QueryRowContext(ctx, `DELETE FROM owner_messages WHERE key = ? RETURNING thread_id`, key).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return ThreadRow{}, false, nil
	} else if err != nil {
		return ThreadRow{}, false, err
	}
	if reopened, err = reopenThread(ctx, tx, id, now); err != nil {
		return ThreadRow{}, false, err
	}
	thread, err = scanThread(tx.QueryRowContext(ctx, `SELECT `+threadColumns+` FROM threads WHERE id = ?`, id).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return ThreadRow{}, false, ErrNotFound
	} else if err != nil {
		return ThreadRow{}, false, err
	}
	return thread, reopened, tx.Commit()
}

// DropOwnerMessage stops following the owner's message, key, which never
// reached the transcript: an abort withdrew it, or PiG has none.
func (s *Store) DropOwnerMessage(ctx context.Context, key string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM owner_messages WHERE key = ?`, key)
	return err
}

// MessagesToReopen says whether a message on its way may reopen thread:
// an owner's message pocketd follows, or a supervisor's message kept while
// it was closed and not noted yet. A closed thread's worktree stays while
// one is (ADR-0061).
func (s *Store) MessagesToReopen(ctx context.Context, thread int64) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM owner_messages WHERE thread_id = ?)
		+ (SELECT COUNT(*) FROM reports WHERE thread_id = ? AND reopens != 0 AND sent_at IS NULL)`, thread, thread).Scan(&n)
	return n > 0, err
}
