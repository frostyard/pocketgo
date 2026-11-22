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

// An earlier close's stop of the worker stops nothing of a thread a message
// reopened since: its worker works for it again (Codex review of #121,
// round 2).
func TestAnEarlierClosesStopLeavesAReopenedThreadsWorker(t *testing.T) {
	h, thread := workerHarness(t)
	ctx := context.Background()
	if _, err := h.server.TeamWorker(ctx, TeamCall{Session: thread.Session, Task: 7, CallID: "w1"}, team.WorkerMessage{Message: "Fix the flaky test."}); err != nil {
		t.Fatal(err)
	}
	row, err := h.store.Thread(ctx, thread.ID)
	if err != nil || row.WorkerSession == 0 {
		t.Fatalf("no worker: %+v, %v", row, err)
	}
	if _, err := h.store.CloseThread(ctx, thread.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.client.Send(ctx, thread.Session, proto.SendMessage{RequestID: "m1", Text: "One more thing."}); err != nil {
		t.Fatal(err)
	}
	if got, _ := h.store.Thread(ctx, thread.ID); got.State != store.ThreadOpen {
		t.Fatalf("not reopened: %+v", got)
	}
	h.engine.mu.Lock()
	if h.engine.busy == nil {
		h.engine.busy = map[int64]bool{}
	}
	h.engine.busy[row.WorkerSession] = true
	aborted := len(h.engine.aborted)
	h.engine.mu.Unlock()
	// The close's stop, run after its locks, once the thread reopened, of
	// a run that is no longer the one going (run 41).
	h.server.stopWorker(ctx, row.WorkerSession, 41)
	h.engine.mu.Lock()
	defer h.engine.mu.Unlock()
	if len(h.engine.aborted) != aborted {
		t.Fatalf("the earlier close aborted the reopened thread's worker: %v", h.engine.aborted[aborted:])
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
