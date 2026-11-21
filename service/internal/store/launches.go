package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/frostyard/pocketgo/service/internal/team"
)

// A launch (ADR-0034) starts proposals of a supervisor's board as shift
// threads delegated from its standing session, in order, until its end.

// LaunchRow is a launch.
type LaunchRow struct {
	ID int64
	// Key is the request that made it: the owner's request key, or the
	// supervisor's team call.
	Key string
	// Owner is its random identity: its note's key names it.
	Owner             string
	SupervisorSession int64
	Profile           string
	End               time.Time
	State             string
	By                string
	CreatedAt         time.Time
	EndedAt           *time.Time
}

// LaunchItemRow is one proposal of a launch, with its agent and title as
// the proposal has them.
type LaunchItemRow struct {
	ID, LaunchID int64
	Ord          int
	ProposalID   int64
	// Owner is its random identity: its one thread is keyed by it.
	Owner, State string
	ThreadID     int64
	Why          string
	Agent, Title string
	CreatedAt    time.Time
	EndedAt      *time.Time
}

// ThreadKey is the key of the item's one thread.
func (i LaunchItemRow) ThreadKey() string { return "launch:" + i.Owner }

// LaunchFacts is a launch with its items, in order, read in one
// transaction.
type LaunchFacts struct {
	Launch LaunchRow
	Items  []LaunchItemRow
}

const launchColumns = `id, key, owner, supervisor_session, profile, end_at, state, by, created_at, ended_at`

func scanLaunch(scan func(...any) error) (LaunchRow, error) {
	var row LaunchRow
	var end, created int64
	var ended sql.NullInt64
	if err := scan(&row.ID, &row.Key, &row.Owner, &row.SupervisorSession, &row.Profile, &end, &row.State, &row.By, &created, &ended); err != nil {
		return LaunchRow{}, err
	}
	row.End, row.CreatedAt = fromMillis(end), fromMillis(created)
	if ended.Valid {
		at := fromMillis(ended.Int64)
		row.EndedAt = &at
	}
	return row, nil
}

// The item's agent and title are as its launch took them: a skipped
// proposal edited since never rewrites its launch (Codex review of #58,
// round 1).
const launchItemColumns = `i.id, i.launch_id, i.ord, i.proposal_id, i.owner, i.state, i.thread_id, i.why, i.agent, i.title, i.created_at, i.ended_at`

func scanLaunchItem(scan func(...any) error) (LaunchItemRow, error) {
	var row LaunchItemRow
	var created int64
	var ended sql.NullInt64
	if err := scan(&row.ID, &row.LaunchID, &row.Ord, &row.ProposalID, &row.Owner, &row.State, &row.ThreadID, &row.Why, &row.Agent, &row.Title, &created, &ended); err != nil {
		return LaunchItemRow{}, err
	}
	row.CreatedAt = fromMillis(created)
	if ended.Valid {
		at := fromMillis(ended.Int64)
		row.EndedAt = &at
	}
	return row, nil
}

func loadLaunchFacts(ctx context.Context, q queryer, id int64) (LaunchFacts, error) {
	launch, err := scanLaunch(q.QueryRowContext(ctx, `SELECT `+launchColumns+` FROM launches WHERE id = ?`, id).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return LaunchFacts{}, ErrNotFound
	} else if err != nil {
		return LaunchFacts{}, err
	}
	items, err := queryRows(ctx, q, scanLaunchItem, `SELECT `+launchItemColumns+` FROM launch_items i WHERE i.launch_id = ? ORDER BY i.ord`, id)
	if err != nil {
		return LaunchFacts{}, err
	}
	return LaunchFacts{Launch: launch, Items: items}, nil
}

// LaunchStart is a launch to make: the request that makes it, who, from
// which supervisor's standing session and board, until when, and its
// proposals in the order they start, with the rev each was checked at
// (Revs). RequestKind, for an owner's request, claims Key as a request of
// that kind (ErrRequestReused for another), and keeps Receipt's answer
// with it. Note, for an owner's launch, is the supervisor's one message of
// it, made from the launch as recorded.
type LaunchStart struct {
	Key, RequestKind  string
	SupervisorSession int64
	Profile           string
	End               time.Time
	By                string
	Proposals         []int64
	Revs              map[int64]int
	Note              func(LaunchFacts) string
	Receipt           func(LaunchFacts) string
}

