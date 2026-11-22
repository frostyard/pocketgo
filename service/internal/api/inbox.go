package api

import (
	"context"
	"errors"
	"maps"
	"slices"
	"time"

	"github.com/frostyard/pocketgo/service/internal/store"
	"github.com/frostyard/pocketgo/service/internal/team"
)

// TeamInbox is team_inbox (ADR-0046): what waits for the owner now, as the
// desktop's Inbox counts it, and what was settled in the last
// team.InboxWindow, for a supervisor's session alone, which roleTools
// offers it and this checks again (team.CheckInboxCaller). It reads
// pocketd's records alone, with the store queries the Inbox's lists are
// answered from, and runs no gh: each pull request says when pocketd last
// read it on GitHub. It writes nothing, so a call run again reads again.
func (s *Server) TeamInbox(ctx context.Context, call TeamCall) (string, error) {
	caller, err := s.callerProfile(ctx, call.Session)
	if err != nil {
		return "", err
	}
	if err := team.CheckInboxCaller(caller); err != nil {
		return "", err
	}
	in, err := s.inbox(ctx, s.now())
	if err != nil {
		return "", err
	}
	return team.InboxText(in), nil
}

// inbox reads what team_inbox shows at now: the lists of GET /v1/asks,
// /v1/pulls, /v1/plans, /v1/lessons and /v1/approvals, the asks and pull
// requests settled since team.InboxSince(now), and the threads they name.
func (s *Server) inbox(ctx context.Context, now time.Time) (team.Inbox, error) {
	in := team.Inbox{Now: now, Zone: s.zone, Threads: map[int64]team.InboxThread{}}
	s.sessionMu.Lock()
	in.PullsStarted, in.PullEvery = s.pullsStarted, s.pullEvery
	s.sessionMu.Unlock()
	threads := map[int64]bool{}

	asks, err := s.store.OpenAsks(ctx)
	if err != nil {
		return team.Inbox{}, err
	}
	for _, row := range asks {
		in.Asks = append(in.Asks, wireAsk(row))
		threads[row.ThreadID] = true
	}
	// Each pull request with its verdict as the database held them
	// together, as GET /v1/pulls lists them.
	pulls, reviews, err := s.store.PullsWithReviews(ctx)
	if err != nil {
		return team.Inbox{}, err
	}
	for _, row := range pulls {
		pull := wireRow(row)
		if review, ok := reviews[row.ID]; ok {
			pull = withReview(pull, review)
		}
		in.Pulls = append(in.Pulls, pull)
		threads[row.ThreadID] = true
	}
	plans, err := s.store.Plans(ctx)
	if err != nil {
		return team.Inbox{}, err
	}
	for _, facts := range plans {
		in.Plans = append(in.Plans, wirePlan(facts))
		threads[facts.Plan.ThreadID] = true
	}
	lessons, err := s.store.LiveLessons(ctx)
	if err != nil {
		return team.Inbox{}, err
	}
	for _, row := range lessons {
		in.Lessons = append(in.Lessons, wireLesson(row))
	}
	if s.approvals != nil {
		if in.Approvals, err = s.approvals.Pending(ctx); err != nil {
			return team.Inbox{}, err
		}
	}

	since := team.InboxSince(now)
	answered, err := s.store.SettledAsks(ctx, since)
	if err != nil {
		return team.Inbox{}, err
	}
	for _, row := range answered {
		in.Answered = append(in.Answered, team.InboxAsk{Ask: wireAsk(row), WithdrawnAt: row.WithdrawnAt})
		threads[row.ThreadID] = true
	}
	closed, err := s.store.SettledPulls(ctx, since)
	if err != nil {
		return team.Inbox{}, err
	}
	for _, row := range closed {
		in.Closed = append(in.Closed, wireRow(row))
		threads[row.ThreadID] = true
	}

	for _, id := range slices.Sorted(maps.Keys(threads)) {
		t, err := s.store.Thread(ctx, id)
		if errors.Is(err, store.ErrNotFound) {
			continue
		} else if err != nil {
			return team.Inbox{}, err
		}
		in.Threads[id] = team.InboxThread{Title: t.Title, Agent: t.Agent}
	}
	return in, nil
}
