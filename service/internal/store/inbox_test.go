package store

import (
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// SettledAsks and SettledPulls are what team_inbox lists as settled: the
// asks answered or withdrawn, and the pull requests merged or closed, at
// the window's start or later, the most recent first, by the store's
// clock (ADR-0046). An open ask or pull request is never settled.
func TestSettledAsksAndPulls(t *testing.T) {
	base := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	now := base
	path := filepath.Join(t.TempDir(), "w.sqlite")
	copyTemplate(t, path)
	s, err := Open(ctx, path, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	thread, _, err := s.CreateThread(ctx, ThreadRow{Key: "k", Title: "Fix CI", Agent: "vera", SupervisorSession: 1, Task: "x"})
	if err != nil {
		t.Fatal(err)
	}
	ask := func(key string) AskRow {
		t.Helper()
		a, err := s.CreateAsk(ctx, AskRow{Key: key, ThreadID: thread.ID, SessionID: 9, Question: key})
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	a, b, c, _ := ask("a"), ask("b"), ask("c"), ask("open")
	var pulls []PullRow
	for n := int64(1); n <= 4; n++ {
		pulls = append(pulls, PullRow{GitHubID: 100 + n, Repository: "frostyard/example", Number: n, Title: "t", URL: "u", State: PullOpen, Head: "b", HeadSHA: "s", Base: "main"})
	}
	if _, _, _, _, err := s.FileReportWithPulls(ctx, "r", thread.ID, ThreadOpen, "r", nil, pulls, func([]int64, []PullRow) string { return "r" }); err != nil {
		t.Fatal(err)
	}
	settle := func(number int64, state string) {
		t.Helper()
		p, _, err := s.PullByRef(ctx, "frostyard/example", number)
		if err != nil {
			t.Fatal(err)
		}
		seq, err := s.NextPullRead(ctx)
		if err != nil {
			t.Fatal(err)
		}
		p.State, p.ReadSeq, p.CheckedAt = state, seq, now
		if _, changed, err := s.UpdatePull(ctx, p.ID, p); err != nil || !changed {
			t.Fatalf("#%d: %v %v", number, changed, err)
		}
	}
	// Day 0: a is answered, #2 merges.
	if _, _, err := s.AnswerAsk(ctx, "ra", a.ID, "yes", "laptop"); err != nil {
		t.Fatal(err)
	}
	settle(2, PullMerged)
	// Day 5: b is answered, #3 closes.
	now = base.Add(5 * 24 * time.Hour)
	if _, _, err := s.AnswerAsk(ctx, "rb", b.ID, "no", "laptop"); err != nil {
		t.Fatal(err)
	}
	settle(3, PullClosed)
	// Day 6: a supervisor's message withdraws c, #4 merges.
	now = base.Add(6 * 24 * time.Hour)
	if _, err := s.db.ExecContext(ctx, `UPDATE asks SET state = ?, withdrawn_by = 'send', withdrawn_at = ? WHERE id = ?`, AskWithdrawn, now.UnixMilli(), c.ID); err != nil {
		t.Fatal(err)
	}
	settle(4, PullMerged)

	for name, tc := range map[string]struct {
		since time.Time
		asks  []int64
		pulls []int64
	}{
		"from day 1":           {base.Add(24 * time.Hour), []int64{c.ID, b.ID}, []int64{4, 3}},
		"from day 5, its edge": {base.Add(5 * 24 * time.Hour), []int64{c.ID, b.ID}, []int64{4, 3}},
		"just after day 5":     {base.Add(5*24*time.Hour + time.Millisecond), []int64{c.ID}, []int64{4}},
		"from day 0":           {base, []int64{c.ID, b.ID, a.ID}, []int64{4, 3, 2}},
	} {
		asks, err := s.SettledAsks(ctx, tc.since)
		if err != nil {
			t.Fatal(err)
		}
		var askIDs []int64
		for _, row := range asks {
			askIDs = append(askIDs, row.ID)
		}
		settled, err := s.SettledPulls(ctx, tc.since)
		if err != nil {
			t.Fatal(err)
		}
		var numbers []int64
		for _, p := range settled {
			numbers = append(numbers, p.Number)
		}
		if !slices.Equal(askIDs, tc.asks) || !slices.Equal(numbers, tc.pulls) {
			t.Errorf("%s: asks %v, want %v; pull requests %v, want %v", name, askIDs, tc.asks, numbers, tc.pulls)
		}
	}
	if asks, _ := s.SettledAsks(ctx, base); asks[0].WithdrawnAt == nil || !asks[0].WithdrawnAt.Equal(base.Add(6*24*time.Hour)) || asks[1].AnsweredAt == nil {
		t.Fatalf("settled asks %+v", asks)
	}
	// A pull request merged on day 0 whose title a read changes on day 7
	// is listed from day 1: its record changed then, and pocketd keeps no
	// time of the merge (Codex review of #91, round 1).
	now = base.Add(7 * 24 * time.Hour)
	p, _, err := s.PullByRef(ctx, "frostyard/example", 2)
	if err != nil {
		t.Fatal(err)
	}
	if p.ReadSeq, err = s.NextPullRead(ctx); err != nil {
		t.Fatal(err)
	}
	p.Title = "renamed"
	if _, changed, err := s.UpdatePull(ctx, p.ID, p); err != nil || !changed {
		t.Fatalf("the rename: %v %v", changed, err)
	}
	if settled, _ := s.SettledPulls(ctx, base.Add(24*time.Hour)); len(settled) != 3 || settled[0].Number != 2 || !settled[0].UpdatedAt.Equal(now) {
		t.Fatalf("after the rename: %+v", settled)
	}
}

// Inbox reads in one snapshot: an ask answered and a pull request merged
// while it reads, between its reads of what waits and of what was
// settled, are in neither list of that read, never in both, and in the
// settled lists of the next (Codex review of #91, round 1).
func TestInboxReadsOneSnapshot(t *testing.T) {
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "w.sqlite")
	copyTemplate(t, path)
	s, err := Open(ctx, path, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	thread, _, err := s.CreateThread(ctx, ThreadRow{Key: "k", Title: "Fix CI", Agent: "vera", SupervisorSession: 1, Task: "x"})
	if err != nil {
		t.Fatal(err)
	}
	ask, err := s.CreateAsk(ctx, AskRow{Key: "a", ThreadID: thread.ID, SessionID: 9, Question: "Squash?"})
	if err != nil {
		t.Fatal(err)
	}
	pull := PullRow{GitHubID: 101, Repository: "frostyard/example", Number: 1, Title: "t", URL: "u", State: PullOpen, Head: "b", HeadSHA: "s", Base: "main"}
	if _, _, _, _, err := s.FileReportWithPulls(ctx, "r", thread.ID, ThreadOpen, "r", nil, []PullRow{pull}, func([]int64, []PullRow) string { return "r" }); err != nil {
		t.Fatal(err)
	}
	since := now.Add(-time.Hour)
	now = now.Add(time.Minute)
	s.inboxRead = func() {
		s.inboxRead = nil
		if _, _, err := s.AnswerAsk(ctx, "answer", ask.ID, "yes", "laptop"); err != nil {
			t.Error(err)
		}
		p, _, err := s.PullByRef(ctx, "frostyard/example", 1)
		if err != nil {
			t.Error(err)
			return
		}
		if p.ReadSeq, err = s.NextPullRead(ctx); err != nil {
			t.Error(err)
		}
		p.State = PullMerged
		if _, _, err := s.UpdatePull(ctx, p.ID, p); err != nil {
			t.Error(err)
		}
	}
	during, err := s.Inbox(ctx, since)
	if err != nil {
		t.Fatal(err)
	}
	if len(during.Asks) != 1 || len(during.Answered) != 0 || len(during.Pulls) != 1 || during.Pulls[0].State != PullOpen || len(during.Closed) != 0 {
		t.Fatalf("the read the changes crossed: asks %+v, answered %+v, pulls %+v, closed %+v", during.Asks, during.Answered, during.Pulls, during.Closed)
	}
	if during.Threads[thread.ID].Title != "Fix CI" {
		t.Fatalf("threads %+v", during.Threads)
	}
	after, err := s.Inbox(ctx, since)
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Asks) != 0 || len(after.Answered) != 1 || after.Pulls[0].State != PullMerged || len(after.Closed) != 1 {
		t.Fatalf("the next read: asks %+v, answered %+v, pulls %+v, closed %+v", after.Asks, after.Answered, after.Pulls, after.Closed)
	}
}
