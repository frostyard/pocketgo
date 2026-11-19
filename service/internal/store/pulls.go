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

// Pull request states (ADR-0016).
const (
	PullOpen   = "open"
	PullClosed = "closed"
	PullMerged = "merged"
)

// ErrPullTaken is a pull request another thread links already.
var ErrPullTaken = errors.New("the pull request is linked to another thread")

// PullRow is a pull request linked to a thread, as pocketd last read it on
// GitHub.
type PullRow struct {
	ID         int64
	ThreadID   int64
	Repository string
	Number     int64
	Title      string
	URL        string
	State      string
	Draft      bool
	Head       string
	HeadSHA    string
	Base       string
	CheckedAt  time.Time
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// Ref is the pull request as repository#number.
func (p PullRow) Ref() string { return p.Repository + "#" + strconv.FormatInt(p.Number, 10) }

// pullRef is the unique key of a pull request: GitHub's names ignore case.
func pullRef(repository string, number int64) string {
	return strings.ToLower(repository) + "#" + strconv.FormatInt(number, 10)
}

const pullColumns = `id, thread_id, repository, number, title, url, state, draft, head, head_sha, base, checked_at, created_at, updated_at`

func scanPull(scan func(...any) error) (PullRow, error) {
	var p PullRow
	var draft int
	var checked, created, updated int64
	err := scan(&p.ID, &p.ThreadID, &p.Repository, &p.Number, &p.Title, &p.URL, &p.State, &draft, &p.Head, &p.HeadSHA, &p.Base,
		&checked, &created, &updated)
	p.Draft = draft != 0
	p.CheckedAt, p.CreatedAt, p.UpdatedAt = fromMillis(checked), fromMillis(created), fromMillis(updated)
	return p, err
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// linkPull links p to thread in tx, or brings the row it has up to date:
// one another thread links is ErrPullTaken.
func linkPull(ctx context.Context, tx *sql.Tx, now, thread int64, p PullRow) (PullRow, error) {
	ref := pullRef(p.Repository, p.Number)
	cur, err := scanPull(tx.QueryRowContext(ctx, `SELECT `+pullColumns+` FROM pull_requests WHERE ref = ?`, ref).Scan)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if _, err := tx.ExecContext(ctx, `INSERT INTO pull_requests (ref, thread_id, repository, number, title, url, state, draft, head, head_sha, base,
			checked_at, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			ref, thread, p.Repository, p.Number, p.Title, p.URL, p.State, boolInt(p.Draft), p.Head, p.HeadSHA, p.Base, now, now, now); err != nil {
			return PullRow{}, err
		}
	case err != nil:
		return PullRow{}, err
	case cur.ThreadID != thread:
		return PullRow{}, fmt.Errorf("%w: %s is linked to thread T%d", ErrPullTaken, cur.Ref(), cur.ThreadID)
	default:
		if _, err := updatePull(ctx, tx, now, cur, p); err != nil {
			return PullRow{}, err
		}
	}
	return scanPull(tx.QueryRowContext(ctx, `SELECT `+pullColumns+` FROM pull_requests WHERE ref = ?`, ref).Scan)
}

// updatePull records what GitHub says of cur now (p): it was read now, and
// it changed when anything it says did.
func updatePull(ctx context.Context, tx *sql.Tx, now int64, cur, p PullRow) (bool, error) {
	changed := cur.Title != p.Title || cur.URL != p.URL || cur.State != p.State || cur.Draft != p.Draft ||
		cur.Head != p.Head || cur.HeadSHA != p.HeadSHA || cur.Base != p.Base || cur.Repository != p.Repository
	updated := toMillis(cur.UpdatedAt)
	if changed {
		updated = now
	}
	_, err := tx.ExecContext(ctx, `UPDATE pull_requests SET repository = ?, title = ?, url = ?, state = ?, draft = ?, head = ?, head_sha = ?,
		base = ?, checked_at = ?, updated_at = ? WHERE id = ?`,
		p.Repository, p.Title, p.URL, p.State, boolInt(p.Draft), p.Head, p.HeadSHA, p.Base, now, updated, cur.ID)
	return changed, err
}

func toMillis(t time.Time) int64 { return t.UnixMilli() }

// UpdatePull records what GitHub says of pull request id now (p), and says
// whether it changed.
func (s *Store) UpdatePull(ctx context.Context, id int64, p PullRow) (PullRow, bool, error) {
	now := s.millis()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return PullRow{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	cur, err := scanPull(tx.QueryRowContext(ctx, `SELECT `+pullColumns+` FROM pull_requests WHERE id = ?`, id).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return PullRow{}, false, ErrNotFound
	} else if err != nil {
		return PullRow{}, false, err
	}
	changed, err := updatePull(ctx, tx, now, cur, p)
	if err != nil {
		return PullRow{}, false, err
	}
	row, err := scanPull(tx.QueryRowContext(ctx, `SELECT `+pullColumns+` FROM pull_requests WHERE id = ?`, id).Scan)
	if err != nil {
		return PullRow{}, false, err
	}
	return row, changed, tx.Commit()
}

func (s *Store) pulls(ctx context.Context, query string, args ...any) ([]PullRow, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []PullRow
	for rows.Next() {
		p, err := scanPull(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// openPulls is the predicate of the open pull requests (pull_requests_open).
const openPulls = `state = 'open'`

// OpenPulls are the open pull requests, oldest first: those pocketd keeps
// current.
func (s *Store) OpenPulls(ctx context.Context) ([]PullRow, error) {
	return s.pulls(ctx, `SELECT `+pullColumns+` FROM pull_requests WHERE `+openPulls+` ORDER BY id`)
}

// PullsOfThread are thread's pull requests, oldest first.
func (s *Store) PullsOfThread(ctx context.Context, thread int64) ([]PullRow, error) {
	return s.pulls(ctx, `SELECT `+pullColumns+` FROM pull_requests WHERE thread_id = ? ORDER BY id`, thread)
}

// ListPulls are the pull requests of the threads that are not closed, and
// every open one, oldest first.
func (s *Store) ListPulls(ctx context.Context) ([]PullRow, error) {
	return s.pulls(ctx, `SELECT `+pullColumns+` FROM pull_requests WHERE `+openPulls+` UNION
		SELECT `+pullColumns+` FROM pull_requests WHERE thread_id IN (SELECT id FROM threads WHERE state != 'closed') ORDER BY id`)
}

// CloseMergedThread closes thread when its pull requests are all merged
// or closed, one at least merged (ADR-0016), with text of them as its
// report, and says whether it did. A thread closed already, or with an
// open pull request, or none merged, stays as it is.
func (s *Store) CloseMergedThread(ctx context.Context, thread int64, text func(merged []PullRow) string) (ThreadRow, bool, error) {
	now := s.millis()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ThreadRow{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	row, err := scanThread(tx.QueryRowContext(ctx, `SELECT `+threadColumns+` FROM threads WHERE id = ?`, thread).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return ThreadRow{}, false, ErrNotFound
	} else if err != nil {
		return ThreadRow{}, false, err
	}
	pulls, err := pullsIn(ctx, tx, thread)
	if err != nil {
		return ThreadRow{}, false, err
	}
	var merged []PullRow
	for _, p := range pulls {
		switch p.State {
		case PullOpen:
			return row, false, nil
		case PullMerged:
			merged = append(merged, p)
		}
	}
	if row.State == ThreadClosed || len(merged) == 0 {
		return row, false, nil
	}
	if _, err := tx.ExecContext(ctx, `UPDATE threads SET state = ?, report = ?, updated_at = ? WHERE id = ? AND state != ?`,
		ThreadClosed, text(merged), now, thread, ThreadClosed); err != nil {
		return ThreadRow{}, false, err
	}
	row, err = scanThread(tx.QueryRowContext(ctx, `SELECT `+threadColumns+` FROM threads WHERE id = ?`, thread).Scan)
	if err != nil {
		return ThreadRow{}, false, err
	}
	return row, true, tx.Commit()
}

func pullsIn(ctx context.Context, tx *sql.Tx, thread int64) ([]PullRow, error) {
	rows, err := tx.QueryContext(ctx, `SELECT `+pullColumns+` FROM pull_requests WHERE thread_id = ? ORDER BY id`, thread)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []PullRow
	for rows.Next() {
		p, err := scanPull(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
