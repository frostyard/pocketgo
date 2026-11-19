package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strconv"
	"time"
)

// Thread states (ADR-0014).
const (
	ThreadStarting = "starting"
	ThreadOpen     = "open"
	ThreadDone     = "done"
	ThreadBlocked  = "blocked"
	ThreadClosed   = "closed"
)

// ThreadRow is a thread: one delegated piece of work.
type ThreadRow struct {
	ID int64
	// Key is the delegation that made it, so a retry finds it.
	Key               string
	Title             string
	Agent             string
	SupervisorSession int64
	// Session is the specialist's session, 0 while the thread starts.
	Session    int64
	Repository string
	Cwd        string
	Branch     string
	Task       string
	State      string
	Report     string
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

const threadColumns = `id, key, title, agent, supervisor_session, session, repository, cwd, branch, task, state, report, created_at, updated_at`

func scanThread(scan func(...any) error) (ThreadRow, error) {
	var row ThreadRow
	var created, updated int64
	err := scan(&row.ID, &row.Key, &row.Title, &row.Agent, &row.SupervisorSession, &row.Session, &row.Repository, &row.Cwd,
		&row.Branch, &row.Task, &row.State, &row.Report, &created, &updated)
	row.CreatedAt, row.UpdatedAt = fromMillis(created), fromMillis(updated)
	return row, err
}

func (s *Store) thread(ctx context.Context, where string, arg any) (ThreadRow, error) {
	row, err := scanThread(s.db.QueryRowContext(ctx, `SELECT `+threadColumns+` FROM threads WHERE `+where, arg).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return ThreadRow{}, ErrNotFound
	}
	return row, err
}

// CreateThread records a starting thread, unless its key made one already:
// then it returns that one, and existed.
func (s *Store) CreateThread(ctx context.Context, row ThreadRow) (ThreadRow, bool, error) {
	now := s.millis()
	result, err := s.db.ExecContext(ctx, `INSERT INTO threads (key, title, agent, supervisor_session, repository, cwd, branch, task, state, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT (key) DO NOTHING`,
		row.Key, row.Title, row.Agent, row.SupervisorSession, row.Repository, row.Cwd, row.Branch, row.Task, ThreadStarting, now, now)
	if err != nil {
		return ThreadRow{}, false, err
	}
	n, _ := result.RowsAffected()
	got, err := s.thread(ctx, `key = ?`, row.Key)
	return got, n == 0, err
}

// ThreadByKey is the thread the delegation key made.
func (s *Store) ThreadByKey(ctx context.Context, key string) (ThreadRow, error) {
	return s.thread(ctx, `key = ?`, key)
}

// FailThread closes a thread that could not start, with why as its
// report, so it leaves the desk. A thread that started is left alone.
func (s *Store) FailThread(ctx context.Context, id int64, why string) (ThreadRow, error) {
	if _, err := s.db.ExecContext(ctx, `UPDATE threads SET state = ?, report = ?, updated_at = ? WHERE id = ? AND state = ?`,
		ThreadClosed, why, s.millis(), id, ThreadStarting); err != nil {
		return ThreadRow{}, err
	}
	return s.Thread(ctx, id)
}

// Thread is thread id.
func (s *Store) Thread(ctx context.Context, id int64) (ThreadRow, error) {
	return s.thread(ctx, `id = ?`, id)
}

// ThreadOfSession is the thread whose specialist works in session.
func (s *Store) ThreadOfSession(ctx context.Context, session int64) (ThreadRow, error) {
	return s.thread(ctx, `session = ?`, session)
}

// Threads are every thread, the most recently updated first.
func (s *Store) Threads(ctx context.Context) ([]ThreadRow, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+threadColumns+` FROM threads ORDER BY updated_at DESC, id DESC`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []ThreadRow
	for rows.Next() {
		row, err := scanThread(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// OpenThreads counts the threads on the desk: every one not closed.
func (s *Store) OpenThreads(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM threads WHERE state != ?`, ThreadClosed).Scan(&n)
	return n, err
}

// AttachThread gives a starting thread its specialist's session and
// folder; the thread stays starting until OpenThread, so a report finds it
// as soon as the brief goes. A thread closed meanwhile, or attached to
// another session, is returned as it is: the caller sends no brief unless
// the row names its session.
func (s *Store) AttachThread(ctx context.Context, id, session int64, cwd, branch string) (ThreadRow, error) {
	if _, err := s.db.ExecContext(ctx, `UPDATE threads SET session = ?, cwd = ?, branch = ?, updated_at = ? WHERE id = ? AND state = ? AND session IN (0, ?)`,
		session, cwd, branch, s.millis(), id, ThreadStarting, session); err != nil {
		return ThreadRow{}, err
	}
	return s.Thread(ctx, id)
}

// OpenThread opens a starting thread whose brief went. A thread that left
// starting meanwhile (a report, or a close) keeps its state.
func (s *Store) OpenThread(ctx context.Context, id int64) (ThreadRow, error) {
	if _, err := s.db.ExecContext(ctx, `UPDATE threads SET state = ?, updated_at = ? WHERE id = ? AND state = ? AND session != 0`,
		ThreadOpen, s.millis(), id, ThreadStarting); err != nil {
		return ThreadRow{}, err
	}
	return s.Thread(ctx, id)
}

// FileReport files report call key on thread id, in one transaction: the
// thread must not be closed, its asks are recorded, and the thread takes
// state and report. A key filed already changes nothing, so a report run
// again after a restart never writes over a newer one; it returns the
// thread as it is and the asks the key made. A thread closed before the
// report is ErrConflict, and nothing of the report is kept.
func (s *Store) FileReport(ctx context.Context, key string, id int64, state, report string, asks []AskRow) (ThreadRow, []AskRow, error) {
	now := s.millis()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ThreadRow{}, nil, err
	}
	defer func() { _ = tx.Rollback() }()
	thread, err := scanThread(tx.QueryRowContext(ctx, `SELECT `+threadColumns+` FROM threads WHERE id = ?`, id).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return ThreadRow{}, nil, ErrNotFound
	} else if err != nil {
		return ThreadRow{}, nil, err
	}
	_, filed, err := claimRequest(ctx, tx, key, "report:"+strconv.FormatInt(id, 10), now)
	if err != nil {
		return ThreadRow{}, nil, err
	}
	if !filed {
		if thread.State == ThreadClosed {
			return ThreadRow{}, nil, ErrConflict
		}
		for _, a := range asks {
			options := a.Options
			if options == nil {
				options = []string{}
			}
			encoded, err := json.Marshal(options)
			if err != nil {
				return ThreadRow{}, nil, err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO asks (key, thread_id, session_id, question, options, state, created_at)
				VALUES (?, ?, ?, ?, ?, ?, ?) ON CONFLICT (key) DO NOTHING`,
				a.Key, id, a.SessionID, a.Question, string(encoded), AskOpen, now); err != nil {
				return ThreadRow{}, nil, err
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE threads SET state = ?, report = ?, updated_at = ? WHERE id = ?`, state, report, now, id); err != nil {
			return ThreadRow{}, nil, err
		}
	}
	thread, err = scanThread(tx.QueryRowContext(ctx, `SELECT `+threadColumns+` FROM threads WHERE id = ?`, id).Scan)
	if err != nil {
		return ThreadRow{}, nil, err
	}
	out := make([]AskRow, 0, len(asks))
	for _, a := range asks {
		row, err := scanAsk(tx.QueryRowContext(ctx, `SELECT `+askColumns+` FROM asks WHERE key = ?`, a.Key).Scan)
		if errors.Is(err, sql.ErrNoRows) {
			continue // filed before this ask was asked of it
		} else if err != nil {
			return ThreadRow{}, nil, err
		}
		out = append(out, row)
	}
	return thread, out, tx.Commit()
}

// CloseThreadAs closes thread id for request key, once, as AnswerAsk
// answers: the key is claimed in the transaction that closes, so a key
// used for another request is ErrRequestReused and closes nothing.
func (s *Store) CloseThreadAs(ctx context.Context, key string, id int64) (ThreadRow, error) {
	now := s.millis()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ThreadRow{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, _, err := claimRequest(ctx, tx, key, "closeThread:"+strconv.FormatInt(id, 10), now); err != nil {
		return ThreadRow{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE threads SET state = ?, updated_at = ? WHERE id = ? AND state != ?`, ThreadClosed, now, id, ThreadClosed); err != nil {
		return ThreadRow{}, err
	}
	row, err := scanThread(tx.QueryRowContext(ctx, `SELECT `+threadColumns+` FROM threads WHERE id = ?`, id).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return ThreadRow{}, ErrNotFound
	} else if err != nil {
		return ThreadRow{}, err
	}
	return row, tx.Commit()
}

// CloseThread closes a thread; closing a closed one changes nothing.
func (s *Store) CloseThread(ctx context.Context, id int64) (ThreadRow, error) {
	if _, err := s.db.ExecContext(ctx, `UPDATE threads SET state = ?, updated_at = ? WHERE id = ? AND state != ?`,
		ThreadClosed, s.millis(), id, ThreadClosed); err != nil {
		return ThreadRow{}, err
	}
	return s.Thread(ctx, id)
}

// Ask states.
const (
	AskOpen     = "open"
	AskAnswered = "answered"
)

// AskRow is a question a thread's specialist put to the owner.
type AskRow struct {
	ID         int64
	Key        string
	ThreadID   int64
	SessionID  int64
	Question   string
	Options    []string
	State      string
	Answer     string
	AnsweredBy string
	CreatedAt  time.Time
	AnsweredAt *time.Time
}

const askColumns = `id, key, thread_id, session_id, question, options, state, answer, answered_by, created_at, answered_at`

func scanAsk(scan func(...any) error) (AskRow, error) {
	var row AskRow
	var options string
	var created int64
	var answered sql.NullInt64
	err := scan(&row.ID, &row.Key, &row.ThreadID, &row.SessionID, &row.Question, &options, &row.State, &row.Answer, &row.AnsweredBy, &created, &answered)
	if err != nil {
		return AskRow{}, err
	}
	if err := json.Unmarshal([]byte(options), &row.Options); err != nil {
		return AskRow{}, err
	}
	row.CreatedAt = fromMillis(created)
	if answered.Valid {
		at := fromMillis(answered.Int64)
		row.AnsweredAt = &at
	}
	return row, nil
}

// CreateAsk records an open ask, unless its key made one already: then it
// returns that one.
func (s *Store) CreateAsk(ctx context.Context, row AskRow) (AskRow, error) {
	options := row.Options
	if options == nil {
		options = []string{}
	}
	encoded, err := json.Marshal(options)
	if err != nil {
		return AskRow{}, err
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO asks (key, thread_id, session_id, question, options, state, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?) ON CONFLICT (key) DO NOTHING`,
		row.Key, row.ThreadID, row.SessionID, row.Question, string(encoded), AskOpen, s.millis()); err != nil {
		return AskRow{}, err
	}
	got, err := scanAsk(s.db.QueryRowContext(ctx, `SELECT `+askColumns+` FROM asks WHERE key = ?`, row.Key).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return AskRow{}, ErrNotFound
	}
	return got, err
}

// Ask is ask id.
func (s *Store) Ask(ctx context.Context, id int64) (AskRow, error) {
	row, err := scanAsk(s.db.QueryRowContext(ctx, `SELECT `+askColumns+` FROM asks WHERE id = ?`, id).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return AskRow{}, ErrNotFound
	}
	return row, err
}

// OpenAsks are the asks that wait for the owner, the oldest first.
func (s *Store) OpenAsks(ctx context.Context) ([]AskRow, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+askColumns+` FROM asks WHERE state = ? ORDER BY created_at, id`, AskOpen)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []AskRow
	for rows.Next() {
		row, err := scanAsk(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// AnswerAsk answers ask id once, as request key: a retry with the same key
// returns the answered ask and replayed; an ask answered already by another
// request is ErrConflict.
func (s *Store) AnswerAsk(ctx context.Context, key string, id int64, answer, by string) (row AskRow, replayed bool, err error) {
	now := s.millis()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return AskRow{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	_, existed, err := claimRequest(ctx, tx, key, "answer:"+strconv.FormatInt(id, 10), now)
	if err != nil {
		return AskRow{}, false, err
	}
	if !existed {
		result, err := tx.ExecContext(ctx, `UPDATE asks SET state = ?, answer = ?, answered_by = ?, answered_at = ? WHERE id = ? AND state = ?`,
			AskAnswered, answer, by, now, id, AskOpen)
		if err != nil {
			return AskRow{}, false, err
		}
		if n, _ := result.RowsAffected(); n == 0 {
			if _, err := scanAsk(tx.QueryRowContext(ctx, `SELECT `+askColumns+` FROM asks WHERE id = ?`, id).Scan); errors.Is(err, sql.ErrNoRows) {
				return AskRow{}, false, ErrNotFound
			} else if err != nil {
				return AskRow{}, false, err
			}
			return AskRow{}, false, ErrConflict
		}
	}
	row, err = scanAsk(tx.QueryRowContext(ctx, `SELECT `+askColumns+` FROM asks WHERE id = ?`, id).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return AskRow{}, false, ErrNotFound
	} else if err != nil {
		return AskRow{}, false, err
	}
	return row, existed, tx.Commit()
}

// UndeliveredAnswers are the answered asks whose answer has not reached
// the asking session yet, the oldest answer first.
func (s *Store) UndeliveredAnswers(ctx context.Context) ([]AskRow, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+askColumns+` FROM asks WHERE state = ? AND delivered_at IS NULL ORDER BY answered_at, id`, AskAnswered)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []AskRow
	for rows.Next() {
		row, err := scanAsk(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// AnswerDelivered notes that ask id's answer reached the asking session.
func (s *Store) AnswerDelivered(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE asks SET delivered_at = ? WHERE id = ? AND state = ? AND delivered_at IS NULL`, s.millis(), id, AskAnswered)
	return err
}
