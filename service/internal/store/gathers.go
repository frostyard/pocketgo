package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/frostyard/pocketgo/service/internal/team"
)

// A gather (ADR-0033) asks agents for proposals, each in a proposal session
// of its own (an ask), and keeps what they propose as rows of the board.

// GatherRow is a gather.
type GatherRow struct {
	ID int64
	// Key is the team call that started it, or a schedule's time:
	// "schedule:<owner>:<n>".
	Key string
	// Owner is its random identity: its notes' keys name it.
	Owner             string
	SupervisorSession int64
	// Skill is the skill it named, and SkillText the skill's text as it
	// was when it started: every asked agent gets that one text.
	Skill, SkillText, Note string
	// Schedule is the owner's schedule that started it, "" for the
	// supervisor's own.
	Schedule  string
	State     string
	Deadline  time.Time
	CreatedAt time.Time
	EndedAt   *time.Time
}

// GatherAskRow is one agent a gather asks.
type GatherAskRow struct {
	ID, GatherID int64
	// Owner is its random identity: its proposal session and its brief are
	// keyed by it.
	Owner, Agent, State string
	Session             int64
	Why                 string
	CreatedAt           time.Time
	EndedAt             *time.Time
}

// BriefKey is the key of the ask's brief, the note its proposal session
// gets first.
func (a GatherAskRow) BriefKey() string { return GatherBriefPrefix + a.Owner + ":brief" }

// SessionKey is the engine's key of the ask's proposal session.
func (a GatherAskRow) SessionKey() string { return "gather:" + a.Owner }

// GatherBriefPrefix starts the key of every ask's brief.
const GatherBriefPrefix = "gather-ask:"

