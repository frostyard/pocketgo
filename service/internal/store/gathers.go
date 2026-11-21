package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
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
// note and the agents asked. Its time ends team.GatherLimit after it is
// recorded, by the store's clock (insertGather).
type GatherStart struct {
	Key               string
	SupervisorSession int64
	Skill, SkillText  string
	Note, Schedule    string
	Agents            []string
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
	// The agents' time starts as the gather is recorded, whatever its
	// start waited for before (Codex review of #57, round 4).
	deadline := now + team.GatherLimit.Milliseconds()
	result, err := tx.ExecContext(ctx, `INSERT INTO gathers (key, owner, supervisor_session, skill, skill_text, note, schedule, state, deadline, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, g.Key, owner, g.SupervisorSession, g.Skill, g.SkillText, g.Note, g.Schedule, team.GatherRunning, deadline, now)
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
// newest first, as a list shows them (gatherSummaries).
func (s *Store) GathersSince(ctx context.Context, since time.Time) ([]GatherFacts, error) {
	return s.gatherSummaries(ctx, `SELECT id FROM gathers WHERE state = 'running' UNION SELECT id FROM gathers WHERE ended_at >= ?`, toMillis(since))
}

// GatherSummary is gather id as its event shows it (gatherSummaries): a
// change of one proposal publishes it without reading the others' text
// (Codex review of #57, round 5).
func (s *Store) GatherSummary(ctx context.Context, id int64) (GatherFacts, error) {
	list, err := s.gatherSummaries(ctx, `SELECT ?`, id)
	if err != nil {
		return GatherFacts{}, err
	}
	if len(list) == 0 {
		return GatherFacts{}, ErrNotFound
	}
	return list[0], nil
}

// gatherSummaries are the gathers whose ids listed selects, the newest
// first, read in one transaction with three queries whatever their number:
// each with its asks, and its proposals not dropped by number and ask
// alone; no skill's text and no proposal's text is read (Codex review of
// #57, round 4).
func (s *Store) gatherSummaries(ctx context.Context, listed string, args ...any) ([]GatherFacts, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	summary := strings.Replace(gatherColumns, "skill_text", "''", 1)
	gathers, err := queryRows(ctx, tx, scanGather, `SELECT `+summary+` FROM gathers WHERE id IN (`+listed+`) ORDER BY id DESC`, args...)
	if err != nil {
		return nil, err
	}
	asks, err := queryRows(ctx, tx, scanGatherAsk, `SELECT `+gatherAskColumns+` FROM gather_asks WHERE gather_id IN (`+listed+`) ORDER BY id`, args...)
	if err != nil {
		return nil, err
	}
	proposals, err := queryRows(ctx, tx, func(scan func(...any) error) (ProposalRow, error) {
		var p ProposalRow
		return p, scan(&p.ID, &p.GatherID, &p.AskID)
	}, `SELECT id, gather_id, ask_id FROM proposals WHERE state != 'dropped' AND gather_id IN (`+listed+`) ORDER BY ord, id`, args...)
	if err != nil {
		return nil, err
	}
	out := make([]GatherFacts, len(gathers))
	at := map[int64]int{}
	for i, g := range gathers {
		out[i].Gather, at[g.ID] = g, i
	}
	for _, a := range asks {
		if i, ok := at[a.GatherID]; ok {
			out[i].Asks = append(out[i].Asks, a)
		}
	}
	for _, p := range proposals {
		if i, ok := at[p.GatherID]; ok {
			out[i].Proposals = append(out[i].Proposals, p)
		}
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

// PrepareAskBrief keeps brief as ask id's brief while the ask starts,
// before its session is made: a session made before a stop gets the brief
// its start prepared, whatever its profile says by then (Codex review of
// #57, round 2).
func (s *Store) PrepareAskBrief(ctx context.Context, id int64, brief string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE gather_asks SET brief = ? WHERE id = ? AND state = ?`, brief, id, team.AskStarting)
	return err
}