// CreateLaunch records launch l once per key, in one transaction: the
// same key again returns the launch it made, and replayed. Each proposal
// must be proposed, on Profile's board and at the rev it was checked at,
// and the end must not have come by the store's clock, checked in the
// transaction (Codex review of #58, round 1); each proposal becomes
// launched, named by the launch. ErrConflict says one is not.
func (s *Store) CreateLaunch(ctx context.Context, l LaunchStart) (LaunchFacts, []ProposalRow, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return LaunchFacts{}, nil, false, err
	}
	defer func() { _ = tx.Rollback() }()
	now := s.millis()
	if l.RequestKind != "" {
		if _, _, err := claimRequest(ctx, tx, l.Key, l.RequestKind, now); err != nil {
			return LaunchFacts{}, nil, false, err
		}
	}
	var made int64
	if err := tx.QueryRowContext(ctx, `SELECT id FROM launches WHERE key = ?`, l.Key).Scan(&made); err == nil {
		facts, err := loadLaunchFacts(ctx, tx, made)
		return facts, nil, true, err
	} else if !errors.Is(err, sql.ErrNoRows) {
		return LaunchFacts{}, nil, false, err
	}
	if len(l.Proposals) == 0 || l.SupervisorSession == 0 {
		return LaunchFacts{}, nil, false, errors.New("a launch takes a proposal at least, for a session")
	}
	if now >= toMillis(l.End) {
		return LaunchFacts{}, nil, false, fmt.Errorf("%w: the launch's end came while it was made: name a later one", ErrConflict)
	}
	type snapshot struct{ agent, title string }
	snapshots := map[int64]snapshot{}
	for _, id := range l.Proposals {
		var state, profile, agent, title string
		var rev int
		err := tx.QueryRowContext(ctx, `SELECT p.state, p.rev, p.agent, p.title, COALESCE(s.profile, '') FROM proposals p JOIN gathers g ON g.id = p.gather_id
			LEFT JOIN sessions s ON s.id = g.supervisor_session WHERE p.id = ?`, id).Scan(&state, &rev, &agent, &title, &profile)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return LaunchFacts{}, nil, false, fmt.Errorf("%w: there is no proposal #%d", ErrNotFound, id)
		case err != nil:
			return LaunchFacts{}, nil, false, err
		case profile != l.Profile:
			return LaunchFacts{}, nil, false, fmt.Errorf("%w: #%d is on another supervisor's board", ErrConflict, id)
		case state != team.ProposalProposed:
			return LaunchFacts{}, nil, false, fmt.Errorf("%w: #%d is %s: only a proposed one launches", ErrConflict, id, state)
		case l.Revs != nil && rev != l.Revs[id]:
			return LaunchFacts{}, nil, false, fmt.Errorf("%w: #%d changed while the launch was made: it is at rev %d now; launch it again", ErrConflict, id, rev)
		}
		snapshots[id] = snapshot{agent, title}
	}
	owner, err := newOwner()
	if err != nil {
		return LaunchFacts{}, nil, false, err
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO launches (key, owner, supervisor_session, profile, end_at, state, by, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		l.Key, owner, l.SupervisorSession, l.Profile, toMillis(l.End), team.LaunchRunning, l.By, now)
	if err != nil {
		return LaunchFacts{}, nil, false, err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return LaunchFacts{}, nil, false, err
	}
	var proposals []ProposalRow
	for i, p := range l.Proposals {
		itemOwner, err := newOwner()
		if err != nil {
			return LaunchFacts{}, nil, false, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO launch_items (launch_id, ord, proposal_id, owner, agent, title, state, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			id, i, p, itemOwner, snapshots[p].agent, snapshots[p].title, team.LaunchItemWaiting, now); err != nil {
			return LaunchFacts{}, nil, false, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE proposals SET state = ?, launch_id = ?, updated_at = ? WHERE id = ?`, team.ProposalLaunched, id, now, p); err != nil {
			return LaunchFacts{}, nil, false, err
		}
		row, err := scanProposal(tx.QueryRowContext(ctx, `SELECT `+proposalColumns+` FROM proposals WHERE id = ?`, p).Scan)
		if err != nil {
			return LaunchFacts{}, nil, false, err
		}
		proposals = append(proposals, row)
	}
	facts, err := loadLaunchFacts(ctx, tx, id)
	if err != nil {
		return LaunchFacts{}, nil, false, err
	}
	if l.Note != nil {
		if err := insertNote(ctx, tx, NoteRow{Key: "launch:" + owner + ":told", SessionID: l.SupervisorSession, Text: l.Note(facts)}, now); err != nil {
			return LaunchFacts{}, nil, false, err
		}
	}
	// The owner's request answers again with the launch as it was made,
	// not as it has gone on since (Codex review of #58, round 1).
	if l.RequestKind != "" && l.Receipt != nil {
		if _, err := tx.ExecContext(ctx, `UPDATE requests SET result = ? WHERE key = ?`, l.Receipt(facts), l.Key); err != nil {
			return LaunchFacts{}, nil, false, err
		}
	}
	return facts, proposals, false, tx.Commit()
}

// LaunchByKey is the launch the request of key made.
func (s *Store) LaunchByKey(ctx context.Context, key string) (LaunchFacts, error) {
	var id int64
	err := s.db.QueryRowContext(ctx, `SELECT id FROM launches WHERE key = ?`, key).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return LaunchFacts{}, ErrNotFound
	} else if err != nil {
		return LaunchFacts{}, err
	}
	return s.LaunchFacts(ctx, id)
}

// LaunchFacts is launch id with its items.
func (s *Store) LaunchFacts(ctx context.Context, id int64) (LaunchFacts, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return LaunchFacts{}, err
	}
	defer func() { _ = tx.Rollback() }()
	return loadLaunchFacts(ctx, tx, id)
}

// RunningLaunches are the launches that run, oldest first
// (launches_running).
func (s *Store) RunningLaunches(ctx context.Context) ([]LaunchFacts, error) {
	return s.launchesWhere(ctx, `SELECT id FROM launches WHERE state = 'running' ORDER BY id`)
}

// LaunchesSince are the launches that run and those that ended at since or
// later, the newest first.
func (s *Store) LaunchesSince(ctx context.Context, since time.Time) ([]LaunchFacts, error) {
	return s.launchesWhere(ctx, `SELECT id FROM launches WHERE state = 'running' UNION SELECT id FROM launches WHERE ended_at >= ? ORDER BY id DESC`, toMillis(since))
}

func (s *Store) launchesWhere(ctx context.Context, query string, args ...any) ([]LaunchFacts, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	ids, err := queryRows(ctx, tx, func(scan func(...any) error) (int64, error) {
		var id int64
		return id, scan(&id)
	}, query, args...)
	if err != nil {
		return nil, err
	}
	out := make([]LaunchFacts, 0, len(ids))
	for _, id := range ids {
		facts, err := loadLaunchFacts(ctx, tx, id)
		if err != nil {
			return nil, err
		}
		out = append(out, facts)
	}
	return out, nil
}

// UnstartedLaunchThreads are the launch items' threads still starting
// with no specialist's session: a start a stop interrupted, whatever its
// launch's state (ADR-0034).
func (s *Store) UnstartedLaunchThreads(ctx context.Context) ([]ThreadRow, error) {
	// The desk's open threads alone (threads_open), never the launches'
	// history (Codex review of #58, round 2); a launch item's thread has
	// its item's key.
	return queryRows(ctx, s.db, scanThread, unstartedLaunchThreads)
}

// unstartedLaunchThreads reads the launch threads still starting with no
// session from the desk's open threads (threads_open), but those whose
// start failed and waits to be settled (launch_items_failed).
const unstartedLaunchThreads = `SELECT ` + threadColumns + ` FROM threads WHERE state != 'closed' AND (plan = 0 OR plan_task != '')
	AND state = 'starting' AND session = 0 AND key LIKE 'launch:%'
	AND key NOT IN (SELECT 'launch:' || owner FROM launch_items WHERE failure != '') ORDER BY id`

// MarkLaunchStartFailed keeps why on the item of launch thread id: its
// start failed, and is not settled yet. A restart reads it, so the thread
// is settled, never briefed nor started again (Codex review of #58, round
// 4).
func (s *Store) MarkLaunchStartFailed(ctx context.Context, thread int64, why string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE launch_items SET failure = ? WHERE thread_id = ? AND thread_id != 0`, team.Clip(why, team.MaxFailure), thread)
	return err
}

// ClearLaunchStartFailed says the failed start of launch thread id is
// settled.
func (s *Store) ClearLaunchStartFailed(ctx context.Context, thread int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE launch_items SET failure = '' WHERE thread_id = ? AND thread_id != 0`, thread)
	return err
}

// LaunchStartFailures are the launch threads not closed whose failed start
// is not settled, with why.
func (s *Store) LaunchStartFailures(ctx context.Context) (map[int64]string, error) {
	type failure struct {
		thread int64
		why    string
	}
	rows, err := queryRows(ctx, s.db, func(scan func(...any) error) (failure, error) {
		var f failure
		return f, scan(&f.thread, &f.why)
	}, `SELECT i.thread_id, i.failure FROM launch_items i JOIN threads t ON t.id = i.thread_id WHERE i.failure != '' AND t.state != ?`, ThreadClosed)
	if err != nil {
		return nil, err
	}
	out := map[int64]string{}
	for _, f := range rows {
		out[f.thread] = f.why
	}
	return out, nil
}

// LaunchStartFailure is why the start of launch thread id failed while it
// is not settled, "" for none.
func (s *Store) LaunchStartFailure(ctx context.Context, thread int64) (string, error) {
	var why string
	err := s.db.QueryRowContext(ctx, `SELECT failure FROM launch_items WHERE thread_id = ? AND thread_id != 0`, thread).Scan(&why)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return why, err
}

// LaunchThreadsOpen counts launch id's threads that are not closed: those
// its at-once limit counts.
func (s *Store) LaunchThreadsOpen(ctx context.Context, id int64) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM launch_items i JOIN threads t ON t.id = i.thread_id
		WHERE i.launch_id = ? AND i.thread_id != 0 AND t.state != ?`, id, ThreadClosed).Scan(&n)
	return n, err
}

// StartLaunchItem starts item id as its one thread, in one transaction:
// only while the item waits, its launch runs, and its launch's end has not
// come by the store's clock. The thread, row, is keyed by the item
// (ThreadKey), with shift as its shift and the launch's end its deadline;
// the item is started with it, and its proposal names it. started is
// false when the item no longer waits, or its end came (then it is
// skipped, with team.LaunchEndPassed, and its proposal goes back on the
// board). The launch ends when no item waits.
func (s *Store) StartLaunchItem(ctx context.Context, id int64, row ThreadRow, shift ShiftRow) (thread ThreadRow, facts LaunchFacts, proposal ProposalRow, started bool, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ThreadRow{}, LaunchFacts{}, ProposalRow{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	now := s.millis()
	item, launch, err := launchItemIn(ctx, tx, id)
	if err != nil {
		return ThreadRow{}, LaunchFacts{}, ProposalRow{}, false, err
	}
	if item.State != team.LaunchItemWaiting || launch.State != team.LaunchRunning {
		facts, err := loadLaunchFacts(ctx, tx, launch.ID)
		return ThreadRow{}, facts, ProposalRow{}, false, err
	}
	if now >= toMillis(launch.End) {
		proposal, err := skipItemIn(ctx, tx, item, team.LaunchEndPassed, now)
		if err != nil {
			return ThreadRow{}, LaunchFacts{}, ProposalRow{}, false, err
		}
		facts, err := endLaunchIfDone(ctx, tx, launch.ID, now)
		if err != nil {
			return ThreadRow{}, LaunchFacts{}, ProposalRow{}, false, err
		}
		return ThreadRow{}, facts, proposal, false, tx.Commit()
	}
	row.Key, row.SupervisorSession = item.ThreadKey(), launch.SupervisorSession
	thread, existed, err := insertThread(ctx, tx, row, now)
	if err != nil {
		return ThreadRow{}, LaunchFacts{}, ProposalRow{}, false, err
	}
	if !existed {
		if _, err := tx.ExecContext(ctx, `INSERT INTO shifts (thread_id, stop, max_rounds, deadline, ci, state, round, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, 1, ?, ?)`, thread.ID, shift.Stop, shift.MaxRounds, toMillis(launch.End), shift.CI, team.ShiftRunning, now, now); err != nil {
			return ThreadRow{}, LaunchFacts{}, ProposalRow{}, false, err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE launch_items SET state = ?, thread_id = ?, ended_at = ? WHERE id = ?`, team.LaunchItemStarted, thread.ID, now, item.ID); err != nil {
		return ThreadRow{}, LaunchFacts{}, ProposalRow{}, false, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE proposals SET thread_id = ?, updated_at = ? WHERE id = ?`, thread.ID, now, item.ProposalID); err != nil {
		return ThreadRow{}, LaunchFacts{}, ProposalRow{}, false, err
	}
	if proposal, err = scanProposal(tx.QueryRowContext(ctx, `SELECT `+proposalColumns+` FROM proposals WHERE id = ?`, item.ProposalID).Scan); err != nil {
		return ThreadRow{}, LaunchFacts{}, ProposalRow{}, false, err
	}
	if facts, err = endLaunchIfDone(ctx, tx, launch.ID, now); err != nil {
		return ThreadRow{}, LaunchFacts{}, ProposalRow{}, false, err
	}
	return thread, facts, proposal, true, tx.Commit()
}

// SkipLaunchItem skips item id with why, when it still waits: its
// proposal goes back on the board. The launch ends when no item waits.
// did says the item was skipped.
func (s *Store) SkipLaunchItem(ctx context.Context, id int64, why string) (facts LaunchFacts, proposal ProposalRow, did bool, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return LaunchFacts{}, ProposalRow{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	now := s.millis()
	item, launch, err := launchItemIn(ctx, tx, id)
	if err != nil {
		return LaunchFacts{}, ProposalRow{}, false, err
	}
	if item.State != team.LaunchItemWaiting {
		facts, err := loadLaunchFacts(ctx, tx, launch.ID)
		return facts, ProposalRow{}, false, err
	}
	if proposal, err = skipItemIn(ctx, tx, item, why, now); err != nil {
		return LaunchFacts{}, ProposalRow{}, false, err
	}
	if facts, err = endLaunchIfDone(ctx, tx, launch.ID, now); err != nil {
		return LaunchFacts{}, ProposalRow{}, false, err
	}
	return facts, proposal, true, tx.Commit()
}

func launchItemIn(ctx context.Context, tx *sql.Tx, id int64) (LaunchItemRow, LaunchRow, error) {
	item, err := scanLaunchItem(tx.QueryRowContext(ctx, `SELECT `+launchItemColumns+` FROM launch_items i WHERE i.id = ?`, id).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return LaunchItemRow{}, LaunchRow{}, ErrNotFound
	} else if err != nil {
		return LaunchItemRow{}, LaunchRow{}, err
	}
	launch, err := scanLaunch(tx.QueryRowContext(ctx, `SELECT `+launchColumns+` FROM launches WHERE id = ?`, item.LaunchID).Scan)
	return item, launch, err
}

// skipItemIn skips item with why, and puts its proposal back on the board
// while the launch still holds it.
func skipItemIn(ctx context.Context, tx *sql.Tx, item LaunchItemRow, why string, now int64) (ProposalRow, error) {
	if _, err := tx.ExecContext(ctx, `UPDATE launch_items SET state = ?, why = ?, ended_at = ? WHERE id = ?`, team.LaunchItemSkipped, team.Clip(why, team.MaxFailure), now, item.ID); err != nil {
		return ProposalRow{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE proposals SET state = ?, launch_id = 0, updated_at = ? WHERE id = ? AND state = ? AND launch_id = ? AND thread_id = 0`,
		team.ProposalProposed, now, item.ProposalID, team.ProposalLaunched, item.LaunchID); err != nil {
		return ProposalRow{}, err
	}
	return scanProposal(tx.QueryRowContext(ctx, `SELECT `+proposalColumns+` FROM proposals WHERE id = ?`, item.ProposalID).Scan)
}

// endLaunchIfDone ends launch id when no item of it waits, and reads it.
func endLaunchIfDone(ctx context.Context, tx *sql.Tx, id int64, now int64) (LaunchFacts, error) {
	if _, err := tx.ExecContext(ctx, `UPDATE launches SET state = ?, ended_at = ? WHERE id = ? AND state = ?
		AND NOT EXISTS (SELECT 1 FROM launch_items WHERE launch_id = ? AND state = ?)`, team.LaunchDone, now, id, team.LaunchRunning, id, team.LaunchItemWaiting); err != nil {
		return LaunchFacts{}, err
	}
	return loadLaunchFacts(ctx, tx, id)
}
