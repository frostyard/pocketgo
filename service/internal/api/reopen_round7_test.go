package api

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/frostyard/pocketgo/service/internal/store"
	"github.com/frostyard/pocketgo/service/internal/team"
)

// One stop of a worker goes at a time, from its look at the thread to the
// note that it is done, and a stop no longer owed stops nothing: a second
// caller never acts on a look the first one's stop made stale (Codex
// review of #121, round 7).
func TestAWorkersStopsGoOneAtATime(t *testing.T) {
	h, thread := workerHarness(t)
	ctx := context.Background()
	worker := startedWorker(t, h, thread)
	if _, err := h.store.CloseThread(ctx, thread.ID); err != nil {
		t.Fatal(err)
	}
	held := false
	h.server.afterStopRead = func(w int64) {
		if unlock, ok := h.server.workerStops.tryLock(strconv.FormatInt(w, 10)); ok {
			unlock()
		} else {
			held = true
		}
	}
	h.server.stopWorker(ctx, worker)
	if !held {
		t.Fatal("the stop looked at the thread outside its worker's lock")
	}
	// Done: the stop is owed no more, and a second one aborts nothing,
	// whatever runs.
	if got, _ := h.store.Thread(ctx, thread.ID); got.WorkerStopDue {
		t.Fatalf("the stop is still owed: %+v", got)
	}
	h.engine.mu.Lock()
	if h.engine.busy == nil {
		h.engine.busy = map[int64]bool{}
	}
	h.engine.busy[worker] = true
	aborted := len(h.engine.aborted)
	h.engine.mu.Unlock()
	h.server.stopWorker(ctx, worker)
	h.engine.mu.Lock()
	defer h.engine.mu.Unlock()
	if len(h.engine.aborted) != aborted {
		t.Fatalf("a stop no longer owed aborted %v", h.engine.aborted[aborted:])
	}
}

// team_send's answer, run again, notes the owner's placed messages before
// it names the thread's state: a thread one of them reopened is told open
// (Codex review of #121, round 7).
func TestTeamSendsAnswerNotesTheOwnersMessagesFirst(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	thread := homeThread(t, h, "d1")
	call := TeamCall{Session: h.super.ID, CallID: "s1"}
	if _, err := h.server.TeamSend(ctx, call, team.Send{Thread: thread.ID, Text: "Fix it."}); err != nil {
		t.Fatal(err)
	}
	queuedOverAClose(t, h, thread, "m1")
	again, err := h.server.TeamSend(ctx, call, team.Send{Thread: thread.ID, Text: "Fix it."})
	if err != nil || !strings.Contains(again, "T1 is open") {
		t.Fatalf("the answer run again: %q, %v", again, err)
	}
	if got, _ := h.store.Thread(ctx, thread.ID); got.State != store.ThreadOpen {
		t.Fatalf("the thread %+v", got)
	}
}
