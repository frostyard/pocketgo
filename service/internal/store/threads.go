package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"github.com/frostyard/pocketgo/proto"
	"github.com/frostyard/pocketgo/service/internal/team"
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
	// Owner is the thread's random identity, unique across data folders:
	// it marks what the thread makes outside pocketd, as its worktree's
	// branch.
	Owner string
	// TaskID is the delegating call's tool task: once it ended, a thread
	// still starting will not start.
	TaskID int64
	// Brief is the brief, kept once the thread has its session; its
	// submission and attempt are as an AskRow's, and Briefed is set once
	// it is in the specialist's transcript.
	Brief           string
	BriefSubmission int64
	BriefAttempt    int
	Briefed         bool
	// ReviewPull and ReviewHead name the pull request and the head a
	// review thread reviews, which pocketd started (ADR-0019); 0 and ""
	// for any other thread.
	ReviewPull int64
	ReviewHead string
	// ReviewBase is where the review's head leaves its base branch, as
	// GitHub's compare read it when the review started; "" before.
	ReviewBase string
	// WorkerSession is the session of the thread's worker (ADR-0021), 0
	// for none yet; WorkerCursor the entry that began the last of its
	// runs whose reply was kept.
	WorkerSession int64
	WorkerCursor  int64
	// WorkerAsked is the worker model the supervisor asked for, for the
	// owner, when it delegated the thread (ADR-0022): an entry of the
	// owner's list; zero for none.
	WorkerAsked proto.ModelRef
	// Plan is the plan the thread works for (ADR-0020), 0 for none;
	// PlanTask the task of it the thread delivers, "" for the plan
	// thread itself, whose specialist plans.
	Plan      int64
	PlanTask  string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// IsPlanThread says whether the thread is a plan's own: its specialist
// plans, and it takes no place on the desk.
func (t ThreadRow) IsPlanThread() bool { return t.Plan != 0 && t.PlanTask == "" }

const threadColumns = `id, key, title, agent, supervisor_session, session, repository, cwd, branch, task, state, report, owner,
	task_id, brief, brief_submission, brief_attempt, briefed_at IS NOT NULL, review_pull, review_head, review_base, worker_session,
	worker_cursor, worker_provider, worker_model, plan, plan_task, created_at, updated_at`

func scanThread(scan func(...any) error) (ThreadRow, error) {
	var row ThreadRow
	var created, updated int64
	err := scan(&row.ID, &row.Key, &row.Title, &row.Agent, &row.SupervisorSession, &row.Session, &row.Repository, &row.Cwd,
		&row.Branch, &row.Task, &row.State, &row.Report, &row.Owner, &row.TaskID, &row.Brief, &row.BriefSubmission, &row.BriefAttempt, &row.Briefed,
		&row.ReviewPull, &row.ReviewHead, &row.ReviewBase, &row.WorkerSession, &row.WorkerCursor, &row.WorkerAsked.Provider, &row.WorkerAsked.ID,
		&row.Plan, &row.PlanTask, &created, &updated)
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

// CreateThread records a starting thread with a random owner, unless its
// key made one already: then it returns that one, and existed.
func (s *Store) CreateThread(ctx context.Context, row ThreadRow) (ThreadRow, bool, error) {
	return s.createThread(ctx, row, nil)
}

// CreateReviewThread is CreateThread for a review pocketd starts
// (ADR-0019), only while the pull request is, as stored now, open, no
// draft and at the head it reviews, in the transaction that records it: a
// review planned from an older read takes no place and spends no budget
// (Codex review of #26, round 2). ErrConflict says the pull request moved.
//
// base is the base branch the review's key names: a retarget recorded
// meanwhile is a conflict too (Codex review of #26, round 4). readSeq is
// the read the review was planned from: a later read, which may have
// failed, is a conflict too (round 10).
func (s *Store) CreateReviewThread(ctx context.Context, row ThreadRow, base string, readSeq int64) (ThreadRow, bool, error) {
	return s.createThread(ctx, row, func(tx *sql.Tx) error {
		var state, head, branch string
		var draft bool
		var seq int64
		err := tx.QueryRowContext(ctx, `SELECT state, draft, head_sha, base, read_seq FROM pull_requests WHERE id = ?`, row.ReviewPull).
			Scan(&state, &draft, &head, &branch, &seq)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return err
		}
		if state != PullOpen || draft || head != row.ReviewHead || branch != base || seq != readSeq {
			return ErrConflict
		}
		return nil
	})
}

