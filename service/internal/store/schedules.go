package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/frostyard/pocketgo/service/internal/team"
)

// Schedules (ADR-0030) send a session a message later, or on repeat: the
// owner's, from their schedules file, to a profile's standing session, and
// a supervisor's own, from team_schedule, to its session. Each time one
// goes out files a note, keyed by the schedule's random owner and the
// time's number, in the transaction that moves the schedule on, so a time
// goes out once whatever stops pocketd; the outbox places the note.

// A schedule's sources.
const (
	ScheduleOwner   = "owner"
	ScheduleSession = "session"
)

// A schedule's states: active while it goes out again, done once a time
// that went out once is past, cancelled once its supervisor cancelled it.
const (
	ScheduleActive    = "active"
	ScheduleDone      = "done"
	ScheduleCancelled = "cancelled"
)

// What a schedule's time did: its message was filed (sent), a repeat's
// last message still waited (skipped), or no session took it (missed).
const (
	OutcomeSent    = "sent"
	OutcomeSkipped = "skipped"
	OutcomeMissed  = "missed"
)

// ownerKeyPrefix keys the owner's schedule by its id in their file.
const ownerKeyPrefix = "owner:"

// ScheduleNotePrefix starts the key of every note a schedule's time files.
const ScheduleNotePrefix = "schedule:"

