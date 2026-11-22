package store

import (
	"errors"
	"testing"
)

// A close drops, in its own transaction, each of the thread's messages
// that go only while it is open and are not placed: a stop right after
// the close leaves none for a reopen to send. Its other messages stay,
// and a close of a closed thread drops nothing (Codex review of #121,
// round 1).
func TestACloseDropsItsMessagesInItsTransaction(t *testing.T) {
	s, _ := open(t)
	row := openedThread(t, s, "k1", 9)
	for _, key := range []string{"worker-reply:a", "worker-msg:b", "shift-round:c:1", "team-send:1:d", "review-verdict:e"} {
		if _, err := s.db.ExecContext(ctx, `INSERT INTO reports (key, thread_id, session_id, text, created_at) VALUES (?, ?, ?, ?, ?)`, key, row.ID, 9, "x", 1); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.CloseThread(ctx, row.ID); err != nil {
		t.Fatal(err)
	}
	unsent, err := s.UnsentReportsOf(ctx, row.ID)
	if err != nil || len(unsent) != 1 || unsent[0].Key != "review-verdict:e" {
		t.Fatalf("unsent after the close: %+v, %v", unsent, err)
	}
	// Kept while it was closed, a message that would reopen it outlives a
	// close of the closed thread.
	if _, err := s.FileSendTo(ctx, ReportRow{Key: "team-send:1:f", ThreadID: row.ID, SessionID: 9, Text: "x"}, true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CloseThread(ctx, row.ID); err != nil {
		t.Fatal(err)
	}
	if kept, err := s.Report(ctx, "team-send:1:f"); err != nil || kept.SentAt != nil {
		t.Fatalf("a close of the closed thread dropped it: %+v, %v", kept, err)
	}
}

// A thread a message reopened frees its desk place again at its next
// close: a plan the cap held back is told each time, by a note of its own
// (Codex review of #121, round 2).
func TestEachCloseOfAReopenedThreadFreesAHeldPlansPlace(t *testing.T) {
	s, _ := open(t)
	_, facts := approvedPlan(t, s, "p1")
	row := openedThread(t, s, "k1", 9)
	for round := range 2 {
		if err := s.HoldPlan(ctx, facts.Plan.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := s.CloseThread(ctx, row.ID); err != nil {
			t.Fatal(err)
		}
		key := "plan:" + facts.Thread.Owner + ":free:" + row.Owner
		if round > 0 {
			key += ":1"
		}
		if note, err := s.Note(ctx, key); err != nil || note.Text == "" {
			t.Fatalf("close %d: no note %q: %+v, %v", round+1, key, note, err)
		}
		if err := s.CompleteOwnerMessage(ctx, "u:d:m"+string(rune('1'+round)), "100", row.ID, 9); err != nil {
			t.Fatal(err)
		}
		if _, reopened, err := s.OwnerMessagePlaced(ctx, "u:d:m"+string(rune('1'+round))); err != nil || !reopened {
			t.Fatalf("reopen %d: %v, %v", round+1, reopened, err)
		}
	}
}

// openedThread records a thread whose specialist works in session, opened.
func openedThread(t *testing.T, s *Store, key string, session int64) ThreadRow {
	t.Helper()
	row, _, err := s.CreateThread(ctx, ThreadRow{Key: key, Title: "t", Agent: "lucilla", SupervisorSession: 1, Task: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AttachThread(ctx, row.ID, session, "/home/lucilla", "", "the brief"); err != nil {
		t.Fatal(err)
	}
	opened, err := s.OpenThread(ctx, row.ID)
	if err != nil || opened.State != ThreadOpen {
		t.Fatalf("open %+v %v", opened, err)
	}
	return opened
}

// The owner's message, followed once PiG took it, reopens its closed
// thread once when noted placed, and counts the merges it had then; a
// message noted again, or dropped, changes nothing, and a plan's thread
// stays closed (ADR-0061).
func TestTheOwnersMessageReopensItsClosedThreadOnce(t *testing.T) {
	s, _ := open(t)
	row := openedThread(t, s, "k1", 9)
	if _, _, _, _, err := s.FileReportWithPulls(ctx, "r1", row.ID, ThreadDone, "m", nil, []PullRow{pull("frostyard/x", 3, PullMerged)}, pullReportText); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CloseThread(ctx, row.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ClaimRequest(ctx, "u:d:m1", "send:9"); err != nil {
		t.Fatal(err)
	}
	if err := s.CompleteOwnerMessage(ctx, "u:d:m1", "100", row.ID, 9); err != nil {
		t.Fatal(err)
	}
	if result, _, err := s.LookupRequest(ctx, "u:d:m1", "send:9"); err != nil || result != "100" {
		t.Fatalf("the request %q %v", result, err)
	}
	if left, err := s.OwnerMessagesOf(ctx, row.ID); err != nil || len(left) != 1 || left[0].SessionID != 9 {
		t.Fatalf("followed %+v %v", left, err)
	}
	if waiting, err := s.MessagesToReopen(ctx, row.ID); err != nil || !waiting {
		t.Fatalf("waiting %v %v", waiting, err)
	}
	got, reopened, err := s.OwnerMessagePlaced(ctx, "u:d:m1")
	if err != nil || !reopened || got.State != ThreadOpen || got.Reopened != 1 || got.ReopenedAt == nil || got.MergedAtReopen != 1 {
		t.Fatalf("noted %+v %v %v", got, reopened, err)
	}
	if again, reopened, err := s.OwnerMessagePlaced(ctx, "u:d:m1"); err != nil || reopened || again.ID != 0 {
		t.Fatalf("noted again %+v %v %v", again, reopened, err)
	}
	if waiting, _ := s.MessagesToReopen(ctx, row.ID); waiting {
		t.Fatal("a noted message still waits")
	}
	// Its merged pull request does not close it again.
	if ids, err := s.ThreadsMergedOut(ctx); err != nil || len(ids) != 0 {
		t.Fatalf("merged out %v %v", ids, err)
	}
	if _, closed, err := s.CloseMergedThread(ctx, row.ID, func([]PullRow) string { return "merged" }); err != nil || closed {
		t.Fatalf("closed by its old merge: %v %v", closed, err)
	}
	// A dropped message reopens nothing.
	if _, err := s.CloseThread(ctx, row.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.CompleteOwnerMessage(ctx, "u:d:m2", "101", row.ID, 9); err != nil {
		t.Fatal(err)
	}
	if err := s.DropOwnerMessage(ctx, "u:d:m2"); err != nil {
		t.Fatal(err)
	}
	if _, reopened, _ := s.OwnerMessagePlaced(ctx, "u:d:m2"); reopened {
		t.Fatal("a dropped message reopened the thread")
	}
	// A plan's thread stays closed.
	plan, _, err := s.CreateThread(ctx, ThreadRow{Key: "k2", Title: "t", Agent: "lucilla", SupervisorSession: 1, Task: "x", Plan: 4, PlanTask: "t1"})
	if err != nil {
		t.Fatal(err)
	}
	// Closed as its plan closes it, without the plan this test has none of.
	if _, err := s.db.ExecContext(ctx, `UPDATE threads SET state = ? WHERE id = ?`, ThreadClosed, plan.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.CompleteOwnerMessage(ctx, "u:d:m3", "102", plan.ID, 10); err != nil {
		t.Fatal(err)
	}
	if got, reopened, err := s.OwnerMessagePlaced(ctx, "u:d:m3"); err != nil || reopened || got.State != ThreadClosed {
		t.Fatalf("a plan's thread %+v %v %v", got, reopened, err)
	}
	// A message to a session that is no thread's follows nothing.
	if err := s.CompleteOwnerMessage(ctx, "u:d:m4", "103", 0, 11); err != nil {
		t.Fatal(err)
	}
	if threads, _ := s.ThreadsWithOwnerMessages(ctx); len(threads) != 0 {
		t.Fatalf("followed %v", threads)
	}
}

// A reopened thread closes by its pull requests once one more merged than
// had at its reopen (ADR-0061).
func TestAReopenedThreadClosesOnANewMerge(t *testing.T) {
	s, _ := open(t)
	row := openedThread(t, s, "k1", 9)
	if _, _, _, _, err := s.FileReportWithPulls(ctx, "r1", row.ID, ThreadDone, "m", nil, []PullRow{pull("frostyard/x", 3, PullMerged)}, pullReportText); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CloseThread(ctx, row.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.CompleteOwnerMessage(ctx, "u:d:m1", "100", row.ID, 9); err != nil {
		t.Fatal(err)
	}
	if _, reopened, err := s.OwnerMessagePlaced(ctx, "u:d:m1"); err != nil || !reopened {
		t.Fatalf("reopen %v %v", reopened, err)
	}
	if _, _, _, _, err := s.FileReportWithPulls(ctx, "r2", row.ID, ThreadDone, "m", nil, []PullRow{pull("frostyard/x", 4, PullMerged)}, pullReportText); err != nil {
		t.Fatal(err)
	}
	if ids, err := s.ThreadsMergedOut(ctx); err != nil || len(ids) != 1 || ids[0] != row.ID {
		t.Fatalf("merged out %v %v", ids, err)
	}
	if got, closed, err := s.CloseMergedThread(ctx, row.ID, func([]PullRow) string { return "merged" }); err != nil || !closed || got.State != ThreadClosed {
		t.Fatalf("close %+v %v %v", got, closed, err)
	}
}

// A supervisor's message kept for a closed thread names the reopen it
// brings, says nothing of unblocking, and reopens the thread once noted;
// one kept before a reopen that a close followed reopens nothing. A text
// written for another state than the thread's is a conflict (ADR-0061).
func TestASendKeptForAClosedThreadReopensIt(t *testing.T) {
	s, _ := open(t)
	row := openedThread(t, s, "k1", 9)
	if _, err := s.CloseThread(ctx, row.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.FileSend(ctx, ReportRow{Key: "team-send:1:a", ThreadID: row.ID, SessionID: 9, Text: "x"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("an open text for a closed thread: %v", err)
	}
	kept, err := s.FileSendTo(ctx, ReportRow{Key: "team-send:1:b", ThreadID: row.ID, SessionID: 9, Text: "x"}, true)
	if err != nil || kept.Reopens != 1 || kept.Unblocks {
		t.Fatalf("kept %+v %v", kept, err)
	}
	if waiting, _ := s.MessagesToReopen(ctx, row.ID); !waiting {
		t.Fatal("the message does not wait")
	}
	got, change, err := s.SendDelivered(ctx, "team-send:1:b", row.ID)
	if err != nil || !change.Reopened || !change.FromClosed || got.State != ThreadOpen || got.Reopened != 1 {
		t.Fatalf("noted %+v %+v %v", got, change, err)
	}
	if _, err := s.FileSendTo(ctx, ReportRow{Key: "team-send:1:c", ThreadID: row.ID, SessionID: 9, Text: "x"}, true); !errors.Is(err, ErrConflict) {
		t.Fatalf("a closed text for an open thread: %v", err)
	}
	// Kept while closed, then another reopen and a close came: it reopens
	// nothing.
	if _, err := s.CloseThread(ctx, row.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.FileSendTo(ctx, ReportRow{Key: "team-send:1:d", ThreadID: row.ID, SessionID: 9, Text: "x"}, true); err != nil {
		t.Fatal(err)
	}
	if err := s.CompleteOwnerMessage(ctx, "u:d:m1", "100", row.ID, 9); err != nil {
		t.Fatal(err)
	}
	if _, reopened, err := s.OwnerMessagePlaced(ctx, "u:d:m1"); err != nil || !reopened {
		t.Fatalf("the owner's reopen %v %v", reopened, err)
	}
	if _, err := s.CloseThread(ctx, row.ID); err != nil {
		t.Fatal(err)
	}
	if got, change, err := s.SendDelivered(ctx, "team-send:1:d", row.ID); err != nil || change.Reopened || got.State != ThreadClosed {
		t.Fatalf("a stale message %+v %+v %v", got, change, err)
	}
}

// A delegation that follows a thread names it as that thread's successor,
// in the transaction that records it; the newest follower wins, and a
// retry names nothing again (ADR-0061).
func TestFollowsNamesTheSuccessorOnInsert(t *testing.T) {
	s, _ := open(t)
	old, _, _ := s.CreateThread(ctx, ThreadRow{Key: "k1", Title: "t", Agent: "lucilla", SupervisorSession: 1, Task: "x"})
	first, _, err := s.CreateThread(ctx, ThreadRow{Key: "k2", Title: "t", Agent: "murbella", SupervisorSession: 1, Task: "x", Follows: old.ID})
	if err != nil || first.Follows != old.ID {
		t.Fatalf("first %+v %v", first, err)
	}
	if got, _ := s.Thread(ctx, old.ID); got.Successor != first.ID {
		t.Fatalf("old %+v", got)
	}
	second, _, _ := s.CreateThread(ctx, ThreadRow{Key: "k3", Title: "t", Agent: "duncan", SupervisorSession: 1, Task: "x", Follows: old.ID})
	if got, _ := s.Thread(ctx, old.ID); got.Successor != second.ID {
		t.Fatalf("old after the second %+v", got)
	}
	// The first delegation run again names nothing.
	if _, existed, err := s.CreateThread(ctx, ThreadRow{Key: "k2", Title: "t", Agent: "murbella", SupervisorSession: 1, Task: "x", Follows: old.ID}); err != nil || !existed {
		t.Fatalf("retry %v %v", existed, err)
	}
	if got, _ := s.Thread(ctx, old.ID); got.Successor != second.ID {
		t.Fatalf("a retry renamed the successor: %+v", got)
	}
}