// ProposalRow is a proposal: a row of the board.
type ProposalRow struct {
	ID       int64
	Key      string
	GatherID int64
	AskID    int64
	Agent    string
	// The proposal's text (team.Proposal).
	Repository, Title, Task, StopWhen, Note string
	MaxRounds                               int
	// Kind is the gather's skill.
	Kind      string
	Order     int
	Rev       int
	State     string
	By        string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Text is the proposal's text, as team.Proposal checks it.
func (p ProposalRow) Text() team.Proposal {
	return team.Proposal{Repository: p.Repository, Title: p.Title, Task: p.Task, StopWhen: p.StopWhen, Note: p.Note, MaxRounds: p.MaxRounds}
}

// GatherFacts is a gather with its asks and its proposals not dropped,
// read in one transaction.
type GatherFacts struct {
	Gather    GatherRow
	Asks      []GatherAskRow
	Proposals []ProposalRow
}

const gatherColumns = `id, key, owner, supervisor_session, skill, skill_text, note, schedule, state, deadline, created_at, ended_at`

func scanGather(scan func(...any) error) (GatherRow, error) {
	var row GatherRow
	var deadline, created int64
	var ended sql.NullInt64
	if err := scan(&row.ID, &row.Key, &row.Owner, &row.SupervisorSession, &row.Skill, &row.SkillText, &row.Note, &row.Schedule, &row.State, &deadline, &created, &ended); err != nil {
		return GatherRow{}, err
	}
	row.Deadline, row.CreatedAt = fromMillis(deadline), fromMillis(created)
	if ended.Valid {
		at := fromMillis(ended.Int64)
		row.EndedAt = &at
	}
	return row, nil
}

const gatherAskColumns = `id, gather_id, owner, agent, state, session, why, created_at, ended_at`

func scanGatherAsk(scan func(...any) error) (GatherAskRow, error) {
	var row GatherAskRow
	var created int64
	var ended sql.NullInt64
	if err := scan(&row.ID, &row.GatherID, &row.Owner, &row.Agent, &row.State, &row.Session, &row.Why, &created, &ended); err != nil {
		return GatherAskRow{}, err
	}
	row.CreatedAt = fromMillis(created)
	if ended.Valid {
		at := fromMillis(ended.Int64)
		row.EndedAt = &at
	}
	return row, nil
}

const proposalColumns = `id, key, gather_id, ask_id, agent, repository, title, task, stop_when, max_rounds, kind, note, ord, rev, state, by, created_at, updated_at`

func scanProposal(scan func(...any) error) (ProposalRow, error) {
	var row ProposalRow
	var created, updated int64
	if err := scan(&row.ID, &row.Key, &row.GatherID, &row.AskID, &row.Agent, &row.Repository, &row.Title, &row.Task, &row.StopWhen, &row.MaxRounds,
		&row.Kind, &row.Note, &row.Order, &row.Rev, &row.State, &row.By, &created, &updated); err != nil {
		return ProposalRow{}, err
	}
	row.CreatedAt, row.UpdatedAt = fromMillis(created), fromMillis(updated)
	return row, nil
}

// GatherStart is a gather to start: who asks, the skill and its text, the
// note, the agents asked, and when their time ends.
type GatherStart struct {
	Key               string
	SupervisorSession int64
	Skill, SkillText  string
	Note, Schedule    string
	Agents            []string
	Deadline          time.Time
}

// insertGather records gather g with an ask of each of its agents, once per
// key: the same key again returns the one it made, and existed.
func insertGather(ctx context.Context, tx *sql.Tx, g GatherStart, now int64) (GatherRow, bool, error) {
	made, err := scanGather(tx.QueryRowContext(ctx, `SELECT `+gatherColumns+` FROM gathers WHERE key = ?`, g.Key).Scan)
	if err == nil {
		return made, true, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return GatherRow{}, false, err
	}
	if len(g.Agents) == 0 || g.SupervisorSession == 0 {
		return GatherRow{}, false, errors.New("a gather asks an agent at least, for a session")
	}
	owner, err := newOwner()
	if err != nil {
		return GatherRow{}, false, err
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO gathers (key, owner, supervisor_session, skill, skill_text, note, schedule, state, deadline, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, g.Key, owner, g.SupervisorSession, g.Skill, g.SkillText, g.Note, g.Schedule, team.GatherRunning, toMillis(g.Deadline), now)
	if err != nil {
		return GatherRow{}, false, err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return GatherRow{}, false, err
	}
	for _, agent := range g.Agents {
		askOwner, err := newOwner()
		if err != nil {
			return GatherRow{}, false, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO gather_asks (gather_id, owner, agent, state, created_at) VALUES (?, ?, ?, ?, ?)`,
			id, askOwner, agent, team.AskStarting, now); err != nil {
			return GatherRow{}, false, err
		}
	}
	made, err = scanGather(tx.QueryRowContext(ctx, `SELECT `+gatherColumns+` FROM gathers WHERE id = ?`, id).Scan)
	return made, false, err
}

// StartGather records gather g with its asks, once per key: replayed says
// the key made it already, and it is returned as it is now.
func (s *Store) StartGather(ctx context.Context, g GatherStart) (GatherFacts, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return GatherFacts{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	made, existed, err := insertGather(ctx, tx, g, s.millis())
	if err != nil {
		return GatherFacts{}, false, err
	}
	facts, err := loadGatherFacts(ctx, tx, made.ID)
	if err != nil {
		return GatherFacts{}, false, err
	}
	return facts, existed, tx.Commit()
}

func loadGatherFacts(ctx context.Context, q queryer, id int64) (GatherFacts, error) {
	g, err := scanGather(q.QueryRowContext(ctx, `SELECT `+gatherColumns+` FROM gathers WHERE id = ?`, id).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return GatherFacts{}, ErrNotFound
	} else if err != nil {
		return GatherFacts{}, err
	}
	facts := GatherFacts{Gather: g}
	if facts.Asks, err = queryRows(ctx, q, scanGatherAsk, `SELECT `+gatherAskColumns+` FROM gather_asks WHERE gather_id = ? ORDER BY id`, id); err != nil {
		return GatherFacts{}, err
	}
	if facts.Proposals, err = queryRows(ctx, q, scanProposal, `SELECT `+proposalColumns+` FROM proposals WHERE gather_id = ? AND state != 'dropped' ORDER BY ord, id`, id); err != nil {
		return GatherFacts{}, err
	}
	return facts, nil
}

// GatherIDByKey is the gather key started.
func (s *Store) GatherIDByKey(ctx context.Context, key string) (int64, error) {
	var id int64
	err := s.db.QueryRowContext(ctx, `SELECT id FROM gathers WHERE key = ?`, key).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	return id, err
}

// ProposalByKey is the proposal the call of key filed.
func (s *Store) ProposalByKey(ctx context.Context, key string) (ProposalRow, error) {
	row, err := scanProposal(s.db.QueryRowContext(ctx, `SELECT `+proposalColumns+` FROM proposals WHERE key = ?`, key).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return ProposalRow{}, ErrNotFound
	}
	return row, err
}

// GatherFacts is gather id with its asks and its proposals not dropped.
func (s *Store) GatherFacts(ctx context.Context, id int64) (GatherFacts, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return GatherFacts{}, err
	}
	defer func() { _ = tx.Rollback() }()
	return loadGatherFacts(ctx, tx, id)
}

// GathersSince are the gathers that run or ended at since or later, the
// newest first, each with its facts, read in one transaction.
func (s *Store) GathersSince(ctx context.Context, since time.Time) ([]GatherFacts, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	ids, err := queryRows(ctx, tx, func(scan func(...any) error) (int64, error) {
		var id int64
		return id, scan(&id)
	}, `SELECT id FROM gathers WHERE state = 'running' UNION SELECT id FROM gathers WHERE ended_at >= ? ORDER BY id DESC`, toMillis(since))
	if err != nil {
		return nil, err
	}
	out := make([]GatherFacts, 0, len(ids))
	for _, id := range ids {
		facts, err := loadGatherFacts(ctx, tx, id)
		if err != nil {
			return nil, err
		}
		out = append(out, facts)
	}
	return out, nil
}

// OpenGatherAsks are the asks that start or run (gather_asks_open), with
// their gathers' deadlines.
func (s *Store) OpenGatherAsks(ctx context.Context) ([]GatherAskRow, []time.Time, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT a.id, a.gather_id, a.owner, a.agent, a.state, a.session, a.why, a.created_at, a.ended_at, g.deadline
		FROM gather_asks a JOIN gathers g ON g.id = a.gather_id WHERE a.state IN ('starting', 'running') ORDER BY a.id`)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = rows.Close() }()
	var asks []GatherAskRow
	var deadlines []time.Time
	for rows.Next() {
		var deadline int64
		ask, err := scanGatherAsk(func(dest ...any) error { return rows.Scan(append(dest, &deadline)...) })
		if err != nil {
			return nil, nil, err
		}
		asks = append(asks, ask)
		deadlines = append(deadlines, fromMillis(deadline))
	}
	return asks, deadlines, rows.Err()
}

// GatherAsk is ask id.
func (s *Store) GatherAsk(ctx context.Context, id int64) (GatherAskRow, error) {
	row, err := scanGatherAsk(s.db.QueryRowContext(ctx, `SELECT `+gatherAskColumns+` FROM gather_asks WHERE id = ?`, id).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return GatherAskRow{}, ErrNotFound
	}
	return row, err
}

// GatherAskOfSession is the ask whose proposal session session is
// (gather_asks_session).
func (s *Store) GatherAskOfSession(ctx context.Context, session int64) (GatherAskRow, error) {
	row, err := scanGatherAsk(s.db.QueryRowContext(ctx, `SELECT `+gatherAskColumns+` FROM gather_asks WHERE session = ? AND session != 0`, session).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return GatherAskRow{}, ErrNotFound
	}
	return row, err
}

// GatherAskByOwner is the ask whose random owner is owner.
func (s *Store) GatherAskByOwner(ctx context.Context, owner string) (GatherAskRow, error) {
	row, err := scanGatherAsk(s.db.QueryRowContext(ctx, `SELECT `+gatherAskColumns+` FROM gather_asks WHERE owner = ?`, owner).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return GatherAskRow{}, ErrNotFound
	}
	return row, err
}

// RecordGatherSession records ask id's proposal session, made by the
// engine for key, and files its brief, in one transaction, only while the
// ask starts: one that ended meanwhile takes no session (its state says
// so).
func (s *Store) RecordGatherSession(ctx context.Context, key string, session SessionRow, id int64, brief func(GatherRow) string) (GatherAskRow, error) {
	var ask GatherAskRow
	_, err := s.createSession(ctx, key, session, func(tx *sql.Tx, now int64) error {
		if _, err := tx.ExecContext(ctx, `UPDATE gather_asks SET session = ?, state = ? WHERE id = ? AND state = ? AND session IN (0, ?)`,
			session.ID, team.AskRunning, id, team.AskStarting, session.ID); err != nil {
			return err
		}
		var err error
		ask, err = scanGatherAsk(tx.QueryRowContext(ctx, `SELECT `+gatherAskColumns+` FROM gather_asks WHERE id = ?`, id).Scan)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return err
		}
		if ask.State != team.AskRunning || ask.Session != session.ID {
			return nil
		}
		g, err := scanGather(tx.QueryRowContext(ctx, `SELECT `+gatherColumns+` FROM gathers WHERE id = ?`, ask.GatherID).Scan)
		if err != nil {
			return err
		}
		return insertNote(ctx, tx, NoteRow{Key: ask.BriefKey(), SessionID: session.ID, Text: brief(g)}, now)
	})
	return ask, err
}

// EndGatherAsk ends ask id with state (done or failed) and why, when it
// still starts or runs; its brief, if not sent, is dropped. When it was
// the gather's last open ask, the gather is done in the same transaction,
// and its supervisor gets the note done makes from the gather's facts,
// once. did says the ask ended; ended that the gather did too.
func (s *Store) EndGatherAsk(ctx context.Context, id int64, state, why string, done func(GatherFacts) string) (facts GatherFacts, did, ended bool, err error) {
	now := s.millis()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return GatherFacts{}, false, false, err
	}
	defer func() { _ = tx.Rollback() }()
	ask, err := scanGatherAsk(tx.QueryRowContext(ctx, `SELECT `+gatherAskColumns+` FROM gather_asks WHERE id = ?`, id).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return GatherFacts{}, false, false, ErrNotFound
	} else if err != nil {
		return GatherFacts{}, false, false, err
	}
	if !team.AskOpen(ask.State) {
		facts, err := loadGatherFacts(ctx, tx, ask.GatherID)
		return facts, false, false, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE gather_asks SET state = ?, why = ?, ended_at = ? WHERE id = ?`, state, team.Clip(why, team.MaxFailure), now, id); err != nil {
		return GatherFacts{}, false, false, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE notes SET dropped_at = ? WHERE key = ? AND sent_at IS NULL AND dropped_at IS NULL`, now, ask.BriefKey()); err != nil {
		return GatherFacts{}, false, false, err
	}
	var open int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM gather_asks WHERE gather_id = ? AND state IN ('starting', 'running')`, ask.GatherID).Scan(&open); err != nil {
		return GatherFacts{}, false, false, err
	}
	if open == 0 {
		if _, err := tx.ExecContext(ctx, `UPDATE gathers SET state = ?, ended_at = ? WHERE id = ? AND state = ?`, team.GatherDone, now, ask.GatherID, team.GatherRunning); err != nil {
			return GatherFacts{}, false, false, err
		}
		ended = true
	}
	if facts, err = loadGatherFacts(ctx, tx, ask.GatherID); err != nil {
		return GatherFacts{}, false, false, err
	}
	if ended {
		if err := insertNote(ctx, tx, NoteRow{Key: "gather:" + facts.Gather.Owner + ":done", SessionID: facts.Gather.SupervisorSession, Text: done(facts)}, now); err != nil {
			return GatherFacts{}, false, false, err
		}
	}
	return facts, true, ended, tx.Commit()
}

// AddProposal files proposal p of ask askID, once per key: the same key
// again returns the one it made, and replayed. The ask must run and its
// gather's time not have passed by now, checked in the transaction that
// files it; p's agent, gather, kind, state and order are the store's.
func (s *Store) AddProposal(ctx context.Context, key string, askID int64, p team.Proposal, by string, now time.Time) (ProposalRow, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ProposalRow{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	made, err := scanProposal(tx.QueryRowContext(ctx, `SELECT `+proposalColumns+` FROM proposals WHERE key = ?`, key).Scan)
	if err == nil {
		return made, true, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return ProposalRow{}, false, err
	}
	ask, err := scanGatherAsk(tx.QueryRowContext(ctx, `SELECT `+gatherAskColumns+` FROM gather_asks WHERE id = ?`, askID).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return ProposalRow{}, false, ErrNotFound
	} else if err != nil {
		return ProposalRow{}, false, err
	}
	g, err := scanGather(tx.QueryRowContext(ctx, `SELECT `+gatherColumns+` FROM gathers WHERE id = ?`, ask.GatherID).Scan)
	if err != nil {
		return ProposalRow{}, false, err
	}
	if ask.State != team.AskRunning || !now.Before(g.Deadline) {
		return ProposalRow{}, false, fmt.Errorf("%w: G%d takes no proposal of yours now: you said you were done, or its %d minutes passed", ErrConflict, g.ID, int(team.GatherLimit/time.Minute))
	}
	at := s.millis()
	result, err := tx.ExecContext(ctx, `INSERT INTO proposals (key, gather_id, ask_id, agent, repository, title, task, stop_when, max_rounds, kind, note, ord, state, by, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, ?, ?, ?, ?)`, key, g.ID, ask.ID, ask.Agent, p.Repository, p.Title, p.Task, p.StopWhen, p.MaxRounds, g.Skill, p.Note,
		team.ProposalProposed, by, at, at)
	if err != nil {
		return ProposalRow{}, false, err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return ProposalRow{}, false, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE proposals SET ord = ? WHERE id = ?`, id*10, id); err != nil {
		return ProposalRow{}, false, err
	}
	made, err = scanProposal(tx.QueryRowContext(ctx, `SELECT `+proposalColumns+` FROM proposals WHERE id = ?`, id).Scan)
	if err != nil {
		return ProposalRow{}, false, err
	}
	return made, false, tx.Commit()
}

