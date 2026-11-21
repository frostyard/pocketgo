package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base32"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/frostyard/pocketgo/service/internal/team"
)

// Graph states (ADR-0020): at most one draft and one approved graph per
// plan; a new draft supersedes the old one.
const (
	GraphDraft      = "draft"
	GraphApproved   = "approved"
	GraphSuperseded = "superseded"
)

// PlanRow is a plan: its plan thread, its lead, its text and its state.
type PlanRow struct {
	ID int64
	// Key is the call that started it, as its thread's.
	Key               string
	ThreadID          int64
	SupervisorSession int64
	Lead              string
	Title             string
	State             string
	Text              string
	// Rev is Text's revision (team.Rev), "" before the first write.
	Rev        string
	ApprovedBy string
	ApprovedAt *time.Time
	// Held says the desk's cap refused a ready task of the plan since a
	// close last told its supervisor of one (HoldPlan).
	Held      bool
	CreatedAt time.Time
	UpdatedAt time.Time
}

const planColumns = `id, key, thread_id, supervisor_session, lead, title, state, text, rev, approved_by, approved_at, held, created_at, updated_at`

func scanPlan(scan func(...any) error) (PlanRow, error) {
	var row PlanRow
	var approved sql.NullInt64
	var created, updated int64
	if err := scan(&row.ID, &row.Key, &row.ThreadID, &row.SupervisorSession, &row.Lead, &row.Title, &row.State, &row.Text, &row.Rev,
		&row.ApprovedBy, &approved, &row.Held, &created, &updated); err != nil {
		return PlanRow{}, err
	}
	if approved.Valid {
		at := fromMillis(approved.Int64)
		row.ApprovedAt = &at
	}
	row.CreatedAt, row.UpdatedAt = fromMillis(created), fromMillis(updated)
	return row, nil
}

// GraphRow is a plan's task graph, with its tasks in the order drafted.
type GraphRow struct {
	ID        int64
	PlanID    int64
	Version   int
	Rev       string
	State     string
	CreatedAt time.Time
	Tasks     []team.TaskFacts
}

// ConsultRow is a consult of a plan: an expert's session, read-only, and
// its one finding.
type ConsultRow struct {
	ID int64
	// Key is the consult call's key and the agent's place in it; Owner its
	// random identity, which keys its session and its notes.
	Key      string
	Owner    string
	PlanID   int64
	Agent    string
	Question string
	AskID    int64
	State    string
	Session  int64
	Stance   string
	Summary  string
	// Why says how a consult ended without a finding.
	Why       string
	CreatedAt time.Time
	EndedAt   *time.Time
}

const consultColumns = `id, key, owner, plan_id, agent, question, ask_id, state, session, stance, summary, why, created_at, ended_at`

func scanConsult(scan func(...any) error) (ConsultRow, error) {
	var row ConsultRow
	var created int64
	var ended sql.NullInt64
	if err := scan(&row.ID, &row.Key, &row.Owner, &row.PlanID, &row.Agent, &row.Question, &row.AskID, &row.State, &row.Session,
		&row.Stance, &row.Summary, &row.Why, &created, &ended); err != nil {
		return ConsultRow{}, err
	}
	row.CreatedAt = fromMillis(created)
	if ended.Valid {
		at := fromMillis(ended.Int64)
		row.EndedAt = &at
	}
	return row, nil
}

// Deadline is when a consult with no finding fails (team.ConsultLimit).
func (c ConsultRow) Deadline() time.Time { return c.CreatedAt.Add(team.ConsultLimit) }

// PlanFacts is a plan with what its gate, its tools and its view read, as
// one read saw them.
type PlanFacts struct {
	Plan PlanRow
	// Thread is the plan thread.
	Thread ThreadRow
	// Graph is the newest graph, nil before one.
	Graph *GraphRow
	// Asks are the plan thread's asks, oldest first.
	Asks     []AskRow
	Consults []ConsultRow
}

// Gate is what the plan waits for (team.PlanGate).
func (f PlanFacts) Gate() []string {
	asks := make([]team.GateAsk, 0, len(f.Asks))
	for _, a := range f.Asks {
		asks = append(asks, team.GateAsk{ID: a.ID, Open: a.State == AskOpen, Written: a.Written, Withdrawn: a.State == AskWithdrawn})
	}
	consults := make([]team.GateConsult, len(f.Consults))
	for i, c := range f.Consults {
		consults[i] = team.GateConsult{ID: c.ID, State: c.State}
	}
	return team.PlanGate(f.Plan.State, asks, consults)
}

// DraftRev is the revision the plan's draft graph was drafted for, ""
// without a draft.
func (f PlanFacts) DraftRev() string {
	if f.Graph == nil || f.Graph.State != GraphDraft {
		return ""
	}
	return f.Graph.Rev
}

// Task is the task id of the approved graph, nil when it has none.
func (f PlanFacts) Task(id string) *team.TaskFacts {
	if f.Graph == nil || f.Graph.State != GraphApproved {
		return nil
	}
	for i := range f.Graph.Tasks {
		if f.Graph.Tasks[i].ID == id {
			return &f.Graph.Tasks[i]
		}
	}
	return nil
}

// Ask is the plan thread's ask id, nil when it has none.
func (f PlanFacts) Ask(id int64) *AskRow {
	for i := range f.Asks {
		if f.Asks[i].ID == id {
			return &f.Asks[i]
		}
	}
	return nil
}

// queryer is a *sql.DB or a *sql.Tx.
type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func loadPlanFacts(ctx context.Context, q queryer, id int64) (PlanFacts, error) {
	plan, err := scanPlan(q.QueryRowContext(ctx, `SELECT `+planColumns+` FROM plans WHERE id = ?`, id).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return PlanFacts{}, ErrNotFound
	} else if err != nil {
		return PlanFacts{}, err
	}
	facts := PlanFacts{Plan: plan}
	if facts.Thread, err = scanThread(q.QueryRowContext(ctx, `SELECT `+threadColumns+` FROM threads WHERE id = ?`, plan.ThreadID).Scan); err != nil {
		return PlanFacts{}, err
	}
	if facts.Graph, err = newestGraph(ctx, q, id); err != nil {
		return PlanFacts{}, err
	}
	if facts.Asks, err = queryRows(ctx, q, scanAsk, `SELECT `+askColumns+` FROM asks WHERE thread_id = ? ORDER BY id`, plan.ThreadID); err != nil {
		return PlanFacts{}, err
	}
	if facts.Consults, err = queryRows(ctx, q, scanConsult, `SELECT `+consultColumns+` FROM consults WHERE plan_id = ? ORDER BY id`, id); err != nil {
		return PlanFacts{}, err
	}
	return facts, nil
}

