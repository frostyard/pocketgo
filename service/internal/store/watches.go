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

// Watches (ADR-0072): a session's watch of a pull request, kept until the
// pull request reaches its condition, can no longer reach it, or its time
// is up; then its end files one note to the session, in the transaction
// that ends it (EndWatch), which the outbox places.

// Watch states: live while it waits, ended once its outcome is set.
const (
	WatchLive  = "live"
	WatchEnded = "ended"
)

// WatchNotePrefix starts the key of the note a watch's end files.
const WatchNotePrefix = "watch:"

// WatchRow is a watch.
type WatchRow struct {
	ID int64
	// Key is the team_watch call that made it; Owner its random owner,
	// which keys its note, never its row number.
	Key, Owner string
	SessionID  int64
	// Repository and Number name the pull request, as GitHub spelled it
	// when the add read it, and GitHubID is GitHub's own id of it.
	Repository string
	Number     int64
	GitHubID   int64
	// Until is what it waits for (team.UntilChecks and the others), Hours
	// how long at most, and Deadline when it gives up.
	Until    string
	Hours    float64
	Deadline time.Time
	State    string
	// Outcome is how it ended (team.WatchMet and the others), Seen what
	// pocketd read last, in words.
	Outcome, Seen string
	// Answer is what the add answered: the call run again answers it.
	Answer    string
	CreatedAt time.Time
	EndedAt   *time.Time
}

// Ref is the watched pull request as owner/name#number.
func (w WatchRow) Ref() string { return w.Repository + "#" + strconv.FormatInt(w.Number, 10) }

// NoteKey is the key of the one note the watch's end files.
func (w WatchRow) NoteKey() string { return WatchNotePrefix + w.Owner }

const watchColumns = `id, key, owner, session_id, repository, number, github_id, until, hours, deadline, state, outcome, seen, answer, created_at, ended_at`

func scanWatch(scan func(...any) error) (WatchRow, error) {
	var w WatchRow
	var deadline, created int64
	var ended sql.NullInt64
	if err := scan(&w.ID, &w.Key, &w.Owner, &w.SessionID, &w.Repository, &w.Number, &w.GitHubID, &w.Until, &w.Hours, &deadline, &w.State, &w.Outcome, &w.Seen,
		&w.Answer, &created, &ended); err != nil {
		return WatchRow{}, err
	}
	w.Deadline, w.CreatedAt = fromMillis(deadline), fromMillis(created)
	if ended.Valid {
		at := fromMillis(ended.Int64)
		w.EndedAt = &at
	}
	return w, nil
}

// WatchByKey is the watch team_watch call key made.
func (s *Store) WatchByKey(ctx context.Context, key string) (WatchRow, error) {
	w, err := scanWatch(s.db.QueryRowContext(ctx, `SELECT `+watchColumns+` FROM watches WHERE key = ?`, key).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return WatchRow{}, ErrNotFound
	}
	return w, err
}

