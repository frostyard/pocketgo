package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/frostyard/pocketgo/proto"
	"github.com/frostyard/pocketgo/service/internal/store"
	"github.com/frostyard/pocketgo/service/internal/team"
)

// A thread that closed after the owner's send looked at it gets the whole
// check of a closed thread's message, its folder's included: a worktree
// no longer on its branch takes no work (Codex review of #121, round 3).
func TestAThreadThatClosedDuringTheSendHasItsFolderChecked(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	thread := veraThread(t, h)
	h.server.beforeOwnerSend = func() {
		h.server.beforeOwnerSend = nil
		if _, err := h.server.TeamClose(ctx, TeamCall{Session: h.super.ID, CallID: "close"}, thread.ID); err != nil {
			t.Error(err)
		}
		branchBaseGit(t, thread.Cwd, "checkout", "-q", "-b", "elsewhere")
	}
	sent := len(h.engine.order)
	_, err := h.client.Send(ctx, thread.Session, proto.SendMessage{RequestID: "m1", Text: "Take Murbella's point."})
	asError(t, err, http.StatusConflict, proto.ErrorCodeConflict)
	if !strings.Contains(err.Error(), "its folder is no longer its own") {
		t.Fatalf("the refusal: %v", err)
	}
	if len(h.engine.order) != sent {
		t.Fatalf("the message went: %q", h.engine.order[sent:])
	}
}