func (s *Store) createThread(ctx context.Context, row ThreadRow, check func(tx *sql.Tx) error) (ThreadRow, bool, error) {
	now := s.millis()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ThreadRow{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	if check != nil {
		if err := check(tx); err != nil {
			return ThreadRow{}, false, err
		}
	}
	// Read back in the transaction: a row that commits is one the caller
	// has, to publish.
	got, existed, err := insertThread(ctx, tx, row, now)
	if err != nil {
		return ThreadRow{}, false, err
	}
	return got, existed, tx.Commit()
}

// updateThread runs one change of thread id and reads the row back in the
// same transaction, so a change that commits is a row the caller has, to
// publish: none commits unseen.
// A change that closes a thread of a plan settles the plan in the same
// transaction (threadClosed).
func (s *Store) updateThread(ctx context.Context, id int64, query string, args ...any) (ThreadRow, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ThreadRow{}, err
	}
	defer func() { _ = tx.Rollback() }()
	before, err := scanThread(tx.QueryRowContext(ctx, `SELECT `+threadColumns+` FROM threads WHERE id = ?`, id).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return ThreadRow{}, ErrNotFound
	} else if err != nil {
		return ThreadRow{}, err
	}
	if _, err := tx.ExecContext(ctx, query, args...); err != nil {
		return ThreadRow{}, err
	}
	row, err := scanThread(tx.QueryRowContext(ctx, `SELECT `+threadColumns+` FROM threads WHERE id = ?`, id).Scan)
	if err != nil {
		return ThreadRow{}, err
	}
	if err := threadClosed(ctx, tx, before, row, s.millis()); err != nil {
		return ThreadRow{}, err
	}
	return row, tx.Commit()
}

// ThreadByKey is the thread the delegation key made.
func (s *Store) ThreadByKey(ctx context.Context, key string) (ThreadRow, error) {
	return s.thread(ctx, `key = ?`, key)
}

// FailThread closes a thread that could not start, with why as its
// report, so it leaves the desk. A thread that started is left alone.
func (s *Store) FailThread(ctx context.Context, id int64, why string) (ThreadRow, error) {
	return s.updateThread(ctx, id, `UPDATE threads SET state = ?, report = ?, updated_at = ? WHERE id = ? AND state = ?`,
		ThreadClosed, why, s.millis(), id, ThreadStarting)
}

// FailUnbriefed closes a starting or open thread whose brief never
// reached its specialist (none noted in the transcript), with why as its
// report: it cannot start. A thread closed, reported on, or whose brief was
// noted is left alone.
func (s *Store) FailUnbriefed(ctx context.Context, id int64, why string) (ThreadRow, error) {
	return s.updateThread(ctx, id, `UPDATE threads SET state = ?, report = ?, updated_at = ? WHERE id = ? AND state IN (?, ?) AND briefed_at IS NULL`,
		ThreadClosed, why, s.millis(), id, ThreadStarting, ThreadOpen)
}

// Thread is thread id.
func (s *Store) Thread(ctx context.Context, id int64) (ThreadRow, error) {
	return s.thread(ctx, `id = ?`, id)
}

// ThreadsOnDesk are the threads not closed, by id (threads_open).
func (s *Store) ThreadsOnDesk(ctx context.Context) ([]ThreadRow, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+threadColumns+` FROM threads WHERE state != 'closed' ORDER BY id`)
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

