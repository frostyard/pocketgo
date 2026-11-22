package store

import (
	"context"
	"database/sql"
	"errors"

	"github.com/frostyard/pocketgo/service/internal/team"
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
// reopen_seq keeps the order of GitHub's reads now, so only a merge of a
// pull request a later read found open closes it (team.MergedOut). A plan's thread,
// whose plan settled it at its close, stays closed, and so does a thread
// not closed. Its worker, if any, is retired in the same transaction
// (retired_workers): the close owes its run a stop, and the reopened
// thread's next team_worker starts a worker of its own, so no stop of the
// old one ever reaches the new one's work, and no run of the closed time
// replies to the specialist (Codex review of #121, rounds 3 to 5).
func reopenThread(ctx context.Context, tx *sql.Tx, id, now int64) (bool, error) {
	if _, err := tx.ExecContext(ctx, `INSERT INTO retired_workers (session_id, thread_id, created_at)
		SELECT worker_session, id, ? FROM threads WHERE id = ? AND state = ? AND plan = 0 AND worker_session != 0
		ON CONFLICT (session_id) DO NOTHING`, now, id, ThreadClosed); err != nil {
		return false, err
	}
	return changed(tx.ExecContext(ctx, `UPDATE threads SET state = ?, reopened = reopened + 1, reopened_at = ?,
		reopen_seq = COALESCE((SELECT seq FROM pull_reads WHERE id = 1), 0),
		worker_session = 0, worker_cursor = 0, updated_at = MAX(updated_at, ?) WHERE id = ? AND state = ? AND plan = 0`,
		ThreadOpen, now, now, id, ThreadClosed))
}

// RetiredWorkers are the sessions of the workers reopens retired: no
// thread names them, and each run of theirs is stopped, after a restart
// too (scanWorkers).
func (s *Store) RetiredWorkers(ctx context.Context) ([]int64, error) {
	return queryRows(ctx, s.db, func(scan func(...any) error) (int64, error) {
		var id int64
		err := scan(&id)
		return id, err
	}, `SELECT session_id FROM retired_workers ORDER BY session_id`)
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

// dropAtClose notes each of thread's outbox messages that go only while
// it is open (team.DroppedAtClose) and are not placed as sent, in tx, the
// close's: they go to no one. The close withdrew those still queued before
// it committed, and noted those placed.
func dropAtClose(ctx context.Context, tx *sql.Tx, thread, now int64) error {
	keys, err := queryRows(ctx, tx, func(scan func(...any) error) (string, error) {
		var key string
		err := scan(&key)
		return key, err
	}, `SELECT key FROM reports WHERE thread_id = ? AND sent_at IS NULL`, thread)
	if err != nil {
		return err
	}
	for _, key := range keys {
		if !team.DroppedAtClose(key) {
			continue
		}
		if _, err := tx.ExecContext(ctx, `UPDATE reports SET sent_at = ? WHERE key = ? AND sent_at IS NULL`, now, key); err != nil {
			return err
		}
	}
	return nil
}

// CleanState is closed thread id as its worktree's removal reads it, and
// whether a message on its way may reopen it (MessagesToReopen), from one
// snapshot of the work database: a noting that reopens the thread and
// ends its message's follow is seen whole or not at all (ADR-0061).
func (s *Store) CleanState(ctx context.Context, id int64) (row ThreadRow, waiting bool, err error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return ThreadRow{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	row, err = scanThread(tx.QueryRowContext(ctx, `SELECT `+threadColumns+` FROM threads WHERE id = ?`, id).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return ThreadRow{}, false, ErrNotFound
	} else if err != nil {
		return ThreadRow{}, false, err
	}
	if waiting, err = messagesToReopen(ctx, tx, id); err != nil {
		return ThreadRow{}, false, err
	}
	return row, waiting, tx.Commit()
}

// MessagesToReopen says whether a message on its way may reopen thread:
// an owner's message pocketd follows, or a supervisor's message kept while
// it was closed and not noted yet. A closed thread's worktree stays while
// one is (ADR-0061).
func (s *Store) MessagesToReopen(ctx context.Context, thread int64) (bool, error) {
	return messagesToReopen(ctx, s.db, thread)
}

func messagesToReopen(ctx context.Context, q queryer, thread int64) (bool, error) {
	var n int
	err := q.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM owner_messages WHERE thread_id = ?)
		+ (SELECT COUNT(*) FROM reports WHERE thread_id = ? AND reopens != 0 AND sent_at IS NULL)`, thread, thread).Scan(&n)
	return n > 0, err
}