// queryRows scans every row query returns.
func queryRows[T any](ctx context.Context, q queryer, scan func(func(...any) error) (T, error), query string, args ...any) ([]T, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []T
	for rows.Next() {
		row, err := scan(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// newestGraph is plan's newest graph with its tasks, nil before one.
func newestGraph(ctx context.Context, q queryer, plan int64) (*GraphRow, error) {
	var g GraphRow
	var created int64
	err := q.QueryRowContext(ctx, `SELECT id, plan_id, version, rev, state, created_at FROM plan_graphs WHERE plan_id = ? ORDER BY version DESC LIMIT 1`, plan).
		Scan(&g.ID, &g.PlanID, &g.Version, &g.Rev, &g.State, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	g.CreatedAt = fromMillis(created)
	if g.Tasks, err = graphTasks(ctx, q, g.ID); err != nil {
		return nil, err
	}
	return &g, nil
}

func graphTasks(ctx context.Context, q queryer, graph int64) ([]team.TaskFacts, error) {
	return queryRows(ctx, q, func(scan func(...any) error) (team.TaskFacts, error) {
		var t team.TaskFacts
		var acceptance, deps string
		if err := scan(&t.ID, &t.Title, &t.Agent, &t.Repository, &acceptance, &deps, &t.State, &t.Thread); err != nil {
			return team.TaskFacts{}, err
		}
		if err := json.Unmarshal([]byte(acceptance), &t.Acceptance); err != nil {
			return team.TaskFacts{}, err
		}
		return t, json.Unmarshal([]byte(deps), &t.Deps)
	}, `SELECT task, title, agent, repository, acceptance, deps, state, thread_id FROM plan_tasks WHERE graph_id = ? ORDER BY position`, graph)
}

// Plan is plan id.
func (s *Store) Plan(ctx context.Context, id int64) (PlanRow, error) {
	row, err := scanPlan(s.db.QueryRowContext(ctx, `SELECT `+planColumns+` FROM plans WHERE id = ?`, id).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return PlanRow{}, ErrNotFound
	}
	return row, err
}

// PlanFacts is plan id with its thread, its newest graph, its asks and
// its consults, read in one transaction.
func (s *Store) PlanFacts(ctx context.Context, id int64) (PlanFacts, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return PlanFacts{}, err
	}
	defer func() { _ = tx.Rollback() }()
	return loadPlanFacts(ctx, tx, id)
}

// Plans are every plan with its facts, the most recently changed first,
// read in one transaction.
func (s *Store) Plans(ctx context.Context) ([]PlanFacts, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	ids, err := queryRows(ctx, tx, func(scan func(...any) error) (int64, error) {
		var id int64
		return id, scan(&id)
	}, `SELECT id FROM plans ORDER BY updated_at DESC, id DESC`)
	if err != nil {
		return nil, err
	}
	out := make([]PlanFacts, 0, len(ids))
	for _, id := range ids {
		facts, err := loadPlanFacts(ctx, tx, id)
		if err != nil {
			return nil, err
		}
		out = append(out, facts)
	}
	return out, nil
}

// openPlans counts the plans not done or dropped (plans_open).
func openPlans(ctx context.Context, q queryer) (int, error) {
	var n int
	err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM plans WHERE state NOT IN ('done', 'dropped')`).Scan(&n)
	return n, err
}

// newOwner is 80 random bits, as 16 lowercase base32 characters: a
// thread's or a consult's identity, unique across data folders.
func newOwner() (string, error) {
	var bits [10]byte
	if _, err := rand.Read(bits[:]); err != nil {
		return "", err
	}
	return strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(bits[:])), nil
}

// insertThread records a starting thread with a new random owner, unless
// its key made one already: then it returns that one, and existed.
func insertThread(ctx context.Context, tx *sql.Tx, row ThreadRow, now int64) (ThreadRow, bool, error) {
	owner, err := newOwner()
	if err != nil {
		return ThreadRow{}, false, err
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO threads (key, title, agent, supervisor_session, repository, cwd, branch, task, state, owner, task_id,
		review_pull, review_head, review_author_provider, review_author_model, worker_provider, worker_model, plan, plan_task, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT (key) DO NOTHING`,
		row.Key, row.Title, row.Agent, row.SupervisorSession, row.Repository, row.Cwd, row.Branch, row.Task, ThreadStarting, owner, row.TaskID,
		row.ReviewPull, row.ReviewHead, row.ReviewAuthor.Provider, row.ReviewAuthor.ID, row.WorkerAsked.Provider, row.WorkerAsked.ID, row.Plan,
		row.PlanTask, now, now)
	if err != nil {
		return ThreadRow{}, false, err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return ThreadRow{}, false, err
	}
	got, err := scanThread(tx.QueryRowContext(ctx, `SELECT `+threadColumns+` FROM threads WHERE key = ?`, row.Key).Scan)
	return got, n == 0, err
}

// CreatePlanThread records a new plan and its plan thread, starting, in
// one transaction, unless the thread's key made them already: then it
// returns those, and existed. check gets the plans open before this one,
// in the transaction, and refuses the start (team.CheckPlanStart): then
// nothing is recorded.
func (s *Store) CreatePlanThread(ctx context.Context, row ThreadRow, check func(open int) error) (ThreadRow, PlanRow, bool, error) {
	now := s.millis()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ThreadRow{}, PlanRow{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	if existing, err := scanThread(tx.QueryRowContext(ctx, `SELECT `+threadColumns+` FROM threads WHERE key = ?`, row.Key).Scan); err == nil {
		plan, err := scanPlan(tx.QueryRowContext(ctx, `SELECT `+planColumns+` FROM plans WHERE thread_id = ?`, existing.ID).Scan)
		if errors.Is(err, sql.ErrNoRows) {
			return ThreadRow{}, PlanRow{}, false, ErrConflict // the key made a thread that is no plan's
		}
		return existing, plan, true, err
	} else if !errors.Is(err, sql.ErrNoRows) {
		return ThreadRow{}, PlanRow{}, false, err
	}
	open, err := openPlans(ctx, tx)
	if err != nil {
		return ThreadRow{}, PlanRow{}, false, err
	}
	if err := check(open); err != nil {
		return ThreadRow{}, PlanRow{}, false, err
	}
	row.Plan, row.PlanTask = 0, ""
	thread, _, err := insertThread(ctx, tx, row, now)
	if err != nil {
		return ThreadRow{}, PlanRow{}, false, err
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO plans (key, thread_id, supervisor_session, lead, title, state, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, row.Key, thread.ID, row.SupervisorSession, row.Agent, row.Title, team.PlanDrafting, now, now)
	if err != nil {
		return ThreadRow{}, PlanRow{}, false, err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return ThreadRow{}, PlanRow{}, false, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE threads SET plan = ? WHERE id = ?`, id, thread.ID); err != nil {
		return ThreadRow{}, PlanRow{}, false, err
	}
	if thread, err = scanThread(tx.QueryRowContext(ctx, `SELECT `+threadColumns+` FROM threads WHERE id = ?`, thread.ID).Scan); err != nil {
		return ThreadRow{}, PlanRow{}, false, err
	}
	plan, err := scanPlan(tx.QueryRowContext(ctx, `SELECT `+planColumns+` FROM plans WHERE id = ?`, id).Scan)
	if err != nil {
		return ThreadRow{}, PlanRow{}, false, err
	}
	return thread, plan, false, tx.Commit()
}

// CreateTaskThread records the thread that delivers task of plan, in the
// transaction that checks the gate: check gets the plan's facts there
// (team.CheckDelivery), and refuses the delivery, which records nothing;
// build gives the thread's title and task from the same facts, so the
// brief is the approved task that passed the gate. The task is delegated
// to the thread, and the plan is building. A key that made a thread
// already returns it, and existed, checking nothing.
func (s *Store) CreateTaskThread(ctx context.Context, row ThreadRow, plan int64, task string, check func(PlanFacts) error,
	build func(PlanFacts) (title, text string)) (ThreadRow, bool, error) {
	now := s.millis()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ThreadRow{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	if existing, err := scanThread(tx.QueryRowContext(ctx, `SELECT `+threadColumns+` FROM threads WHERE key = ?`, row.Key).Scan); err == nil {
		return existing, true, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return ThreadRow{}, false, err
	}
	facts, err := loadPlanFacts(ctx, tx, plan)
	if err != nil {
		return ThreadRow{}, false, err
	}
	if err := check(facts); err != nil {
		return ThreadRow{}, false, err
	}
	if facts.Task(task) == nil {
		return ThreadRow{}, false, ErrConflict // check refuses it first
	}
	row.Title, row.Task = build(facts)
	row.Plan, row.PlanTask = plan, task
	thread, _, err := insertThread(ctx, tx, row, now)
	if err != nil {
		return ThreadRow{}, false, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE plan_tasks SET state = ?, thread_id = ? WHERE graph_id = ? AND task = ?`,
		team.TaskDelegated, thread.ID, facts.Graph.ID, task); err != nil {
		return ThreadRow{}, false, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE plans SET state = CASE WHEN state = ? THEN ? ELSE state END, updated_at = ? WHERE id = ?`,
		team.PlanApproved, team.PlanBuilding, now, plan); err != nil {
		return ThreadRow{}, false, err
	}
	return thread, false, tx.Commit()
}

// PlanCallResult is the answer team_plan call key gave, when it changed
// something already: run again, it answers so.
func (s *Store) PlanCallResult(ctx context.Context, key string) (string, bool, error) {
	var result string
	err := s.db.QueryRowContext(ctx, `SELECT result FROM plan_calls WHERE key = ?`, key).Scan(&result)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return result, err == nil, err
}

// planCall runs one team_plan call's change on plan, in one transaction,
// once per key: check gets the plan's facts there and refuses the call,
// which changes nothing; change makes it; result is the call's answer from
// the facts after, recorded with the key. A key recorded already answers
// as recorded, and changes nothing.
func (s *Store) planCall(ctx context.Context, key string, plan int64, action string, check func(PlanFacts) error,
	change func(tx *sql.Tx, facts PlanFacts, now int64) error, result func(PlanFacts) string) (PlanFacts, string, bool, error) {
	now := s.millis()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return PlanFacts{}, "", false, err
	}
	defer func() { _ = tx.Rollback() }()
	var recorded string
	err = tx.QueryRowContext(ctx, `SELECT result FROM plan_calls WHERE key = ?`, key).Scan(&recorded)
	if err == nil {
		facts, err := loadPlanFacts(ctx, tx, plan)
		return facts, recorded, true, err
	} else if !errors.Is(err, sql.ErrNoRows) {
		return PlanFacts{}, "", false, err
	}
	facts, err := loadPlanFacts(ctx, tx, plan)
	if err != nil {
		return PlanFacts{}, "", false, err
	}
	if err := check(facts); err != nil {
		return PlanFacts{}, "", false, err
	}
	if err := change(tx, facts, now); err != nil {
		return PlanFacts{}, "", false, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE plans SET updated_at = ? WHERE id = ?`, now, plan); err != nil {
		return PlanFacts{}, "", false, err
	}
	if facts, err = loadPlanFacts(ctx, tx, plan); err != nil {
		return PlanFacts{}, "", false, err
	}
	text := result(facts)
	if _, err := tx.ExecContext(ctx, `INSERT INTO plan_calls (key, plan_id, action, result, created_at) VALUES (?, ?, ?, ?, ?)`,
		key, plan, action, text, now); err != nil {
		return PlanFacts{}, "", false, err
	}
	return facts, text, false, tx.Commit()
}

// backToDrafting takes a plan in review back to drafting.
func backToDrafting(ctx context.Context, tx *sql.Tx, plan int64, now int64) error {
	_, err := tx.ExecContext(ctx, `UPDATE plans SET state = ?, updated_at = ? WHERE id = ? AND state = ?`, team.PlanDrafting, now, plan, team.PlanReview)
	return err
}

// WritePlan replaces plan's text with text, for team_plan call key, once
// (planCall): check refuses a write (team.CheckDraft, team.CheckWrite, the
// asks it names). The asks it names that are answered asks of the plan
// thread are written, and a plan in review is drafting again.
func (s *Store) WritePlan(ctx context.Context, key string, plan int64, text string, asks []int64, check func(PlanFacts) error,
	result func(PlanFacts) string) (PlanFacts, string, bool, error) {
	return s.planCall(ctx, key, plan, team.PlanWrite, check, func(tx *sql.Tx, facts PlanFacts, now int64) error {
		if _, err := tx.ExecContext(ctx, `UPDATE plans SET text = ?, rev = ? WHERE id = ?`, text, team.Rev(text), plan); err != nil {
			return err
		}
		for _, id := range asks {
			if _, err := tx.ExecContext(ctx, `UPDATE asks SET written_at = ? WHERE id = ? AND thread_id = ? AND state = ? AND written_at IS NULL`,
				now, id, facts.Plan.ThreadID, AskAnswered); err != nil {
				return err
			}
		}
		return backToDrafting(ctx, tx, plan, now)
	}, result)
}

// DraftGraph drafts tasks (checked, team.CheckGraph) as plan's graph for
// its current revision, for team_plan call key, once (planCall): the new
// version supersedes a draft, and a plan in review is drafting again.
// Every task waits until the approval.
func (s *Store) DraftGraph(ctx context.Context, key string, plan int64, tasks []team.Task, check func(PlanFacts) error,
	result func(PlanFacts) string) (PlanFacts, string, bool, error) {
	return s.planCall(ctx, key, plan, team.PlanTasks, check, func(tx *sql.Tx, facts PlanFacts, now int64) error {
		if _, err := tx.ExecContext(ctx, `UPDATE plan_graphs SET state = ? WHERE plan_id = ? AND state = ?`, GraphSuperseded, plan, GraphDraft); err != nil {
			return err
		}
		version := 1
		if facts.Graph != nil {
			version = facts.Graph.Version + 1
		}
		inserted, err := tx.ExecContext(ctx, `INSERT INTO plan_graphs (plan_id, version, rev, state, created_at) VALUES (?, ?, ?, ?, ?)`,
			plan, version, facts.Plan.Rev, GraphDraft, now)
		if err != nil {
			return err
		}
		graph, err := inserted.LastInsertId()
		if err != nil {
			return err
		}
		for i, t := range tasks {
			acceptance, err := json.Marshal(t.Acceptance)
			if err != nil {
				return err
			}
			deps := t.Deps
			if deps == nil {
				deps = []string{}
			}
			encoded, err := json.Marshal(deps)
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO plan_tasks (graph_id, position, task, title, agent, repository, acceptance, deps, state)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, graph, i, t.ID, t.Title, t.Agent, t.Repository, string(acceptance), string(encoded), team.TaskWaiting); err != nil {
				return err
			}
		}
		return backToDrafting(ctx, tx, plan, now)
	}, result)
}

// ReadyPlan moves plan to review, for team_plan call key, once (planCall),
// when check (team.CheckReady) passes in the transaction.
func (s *Store) ReadyPlan(ctx context.Context, key string, plan int64, check func(PlanFacts) error, result func(PlanFacts) string) (PlanFacts, string, bool, error) {
	return s.planCall(ctx, key, plan, team.PlanReady, check, func(tx *sql.Tx, _ PlanFacts, now int64) error {
		_, err := tx.ExecContext(ctx, `UPDATE plans SET state = ?, updated_at = ? WHERE id = ? AND state = ?`, team.PlanReview, now, plan, team.PlanDrafting)
		return err
	}, result)
}

// AddConsults records consults of plan, starting, each with a random
// owner, for team_plan call key, once (planCall), when check
// (team.CheckDraft, team.CheckConsult) passes in the transaction. Each
// row's Key, Agent, Question and AskID are kept.
func (s *Store) AddConsults(ctx context.Context, key string, plan int64, consults []ConsultRow, check func(PlanFacts) error,
	result func(PlanFacts) string) (PlanFacts, string, bool, error) {
	return s.planCall(ctx, key, plan, team.PlanConsult, check, func(tx *sql.Tx, _ PlanFacts, now int64) error {
		for _, c := range consults {
			owner, err := newOwner()
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO consults (key, owner, plan_id, agent, question, ask_id, state, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
				c.Key, owner, plan, c.Agent, c.Question, c.AskID, team.ConsultStarting, now); err != nil {
				return err
			}
		}
		return backToDrafting(ctx, tx, plan, now)
	}, result)
}

// ApprovePlan approves plan for the owner's request key, once, as AnswerAsk
// answers: the key is claimed in the transaction, so one used for another
// request is ErrRequestReused, and a retry returns the plan as it is, and
// replayed. check (team.CheckApprove) refuses an approval of a plan that
// does not wait for it as the owner saw it; its error comes back wrapped
// in ErrConflict. The draft graph is approved, its tasks with no
// dependency are ready, and the supervisor's note names them.
func (s *Store) ApprovePlan(ctx context.Context, key string, plan int64, by string, check func(PlanFacts) error) (PlanFacts, bool, error) {
	now := s.millis()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return PlanFacts{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	_, existed, err := claimRequest(ctx, tx, key, "approvePlan:"+strconv.FormatInt(plan, 10), now)
	if err != nil {
		return PlanFacts{}, false, err
	}
	facts, err := loadPlanFacts(ctx, tx, plan)
	if err != nil {
		return PlanFacts{}, false, err
	}
	if existed {
		return facts, true, nil
	}
	if err := check(facts); err != nil {
		return PlanFacts{}, false, conflict(err)
	}
	if facts.Graph == nil || facts.Graph.State != GraphDraft {
		return PlanFacts{}, false, conflict(errors.New("the plan has no draft graph"))
	}
	if _, err := tx.ExecContext(ctx, `UPDATE plans SET state = ?, approved_by = ?, approved_at = ?, updated_at = ? WHERE id = ?`,
		team.PlanApproved, by, now, now, plan); err != nil {
		return PlanFacts{}, false, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE plan_graphs SET state = ? WHERE id = ?`, GraphApproved, facts.Graph.ID); err != nil {
		return PlanFacts{}, false, err
	}
	ready := team.NowReady(facts.Graph.Tasks)
	if err := setTasks(ctx, tx, facts.Graph.ID, ready, team.TaskReady); err != nil {
		return PlanFacts{}, false, err
	}
	note := NoteRow{Key: "plan:" + facts.Thread.Owner + ":approved", SessionID: facts.Plan.SupervisorSession,
		Text: team.ApprovedNote(plan, facts.Plan.Title, facts.Plan.Rev, facts.Graph.Version, team.TasksNamed(facts.Graph.Tasks, ready))}
	if err := insertNote(ctx, tx, note, now); err != nil {
		return PlanFacts{}, false, err
	}
	if facts, err = loadPlanFacts(ctx, tx, plan); err != nil {
		return PlanFacts{}, false, err
	}
	return facts, false, tx.Commit()
}

func setTasks(ctx context.Context, tx *sql.Tx, graph int64, tasks []string, state string) error {
	for _, id := range tasks {
		if _, err := tx.ExecContext(ctx, `UPDATE plan_tasks SET state = ? WHERE graph_id = ? AND task = ?`, state, graph, id); err != nil {
			return err
		}
	}
	return nil
}

// threadClosed settles the plan of a thread that a change closed, in that
// change's transaction (ADR-0020). The plan thread's close drops a plan
// not done, and stops its consults. A task thread's close makes its task
// done when its work is (team.TaskWorkDone): the tasks it frees are
// ready, and the supervisor is told; the last one makes the plan done and
// closes its thread. A task thread closed otherwise makes the task ready
// again, and the supervisor is told, unless it closed while it started:
// the delegating call says so. An older delivery of the task, or a plan
// ended, changes nothing.
//
// The close of any thread on the desk (a plan's own thread is none) frees
// a place: a plan whose ready task the cap refused (HoldPlan) tells its
// supervisor so, once (freedPlace).
func threadClosed(ctx context.Context, tx *sql.Tx, before, after ThreadRow, now int64) error {
	if before.State == ThreadClosed || after.State != ThreadClosed {
		return nil
	}
	// A thread's close ends its shift, whatever closed it (ADR-0024).
	if err := shiftClosed(ctx, tx, before, now); err != nil {
		return err
	}
	if before.Plan != 0 {
		if err := planThreadClosed(ctx, tx, before, now); err != nil {
			return err
		}
	}
	if before.IsPlanThread() {
		return nil
	}
	return freedPlace(ctx, tx, before, now)
}

// planThreadClosed is threadClosed's work on the plan of the thread that
// closed.
func planThreadClosed(ctx context.Context, tx *sql.Tx, before ThreadRow, now int64) error {
	facts, err := loadPlanFacts(ctx, tx, before.Plan)
	if err != nil {
		return err
	}
	if team.PlanEnded(facts.Plan.State) {
		return nil
	}
	plan := facts.Plan.ID
	if before.PlanTask == "" {
		// A dropped plan holds no place on the desk's wait (plans_held).
		if _, err := tx.ExecContext(ctx, `UPDATE plans SET state = ?, held = 0, updated_at = ? WHERE id = ?`, team.PlanDropped, now, plan); err != nil {
			return err
		}
		return stopConsults(ctx, tx, plan, "its plan was dropped", now)
	}
	task := facts.Task(before.PlanTask)
	if task == nil || task.State != team.TaskDelegated || task.Thread != before.ID {
		return nil
	}
	pulls, err := pullsIn(ctx, tx, before.ID)
	if err != nil {
		return err
	}
	states := make([]string, len(pulls))
	for i, p := range pulls {
		states[i] = p.State
	}
	// This close frees a place on the desk: when the cap held back a
	// ready task of this plan, its note names every ready task, else the
	// ones it makes ready (Codex review of #25, round 3).
	if _, err := tx.ExecContext(ctx, `UPDATE plans SET updated_at = ? WHERE id = ?`, now, plan); err != nil {
		return err
	}
	prefix := "plan:" + facts.Thread.Owner
	// While an open ask holds the gate, the gate refuses every delivery:
	// the note waits for the answer that clears it (askCleared), and the
	// hold stays (Codex review of #25, round 5).
	gateClosed := len(facts.Gate()) > 0
	if !team.TaskWorkDone(before.State, states) {
		if err := setTasks(ctx, tx, facts.Graph.ID, []string{task.ID}, team.TaskReady); err != nil {
			return err
		}
		// Told whether or not the thread had started: a supervisor told
		// the task was delivered learns it was not (Codex review of #25,
		// round 5).
		if gateClosed {
			return nil
		}
		if _, err := tx.ExecContext(ctx, `UPDATE plans SET held = 0 WHERE id = ?`, plan); err != nil {
			return err
		}
		again := []string{task.ID}
		if facts.Plan.Held {
			again = nil
			for _, t := range facts.Graph.Tasks {
				if t.State == team.TaskReady || t.ID == task.ID {
					again = append(again, t.ID)
				}
			}
		}
		return insertNote(ctx, tx, NoteRow{Key: prefix + ":again:" + before.Owner, SessionID: facts.Plan.SupervisorSession,
			Text: team.AgainNote(plan, task.Task, before.ID, team.TasksNamed(facts.Graph.Tasks, again))}, now)
	}
	task.State = team.TaskDone
	freed := team.NowReady(facts.Graph.Tasks)
	if err := setTasks(ctx, tx, facts.Graph.ID, []string{task.ID}, team.TaskDone); err != nil {
		return err
	}
	if err := setTasks(ctx, tx, facts.Graph.ID, freed, team.TaskReady); err != nil {
		return err
	}
	ready := freed
	if facts.Plan.Held {
		ready = nil
		for i, t := range facts.Graph.Tasks {
			if slices.Contains(freed, t.ID) {
				facts.Graph.Tasks[i].State = team.TaskReady
			}
			if facts.Graph.Tasks[i].State == team.TaskReady {
				ready = append(ready, t.ID)
			}
		}
	}
	for _, t := range facts.Graph.Tasks {
		if t.State != team.TaskDone {
			if len(ready) == 0 || gateClosed {
				return nil
			}
			if _, err := tx.ExecContext(ctx, `UPDATE plans SET held = 0 WHERE id = ?`, plan); err != nil {
				return err
			}
			return insertNote(ctx, tx, NoteRow{Key: prefix + ":ready:" + before.Owner, SessionID: facts.Plan.SupervisorSession,
				Text: team.ReadyNote(plan, task.ID, before.ID, team.TasksNamed(facts.Graph.Tasks, ready))}, now)
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE plans SET state = ?, updated_at = ? WHERE id = ?`, team.PlanDone, now, plan); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE threads SET state = ?, report = ?, updated_at = ? WHERE id = ? AND state != ?`,
		ThreadClosed, team.PlanClosedReport(plan), now, facts.Plan.ThreadID, ThreadClosed); err != nil {
		return err
	}
	return insertNote(ctx, tx, NoteRow{Key: prefix + ":done", SessionID: facts.Plan.SupervisorSession,
		Text: team.DoneNote(plan, facts.Plan.Title, facts.Plan.ThreadID)}, now)
}

// HoldPlan notes that the desk's cap refused a ready task of plan: the
// next close of a thread on the desk tells its supervisor (freedPlace).
func (s *Store) HoldPlan(ctx context.Context, plan int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE plans SET held = 1 WHERE id = ? AND state IN (?, ?)`, plan, team.PlanApproved, team.PlanBuilding)
	return err
}

// HoldIfFull holds plan (HoldPlan) only while the desk is full, counted
// in the same transaction: a close that commits first frees a place the
// caller may take (false), and one that commits after finds the hold
// (Codex review of #25, round 4). check refuses a task the gate would not
// deliver anyway (team.CheckDelivery), in the transaction too: its error
// comes back, and nothing is held (round 5).
func (s *Store) HoldIfFull(ctx context.Context, plan int64, cap int, check func(PlanFacts) error) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	facts, err := loadPlanFacts(ctx, tx, plan)
	if err != nil {
		return false, err
	}
	if err := check(facts); err != nil {
		return false, err
	}
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM threads WHERE state != 'closed' AND (plan = 0 OR plan_task != '')`).Scan(&n); err != nil {
		return false, err
	}
	if n < cap {
		return false, nil
	}
	if _, err := tx.ExecContext(ctx, `UPDATE plans SET held = 1 WHERE id = ? AND state IN (?, ?)`, plan, team.PlanApproved, team.PlanBuilding); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// heldQuery reads the held plans through plans_held: every close on the
// desk runs it, never over the whole history of plans (Codex review of
// #25, round 4).
const heldQuery = `SELECT id FROM plans WHERE held = 1 AND state IN (?, ?) ORDER BY id`

// freedPlace tells the supervisor of each plan the cap held back
// (HoldPlan), once, that the close of thread before freed a place on the
// desk, naming its ready tasks, when its gate lets them go (Codex review
// of #25, round 3). A plan held with an open ask learns of it when the
// ask clears (askCleared).
func freedPlace(ctx context.Context, tx *sql.Tx, before ThreadRow, now int64) error {
	held, err := queryRows(ctx, tx, func(scan func(...any) error) (int64, error) {
		var id int64
		return id, scan(&id)
	}, heldQuery, team.PlanApproved, team.PlanBuilding)
	if err != nil {
		return err
	}
	for _, plan := range held {
		if _, err := tx.ExecContext(ctx, `UPDATE plans SET held = 0 WHERE id = ?`, plan); err != nil {
			return err
		}
		facts, err := loadPlanFacts(ctx, tx, plan)
		if err != nil {
			return err
		}
		if len(facts.Gate()) > 0 || facts.Graph == nil {
			continue
		}
		var ready []string
		for _, t := range facts.Graph.Tasks {
			if t.State == team.TaskReady {
				ready = append(ready, t.ID)
			}
		}
		if len(ready) == 0 {
			continue
		}
		if err := insertNote(ctx, tx, NoteRow{Key: "plan:" + facts.Thread.Owner + ":free:" + before.Owner, SessionID: facts.Plan.SupervisorSession,
			Text: team.FreeNote(plan, before.ID, team.TasksNamed(facts.Graph.Tasks, ready))}, now); err != nil {
			return err
		}
	}
	return nil
}

// askCleared tells the plan's supervisor, once, when the answer to ask id
// clears the last open ask of an approved plan that has ready tasks: a
// delivery the gate refused for that ask may go now (Codex review of #25,
// round 2). A plan in drafting or review tells nobody: its lead writes the
// answer in, and the owner approves it later. withdrawn says a
// supervisor's message withdrew the ask instead (ADR-0037).
func askCleared(ctx context.Context, tx *sql.Tx, id int64, withdrawn bool, now int64) error {
	ask, err := scanAsk(tx.QueryRowContext(ctx, `SELECT `+askColumns+` FROM asks WHERE id = ?`, id).Scan)
	if err != nil {
		return err
	}
	var plan int64
	err = tx.QueryRowContext(ctx, `SELECT id FROM plans WHERE thread_id = ?`, ask.ThreadID).Scan(&plan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	} else if err != nil {
		return err
	}
	facts, err := loadPlanFacts(ctx, tx, plan)
	if err != nil {
		return err
	}
	if (facts.Plan.State != team.PlanApproved && facts.Plan.State != team.PlanBuilding) || len(facts.Gate()) > 0 || facts.Graph == nil {
		return nil
	}
	var ready []string
	for _, t := range facts.Graph.Tasks {
		if t.State == team.TaskReady {
			ready = append(ready, t.ID)
		}
	}
	if len(ready) == 0 {
		return nil
	}
	note := team.ClearedNote
	if withdrawn {
		note = team.WithdrawnNote
	}
	return insertNote(ctx, tx, NoteRow{Key: "plan:" + facts.Thread.Owner + ":cleared:" + ask.Key, SessionID: facts.Plan.SupervisorSession,
		Text: note(plan, ask.ID, team.TasksNamed(facts.Graph.Tasks, ready))}, now)
}

// stopConsults stops plan's consults that start or run, with why, and
// drops their questions not sent yet.
func stopConsults(ctx context.Context, tx *sql.Tx, plan int64, why string, now int64) error {
	if _, err := tx.ExecContext(ctx, `UPDATE notes SET dropped_at = ? WHERE sent_at IS NULL AND dropped_at IS NULL
		AND consult_id IN (SELECT id FROM consults WHERE plan_id = ? AND state IN (?, ?))`, now, plan, team.ConsultStarting, team.ConsultRunning); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `UPDATE consults SET state = ?, why = ?, ended_at = ? WHERE plan_id = ? AND state IN (?, ?)`,
		team.ConsultStopped, why, now, plan, team.ConsultStarting, team.ConsultRunning)
	return err
}

// Consult is consult id.
func (s *Store) Consult(ctx context.Context, id int64) (ConsultRow, error) {
	row, err := scanConsult(s.db.QueryRowContext(ctx, `SELECT `+consultColumns+` FROM consults WHERE id = ?`, id).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return ConsultRow{}, ErrNotFound
	}
	return row, err
}

// ConsultOfSession is the consult whose expert works in session. The
// predicate repeats consults_session's, so SQLite reads that index.
func (s *Store) ConsultOfSession(ctx context.Context, session int64) (ConsultRow, error) {
	row, err := scanConsult(s.db.QueryRowContext(ctx, `SELECT `+consultColumns+` FROM consults WHERE session = ? AND session != 0`, session).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return ConsultRow{}, ErrNotFound
	}
	return row, err
}

// OpenConsults are the consults that start or run, by id (consults_open).
func (s *Store) OpenConsults(ctx context.Context) ([]ConsultRow, error) {
	return queryRows(ctx, s.db, scanConsult, `SELECT `+consultColumns+` FROM consults WHERE state IN ('starting', 'running') ORDER BY id`)
}

// RecordConsultSession records a consult's session (as CreateSession),
// gives it to consult id and files its question, in one transaction: a
// consult that started meanwhile keeps its session; one that ended gets
// the session (it exists) but no question. question writes the question
// from the consult's plan and its ask (nil for none) as that transaction
// reads them, so the plan's text it quotes is the plan's as the consult
// starts. It reads those two rows alone, since the caller holds the
// outbox and work locks (Codex review of #31).
func (s *Store) RecordConsultSession(ctx context.Context, key string, session SessionRow, id int64, question func(PlanRow, *AskRow) string) (ConsultRow, error) {
	var consult ConsultRow
	_, err := s.createSession(ctx, key, session, func(tx *sql.Tx, now int64) error {
		if _, err := tx.ExecContext(ctx, `UPDATE consults SET session = ?, state = ? WHERE id = ? AND state = ? AND session IN (0, ?)`,
			session.ID, team.ConsultRunning, id, team.ConsultStarting, session.ID); err != nil {
			return err
		}
		var err error
		consult, err = scanConsult(tx.QueryRowContext(ctx, `SELECT `+consultColumns+` FROM consults WHERE id = ?`, id).Scan)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return err
		}
		if consult.State != team.ConsultRunning || consult.Session != session.ID {
			return nil
		}
		if _, err := tx.ExecContext(ctx, `UPDATE plans SET updated_at = ? WHERE id = ?`, now, consult.PlanID); err != nil {
			return err
		}
		plan, err := scanPlan(tx.QueryRowContext(ctx, `SELECT `+planColumns+` FROM plans WHERE id = ?`, consult.PlanID).Scan)
		if err != nil {
			return err
		}
		var ask *AskRow
		if consult.AskID != 0 {
			row, err := scanAsk(tx.QueryRowContext(ctx, `SELECT `+askColumns+` FROM asks WHERE id = ? AND thread_id = ?`, consult.AskID, plan.ThreadID).Scan)
			switch {
			case err == nil:
				ask = &row
			case !errors.Is(err, sql.ErrNoRows):
				return err
			}
		}
		return insertNote(ctx, tx, NoteRow{Key: "consult:" + consult.Owner + ":question", SessionID: session.ID, ConsultID: id, Text: question(plan, ask)}, now)
	})
	return consult, err
}

// EndConsult ends consult id without a finding, with state (failed or
// stopped) and why, when it still starts or runs; its question, if not
// sent, is dropped, and the planning session gets text, once. ended says
// whether it ended it.
func (s *Store) EndConsult(ctx context.Context, id int64, state, why, text string) (ConsultRow, bool, error) {
	now := s.millis()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ConsultRow{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	consult, err := scanConsult(tx.QueryRowContext(ctx, `SELECT `+consultColumns+` FROM consults WHERE id = ?`, id).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return ConsultRow{}, false, ErrNotFound
	} else if err != nil {
		return ConsultRow{}, false, err
	}
	if !team.ConsultOpen(consult.State) {
		return consult, false, nil
	}
	facts, err := loadPlanFacts(ctx, tx, consult.PlanID)
	if err != nil {
		return ConsultRow{}, false, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE notes SET dropped_at = ? WHERE consult_id = ? AND sent_at IS NULL AND dropped_at IS NULL`, now, id); err != nil {
		return ConsultRow{}, false, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE consults SET state = ?, why = ?, ended_at = ? WHERE id = ?`, state, why, now, id); err != nil {
		return ConsultRow{}, false, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE plans SET updated_at = ? WHERE id = ?`, now, consult.PlanID); err != nil {
		return ConsultRow{}, false, err
	}
	if facts.Thread.Session != 0 {
		if err := insertNote(ctx, tx, NoteRow{Key: "consult:" + consult.Owner + ":end", SessionID: facts.Thread.Session, Text: text}, now); err != nil {
			return ConsultRow{}, false, err
		}
	}
	consult, err = scanConsult(tx.QueryRowContext(ctx, `SELECT `+consultColumns+` FROM consults WHERE id = ?`, id).Scan)
	if err != nil {
		return ConsultRow{}, false, err
	}
	return consult, true, tx.Commit()
}

// FileFinding records the finding of the consult session works in, for
// team_finding call key, once: check refuses it (a consult that does not
// run, a plan that does not draft), and nothing is recorded. The consult
// is answered, and the planning session gets text, once; result is the
// call's answer, recorded with the key. A key recorded already answers as
// recorded.
func (s *Store) FileFinding(ctx context.Context, key string, session int64, f team.Finding, check func(ConsultRow, PlanFacts) error,
	text func(ConsultRow) string, result string) (ConsultRow, string, bool, error) {
	now := s.millis()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ConsultRow{}, "", false, err
	}
	defer func() { _ = tx.Rollback() }()
	consult, err := scanConsult(tx.QueryRowContext(ctx, `SELECT `+consultColumns+` FROM consults WHERE session = ? AND session != 0`, session).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return ConsultRow{}, "", false, ErrNotFound
	} else if err != nil {
		return ConsultRow{}, "", false, err
	}
	var recorded string
	if err := tx.QueryRowContext(ctx, `SELECT result FROM plan_calls WHERE key = ?`, key).Scan(&recorded); err == nil {
		return consult, recorded, true, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return ConsultRow{}, "", false, err
	}
	facts, err := loadPlanFacts(ctx, tx, consult.PlanID)
	if err != nil {
		return ConsultRow{}, "", false, err
	}
	if err := check(consult, facts); err != nil {
		return ConsultRow{}, "", false, err
	}
	summary := strings.TrimSpace(f.Summary)
	if _, err := tx.ExecContext(ctx, `UPDATE consults SET state = ?, stance = ?, summary = ?, ended_at = ? WHERE id = ?`,
		team.ConsultAnswered, f.Stance, summary, now, consult.ID); err != nil {
		return ConsultRow{}, "", false, err
	}
	consult.State, consult.Stance, consult.Summary = team.ConsultAnswered, f.Stance, summary
	if _, err := tx.ExecContext(ctx, `UPDATE plans SET updated_at = ? WHERE id = ?`, now, consult.PlanID); err != nil {
		return ConsultRow{}, "", false, err
	}
	if err := insertNote(ctx, tx, NoteRow{Key: "consult:" + consult.Owner + ":finding", SessionID: facts.Thread.Session, Text: text(consult)}, now); err != nil {
		return ConsultRow{}, "", false, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO plan_calls (key, plan_id, action, result, created_at) VALUES (?, ?, ?, ?, ?)`,
		key, consult.PlanID, "finding", result, now); err != nil {
		return ConsultRow{}, "", false, err
	}
	consult, err = scanConsult(tx.QueryRowContext(ctx, `SELECT `+consultColumns+` FROM consults WHERE id = ?`, consult.ID).Scan)
	if err != nil {
		return ConsultRow{}, "", false, err
	}
	return consult, result, false, tx.Commit()
}

// NoteRow is a message pocketd owes a session that is no report, answer
// or brief: a plan's note to its supervisor, a consult's question, or a
// finding (ADR-0020). Its key sends it once.
type NoteRow struct {
	ID        int64
	Key       string
	SessionID int64
	// ConsultID is the consult whose question it is, 0 for another note:
	// it is dropped once the consult ends.
	ConsultID int64
	Text      string
	// Submission and Attempt are as an AskRow's.
	Submission int64
	Attempt    int
	SentAt     *time.Time
	DroppedAt  *time.Time
}

const noteColumns = `id, key, session_id, consult_id, text, submission, attempt, sent_at, dropped_at`

func scanNote(scan func(...any) error) (NoteRow, error) {
	var row NoteRow
	var sent, dropped sql.NullInt64
	if err := scan(&row.ID, &row.Key, &row.SessionID, &row.ConsultID, &row.Text, &row.Submission, &row.Attempt, &sent, &dropped); err != nil {
		return NoteRow{}, err
	}
	if sent.Valid {
		at := fromMillis(sent.Int64)
		row.SentAt = &at
	}
	if dropped.Valid {
		at := fromMillis(dropped.Int64)
		row.DroppedAt = &at
	}
	return row, nil
}

// insertNote files a note, once per key.
func insertNote(ctx context.Context, tx *sql.Tx, n NoteRow, now int64) error {
	if n.SessionID == 0 {
		return errors.New("a note needs its session")
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO notes (key, session_id, consult_id, text, created_at) VALUES (?, ?, ?, ?, ?) ON CONFLICT (key) DO NOTHING`,
		n.Key, n.SessionID, n.ConsultID, n.Text, now)
	return err
}

