package api

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/frostyard/pocketgo/proto"
	"github.com/frostyard/pocketgo/service/internal/store"
	"github.com/frostyard/pocketgo/service/internal/team"
)

// queuedOverAClose sends the owner's message m to thread's specialist while
// it is at work, closes the thread over it, and places it in the
// transcript, with no pass to note it yet.
func queuedOverAClose(t *testing.T, h *teamHarness, thread store.ThreadRow, m string) {
	t.Helper()
	ctx := context.Background()
	h.queueOwner(thread.Session)
	if _, err := h.client.Send(ctx, thread.Session, proto.SendMessage{RequestID: m, Text: "Take Murbella's point."}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.server.TeamClose(ctx, TeamCall{Session: h.super.ID, CallID: "close-" + m}, thread.ID); err != nil {
		t.Fatal(err)
	}
	h.placeQueued(requestKeyOf(h.harness, m), thread.Session)
	h.engine.onSend = nil
}

// A cleanup reads the thread and the messages that may reopen it from one
// snapshot: a noting that reopens the thread between two reads would leave
// it closed in the first and with no message in the second, and its
// worktree would go from under the open thread (Codex review of #121,
// round 1).
func TestACleanupReadsTheThreadAndItsMessagesTogether(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	thread := veraThread(t, h)
	queuedOverAClose(t, h, thread, "m1")
	noted := false
	h.server.afterCleanState = func(id int64) {
		if id == thread.ID && !noted {
			noted = true
			if _, err := h.server.deliverAll(ctx); err != nil {
				t.Error(err)
			}
		}
	}
	h.server.reviewPass(ctx)
	if !noted {
		t.Fatal("no cleanup looked at the thread")
	}
	if got, _ := h.store.Thread(ctx, thread.ID); got.State != store.ThreadOpen {
		t.Fatalf("the thread %+v", got)
	}
	if _, err := os.Stat(thread.Cwd); err != nil {
		t.Fatalf("the reopened thread's worktree was removed: %v", err)
	}
}

// The owner's close run again with its request id answers with the thread
// as it is, and closes nothing again: a message may have reopened it
// (Codex review of #121, round 1).
func TestTheOwnersCloseRunAgainClosesNothing(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	thread := homeThread(t, h, "d1")
	if _, err := h.client.CloseThread(ctx, thread.ID, proto.CloseThread{RequestID: "c1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.client.Send(ctx, thread.Session, proto.SendMessage{RequestID: "m1", Text: "One more thing."}); err != nil {
		t.Fatal(err)
	}
	if got, _ := h.store.Thread(ctx, thread.ID); got.State != store.ThreadOpen {
		t.Fatalf("not reopened: %+v", got)
	}
	again, err := h.client.CloseThread(ctx, thread.ID, proto.CloseThread{RequestID: "c1"})
	if err != nil || again.State != proto.ThreadOpen {
		t.Fatalf("the close run again: %+v, %v", again, err)
	}
	if got, _ := h.store.Thread(ctx, thread.ID); got.State != store.ThreadOpen {
		t.Fatalf("the close run again closed the reopened thread: %+v", got)
	}
}

// The owner's message to a thread's specialist goes and is recorded under
// outboxMu, which every close holds as it notes the thread's messages: no
// close comes between its placement and its record (Codex review of #121,
// round 1).
func TestTheOwnersMessageGoesAndIsRecordedUnderTheOutboxLock(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	thread := homeThread(t, h, "d1")
	checked := false
	h.server.afterOwnerSend = func() {
		checked = true
		if h.server.outboxMu.TryLock() {
			h.server.outboxMu.Unlock()
			t.Error("outboxMu is free between the message's placement and its record")
		}
	}
	if _, err := h.client.Send(ctx, thread.Session, proto.SendMessage{RequestID: "m1", Text: "Hi."}); err != nil {
		t.Fatal(err)
	}
	if !checked {
		t.Fatal("the hook never ran")
	}
}

// A close of a thread closed already notes its placed messages first, and
// closes the thread one reopened after it; one still queued stays, and
// reopens the thread once placed: the no-op close drops nothing (Codex
// review of #121, round 1).
func TestACloseOfAClosedThreadNotesFirstAndDropsNothing(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	thread := closedHomeThread(t, h, "d1")
	// Placed, not noted: the close notes it, so the thread reopens before
	// this close, which closes it; no later pass reopens it.
	h.queueOwner(thread.Session)
	if _, err := h.client.Send(ctx, thread.Session, proto.SendMessage{RequestID: "m1", Text: "Take Murbella's point."}); err != nil {
		t.Fatal(err)
	}
	h.placeQueued(requestKeyOf(h.harness, "m1"), thread.Session)
	h.engine.onSend = nil
	if _, err := h.server.TeamClose(ctx, TeamCall{Session: h.super.ID, CallID: "c2"}, thread.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.server.deliverAll(ctx); err != nil {
		t.Fatal(err)
	}
	if got, _ := h.store.Thread(ctx, thread.ID); got.State != store.ThreadClosed || got.Reopened != 1 {
		t.Fatalf("after the close of the placed message: %+v", got)
	}
	// Queued: a close of the closed thread leaves it, and it reopens the
	// thread once placed.
	h.queueSends(thread.Session)
	call := TeamCall{Session: h.super.ID, CallID: "s1"}
	if _, err := h.server.TeamSend(ctx, call, team.Send{Thread: thread.ID, Text: "Fix it."}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.server.TeamClose(ctx, TeamCall{Session: h.super.ID, CallID: "c3"}, thread.ID); err != nil {
		t.Fatal(err)
	}
	if row, err := h.store.Report(ctx, team.SendKey(call.Session, call.key())); err != nil || row.SentAt != nil {
		t.Fatalf("the no-op close dropped the message: %+v, %v", row, err)
	}
	h.placeQueued(h.sendKey(call), thread.Session)
	if _, err := h.server.deliverAll(ctx); err != nil {
		t.Fatal(err)
	}
	if got, _ := h.store.Thread(ctx, thread.ID); got.State != store.ThreadOpen || got.Reopened != 2 {
		t.Fatalf("the queued message did not reopen it: %+v", got)
	}
}

// team_worker on a closed thread notes the thread's placed messages first,
// as team_message does: the message this run acts on reopened it (Codex
// review of #121, round 1).
func TestTeamWorkerNotesAReopeningMessageFirst(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	thread := homeThread(t, h, "d1")
	queuedOverAClose(t, h, thread, "m1")
	_, err := h.server.TeamWorker(ctx, TeamCall{Session: thread.Session, CallID: "w1"}, team.WorkerMessage{Message: "Run the tests."})
	if err != nil && strings.Contains(err.Error(), "is closed") {
		t.Fatalf("team_worker refused the reopened thread: %v", err)
	}
	if got, _ := h.store.Thread(ctx, thread.ID); got.State != store.ThreadOpen {
		t.Fatalf("the thread %+v", got)
	}
}

// A verdict notes its author's placed messages first: one that reopened
// the author's thread has it get the verdict (Codex review of #121, round
// 1).
func TestAVerdictReachesAnAuthorItsMessageReopened(t *testing.T) {
	h := newReviewHarness(t)
	ctx := context.Background()
	// The reviewer is at work: the close's review pass leaves its review.
	h.engine.mu.Lock()
	if h.engine.busy == nil {
		h.engine.busy = map[int64]bool{}
	}
	h.engine.busy[h.teg.Session] = true
	h.engine.mu.Unlock()
	queuedOverAClose(t, h.teamHarness, h.vera, "m1")
	if got, _ := h.store.Thread(ctx, h.vera.ID); got.State != store.ThreadClosed {
		t.Fatalf("the author before the verdict: %+v", got)
	}
	if got, _ := h.store.Thread(ctx, h.teg.ID); got.State == store.ThreadClosed {
		t.Fatalf("the review before the verdict: %q", got.Report)
	}
	if _, err := h.verdict(h.teg.Session, "v1", headA, team.VerdictNotReady, "high blocking: the retry never ends"); err != nil {
		t.Fatal(err)
	}
	if got, _ := h.store.Thread(ctx, h.vera.ID); got.State != store.ThreadOpen {
		t.Fatalf("the author after the verdict: %+v", got)
	}
	for range 2 {
		if _, err := h.server.deliverAll(ctx); err != nil {
			t.Fatal(err)
		}
	}
	var told bool
	for _, text := range h.sentTo(h.vera.Session) {
		told = told || strings.Contains(text, "[T1 · cross-review · not-ready]")
	}
	if !told {
		t.Fatalf("the reopened author got no verdict: %q", h.sentTo(h.vera.Session))
	}
}

// A kept worktree's note whose write failed belongs to its close: once the
// thread reopened and closed again, it is dropped, and the worktree is
// looked at again as it is then (Codex review of #121, round 1).
func TestAKeptNoteOfAnEarlierCloseLandsOnNoLaterOne(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	thread := veraThread(t, h)
	// A commit in no linked pull request keeps the worktree.
	cleanupCommit(t, thread.Cwd)
	db, err := sql.Open("sqlite", h.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`CREATE TRIGGER cleanup_note_fails BEFORE UPDATE OF report ON threads
		WHEN instr(NEW.report, 'pocketd kept the worktree') > 0 BEGIN SELECT RAISE(ABORT, 'disk I/O error'); END`); err != nil {
		t.Fatal(err)
	}
	cleanupClose(t, h, thread)
	if _, err := h.client.Send(ctx, thread.Session, proto.SendMessage{RequestID: "m1", Text: "One more thing."}); err != nil {
		t.Fatal(err)
	}
	if got, _ := h.store.Thread(ctx, thread.ID); got.State != store.ThreadOpen {
		t.Fatalf("not reopened: %+v", got)
	}
	if _, err := db.Exec(`DROP TRIGGER cleanup_note_fails`); err != nil {
		t.Fatal(err)
	}
	// The reopened thread drops its commit: its worktree may go now.
	branchBaseGit(t, thread.Cwd, "reset", "-q", "--hard", "HEAD~1")
	if _, err := h.server.TeamClose(ctx, TeamCall{Session: h.super.ID, CallID: "close-again"}, thread.ID); err != nil {
		t.Fatal(err)
	}
	h.server.reviewPass(ctx)
	h.server.reviewPass(ctx)
	got, err := h.store.Thread(ctx, thread.ID)
	if err != nil || strings.Contains(got.Report, "pocketd kept the worktree") {
		t.Fatalf("the earlier close's note landed on the later one: %q, %v", got.Report, err)
	}
	cleanupGone(t, h, thread)
}

// The event of the thread a delegation follows, which names its successor,
// is published by a delivery pass when it cannot be read at once (Codex
// review of #121, round 1).
func TestASuccessorsEventIsPublishedLater(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	old := closedHomeThread(t, h, "d1")
	named := func() bool {
		h.mu.Lock()
		defer h.mu.Unlock()
		for _, e := range h.events {
			if e.Type == proto.EventThread && e.Thread.ID == old.ID && e.Thread.Successor != 0 {
				return true
			}
		}
		return false
	}
	h.server.failFollowedRead = errors.New("database is locked")
	if _, err := h.server.TeamDelegate(ctx, TeamCall{Session: h.super.ID, CallID: "d2"}, team.Delegation{Agent: "duncan", Task: "Carry T1 on.", Follows: old.ID}); err != nil {
		t.Fatal(err)
	}
	if named() {
		t.Fatal("an event named the successor though its read failed")
	}
	h.server.failFollowedRead = nil
	if _, err := h.server.deliverAll(ctx); err != nil {
		t.Fatal(err)
	}
	if !named() {
		t.Fatal("no pass published the successor")
	}
}

// A repository successor that reports its predecessor's pull request is
// told to report again without it, before GitHub is read (Codex review of
// #121, round 1).
func TestARepositorySuccessorIsToldToReportWithoutItsPredecessorsPull(t *testing.T) {
	h := newReviewHarness(t)
	ctx := context.Background()
	if _, err := h.server.TeamClose(ctx, TeamCall{Session: h.super.ID, CallID: "c1"}, h.vera.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.server.TeamDelegate(ctx, TeamCall{Session: h.super.ID, CallID: "next"},
		team.Delegation{Agent: "vera", Task: "Carry T1 on.", Repository: "frostyard/example", Follows: h.vera.ID}); err != nil {
		t.Fatal(err)
	}
	next, err := h.store.ThreadByKey(ctx, TeamCall{Session: h.super.ID, CallID: "next"}.key())
	if err != nil {
		t.Fatal(err)
	}
	h.engine.mu.Lock()
	reads := len(h.engine.ghCalls)
	h.engine.mu.Unlock()
	_, err = h.server.TeamMessage(ctx, TeamCall{Session: next.Session, CallID: "r1"}, team.Report{Status: team.StatusDone, Message: "Fixed.", PullRequests: []team.PullRef{{Number: 3}}})
	if err == nil || !strings.Contains(err.Error(), "frostyard/example#3 is linked to thread T1: report again without it") {
		t.Fatalf("the report: %v", err)
	}
	h.engine.mu.Lock()
	defer h.engine.mu.Unlock()
	if len(h.engine.ghCalls) != reads {
		t.Fatalf("GitHub was read: %q", h.engine.ghCalls[reads:])
	}
}
