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
	`ALTER TABLE sessions ADD COLUMN profile TEXT NOT NULL DEFAULT '';
	ALTER TABLE sessions ADD COLUMN route TEXT NOT NULL DEFAULT '';`,
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
	Profile   string
	Route     string
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

const sessionColumns = `id, title, cwd, provider, model, thinking, profile, route, created_by, created_at, updated_at`

func scanSession(scan func(...any) error) (SessionRow, error) {
	var row SessionRow
	var created, updated int64
	err := scan(&row.ID, &row.Title, &row.Cwd, &row.Provider, &row.Model, &row.Thinking, &row.Profile, &row.Route, &row.CreatedBy, &created, &updated)
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