// ProposalChange is one change of a proposal: the row as it is, checked
// by change, which gives it changed or refuses it; answer is the text the
// call answers, kept with its key.
type ProposalChange struct {
	Key    string
	ID     int64
	Rev    int
	Change func(ProposalRow) (ProposalRow, error)
	Answer func(ProposalRow) string
}

// ChangeProposal changes proposal c.ID once per key, in one transaction:
// the same key again answers as it did, and replayed. The proposal must be
// proposed, at rev c.Rev (a stale rev is ErrConflict); the change moves its
// rev on.
func (s *Store) ChangeProposal(ctx context.Context, c ProposalChange) (ProposalRow, string, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ProposalRow{}, "", false, err
	}
	defer func() { _ = tx.Rollback() }()
	var result string
	err = tx.QueryRowContext(ctx, `SELECT result FROM proposal_calls WHERE key = ?`, c.Key).Scan(&result)
	if err == nil {
		row, err := scanProposal(tx.QueryRowContext(ctx, `SELECT `+proposalColumns+` FROM proposals WHERE id = ?`, c.ID).Scan)
		return row, result, true, err
	} else if !errors.Is(err, sql.ErrNoRows) {
		return ProposalRow{}, "", false, err
	}
	row, err := scanProposal(tx.QueryRowContext(ctx, `SELECT `+proposalColumns+` FROM proposals WHERE id = ?`, c.ID).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return ProposalRow{}, "", false, ErrNotFound
	} else if err != nil {
		return ProposalRow{}, "", false, err
	}
	switch {
	case row.State != team.ProposalProposed:
		return ProposalRow{}, "", false, fmt.Errorf("%w: #%d is %s: only a proposed one changes", ErrConflict, row.ID, row.State)
	case row.Rev != c.Rev:
		return ProposalRow{}, "", false, fmt.Errorf("%w: #%d changed: it is at rev %d now, not %d; read it again", ErrConflict, row.ID, row.Rev, c.Rev)
	}
	changed, err := c.Change(row)
	if err != nil {
		return ProposalRow{}, "", false, err
	}
	now := s.millis()
	if _, err := tx.ExecContext(ctx, `UPDATE proposals SET repository = ?, title = ?, task = ?, stop_when = ?, max_rounds = ?, note = ?, state = ?, rev = rev + 1, updated_at = ? WHERE id = ?`,
		changed.Repository, changed.Title, changed.Task, changed.StopWhen, changed.MaxRounds, changed.Note, changed.State, now, row.ID); err != nil {
		return ProposalRow{}, "", false, err
	}
	row, err = scanProposal(tx.QueryRowContext(ctx, `SELECT `+proposalColumns+` FROM proposals WHERE id = ?`, c.ID).Scan)
	if err != nil {
		return ProposalRow{}, "", false, err
	}
	result = c.Answer(row)
	if _, err := tx.ExecContext(ctx, `INSERT INTO proposal_calls (key, result, created_at) VALUES (?, ?, ?)`, c.Key, result, now); err != nil {
		return ProposalRow{}, "", false, err
	}
	return row, result, false, tx.Commit()
}