// ThreadOfSession is the thread whose specialist works in session. The
// predicate repeats threads_session's, so SQLite reads that index.
func (s *Store) ThreadOfSession(ctx context.Context, session int64) (ThreadRow, error) {
	return s.thread(ctx, `session = ? AND session != 0`, session)
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

// OpenThreads counts the threads on the desk: every one not closed, but
// a plan's own thread, which takes no place there (ADR-0020).
func (s *Store) OpenThreads(ctx context.Context) (int, error) {
	var n int
	// The literal predicate is threads_open's: the count reads the desk,
	// never the closed history.
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM threads WHERE state != 'closed' AND (plan = 0 OR plan_task != '')`).Scan(&n)
	return n, err
}

// AttachThread gives a starting thread its specialist's session, folder
// and brief; the thread stays starting until OpenThread, so a report finds it
// as soon as the brief goes. A thread closed meanwhile, or attached to
// another session, is returned as it is: the caller sends no brief unless
// the row names its session.
func (s *Store) AttachThread(ctx context.Context, id, session int64, cwd, branch, brief string) (ThreadRow, error) {
	return s.updateThread(ctx, id, `UPDATE threads SET session = ?, cwd = ?, branch = ?, brief = ?, updated_at = ? WHERE id = ? AND state = ? AND session IN (0, ?)`,
		session, cwd, branch, brief, s.millis(), id, ThreadStarting, session)
}

// RecordThreadSession records a thread's specialist session (as
// CreateSession) and attaches it to thread id (as AttachThread) in one
// transaction: no session of a thread is ever recorded, and published,
// without its thread. It returns the thread as it is then.
func (s *Store) RecordThreadSession(ctx context.Context, key string, session SessionRow, id int64, cwd, branch, brief string) (ThreadRow, error) {
	var thread ThreadRow
	_, err := s.createSession(ctx, key, session, func(tx *sql.Tx, now int64) error {
		if _, err := tx.ExecContext(ctx, `UPDATE threads SET session = ?, cwd = ?, branch = ?, brief = ?, updated_at = ? WHERE id = ? AND state = ? AND session IN (0, ?)`,
			session.ID, cwd, branch, brief, now, id, ThreadStarting, session.ID); err != nil {
			return err
		}
		var err error
		thread, err = scanThread(tx.QueryRowContext(ctx, `SELECT `+threadColumns+` FROM threads WHERE id = ?`, id).Scan)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	})
	return thread, err
}

// OpenThread opens a starting thread whose brief went. A thread that left
// starting meanwhile (a report, or a close) keeps its state.
func (s *Store) OpenThread(ctx context.Context, id int64) (ThreadRow, error) {
	return s.updateThread(ctx, id, `UPDATE threads SET state = ?, updated_at = ? WHERE id = ? AND state = ? AND session != 0`,
		ThreadOpen, s.millis(), id, ThreadStarting)
}

// ReportRow is a report's message to the supervisor's session, kept until
// it went, so a send that fails is tried again with the same key.
type ReportRow struct {
	Key       string
	ThreadID  int64
	SessionID int64
	Text      string
	// Submission and Attempt are as an AskRow's.
	Submission int64
	Attempt    int
	SentAt     *time.Time
}

// FileReport files report call key on thread id, in one transaction: the
// thread must not be closed, its asks are recorded, the thread takes state
// and report, and the supervisor's message (text, given the asks' ids) is
// kept to send. A key filed already changes nothing, so a report run again
// after a restart never writes over a newer one: it returns the thread as
// it is, the asks the key made and its message. A thread closed before the
// report is ErrConflict, and nothing of the report is kept.
func (s *Store) FileReport(ctx context.Context, key string, id int64, state, report string, asks []AskRow,
	text func(asks []int64) string) (ThreadRow, []AskRow, ReportRow, error) {
	thread, filed, message, _, err := s.FileReportWithPulls(ctx, key, id, state, report, asks, nil,
		func(asks []int64, _ []PullRow) string { return text(asks) })
	return thread, filed, message, err
}

// FileReportWithPulls is FileReport that also links pulls, as GitHub has
// them, to thread id, in the same transaction (ADR-0016): one another
// thread links is ErrPullTaken, and nothing of the report is kept. The
// supervisor's message (text) is written from the rows the transaction
// keeps, a newer read of one included, never from pulls as they came. It
// returns those rows; a key filed already links none.
func (s *Store) FileReportWithPulls(ctx context.Context, key string, id int64, state, report string, asks []AskRow, pulls []PullRow,
	text func(asks []int64, pulls []PullRow) string) (ThreadRow, []AskRow, ReportRow, []PullRow, error) {
	now := s.millis()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ThreadRow{}, nil, ReportRow{}, nil, err
	}
	defer func() { _ = tx.Rollback() }()
	var filed int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM reports WHERE key = ?`, key).Scan(&filed); err != nil {
		return ThreadRow{}, nil, ReportRow{}, nil, err
	}
	var linked []PullRow
	if filed == 0 {
		for _, p := range pulls {
			row, err := linkPull(ctx, tx, now, id, p)
			if err != nil {
				return ThreadRow{}, nil, ReportRow{}, nil, err
			}
			linked = append(linked, row)
		}
	}
	thread, out, message, _, err := fileReport(ctx, tx, now, key, id, state, report, asks,
		func(asks []int64) string { return text(asks, linked) })
	if err != nil {
		return ThreadRow{}, nil, ReportRow{}, nil, err
	}
	return thread, out, message, linked, tx.Commit()
}

