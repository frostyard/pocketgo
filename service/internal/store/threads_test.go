package store

import (
	"errors"
	"slices"
	"testing"
)

func TestThreadsLifecycle(t *testing.T) {
	s, _ := open(t)
	row, existed, err := s.CreateThread(ctx, ThreadRow{Key: "delegate:1:c1", Title: "Fix CI", Agent: "vera", SupervisorSession: 1, Task: "Fix it"})
	if err != nil || existed || row.State != ThreadStarting || row.ID == 0 {
		t.Fatalf("create %+v %v %v", row, existed, err)
	}
	// The same delegation finds the same thread.
	again, existed, err := s.CreateThread(ctx, ThreadRow{Key: "delegate:1:c1", Title: "other", Agent: "x", SupervisorSession: 1, Task: "x"})
	if err != nil || !existed || again.ID != row.ID || again.Title != "Fix CI" {
		t.Fatalf("retry %+v %v %v", again, existed, err)
	}
	if n, _ := s.OpenThreads(ctx); n != 1 {
		t.Fatalf("open threads %d", n)
	}
	// A report before the thread started is refused.
	if _, err := s.ReportThread(ctx, row.ID, ThreadDone, "early"); !errors.Is(err, ErrConflict) {
		t.Fatalf("report before start: %v", err)
	}
	started, err := s.StartThread(ctx, row.ID, 9, "/w--vera-fix-ci", "vera/fix-ci")
	if err != nil || started.State != ThreadOpen || started.Session != 9 || started.Branch != "vera/fix-ci" {
		t.Fatalf("start %+v %v", started, err)
	}
	if got, err := s.ThreadOfSession(ctx, 9); err != nil || got.ID != row.ID {
		t.Fatalf("thread of session %+v %v", got, err)
	}
	if got, err := s.ReportThread(ctx, row.ID, ThreadDone, "PR #3 is open"); err != nil || got.State != ThreadDone || got.Report != "PR #3 is open" {
		t.Fatalf("report %+v %v", got, err)
	}
	if got, err := s.CloseThread(ctx, row.ID); err != nil || got.State != ThreadClosed {
		t.Fatalf("close %+v %v", got, err)
	}
	if n, _ := s.OpenThreads(ctx); n != 0 {
		t.Fatalf("a closed thread is on the desk: %d", n)
	}
	if _, err := s.ReportThread(ctx, row.ID, ThreadDone, "late"); !errors.Is(err, ErrConflict) {
		t.Fatalf("report after close: %v", err)
	}
}

// A thread that could not start leaves the desk with why; one that started
// is never failed.
func TestFailThread(t *testing.T) {
	s, _ := open(t)
	row, _, _ := s.CreateThread(ctx, ThreadRow{Key: "k1", Title: "t", Agent: "vera", SupervisorSession: 1, Task: "x"})
	if got, err := s.ThreadByKey(ctx, "k1"); err != nil || got.ID != row.ID {
		t.Fatalf("by key %+v %v", got, err)
	}
	failed, err := s.FailThread(ctx, row.ID, "no checkout")
	if err != nil || failed.State != ThreadClosed || failed.Report != "no checkout" || failed.Session != 0 {
		t.Fatalf("fail %+v %v", failed, err)
	}
	other, _, _ := s.CreateThread(ctx, ThreadRow{Key: "k2", Title: "t", Agent: "vera", SupervisorSession: 1, Task: "x"})
	_, _ = s.StartThread(ctx, other.ID, 5, "/w", "")
	if got, _ := s.FailThread(ctx, other.ID, "late"); got.State != ThreadOpen || got.Report != "" {
		t.Fatalf("a started thread failed: %+v", got)
	}
	if _, err := s.ThreadByKey(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown key: %v", err)
	}
}

func TestAsksAreAnsweredOnce(t *testing.T) {
	s, _ := open(t)
	thread, _, _ := s.CreateThread(ctx, ThreadRow{Key: "k", Title: "t", Agent: "vera", SupervisorSession: 1, Task: "x"})
	ask, err := s.CreateAsk(ctx, AskRow{Key: "message:9:c2:0", ThreadID: thread.ID, SessionID: 9, Question: "Squash?", Options: []string{"yes", "no"}})
	if err != nil || ask.State != AskOpen || !slices.Equal(ask.Options, []string{"yes", "no"}) {
		t.Fatalf("create %+v %v", ask, err)
	}
	if again, _ := s.CreateAsk(ctx, AskRow{Key: "message:9:c2:0", ThreadID: thread.ID, SessionID: 9, Question: "other"}); again.ID != ask.ID {
		t.Fatalf("a retried ask made %+v", again)
	}
	if open, _ := s.OpenAsks(ctx); len(open) != 1 {
		t.Fatalf("open asks %+v", open)
	}
	answered, replayed, err := s.AnswerAsk(ctx, "r1", ask.ID, "yes", "laptop")
	if err != nil || replayed || answered.State != AskAnswered || answered.Answer != "yes" || answered.AnsweredAt == nil {
		t.Fatalf("answer %+v %v %v", answered, replayed, err)
	}
	if again, replayed, err := s.AnswerAsk(ctx, "r1", ask.ID, "yes", "laptop"); err != nil || !replayed || again.Answer != "yes" {
		t.Fatalf("replay %+v %v %v", again, replayed, err)
	}
	if _, _, err := s.AnswerAsk(ctx, "r2", ask.ID, "no", "laptop"); !errors.Is(err, ErrConflict) {
		t.Fatalf("a second answer: %v", err)
	}
	if _, _, err := s.AnswerAsk(ctx, "r3", 999, "no", "laptop"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an unknown ask: %v", err)
	}
	if open, _ := s.OpenAsks(ctx); len(open) != 0 {
		t.Fatalf("an answered ask is open: %+v", open)
	}
	// The answer waits for delivery until the asking session has it.
	if waiting, err := s.UndeliveredAnswers(ctx); err != nil || len(waiting) != 1 || waiting[0].ID != ask.ID {
		t.Fatalf("undelivered %+v %v", waiting, err)
	}
	if err := s.AnswerDelivered(ctx, ask.ID); err != nil {
		t.Fatal(err)
	}
	if waiting, _ := s.UndeliveredAnswers(ctx); len(waiting) != 0 {
		t.Fatalf("a delivered answer waits: %+v", waiting)
	}
}
