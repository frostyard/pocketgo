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

// ErrPullReplaced is a read of repository#number that GitHub answered with
// another pull request than the one linked: the repository was renamed or
// deleted, and its name reused.
var ErrPullReplaced = errors.New("GitHub answered with another pull request than the one linked")

// PullRow is a pull request linked to a thread, as pocketd last read it on
// GitHub.
type PullRow struct {
	ID int64
	// GitHubID is GitHub's own id of the pull request; ReadSeq the order of
	// the read that recorded it (NextPullRead).
	GitHubID   int64
	ReadSeq    int64
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
	// Ready is whether the pull request has ready-for-human, as GitHub
	// last said or pocketd last wrote; Checks CI's state at its head when
	// pocketd last read it, "" before (ADR-0017).
	Ready     bool
	Checks    string
	CheckedAt time.Time
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Ref is the pull request as repository#number.
func (p PullRow) Ref() string { return p.Repository + "#" + strconv.FormatInt(p.Number, 10) }

// pullRef is the unique key of a pull request: GitHub's names ignore case.
func pullRef(repository string, number int64) string {
	return strings.ToLower(repository) + "#" + strconv.FormatInt(number, 10)
}

const pullColumns = `id, github_id, read_seq, thread_id, repository, number, title, url, state, draft, head, head_sha, base, ready, checks,
	checked_at, created_at, updated_at`

func scanPull(scan func(...any) error) (PullRow, error) {
	var p PullRow
	var draft, ready int
	var checked, created, updated int64
	err := scan(&p.ID, &p.GitHubID, &p.ReadSeq, &p.ThreadID, &p.Repository, &p.Number, &p.Title, &p.URL, &p.State, &draft, &p.Head, &p.HeadSHA, &p.Base,
		&ready, &p.Checks, &checked, &created, &updated)
	p.Draft, p.Ready = draft != 0, ready != 0
	p.CheckedAt, p.CreatedAt, p.UpdatedAt = fromMillis(checked), fromMillis(created), fromMillis(updated)
	return p, err
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// NextPullRead is the order of a read of GitHub about to begin: each is
// higher than every one before, whatever the clock does.
func (s *Store) NextPullRead(ctx context.Context) (int64, error) {
	var seq int64
	err := s.db.QueryRowContext(ctx, `UPDATE pull_reads SET seq = seq + 1 WHERE id = 1 RETURNING seq`).Scan(&seq)
	return seq, err
}

// linkPull links p to thread in tx, or brings the row it has up to date:
// one another thread links is ErrPullTaken, and another pull request at
// the linked one's name ErrPullReplaced. p.ReadSeq orders its read: an
// older read than the row's changes nothing.
func linkPull(ctx context.Context, tx *sql.Tx, now, thread int64, p PullRow) (PullRow, error) {
	ref := pullRef(p.Repository, p.Number)
	cur, err := scanPull(tx.QueryRowContext(ctx, `SELECT `+pullColumns+` FROM pull_requests WHERE ref = ?`, ref).Scan)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if _, err := tx.ExecContext(ctx, `INSERT INTO pull_requests (ref, github_id, read_seq, thread_id, repository, number, title, url, state, draft,
			head, head_sha, base, ready, checked_at, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			ref, p.GitHubID, p.ReadSeq, thread, p.Repository, p.Number, p.Title, p.URL, p.State, boolInt(p.Draft), p.Head, p.HeadSHA, p.Base,
			boolInt(p.Ready), seenAt(p, now), now, now); err != nil {
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

// seenAt is when GitHub was read for p: its CheckedAt, else now.
func seenAt(p PullRow, now int64) int64 {
	if p.CheckedAt.IsZero() {
		return now
	}
	return toMillis(p.CheckedAt)
}

// updatePull records what GitHub said of cur (p), read in order
// p.ReadSeq: it changed when anything it says did. A read older than the
// one cur holds is stale, and changes nothing: two reads that cross never
// put an older state over a newer one, whatever the clock did between.
// Another pull request at cur's name is ErrPullReplaced.
func updatePull(ctx context.Context, tx *sql.Tx, now int64, cur, p PullRow) (bool, error) {
	if p.GitHubID != cur.GitHubID {
		return false, fmt.Errorf("%w: %s was %d, and is %d now", ErrPullReplaced, cur.Ref(), cur.GitHubID, p.GitHubID)
	}
	if p.ReadSeq < cur.ReadSeq {
		return false, nil
	}
	seen := seenAt(p, now)
	changed := cur.Title != p.Title || cur.URL != p.URL || cur.State != p.State || cur.Draft != p.Draft ||
		cur.Head != p.Head || cur.HeadSHA != p.HeadSHA || cur.Base != p.Base || cur.Repository != p.Repository || cur.Ready != p.Ready
	updated := toMillis(cur.UpdatedAt)
	if changed {
		updated = now
	}
	// CI's state was read at the head: a new head has not been read.
	checks := cur.Checks
	if cur.HeadSHA != p.HeadSHA {
		checks = ""
	}
	_, err := tx.ExecContext(ctx, `UPDATE pull_requests SET repository = ?, title = ?, url = ?, state = ?, draft = ?, head = ?, head_sha = ?,
		base = ?, ready = ?, checks = ?, read_seq = ?, checked_at = ?, updated_at = ? WHERE id = ?`,
		p.Repository, p.Title, p.URL, p.State, boolInt(p.Draft), p.Head, p.HeadSHA, p.Base, boolInt(p.Ready), checks, p.ReadSeq, seen, updated, cur.ID)
	return changed, err
}

func toMillis(t time.Time) int64 { return t.UnixMilli() }

// UpdatePull records what GitHub said of pull request id (p), read in
// order p.ReadSeq, and says whether it changed; a read older than the one
// the row holds changes nothing, and another pull request at its name is
// ErrPullReplaced.
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

// PullsToRead are the pull requests a round reads again, oldest first:
// every open one, and each closed one, not merged, of a thread not
// closed, since it may reopen and merge.
func (s *Store) PullsToRead(ctx context.Context) ([]PullRow, error) {
	return s.pulls(ctx, `SELECT `+pullColumns+` FROM pull_requests WHERE `+openPulls+` UNION
		SELECT `+pullColumns+` FROM pull_requests WHERE state = 'closed' AND thread_id IN (SELECT id FROM threads WHERE state != 'closed')
		ORDER BY id`)
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

// ThreadsMergedOut are the threads not closed whose pull requests are all
// merged or closed, one at least merged: those CloseMergedThread would
// close. A round finds them from the work database alone, so a thread
// whose last pull request merged closes even when pocketd stopped between
// the two.
func (s *Store) ThreadsMergedOut(ctx context.Context) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM threads WHERE state != 'closed'
		AND EXISTS (SELECT 1 FROM pull_requests p WHERE p.thread_id = threads.id AND p.state = 'merged')
		AND NOT EXISTS (SELECT 1 FROM pull_requests p WHERE p.thread_id = threads.id AND p.state = 'open') ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
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