// AddWatch keeps row, once per key, with the answer answer writes of it
// as kept, in the same transaction: the same call run again gets the
// watch it made, existed true, whatever the counts are by then. A live
// one is kept only while its session keeps fewer than perSession live
// watches and pocketd fewer than total, counted in the transaction; one
// that ended as it was read (row.State WatchEnded, with its Outcome and
// Seen) counts against neither, and its end files no note.
func (s *Store) AddWatch(ctx context.Context, row WatchRow, perSession, total int, answer func(WatchRow) string) (WatchRow, bool, error) {
	if row.SessionID == 0 || row.Key == "" {
		return WatchRow{}, false, errors.New("a watch needs its session and its key")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return WatchRow{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	made, err := scanWatch(tx.QueryRowContext(ctx, `SELECT `+watchColumns+` FROM watches WHERE key = ?`, row.Key).Scan)
	if err == nil {
		return made, true, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return WatchRow{}, false, err
	}
	now := s.millis()
	ended := sql.NullInt64{}
	if row.State == WatchEnded {
		ended = sql.NullInt64{Int64: now, Valid: true}
	} else {
		row.State, row.Outcome = WatchLive, ""
		// A closed thread's specialist keeps no watch: its close ended
		// them in its own transaction, which this one comes before or
		// after (Codex review of #173, round 1).
		var thread int64
		err := tx.QueryRowContext(ctx, `SELECT id FROM threads WHERE session = ? AND state = ? LIMIT 1`, row.SessionID, ThreadClosed).Scan(&thread)
		if err == nil {
			return WatchRow{}, false, fmt.Errorf("%w: this session's thread T%d is closed, so it keeps no watch", ErrConflict, thread)
		} else if !errors.Is(err, sql.ErrNoRows) {
			return WatchRow{}, false, err
		}
		var mine, all int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(session_id = ?), 0) FROM watches WHERE state = ?`, row.SessionID, WatchLive).Scan(&all, &mine); err != nil {
			return WatchRow{}, false, err
		}
		switch {
		case mine >= perSession:
			return WatchRow{}, false, fmt.Errorf("%w: a session keeps at most %d live watches: cancel one first", ErrConflict, perSession)
		case all >= total:
			return WatchRow{}, false, fmt.Errorf("%w: pocketd keeps at most %d live watches across the team, and has them now: try again later", ErrConflict, total)
		}
	}
	owner, err := newOwner()
	if err != nil {
		return WatchRow{}, false, err
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO watches (key, owner, session_id, repository, number, github_id, until, hours, deadline, state, outcome, seen,
		answer, created_at, ended_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, '', ?, ?)`,
		row.Key, owner, row.SessionID, row.Repository, row.Number, row.GitHubID, row.Until, row.Hours, toMillis(row.Deadline), row.State, row.Outcome, row.Seen, now, ended)
	if err != nil {
		return WatchRow{}, false, err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return WatchRow{}, false, err
	}
	made, err = scanWatch(tx.QueryRowContext(ctx, `SELECT `+watchColumns+` FROM watches WHERE id = ?`, id).Scan)
	if err != nil {
		return WatchRow{}, false, err
	}
	made.Answer = answer(made)
	if _, err := tx.ExecContext(ctx, `UPDATE watches SET answer = ? WHERE id = ?`, made.Answer, id); err != nil {
		return WatchRow{}, false, err
	}
	return made, false, tx.Commit()
}

// LiveWatches are the watches that still wait, oldest first.
func (s *Store) LiveWatches(ctx context.Context) ([]WatchRow, error) {
	return queryRows(ctx, s.db, scanWatch, `SELECT `+watchColumns+` FROM watches WHERE state = ? ORDER BY id`, WatchLive)
}

// SessionWatches are session's live watches, oldest first, then at most
// ended of those that ended, the newest first, read in one transaction: a
// watch a pass ends meanwhile is in one list, never both (Codex review of
// #173, round 1).
func (s *Store) SessionWatches(ctx context.Context, session int64, ended int) ([]WatchRow, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	live, err := queryRows(ctx, tx, scanWatch, `SELECT `+watchColumns+` FROM watches WHERE session_id = ? AND state = ? ORDER BY id`, session, WatchLive)
	if err != nil {
		return nil, err
	}
	if s.watchesRead != nil {
		s.watchesRead()
	}
	done, err := queryRows(ctx, tx, scanWatch, `SELECT `+watchColumns+` FROM watches WHERE session_id = ? AND state = ? ORDER BY ended_at DESC, id DESC LIMIT ?`,
		session, WatchEnded, ended)
	if err != nil {
		return nil, err
	}
	return append(live, done...), nil
}

// EndWatch ends live watch id with outcome and seen, and files note, its
// one message, to its session in the same transaction, keyed by the
// watch's owner (NoteKey): the outbox places it. It is the one code path
// that files a watch's note. A watch that ended already changes nothing
// and files nothing, whoever ended it: ended says whether this call did.
func (s *Store) EndWatch(ctx context.Context, id int64, outcome, seen, note string) (WatchRow, bool, error) {
	if outcome == "" || note == "" {
		return WatchRow{}, false, errors.New("a watch's end needs its outcome and its note")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return WatchRow{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	now := s.millis()
	ended, err := changed(tx.ExecContext(ctx, `UPDATE watches SET state = ?, outcome = ?, seen = ?, ended_at = ? WHERE id = ? AND state = ?`,
		WatchEnded, outcome, seen, now, id, WatchLive))
	if err != nil {
		return WatchRow{}, false, err
	}
	w, err := scanWatch(tx.QueryRowContext(ctx, `SELECT `+watchColumns+` FROM watches WHERE id = ?`, id).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return WatchRow{}, false, ErrNotFound
	} else if err != nil {
		return WatchRow{}, false, err
	}
	if !ended {
		return w, false, nil
	}
	if err := insertNote(ctx, tx, NoteRow{Key: w.NoteKey(), SessionID: w.SessionID, Text: note}, now); err != nil {
		return WatchRow{}, false, err
	}
	return w, true, tx.Commit()
}

// CancelWatch ends session's watch id as cancelled, with no note, and
// answers it as it is then: cancelled says this call ended it. A watch
// that is not session's is ErrNotFound.
func (s *Store) CancelWatch(ctx context.Context, id, session int64) (WatchRow, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return WatchRow{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	cancelled, err := changed(tx.ExecContext(ctx, `UPDATE watches SET state = ?, outcome = ?, ended_at = ? WHERE id = ? AND session_id = ? AND state = ?`,
		WatchEnded, team.WatchCancelled, s.millis(), id, session, WatchLive))
	if err != nil {
		return WatchRow{}, false, err
	}
	w, err := scanWatch(tx.QueryRowContext(ctx, `SELECT `+watchColumns+` FROM watches WHERE id = ? AND session_id = ?`, id, session).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return WatchRow{}, false, ErrNotFound
	} else if err != nil {
		return WatchRow{}, false, err
	}
	return w, cancelled, tx.Commit()
}

// endThreadWatches ends the live watches of thread's specialist session
// with no note, and drops each note an ended one filed that is not sent,
// in the transaction that closes the thread (ADR-0072): a closed thread's
// specialist starts no run for a watch, as a close drops what goes only
// while its thread is open (dropAtClose).
func endThreadWatches(ctx context.Context, tx *sql.Tx, thread, now int64) error {
	var session int64
	if err := tx.QueryRowContext(ctx, `SELECT session FROM threads WHERE id = ?`, thread).Scan(&session); err != nil {
		return err
	}
	if session == 0 {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `UPDATE watches SET state = ?, outcome = ?, ended_at = ? WHERE session_id = ? AND state = ?`,
		WatchEnded, team.WatchThreadClosed, now, session, WatchLive); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `UPDATE notes SET dropped_at = ? WHERE session_id = ? AND substr(key, 1, ?) = ? AND sent_at IS NULL AND dropped_at IS NULL`,
		now, session, len(WatchNotePrefix), WatchNotePrefix)
	return err
}
