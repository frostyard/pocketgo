package store

import (
	"errors"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/frostyard/pocketgo/service/internal/team"
)

// threadsStore is a store whose clock the test moves.
func threadsStore(t *testing.T, now *time.Time) *Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "w.sqlite")
	copyTemplate(t, path)
	s, err := Open(ctx, path, func() time.Time { return *now })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func threadIDs(rows []ThreadRow) []int64 {
	out := make([]int64, len(rows))
	for i, r := range rows {
		out[i] = r.ID
	}
	return out
}

// TeamThreads reads, from one snapshot, every thread not closed and every
// closed one whose record changed in the window, the most recently
// changed first, with their shifts, the open asks, their pull requests
// with the verdict of each one's change set, the pull request a review
// reviews, the plans of the plan threads not closed, and the desk's count
// as its cap counts it (ADR-0071).
func TestTeamThreadsReadsTheBoardAndTheWindow(t *testing.T) {
	base := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	now := base
	s := threadsStore(t, &now)
	create := func(row ThreadRow) ThreadRow {
		t.Helper()
		row.SupervisorSession, row.Task = 1, "x"
		got, _, err := s.CreateThread(ctx, row)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	// Day 0: an old thread closes.
	old := create(ThreadRow{Key: "old", Title: "Old", Agent: "vera"})
	if _, err := s.CloseThread(ctx, old.ID); err != nil {
		t.Fatal(err)
	}
	// Day 5: a thread with a pull request and an ask, a shift, a plan, a
	// review of the pull request, and a thread that closes.
	now = base.Add(5 * 24 * time.Hour)
	work := create(ThreadRow{Key: "work", Title: "Fix CI", Agent: "vera"})
	if _, _, _, _, err := s.FileReportWithPulls(ctx, "r", work.ID, ThreadOpen, "One pull request.", []AskRow{{Key: "r:ask:0", SessionID: 9, Question: "Squash?"}},
		[]PullRow{{GitHubID: 7, Repository: "frostyard/example", Number: 12, Title: "Fix", URL: "u", State: PullOpen, Head: "b", HeadSHA: "abc", Base: "main"}},
		func([]int64, []PullRow) string { return "r" }); err != nil {
		t.Fatal(err)
	}
	pull, _, err := s.PullByRef(ctx, "frostyard/example", 12)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, _, err := s.AddReview(ctx, pull, ReviewRow{Key: "v", PullID: pull.ID, ThreadID: work.ID, SessionID: 9, Agent: "irulan", Provider: "openai",
		Model: "gpt", Family: "openai", HeadSHA: "abc", Verdict: "ready", Summary: "ok", BaseRef: "main"}, nil); err != nil {
		t.Fatal(err)
	}
	shift, _, err := s.CreateShiftThread(ctx, ThreadRow{Key: "shift", Title: "Shift", Agent: "vera", SupervisorSession: 1, Task: "x"}, ShiftRow{Stop: "green", MaxRounds: 5})
	if err != nil {
		t.Fatal(err)
	}
	planThread, plan := newPlan(t, s, "plan")
	review := create(ThreadRow{Key: "review", Title: "Review", Agent: "irulan", ReviewPull: pull.ID, ReviewHead: "abc"})
	gone := create(ThreadRow{Key: "gone", Title: "Gone", Agent: "duncan"})
	if _, err := s.CloseThread(ctx, gone.ID); err != nil {
		t.Fatal(err)
	}
	// Day 8: the window starts on day 1.
	now = base.Add(8 * 24 * time.Hour)
	rows, err := s.TeamThreads(ctx, now.Add(-team.InboxWindow))
	if err != nil {
		t.Fatal(err)
	}
	ids := threadIDs(rows.Threads)
	if slices.Contains(ids, old.ID) || len(ids) != 5 {
		t.Fatalf("threads %v: want every one but %d", ids, old.ID)
	}
	for _, id := range []int64{work.ID, shift.ID, planThread.ID, review.ID, gone.ID} {
		if !slices.Contains(ids, id) {
			t.Errorf("T%d is not read: %v", id, ids)
		}
	}
	if !slices.IsSortedFunc(rows.Threads, func(a, b ThreadRow) int { return b.UpdatedAt.Compare(a.UpdatedAt) }) {
		t.Errorf("not the most recently changed first: %v", ids)
	}
	if sh, ok := rows.Shifts[shift.ID]; !ok || sh.MaxRounds != 5 || len(rows.Shifts) != 1 {
		t.Errorf("shifts %+v", rows.Shifts)
	}
	if len(rows.Asks) != 1 || rows.Asks[0].ThreadID != work.ID {
		t.Errorf("open asks %+v", rows.Asks)
	}
	if len(rows.Pulls) != 1 || rows.Pulls[0].Number != 12 || rows.Reviews[pull.ID].Verdict != "ready" {
		t.Errorf("pull requests %+v, reviews %+v", rows.Pulls, rows.Reviews)
	}
	if p, ok := rows.ReviewPulls[pull.ID]; !ok || p.Ref() != "frostyard/example#12" {
		t.Errorf("the reviewed pull request %+v", rows.ReviewPulls)
	}
	if f, ok := rows.Plans[plan.ID]; !ok || f.Thread.ID != planThread.ID || len(rows.Plans) != 1 {
		t.Errorf("plans %+v", rows.Plans)
	}
	// work, shift and review are on the desk; a plan's own thread and a
	// closed one are not.
	if rows.Desk != 3 {
		t.Errorf("the desk counts %d, want 3", rows.Desk)
	}
}

// TeamThread reads one thread whole, every ask and pull request of it
// with its verdict, its shift, its plan and its sessions, with the board,
// in one snapshot; a thread that does not exist is ErrNotFound (ADR-0071).
func TestTeamThreadReadsOneThreadWhole(t *testing.T) {
	s, _ := open(t)
	for _, row := range []SessionRow{{ID: 1, Profile: "odrade", Provider: "faux", Model: "faux-1"}, {ID: 14, Profile: "vera", Provider: "faux", Model: "faux-2"}} {
		if _, err := s.CreateSession(ctx, "s"+row.Profile, row); err != nil {
			t.Fatal(err)
		}
	}
	thread, _, err := s.CreateShiftThread(ctx, ThreadRow{Key: "shift", Title: "Shift", Agent: "vera", SupervisorSession: 1, Task: "x"}, ShiftRow{Stop: "green", MaxRounds: 5})
	if err != nil {
		t.Fatal(err)
	}
	if thread, err = s.AttachThread(ctx, thread.ID, 14, "/w", "vera/b", "brief"); err != nil {
		t.Fatal(err)
	}
	_, asks, _, err := s.FileReport(ctx, "r", thread.ID, ThreadOpen, "Two questions.", []AskRow{{Key: "r:ask:0", SessionID: 14, Question: "Squash?"},
		{Key: "r:ask:1", SessionID: 14, Question: "Rebase?"}}, reportText)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.AnswerAsk(ctx, "a", asks[0].ID, "yes", "laptop"); err != nil {
		t.Fatal(err)
	}
	board, whole, err := s.TeamThread(ctx, thread.ID, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if whole.Thread.ID != thread.ID || whole.Shift == nil || whole.Shift.Stop != "green" || len(whole.Asks) != 2 || whole.Asks[0].State != AskAnswered ||
		whole.Asks[1].State != AskOpen || whole.Plan != nil || whole.ReviewPull != nil {
		t.Fatalf("the thread whole %+v", whole)
	}
	if whole.Sessions[1].Profile != "odrade" || whole.Sessions[14].Model != "faux-2" || len(whole.Sessions) != 2 {
		t.Fatalf("sessions %+v", whole.Sessions)
	}
	if !slices.Contains(threadIDs(board.Threads), thread.ID) || len(board.Asks) != 1 {
		t.Fatalf("the board %+v", board)
	}
	if _, _, err := s.TeamThread(ctx, 999, time.Time{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("no thread: %v", err)
	}
}

// The board reads the shifts of the threads it lists alone, and no round
// of any: a shift and a round of a thread closed before the window, and a
// round of a listed thread, each a row no read could scan, break neither
// list nor a read of another thread. A read reads its own thread's rounds
// (Codex review of #175, round 1).
func TestTheBoardReadsOnlyItsThreadsShifts(t *testing.T) {
	base := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	now := base
	s := threadsStore(t, &now)
	shift := func(key string) ThreadRow {
		t.Helper()
		row, _, err := s.CreateShiftThread(ctx, ThreadRow{Key: key, Title: key, Agent: "vera", SupervisorSession: 1, Task: "x"}, ShiftRow{Stop: "green", MaxRounds: 5})
		if err != nil {
			t.Fatal(err)
		}
		return row
	}
	old := shift("old")
	if _, err := s.CloseThread(ctx, old.ID); err != nil {
		t.Fatal(err)
	}
	now = base.Add(8 * 24 * time.Hour)
	listed, read := shift("listed"), shift("read")
	for _, q := range []struct {
		query string
		args  []any
	}{
		{`UPDATE shifts SET deadline = 'not a time' WHERE thread_id = ?`, []any{old.ID}},
		{`INSERT INTO shift_rounds (thread_id, n, verdict, reason, at) VALUES (?, 1, 'continue', '', 'not a time')`, []any{old.ID}},
		{`INSERT INTO shift_rounds (thread_id, n, verdict, reason, at) VALUES (?, 1, 'continue', '', 'not a time')`, []any{listed.ID}},
		{`INSERT INTO shift_rounds (thread_id, n, verdict, reason, at) VALUES (?, 1, 'continue', 'still red', ?)`, []any{read.ID, now.UnixMilli()}},
	} {
		if _, err := s.db.ExecContext(ctx, q.query, q.args...); err != nil {
			t.Fatal(err)
		}
	}
	since := now.Add(-team.InboxWindow)
	rows, err := s.TeamThreads(ctx, since)
	if err != nil {
		t.Fatalf("the list read a shift or round it does not show: %v", err)
	}
	if _, ok := rows.Shifts[old.ID]; ok || len(rows.Shifts) != 2 || len(rows.Shifts[listed.ID].Rounds) != 0 || len(rows.Shifts[read.ID].Rounds) != 0 {
		t.Fatalf("the board's shifts %+v", rows.Shifts)
	}
	_, whole, err := s.TeamThread(ctx, read.ID, since)
	if err != nil {
		t.Fatalf("a read of another thread: %v", err)
	}
	if whole.Shift == nil || len(whole.Shift.Rounds) != 1 || whole.Shift.Rounds[0].Reason != "still red" {
		t.Fatalf("the read's shift %+v", whole.Shift)
	}
}