// Note is note key.
func (s *Store) Note(ctx context.Context, key string) (NoteRow, error) {
	row, err := scanNote(s.db.QueryRowContext(ctx, `SELECT `+noteColumns+` FROM notes WHERE key = ?`, key).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return NoteRow{}, ErrNotFound
	}
	return row, err
}

// SessionsWithNotes are the sessions with a note not sent or dropped, by
// their oldest one (notes_unsent).
func (s *Store) SessionsWithNotes(ctx context.Context) ([]int64, error) {
	return queryRows(ctx, s.db, func(scan func(...any) error) (int64, error) {
		var id int64
		return id, scan(&id)
	}, `SELECT session_id FROM notes WHERE sent_at IS NULL AND dropped_at IS NULL GROUP BY session_id ORDER BY MIN(id)`)
}

// OldestNote is session's oldest note not sent or dropped; ok is false
// when none waits.
func (s *Store) OldestNote(ctx context.Context, session int64) (NoteRow, bool, error) {
	row, err := scanNote(s.db.QueryRowContext(ctx, `SELECT `+noteColumns+` FROM notes WHERE session_id = ? AND sent_at IS NULL AND dropped_at IS NULL ORDER BY id LIMIT 1`, session).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return NoteRow{}, false, nil
	}
	return row, err == nil, err
}

// NoteSubmitted, NoteWithdrawn and NoteSent are ReportSubmitted,
// ReportWithdrawn and ReportSent for note key.
func (s *Store) NoteSubmitted(ctx context.Context, key string, submission int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE notes SET submission = ? WHERE key = ?`, submission, key)
	return err
}

func (s *Store) NoteWithdrawn(ctx context.Context, key string, attempt int) (bool, error) {
	return changed(s.db.ExecContext(ctx, `UPDATE notes SET submission = 0, attempt = attempt + 1 WHERE key = ? AND attempt = ?`, key, attempt))
}

func (s *Store) NoteSent(ctx context.Context, key string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE notes SET sent_at = ? WHERE key = ? AND sent_at IS NULL`, s.millis(), key)
	return err
}

// DropNote drops note key, not sent: a consult's question once the
// consult ended.
func (s *Store) DropNote(ctx context.Context, key string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE notes SET dropped_at = ? WHERE key = ? AND sent_at IS NULL AND dropped_at IS NULL`, s.millis(), key)
	return err
}
