package api

import (
	"context"
	"time"

	"github.com/frostyard/pocketgo/service/internal/approvals"
	"github.com/frostyard/pocketgo/service/internal/team"
)

// TeamInbox is team_inbox (ADR-0046): what waits for the owner now, as the
// desktop's Inbox counts it, and what was settled in the last
// team.InboxWindow, for a supervisor's session alone, which roleTools
// offers it and this checks again (team.CheckInboxCaller). It reads
// pocketd's records alone, with the queries the Inbox's lists are answered
// from, in one read transaction (store.Inbox), and runs no gh: each pull
// request says when pocketd last read it on GitHub. It writes nothing, so
// a call run again reads again.
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
// requests settled since team.InboxSince(now), and the threads they name,
// all from one snapshot of the work database.
func (s *Server) inbox(ctx context.Context, now time.Time) (team.Inbox, error) {
	rows, err := s.store.Inbox(ctx, team.InboxSince(now))
	if err != nil {
		return team.Inbox{}, err
	}
	in := team.Inbox{Now: now, Zone: s.zone, Threads: map[int64]team.InboxThread{}}
	s.sessionMu.Lock()
	in.PullsStarted, in.PullEvery = s.pullsStarted, s.pullEvery
	s.sessionMu.Unlock()
	for _, row := range rows.Asks {
		in.Asks = append(in.Asks, wireAsk(row))
	}
	// Each pull request with its verdict as the database held them
	// together, as GET /v1/pulls lists them.
	for _, row := range rows.Pulls {
		pull := wireRow(row)
		if review, ok := rows.Reviews[row.ID]; ok {
			pull = withReview(pull, review)
		}
		in.Pulls = append(in.Pulls, pull)
	}
	for _, facts := range rows.Plans {
		in.Plans = append(in.Plans, wirePlan(facts))
	}
	for _, row := range rows.Lessons {
		in.Lessons = append(in.Lessons, wireLesson(row))
	}
	for _, row := range rows.Approvals {
		in.Approvals = append(in.Approvals, approvals.Wire(row))
	}
	for _, row := range rows.Questions {
		in.Questions = append(in.Questions, team.InboxQuestion{Question: approvals.WireQuestion(row.QuestionRow), Profile: row.Profile})
	}
	for _, row := range rows.Answered {
		in.Answered = append(in.Answered, team.InboxAsk{Ask: wireAsk(row), WithdrawnAt: row.WithdrawnAt})
	}
	for _, row := range rows.Closed {
		in.Closed = append(in.Closed, wireRow(row))
	}
	for id, t := range rows.Threads {
		in.Threads[id] = team.InboxThread{Title: t.Title, Agent: t.Agent}
	}
	return in, nil
}
