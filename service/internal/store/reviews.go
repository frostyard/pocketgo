package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// ReviewRow is a cross-review verdict on a linked pull request, at the
// head its reviewer read (ADR-0017).
type ReviewRow struct {
	ID int64
	// Key is the team call that filed it.
	Key       string
	PullID    int64
	ThreadID  int64
	SessionID int64
	Agent     string
	Provider  string
	Model     string
	Family    string
	HeadSHA   string
	Verdict   string
	Summary   string
	Findings  []string
	CreatedAt time.Time
}

const reviewColumns = `id, key, pull_id, thread_id, session_id, agent, provider, model, family, head_sha, verdict, summary, findings, created_at`

func scanReview(scan func(...any) error) (ReviewRow, error) {
	var r ReviewRow
	var findings string
	var created int64
	err := scan(&r.ID, &r.Key, &r.PullID, &r.ThreadID, &r.SessionID, &r.Agent, &r.Provider, &r.Model, &r.Family, &r.HeadSHA, &r.Verdict,
		&r.Summary, &findings, &created)
	if err != nil {
		return ReviewRow{}, err
	}
	if err := json.Unmarshal([]byte(findings), &r.Findings); err != nil {
		return ReviewRow{}, err
	}
	r.CreatedAt = fromMillis(created)
	return r, nil
}

// ReviewCall is the review the team call key filed, if it filed one.
func (s *Store) ReviewCall(ctx context.Context, key string) (ReviewRow, bool, error) {
	r, err := scanReview(s.db.QueryRowContext(ctx, `SELECT `+reviewColumns+` FROM reviews WHERE key = ?`, key).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return ReviewRow{}, false, nil
	}
	return r, err == nil, err
}

// AddReview records review r, with the read of its pull request p that
// checked its head, in one transaction: the pull request as read, then the
// review. A key that filed a review already returns that one, replayed,
// and records nothing. A reviewer's thread that is closed by now files no
// review: ErrConflict.
func (s *Store) AddReview(ctx context.Context, p PullRow, r ReviewRow) (ReviewRow, PullRow, bool, bool, error) {
	now := s.millis()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ReviewRow{}, PullRow{}, false, false, err
	}
	defer func() { _ = tx.Rollback() }()
	if prior, err := scanReview(tx.QueryRowContext(ctx, `SELECT `+reviewColumns+` FROM reviews WHERE key = ?`, r.Key).Scan); err == nil {
		row, err := scanPull(tx.QueryRowContext(ctx, `SELECT `+pullColumns+` FROM pull_requests WHERE id = ?`, prior.PullID).Scan)
		return prior, row, false, true, err
	} else if !errors.Is(err, sql.ErrNoRows) {
		return ReviewRow{}, PullRow{}, false, false, err
	}
	var state string
	if err := tx.QueryRowContext(ctx, `SELECT state FROM threads WHERE id = ?`, r.ThreadID).Scan(&state); errors.Is(err, sql.ErrNoRows) {
		return ReviewRow{}, PullRow{}, false, false, ErrNotFound
	} else if err != nil {
		return ReviewRow{}, PullRow{}, false, false, err
	}
	if state == ThreadClosed {
		return ReviewRow{}, PullRow{}, false, false, ErrConflict
	}
	cur, err := scanPull(tx.QueryRowContext(ctx, `SELECT `+pullColumns+` FROM pull_requests WHERE id = ?`, r.PullID).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return ReviewRow{}, PullRow{}, false, false, ErrNotFound
	} else if err != nil {
		return ReviewRow{}, PullRow{}, false, false, err
	}
	changed, err := updatePull(ctx, tx, now, cur, p)
	if err != nil {
		return ReviewRow{}, PullRow{}, false, false, err
	}
	findings, err := json.Marshal(nonNil(r.Findings))
	if err != nil {
		return ReviewRow{}, PullRow{}, false, false, err
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO reviews (key, pull_id, thread_id, session_id, agent, provider, model, family, head_sha, verdict,
		summary, findings, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.Key, r.PullID, r.ThreadID, r.SessionID, r.Agent, r.Provider, r.Model, r.Family, r.HeadSHA, r.Verdict, r.Summary, string(findings), now)
	if err != nil {
		return ReviewRow{}, PullRow{}, false, false, err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return ReviewRow{}, PullRow{}, false, false, err
	}
	review, err := scanReview(tx.QueryRowContext(ctx, `SELECT `+reviewColumns+` FROM reviews WHERE id = ?`, id).Scan)
	if err != nil {
		return ReviewRow{}, PullRow{}, false, false, err
	}
	row, err := scanPull(tx.QueryRowContext(ctx, `SELECT `+pullColumns+` FROM pull_requests WHERE id = ?`, r.PullID).Scan)
	if err != nil {
		return ReviewRow{}, PullRow{}, false, false, err
	}
	return review, row, changed, false, tx.Commit()
}