// OrderProposals puts proposals ids at the top of the board, in that
// order, once per key: each must be proposed and pass check. Their order
// moves; their rev does not, since their text is as it was.
func (s *Store) OrderProposals(ctx context.Context, key string, ids []int64, check func(ProposalRow) error, answer func([]ProposalRow) string) ([]ProposalRow, string, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, "", false, err
	}
	defer func() { _ = tx.Rollback() }()
	var result string
	if err := tx.QueryRowContext(ctx, `SELECT result FROM proposal_calls WHERE key = ?`, key).Scan(&result); err == nil {
		return nil, result, true, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, "", false, err
	}
	var rows []ProposalRow
	for _, id := range ids {
		row, err := scanProposal(tx.QueryRowContext(ctx, `SELECT `+proposalColumns+` FROM proposals WHERE id = ?`, id).Scan)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, "", false, fmt.Errorf("%w: there is no proposal #%d", ErrNotFound, id)
		} else if err != nil {
			return nil, "", false, err
		}
		if row.State != team.ProposalProposed {
			return nil, "", false, fmt.Errorf("%w: #%d is %s: only proposed ones are ordered", ErrConflict, row.ID, row.State)
		}
		if err := check(row); err != nil {
			return nil, "", false, err
		}
		rows = append(rows, row)
	}
	// The named rows take the places above every row on the board, one
	// apart; the others keep theirs, below.
	var lowest int
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MIN(ord), 0) FROM proposals WHERE state != 'dropped'`).Scan(&lowest); err != nil {
		return nil, "", false, err
	}
	now := s.millis()
	for i := range rows {
		order := lowest - len(rows) + i
		if _, err := tx.ExecContext(ctx, `UPDATE proposals SET ord = ?, updated_at = ? WHERE id = ?`, order, now, rows[i].ID); err != nil {
			return nil, "", false, err
		}
		rows[i].Order = order
	}
	result = answer(rows)
	if _, err := tx.ExecContext(ctx, `INSERT INTO proposal_calls (key, result, created_at) VALUES (?, ?, ?)`, key, result, now); err != nil {
		return nil, "", false, err
	}
	return rows, result, false, tx.Commit()
}

// Proposal is proposal id.
func (s *Store) Proposal(ctx context.Context, id int64) (ProposalRow, error) {
	row, err := scanProposal(s.db.QueryRowContext(ctx, `SELECT `+proposalColumns+` FROM proposals WHERE id = ?`, id).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return ProposalRow{}, ErrNotFound
	}
	return row, err
}

// Proposals are the proposals not dropped, in the board's order
// (proposals_open).
func (s *Store) Proposals(ctx context.Context) ([]ProposalRow, error) {
	return queryRows(ctx, s.db, scanProposal, `SELECT `+proposalColumns+` FROM proposals WHERE state != 'dropped' ORDER BY ord, id`)
}

// ProposalsOfAsk are ask's proposals not dropped, by number.
func (s *Store) ProposalsOfAsk(ctx context.Context, ask int64) ([]ProposalRow, error) {
	return queryRows(ctx, s.db, scanProposal, `SELECT `+proposalColumns+` FROM proposals WHERE ask_id = ? AND state != 'dropped' ORDER BY id`, ask)
}
