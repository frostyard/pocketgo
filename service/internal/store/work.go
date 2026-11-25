package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// An agent's work session (ADR-0073): team_work starts a session of the
// caller's profile in a fresh worktree of one of its repositories. Its
// record is kept by the call's work key before its worktree is made
// (StartWork), names its session once that is recorded, in the same
// transaction as the session and its task (RecordWorkSession), and keeps
// the cursor its runs' answers were read to (QueueWorkReplies).

// WorkRow is a work session's record.
type WorkRow struct {
	// Key is the work key: the team call's, which keys its worktree's
	// claim (ClaimWorktree) and its session's request too.
	Key string
	// Caller is the session that called team_work: it takes the work
	// session's answers.
	Caller int64
	// Agent is the caller's profile, which the work session runs;
	// Repository the repository as the profile names it, and Checkout its
	// checkout, as they were when the call was recorded.
	Agent, Repository, Checkout string
	// Path and Branch are the worktree's, named from the work key.
	Path, Branch string
	Title, Task  string
	// Stale is why its branch starts from the checkout's HEAD, as the
	// making of its worktree found it (worktree.Start.Stale), "" for a
	// fresh fetch: its task says so, a replayed or recovered one too.
	Stale string
	// Session is the work session, 0 until it started.
	Session int64
	// Cursor is the entry that began the last of its runs whose answer
	// was kept.
	Cursor    int64
	CreatedAt time.Time
}

// ErrWorkSessionTaken is a work record that names another session than
// the one its record was asked for.
var ErrWorkSessionTaken = errors.New("the work names another session already")

const workColumns = `key, caller, agent, repository, checkout, path, branch, title, task, stale, session, cursor, created_at`

// workBySession finds a work by its session, in the words of the partial
// index work_sessions_session, so SQLite reads it (session != 0).
const workBySession = `session = ? AND session != 0`

func scanWork(scan func(...any) error) (WorkRow, error) {
	var row WorkRow
	var created int64
	err := scan(&row.Key, &row.Caller, &row.Agent, &row.Repository, &row.Checkout, &row.Path, &row.Branch, &row.Title, &row.Task,
		&row.Stale, &row.Session, &row.Cursor, &created)
	row.CreatedAt = fromMillis(created)
	return row, err
}