// ScheduleRow is a schedule.
type ScheduleRow struct {
	ID int64
	// Key is the owner's id in their file as "owner:<id>", or the team
	// call that made a session's own.
	Key string
	// Owner is the schedule's random identity: its notes' keys name it,
	// never its row number.
	Owner  string
	Source string
	// Name is the owner's id in their file; "" for a session's own.
	Name string
	// SessionID is the session a session's own goes to; 0 for the
	// owner's, which go to Profile's standing session.
	SessionID int64
	Profile   string
	// Spec is when it goes out, as written (schedule.Parse), Zone the IANA
	// zone its clock times are in, and Text its message.
	Spec, Zone, Text string
	// Gather is a gather action's JSON (ADR-0033), "" for a message.
	Gather string
	// FirstAt is the first time it went out for; NextAt when it goes out
	// next, nil once it does not.
	FirstAt time.Time
	NextAt  *time.Time
	State   string
	// Runs counts the messages it filed, Skipped the times a repeat's
	// last message still waited.
	Runs, Skipped int
	// LastAt is when its last time came, with what it did and why.
	LastAt      *time.Time
	LastOutcome string
	LastWhy     string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// NoteKey is the key of the note its time run files: never its row
// number, which a restored work database gives out again, and the request
// key of the message in the durable store.
func (r ScheduleRow) NoteKey(run int) string {
	return ScheduleNotePrefix + r.Owner + ":" + strconv.Itoa(run)
}

const scheduleColumns = `id, key, owner, source, name, session_id, profile, spec, zone, text, first_at, next_at, state, runs, skipped,
	last_at, last_outcome, last_why, created_at, updated_at, gather`

// The queries a pass runs, each on its partial index, never over the
// session schedules that went out or were cancelled: SQLite takes a partial
// index only for a query whose own text names its condition, never a
// bound value (Codex review of #51, round 1). listSchedules reads the
// owner's schedules and every session's own that still goes out, by id;
// ownerSchedules the owner's alone; dueSchedules those that go out by a
// time; sessionSchedules and countSessionSchedules a session's own that
// still go out.
const (
	listSchedules = `SELECT ` + scheduleColumns + ` FROM schedules WHERE source = 'owner'
		UNION ALL SELECT ` + scheduleColumns + ` FROM schedules WHERE state = 'active' AND source = 'session' ORDER BY id`
	ownerSchedules        = `SELECT ` + scheduleColumns + ` FROM schedules WHERE source = 'owner'`
	dueSchedules          = `SELECT ` + scheduleColumns + ` FROM schedules WHERE state = 'active' AND next_at <= ? ORDER BY next_at, id`
	sessionSchedules      = `SELECT ` + scheduleColumns + ` FROM schedules WHERE state = 'active' AND source = 'session' AND session_id = ? ORDER BY id`
	countSessionSchedules = `SELECT COUNT(*) FROM schedules WHERE state = 'active' AND source = 'session' AND session_id = ?`
)

func scanSchedule(scan func(...any) error) (ScheduleRow, error) {
	var row ScheduleRow
	var first, created, updated int64
	var next, last sql.NullInt64
	if err := scan(&row.ID, &row.Key, &row.Owner, &row.Source, &row.Name, &row.SessionID, &row.Profile, &row.Spec, &row.Zone, &row.Text,
		&first, &next, &row.State, &row.Runs, &row.Skipped, &last, &row.LastOutcome, &row.LastWhy, &created, &updated, &row.Gather); err != nil {
		return ScheduleRow{}, err
	}
	row.FirstAt, row.CreatedAt, row.UpdatedAt = fromMillis(first), fromMillis(created), fromMillis(updated)
	if next.Valid {
		at := fromMillis(next.Int64)
		row.NextAt = &at
	}
	if last.Valid {
		at := fromMillis(last.Int64)
		row.LastAt = &at
	}
	return row, nil
}

// AddSchedule records a session's own schedule, row, once per key: the
// same key again returns the one it made, and replayed. A session that
// keeps max schedules that go out is ErrConflict, counted in the
// transaction that records it.
func (s *Store) AddSchedule(ctx context.Context, row ScheduleRow, max int) (ScheduleRow, bool, error) {
	if row.SessionID == 0 || row.Key == "" {
		return ScheduleRow{}, false, errors.New("a session's schedule needs its session and its key")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ScheduleRow{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	made, err := scanSchedule(tx.QueryRowContext(ctx, `SELECT `+scheduleColumns+` FROM schedules WHERE key = ?`, row.Key).Scan)
	if err == nil {
		return made, true, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return ScheduleRow{}, false, err
	}
	var kept int
	if err := tx.QueryRowContext(ctx, countSessionSchedules, row.SessionID).Scan(&kept); err != nil {
		return ScheduleRow{}, false, err
	}
	if kept >= max {
		return ScheduleRow{}, false, fmt.Errorf("%w: a session keeps at most %d schedules that go out: cancel one first", ErrConflict, max)
	}
	owner, err := newOwner()
	if err != nil {
		return ScheduleRow{}, false, err
	}
	now := s.millis()
	result, err := tx.ExecContext(ctx, `INSERT INTO schedules (key, owner, source, session_id, spec, zone, text, first_at, next_at, state, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		row.Key, owner, ScheduleSession, row.SessionID, row.Spec, row.Zone, row.Text, toMillis(row.FirstAt), toMillis(row.FirstAt), ScheduleActive, now, now)
	if err != nil {
		return ScheduleRow{}, false, err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return ScheduleRow{}, false, err
	}
	made, err = scanSchedule(tx.QueryRowContext(ctx, `SELECT `+scheduleColumns+` FROM schedules WHERE id = ?`, id).Scan)
	if err != nil {
		return ScheduleRow{}, false, err
	}
	return made, false, tx.Commit()
}

// OwnerSchedule is one of the owner's schedules as their file has it now,
// with First, when it goes out first were it new.
type OwnerSchedule struct {
	Name, Profile, Spec, Zone, Text, Gather string
	First                                   time.Time
}

// SyncOwnerSchedules brings the owner's schedules in step with their file,
// in one transaction: a new one goes out first at its First; one whose
// spec or zone changed starts again from its First, keeping its count, so
// its notes' keys go on; one whose message or profile changed takes them
// for its next time. kept names the file's entries that break a rule: they
// keep their record, and go out never while they do (the caller fires only
// the entries it names). Every other owner's schedule left the file, and
// goes. changed says something did.
func (s *Store) SyncOwnerSchedules(ctx context.Context, entries []OwnerSchedule, kept []string) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := queryRows(ctx, tx, scanSchedule, ownerSchedules)
	if err != nil {
		return false, err
	}
	held := map[string]ScheduleRow{}
	for _, row := range rows {
		held[row.Name] = row
	}
	now := s.millis()
	changed := false
	stay := map[string]bool{}
	for _, name := range kept {
		stay[name] = true
	}
	for _, e := range entries {
		stay[e.Name] = true
		row, ok := held[e.Name]
		switch {
		case !ok:
			owner, err := newOwner()
			if err != nil {
				return false, err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO schedules (key, owner, source, name, profile, spec, zone, text, gather, first_at, next_at, state, created_at, updated_at)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				ownerKeyPrefix+e.Name, owner, ScheduleOwner, e.Name, e.Profile, e.Spec, e.Zone, e.Text, e.Gather, toMillis(e.First), toMillis(e.First), ScheduleActive, now, now); err != nil {
				return false, err
			}
		case row.Spec != e.Spec || row.Zone != e.Zone:
			if _, err := tx.ExecContext(ctx, `UPDATE schedules SET spec = ?, zone = ?, profile = ?, text = ?, gather = ?, first_at = ?, next_at = ?, state = ?, updated_at = ? WHERE id = ?`,
				e.Spec, e.Zone, e.Profile, e.Text, e.Gather, toMillis(e.First), toMillis(e.First), ScheduleActive, now, row.ID); err != nil {
				return false, err
			}
		case row.Profile != e.Profile || row.Text != e.Text || row.Gather != e.Gather:
			if _, err := tx.ExecContext(ctx, `UPDATE schedules SET profile = ?, text = ?, gather = ?, updated_at = ? WHERE id = ?`, e.Profile, e.Text, e.Gather, now, row.ID); err != nil {
				return false, err
			}
		default:
			continue
		}
		changed = true
	}
	for name, row := range held {
		if stay[name] {
			continue
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM schedules WHERE id = ?`, row.ID); err != nil {
			return false, err
		}
		changed = true
	}
	return changed, tx.Commit()
}

// Schedules are the owner's schedules and every session's own that still
// go out, by id.
func (s *Store) Schedules(ctx context.Context) ([]ScheduleRow, error) {
	return queryRows(ctx, s.db, scanSchedule, listSchedules)
}

// SessionSchedules are session's own schedules that still go out, by id
// (schedules_session).
func (s *Store) SessionSchedules(ctx context.Context, session int64) ([]ScheduleRow, error) {
	return queryRows(ctx, s.db, scanSchedule, sessionSchedules, session)
}

// Schedule is schedule id.
func (s *Store) Schedule(ctx context.Context, id int64) (ScheduleRow, error) {
	row, err := scanSchedule(s.db.QueryRowContext(ctx, `SELECT `+scheduleColumns+` FROM schedules WHERE id = ?`, id).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return ScheduleRow{}, ErrNotFound
	}
	return row, err
}

// ScheduleByKey is the schedule key made: a session's own by the team
// call that made it.
func (s *Store) ScheduleByKey(ctx context.Context, key string) (ScheduleRow, error) {
	row, err := scanSchedule(s.db.QueryRowContext(ctx, `SELECT `+scheduleColumns+` FROM schedules WHERE key = ?`, key).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return ScheduleRow{}, ErrNotFound
	}
	return row, err
}

// DueSchedules are the schedules that go out by now, soonest first
// (schedules_due).
func (s *Store) DueSchedules(ctx context.Context, now time.Time) ([]ScheduleRow, error) {
	return queryRows(ctx, s.db, scanSchedule, dueSchedules, toMillis(now))
}

// NextSchedule is the soonest time a schedule goes out; false for none.
func (s *Store) NextSchedule(ctx context.Context) (time.Time, bool, error) {
	var next sql.NullInt64
	if err := s.db.QueryRowContext(ctx, `SELECT MIN(next_at) FROM schedules WHERE state = ?`, ScheduleActive).Scan(&next); err != nil {
		return time.Time{}, false, err
	}
	if !next.Valid {
		return time.Time{}, false, nil
	}
	return fromMillis(next.Int64), true, nil
}

// Fire is what a schedule's time does: it files Text for Session, or,
// with no Session, is missed for Why; and Next is when the schedule goes
// out after it, zero for never.
type Fire struct {
	Now     time.Time
	Session int64
	Why     string
	Text    string
	Next    time.Time
	// Gather, for a gather's schedule, is the gather the time starts in
	// place of a note (ADR-0033): keyed as the note would be, so a time
	// starts one gather whatever stops pocketd.
	Gather *GatherStart
}

// FireSchedule does schedule id's time due, once, in one transaction: only
// while it is active and due then, so a time a cancel, a change of the
// owner's file or another pass moved on does nothing (outcome ""). A
// repeat (one with a next time) whose last message's note is not placed
// yet skips the time, counted, and keeps its count, so its next time looks
// at the same note.
// Otherwise the time files its note, keyed by the schedule's owner and
// the time's number, and counts it; with no session it is missed. Either
// way the schedule moves to f.Next, and is done with none.
func (s *Store) FireSchedule(ctx context.Context, id int64, due time.Time, f Fire) (ScheduleRow, string, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ScheduleRow{}, "", err
	}
	defer func() { _ = tx.Rollback() }()
	row, err := scanSchedule(tx.QueryRowContext(ctx, `SELECT `+scheduleColumns+` FROM schedules WHERE id = ?`, id).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return ScheduleRow{}, "", ErrNotFound
	} else if err != nil {
		return ScheduleRow{}, "", err
	}
	if row.State != ScheduleActive || row.NextAt == nil || !row.NextAt.Equal(due) {
		return row, "", nil
	}
	outcome, why := OutcomeSent, ""
	switch {
	case f.Session == 0:
		outcome, why = OutcomeMissed, f.Why
	case row.Runs > 0 && !f.Next.IsZero():
		// Only a repeat waits for its last message: a time that goes out
		// once, as one the owner's file made of a repeat, has no next time to
		// wait for (Codex review of #51, round 1).
		// A gather's repeat waits for its last gather to end, as a
		// message's waits for its last message (ADR-0033).
		held, err := heldBy(ctx, tx, row)
		if err != nil {
			return ScheduleRow{}, "", err
		}
		if held != "" {
			outcome, why = OutcomeSkipped, held
		}
	}
	runs, skipped := row.Runs, row.Skipped
	switch {
	case outcome == OutcomeSent && f.Gather != nil:
		g := *f.Gather
		g.Key, g.SupervisorSession = row.NoteKey(row.Runs), f.Session
		if _, _, err := insertGather(ctx, tx, g, s.millis()); err != nil {
			return ScheduleRow{}, "", err
		}
		runs++
	case outcome == OutcomeSent:
		if err := insertNote(ctx, tx, NoteRow{Key: row.NoteKey(row.Runs), SessionID: f.Session, Text: f.Text}, s.millis()); err != nil {
			return ScheduleRow{}, "", err
		}
		runs++
	case outcome == OutcomeSkipped:
		skipped++
	}
	state, next := ScheduleActive, sql.NullInt64{}
	if f.Next.IsZero() {
		state = ScheduleDone
	} else {
		next = sql.NullInt64{Int64: toMillis(f.Next), Valid: true}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE schedules SET runs = ?, skipped = ?, last_at = ?, last_outcome = ?, last_why = ?, next_at = ?, state = ?, updated_at = ? WHERE id = ?`,
		runs, skipped, toMillis(f.Now), outcome, why, next, state, s.millis(), id); err != nil {
		return ScheduleRow{}, "", err
	}
	row, err = scanSchedule(tx.QueryRowContext(ctx, `SELECT `+scheduleColumns+` FROM schedules WHERE id = ?`, id).Scan)
	if err != nil {
		return ScheduleRow{}, "", err
	}
	return row, outcome, tx.Commit()
}

// gatherRuns says the gather of key was started and still runs.
// heldBy is what holds a repeat's time, "" for nothing: its last message
// not placed yet, or its last gather still running. The skip says which,
// whatever the next time's action is (Codex review of #57, round 3).
func heldBy(ctx context.Context, tx *sql.Tx, row ScheduleRow) (string, error) {
	if row.Runs == 0 {
		return "", nil
	}
	message, err := noteWaits(ctx, tx, row.NoteKey(row.Runs-1))
	if err != nil {
		return "", err
	}
	if message {
		return "its last message still waits for the session", nil
	}
	gather, err := gatherRuns(ctx, tx, row.NoteKey(row.Runs-1))
	if err != nil || !gather {
		return "", err
	}
	return "its last gather still runs", nil
}

// SkipIfHeld skips schedule id's time due, a repeat's, once, in one
// transaction, when its last message or gather still holds it (heldBy),
// before its next action is prepared: a time held so is skipped whatever
// that action's checks would say (Codex review of #57, round 5). It moves
// the schedule to next; outcome is "" when nothing holds the time, or it
// is no longer due then.
func (s *Store) SkipIfHeld(ctx context.Context, id int64, due, now, next time.Time) (string, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback() }()
	row, err := scanSchedule(tx.QueryRowContext(ctx, `SELECT `+scheduleColumns+` FROM schedules WHERE id = ?`, id).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	} else if err != nil {
		return "", err
	}
	if row.State != ScheduleActive || row.NextAt == nil || !row.NextAt.Equal(due) || next.IsZero() {
		return "", nil
	}
	why, err := heldBy(ctx, tx, row)
	if err != nil || why == "" {
		return "", err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE schedules SET skipped = skipped + 1, last_at = ?, last_outcome = ?, last_why = ?, next_at = ?, updated_at = ? WHERE id = ?`,
		toMillis(now), OutcomeSkipped, why, toMillis(next), s.millis(), id); err != nil {
		return "", err
	}
	return OutcomeSkipped, tx.Commit()
}

func gatherRuns(ctx context.Context, tx *sql.Tx, key string) (bool, error) {
	var state string
	err := tx.QueryRowContext(ctx, `SELECT state FROM gathers WHERE key = ?`, key).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return state == team.GatherRunning, err
}

// noteWaits says note key is filed and neither placed nor dropped.
func noteWaits(ctx context.Context, tx *sql.Tx, key string) (bool, error) {
	var sent, dropped sql.NullInt64
	err := tx.QueryRowContext(ctx, `SELECT sent_at, dropped_at FROM notes WHERE key = ?`, key).Scan(&sent, &dropped)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	return !sent.Valid && !dropped.Valid, nil
}

// CancelSchedule cancels session's own schedule id: it goes out no more.
// One that is done or cancelled stays as it is; one of another session,
// or the owner's, is ErrNotFound.
func (s *Store) CancelSchedule(ctx context.Context, id, session int64) (ScheduleRow, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ScheduleRow{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `UPDATE schedules SET state = ?, next_at = NULL, updated_at = ? WHERE id = ? AND source = ? AND session_id = ? AND state = ?`,
		ScheduleCancelled, s.millis(), id, ScheduleSession, session, ScheduleActive); err != nil {
		return ScheduleRow{}, err
	}
	row, err := scanSchedule(tx.QueryRowContext(ctx, `SELECT `+scheduleColumns+` FROM schedules WHERE id = ? AND source = ? AND session_id = ?`,
		id, ScheduleSession, session).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return ScheduleRow{}, ErrNotFound
	} else if err != nil {
		return ScheduleRow{}, err
	}
	return row, tx.Commit()
}
