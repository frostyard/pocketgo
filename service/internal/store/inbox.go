package store

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"time"
)

// InboxRows are what team_inbox reads (ADR-0046): the lists GET /v1/asks,
// /v1/pulls, /v1/plans, /v1/lessons and /v1/approvals answer from, what
// was settled since the window's start, and the threads they name.
type InboxRows struct {
	// Asks are the open asks, oldest first (OpenAsks); Pulls and Reviews
	// the listed pull requests with the newest verdict of each's change
	// set (PullsWithReviews); Plans every plan (Plans); Lessons those not
	// gone (LiveLessons); Approvals the pending ones (PendingApprovals).
	Asks      []AskRow
	Pulls     []PullRow
	Reviews   map[int64]ReviewRow
	Plans     []PlanFacts
	Lessons   []LessonRow
	Approvals []ApprovalRow
	// Answered and Closed are SettledAsks and SettledPulls.
	Answered []AskRow
	Closed   []PullRow
	// Threads are the threads the rows name, by id.
	Threads map[int64]ThreadRow
}

// Inbox reads InboxRows in one read transaction, which sees the database
// as one commit left it: an answer, a merge or a close recorded while it
// reads is in none of its rows or in all of them, so an ask or a pull
// request is never both waiting and settled in one answer (Codex review
// of #91, round 1).
func (s *Store) Inbox(ctx context.Context, since time.Time) (InboxRows, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return InboxRows{}, err
	}
	defer func() { _ = tx.Rollback() }()
	var in InboxRows
	if in.Asks, err = openAsksIn(ctx, tx); err != nil {
		return InboxRows{}, err
	}
	if in.Pulls, in.Reviews, err = pullsWithReviewsIn(ctx, tx); err != nil {
		return InboxRows{}, err
	}
	if in.Plans, err = plansIn(ctx, tx); err != nil {
		return InboxRows{}, err
	}
	if in.Lessons, err = lessonsIn(ctx, tx, liveLessons); err != nil {
		return InboxRows{}, err
	}
	if in.Approvals, err = pendingApprovalsIn(ctx, tx); err != nil {
		return InboxRows{}, err
	}
	if s.inboxRead != nil {
		s.inboxRead()
	}
	if in.Answered, err = settledAsksIn(ctx, tx, since); err != nil {
		return InboxRows{}, err
	}
	if in.Closed, err = settledPullsIn(ctx, tx, since); err != nil {
		return InboxRows{}, err
	}
	ids := map[int64]bool{}
	for _, a := range slices.Concat(in.Asks, in.Answered) {
		ids[a.ThreadID] = true
	}
	for _, p := range slices.Concat(in.Pulls, in.Closed) {
		ids[p.ThreadID] = true
	}
	for _, f := range in.Plans {
		ids[f.Plan.ThreadID] = true
	}
	in.Threads = map[int64]ThreadRow{}
	for id := range ids {
		t, err := scanThread(tx.QueryRowContext(ctx, `SELECT `+threadColumns+` FROM threads WHERE id = ?`, id).Scan)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		} else if err != nil {
			return InboxRows{}, err
		}
		in.Threads[id] = t
	}
	return in, nil
}
