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
	attached, err := s.AttachThread(ctx, row.ID, 9, "/w--vera-fix-ci", "vera/fix-ci")
	if err != nil || attached.State != ThreadStarting || attached.Session != 9 || attached.Branch != "vera/fix-ci" {
		t.Fatalf("attach %+v %v", attached, err)
	}
	// Another session never takes it over.
	if other, _ := s.AttachThread(ctx, row.ID, 10, "/x", ""); other.Session != 9 {
		t.Fatalf("a second attach moved it to %+v", other)
	}
	if got, err := s.ThreadOfSession(ctx, 9); err != nil || got.ID != row.ID {
		t.Fatalf("thread of session %+v %v", got, err)
	}
	if opened, err := s.OpenThread(ctx, row.ID); err != nil || opened.State != ThreadOpen {
		t.Fatalf("open %+v %v", opened, err)
	}
	got, asks, err := s.FileReport(ctx, "r1", row.ID, ThreadDone, "PR #3 is open", nil)
	if err != nil || got.State != ThreadDone || got.Report != "PR #3 is open" || len(asks) != 0 {
		t.Fatalf("report %+v %v %v", got, asks, err)
	}
	if got, err := s.CloseThread(ctx, row.ID); err != nil || got.State != ThreadClosed {
		t.Fatalf("close %+v %v", got, err)
	}
	if n, _ := s.OpenThreads(ctx); n != 0 {
		t.Fatalf("a closed thread is on the desk: %d", n)
	}
	// A closed thread takes no report, and keeps no ask of one.
	if _, _, err := s.FileReport(ctx, "r2", row.ID, ThreadDone, "late", []AskRow{{Key: "r2:ask:0", SessionID: 9, Question: "q"}}); !errors.Is(err, ErrConflict) {
		t.Fatalf("report after close: %v", err)
	}
	if open, _ := s.OpenAsks(ctx); len(open) != 0 {
		t.Fatalf("a refused report left asks %+v", open)
	}
	// Nor does it open again.
	if got, _ := s.OpenThread(ctx, row.ID); got.State != ThreadClosed {
		t.Fatalf("a closed thread opened: %+v", got)
	}
}

// A report filed once is never filed again: a report run again after a
// newer one leaves the newer one, and returns the asks it made.
func TestAReportIsFiledOnce(t *testing.T) {
	s, _ := open(t)
	row, _, _ := s.CreateThread(ctx, ThreadRow{Key: "k", Title: "t", Agent: "vera", SupervisorSession: 1, Task: "x"})
	_, _ = s.AttachThread(ctx, row.ID, 9, "/w", "")
	// A report while the thread starts is taken: the brief went.
	first, asks, err := s.FileReport(ctx, "r1", row.ID, ThreadOpen, "halfway", []AskRow{{Key: "r1:ask:0", SessionID: 9, Question: "Squash?", Options: []string{"yes", "no"}}})
	if err != nil || first.State != ThreadOpen || len(asks) != 1 || asks[0].Question != "Squash?" || asks[0].ThreadID != row.ID {
		t.Fatalf("first %+v %+v %v", first, asks, err)
	}
	if _, _, err := s.FileReport(ctx, "r2", row.ID, ThreadDone, "done", nil); err != nil {
		t.Fatal(err)
	}
	again, replayed, err := s.FileReport(ctx, "r1", row.ID, ThreadOpen, "halfway", []AskRow{{Key: "r1:ask:0", SessionID: 9, Question: "Squash?"}})
	if err != nil || again.State != ThreadDone || again.Report != "done" || len(replayed) != 1 || replayed[0].ID != asks[0].ID {
		t.Fatalf("a replay %+v %+v %v", again, replayed, err)
	}
	if open, _ := s.OpenAsks(ctx); len(open) != 1 {
		t.Fatalf("asks %+v", open)
	}
	if _, _, err := s.FileReport(ctx, "r3", 99, ThreadDone, "x", nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an unknown thread: %v", err)
	}
}

// The owner's close claims its request id in the transaction that closes:
// an id used for another thread closes nothing.
func TestCloseThreadAsClaimsItsRequest(t *testing.T) {
	s, _ := open(t)
	one, _, _ := s.CreateThread(ctx, ThreadRow{Key: "k1", Title: "t", Agent: "vera", SupervisorSession: 1, Task: "x"})
	two, _, _ := s.CreateThread(ctx, ThreadRow{Key: "k2", Title: "t", Agent: "vera", SupervisorSession: 1, Task: "x"})
	if got, err := s.CloseThreadAs(ctx, "u:1", one.ID); err != nil || got.State != ThreadClosed {
		t.Fatalf("close %+v %v", got, err)
	}
	if got, err := s.CloseThreadAs(ctx, "u:1", one.ID); err != nil || got.State != ThreadClosed {
		t.Fatalf("a retry %+v %v", got, err)
	}
	if _, err := s.CloseThreadAs(ctx, "u:1", two.ID); !errors.Is(err, ErrRequestReused) {
		t.Fatalf("a reused id: %v", err)
	}
	if got, _ := s.Thread(ctx, two.ID); got.State != ThreadStarting {
		t.Fatalf("a reused id closed %+v", got)
	}
	if _, err := s.CloseThreadAs(ctx, "u:2", 99); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an unknown thread: %v", err)
	}
	if _, _, err := s.LookupRequest(ctx, "u:2", "closeThread:99"); err != nil {
		t.Fatal(err)
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
	_, _ = s.AttachThread(ctx, other.ID, 5, "/w", "")
	_, _ = s.OpenThread(ctx, other.ID)
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