// fileReport is FileReport's work in tx; before says key was filed
// already.
func fileReport(ctx context.Context, tx *sql.Tx, now int64, key string, id int64, state, report string, asks []AskRow,
	text func(asks []int64) string) (ThreadRow, []AskRow, ReportRow, bool, error) {
	thread, err := scanThread(tx.QueryRowContext(ctx, `SELECT `+threadColumns+` FROM threads WHERE id = ?`, id).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return ThreadRow{}, nil, ReportRow{}, false, ErrNotFound
	} else if err != nil {
		return ThreadRow{}, nil, ReportRow{}, false, err
	}
	message, err := scanReport(tx.QueryRowContext(ctx, `SELECT `+reportColumns+` FROM reports WHERE key = ?`, key).Scan)
	filed := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return ThreadRow{}, nil, ReportRow{}, false, err
	}
	if !filed && thread.State == ThreadClosed {
		return ThreadRow{}, nil, ReportRow{}, false, ErrConflict
	}
	var out []AskRow
	for _, a := range asks {
		if !filed {
			options := a.Options
			if options == nil {
				options = []string{}
			}
			encoded, err := json.Marshal(options)
			if err != nil {
				return ThreadRow{}, nil, ReportRow{}, false, err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO asks (key, thread_id, session_id, question, options, state, created_at)
				VALUES (?, ?, ?, ?, ?, ?, ?) ON CONFLICT (key) DO NOTHING`,
				a.Key, id, a.SessionID, a.Question, string(encoded), AskOpen, now); err != nil {
				return ThreadRow{}, nil, ReportRow{}, false, err
			}
		}
		row, err := scanAsk(tx.QueryRowContext(ctx, `SELECT `+askColumns+` FROM asks WHERE key = ?`, a.Key).Scan)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		} else if err != nil {
			return ThreadRow{}, nil, ReportRow{}, false, err
		}
		out = append(out, row)
	}
	if !filed {
		ids := make([]int64, len(out))
		for i, a := range out {
			ids[i] = a.ID
		}
		message = ReportRow{Key: key, ThreadID: id, SessionID: thread.SupervisorSession, Text: text(ids)}
		if _, err := tx.ExecContext(ctx, `INSERT INTO reports (key, thread_id, session_id, text, created_at) VALUES (?, ?, ?, ?, ?)`,
			key, id, message.SessionID, message.Text, now); err != nil {
			return ThreadRow{}, nil, ReportRow{}, false, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE threads SET state = ?, report = ?, updated_at = ? WHERE id = ?`, state, report, now, id); err != nil {
			return ThreadRow{}, nil, ReportRow{}, false, err
		}
		// A new ask of a plan changes its gate, and takes one that waits
		// for the owner's approval back to drafting (ADR-0020): the plan
		// is not settled.
		if thread.IsPlanThread() && len(out) > 0 {
			if _, err := tx.ExecContext(ctx, `UPDATE plans SET state = CASE WHEN state = ? THEN ? ELSE state END, updated_at = ? WHERE id = ?`,
				team.PlanReview, team.PlanDrafting, now, thread.Plan); err != nil {
				return ThreadRow{}, nil, ReportRow{}, false, err
			}
		}
		if thread, err = scanThread(tx.QueryRowContext(ctx, `SELECT `+threadColumns+` FROM threads WHERE id = ?`, id).Scan); err != nil {
			return ThreadRow{}, nil, ReportRow{}, false, err
		}
	}
	return thread, out, message, filed, nil
}