func nonNil(list []string) []string {
	if list == nil {
		return []string{}
	}
	return list
}

// LatestReview is the newest review of pull request pull at head, if one
// was filed: the verdict that decides its label (ADR-0017).
func (s *Store) LatestReview(ctx context.Context, pull int64, head string) (ReviewRow, bool, error) {
	r, err := scanReview(s.db.QueryRowContext(ctx, `SELECT `+reviewColumns+` FROM reviews WHERE pull_id = ? AND head_sha = ? ORDER BY id DESC LIMIT 1`,
		pull, head).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return ReviewRow{}, false, nil
	}
	return r, err == nil, err
}

// PullByRef is the linked pull request repository#number, if one is.
func (s *Store) PullByRef(ctx context.Context, repository string, number int64) (PullRow, bool, error) {
	p, err := scanPull(s.db.QueryRowContext(ctx, `SELECT `+pullColumns+` FROM pull_requests WHERE ref = ?`, pullRef(repository, number)).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return PullRow{}, false, nil
	}
	return p, err == nil, err
}

// SetPullChecks records CI's state at head, as pocketd just read it, for
// pull request id; SetPullReady whether it has ready-for-human there, as
// pocketd just wrote it. Each keeps the other's last observation. A head
// that moved since changes nothing: the next round decides for the new
// one. Each says whether the row changed.
func (s *Store) SetPullChecks(ctx context.Context, id int64, head, checks string) (PullRow, bool, error) {
	return s.setPullGate(ctx, id, head, `checks = ?`, checks, func(p PullRow) bool { return p.Checks == checks })
}

// SetPullReady: see SetPullChecks.
func (s *Store) SetPullReady(ctx context.Context, id int64, head string, ready bool) (PullRow, bool, error) {
	return s.setPullGate(ctx, id, head, `ready = ?`, boolInt(ready), func(p PullRow) bool { return p.Ready == ready })
}

func (s *Store) setPullGate(ctx context.Context, id int64, head, set string, value any, same func(PullRow) bool) (PullRow, bool, error) {
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
	if cur.HeadSHA != head || same(cur) {
		return cur, false, nil
	}
	if _, err := tx.ExecContext(ctx, `UPDATE pull_requests SET `+set+`, updated_at = ? WHERE id = ?`, value, now, id); err != nil {
		return PullRow{}, false, err
	}
	row, err := scanPull(tx.QueryRowContext(ctx, `SELECT `+pullColumns+` FROM pull_requests WHERE id = ?`, id).Scan)
	if err != nil {
		return PullRow{}, false, err
	}
	return row, true, tx.Commit()
}

// PullByID is linked pull request id.
func (s *Store) PullByID(ctx context.Context, id int64) (PullRow, error) {
	p, err := scanPull(s.db.QueryRowContext(ctx, `SELECT `+pullColumns+` FROM pull_requests WHERE id = ?`, id).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return PullRow{}, ErrNotFound
	}
	return p, err
}

// PullsWithReviews are ListPulls' pull requests, each with the newest
// review at its head when it has one, read in one transaction: a pull
// request and its verdict as the database held them together.
func (s *Store) PullsWithReviews(ctx context.Context) ([]PullRow, map[int64]ReviewRow, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := tx.QueryContext(ctx, `SELECT `+pullColumns+` FROM pull_requests WHERE `+openPulls+` UNION
		SELECT `+pullColumns+` FROM pull_requests WHERE thread_id IN (SELECT id FROM threads WHERE state != 'closed') ORDER BY id`)
	if err != nil {
		return nil, nil, err
	}
	var pulls []PullRow
	for rows.Next() {
		p, err := scanPull(rows.Scan)
		if err != nil {
			_ = rows.Close()
			return nil, nil, err
		}
		pulls = append(pulls, p)
	}
	if err := rows.Close(); err != nil {
		return nil, nil, err
	}
	reviews := map[int64]ReviewRow{}
	for _, p := range pulls {
		r, err := scanReview(tx.QueryRowContext(ctx, `SELECT `+reviewColumns+` FROM reviews WHERE pull_id = ? AND head_sha = ? ORDER BY id DESC LIMIT 1`,
			p.ID, p.HeadSHA).Scan)
		switch {
		case errors.Is(err, sql.ErrNoRows):
		case err != nil:
			return nil, nil, err
		default:
			reviews[p.ID] = r
		}
	}
	return pulls, reviews, nil
}