// StartWork records the work row names, by its key, before its worktree
// is made, and returns the record as it is kept: a key recorded already
// keeps its first record, so a call run again finishes the work its first
// run began, with what it named then.
func (s *Store) StartWork(ctx context.Context, row WorkRow) (WorkRow, error) {
	if row.Key == "" || row.Caller == 0 {
		return WorkRow{}, errors.New("a work record needs its key and its caller")
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO work_sessions (key, caller, agent, repository, checkout, path, branch, title, task, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT (key) DO NOTHING`,
		row.Key, row.Caller, row.Agent, row.Repository, row.Checkout, row.Path, row.Branch, row.Title, row.Task, s.millis()); err != nil {
		return WorkRow{}, err
	}
	return s.WorkByKey(ctx, row.Key)
}

// WorkByKey is the work recorded under key.
func (s *Store) WorkByKey(ctx context.Context, key string) (WorkRow, error) {
	row, err := scanWork(s.db.QueryRowContext(ctx, `SELECT `+workColumns+` FROM work_sessions WHERE key = ?`, key).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return WorkRow{}, ErrNotFound
	}
	return row, err
}

// WorkOfSession is the work whose session session is: ErrNotFound for a
// session that is no work session.
func (s *Store) WorkOfSession(ctx context.Context, session int64) (WorkRow, error) {
	if session == 0 {
		return WorkRow{}, ErrNotFound
	}
	row, err := scanWork(s.db.QueryRowContext(ctx, `SELECT `+workColumns+` FROM work_sessions WHERE `+workBySession, session).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return WorkRow{}, ErrNotFound
	}
	return row, err
}

// KeepWorkStale keeps why the worktree of the work keyed key starts from
// its checkout's HEAD, once: the first reason kept stays, since only the
// first making of the worktree fetched.
func (s *Store) KeepWorkStale(ctx context.Context, key, stale string) error {
	if stale == "" {
		return nil
	}
	result, err := s.db.ExecContext(ctx, `UPDATE work_sessions SET stale = ? WHERE key = ? AND stale = ''`, stale, key)
	if err != nil {
		return err
	}
	if n, err := result.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		if _, err := s.WorkByKey(ctx, key); err != nil {
			return err
		}
	}
	return nil
}

// WorkSessionIDs are the sessions of the works whose session started,
// oldest first: a scan of every work reads no task's text (Codex review of
// #174, round 2).
func (s *Store) WorkSessionIDs(ctx context.Context) ([]int64, error) {
	return queryRows(ctx, s.db, func(scan func(...any) error) (int64, error) {
		var id int64
		return id, scan(&id)
	}, `SELECT session FROM work_sessions WHERE session != 0 ORDER BY created_at, key`)
}

// RecordWorkSession records the work session the engine made for the
// work key: the session's row, the key's request, the work's naming of
// the session, and its task as its first note, all in one transaction,
// so no work names a session the work database lacks, and no work session
// lacks its task. Recorded again, it changes nothing; a work that names
// another session already is ErrWorkSessionTaken, with nothing written.
func (s *Store) RecordWorkSession(ctx context.Context, key string, session SessionRow, task NoteRow) (WorkRow, error) {
	var row WorkRow
	_, err := s.createSession(ctx, key, session, func(tx *sql.Tx, now int64) error {
		result, err := tx.ExecContext(ctx, `UPDATE work_sessions SET session = ? WHERE key = ? AND session IN (0, ?)`, session.ID, key, session.ID)
		if err != nil {
			return err
		}
		if n, err := result.RowsAffected(); err != nil {
			return err
		} else if n == 0 {
			current, err := scanWork(tx.QueryRowContext(ctx, `SELECT `+workColumns+` FROM work_sessions WHERE key = ?`, key).Scan)
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("no work recorded under %s: %w", key, ErrNotFound)
			} else if err != nil {
				return err
			}
			return fmt.Errorf("%w: session %d, not %d", ErrWorkSessionTaken, current.Session, session.ID)
		}
		task.SessionID = session.ID
		if err := insertNote(ctx, tx, task, now); err != nil {
			return err
		}
		row, err = scanWork(tx.QueryRowContext(ctx, `SELECT `+workColumns+` FROM work_sessions WHERE key = ?`, key).Scan)
		return err
	})
	return row, err
}

// DropWorkTask drops the work keyed key's task, note taskKey, while it is
// neither sent nor dropped, and keeps notice for its caller once, in one
// transaction: a task its worktree no longer backs goes to no one, and
// the session that called team_work hears so once. A task sent already
// stays, and keeps no notice.
func (s *Store) DropWorkTask(ctx context.Context, key, taskKey string, notice NoteRow) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	now := s.millis()
	var sent bool
	if err := tx.QueryRowContext(ctx, `SELECT sent_at IS NOT NULL FROM notes WHERE key = ?`, taskKey).Scan(&sent); errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("no task %s of the work %s: %w", taskKey, key, ErrNotFound)
	} else if err != nil {
		return err
	}
	if sent {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `UPDATE notes SET dropped_at = ? WHERE key = ? AND sent_at IS NULL AND dropped_at IS NULL`, now, taskKey); err != nil {
		return err
	}
	if err := insertNote(ctx, tx, notice, now); err != nil {
		return err
	}
	return tx.Commit()
}

// QueueWorkReplies keeps replies, each a note to its work's calling
// session once by its key, and moves session's cursor on to cursor, in
// one transaction: a reply kept is never read again, and a read again
// keeps none twice. The cursor only moves forward.
func (s *Store) QueueWorkReplies(ctx context.Context, session int64, replies []NoteRow, cursor int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	now := s.millis()
	for _, reply := range replies {
		if err := insertNote(ctx, tx, reply, now); err != nil {
			return err
		}
	}
	result, err := tx.ExecContext(ctx, `UPDATE work_sessions SET cursor = MAX(cursor, ?) WHERE `+workBySession, cursor, session)
	if err != nil {
		return err
	}
	if n, err := result.RowsAffected(); err != nil {
		return err
	} else if n == 0 && session != 0 {
		return fmt.Errorf("session %d is no work session: %w", session, ErrNotFound)
	}
	return tx.Commit()
}