// ReportFiled says whether report call key was filed.
func (s *Store) ReportFiled(ctx context.Context, key string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM reports WHERE key = ?`, key).Scan(&n)
	return n > 0, err
}

const reportColumns = `key, thread_id, session_id, text, submission, attempt, sent_at`

func scanReport(scan func(...any) error) (ReportRow, error) {
	var row ReportRow
	var sent sql.NullInt64
	if err := scan(&row.Key, &row.ThreadID, &row.SessionID, &row.Text, &row.Submission, &row.Attempt, &sent); err != nil {
		return ReportRow{}, err
	}
	if sent.Valid {
		at := fromMillis(sent.Int64)
		row.SentAt = &at
	}
	return row, nil
}

// UnsentReports are the reports whose message has not reached the
// supervisor's session yet, in the order they were filed.
func (s *Store) UnsentReports(ctx context.Context) ([]ReportRow, error) {
	return s.unsentReports(ctx, `SELECT `+reportColumns+` FROM reports WHERE sent_at IS NULL ORDER BY rowid`)
}

// UnsentReportsOf are thread's unsent reports, in the order they were
// filed.
func (s *Store) UnsentReportsOf(ctx context.Context, thread int64) ([]ReportRow, error) {
	return s.unsentReports(ctx, `SELECT `+reportColumns+` FROM reports WHERE thread_id = ? AND sent_at IS NULL ORDER BY rowid`, thread)
}

// OldestUnsentReport is thread's oldest unsent report to session; ok is
// false when none waits. A session that takes no message reads one row,
// however long its backlog.
func (s *Store) OldestUnsentReport(ctx context.Context, thread, session int64) (row ReportRow, ok bool, err error) {
	row, err = scanReport(s.db.QueryRowContext(ctx, `SELECT `+reportColumns+` FROM reports WHERE thread_id = ? AND session_id = ? AND sent_at IS NULL
		ORDER BY rowid LIMIT 1`, thread, session).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return ReportRow{}, false, nil
	}
	return row, err == nil, err
}

// FirstReportKey is the key of the first report thread filed for
// session, sent or not: a worker's brief, for its worker.
func (s *Store) FirstReportKey(ctx context.Context, thread, session int64) (string, error) {
	var key string
	err := s.db.QueryRowContext(ctx, `SELECT key FROM reports WHERE thread_id = ? AND session_id = ? ORDER BY rowid LIMIT 1`, thread, session).Scan(&key)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return key, err
}

// UnsentReportSessions are the sessions thread's unsent reports go to, by
// the filing order of their oldest one: each takes its own in order,
// whatever another one takes (Codex review of #29, round 1).
func (s *Store) UnsentReportSessions(ctx context.Context, thread int64) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT session_id FROM reports WHERE thread_id = ? AND sent_at IS NULL
		GROUP BY session_id ORDER BY MIN(rowid)`, thread)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []int64
	for rows.Next() {
		var session int64
		if err := rows.Scan(&session); err != nil {
			return nil, err
		}
		out = append(out, session)
	}
	return out, rows.Err()
}

// ThreadsWithUnsentReports are the threads with a report that has not
// reached the supervisor, by the filing order of their oldest one.
func (s *Store) ThreadsWithUnsentReports(ctx context.Context) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT thread_id FROM reports WHERE sent_at IS NULL GROUP BY thread_id ORDER BY MIN(rowid)`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func (s *Store) unsentReports(ctx context.Context, query string, args ...any) ([]ReportRow, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []ReportRow
	for rows.Next() {
		row, err := scanReport(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// ReportSubmitted notes the submission that carries report key's message.
func (s *Store) ReportSubmitted(ctx context.Context, key string, submission int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE reports SET submission = ? WHERE key = ?`, submission, key)
	return err
}

// ReportWithdrawn is AnswerWithdrawn for report key's message.
func (s *Store) ReportWithdrawn(ctx context.Context, key string, attempt int) (bool, error) {
	return changed(s.db.ExecContext(ctx, `UPDATE reports SET submission = 0, attempt = attempt + 1 WHERE key = ? AND attempt = ?`, key, attempt))
}