// AskBrief is the brief PrepareAskBrief kept for ask id, "" for none.
func (s *Store) AskBrief(ctx context.Context, id int64) (string, error) {
	var brief string
	err := s.db.QueryRowContext(ctx, `SELECT brief FROM gather_asks WHERE id = ?`, id).Scan(&brief)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return brief, err
}

// GatherStartedAt is when gather id started.
func (s *Store) GatherStartedAt(ctx context.Context, id int64) (time.Time, error) {
	var at int64
	err := s.db.QueryRowContext(ctx, `SELECT created_at FROM gathers WHERE id = ?`, id).Scan(&at)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, ErrNotFound
	}
	return fromMillis(at), err
}

// ProposalCall is the answer a board change of call key gave, recorded
// with it (proposal_calls), or ErrNotFound.
func (s *Store) ProposalCall(ctx context.Context, key string) (string, error) {
	var result string
	err := s.db.QueryRowContext(ctx, `SELECT result FROM proposal_calls WHERE key = ?`, key).Scan(&result)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return result, err
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
// still starts or runs; its brief, if not sent, is dropped. An end that is
// timed (its agent's done, its run's end, its deadline's pass) is decided
// in the transaction by the store's clock: past the gather's deadline the
// ask ends as its time's end does, done with a proposal, else failed, with
// team.GatherTimeWhy (Codex review of #57, round 3). When it was the
// gather's last open ask, the gather is done in the same transaction, and
// its supervisor gets the note done makes from the gather's facts, once.
// did says the ask ended; ended that the gather did too.
func (s *Store) EndGatherAsk(ctx context.Context, id int64, state, why string, timed bool, done func(GatherFacts) string) (facts GatherFacts, did, ended bool, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return GatherFacts{}, false, false, err
	}
	defer func() { _ = tx.Rollback() }()
	now := s.millis()
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
	if timed {
		var deadline int64
		var proposals int
		if err := tx.QueryRowContext(ctx, `SELECT deadline, (SELECT COUNT(*) FROM proposals WHERE ask_id = ? AND state != 'dropped') FROM gathers WHERE id = ?`,
			id, ask.GatherID).Scan(&deadline, &proposals); err != nil {
			return GatherFacts{}, false, false, err
		}
		if now >= deadline {
			state, why = team.AskFailed, team.GatherTimeWhy
			if proposals > 0 {
				state = team.AskDone
			}
		}
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

// AddProposal files proposal p of ask askID, once per key, and keeps the
// answer answer gives with the key (proposal_calls): the same key again
// answers as it did, and replayed, whatever changed the row since (Codex
// review of #57, round 4). The ask must run and its
// gather's time not have passed, by the store's clock read once its
// transaction holds the database, so a write that waited for another past
// the deadline is refused (Codex review of #57, round 2); p's agent,
// gather, kind, state and order are the store's.
func (s *Store) AddProposal(ctx context.Context, key string, askID int64, p team.Proposal, by string, answer func(ProposalRow) string) (ProposalRow, string, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ProposalRow{}, "", false, err
	}
	defer func() { _ = tx.Rollback() }()
	now := s.now()
	var receipt string
	if err := tx.QueryRowContext(ctx, `SELECT result FROM proposal_calls WHERE key = ?`, key).Scan(&receipt); err == nil {
		return ProposalRow{}, receipt, true, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return ProposalRow{}, "", false, err
	}
	ask, err := scanGatherAsk(tx.QueryRowContext(ctx, `SELECT `+gatherAskColumns+` FROM gather_asks WHERE id = ?`, askID).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return ProposalRow{}, "", false, ErrNotFound
	} else if err != nil {
		return ProposalRow{}, "", false, err
	}
	g, err := scanGather(tx.QueryRowContext(ctx, `SELECT `+gatherColumns+` FROM gathers WHERE id = ?`, ask.GatherID).Scan)
	if err != nil {
		return ProposalRow{}, "", false, err
	}
	if ask.State != team.AskRunning || !now.Before(g.Deadline) {
		return ProposalRow{}, "", false, fmt.Errorf("%w: G%d takes no proposal of yours now: you said you were done, or its %d minutes passed", ErrConflict, g.ID, int(team.GatherLimit/time.Minute))
	}
	at := now.UnixMilli()
	result, err := tx.ExecContext(ctx, `INSERT INTO proposals (key, gather_id, ask_id, agent, repository, title, task, stop_when, max_rounds, kind, note, ord, state, by, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, ?, ?, ?, ?)`, key, g.ID, ask.ID, ask.Agent, p.Repository, p.Title, p.Task, p.StopWhen, p.MaxRounds, g.Skill, p.Note,
		team.ProposalProposed, by, at, at)
	if err != nil {
		return ProposalRow{}, "", false, err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return ProposalRow{}, "", false, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE proposals SET ord = ? WHERE id = ?`, id*10, id); err != nil {
		return ProposalRow{}, "", false, err
	}
	made, err := scanProposal(tx.QueryRowContext(ctx, `SELECT `+proposalColumns+` FROM proposals WHERE id = ?`, id).Scan)
	if err != nil {
		return ProposalRow{}, "", false, err
	}
	receipt = answer(made)
	if _, err := tx.ExecContext(ctx, `INSERT INTO proposal_calls (key, result, created_at) VALUES (?, ?, ?)`, key, receipt, at); err != nil {
		return ProposalRow{}, "", false, err
	}
	return made, receipt, false, tx.Commit()
}

// ProposalChange is one change of a proposal: the row as it is, checked
// by change, which gives it changed or refuses it; answer is the text the
// call answers, kept with its key. Ask, when not 0, is the ask of the
// agent that changes it: the proposal must be that ask's, the ask must
// run, and its gather's time not have passed by the store's clock in the
// change's transaction. Allowed, when set, checks the caller may change
// the row at all, before anything else of it is told (Codex review of
// #57, round 4).
type ProposalChange struct {
	Key     string
	ID      int64
	Rev     int
	Ask     int64
	Allowed func(ProposalRow) error
	Change  func(ProposalRow) (ProposalRow, error)
	Answer  func(ProposalRow) string
}

// ChangeProposal changes proposal c.ID once per key, in one transaction:
// the same key again answers as it did, and replayed. The proposal must be
// proposed, at rev c.Rev (a stale rev is ErrConflict); the change moves its
// rev on. An agent's change is decided in the same transaction as the
// ask's state and its gather's time: past it, the agent's part has ended,
// whether or not a pass has ended its ask yet (Codex review of #57, round
// 1).
func (s *Store) ChangeProposal(ctx context.Context, c ProposalChange) (ProposalRow, string, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ProposalRow{}, "", false, err
	}
	defer func() { _ = tx.Rollback() }()
	now := s.millis()
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
	if c.Allowed != nil {
		if err := c.Allowed(row); err != nil {
			return ProposalRow{}, "", false, err
		}
	}
	if c.Ask != 0 {
		if row.AskID != c.Ask {
			return ProposalRow{}, "", false, fmt.Errorf("#%d is not yours: an agent changes only its own proposals", row.ID)
		}
		var state string
		var deadline int64
		if err := tx.QueryRowContext(ctx, `SELECT a.state, g.deadline FROM gather_asks a JOIN gathers g ON g.id = a.gather_id WHERE a.id = ?`, c.Ask).Scan(&state, &deadline); err != nil {
			return ProposalRow{}, "", false, err
		}
		if state != team.AskRunning || now >= deadline {
			return ProposalRow{}, "", false, fmt.Errorf("%w: your part in G%d has ended: the supervisor changes its proposals now", ErrConflict, row.GatherID)
		}
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
		if err := check(row); err != nil {
			return nil, "", false, err
		}
		if row.State != team.ProposalProposed {
			return nil, "", false, fmt.Errorf("%w: #%d is %s: only proposed ones are ordered", ErrConflict, row.ID, row.State)
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
		rows[i].Order, rows[i].UpdatedAt = order, fromMillis(now)
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

// BoardRow is a proposal as a board lists it, with when its gather
// started.
type BoardRow struct {
	ProposalRow
	GatheredAt time.Time
}

// boardColumns are proposalColumns of p, with its gather g's start; a
// summary leaves the task, the stop and the note out.
func boardColumns(summary bool) string {
	text := `p.task, p.stop_when, p.note`
	if summary {
		text = `'', '', ''`
	}
	return `p.id, p.key, p.gather_id, p.ask_id, p.agent, p.repository, p.title, ` + text + `, p.max_rounds, p.kind, p.ord, p.rev, p.state, p.by, p.created_at, p.updated_at, g.created_at`
}

func scanBoard(scan func(...any) error) (BoardRow, error) {
	var row BoardRow
	var created, updated, gathered int64
	if err := scan(&row.ID, &row.Key, &row.GatherID, &row.AskID, &row.Agent, &row.Repository, &row.Title, &row.Task, &row.StopWhen, &row.Note, &row.MaxRounds,
		&row.Kind, &row.Order, &row.Rev, &row.State, &row.By, &created, &updated, &gathered); err != nil {
		return BoardRow{}, err
	}
	row.CreatedAt, row.UpdatedAt, row.GatheredAt = fromMillis(created), fromMillis(updated), fromMillis(gathered)
	return row, nil
}

// Board are the proposals not dropped, whole, in the board's order, each
// with its gather's start, read with one query: GET /v1/proposals.
func (s *Store) Board(ctx context.Context) ([]BoardRow, error) {
	return queryRows(ctx, s.db, scanBoard, `SELECT `+boardColumns(false)+` FROM proposals p JOIN gathers g ON g.id = p.gather_id
		WHERE p.state != 'dropped' ORDER BY p.ord, p.id`)
}

// BoardOf is a supervisor's board, team_board's list: the proposals not
// dropped of the gathers profile's sessions asked for, in the board's
// order, each with its gather's start, without its task, stop and note.
// The supervisor is chosen in the query, so another's board and the
// proposals' text are never read (Codex review of #57, round 1).
func (s *Store) BoardOf(ctx context.Context, profile string) ([]BoardRow, error) {
	return queryRows(ctx, s.db, scanBoard, `SELECT `+boardColumns(true)+` FROM proposals p JOIN gathers g ON g.id = p.gather_id
		JOIN sessions s ON s.id = g.supervisor_session WHERE p.state != 'dropped' AND s.profile = ? ORDER BY p.ord, p.id`, profile)
}

// ProposalBoard is whose board proposal id is on: the profile of the
// session its gather's supervisor asked from ("" for a session not
// recorded), with when its gather started.
func (s *Store) ProposalBoard(ctx context.Context, id int64) (profile string, gathered time.Time, err error) {
	var at int64
	err = s.db.QueryRowContext(ctx, `SELECT COALESCE(s.profile, ''), g.created_at FROM proposals p JOIN gathers g ON g.id = p.gather_id
		LEFT JOIN sessions s ON s.id = g.supervisor_session WHERE p.id = ?`, id).Scan(&profile, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return "", time.Time{}, ErrNotFound
	} else if err != nil {
		return "", time.Time{}, err
	}
	return profile, fromMillis(at), nil
}

// ProposalsOfAsk are ask's proposals not dropped, by number.
func (s *Store) ProposalsOfAsk(ctx context.Context, ask int64) ([]ProposalRow, error) {
	return queryRows(ctx, s.db, scanProposal, `SELECT `+proposalColumns+` FROM proposals WHERE ask_id = ? AND state != 'dropped' ORDER BY id`, ask)
}
