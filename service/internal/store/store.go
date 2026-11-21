// Package store is the service's work database (ADR-0008): a SQLite file of
// typed tables, versioned by the migrations in this file. Pi Durable keeps
// conversations and tasks in a store of its own; this one keeps what the
// service adds around them.
package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"time"

	_ "modernc.org/sqlite" // the pure-Go driver PiG uses, so the binary carries one copy
)

// migrations are applied in order; migration i brings the schema to version
// i+1. A migration is never edited once released: a change is a new entry.
var migrations = []string{
	`CREATE TABLE devices (
		id TEXT PRIMARY KEY,
		name TEXT NOT NULL UNIQUE,
		token_hash TEXT NOT NULL UNIQUE,
		created_at INTEGER NOT NULL,
		revoked_at INTEGER
	);
	CREATE TABLE sessions (
		id INTEGER PRIMARY KEY,
		title TEXT NOT NULL,
		cwd TEXT NOT NULL,
		provider TEXT NOT NULL,
		model TEXT NOT NULL,
		thinking TEXT NOT NULL DEFAULT '',
		created_by TEXT NOT NULL,
		created_at INTEGER NOT NULL,
		updated_at INTEGER NOT NULL
	);
	CREATE TABLE requests (
		key TEXT PRIMARY KEY,
		kind TEXT NOT NULL,
		result TEXT NOT NULL,
		created_at INTEGER NOT NULL
	);
	CREATE TABLE approvals (
		id TEXT PRIMARY KEY,
		session_id INTEGER NOT NULL,
		task_id INTEGER NOT NULL,
		call_id TEXT NOT NULL,
		tool TEXT NOT NULL,
		arguments TEXT NOT NULL,
		state TEXT NOT NULL CHECK (state IN ('pending', 'allowed', 'denied')),
		requested_at INTEGER NOT NULL,
		decided_at INTEGER,
		decided_by TEXT NOT NULL DEFAULT '',
		reason TEXT NOT NULL DEFAULT '',
		UNIQUE (task_id, call_id)
	);
	CREATE INDEX approvals_pending ON approvals (state) WHERE state = 'pending';`,
	`CREATE TABLE meta (
		key TEXT PRIMARY KEY,
		value INTEGER NOT NULL
	);
	CREATE INDEX approvals_session_pending ON approvals (session_id) WHERE state = 'pending';`,
	// 3: a session's profile, and the rule that routed its model (ADR-0013).
	// Every session before profiles ran on the model its owner named.
	`ALTER TABLE sessions ADD COLUMN profile TEXT NOT NULL DEFAULT '';
	ALTER TABLE sessions ADD COLUMN route TEXT NOT NULL DEFAULT '';
	UPDATE sessions SET route = 'owner';`,
	// 4: threads and asks (ADR-0014).
	`CREATE TABLE threads (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		key TEXT NOT NULL UNIQUE,
		title TEXT NOT NULL,
		agent TEXT NOT NULL,
		supervisor_session INTEGER NOT NULL,
		session INTEGER NOT NULL DEFAULT 0,
		repository TEXT NOT NULL DEFAULT '',
		cwd TEXT NOT NULL DEFAULT '',
		branch TEXT NOT NULL DEFAULT '',
		task TEXT NOT NULL,
		state TEXT NOT NULL,
		report TEXT NOT NULL DEFAULT '',
		owner TEXT NOT NULL,
		task_id INTEGER NOT NULL DEFAULT 0,
		brief TEXT NOT NULL DEFAULT '',
		brief_submission INTEGER NOT NULL DEFAULT 0,
		brief_attempt INTEGER NOT NULL DEFAULT 0,
		briefed_at INTEGER,
		created_at INTEGER NOT NULL,
		updated_at INTEGER NOT NULL
	);
	CREATE INDEX threads_session ON threads (session) WHERE session != 0;
	CREATE INDEX threads_open ON threads (id) WHERE state != 'closed';
	CREATE TABLE reports (
		key TEXT PRIMARY KEY,
		thread_id INTEGER NOT NULL REFERENCES threads (id),
		session_id INTEGER NOT NULL,
		text TEXT NOT NULL,
		created_at INTEGER NOT NULL,
		submission INTEGER NOT NULL DEFAULT 0,
		attempt INTEGER NOT NULL DEFAULT 0,
		sent_at INTEGER
	);
	CREATE INDEX reports_thread ON reports (thread_id) WHERE sent_at IS NULL;
	CREATE INDEX reports_unsent ON reports (created_at) WHERE sent_at IS NULL;
	CREATE TABLE asks (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		key TEXT NOT NULL UNIQUE,
		thread_id INTEGER NOT NULL REFERENCES threads (id),
		session_id INTEGER NOT NULL,
		question TEXT NOT NULL,
		options TEXT NOT NULL DEFAULT '[]',
		state TEXT NOT NULL,
		answer TEXT NOT NULL DEFAULT '',
		answered_by TEXT NOT NULL DEFAULT '',
		created_at INTEGER NOT NULL,
		answered_at INTEGER,
		submission INTEGER NOT NULL DEFAULT 0,
		attempt INTEGER NOT NULL DEFAULT 0,
		delivered_at INTEGER
	);
	CREATE INDEX asks_open ON asks (state) WHERE state = 'open';
	CREATE INDEX asks_undelivered ON asks (state) WHERE state = 'answered' AND delivered_at IS NULL;`,
	// 5: each agent's lessons (ADR-0015). A lesson that is gone keeps its
	// row: a call run again finds what it did by its key.
	`CREATE TABLE lessons (
		id INTEGER PRIMARY KEY,
		key TEXT NOT NULL,
		agent TEXT NOT NULL,
		text TEXT NOT NULL,
		state TEXT NOT NULL,
		writer TEXT NOT NULL,
		session_id INTEGER NOT NULL DEFAULT 0,
		change_text TEXT NOT NULL DEFAULT '',
		change_forget INTEGER NOT NULL DEFAULT 0,
		change_writer TEXT NOT NULL DEFAULT '',
		change_session INTEGER NOT NULL DEFAULT 0,
		created_at INTEGER NOT NULL,
		updated_at INTEGER NOT NULL
	);
	CREATE UNIQUE INDEX lessons_key ON lessons (key) WHERE key != '';
	CREATE INDEX lessons_agent ON lessons (agent, state);
	CREATE INDEX lessons_live ON lessons (state) WHERE state IN ('pending', 'active');
	CREATE INDEX sessions_profile ON sessions (profile) WHERE profile != '';`,
	// 6: the pull requests linked to threads (ADR-0016). ref is the
	// repository, lowercased, # the number: GitHub's names ignore case.
	// github_id is GitHub's own id of the pull request, which no rename
	// reuses; read_seq orders the reads of it (pull_reads), whatever the
	// clock does.
	`CREATE TABLE pull_requests (
		id INTEGER PRIMARY KEY,
		ref TEXT NOT NULL UNIQUE,
		github_id INTEGER NOT NULL,
		read_seq INTEGER NOT NULL DEFAULT 0,
		thread_id INTEGER NOT NULL,
		repository TEXT NOT NULL,
		number INTEGER NOT NULL,
		title TEXT NOT NULL,
		url TEXT NOT NULL,
		state TEXT NOT NULL,
		draft INTEGER NOT NULL DEFAULT 0,
		head TEXT NOT NULL,
		head_sha TEXT NOT NULL,
		base TEXT NOT NULL,
		checked_at INTEGER NOT NULL,
		created_at INTEGER NOT NULL,
		updated_at INTEGER NOT NULL
	);
	CREATE INDEX pull_requests_open ON pull_requests (state) WHERE state = 'open';
	CREATE INDEX pull_requests_thread ON pull_requests (thread_id);
	CREATE TABLE pull_reads (id INTEGER PRIMARY KEY CHECK (id = 1), seq INTEGER NOT NULL);
	INSERT INTO pull_reads (id, seq) VALUES (1, 0);`,
	// Version 7: cross-review verdicts (ADR-0017). A review is keyed by its
	// team call; pull_id is the linked pull request it reviews, head_sha
	// the head it read. ready is whether the pull request has
	// ready-for-human, as pocketd last read or wrote it; checks CI's state
	// at its head when pocketd last read it ("" before a read).
	`CREATE TABLE reviews (
		id INTEGER PRIMARY KEY,
		key TEXT NOT NULL UNIQUE,
		pull_id INTEGER NOT NULL,
		thread_id INTEGER NOT NULL,
		session_id INTEGER NOT NULL,
		agent TEXT NOT NULL,
		provider TEXT NOT NULL,
		model TEXT NOT NULL,
		family TEXT NOT NULL,
		head_sha TEXT NOT NULL,
		verdict TEXT NOT NULL,
		summary TEXT NOT NULL,
		findings TEXT NOT NULL,
		created_at INTEGER NOT NULL
	);
	CREATE INDEX reviews_pull ON reviews (pull_id, head_sha, id);
	ALTER TABLE pull_requests ADD COLUMN ready INTEGER NOT NULL DEFAULT 0;
	ALTER TABLE pull_requests ADD COLUMN checks TEXT NOT NULL DEFAULT '';`,
	// A review thread pocketd starts names the pull request and the head
	// it reviews (ADR-0019); 0 and '' for any other thread.
	`ALTER TABLE threads ADD COLUMN review_pull INTEGER NOT NULL DEFAULT 0;
	ALTER TABLE threads ADD COLUMN review_head TEXT NOT NULL DEFAULT '';
	ALTER TABLE threads ADD COLUMN review_base TEXT NOT NULL DEFAULT '';
	ALTER TABLE reviews ADD COLUMN base_ref TEXT NOT NULL DEFAULT '';
	ALTER TABLE reviews ADD COLUMN merge_base TEXT NOT NULL DEFAULT '';
	ALTER TABLE pull_requests ADD COLUMN merge_base TEXT NOT NULL DEFAULT '';
	CREATE INDEX threads_review ON threads (review_pull) WHERE review_pull != 0;`,
	// A thread's specialist has at most one worker, whose session the
	// thread names (ADR-0021); 0 for none.
	// worker_cursor is the message that began the last of its worker's
	// runs whose reply was kept, so a pass reads on from there.
	`ALTER TABLE threads ADD COLUMN worker_session INTEGER NOT NULL DEFAULT 0;
	ALTER TABLE threads ADD COLUMN worker_cursor INTEGER NOT NULL DEFAULT 0;
	CREATE INDEX threads_worker ON threads (worker_session) WHERE worker_session != 0;`,
	// Version 10: plans, their task graphs, consults and notes (ADR-0020).
	// A plan is bound to its plan thread (threads.plan, with plan_task
	// ''); a task's thread names the plan and the task it delivers. A
	// plan's call records are keyed by their team call. A note is a
	// message pocketd owes a session, sent once by its key; one for a
	// consult that ended is dropped. An ask is written once a plan write
	// named it.
	`CREATE TABLE plans (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		key TEXT NOT NULL UNIQUE,
		thread_id INTEGER NOT NULL UNIQUE,
		supervisor_session INTEGER NOT NULL,
		lead TEXT NOT NULL,
		title TEXT NOT NULL,
		state TEXT NOT NULL,
		text TEXT NOT NULL DEFAULT '',
		rev TEXT NOT NULL DEFAULT '',
		approved_by TEXT NOT NULL DEFAULT '',
		approved_at INTEGER,
		held INTEGER NOT NULL DEFAULT 0,
		created_at INTEGER NOT NULL,
		updated_at INTEGER NOT NULL
	);
	CREATE INDEX plans_open ON plans (state) WHERE state NOT IN ('done', 'dropped');
	CREATE INDEX plans_held ON plans (id) WHERE held = 1;
	CREATE TABLE plan_graphs (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		plan_id INTEGER NOT NULL,
		version INTEGER NOT NULL,
		rev TEXT NOT NULL,
		state TEXT NOT NULL,
		created_at INTEGER NOT NULL,
		UNIQUE (plan_id, version)
	);
	CREATE TABLE plan_tasks (
		graph_id INTEGER NOT NULL,
		position INTEGER NOT NULL,
		task TEXT NOT NULL,
		title TEXT NOT NULL,
		agent TEXT NOT NULL,
		repository TEXT NOT NULL DEFAULT '',
		acceptance TEXT NOT NULL,
		deps TEXT NOT NULL,
		state TEXT NOT NULL,
		thread_id INTEGER NOT NULL DEFAULT 0,
		PRIMARY KEY (graph_id, task)
	);
	CREATE TABLE plan_calls (
		key TEXT PRIMARY KEY,
		plan_id INTEGER NOT NULL,
		action TEXT NOT NULL,
		result TEXT NOT NULL,
		created_at INTEGER NOT NULL
	);
	CREATE TABLE consults (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		key TEXT NOT NULL UNIQUE,
		owner TEXT NOT NULL,
		plan_id INTEGER NOT NULL,
		agent TEXT NOT NULL,
		question TEXT NOT NULL,
		ask_id INTEGER NOT NULL DEFAULT 0,
		state TEXT NOT NULL,
		session INTEGER NOT NULL DEFAULT 0,
		stance TEXT NOT NULL DEFAULT '',
		summary TEXT NOT NULL DEFAULT '',
		why TEXT NOT NULL DEFAULT '',
		created_at INTEGER NOT NULL,
		ended_at INTEGER
	);
	CREATE INDEX consults_plan ON consults (plan_id);
	CREATE INDEX consults_session ON consults (session) WHERE session != 0;
	CREATE INDEX consults_open ON consults (state) WHERE state IN ('starting', 'running');
	CREATE TABLE notes (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		key TEXT NOT NULL UNIQUE,
		session_id INTEGER NOT NULL,
		consult_id INTEGER NOT NULL DEFAULT 0,
		text TEXT NOT NULL,
		created_at INTEGER NOT NULL,
		submission INTEGER NOT NULL DEFAULT 0,
		attempt INTEGER NOT NULL DEFAULT 0,
		sent_at INTEGER,
		dropped_at INTEGER
	);
	CREATE INDEX notes_unsent ON notes (session_id, id) WHERE sent_at IS NULL AND dropped_at IS NULL;
	ALTER TABLE threads ADD COLUMN plan INTEGER NOT NULL DEFAULT 0;
	ALTER TABLE threads ADD COLUMN plan_task TEXT NOT NULL DEFAULT '';
	ALTER TABLE asks ADD COLUMN written_at INTEGER;
	CREATE INDEX asks_thread ON asks (thread_id);`,
	// Version 11: the worker model the supervisor asked for, for the
	// owner, when it delegated the thread (ADR-0022); '' for none, when
	// the profile and the owner's list choose.
	`ALTER TABLE threads ADD COLUMN worker_provider TEXT NOT NULL DEFAULT '';
	ALTER TABLE threads ADD COLUMN worker_model TEXT NOT NULL DEFAULT '';`,
	// Version 12: a thread's shift (ADR-0024): its rules, its round and
	// state, why it stopped, the streak of CI failures, and each round's
	// verdict.
	`CREATE TABLE shifts (
		thread_id INTEGER PRIMARY KEY REFERENCES threads(id),
		stop TEXT NOT NULL,
		max_rounds INTEGER NOT NULL,
		deadline INTEGER,
		ci INTEGER NOT NULL DEFAULT 0,
		state TEXT NOT NULL,
		reason TEXT NOT NULL DEFAULT '',
		why TEXT NOT NULL DEFAULT '',
		round INTEGER NOT NULL DEFAULT 1,
		failures INTEGER NOT NULL DEFAULT 0,
		fingerprint TEXT NOT NULL DEFAULT '',
		ci_heads TEXT NOT NULL DEFAULT '',
		report_run INTEGER NOT NULL DEFAULT 0,
		created_at INTEGER NOT NULL,
		updated_at INTEGER NOT NULL,
		ended_at INTEGER
	);
	CREATE INDEX shifts_live ON shifts (thread_id) WHERE state IN ('running', 'summary');
	CREATE TABLE shift_rounds (
		thread_id INTEGER NOT NULL REFERENCES threads(id),
		n INTEGER NOT NULL,
		verdict TEXT NOT NULL,
		reason TEXT NOT NULL DEFAULT '',
		at INTEGER NOT NULL,
		PRIMARY KEY (thread_id, n)
	);`,
	// Version 13: each profile's standing session (ADR-0026), the one
	// New Session opens for good.
	`CREATE TABLE standing_sessions (
		profile TEXT PRIMARY KEY,
		session INTEGER NOT NULL UNIQUE REFERENCES sessions(id),
		created_at INTEGER NOT NULL
	);`,
	// Version 14: the worktrees the owner's sessions work in (ADR-0027):
	// one a request, each branch, path and place one request's, with the
	// checkout as git named it, the worktree's real place, and how far the
	// request's own making of it got.
	`CREATE TABLE session_worktrees (
		branch TEXT PRIMARY KEY,
		path TEXT NOT NULL UNIQUE,
		request TEXT NOT NULL UNIQUE,
		top TEXT NOT NULL,
		dest TEXT NOT NULL UNIQUE,
		target TEXT NOT NULL,
		made INTEGER NOT NULL DEFAULT 0,
		created_at INTEGER NOT NULL
	);`,
	// Version 15: the author's model a review thread's reviewer was
	// chosen by (ADR-0019, ADR-0028), kept with the review, so a change of
	// the author's model counts from its next review; '' for any other
	// thread. A review recorded before takes its author's session's model
	// now, the one its reviewer was chosen by, since no model changed
	// before this version (Codex review of #48, round 2).
	`ALTER TABLE threads ADD COLUMN review_author_provider TEXT NOT NULL DEFAULT '';
	ALTER TABLE threads ADD COLUMN review_author_model TEXT NOT NULL DEFAULT '';
	UPDATE threads SET
		review_author_provider = COALESCE((SELECT s.provider FROM pull_requests p JOIN threads a ON a.id = p.thread_id
			JOIN sessions s ON s.id = a.session WHERE p.id = threads.review_pull), ''),
		review_author_model = COALESCE((SELECT s.model FROM pull_requests p JOIN threads a ON a.id = p.thread_id
			JOIN sessions s ON s.id = a.session WHERE p.id = threads.review_pull), '')
		WHERE review_pull != 0;`,
	// Version 16: schedules (ADR-0030): the owner's, from their schedules
	// file, keyed by their id there, and a supervisor's own, keyed by the
	// team call that made them, each with a random owner its messages'
	// note keys name. spec is the when as written, zone the IANA zone it
	// was read in, and first_at the first time it went out for; next_at is
	// when it goes out next, NULL once it will not. runs counts the
	// messages filed, skipped the times a repeat's last message still
	// waited, and last_* says what its last time did.
	`CREATE TABLE schedules (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		key TEXT NOT NULL UNIQUE,
		owner TEXT NOT NULL UNIQUE,
		source TEXT NOT NULL,
		name TEXT NOT NULL DEFAULT '',
		session_id INTEGER NOT NULL DEFAULT 0,
		profile TEXT NOT NULL DEFAULT '',
		spec TEXT NOT NULL,
		zone TEXT NOT NULL,
		text TEXT NOT NULL,
		first_at INTEGER NOT NULL,
		next_at INTEGER,
		state TEXT NOT NULL,
		runs INTEGER NOT NULL DEFAULT 0,
		skipped INTEGER NOT NULL DEFAULT 0,
		last_at INTEGER,
		last_outcome TEXT NOT NULL DEFAULT '',
		last_why TEXT NOT NULL DEFAULT '',
		created_at INTEGER NOT NULL,
		updated_at INTEGER NOT NULL
	);
	CREATE INDEX schedules_due ON schedules (next_at) WHERE state = 'active';
	CREATE INDEX schedules_session ON schedules (session_id) WHERE state = 'active' AND source = 'session';`,
}

