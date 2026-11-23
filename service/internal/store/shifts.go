package store

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/frostyard/pocketgo/service/internal/team"
)

// ShiftRow is a thread's shift (ADR-0024): its rules, its round and state,
// and each round's verdict.
type ShiftRow struct {
	ThreadID  int64
	Stop      string
	MaxRounds int
	// Deadline is zero for none.
	Deadline time.Time
	CI       bool
	// State is team.ShiftRunning, ShiftSummary, ShiftPaused or ShiftEnded;
	// Reason why it stopped or paused (a team.Shift… reason), Why in
	// words.
	State, Reason, Why string
	// Round is the round under way, from 1: the brief is round 1.
	Round int
	// Failures counts the rounds in a row whose done saw Fingerprint's
	// checks fail.
	Failures    int
	Fingerprint string
	// CIHeads are the pull requests a done's CI was checked green on, each
	// "owner/name#number@head", for a shift whose summary round that done
	// began: its report links no other (Codex review of #36).
	CIHeads []string
	// ReportRun is the run, by its first entry, that filed the summary
	// round's report: it files no second final report (Codex review of
	// #36).
	ReportRun int64
	CreatedAt time.Time
	UpdatedAt time.Time
	EndedAt   *time.Time
	// Rounds are the verdicts of the rounds that ended, oldest first.
	Rounds []ShiftRound
}

// ShiftRound is one round's verdict, the specialist's STATUS line.
type ShiftRound struct {
	N       int
	Verdict string
	Reason  string
	At      time.Time
}

const shiftColumns = `thread_id, stop, max_rounds, deadline, ci, state, reason, why, round, failures, fingerprint, ci_heads, report_run, created_at, updated_at, ended_at`

func scanShift(scan func(...any) error) (ShiftRow, error) {
	var row ShiftRow
	var deadline, ended sql.NullInt64
	var created, updated int64
	var heads string
	if err := scan(&row.ThreadID, &row.Stop, &row.MaxRounds, &deadline, &row.CI, &row.State, &row.Reason, &row.Why, &row.Round,
		&row.Failures, &row.Fingerprint, &heads, &row.ReportRun, &created, &updated, &ended); err != nil {
		return ShiftRow{}, err
	}
	if heads != "" {
		row.CIHeads = strings.Split(heads, "\n")
	}
	if deadline.Valid {
		row.Deadline = fromMillis(deadline.Int64)
	}
	if ended.Valid {
		t := fromMillis(ended.Int64)
		row.EndedAt = &t
	}
	row.CreatedAt, row.UpdatedAt = fromMillis(created), fromMillis(updated)
	return row, nil
}

// loadShift reads thread's shift with its rounds.
func loadShift(ctx context.Context, q queryer, thread int64) (ShiftRow, error) {
	row, err := scanShift(q.QueryRowContext(ctx, `SELECT `+shiftColumns+` FROM shifts WHERE thread_id = ?`, thread).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return ShiftRow{}, ErrNotFound
	} else if err != nil {
		return ShiftRow{}, err
	}
	row.Rounds, err = queryRows(ctx, q, func(scan func(...any) error) (ShiftRound, error) {
		var r ShiftRound
		var at int64
		err := scan(&r.N, &r.Verdict, &r.Reason, &at)
		r.At = fromMillis(at)
		return r, err
	}, `SELECT n, verdict, reason, at FROM shift_rounds WHERE thread_id = ? ORDER BY n`, thread)
	return row, err
}

// Shift is thread's shift, ErrNotFound for a thread with none.
func (s *Store) Shift(ctx context.Context, thread int64) (ShiftRow, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return ShiftRow{}, err
	}
	defer func() { _ = tx.Rollback() }()
	return loadShift(ctx, tx, thread)
}

// liveShiftPredicate is shifts_live's own predicate, as written there, so
// SQLite reads the index, never every shift ended (Codex review of #36).
const liveShiftPredicate = `state IN ('running', 'summary')`

