package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Lesson states (ADR-0015).
const (
	LessonPending   = "pending"
	LessonActive    = "active"
	LessonDiscarded = "discarded"
	LessonForgotten = "forgotten"
)

// LessonRow is a lesson: an agent's, written by a profile's session, in
// force once the owner approved it. A change to one in force waits in its
// Change fields.
type LessonRow struct {
	ID int64
	// Key is the team_memory call that wrote it, "" for none.
	Key       string
	Agent     string
	Text      string
	State     string
	Writer    string
	SessionID int64
	// ChangeText or ChangeForget: a change that waits, by ChangeWriter in
	// ChangeSession.
	ChangeText    string
	ChangeForget  bool
	ChangeWriter  string
	ChangeSession int64
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// Waits says whether the owner has something to decide on the lesson.
func (l LessonRow) Waits() bool {
	return l.State == LessonPending || (l.State == LessonActive && (l.ChangeText != "" || l.ChangeForget))
}

// Gone says whether the lesson was discarded or forgotten.
func (l LessonRow) Gone() bool { return l.State == LessonDiscarded || l.State == LessonForgotten }

const lessonColumns = `id, key, agent, text, state, writer, session_id, change_text, change_forget, change_writer, change_session, created_at, updated_at`

func scanLesson(scan func(...any) error) (LessonRow, error) {
	var l LessonRow
	var forget int
	var created, updated int64
	err := scan(&l.ID, &l.Key, &l.Agent, &l.Text, &l.State, &l.Writer, &l.SessionID, &l.ChangeText, &forget, &l.ChangeWriter, &l.ChangeSession, &created, &updated)
	l.ChangeForget = forget != 0
	l.CreatedAt, l.UpdatedAt = fromMillis(created), fromMillis(updated)
	return l, err
}

type querier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func lessonIn(ctx context.Context, q querier, id int64) (LessonRow, error) {
	row, err := scanLesson(q.QueryRowContext(ctx, `SELECT `+lessonColumns+` FROM lessons WHERE id = ?`, id).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return LessonRow{}, ErrNotFound
	}
	return row, err
}

// Lesson is lesson id.
func (s *Store) Lesson(ctx context.Context, id int64) (LessonRow, error) {
	return lessonIn(ctx, s.db, id)
}

func (s *Store) lessons(ctx context.Context, where string, args ...any) ([]LessonRow, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+lessonColumns+` FROM lessons WHERE `+where+` ORDER BY id`, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []LessonRow
	for rows.Next() {
		row, err := scanLesson(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// LiveLessons are every agent's lessons that are not gone, oldest first.
func (s *Store) LiveLessons(ctx context.Context) ([]LessonRow, error) {
	return s.lessons(ctx, `state IN ('pending', 'active')`)
}

// LessonsOf are agent's lessons that are not gone, oldest first.
func (s *Store) LessonsOf(ctx context.Context, agent string) ([]LessonRow, error) {
	return s.lessons(ctx, `state IN ('pending', 'active') AND agent = ?`, agent)
}

// ActiveLessons are agent's lessons in force, oldest first: what its
// sessions' instructions list.
func (s *Store) ActiveLessons(ctx context.Context, agent string) ([]LessonRow, error) {
	return s.lessons(ctx, `state = 'active' AND agent = ?`, agent)
}

// AddLesson saves a new lesson, pending, in one transaction: the lesson of
// row.Key, when there is one, is returned as it is (replayed), so a call
// run again saves nothing twice. check gets how many items of row.Writer
// wait for row.Agent's owner (waitingFrom), and refuses one more
// (team.CheckPending).
func (s *Store) AddLesson(ctx context.Context, row LessonRow, check func(waiting int) error) (LessonRow, bool, error) {
	now := s.millis()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return LessonRow{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	if row.Key != "" {
		found, err := scanLesson(tx.QueryRowContext(ctx, `SELECT `+lessonColumns+` FROM lessons WHERE key = ?`, row.Key).Scan)
		if err == nil {
			return found, true, nil
		} else if !errors.Is(err, sql.ErrNoRows) {
			return LessonRow{}, false, err
		}
	}
	waiting, err := waitingFrom(ctx, tx, row.Agent, row.Writer)
	if err != nil {
		return LessonRow{}, false, err
	}
	if err := check(waiting); err != nil {
		return LessonRow{}, false, conflict(err)
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO lessons (key, agent, text, state, writer, session_id, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		row.Key, row.Agent, row.Text, LessonPending, row.Writer, row.SessionID, now, now)
	if err != nil {
		return LessonRow{}, false, err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return LessonRow{}, false, err
	}
	saved, err := lessonIn(ctx, tx, id)
	if err != nil {
		return LessonRow{}, false, err
	}
	return saved, false, tx.Commit()
}

// waitingFrom counts what writer has waiting for the owner on agent's
// lessons: new lessons, and changes to lessons in force.
func waitingFrom(ctx context.Context, tx *sql.Tx, agent, writer string) (int, error) {
	var n int
	err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM lessons WHERE agent = ? AND ((state = ? AND writer = ?) OR
		(state = ? AND change_writer = ? AND (change_text != '' OR change_forget = 1)))`, agent, LessonPending, writer, LessonActive, writer).Scan(&n)
	return n, err
}

// LessonCall is what team_memory call key did, recorded with it: the
// lesson an add saved (outcome ""), or the lesson an update or remove
// acted on and its outcome. found is false for a call not recorded.
func (s *Store) LessonCall(ctx context.Context, key string) (row LessonRow, outcome string, found bool, err error) {
	row, err = scanLesson(s.db.QueryRowContext(ctx, `SELECT `+lessonColumns+` FROM lessons WHERE key = ?`, key).Scan)
	if err == nil {
		return row, "", true, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return LessonRow{}, "", false, err
	}
	var kind string
	err = s.db.QueryRowContext(ctx, `SELECT kind, result FROM requests WHERE key = ?`, key).Scan(&kind, &outcome)
	if errors.Is(err, sql.ErrNoRows) {
		return LessonRow{}, "", false, nil
	} else if err != nil {
		return LessonRow{}, "", false, err
	}
	idText, ok := strings.CutPrefix(kind, lessonChangeKind)
	id, perr := strconv.ParseInt(idText, 10, 64)
	if !ok || perr != nil {
		return LessonRow{}, "", false, ErrRequestReused
	}
	row, err = lessonIn(ctx, s.db, id)
	return row, outcome, err == nil, err
}

// lessonChangeKind is the request kind of an agent's update or remove of a
// lesson, before the lesson's id.
const lessonChangeKind = "lesson-change:"

// What an agent's update or remove did to a lesson.
const (
	// LessonChanged: a waiting lesson took the new text, and still waits.
	LessonChanged = "changed"
	// LessonProposed: new text for a lesson in force waits for the owner.
	LessonProposed = "proposed"
	// LessonKept: the text in force again; the writer's own change that
	// waited, if any, is withdrawn.
	LessonKept = "kept"
	// LessonDropped: a waiting lesson is discarded.
	LessonDropped = "dropped"
	// LessonForgetProposed: forgetting a lesson in force waits for the
	// owner.
	LessonForgetProposed = "forget-proposed"
	// LessonWasGone: the lesson was gone already.
	LessonWasGone = "gone"
)

// ChangeLesson runs team_memory call key, an agent's update (text set) or
// remove (text "") of lesson id, which must be agent's, in one
// transaction, as writer in session. A waiting lesson changes or goes at
// once. A change to a lesson in force waits for the owner, and the lesson
// stays in force until then; a new one counts against the writer's
// waiting items (check, team.CheckPending), one that replaces the
// writer's own does not. The call's outcome is recorded with it: the call
// run again returns that outcome and the lesson as it is now, replayed,
// and changes nothing.
func (s *Store) ChangeLesson(ctx context.Context, key string, id int64, agent, writer string, session int64, text string, check func(waiting int) error) (row LessonRow, outcome string, replayed bool, err error) {
	now := s.millis()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return LessonRow{}, "", false, err
	}
	defer func() { _ = tx.Rollback() }()
	kind := lessonChangeKind + strconv.FormatInt(id, 10)
	recorded, existed, err := claimRequest(ctx, tx, key, kind, now)
	if err != nil {
		return LessonRow{}, "", false, err
	}
	row, err = lessonIn(ctx, tx, id)
	switch {
	case errors.Is(err, ErrNotFound) || (err == nil && row.Agent != agent):
		// Another agent's lesson is not this agent's to see.
		return LessonRow{}, "", false, ErrNotFound
	case err != nil:
		return LessonRow{}, "", false, err
	case existed:
		return row, recorded, true, nil
	}
	record := func(outcome string) (LessonRow, string, bool, error) {
		if _, err := tx.ExecContext(ctx, `UPDATE requests SET result = ? WHERE key = ?`, outcome, key); err != nil {
			return LessonRow{}, "", false, err
		}
		after, err := lessonIn(ctx, tx, id)
		if err != nil {
			return LessonRow{}, "", false, err
		}
		return after, outcome, false, tx.Commit()
	}
	if row.Gone() {
		return record(LessonWasGone)
	}
	// A change that waits anew counts against the writer's waiting items;
	// one that replaces the writer's own takes no more room.
	ownChange := row.ChangeWriter == writer && (row.ChangeText != "" || row.ChangeForget)
	if row.State == LessonActive && text != row.Text && !ownChange {
		waiting, err := waitingFrom(ctx, tx, agent, writer)
		if err != nil {
			return LessonRow{}, "", false, err
		}
		if err := check(waiting); err != nil {
			return LessonRow{}, "", false, conflict(err)
		}
	}
	var query string
	var args []any
	switch {
	case row.State == LessonPending && text != "":
		outcome, query, args = LessonChanged, `UPDATE lessons SET text = ?, updated_at = ? WHERE id = ?`, []any{text, now, id}
	case row.State == LessonPending:
		outcome, query, args = LessonDropped, `UPDATE lessons SET state = ?, updated_at = ? WHERE id = ?`, []any{LessonDiscarded, now, id}
	case text == row.Text:
		// Back to the text in force: nothing to propose. The writer's own
		// change goes; another's stays for the owner.
		outcome = LessonKept
		if row.ChangeWriter == writer {
			query, args = `UPDATE lessons SET change_text = '', change_forget = 0, change_writer = '', change_session = 0, updated_at = ? WHERE id = ?`, []any{now, id}
		}
	case text != "":
		outcome, query, args = LessonProposed, `UPDATE lessons SET change_text = ?, change_forget = 0, change_writer = ?, change_session = ?, updated_at = ? WHERE id = ?`,
			[]any{text, writer, session, now, id}
	default:
		outcome, query, args = LessonForgetProposed, `UPDATE lessons SET change_text = '', change_forget = 1, change_writer = ?, change_session = ?, updated_at = ? WHERE id = ?`,
			[]any{writer, session, now, id}
	}
	if query != "" {
		if _, err := tx.ExecContext(ctx, query, args...); err != nil {
			return LessonRow{}, "", false, err
		}
	}
	return record(outcome)
}

// The owner's decisions on a lesson (ADR-0015).
const (
	DecideApprove = "approve"
	DecideDiscard = "discard"
	DecideForget  = "forget"
)

// DecideLesson runs the owner's decision on lesson id once, as request
// key, in one transaction: a retry with the same key returns the lesson as
// it is now, and replayed. An approval holds only for what the owner saw
// waiting (seenText, or seenForget): anything else is ErrConflict, as is a
// decision with nothing to decide. check gets how many lessons the agent
// keeps in force, and refuses one more (team.CheckRoom). inForce says the
// lessons in force changed, so the agent's sessions need them again.
func (s *Store) DecideLesson(ctx context.Context, key string, id int64, decision, seenText string, seenForget bool, check func(active int) error) (row LessonRow, replayed, inForce bool, err error) {
	now := s.millis()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return LessonRow{}, false, false, err
	}
	defer func() { _ = tx.Rollback() }()
	_, existed, err := claimRequest(ctx, tx, key, "lesson:"+strconv.FormatInt(id, 10), now)
	if err != nil {
		return LessonRow{}, false, false, err
	}
	row, err = lessonIn(ctx, tx, id)
	if err != nil {
		return LessonRow{}, false, false, err
	}
	if existed {
		return row, true, false, nil
	}
	const clear = `change_text = '', change_forget = 0, change_writer = '', change_session = 0`
	var query string
	var args []any
	switch {
	case row.Gone():
		return LessonRow{}, false, false, conflict(fmt.Errorf("lesson L%d is %s", id, row.State))
	case decision == DecideApprove && !row.Waits():
		return LessonRow{}, false, false, conflict(fmt.Errorf("nothing waits for L%d", id))
	case decision == DecideApprove && !sawWhatWaits(row, seenText, seenForget):
		return LessonRow{}, false, false, conflict(fmt.Errorf("lesson L%d changed since you looked: check it again", id))
	case decision == DecideApprove && row.State == LessonPending:
		var active int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM lessons WHERE state = ? AND agent = ?`, LessonActive, row.Agent).Scan(&active); err != nil {
			return LessonRow{}, false, false, err
		}
		if err := check(active); err != nil {
			return LessonRow{}, false, false, conflict(err)
		}
		query, args = `UPDATE lessons SET state = ?, updated_at = ? WHERE id = ?`, []any{LessonActive, now, id}
	case decision == DecideApprove && row.ChangeForget:
		query, args = `UPDATE lessons SET state = ?, `+clear+`, updated_at = ? WHERE id = ?`, []any{LessonForgotten, now, id}
	case decision == DecideApprove:
		query, args = `UPDATE lessons SET text = change_text, `+clear+`, updated_at = ? WHERE id = ?`, []any{now, id}
	case decision == DecideDiscard && row.State == LessonPending:
		query, args = `UPDATE lessons SET state = ?, updated_at = ? WHERE id = ?`, []any{LessonDiscarded, now, id}
	case decision == DecideDiscard && row.Waits():
		query, args = `UPDATE lessons SET `+clear+`, updated_at = ? WHERE id = ?`, []any{now, id}
	case decision == DecideDiscard:
		return LessonRow{}, false, false, conflict(fmt.Errorf("nothing waits for L%d", id))
	case decision == DecideForget && row.State == LessonActive:
		query, args = `UPDATE lessons SET state = ?, `+clear+`, updated_at = ? WHERE id = ?`, []any{LessonForgotten, now, id}
	case decision == DecideForget:
		return LessonRow{}, false, false, conflict(fmt.Errorf("lesson L%d is not in force: discard it", id))
	default:
		return LessonRow{}, false, false, fmt.Errorf("the decision is approve, discard or forget, not %q", decision)
	}
	if _, err := tx.ExecContext(ctx, query, args...); err != nil {
		return LessonRow{}, false, false, err
	}
	after, err := lessonIn(ctx, tx, id)
	if err != nil {
		return LessonRow{}, false, false, err
	}
	inForce = (row.State == LessonActive) != (after.State == LessonActive) || (after.State == LessonActive && after.Text != row.Text)
	return after, false, inForce, tx.Commit()
}

// conflictError is ErrConflict with why, which its text is.
type conflictError struct{ why error }

func (c conflictError) Error() string        { return c.why.Error() }
func (c conflictError) Unwrap() error        { return c.why }
func (c conflictError) Is(target error) bool { return target == ErrConflict }

// conflict is ErrConflict that says why.
func conflict(why error) error { return conflictError{why} }

// sawWhatWaits says whether the owner saw what waits on row: its text, for
// a new lesson or new text, or that it is to be forgotten.
func sawWhatWaits(row LessonRow, text string, forget bool) bool {
	switch {
	case row.State == LessonPending:
		return !forget && text == row.Text
	case row.ChangeForget:
		return forget
	default:
		return !forget && text == row.ChangeText
	}
}

// SessionsOfProfile are the ids of the sessions started from profile:
// those whose instructions carry its lessons.
func (s *Store) SessionsOfProfile(ctx context.Context, profile string) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM sessions WHERE profile = ? ORDER BY id`, profile)
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

// SessionProfiles are the profiles that started a session, each once.
func (s *Store) SessionProfiles(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT profile FROM sessions WHERE profile != '' ORDER BY profile`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