// BriefSubmitted, BriefWithdrawn and Briefed are AnswerSubmitted,
// AnswerWithdrawn and AnswerDelivered for thread id's brief.
func (s *Store) BriefSubmitted(ctx context.Context, id, submission int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE threads SET brief_submission = ? WHERE id = ?`, submission, id)
	return err
}

func (s *Store) BriefWithdrawn(ctx context.Context, id int64, attempt int) (bool, error) {
	return changed(s.db.ExecContext(ctx, `UPDATE threads SET brief_submission = 0, brief_attempt = brief_attempt + 1 WHERE id = ? AND brief_attempt = ?`,
		id, attempt))
}

func (s *Store) Briefed(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE threads SET briefed_at = ? WHERE id = ? AND briefed_at IS NULL`, s.millis(), id)
	return err
}

// followPredicate selects the threads a delivery pass follows. Its literal
// state != 'closed' is the partial index threads_open's (migration 4), so
// SQLite reads the desk, never the closed history.
const followPredicate = `state != 'closed' AND (state = 'starting' OR (brief != '' AND briefed_at IS NULL))`

// ThreadsToFollow are the threads a delivery pass looks at: those still
// starting, and those whose brief, kept, is not in the specialist's
// transcript yet; closed ones aside.
func (s *Store) ThreadsToFollow(ctx context.Context) ([]ThreadRow, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+threadColumns+` FROM threads WHERE `+followPredicate+` ORDER BY id`)
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

// ReportSent notes that report key's message reached the supervisor.
func (s *Store) ReportSent(ctx context.Context, key string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE reports SET sent_at = ? WHERE key = ? AND sent_at IS NULL`, s.millis(), key)
	return err
}

// RequestClosed is the result of a close's request once the close
// committed (CloseThreadAs); a reserved one has none.
const RequestClosed = "closed"

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
	before, err := scanThread(tx.QueryRowContext(ctx, `SELECT `+threadColumns+` FROM threads WHERE id = ?`, id).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return ThreadRow{}, ErrNotFound
	} else if err != nil {
		return ThreadRow{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE threads SET state = ?, updated_at = ? WHERE id = ? AND state != ?`, ThreadClosed, now, id, ThreadClosed); err != nil {
		return ThreadRow{}, err
	}
	// The request's result says the close committed, apart from a
	// reservation (ClaimRequest) whose close did not.
	if _, err := tx.ExecContext(ctx, `UPDATE requests SET result = ? WHERE key = ?`, RequestClosed, key); err != nil {
		return ThreadRow{}, err
	}
	row, err := scanThread(tx.QueryRowContext(ctx, `SELECT `+threadColumns+` FROM threads WHERE id = ?`, id).Scan)
	if err != nil {
		return ThreadRow{}, err
	}
	if err := threadClosed(ctx, tx, before, row, now); err != nil {
		return ThreadRow{}, err
	}
	return row, tx.Commit()
}

// CloseThread closes a thread; closing a closed one changes nothing.
func (s *Store) CloseThread(ctx context.Context, id int64) (ThreadRow, error) {
	return s.updateThread(ctx, id, `UPDATE threads SET state = ?, updated_at = ? WHERE id = ? AND state != ?`,
		ThreadClosed, s.millis(), id, ThreadClosed)
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
	// Submission is the submission that carries the answer to the asking
	// session, 0 before one; Attempt counts the submissions a withdrawal
	// ended, so the next has a key of its own.
	Submission int64
	Attempt    int
	// Delivered is set once the answer is in the asking session's
	// transcript.
	Delivered bool
	// Written is set once a write of the ask's plan named it (ADR-0020).
	Written bool
	// Plan is the plan whose own thread asked, 0 for none, from the same
	// read as the ask: its events need no other lookup (Codex review of
	// #25, round 4).
	Plan int64
}

const askColumns = `id, key, thread_id, session_id, question, options, state, answer, answered_by, created_at, answered_at, submission, attempt,
	delivered_at IS NOT NULL, written_at IS NOT NULL, COALESCE((SELECT plans.id FROM plans WHERE plans.thread_id = asks.thread_id), 0)`

