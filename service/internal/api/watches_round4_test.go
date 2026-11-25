package api

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/frostyard/pocketgo/service/internal/store"
	"github.com/frostyard/pocketgo/service/internal/team"
)

// Once the budget holds a pass back, the pass makes no more requests, but
// a later watch of a pull request the pass read already still decides
// from that read: two watches of one merged pull request are told the
// same (Codex review of #173, round 4).
func TestAPassDecidesFromItsReadsOnceTheBudgetHoldsItBack(t *testing.T) {
	h := newTeamHarness(t)
	g := watchGitHub(h)
	ctx := context.Background()
	other := githubPull("frostyard/example", 8, "vera/other", "frostyard/example", "open", false)
	h.engine.mu.Lock()
	h.engine.onGH = func(ctx context.Context, args []string) (string, error) {
		if strings.HasSuffix(strings.Join(args, " "), "repos/frostyard/example/pulls/8") {
			return other, nil
		}
		return g.gh(ctx, args)
	}
	h.engine.mu.Unlock()
	first := TeamCall{Session: h.super.ID, Task: 1, CallID: "w1"}
	held := TeamCall{Session: h.super.ID, Task: 1, CallID: "w2"}
	later := TeamCall{Session: h.super.ID, Task: 1, CallID: "w3"}
	if _, err := h.server.TeamWatch(ctx, first, watchUntil(team.UntilMerged)); err != nil {
		t.Fatal(err)
	}
	if _, err := h.server.TeamWatch(ctx, held, team.WatchCall{Action: team.WatchAdd, Pull: "frostyard/example#8", Until: team.UntilChecks}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.server.TeamWatch(ctx, later, watchUntil(team.UntilMerged)); err != nil {
		t.Fatal(err)
	}
	g.set(func(g *fakeGitHub) { g.state, g.merged = "closed", true })
	// The budget holds 2 requests: #7's read, and not #8's whole read.
	for h.server.watchBudget.Take(h.clock.Now()) {
	}
	h.clock.Advance(6 * time.Second)
	if err := h.server.watchPass(ctx); err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		call    TeamCall
		outcome string
	}{"the first watch of #7": {first, team.WatchMet}, "the held watch of #8": {held, ""}, "the later watch of #7": {later, team.WatchMet}} {
		if w := h.watchOf(tc.call); w.Outcome != tc.outcome {
			t.Errorf("%s: %+v", name, w)
		}
	}
}

// A pull round starts the watch pass and never waits for it: the next
// round is scheduled before any watched pull request is read (Codex
// review of #173, round 4).
func TestAPullRoundDoesNotWaitForTheWatchPass(t *testing.T) {
	h := newTeamHarness(t)
	g := watchGitHub(h)
	ctx := context.Background()
	var rounds []func()
	h.server.pullTimer = func(_ time.Duration, f func()) func() {
		rounds = append(rounds, f)
		return func() {}
	}
	h.server.StartPulls(5 * time.Minute)
	call := TeamCall{Session: h.super.ID, Task: 1, CallID: "w1"}
	if _, err := h.server.TeamWatch(ctx, call, watchUntil(team.UntilMerged)); err != nil {
		t.Fatal(err)
	}
	roundsAtRead := -1
	h.engine.mu.Lock()
	h.engine.onGH = func(ctx context.Context, args []string) (string, error) {
		if strings.HasSuffix(strings.Join(args, " "), "repos/frostyard/example/pulls/7") {
			roundsAtRead = len(rounds)
		}
		return g.gh(ctx, args)
	}
	h.engine.mu.Unlock()
	g.set(func(g *fakeGitHub) { g.state, g.merged = "closed", true })
	rounds[0]()
	if len(rounds) != 2 || roundsAtRead != -1 {
		t.Fatalf("the round waited for the watches: %d rounds, the pull request read with %d", len(rounds), roundsAtRead)
	}
	if n := h.runWatchPasses(); n != 1 {
		t.Fatalf("%d watch passes", n)
	}
	if w := h.watchOf(call); w.Outcome != team.WatchMet || roundsAtRead != 2 {
		t.Fatalf("the watch pass %+v, read with %d rounds", w, roundsAtRead)
	}
}

// With no pull rounds, admissions close at once, and the sweep of the
// live watches runs off the start path: StartPulls returns before it, and
// serving waits for none of it (Codex review of #173, round 4).
func TestNoPullRoundsSweepRunsOffTheStartPath(t *testing.T) {
	h := newTeamHarness(t)
	watchGitHub(h)
	ctx := context.Background()
	call := TeamCall{Session: h.super.ID, Task: 1, CallID: "w1"}
	if _, err := h.server.TeamWatch(ctx, call, watchUntil(team.UntilMerged)); err != nil {
		t.Fatal(err)
	}
	h.runScheduled(100)
	h.server.StartPulls(0)
	if w := h.watchOf(call); w.State != store.WatchLive {
		t.Fatalf("StartPulls swept on the start path: %+v", w)
	}
	if _, err := h.server.TeamWatch(ctx, TeamCall{Session: h.super.ID, Task: 1, CallID: "w2"}, watchUntil(team.UntilMerged)); err == nil || err.Error() != team.WatchesOff {
		t.Fatalf("an add after StartPulls(0): %v", err)
	}
	h.runScheduled(100)
	if w := h.watchOf(call); w.Outcome != team.WatchUnchecked {
		t.Fatalf("the scheduled sweep left %+v", w)
	}
}
