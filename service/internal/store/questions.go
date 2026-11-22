package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// The questions a supervisor's or a private agent's session puts to the
// owner with its question tool (ADR-0047). Each is keyed by its tool
// call, as an approval is: the call run again after a restart finds its
// question, waiting or settled.

// The states of a question, as proto.QuestionState names them.
const (
	QuestionWaiting  = "waiting"
	QuestionAnswered = "answered"
	QuestionSkipped  = "skipped"
)

// QuestionRow is a question's row.
type QuestionRow struct {
	ID          string
	SessionID   int64
	TaskID      int64
	CallID      string
	Question    string
	Options     []string
	Multiple    bool
	Placeholder string
	State       string
	CreatedAt   time.Time
	Deadline    time.Time
	Answer      string
	Chosen      []int
	Reason      string
	SettledBy   string
	SettledAt   *time.Time
}

// ErrQuestionWaits refuses a question while another of its session waits:
// a session has at most one.
var ErrQuestionWaits = errors.New("a question already waits in this session")

// ErrQuestionLate refuses an answer to a question whose deadline passed:
// it is skipped, as its timer skips it. It is an ErrConflict.
var ErrQuestionLate = fmt.Errorf("%w: the question's time ran out", ErrConflict)

// QuestionID is the id of the question of one tool call of one task: the
// same call shows the same question after a restart.
func QuestionID(taskID int64, callID string) string {
	sum := sha256.Sum256([]byte(strconv.FormatInt(taskID, 10) + "\x00" + callID))
	return "q" + hex.EncodeToString(sum[:8])
}

const questionColumns = `id, session_id, task_id, call_id, question, options, multiple, placeholder, state, created_at, deadline, answer, chosen, reason, settled_by, settled_at`

func scanQuestion(scan func(...any) error) (QuestionRow, error) {
	var row QuestionRow
	var options, chosen string
	var created, deadline int64
	var settled sql.NullInt64
	if err := scan(&row.ID, &row.SessionID, &row.TaskID, &row.CallID, &row.Question, &options, &row.Multiple, &row.Placeholder, &row.State,
		&created, &deadline, &row.Answer, &chosen, &row.Reason, &row.SettledBy, &settled); err != nil {
		return QuestionRow{}, err
	}
	if err := json.Unmarshal([]byte(options), &row.Options); err != nil {
		return QuestionRow{}, err
	}
	if err := json.Unmarshal([]byte(chosen), &row.Chosen); err != nil {
		return QuestionRow{}, err
	}
	row.CreatedAt, row.Deadline = fromMillis(created), fromMillis(deadline)
	if settled.Valid {
		t := fromMillis(settled.Int64)
		row.SettledAt = &t
	}
	return row, nil
}

// jsonList is list as its column keeps it: [] for none.
func jsonList[T any](list []T) string {
	if len(list) == 0 {
		return "[]"
	}
	data, _ := json.Marshal(list)
	return string(data)
}