func scanAsk(scan func(...any) error) (AskRow, error) {
	var row AskRow
	var options string
	var created int64
	var answered sql.NullInt64
	err := scan(&row.ID, &row.Key, &row.ThreadID, &row.SessionID, &row.Question, &options, &row.State, &row.Answer, &row.AnsweredBy, &created, &answered,
		&row.Submission, &row.Attempt, &row.Delivered, &row.Written, &row.Plan)
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
		// An answer changes the gate of the plan whose thread asked
		// (ADR-0020): the plan changed with it, and one it clears may let
		// the supervisor deliver again.
		if _, err := tx.ExecContext(ctx, `UPDATE plans SET updated_at = ? WHERE thread_id = (SELECT thread_id FROM asks WHERE id = ?)`, now, id); err != nil {
			return AskRow{}, false, err
		}
		if err := askCleared(ctx, tx, id, now); err != nil {
			return AskRow{}, false, err
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

// AnswerSubmitted notes the submission that carries ask id's answer.
func (s *Store) AnswerSubmitted(ctx context.Context, id, submission int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE asks SET submission = ? WHERE id = ?`, submission, id)
	return err
}

// AnswerWithdrawn notes that attempt, the submission ask id's answer went
// in, ended without reaching the transcript: the next one takes a new key.
// It changes nothing, and says so, when the ask is past that attempt.
func (s *Store) AnswerWithdrawn(ctx context.Context, id int64, attempt int) (bool, error) {
	return changed(s.db.ExecContext(ctx, `UPDATE asks SET submission = 0, attempt = attempt + 1 WHERE id = ? AND attempt = ?`, id, attempt))
}

// changed says whether a statement changed a row.
func changed(result sql.Result, err error) (bool, error) {
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	return n > 0, err
}

// UndeliveredAnswerIDs are the ids of the answered asks whose answer has
// not reached its session yet, the oldest answer first: a pass reads each
// ask when it delivers it, never the whole backlog at once.
func (s *Store) UndeliveredAnswerIDs(ctx context.Context) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM asks WHERE state = ? AND delivered_at IS NULL ORDER BY answered_at, id`, AskAnswered)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// AnswerDelivered notes that ask id's answer reached the asking session.
func (s *Store) AnswerDelivered(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE asks SET delivered_at = ? WHERE id = ? AND state = ? AND delivered_at IS NULL`, s.millis(), id, AskAnswered)
	return err
}

// ReviewThreadsOf are the review threads pocketd started for pull request
// pull, oldest first (ADR-0019): the budget counts them.
func (s *Store) ReviewThreadsOf(ctx context.Context, pull int64) ([]ThreadRow, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+threadColumns+` FROM threads WHERE review_pull = ? AND review_pull != 0 ORDER BY id`, pull)
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

// CloseReview closes review thread id with report, and keeps message, if
// any, to send, in one transaction (ADR-0019): the author is told a
// verdict exactly when the review closes on it. A thread closed already
// changes nothing, and a message kept already is not kept twice; changed
// says whether the thread closed now.
func (s *Store) CloseReview(ctx context.Context, id int64, report string, message *ReportRow) (row ThreadRow, changed bool, err error) {
	now := s.millis()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ThreadRow{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.ExecContext(ctx, `UPDATE threads SET state = ?, report = ?, updated_at = ? WHERE id = ? AND state != ? AND review_pull != 0`,
		ThreadClosed, report, now, id, ThreadClosed)
	if err != nil {
		return ThreadRow{}, false, err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return ThreadRow{}, false, err
	}
	if n == 1 && message != nil {
		if _, err := tx.ExecContext(ctx, `INSERT INTO reports (key, thread_id, session_id, text, created_at) VALUES (?, ?, ?, ?, ?) ON CONFLICT (key) DO NOTHING`,
			message.Key, message.ThreadID, message.SessionID, message.Text, now); err != nil {
			return ThreadRow{}, false, err
		}
	}
	row, err = scanThread(tx.QueryRowContext(ctx, `SELECT `+threadColumns+` FROM threads WHERE id = ?`, id).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return ThreadRow{}, false, ErrNotFound
	} else if err != nil {
		return ThreadRow{}, false, err
	}
	if n == 1 {
		if err := freedPlace(ctx, tx, row, now); err != nil {
			return ThreadRow{}, false, err
		}
	}
	return row, n == 1, tx.Commit()
}

// SetReviewBase keeps review thread id's base, once: a base kept already
// stays, so the brief and team_diff always name the same change set
// (ADR-0019).
func (s *Store) SetReviewBase(ctx context.Context, id int64, base string) (ThreadRow, error) {
	// review_base goes on no wire: updated_at, which does, stays (Codex
	// review of #26, round 3).
	return s.updateThread(ctx, id, `UPDATE threads SET review_base = ? WHERE id = ? AND review_pull != 0 AND review_base = ''`, base, id)
}

// OpenReviewThreads are the review threads not closed, oldest first: a
// review pass settles each, whatever its pull request is now (ADR-0019).
func (s *Store) OpenReviewThreads(ctx context.Context) ([]ThreadRow, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+threadColumns+` FROM threads WHERE review_pull != 0 AND state != 'closed' ORDER BY id`)
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

// RecordWorkerSession records a thread's worker session (as
// CreateSession), names it thread id's worker, and keeps brief, the
// message that starts it, first for it, in one transaction, only while the
// thread is not closed and has no other worker (ADR-0021): a call run
// again after a restart finds its own, and no other message goes before
// the brief (Codex review of #29, round 4). ErrConflict says the thread
// closed, or has another worker.
func (s *Store) RecordWorkerSession(ctx context.Context, key string, session SessionRow, id int64, brief ReportRow) (ThreadRow, error) {
	var thread ThreadRow
	_, err := s.createSession(ctx, key, session, func(tx *sql.Tx, now int64) error {
		result, err := tx.ExecContext(ctx, `UPDATE threads SET worker_session = ?, updated_at = ? WHERE id = ? AND state != 'closed'
			AND worker_session IN (0, ?)`, session.ID, now, id, session.ID)
		if err != nil {
			return err
		}
		if n, err := result.RowsAffected(); err != nil {
			return err
		} else if n != 1 {
			return ErrConflict
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO reports (key, thread_id, session_id, text, created_at) VALUES (?, ?, ?, ?, ?)
			ON CONFLICT (key) DO NOTHING`, brief.Key, id, session.ID, brief.Text, now); err != nil {
			return err
		}
		thread, err = scanThread(tx.QueryRowContext(ctx, `SELECT `+threadColumns+` FROM threads WHERE id = ?`, id).Scan)
		return err
	})
	return thread, err
}