// SchemaVersion is the schema version this build migrates to.
var SchemaVersion = len(migrations)

// ErrNotFound is returned for a row that does not exist.
var ErrNotFound = errors.New("not found")

// ErrConflict is returned for a change the row's state does not allow.
var ErrConflict = errors.New("conflict")

// ErrRequestReused is returned when a request key already names a different
// operation or target.
var ErrRequestReused = errors.New("the request id was already used for a different request")

// Store is the work database. It is safe for concurrent use.
type Store struct {
	db  *sql.DB
	now func() time.Time
}

// Open opens or creates the database at path and migrates it. now is the
// clock of every timestamp the store writes; nil is time.Now.
func Open(ctx context.Context, path string, now func() time.Time) (*Store, error) {
	if now == nil {
		now = time.Now
	}
	db, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		return nil, err
	}
	s := &Store{db: db, now: now}
	if err := retryBusy(ctx, s.migrate); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// busyBudget bounds retryBusy's whole run, and slowBusy marks an attempt
// that already waited out SQLite's busy_timeout: that one is not retried.
const (
	busyBudget = 6 * time.Second
	slowBusy   = time.Second
)

// retryBusy runs step until it does not fail with SQLITE_BUSY. SQLite
// reports some lock conflicts at once instead of waiting out busy_timeout:
// two connections switching a new database to WAL, or upgrading a read
// lock. Two processes opening the same new database meet them. Only those
// immediate conflicts are retried, within busyBudget: an attempt that
// waited out the busy timeout already waited long enough.
func retryBusy(ctx context.Context, step func(context.Context) error) error {
	start := time.Now()
	wait := 5 * time.Millisecond
	for {
		began := time.Now()
		err := step(ctx)
		if !isBusy(err) || time.Since(began) >= slowBusy || time.Since(start)+wait >= busyBudget {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
		wait = min(2*wait, time.Second)
	}
}

// isBusy reports SQLITE_BUSY (5) and its extended codes.
func isBusy(err error) bool {
	var coded interface{ Code() int }
	return errors.As(err, &coded) && coded.Code()&0xff == 5
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// dsn is the SQLite URI of path: the path escaped, so a folder name with
// "?" or "#" names that folder, then the pragmas.
func dsn(path string) string {
	u := url.URL{Scheme: "file", Path: path, OmitHost: true}
	return u.String() + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_txlock=immediate"
}

// migrate reads the schema version and applies the pending migrations in one
// immediate transaction, so two processes opening a new or older database at
// once apply each migration once.
func (s *Store) migrate(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_version (version INTEGER NOT NULL)`); err != nil {
		return err
	}
	var version int
	err = tx.QueryRowContext(ctx, `SELECT version FROM schema_version`).Scan(&version)
	if errors.Is(err, sql.ErrNoRows) {
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_version (version) VALUES (0)`); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	if version > len(migrations) {
		return fmt.Errorf("work database schema %d is newer than this build's %d", version, len(migrations))
	}
	for i := version; i < len(migrations); i++ {
		if _, err := tx.ExecContext(ctx, migrations[i]); err != nil {
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE schema_version SET version = ?`, len(migrations)); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) millis() int64 { return s.now().UnixMilli() }

func fromMillis(ms int64) time.Time { return time.UnixMilli(ms).UTC() }

// Device is a signed-in device of the owner.
type Device struct {
	ID        string
	Name      string
	CreatedAt time.Time
	RevokedAt *time.Time
}

// CreateDevice records a device with the hash of its token.
func (s *Store) CreateDevice(ctx context.Context, name, tokenHash string) (Device, error) {
	sum := sha256.Sum256([]byte(name + "\x00" + tokenHash))
	d := Device{ID: "d" + hex.EncodeToString(sum[:6]), Name: name, CreatedAt: fromMillis(s.millis())}
	_, err := s.db.ExecContext(ctx, `INSERT INTO devices (id, name, token_hash, created_at) VALUES (?, ?, ?, ?)`,
		d.ID, d.Name, tokenHash, d.CreatedAt.UnixMilli())
	if err != nil {
		return Device{}, fmt.Errorf("device %q: %w", name, err)
	}
	return d, nil
}

// DeviceByTokenHash returns the unrevoked device with that token hash.
func (s *Store) DeviceByTokenHash(ctx context.Context, tokenHash string) (Device, error) {
	var d Device
	var created int64
	err := s.db.QueryRowContext(ctx, `SELECT id, name, created_at FROM devices WHERE token_hash = ? AND revoked_at IS NULL`, tokenHash).
		Scan(&d.ID, &d.Name, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return Device{}, ErrNotFound
	}
	d.CreatedAt = fromMillis(created)
	return d, err
}

// Devices lists every device, revoked ones included, by name.
func (s *Store) Devices(ctx context.Context) ([]Device, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, name, created_at, revoked_at FROM devices ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Device
	for rows.Next() {
		var d Device
		var created int64
		var revoked sql.NullInt64
		if err := rows.Scan(&d.ID, &d.Name, &created, &revoked); err != nil {
			return nil, err
		}
		d.CreatedAt = fromMillis(created)
		if revoked.Valid {
			t := fromMillis(revoked.Int64)
			d.RevokedAt = &t
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// RevokeDevice revokes the device's token. Revoking twice is ErrNotFound.
func (s *Store) RevokeDevice(ctx context.Context, name string) error {
	result, err := s.db.ExecContext(ctx, `UPDATE devices SET revoked_at = ? WHERE name = ? AND revoked_at IS NULL`, s.millis(), name)
	if err != nil {
		return err
	}
	if n, _ := result.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// SessionRow is a session's row; its id is its Durable conversation id.
type SessionRow struct {
	ID       int64
	Title    string
	Cwd      string
	Provider string
	Model    string
	Thinking string
	// Profile is the profile the session started from, Route the router's
	// rule for its model; both empty for a plain session.
	Profile string
	Route   string
	// Standing says the session is its profile's standing session
	// (ADR-0026).
	Standing bool
	// StartedBy is the session that started this one, 0 for none: for a
	// thread's worker, the thread's specialist session, which called
	// team_worker (protocol rule 23a). It is read from the thread that
	// names the session its worker, which RecordWorkerSession writes in
	// the transaction that records the session, so no read of the session
	// lacks it; InsertSession does not write it.
	StartedBy int64
	CreatedBy string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// InsertSession records a new session; CreatedAt and UpdatedAt are set now.
func (s *Store) InsertSession(ctx context.Context, row SessionRow) (SessionRow, error) {
	now := s.millis()
	row.CreatedAt, row.UpdatedAt = fromMillis(now), fromMillis(now)
	_, err := s.db.ExecContext(ctx, `INSERT INTO sessions (id, title, cwd, provider, model, thinking, profile, route, created_by, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, row.ID, row.Title, row.Cwd, row.Provider, row.Model, row.Thinking, row.Profile, row.Route, row.CreatedBy, now, now)
	return row, err
}

// sessionColumns are a session's columns, its standing and its starter:
// the specialist's session of the thread whose worker it is. The
// starter's predicate repeats threads_worker's, so SQLite reads that
// index.
const sessionColumns = `id, title, cwd, provider, model, thinking, profile, route,
	EXISTS (SELECT 1 FROM standing_sessions WHERE standing_sessions.session = sessions.id),
	COALESCE((SELECT threads.session FROM threads WHERE threads.worker_session = sessions.id AND threads.worker_session != 0
		ORDER BY threads.id LIMIT 1), 0),
	created_by, created_at, updated_at`

func scanSession(scan func(...any) error) (SessionRow, error) {
	var row SessionRow
	var created, updated int64
	err := scan(&row.ID, &row.Title, &row.Cwd, &row.Provider, &row.Model, &row.Thinking, &row.Profile, &row.Route, &row.Standing, &row.StartedBy,
		&row.CreatedBy, &created, &updated)
	row.CreatedAt, row.UpdatedAt = fromMillis(created), fromMillis(updated)
	return row, err
}

// Session returns one session.
func (s *Store) Session(ctx context.Context, id int64) (SessionRow, error) {
	row, err := scanSession(s.db.QueryRowContext(ctx, `SELECT `+sessionColumns+` FROM sessions WHERE id = ?`, id).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return SessionRow{}, ErrNotFound
	}
	return row, err
}

// Sessions lists every session, the most recently updated first.
func (s *Store) Sessions(ctx context.Context) ([]SessionRow, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+sessionColumns+` FROM sessions ORDER BY updated_at DESC, id DESC`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []SessionRow
	for rows.Next() {
		row, err := scanSession(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// TouchSession moves a session's UpdatedAt to at, the time of its newest
// entry, unless it is later already: a write retried after a later entry's
// never moves a session back, nor ahead of what happened after.
func (s *Store) TouchSession(ctx context.Context, id int64, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE sessions SET updated_at = MAX(updated_at, ?) WHERE id = ?`, at.UnixMilli(), id)
	return err
}

// A request record binds a device-scoped request key to one operation on
// one target (its kind, for example "abort:12") and keeps its result, so a
// retry replays the first outcome and a reused key for another operation is
// refused.

// ClaimRequest records key for kind, or returns the existing record: existed
// reports a retry, and result is what CompleteRequest stored ("" while
// incomplete). A key recorded for another kind is ErrRequestReused.
func (s *Store) ClaimRequest(ctx context.Context, key, kind string) (result string, existed bool, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", false, err
	}
	defer func() { _ = tx.Rollback() }()
	result, existed, err = claimRequest(ctx, tx, key, kind, s.millis())
	if err != nil {
		return "", false, err
	}
	return result, existed, tx.Commit()
}

func claimRequest(ctx context.Context, tx *sql.Tx, key, kind string, now int64) (string, bool, error) {
	var storedKind, result string
	err := tx.QueryRowContext(ctx, `SELECT kind, result FROM requests WHERE key = ?`, key).Scan(&storedKind, &result)
	switch {
	case err == nil:
		if storedKind != kind {
			return "", true, ErrRequestReused
		}
		return result, true, nil
	case !errors.Is(err, sql.ErrNoRows):
		return "", false, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO requests (key, kind, result, created_at) VALUES (?, ?, '', ?)`, key, kind, now)
	return "", false, err
}

// LookupRequest returns the record of key without writing one: existed is
// false for a new key. A key recorded for another kind is ErrRequestReused.
func (s *Store) LookupRequest(ctx context.Context, key, kind string) (result string, existed bool, err error) {
	var storedKind string
	err = s.db.QueryRowContext(ctx, `SELECT kind, result FROM requests WHERE key = ?`, key).Scan(&storedKind, &result)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "", false, nil
	case err != nil:
		return "", false, err
	case storedKind != kind:
		return "", true, ErrRequestReused
	}
	return result, true, nil
}

// RecordRequest records a request that took effect. Recording a key twice
// keeps the first record.
func (s *Store) RecordRequest(ctx context.Context, key, kind, result string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO requests (key, kind, result, created_at) VALUES (?, ?, ?, ?) ON CONFLICT (key) DO NOTHING`,
		key, kind, result, s.millis())
	return err
}

// DeleteRequest removes the record of a request that did not take effect,
// so its key is free again.
func (s *Store) DeleteRequest(ctx context.Context, key string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM requests WHERE key = ?`, key)
	return err
}

// ReleaseRequest frees key when it still holds a reservation of kind that
// did not complete (no result): one completed, or of another kind, stays.
// A release tried again later never frees a request that took effect.
func (s *Store) ReleaseRequest(ctx context.Context, key, kind string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM requests WHERE key = ? AND kind = ? AND result = ''`, key, kind)
	return err
}

// LeftClaims are the claims of kind prefix that never completed, by key
// with their kind, made before before: a stop of pocketd left them, and
// no request of this run is under way with them (Codex review of #48,
// round 4).
func (s *Store) LeftClaims(ctx context.Context, prefix string, before time.Time) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT key, kind FROM requests WHERE result = '' AND substr(kind, 1, length(?)) = ? AND created_at < ?`,
		prefix, prefix, before.UnixMilli())
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string]string{}
	for rows.Next() {
		var key, kind string
		if err := rows.Scan(&key, &kind); err != nil {
			return nil, err
		}
		out[key] = kind
	}
	return out, rows.Err()
}

// CompleteRequest stores the result of a claimed request.
func (s *Store) CompleteRequest(ctx context.Context, key, result string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE requests SET result = ? WHERE key = ?`, result, key)
	return err
}

// CreateSession records a new session and the result of the request that
// created it in one transaction: either both exist or neither does. A
// session row that already exists (an earlier attempt recorded it, then
// failed) is kept.
func (s *Store) CreateSession(ctx context.Context, key string, row SessionRow) (SessionRow, error) {
	return s.createSession(ctx, key, row, nil)
}

// createSession is CreateSession, and runs also, in its transaction, when
// set (a thread's attachment of its specialist's session).
func (s *Store) createSession(ctx context.Context, key string, row SessionRow, also func(tx *sql.Tx, now int64) error) (SessionRow, error) {
	now := s.millis()
	row.CreatedAt, row.UpdatedAt = fromMillis(now), fromMillis(now)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return SessionRow{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `INSERT INTO sessions (id, title, cwd, provider, model, thinking, profile, route, created_by, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT (id) DO NOTHING`, row.ID, row.Title, row.Cwd, row.Provider, row.Model, row.Thinking, row.Profile, row.Route, row.CreatedBy, now, now); err != nil {
		return SessionRow{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO requests (key, kind, result, created_at) VALUES (?, 'createSession', ?, ?)
		ON CONFLICT (key) DO UPDATE SET result = excluded.result WHERE kind = 'createSession'`,
		key, strconv.FormatInt(row.ID, 10), now); err != nil {
		return SessionRow{}, err
	}
	if also != nil {
		if err := also(tx, now); err != nil {
			return SessionRow{}, err
		}
	}
	return row, tx.Commit()
}

// ApprovalRow is an approval's row.
type ApprovalRow struct {
	ID          string
	SessionID   int64
	TaskID      int64
	CallID      string
	Tool        string
	Arguments   []byte
	State       string
	RequestedAt time.Time
	DecidedAt   *time.Time
	DecidedBy   string
	Reason      string
}

// ApprovalID is the id of the approval of one tool call of one task: the
// same call asks for the same approval after a restart.
func ApprovalID(taskID int64, callID string) string {
	sum := sha256.Sum256([]byte(strconv.FormatInt(taskID, 10) + "\x00" + callID))
	return "a" + hex.EncodeToString(sum[:8])
}

// RequestApproval records a pending approval for a tool call, or returns the
// existing row for that call (pending or decided), and reports whether it
// was created.
func (s *Store) RequestApproval(ctx context.Context, row ApprovalRow) (ApprovalRow, bool, error) {
	row.ID = ApprovalID(row.TaskID, row.CallID)
	row.State = "pending"
	row.RequestedAt = fromMillis(s.millis())
	result, err := s.db.ExecContext(ctx, `INSERT INTO approvals (id, session_id, task_id, call_id, tool, arguments, state, requested_at)
		VALUES (?, ?, ?, ?, ?, ?, 'pending', ?) ON CONFLICT (id) DO NOTHING`,
		row.ID, row.SessionID, row.TaskID, row.CallID, row.Tool, string(row.Arguments), row.RequestedAt.UnixMilli())
	if err != nil {
		return ApprovalRow{}, false, err
	}
	n, _ := result.RowsAffected()
	stored, err := s.Approval(ctx, row.ID)
	return stored, n == 1, err
}

const approvalColumns = `id, session_id, task_id, call_id, tool, arguments, state, requested_at, decided_at, decided_by, reason`

func scanApproval(scan func(...any) error) (ApprovalRow, error) {
	var row ApprovalRow
	var arguments string
	var requested int64
	var decided sql.NullInt64
	err := scan(&row.ID, &row.SessionID, &row.TaskID, &row.CallID, &row.Tool, &arguments, &row.State, &requested, &decided, &row.DecidedBy, &row.Reason)
	row.Arguments = []byte(arguments)
	row.RequestedAt = fromMillis(requested)
	if decided.Valid {
		t := fromMillis(decided.Int64)
		row.DecidedAt = &t
	}
	return row, err
}

// Approval returns one approval.
func (s *Store) Approval(ctx context.Context, id string) (ApprovalRow, error) {
	row, err := scanApproval(s.db.QueryRowContext(ctx, `SELECT `+approvalColumns+` FROM approvals WHERE id = ?`, id).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return ApprovalRow{}, ErrNotFound
	}
	return row, err
}

// PendingApprovals lists pending approvals, the oldest first.
func (s *Store) PendingApprovals(ctx context.Context) ([]ApprovalRow, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+approvalColumns+` FROM approvals WHERE state = 'pending' ORDER BY requested_at, id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []ApprovalRow
	for rows.Next() {
		row, err := scanApproval(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// DecideApproval moves a pending approval to allowed or denied, and records
// the request key in the same transaction. A retry with the same key returns
// the decided row (replayed true). Deciding a decided approval with a new key
// is ErrConflict; an unknown id is ErrNotFound; a key used for another
// request is ErrRequestReused.
func (s *Store) DecideApproval(ctx context.Context, key, id string, allow bool, by, reason string) (row ApprovalRow, replayed bool, err error) {
	state := "denied"
	if allow {
		state = "allowed"
	}
	now := s.millis()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ApprovalRow{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	_, existed, err := claimRequest(ctx, tx, key, "decide:"+id, now)
	if err != nil {
		return ApprovalRow{}, false, err
	}
	if !existed {
		result, err := tx.ExecContext(ctx, `UPDATE approvals SET state = ?, decided_at = ?, decided_by = ?, reason = ? WHERE id = ? AND state = 'pending'`,
			state, now, by, reason, id)
		if err != nil {
			return ApprovalRow{}, false, err
		}
		if n, _ := result.RowsAffected(); n == 0 {
			if _, err := scanApproval(tx.QueryRowContext(ctx, `SELECT `+approvalColumns+` FROM approvals WHERE id = ?`, id).Scan); errors.Is(err, sql.ErrNoRows) {
				return ApprovalRow{}, false, ErrNotFound
			} else if err != nil {
				return ApprovalRow{}, false, err
			}
			return ApprovalRow{}, false, ErrConflict
		}
	}
	row, err = scanApproval(tx.QueryRowContext(ctx, `SELECT `+approvalColumns+` FROM approvals WHERE id = ?`, id).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return ApprovalRow{}, false, ErrNotFound
	} else if err != nil {
		return ApprovalRow{}, false, err
	}
	return row, existed, tx.Commit()
}

// RetireApproval denies one pending approval whose task ended; it returns the
// row, or ErrConflict when it was no longer pending.
func (s *Store) RetireApproval(ctx context.Context, id, reason string) (ApprovalRow, error) {
	rows, err := s.retire(ctx, `id = ?`, id, reason)
	if err != nil {
		return ApprovalRow{}, err
	}
	if len(rows) == 0 {
		return ApprovalRow{}, ErrConflict
	}
	return rows[0], nil
}

func (s *Store) retire(ctx context.Context, where string, arg any, reason string) ([]ApprovalRow, error) { //nolint:unparam // one caller today
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := tx.QueryContext(ctx, `SELECT id FROM approvals WHERE state = 'pending' AND `+where, arg)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	_ = rows.Close()
	now := s.millis()
	var out []ApprovalRow
	for _, id := range ids {
		if _, err := tx.ExecContext(ctx, `UPDATE approvals SET state = 'denied', decided_at = ?, decided_by = 'pocketd', reason = ? WHERE id = ?`, now, reason, id); err != nil {
			return nil, err
		}
		row, err := scanApproval(tx.QueryRowContext(ctx, `SELECT `+approvalColumns+` FROM approvals WHERE id = ?`, id).Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, tx.Commit()
}

// HasPendingApproval reports whether a session has a pending approval.
func (s *Store) HasPendingApproval(ctx context.Context, sessionID int64) (bool, error) {
	var one int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM approvals WHERE session_id = ? AND state = 'pending' LIMIT 1`, sessionID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// PendingSessions returns the ids of the sessions with a pending approval.
func (s *Store) PendingSessions(ctx context.Context) (map[int64]bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT session_id FROM approvals WHERE state = 'pending'`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[int64]bool{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// ReserveSeq reserves the event numbers [start, start+block) and returns
// start: at least floor, and above every number reserved before, so numbers
// grow across restarts even when the clock moves back.
func (s *Store) ReserveSeq(ctx context.Context, floor, block int64) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	var high int64
	err = tx.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = 'event_seq'`).Scan(&high)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	start := max(floor, high)
	if _, err := tx.ExecContext(ctx, `INSERT INTO meta (key, value) VALUES ('event_seq', ?) ON CONFLICT (key) DO UPDATE SET value = excluded.value`, start+block); err != nil {
		return 0, err
	}
	return start, tx.Commit()
}