// LiveShifts are the shifts pocketd drives: running or in their summary
// round, oldest first.
func (s *Store) LiveShifts(ctx context.Context) ([]ShiftRow, error) {
	ids, err := queryRows(ctx, s.db, func(scan func(...any) error) (int64, error) {
		var id int64
		return id, scan(&id)
	}, `SELECT thread_id FROM shifts WHERE `+liveShiftPredicate+` ORDER BY thread_id`)
	if err != nil {
		return nil, err
	}
	out := make([]ShiftRow, 0, len(ids))
	for _, id := range ids {
		row, err := s.Shift(ctx, id)
		if err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, nil
}

// CreateShiftThread is CreateThread for a delegation with a shift: the
// thread and its shift, running at round 1, are recorded in one
// transaction. A key recorded already returns its thread, as CreateThread
// does, with the shift it was made with.
func (s *Store) CreateShiftThread(ctx context.Context, row ThreadRow, shift ShiftRow) (ThreadRow, bool, error) {
	now := s.millis()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ThreadRow{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	got, existed, err := insertThread(ctx, tx, row, now)
	if err != nil {
		return ThreadRow{}, false, err
	}
	if !existed {
		var deadline any
		if !shift.Deadline.IsZero() {
			deadline = toMillis(shift.Deadline)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO shifts (thread_id, stop, max_rounds, deadline, ci, state, round, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, 1, ?, ?)`, got.ID, shift.Stop, shift.MaxRounds, deadline, shift.CI, team.ShiftRunning, now, now); err != nil {
			return ThreadRow{}, false, err
		}
	}
	return got, existed, tx.Commit()
}

// ShiftClaim is the owner's request a change of a shift claims, in the
// transaction that changes it: the same request again changes nothing,
// and reads the shift as it is. KeepOnConflict claims it also when the
// shift is not as the change wants: a request that succeeds whatever the
// shift is (a stop of an ended shift), so its retry never acts on a later
// state (Codex review of #36).
type ShiftClaim struct {
	Key, Kind      string
	KeepOnConflict bool
}

// shiftChange runs change on thread's shift, read in the transaction, and
// reads it back: ErrConflict when want says the shift is not as the
// caller read it. With claim, a request claimed already changes nothing
// and returns the shift as it is (replayed); another request's key is
// ErrRequestReused. The thread's own updated_at moves with the shift, which
// the thread carries (Codex review of #36).
func (s *Store) shiftChange(ctx context.Context, thread int64, claim *ShiftClaim, want func(ShiftRow) bool, change func(tx *sql.Tx, row ShiftRow, now int64) error) (row ShiftRow, replayed bool, err error) {
	now := s.millis()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ShiftRow{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	row, err = loadShift(ctx, tx, thread)
	if err != nil {
		return ShiftRow{}, false, err
	}
	if claim != nil {
		_, existed, err := claimRequest(ctx, tx, claim.Key, claim.Kind, now)
		if err != nil {
			return ShiftRow{}, false, err
		}
		if existed {
			return row, true, nil
		}
	}
	if !want(row) {
		if claim != nil && claim.KeepOnConflict {
			if err := tx.Commit(); err != nil {
				return ShiftRow{}, false, err
			}
		}
		return row, false, ErrConflict
	}
	if err := change(tx, row, now); err != nil {
		return ShiftRow{}, false, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE shifts SET updated_at = ? WHERE thread_id = ?`, now, thread); err != nil {
		return ShiftRow{}, false, err
	}
	if err := touchThread(ctx, tx, thread, now); err != nil {
		return ShiftRow{}, false, err
	}
	got, err := loadShift(ctx, tx, thread)
	if err != nil {
		return ShiftRow{}, false, err
	}
	return got, false, tx.Commit()
}

// touchThread moves thread's updated_at to now, never back.
func touchThread(ctx context.Context, tx *sql.Tx, thread, now int64) error {
	_, err := tx.ExecContext(ctx, `UPDATE threads SET updated_at = MAX(updated_at, ?) WHERE id = ?`, now, thread)
	return err
}

// queueShiftMessage keeps message for the thread's specialist in the
// outbox, in tx, while the thread is open.
func queueShiftMessage(ctx context.Context, tx *sql.Tx, message ReportRow, now int64) error {
	var state string
	if err := tx.QueryRowContext(ctx, `SELECT state FROM threads WHERE id = ?`, message.ThreadID).Scan(&state); err != nil {
		return err
	}
	if state == ThreadClosed {
		return ErrConflict
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO reports (key, thread_id, session_id, text, created_at) VALUES (?, ?, ?, ?, ?) ON CONFLICT (key) DO NOTHING`,
		message.Key, message.ThreadID, message.SessionID, message.Text, now)
	return err
}

// recordRound keeps round n's verdict, once.
func recordRound(ctx context.Context, tx *sql.Tx, thread int64, n int, verdict, reason string, now int64) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO shift_rounds (thread_id, n, verdict, reason, at) VALUES (?, ?, ?, ?, ?) ON CONFLICT DO NOTHING`,
		thread, n, verdict, reason, now)
	return err
}

// NextShiftRound ends round `from` of thread's running shift with its
// verdict, and starts the next: the round count, the CI failure streak and
// the next round's message, kept in the outbox, in one transaction.
// ErrConflict says the shift is no longer running at that round.
func (s *Store) NextShiftRound(ctx context.Context, thread int64, from int, verdict, reason string, failures int, fingerprint string, message ReportRow, quiet ...int64) (ShiftRow, error) {
	row, _, err := s.shiftChange(ctx, thread, nil, func(r ShiftRow) bool { return r.State == team.ShiftRunning && r.Round == from },
		func(tx *sql.Tx, _ ShiftRow, now int64) error {
			if err := owedNothing(ctx, tx, quiet); err != nil {
				return err
			}
			if err := recordRound(ctx, tx, thread, from, verdict, reason, now); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE shifts SET round = ?, failures = ?, fingerprint = ? WHERE thread_id = ?`, from+1, failures, fingerprint, thread); err != nil {
				return err
			}
			return queueShiftMessage(ctx, tx, message, now)
		})
	return row, err
}

// SummarizeShift ends round `from` of thread's running shift with its
// verdict and stops the shift for stop (a reason) and why, with the CI
// failure streak as it stands then, and the pull requests whose CI a done
// was checked green on (ciHeads): the shift moves to its summary round,
// whose message is kept in the outbox, in one transaction. When text is
// non-nil, it builds the message from the thread's open asks and those
// answered since the shift began, read in that transaction.
func (s *Store) SummarizeShift(ctx context.Context, thread int64, from int, verdict, reason string, failures int, fingerprint, stop, why string, ciHeads []string, message ReportRow, text func(open, answered []team.ShiftAsk) string, quiet ...int64) (ShiftRow, error) {
	row, _, err := s.shiftChange(ctx, thread, nil, func(r ShiftRow) bool { return r.State == team.ShiftRunning && r.Round == from },
		func(tx *sql.Tx, _ ShiftRow, now int64) error {
			if err := owedNothing(ctx, tx, quiet); err != nil {
				return err
			}
			if verdict != "" {
				if err := recordRound(ctx, tx, thread, from, verdict, reason, now); err != nil {
					return err
				}
			}
			if _, err := tx.ExecContext(ctx, `UPDATE shifts SET state = ?, reason = ?, why = ?, failures = ?, fingerprint = ?, ci_heads = ? WHERE thread_id = ?`,
				team.ShiftSummary, stop, why, failures, fingerprint, strings.Join(ciHeads, "\n"), thread); err != nil {
				return err
			}
			if text != nil {
				open, answered, err := shiftAsks(ctx, tx, thread)
				if err != nil {
					return err
				}
				message.Text = text(open, answered)
			}
			return queueShiftMessage(ctx, tx, message, now)
		})
	return row, err
}

// shiftAsks reads the asks a shift's summary names in the same transaction.
func shiftAsks(ctx context.Context, tx *sql.Tx, thread int64) (open, answered []team.ShiftAsk, err error) {
	for _, state := range []string{AskOpen, AskAnswered} {
		query := `SELECT ` + askColumns + ` FROM asks WHERE thread_id = ? AND state = ?`
		args := []any{thread, state}
		if state == AskAnswered {
			query += ` AND answered_at >= (SELECT created_at FROM shifts WHERE thread_id = ?)`
			args = append(args, thread)
		}
		rows, err := tx.QueryContext(ctx, query+` ORDER BY id`, args...)
		if err != nil {
			return nil, nil, err
		}
		for rows.Next() {
			a, err := scanAsk(rows.Scan)
			if err != nil {
				_ = rows.Close()
				return nil, nil, err
			}
			ask := team.ShiftAsk{ID: a.ID, Question: a.Question, Answer: a.Answer}
			if state == AskOpen {
				open = append(open, ask)
			} else {
				answered = append(answered, ask)
			}
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			return nil, nil, err
		}
	}
	return open, answered, nil
}

// ShiftReport is the report pocketd files for its supervisor when a shift
// ends with none of the specialist's: its key, the thread's state and its
// text, as FileReport takes them.
type ShiftReport struct {
	Key, State, Message string
	Text                func(asks []int64) string
}

// ShiftEnd is how a shift ends (EndShift).
type ShiftEnd struct {
	Reason, Why string
	// From are the states it ends from; Round, when not 0, the round the
	// end was decided at: a shift at another is ErrConflict, so a stale
	// decision never ends a later round (Codex review of #36).
	From  []string
	Round int
	// Claim, when set, is the owner's request, claimed with the change.
	Claim *ShiftClaim
	// Report is pocketd's report, made from the shift as it ended.
	Report func(ShiftRow) ShiftReport
	// Quiet are the sessions the end needs owed nothing (owedNothing).
	Quiet []int64
	// RecordRound records the round it ends at with no verdict, and ends
	// the CI failure streak: a run that failed with no answer (Codex
	// review of #36).
	RecordRound bool
}

// EndShift ends thread's shift as end says, and files its report on its
// thread in the same transaction, so the supervisor is told once whatever
// stops pocketd (ADR-0024; Codex review of #36). replayed says the claim
// was made already: nothing changed.
func (s *Store) EndShift(ctx context.Context, thread int64, end ShiftEnd) (ShiftRow, ThreadRow, ReportRow, bool, error) {
	var updated ThreadRow
	var message ReportRow
	want := func(r ShiftRow) bool {
		return slices.Contains(end.From, r.State) && (end.Round == 0 || r.Round == end.Round)
	}
	row, replayed, err := s.shiftChange(ctx, thread, end.Claim, want, func(tx *sql.Tx, cur ShiftRow, now int64) error {
		if err := owedNothing(ctx, tx, end.Quiet); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE shifts SET state = ?, reason = ?, why = ?, ended_at = ? WHERE thread_id = ?`,
			team.ShiftEnded, end.Reason, end.Why, now, thread); err != nil {
			return err
		}
		if end.RecordRound {
			if err := recordRound(ctx, tx, thread, cur.Round, team.VerdictNone, "", now); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE shifts SET failures = 0, fingerprint = '' WHERE thread_id = ?`, thread); err != nil {
				return err
			}
		}
		ended, err := loadShift(ctx, tx, thread)
		if err != nil {
			return err
		}
		r := end.Report(ended)
		updated, _, message, err = fileReport(ctx, tx, now, r.Key, thread, r.State, r.Message, nil, r.Text)
		return err
	})
	if replayed || errors.Is(err, ErrConflict) {
		if got, readErr := s.Thread(ctx, thread); readErr == nil {
			updated = got
		}
	}
	return row, updated, message, replayed, err
}

// ResumeShift goes on with thread's shift at its next round, whose message
// is kept in the outbox, with the owner's request claimed, in one
// transaction: a paused shift, or one that ended for a reason ok takes,
// with rounds left. ErrConflict says it cannot.
func (s *Store) ResumeShift(ctx context.Context, thread int64, claim *ShiftClaim, ok func(ShiftRow) bool, message func(ShiftRow) ReportRow) (ShiftRow, bool, error) {
	return s.shiftChange(ctx, thread, claim, ok, func(tx *sql.Tx, row ShiftRow, now int64) error {
		if _, err := tx.ExecContext(ctx, `UPDATE shifts SET state = ?, reason = '', why = '', ended_at = NULL, round = ? WHERE thread_id = ?`,
			team.ShiftRunning, row.Round+1, thread); err != nil {
			return err
		}
		next := row
		next.Round++
		return queueShiftMessage(ctx, tx, message(next), now)
	})
}

// shiftClosed ends the shift of a thread that closed, in the transaction
// that closes it (threadClosed). A shift that was not ended yet tells its
// supervisor why it ended, by a note in the same transaction: a closed
// thread takes no report (Codex review of #36).
func shiftClosed(ctx context.Context, tx *sql.Tx, before ThreadRow, now int64) error {
	row, err := loadShift(ctx, tx, before.ID)
	if errors.Is(err, ErrNotFound) || err == nil && row.State == team.ShiftEnded {
		return nil
	} else if err != nil {
		return err
	}
	why := team.ShiftWhy(team.ShiftClosed, "", 0, 0)
	if _, err := tx.ExecContext(ctx, `UPDATE shifts SET state = ?, reason = ?, why = ?, ended_at = ?, updated_at = ? WHERE thread_id = ?`,
		team.ShiftEnded, team.ShiftClosed, why, now, now, before.ID); err != nil {
		return err
	}
	if before.SupervisorSession == 0 {
		return nil
	}
	return insertNote(ctx, tx, NoteRow{Key: "shift-closed:" + before.Owner + ":" + strconv.Itoa(row.Round), SessionID: before.SupervisorSession,
		Text: team.ShiftClosedNote(before.ID, row.Round, row.MaxRounds)}, now)
}

// ShiftsOf are the shifts of threads, by thread, read with them in tx.
func shiftsOf(ctx context.Context, tx *sql.Tx, threads []ThreadRow) (map[int64]ShiftRow, error) {
	// Two reads, however many shifts there are (Codex review of #36).
	have := make(map[int64]bool, len(threads))
	for _, t := range threads {
		have[t.ID] = true
	}
	rows, err := queryRows(ctx, tx, scanShift, `SELECT `+shiftColumns+` FROM shifts ORDER BY thread_id`)
	if err != nil {
		return nil, err
	}
	out := make(map[int64]ShiftRow, len(rows))
	for _, row := range rows {
		if have[row.ThreadID] {
			out[row.ThreadID] = row
		}
	}
	type round struct {
		thread int64
		ShiftRound
	}
	rounds, err := queryRows(ctx, tx, func(scan func(...any) error) (round, error) {
		var r round
		var at int64
		err := scan(&r.thread, &r.N, &r.Verdict, &r.Reason, &at)
		r.At = fromMillis(at)
		return r, err
	}, `SELECT thread_id, n, verdict, reason, at FROM shift_rounds ORDER BY thread_id, n`)
	if err != nil {
		return nil, err
	}
	for _, r := range rounds {
		if row, ok := out[r.thread]; ok {
			row.Rounds = append(row.Rounds, r.ShiftRound)
			out[r.thread] = row
		}
	}
	return out, nil
}

// ThreadsWithShifts is Threads with each one's shift, read in one
// transaction: a change that moves a thread and its shift together is
// seen whole, or not at all (Codex review of #36).
func (s *Store) ThreadsWithShifts(ctx context.Context) ([]ThreadRow, map[int64]ShiftRow, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = tx.Rollback() }()
	threads, err := queryRows(ctx, tx, scanThread, `SELECT `+threadColumns+` FROM threads ORDER BY updated_at DESC, id DESC`)
	if err != nil {
		return nil, nil, err
	}
	shifts, err := shiftsOf(ctx, tx, threads)
	return threads, shifts, err
}

// Report is the outbox message key, ErrNotFound for none.
func (s *Store) Report(ctx context.Context, key string) (ReportRow, error) {
	row, err := scanReport(s.db.QueryRowContext(ctx, `SELECT `+reportColumns+` FROM reports WHERE key = ?`, key).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return ReportRow{}, ErrNotFound
	}
	return row, err
}

// Owed is what pocketd still owes sessions, whatever thread filed it:
// the messages not placed yet (a cross-review's verdict to its author
// included) and the answers not delivered yet. A shift's round that owes
// either is not over, and the owner's stop drops both (ADR-0024; Codex
// review of #36).
func (s *Store) Owed(ctx context.Context, sessions ...int64) (reports []ReportRow, answers []AskRow, err error) {
	for _, session := range sessions {
		if session == 0 {
			continue
		}
		rows, err := s.unsentReports(ctx, `SELECT `+reportColumns+` FROM reports WHERE session_id = ? AND sent_at IS NULL ORDER BY rowid`, session)
		if err != nil {
			return nil, nil, err
		}
		reports = append(reports, rows...)
		asks, err := queryRows(ctx, s.db, scanAsk, `SELECT `+askColumns+` FROM asks WHERE session_id = ? AND state = ? AND delivered_at IS NULL ORDER BY id`, session, AskAnswered)
		if err != nil {
			return nil, nil, err
		}
		answers = append(answers, asks...)
	}
	return reports, answers, nil
}

// ThreadWithShift is thread id with its shift (nil for none), read in one
// transaction (Codex review of #36).
func (s *Store) ThreadWithShift(ctx context.Context, id int64) (ThreadRow, *ShiftRow, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
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
	sh, err := loadShift(ctx, tx, id)
	if errors.Is(err, ErrNotFound) {
		return thread, nil, nil
	} else if err != nil {
		return ThreadRow{}, nil, err
	}
	return thread, &sh, nil
}

// ClaimSend claims the owner's message to a session, by its request key,
// before it goes, as ClaimRequest does: a new claim pauses the running
// shift of thread, whose specialist's session it is (0 for none), in the
// same transaction, so a message PiG has pauses its shift whatever stops
// pocketd after, and a retry of it pauses nothing again (ADR-0024; Codex
// review of #36). The caller completes the claim once the message went
// (CompleteRequest), and releases it if it did not (ReleaseRequest), which
// frees it only if it paused nothing; a pause stays. paused says the shift
// paused.
func (s *Store) ClaimSend(ctx context.Context, key, kind string, thread int64, why string) (result string, existed, paused bool, err error) {
	now := s.millis()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", false, false, err
	}
	defer func() { _ = tx.Rollback() }()
	result, existed, err = claimRequest(ctx, tx, key, kind, now)
	if err != nil {
		return "", false, false, err
	}
	if existed || thread == 0 {
		return result, existed, false, tx.Commit()
	}
	res, err := tx.ExecContext(ctx, `UPDATE shifts SET state = ?, reason = ?, why = ?, updated_at = ? WHERE thread_id = ? AND state = ?`,
		team.ShiftPaused, team.ShiftPerson, why, now, thread, team.ShiftRunning)
	if err != nil {
		return "", false, false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return "", false, false, err
	}
	if n > 0 {
		if err := touchThread(ctx, tx, thread, now); err != nil {
			return "", false, false, err
		}
		// The claim says it paused the shift, in the work database: no
		// release frees it (ReleaseRequest frees only an empty one), so a
		// retry, after a restart too, pauses no later round (Codex review
		// of #36).
		if _, err := tx.ExecContext(ctx, `UPDATE requests SET result = ? WHERE key = ?`, RequestPaused, key); err != nil {
			return "", false, false, err
		}
	}
	return "", false, n > 0, tx.Commit()
}

// RequestPaused is the result of the owner's message's claim that paused
// a shift, until the message went (ClaimSend).
const RequestPaused = "paused"

// owedNothing is ErrConflict when anything is owed, in tx, to one of
// sessions: a message not placed, whatever thread filed it, or an answer
// not delivered. A shift's round moves only while its specialist and its
// worker are owed nothing, checked in the transaction that moves it, so
// an answer or a verdict recorded after the round was read holds it
// (Codex review of #36).
func owedNothing(ctx context.Context, tx *sql.Tx, sessions []int64) error {
	for _, session := range sessions {
		if session == 0 {
			continue
		}
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM reports WHERE session_id = ? AND sent_at IS NULL)
			+ (SELECT COUNT(*) FROM asks WHERE session_id = ? AND state = ? AND delivered_at IS NULL)`, session, session, AskAnswered).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			return ErrConflict
		}
	}
	return nil
}

// ExpireUnbriefed closes thread id, starting or open, whose brief never
// reached its specialist, when its shift's deadline passed first: the
// shift ends for its deadline, and its supervisor gets a note why, in the
// transaction that closes the thread (Codex review of #36). A thread
// briefed, reported on or closed is left alone.
func (s *Store) ExpireUnbriefed(ctx context.Context, id int64, why string) (ThreadRow, error) {
	now := s.millis()
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
	res, err := tx.ExecContext(ctx, `UPDATE threads SET state = ?, report = ?, updated_at = ? WHERE id = ? AND state IN (?, ?) AND briefed_at IS NULL`,
		ThreadClosed, why, now, id, ThreadStarting, ThreadOpen)
	if err != nil {
		return ThreadRow{}, err
	}
	if n, err := res.RowsAffected(); err != nil {
		return ThreadRow{}, err
	} else if n == 0 {
		return before, tx.Commit()
	}
	sh, err := loadShift(ctx, tx, id)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return ThreadRow{}, err
	}
	if err == nil && sh.State != team.ShiftEnded {
		if _, err := tx.ExecContext(ctx, `UPDATE shifts SET state = ?, reason = ?, why = ?, ended_at = ?, updated_at = ? WHERE thread_id = ?`,
			team.ShiftEnded, team.ShiftDeadline, why, now, now, id); err != nil {
			return ThreadRow{}, err
		}
		if before.SupervisorSession != 0 {
			if err := insertNote(ctx, tx, NoteRow{Key: "shift-expired:" + before.Owner, SessionID: before.SupervisorSession,
				Text: team.ShiftExpiredNote(id, why)}, now); err != nil {
				return ThreadRow{}, err
			}
		}
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