// ThreadOfWorker is the thread whose worker runs in session.
func (s *Store) ThreadOfWorker(ctx context.Context, session int64) (ThreadRow, error) {
	if session == 0 {
		return ThreadRow{}, ErrNotFound
	}
	// The partial index's own predicate, so SQLite uses it (Codex review
	// of #29, round 2).
	return s.thread(ctx, "worker_session = ? AND worker_session != 0", session)
}

// QueueWorkerReport keeps a message between a thread's worker and its
// specialist (ADR-0021), a worker's reply or a message to the worker, once
// by its key, only while the thread is not closed: queued says it is kept
// now or was before.
func (s *Store) QueueWorkerReport(ctx context.Context, row ReportRow) (queued bool, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM reports WHERE key = ?`, row.Key).Scan(&n); err != nil {
		return false, err
	}
	if n > 0 {
		return true, nil
	}
	var state string
	if err := tx.QueryRowContext(ctx, `SELECT state FROM threads WHERE id = ?`, row.ThreadID).Scan(&state); errors.Is(err, sql.ErrNoRows) {
		return false, ErrNotFound
	} else if err != nil {
		return false, err
	}
	if state == ThreadClosed {
		return false, nil
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO reports (key, thread_id, session_id, text, created_at) VALUES (?, ?, ?, ?, ?)`,
		row.Key, row.ThreadID, row.SessionID, row.Text, s.millis()); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// ThreadsWithWorkers are the threads that have a worker, open or closed
// (ADR-0021): at the service's start, an open one's replies are sent and a
// closed one's worker is stopped.
func (s *Store) ThreadsWithWorkers(ctx context.Context) ([]ThreadRow, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+threadColumns+` FROM threads WHERE worker_session != 0 ORDER BY id`)
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

// SetWorkerCursor moves thread id's worker cursor on to cursor, never back
// (ADR-0021): the replies of the runs up to it are kept.
func (s *Store) SetWorkerCursor(ctx context.Context, id, cursor int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE threads SET worker_cursor = ? WHERE id = ? AND worker_cursor < ?`, cursor, id, cursor)
	return err
}

// ClosedReviewSessions are the sessions of the closed review threads: the
// service's start stops any run of theirs that a restart brought back
// (ADR-0019).
func (s *Store) ClosedReviewSessions(ctx context.Context) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT session FROM threads WHERE review_pull != 0 AND state = 'closed' AND session != 0 ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