// A merge's close reads the thread again under the team's locks before it
// withdraws anything: a thread a close and a reopen moved past the merges
// it counted keeps its queued messages (Codex review of #121, round 3).
func TestAMergeCloseChecksAgainBeforeItWithdraws(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	thread := homeThread(t, h, "d1")
	merged := store.PullRow{GitHubID: 31, Repository: "frostyard/example", Number: 31, Title: "Pin the runner", URL: "u", State: store.PullMerged,
		Head: "h", HeadSHA: strings.Repeat("a", 40), Base: "main"}
	if _, _, _, _, err := h.store.FileReportWithPulls(ctx, "r1", thread.ID, store.ThreadDone, "Merged.", nil, []store.PullRow{merged},
		func([]int64, []store.PullRow) string { return "r" }); err != nil {
		t.Fatal(err)
	}
	call := TeamCall{Session: h.super.ID, CallID: "s1"}
	h.server.beforeMergeClose = func(int64) {
		h.server.beforeMergeClose = nil
		// Closed and reopened since the merges were counted, with a
		// supervisor's message queued for the reopened thread.
		if _, err := h.server.TeamClose(ctx, TeamCall{Session: h.super.ID, CallID: "c1"}, thread.ID); err != nil {
			t.Error(err)
		}
		if _, err := h.client.Send(ctx, thread.Session, proto.SendMessage{RequestID: "m1", Text: "One more thing."}); err != nil {
			t.Error(err)
		}
		h.queueSends(thread.Session)
		if _, err := h.server.TeamSend(ctx, call, team.Send{Thread: thread.ID, Text: "And this."}); err != nil {
			t.Error(err)
		}
	}
	if err := h.server.closeIfMerged(ctx, thread.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := h.store.Thread(ctx, thread.ID); got.State == store.ThreadClosed {
		t.Fatalf("the reopened thread closed on its old merge: %+v", got)
	}
	h.engine.mu.Lock()
	defer h.engine.mu.Unlock()
	for _, key := range h.engine.withdrawals {
		if key == h.sendKey(call) {
			t.Fatalf("the merge's close withdrew the reopened thread's message: %q", h.engine.withdrawals)
		}
	}
}

// The close's stop of the worker moves its cursor past the closed time:
// a run the close stopped replies to no one once the thread is open again
// (Codex review of #121, rounds 3 to 6).
func TestAReopenLeavesTheClosedTimesWorkerRunsUnanswered(t *testing.T) {
	h, thread := workerHarness(t)
	ctx := context.Background()
	worker := startedWorker(t, h, thread)
	h.engine.mu.Lock()
	h.engine.transcripts = map[int64][]proto.Entry{worker: {{ID: 3, Kind: proto.EntryUser, Text: "Go."},
		{ID: 4, Kind: proto.EntryAssistant, Text: "The aborted run's reply."}}}
	h.engine.mu.Unlock()
	if _, err := h.server.TeamClose(ctx, TeamCall{Session: h.super.ID, CallID: "close"}, thread.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.client.Send(ctx, thread.Session, proto.SendMessage{RequestID: "m1", Text: "One more thing."}); err != nil {
		t.Fatal(err)
	}
	reopened, err := h.store.Thread(ctx, thread.ID)
	if err != nil || reopened.State != store.ThreadOpen || reopened.WorkerCursor != 4 {
		t.Fatalf("the reopened thread %+v, %v", reopened, err)
	}
	if err := h.server.queueWorkerReplies(ctx, reopened, worker); err != nil {
		t.Fatal(err)
	}
	unsent, _ := h.store.UnsentReportsOf(ctx, thread.ID)
	for _, row := range unsent {
		if strings.HasPrefix(row.Key, "worker-reply:") {
			t.Fatalf("the closed time's run replied: %+v", row)
		}
	}
}

// A close's worker stop that failed holds the reopen until the delivery
// pass's retry stopped the run (Codex review of #121, rounds 5 and 6).
func TestAFailedStopOfARetiredWorkerIsTriedAgain(t *testing.T) {
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
	h.server.failRead = func(what string, id int64) error {
		if what == "readThreadOfWorker" {
			return errors.New("database is locked")
		}
		return nil
	}
	h.server.stopWorker(ctx, worker)
	h.server.failRead = nil
	if _, err := h.client.Send(ctx, thread.Session, proto.SendMessage{RequestID: "m1", Text: "One more thing."}); err != nil {
		t.Fatal(err)
	}
	if got, _ := h.store.Thread(ctx, thread.ID); got.State != store.ThreadClosed {
		t.Fatalf("reopened before the stop: %+v", got)
	}
	for range 2 {
		if _, err := h.server.deliverAll(ctx); err != nil {
			t.Fatal(err)
		}
	}
	h.engine.mu.Lock()
	aborted := append([]int64(nil), h.engine.aborted...)
	h.engine.mu.Unlock()
	if len(aborted) == 0 || aborted[len(aborted)-1] != worker {
		t.Fatalf("aborted %v; want the worker %d", aborted, worker)
	}
	if got, _ := h.store.Thread(ctx, thread.ID); got.State != store.ThreadOpen {
		t.Fatalf("not reopened once the stop was done: %+v", got)
	}
}

// The worker's stop aborts nothing when its thread cannot be read: the
// delivery pass tries it again, so a reopened thread's new run is never
// stopped on a guess (Codex review of #121, round 3).
func TestAWorkersStopWaitsForAReadOfItsThread(t *testing.T) {
	h, thread := workerHarness(t)
	ctx := context.Background()
	worker := startedWorker(t, h, thread)
	h.engine.mu.Lock()
	if h.engine.busy == nil {
		h.engine.busy = map[int64]bool{}
	}
	h.engine.busy[worker] = true
	aborted := len(h.engine.aborted)
	h.engine.mu.Unlock()
	h.server.failRead = func(what string, id int64) error {
		if what == "readThreadOfWorker" {
			return errors.New("database is locked")
		}
		return nil
	}
	h.server.stopWorker(ctx, worker)
	h.engine.mu.Lock()
	stopped := len(h.engine.aborted) != aborted
	h.engine.mu.Unlock()
	if stopped {
		t.Fatal("the stop aborted the worker though its thread could not be read")
	}
	h.server.workerMu.Lock()
	due := h.server.workersDue[worker]
	h.server.workerMu.Unlock()
	if !due {
		t.Fatal("the stop is not tried again")
	}
}

// A reopened thread's pull requests that cannot be read to publish are
// published by a delivery pass (Codex review of #121, round 3).
func TestAReopensPullRequestsArePublishedLater(t *testing.T) {
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
	published := func() bool {
		h.mu.Lock()
		defer h.mu.Unlock()
		reopened := false
		for _, e := range h.events {
			reopened = reopened || (e.Type == proto.EventThread && e.Thread.ID == thread.ID && e.Thread.State == proto.ThreadOpen && e.Thread.Reopens == 1)
			if reopened && e.Type == proto.EventPull && e.Pull.Number == 31 {
				return true
			}
		}
		return false
	}
	h.server.failRead = func(what string, id int64) error {
		if what == "readPullsOf" {
			return errors.New("database is locked")
		}
		return nil
	}
	if _, err := h.client.Send(ctx, thread.Session, proto.SendMessage{RequestID: "m1", Text: "One more thing."}); err != nil {
		t.Fatal(err)
	}
	if published() {
		t.Fatal("published though the read failed")
	}
	h.server.failRead = nil
	if _, err := h.server.deliverAll(ctx); err != nil {
		t.Fatal(err)
	}
	if !published() {
		t.Fatal("no pass published the reopened thread's pull request")
	}
}