// PutQuestion records row as its call's waiting question (its ID is
// QuestionID's), created now, or returns the question its call recorded
// already, waiting or settled; created says which. A question of a call
// that has none, while another of its session waits, is
// ErrQuestionWaits. Two calls of one turn that run at once are read and
// written one after the other: every transaction of the store takes the
// write lock as it begins (dsn's _txlock).
func (s *Store) PutQuestion(ctx context.Context, row QuestionRow) (QuestionRow, bool, error) {
	row.ID = QuestionID(row.TaskID, row.CallID)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return QuestionRow{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	found, err := scanQuestion(tx.QueryRowContext(ctx, `SELECT `+questionColumns+` FROM questions WHERE id = ?`, row.ID).Scan)
	if err == nil {
		return found, false, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return QuestionRow{}, false, err
	}
	var one int
	switch err := tx.QueryRowContext(ctx, `SELECT 1 FROM questions WHERE session_id = ? AND state = 'waiting'`, row.SessionID).Scan(&one); {
	case err == nil:
		return QuestionRow{}, false, ErrQuestionWaits
	case !errors.Is(err, sql.ErrNoRows):
		return QuestionRow{}, false, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO questions (id, session_id, task_id, call_id, question, options, multiple, placeholder, state, created_at, deadline)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, 'waiting', ?, ?)`,
		row.ID, row.SessionID, row.TaskID, row.CallID, row.Question, jsonList(row.Options), row.Multiple, row.Placeholder, s.millis(), row.Deadline.UnixMilli()); err != nil {
		return QuestionRow{}, false, err
	}
	stored, err := scanQuestion(tx.QueryRowContext(ctx, `SELECT `+questionColumns+` FROM questions WHERE id = ?`, row.ID).Scan)
	if err != nil {
		return QuestionRow{}, false, err
	}
	return stored, true, tx.Commit()
}

// Question returns one question.
func (s *Store) Question(ctx context.Context, id string) (QuestionRow, error) {
	row, err := scanQuestion(s.db.QueryRowContext(ctx, `SELECT `+questionColumns+` FROM questions WHERE id = ?`, id).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return QuestionRow{}, ErrNotFound
	}
	return row, err
}

// WaitingQuestions are the questions that wait, by session; the
// predicate is questions_waiting's.
func (s *Store) WaitingQuestions(ctx context.Context) (map[int64]QuestionRow, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+questionColumns+` FROM questions WHERE state = 'waiting'`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[int64]QuestionRow{}
	for rows.Next() {
		row, err := scanQuestion(rows.Scan)
		if err != nil {
			return nil, err
		}
		out[row.SessionID] = row
	}
	return out, rows.Err()
}

// InboxQuestionRow is a waiting question with its session's profile, ""
// for none.
type InboxQuestionRow struct {
	QuestionRow
	Profile string
}

// waitingQuestionsIn are the questions that wait, oldest first, each with
// its session's profile, read in tx (Inbox).
func waitingQuestionsIn(ctx context.Context, tx *sql.Tx) ([]InboxQuestionRow, error) {
	columns := "q." + strings.ReplaceAll(questionColumns, ", ", ", q.")
	rows, err := tx.QueryContext(ctx, `SELECT `+columns+`, COALESCE(s.profile, '') FROM questions q LEFT JOIN sessions s ON s.id = q.session_id
		WHERE q.state = 'waiting' ORDER BY q.created_at, q.id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []InboxQuestionRow
	for rows.Next() {
		var profile string
		row, err := scanQuestion(func(dest ...any) error { return rows.Scan(append(dest, &profile)...) })
		if err != nil {
			return nil, err
		}
		out = append(out, InboxQuestionRow{QuestionRow: row, Profile: profile})
	}
	return out, rows.Err()
}

// WaitingQuestion is the question that waits in session, ErrNotFound for
// none.
func (s *Store) WaitingQuestion(ctx context.Context, session int64) (QuestionRow, error) {
	row, err := scanQuestion(s.db.QueryRowContext(ctx, `SELECT `+questionColumns+` FROM questions WHERE session_id = ? AND state = 'waiting'`, session).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return QuestionRow{}, ErrNotFound
	}
	return row, err
}

// AnswerQuestion settles question id of session, as the owner answered
// it (answer and chosen) or skipped it (skipped), by device by, and
// records the request key in the same transaction. A retry with the same
// key returns the question as it is (replayed true). A question that is
// settled already is ErrConflict, and one whose deadline passed
// ErrQuestionLate, writing nothing; one of another session, or none,
// ErrNotFound; a key used for another request, ErrRequestReused.
func (s *Store) AnswerQuestion(ctx context.Context, key string, session int64, id, answer string, chosen []int, skipped bool, by string) (row QuestionRow, replayed bool, err error) {
	now := s.millis()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return QuestionRow{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	_, existed, err := claimRequest(ctx, tx, key, "question:"+id, now)
	if err != nil {
		return QuestionRow{}, false, err
	}
	row, err = scanQuestion(tx.QueryRowContext(ctx, `SELECT `+questionColumns+` FROM questions WHERE id = ? AND session_id = ?`, id, session).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return QuestionRow{}, false, ErrNotFound
	} else if err != nil {
		return QuestionRow{}, false, err
	}
	if existed {
		return row, true, nil
	}
	if row.State != QuestionWaiting {
		return QuestionRow{}, false, ErrConflict
	}
	// Its deadline passed: it is skipped, whether or not its timer did it
	// yet (Codex review of #95, round 1).
	if now >= row.Deadline.UnixMilli() {
		return QuestionRow{}, false, ErrQuestionLate
	}
	state, reason := QuestionAnswered, ""
	if skipped {
		state, reason, answer, chosen = QuestionSkipped, "owner", "", nil
	}
	if _, err := tx.ExecContext(ctx, `UPDATE questions SET state = ?, answer = ?, chosen = ?, reason = ?, settled_by = ?, settled_at = ? WHERE id = ? AND state = 'waiting'`,
		state, answer, jsonList(chosen), reason, by, now, id); err != nil {
		return QuestionRow{}, false, err
	}
	row, err = scanQuestion(tx.QueryRowContext(ctx, `SELECT `+questionColumns+` FROM questions WHERE id = ?`, id).Scan)
	if err != nil {
		return QuestionRow{}, false, err
	}
	return row, false, tx.Commit()
}

// SkipQuestion skips question id while it waits, by pocketd, for reason:
// its deadline passed, or its run was aborted. A question that is no
// longer waiting is ErrConflict, and one that does not exist ErrNotFound.
func (s *Store) SkipQuestion(ctx context.Context, id, reason string) (QuestionRow, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return QuestionRow{}, err
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.ExecContext(ctx, `UPDATE questions SET state = 'skipped', reason = ?, settled_by = 'pocketd', settled_at = ? WHERE id = ? AND state = 'waiting'`,
		reason, s.millis(), id)
	if err != nil {
		return QuestionRow{}, err
	}
	row, err := scanQuestion(tx.QueryRowContext(ctx, `SELECT `+questionColumns+` FROM questions WHERE id = ?`, id).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return QuestionRow{}, ErrNotFound
	} else if err != nil {
		return QuestionRow{}, err
	}
	if n, _ := result.RowsAffected(); n == 0 {
		return QuestionRow{}, ErrConflict
	}
	return row, tx.Commit()
}
