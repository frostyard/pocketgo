package api

import (
	"context"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/frostyard/pocketgo/proto"
	"github.com/frostyard/pocketgo/service/internal/store"
	"github.com/frostyard/pocketgo/service/internal/team"
)

// reopenedWithWorker starts thread's worker, closes the thread through the
// API, whose stop of the idle worker is done at once, and reopens it with
// the owner's message.
func reopenedWithWorker(t *testing.T, h *teamHarness, thread store.ThreadRow) int64 {
	t.Helper()
	ctx := context.Background()
	worker := startedWorker(t, h, thread)
	if _, err := h.server.TeamClose(ctx, TeamCall{Session: h.super.ID, CallID: "close"}, thread.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.client.Send(ctx, thread.Session, proto.SendMessage{RequestID: "m1", Text: "One more thing."}); err != nil {
		t.Fatal(err)
	}
	if got, _ := h.store.Thread(ctx, thread.ID); got.State != store.ThreadOpen || got.WorkerSession != worker {
		t.Fatalf("the reopened thread %+v", got)
	}
	return worker
}

// A session no thread names as its worker is owed no stop: a due look at
// it that finds none aborts nothing, whatever runs there (Codex review of
// #121, round 6).
func TestADueSessionNoThreadNamesIsNeverStopped(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	plain, err := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "plain", Cwd: h.work, Model: model})
	if err != nil {
		t.Fatal(err)
	}
	h.engine.mu.Lock()
	if h.engine.busy == nil {
		h.engine.busy = map[int64]bool{}
	}
	h.engine.busy[plain.ID] = true
	aborted := len(h.engine.aborted)
	h.engine.mu.Unlock()
	h.server.workerDue(plain.ID)
	if _, err := h.server.deliverAll(ctx); err != nil {
		t.Fatal(err)
	}
	h.server.stopWorker(ctx, plain.ID)
	h.engine.mu.Lock()
	defer h.engine.mu.Unlock()
	if len(h.engine.aborted) != aborted {
		t.Fatalf("aborted a session no thread names: %v", h.engine.aborted[aborted:])
	}
}

// A reopened thread keeps its worker: the owner's message to it is still
// refused, the worker still names its starter, and a team_worker call run
// again answers with it (Codex review of #121, round 6).
func TestAReopenedThreadKeepsItsWorker(t *testing.T) {
	h, thread := workerHarness(t)
	ctx := context.Background()
	worker := reopenedWithWorker(t, h, thread)
	_, err := h.client.Send(ctx, worker, proto.SendMessage{RequestID: "w-direct", Text: "Do this."})
	asError(t, err, http.StatusConflict, proto.ErrorCodeConflict)
	if got, err := h.client.Session(ctx, worker); err != nil || got.Session.StartedBy != thread.Session {
		t.Fatalf("the worker %+v, %v", got.Session, err)
	}
	answer, err := h.server.TeamWorker(ctx, TeamCall{Session: thread.Session, Task: 7, CallID: "w1"}, team.WorkerMessage{Message: "Go."})
	if err != nil || !strings.Contains(answer, "session "+strconv.FormatInt(worker, 10)) {
		t.Fatalf("the call run again: %q, %v", answer, err)
	}
}

// A closed thread whose worker's stop is not done keeps its worktree, and
// no reopen comes before the stop: the worker it names is the one the
// cleanup checks idle (Codex review of #121, round 6).
func TestACleanupWaitsForTheWorkerAStopIsOwed(t *testing.T) {
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
	h.server.reviewPass(ctx)
	if _, err := os.Stat(thread.Cwd); err != nil {
		t.Fatalf("the worktree went under the busy worker: %v", err)
	}
	if got, _ := h.store.Thread(ctx, thread.ID); got.State != store.ThreadClosed || got.WorkerSession != worker {
		t.Fatalf("the thread %+v", got)
	}
}
