package api

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/frostyard/pocketgo/proto"
	"github.com/frostyard/pocketgo/service/internal/store"
	"github.com/frostyard/pocketgo/service/internal/team"
)

// A watch's note waits while the session has an input in PiG's inbox,
// though no run: after a failed run PiG would start that older input
// first and queue the note behind it, past a thread's close (Codex review
// of #173, round 3).
func TestAWatchsNoteWaitsForAnInputInTheInbox(t *testing.T) {
	h := newTeamHarness(t)
	g := watchGitHub(h)
	ctx := context.Background()
	if _, err := h.server.TeamWatch(ctx, TeamCall{Session: h.super.ID, Task: 1, CallID: "w1"}, watchUntil(team.UntilMerged)); err != nil {
		t.Fatal(err)
	}
	// No run, and an older input waits in the inbox.
	waiting := false
	h.engine.mu.Lock()
	h.engine.idleSays = &waiting
	h.engine.mu.Unlock()
	g.set(func(g *fakeGitHub) { g.state, g.merged = "closed", true })
	if err := h.server.watchPass(ctx); err != nil {
		t.Fatal(err)
	}
	if err := h.server.DeliverPending(ctx); err != nil {
		t.Fatal(err)
	}
	if got := h.watchNotesTo(h.super.ID); len(got) != 0 {
		t.Fatalf("the note went behind a waiting input: %q", got)
	}
	h.engine.mu.Lock()
	h.engine.idleSays = nil
	h.engine.mu.Unlock()
	if err := h.server.DeliverPending(ctx); err != nil {
		t.Fatal(err)
	}
	if got := h.watchNotesTo(h.super.ID); len(got) != 1 {
		t.Fatalf("the idle session got %q", got)
	}
}

// A closed thread's specialist that runs on a message that reopens the
// thread, placed and not noted yet, keeps its watch: the add notes the
// thread's placed messages first (Codex review of #173, round 3).
func TestAReopeningSpecialistKeepsItsWatch(t *testing.T) {
	h := newTeamHarness(t)
	watchGitHub(h)
	ctx := context.Background()
	thread := closedHomeThread(t, h, "d1")
	h.queueOwner(thread.Session)
	if _, err := h.client.Send(ctx, thread.Session, proto.SendMessage{RequestID: "m1", Text: "Watch #7's CI, then report."}); err != nil {
		t.Fatal(err)
	}
	// Placed, and no delivery pass has noted it.
	h.placeQueued(requestKeyOf(h.harness, "m1"), thread.Session)
	h.engine.onSend = nil
	if got, _ := h.store.Thread(ctx, thread.ID); got.State != store.ThreadClosed {
		t.Fatalf("the thread %+v", got)
	}
	text, err := h.server.TeamWatch(ctx, TeamCall{Session: thread.Session, Task: 1, CallID: "w1"}, watchUntil(team.UntilChecks))
	if err != nil || !strings.HasPrefix(text, "Watching W1") {
		t.Fatalf("the reopened specialist's add %q %v", text, err)
	}
	if got, _ := h.store.Thread(ctx, thread.ID); got.State != store.ThreadOpen || got.Reopened != 1 {
		t.Fatalf("the thread %+v", got)
	}
}

// A read of many pages of CI finishes under short rounds: the budget
// builds up for the whole read, rather than spending each refill on its
// first pages, and never goes over (Codex review of #173, round 3).
func TestAManyPageCIReadFinishesUnderShortRounds(t *testing.T) {
	h := newTeamHarness(t)
	g := watchGitHub(h)
	ctx := context.Background()
	pending := true
	calls := 0
	h.engine.mu.Lock()
	h.engine.onGH = func(ctx context.Context, args []string) (string, error) {
		call := strings.Join(args, " ")
		ci := strings.Contains(call, "/check-runs") || strings.Contains(call, "/status")
		if !ci || pending {
			if !pending {
				calls++
			}
			return g.gh(ctx, args)
		}
		calls++
		page := 1
		if i := strings.Index(call, "&page="); i >= 0 {
			_, _ = fmt.Sscanf(call[i+len("&page="):], "%d", &page)
		}
		items := make([]string, 100)
		if strings.Contains(call, "/check-runs") {
			for i := range items {
				items[i] = fmt.Sprintf(`{"id":%d,"name":"job %d","status":"completed","conclusion":"success"}`, page*1000+i, page*1000+i)
			}
			return `{"total_count":1000,"check_runs":[` + strings.Join(items, ",") + `]}`, nil
		}
		for i := range items {
			items[i] = fmt.Sprintf(`{"context":"status %d","state":"success"}`, page*1000+i)
		}
		return `{"total_count":1000,"statuses":[` + strings.Join(items, ",") + `]}`, nil
	}
	h.engine.mu.Unlock()
	call := TeamCall{Session: h.super.ID, Task: 1, CallID: "w1"}
	if _, err := h.server.TeamWatch(ctx, call, watchUntil(team.UntilChecks)); err != nil {
		t.Fatal(err)
	}
	pending = false
	// The burst is spent: each 30-second round refills 10 requests, and a
	// whole read takes 21.
	for h.server.watchBudget.Take(h.clock.Now()) {
	}
	var perPass []int
	for pass := 0; pass < 20 && h.watchOf(call).State == store.WatchLive; pass++ {
		h.clock.Advance(30 * time.Second)
		before := calls
		_ = h.server.watchPass(ctx)
		perPass = append(perPass, calls-before)
	}
	w := h.watchOf(call)
	if w.Outcome != team.WatchMet || !strings.Contains(w.Seen, "green") {
		t.Fatalf("the read never finished: %+v, requests by pass %v", w, perPass)
	}
	for _, n := range perPass {
		if n > 21 {
			t.Fatalf("a pass spent %d requests: %v", n, perPass)
		}
	}
}
