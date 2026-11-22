package api

import (
	"context"
	"database/sql"
	"net/http"
	"strings"
	"testing"

	"github.com/frostyard/pocketgo/proto"
	"github.com/frostyard/pocketgo/service/internal/store"
	"github.com/frostyard/pocketgo/service/internal/team"
)

// The owner's message is checked again under outboxMu: a plan task's
// thread that closed after the send looked at it, open, takes no message
// it could not reopen (Codex review of #121, round 2).
func TestTheOwnersMessageToAThreadThatClosedMeanwhileIsCheckedAgain(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	thread := homeThread(t, h, "d1")
	db, err := sql.Open("sqlite", h.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	// A plan task's thread, as far as the send reads it.
	if _, err := db.Exec(`UPDATE threads SET plan = 7, plan_task = 't1' WHERE id = ?`, thread.ID); err != nil {
		t.Fatal(err)
	}
	h.server.beforeOwnerSend = func() {
		// Its plan closes it between the send's look and its message.
		if _, err := db.Exec(`UPDATE threads SET state = 'closed' WHERE id = ?`, thread.ID); err != nil {
			t.Error(err)
		}
	}
	sent := len(h.engine.order)
	_, err = h.client.Send(ctx, thread.Session, proto.SendMessage{RequestID: "m1", Text: "One more thing."})
	asError(t, err, http.StatusConflict, proto.ErrorCodeConflict)
	if !strings.Contains(err.Error(), "delivered task t1 of plan P7 and stays closed") {
		t.Fatalf("the refusal: %v", err)
	}
	if len(h.engine.order) != sent {
		t.Fatalf("the message went: %q", h.engine.order[sent:])
	}
}

// team_send notes the thread's placed messages before it keeps its own:
// a thread the owner's placed message reopened takes a message written for
// an open thread, which unblocks it (Codex review of #121, round 2).
func TestTeamSendNotesPlacedMessagesBeforeItKeepsItsOwn(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	thread := closedHomeThread(t, h, "d1")
	h.queueOwner(thread.Session)
	if _, err := h.client.Send(ctx, thread.Session, proto.SendMessage{RequestID: "m1", Text: "Take Murbella's point."}); err != nil {
		t.Fatal(err)
	}
	h.placeQueued(requestKeyOf(h.harness, "m1"), thread.Session)
	h.engine.onSend = nil
	call := TeamCall{Session: h.super.ID, CallID: "s1"}
	if _, err := h.server.TeamSend(ctx, call, team.Send{Thread: thread.ID, Text: "And the nits."}); err != nil {
		t.Fatal(err)
	}
	row, err := h.store.Report(ctx, team.SendKey(call.Session, call.key()))
	if err != nil || row.Reopens != 0 || !row.Unblocks || !strings.Contains(row.Text, "If T1 is blocked") {
		t.Fatalf("the message was kept for a closed thread: %+v, %v", row, err)
	}
}

// No message reopens a closed thread before the stop its close owes the
// worker is done: the run of the closed time stops first, so the stop
// never reaches work of the reopened thread, and once the thread is open
// the stop stops nothing (Codex review of #121, rounds 2 to 6).
func TestAnEarlierClosesStopLeavesAReopenedThreadsWorker(t *testing.T) {
	h, thread := workerHarness(t)
	ctx := context.Background()
	worker := startedWorker(t, h, thread)
	h.engine.mu.Lock()
	if h.engine.busy == nil {
		h.engine.busy = map[int64]bool{}
	}
	h.engine.busy[worker] = true
	h.engine.mu.Unlock()
	if _, err := h.store.CloseThread(ctx, thread.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.client.Send(ctx, thread.Session, proto.SendMessage{RequestID: "m1", Text: "One more thing."}); err != nil {
		t.Fatal(err)
	}
	if got, _ := h.store.Thread(ctx, thread.ID); got.State != store.ThreadClosed || !got.WorkerStopDue {
		t.Fatalf("reopened before its worker's stop: %+v", got)
	}
	if left, _ := h.store.OwnerMessagesOf(ctx, thread.ID); len(left) != 1 {
		t.Fatalf("the message is no longer followed: %+v", left)
	}
	// The close's stop, run after its locks.
	h.server.stopWorker(ctx, worker)
	h.engine.mu.Lock()
	aborted := append([]int64(nil), h.engine.aborted...)
	delete(h.engine.busy, worker)
	h.engine.mu.Unlock()
	if len(aborted) == 0 || aborted[len(aborted)-1] != worker {
		t.Fatalf("aborted %v, want the worker %d", aborted, worker)
	}
	if _, err := h.server.deliverAll(ctx); err != nil {
		t.Fatal(err)
	}
	got, _ := h.store.Thread(ctx, thread.ID)
	if got.State != store.ThreadOpen || got.WorkerStopDue || got.WorkerSession != worker {
		t.Fatalf("after the stop: %+v", got)
	}
	// A stop of the open thread's worker stops nothing.
	h.engine.mu.Lock()
	if h.engine.busy == nil {
		h.engine.busy = map[int64]bool{}
	}
	h.engine.busy[worker] = true
	h.engine.mu.Unlock()
	h.server.stopWorker(ctx, worker)
	h.engine.mu.Lock()
	defer h.engine.mu.Unlock()
	if len(h.engine.aborted) != len(aborted) {
		t.Fatalf("a stop aborted the reopened thread's worker: %v", h.engine.aborted)
	}
}

// A reopen publishes the thread's pull requests with it: a client lists a
// closed thread's only while open, and has no copy of a merged one (Codex
// review of #121, round 2).
func TestAReopenPublishesTheThreadsPullRequests(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	thread := homeThread(t, h, "d1")
	merged := store.PullRow{GitHubID: 31, Repository: "frostyard/example", Number: 31, Title: "Pin the runner", URL: "u", State: store.PullMerged,
		Head: "h", HeadSHA: strings.Repeat("a", 40), Base: "main"}
	if _, _, _, _, err := h.store.FileReportWithPulls(ctx, "r1", thread.ID, store.ThreadDone, "Merged.", nil, []store.PullRow{merged},
		func([]int64, []store.PullRow) string { return "r" }); err != nil {
		t.Fatal(err)
	}
	if _, err := h.server.TeamClose(ctx, TeamCall{Session: h.super.ID, CallID: "c1"}, thread.ID); err != nil {
		t.Fatal(err)
	}
	h.mu.Lock()
	before := len(h.events)
	h.mu.Unlock()
	if _, err := h.client.Send(ctx, thread.Session, proto.SendMessage{RequestID: "m1", Text: "One more thing."}); err != nil {
		t.Fatal(err)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	reopened, pulled := false, false
	for _, e := range h.events[before:] {
		reopened = reopened || (e.Type == proto.EventThread && e.Thread.ID == thread.ID && e.Thread.State == proto.ThreadOpen)
		pulled = pulled || (reopened && e.Type == proto.EventPull && e.Pull.Number == 31)
	}
	if !reopened || !pulled {
		t.Fatalf("reopened %v, its merged pull request published after %v", reopened, pulled)
	}
}
