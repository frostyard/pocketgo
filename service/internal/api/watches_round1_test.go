package api

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/frostyard/pocketgo/proto"
	"github.com/frostyard/pocketgo/service/internal/store"
	"github.com/frostyard/pocketgo/service/internal/team"
)

// An add that read GitHub while StartPulls(0) stopped the rounds keeps no
// watch: the sweep that ends the live watches has run, and nothing would
// check one kept after it (Codex review of #173, round 1).
func TestAnAddInFlightWhenRoundsStopKeepsNoWatch(t *testing.T) {
	h := newTeamHarness(t)
	watchGitHub(h)
	ctx := context.Background()
	h.server.afterWatchRead = func() {
		h.server.afterWatchRead = nil
		h.server.StartPulls(0)
	}
	call := TeamCall{Session: h.super.ID, Task: 1, CallID: "w1"}
	if text, err := h.server.TeamWatch(ctx, call, watchUntil(team.UntilMerged)); err == nil || err.Error() != team.WatchesOff {
		t.Fatalf("the add %q %v", text, err)
	}
	if _, err := h.store.WatchByKey(ctx, call.key()); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("the add kept a watch nothing checks: %v", err)
	}
	if live, _ := h.store.LiveWatches(ctx); len(live) != 0 {
		t.Fatalf("live %+v", live)
	}
}

// An add whose specialist's thread closed while it read GitHub keeps no
// watch: the close ended the session's watches before the add would have
// kept one (Codex review of #173, round 1).
func TestAnAddWhoseThreadClosedMeanwhileKeepsNoWatch(t *testing.T) {
	h := newTeamHarness(t)
	watchGitHub(h)
	ctx := context.Background()
	thread := homeThread(t, h, "d1")
	h.server.afterWatchRead = func() {
		h.server.afterWatchRead = nil
		if _, err := h.server.TeamClose(ctx, TeamCall{Session: h.super.ID, Task: 2, CallID: "close"}, thread.ID); err != nil {
			t.Error(err)
		}
	}
	call := TeamCall{Session: thread.Session, Task: 1, CallID: "w1"}
	if text, err := h.server.TeamWatch(ctx, call, watchUntil(team.UntilMerged)); err == nil || !strings.Contains(err.Error(), "is closed, so it keeps no watch") {
		t.Fatalf("the add %q %v", text, err)
	}
	if _, err := h.store.WatchByKey(ctx, call.key()); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a closed thread's specialist kept a watch: %v", err)
	}
}

// Each watch checks GitHub's id of the pull request a pass read against
// its own: a watch of a reused repository name's older pull request gets
// the error, and a watch of the pull request GitHub answers with ends on
// its merge, whichever was read first (Codex review of #173, round 1).
func TestEachWatchChecksGitHubsIDOfItsPullRequest(t *testing.T) {
	h := newTeamHarness(t)
	g := watchGitHub(h)
	ctx := context.Background()
	old, _, err := h.store.AddWatch(ctx, store.WatchRow{Key: "old", SessionID: h.super.ID, Repository: "frostyard/example", Number: 7, GitHubID: 999,
		Until: team.UntilMerged, Hours: 24, Deadline: h.clock.Now().Add(24 * time.Hour)}, team.MaxWatchesPerSession, team.MaxWatches,
		func(store.WatchRow) string { return "old" })
	if err != nil {
		t.Fatal(err)
	}
	call := TeamCall{Session: h.super.ID, Task: 1, CallID: "w1"}
	if _, err := h.server.TeamWatch(ctx, call, watchUntil(team.UntilMerged)); err != nil {
		t.Fatal(err)
	}
	g.set(func(g *fakeGitHub) { g.state, g.merged = "closed", true })
	if err := h.server.watchPass(ctx); err == nil || !strings.Contains(err.Error(), "another pull request than the one watched") {
		t.Fatalf("the pass: %v", err)
	}
	if w := h.watchOf(call); w.Outcome != team.WatchMet {
		t.Fatalf("the watch of the pull request GitHub answers with %+v", w)
	}
	if w, _ := h.store.WatchByKey(ctx, old.Key); w.State != store.WatchLive {
		t.Fatalf("the older pull request's watch %+v", w)
	}
}

// A team_watch call stopped while it read CI keeps no watch: CI its abort
// cut short is not CI GitHub could not answer (Codex review of #173,
// round 1).
func TestAStoppedAddKeepsNoWatch(t *testing.T) {
	h := newTeamHarness(t)
	g := watchGitHub(h)
	call, stop := context.WithCancel(context.Background())
	defer stop()
	h.engine.mu.Lock()
	h.engine.onGH = func(ctx context.Context, args []string) (string, error) {
		if strings.Contains(strings.Join(args, " "), "/check-runs") {
			stop()
			return "", errors.New("gh: signal: killed")
		}
		return g.gh(ctx, args)
	}
	h.engine.mu.Unlock()
	tc := TeamCall{Session: h.super.ID, Task: 1, CallID: "w1"}
	if text, err := h.server.TeamWatch(call, tc, watchUntil(team.UntilChecks)); err == nil || !strings.Contains(err.Error(), "stopped") {
		t.Fatalf("the stopped add %q %v", text, err)
	}
	if _, err := h.store.WatchByKey(context.Background(), tc.key()); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a stopped add kept a watch: %v", err)
	}
	// CI GitHub could not answer leaves the watch to the rounds.
	h.engine.mu.Lock()
	h.engine.onGH = func(ctx context.Context, args []string) (string, error) {
		if strings.Contains(strings.Join(args, " "), "/check-runs") {
			return "", errors.New("gh: HTTP 502")
		}
		return g.gh(ctx, args)
	}
	h.engine.mu.Unlock()
	if _, err := h.server.TeamWatch(context.Background(), TeamCall{Session: h.super.ID, Task: 1, CallID: "w2"}, watchUntil(team.UntilChecks)); err != nil {
		t.Fatalf("an add whose CI GitHub did not answer: %v", err)
	}
}

// A specialist session an earlier run made, before the work database knew
// it, gets team_watch before the brief that names it is sent (Codex review
// of #173, round 1).
func TestARecoveredSpecialistGetsTeamWatchBeforeItsBrief(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	duncan, err := h.server.profiles.Find("duncan")
	if err != nil {
		t.Fatal(err)
	}
	row, _, err := h.store.CreateThread(ctx, store.ThreadRow{Key: "recovered", Title: "Tidy", Agent: "duncan", SupervisorSession: h.super.ID, Task: "Tidy up."})
	if err != nil {
		t.Fatal(err)
	}
	// The earlier run's session, made from duncan's profile before
	// team_watch: the engine has it, the work database does not.
	id, err := h.engine.CreateSession(ctx, "thread:"+row.Owner, SessionSpec{Title: "T1 · Tidy", Cwd: duncan.Start(), Model: proto.ModelRef{Provider: "faux", ID: "faux-1"},
		Profile: &duncan, Thread: row.ID})
	if err != nil {
		t.Fatal(err)
	}
	started, err := h.server.startThread(ctx, row)
	if err != nil || started.Session != id {
		t.Fatalf("the start %+v %v", started, err)
	}
	h.engine.mu.Lock()
	given, late := h.engine.watchTool[id], h.engine.watchLate[id]
	h.engine.mu.Unlock()
	if !given || late {
		t.Fatalf("the recovered session: given team_watch %v, after its brief %v", given, late)
	}
	if brief := h.sentTo(id); len(brief) != 1 || !strings.Contains(brief[0], team.WatchGuide) {
		t.Fatalf("the brief %q", brief)
	}
}

// CI is a commit's: a pass reads it once for two pull requests of one
// repository at one head, and each watch's note keeps its own pull
// request's title (Codex review of #173, round 1).
func TestAPassReadsCIOncePerCommit(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	answers := map[string]string{
		"repos/frostyard/example/pulls/7": strings.Replace(githubPull("frostyard/example", 7, "vera/fix", "frostyard/example", "open", false), `"title":"Fix CI"`, `"title":"Fix CI on main"`, 1),
		"repos/frostyard/example/pulls/8": strings.Replace(githubPull("frostyard/example", 8, "vera/fix", "frostyard/example", "open", false), `"title":"Fix CI"`, `"title":"Fix CI on release"`, 1),
	}
	ci := 0
	h.engine.mu.Lock()
	h.engine.onGH = func(_ context.Context, args []string) (string, error) {
		call := strings.Join(args, " ")
		switch {
		case strings.Contains(call, "/check-runs"):
			ci++
			return failingRuns, nil
		case strings.Contains(call, "/status"):
			return `{"total_count":0,"statuses":[]}`, nil
		}
		for path, answer := range answers {
			if strings.HasSuffix(call, path) {
				return answer, nil
			}
		}
		return "", errors.New("gh: HTTP 404")
	}
	h.engine.mu.Unlock()
	// Both are added while their CI reads pending.
	pending := true
	h.engine.mu.Lock()
	inner := h.engine.onGH
	h.engine.onGH = func(ctx context.Context, args []string) (string, error) {
		if pending && strings.Contains(strings.Join(args, " "), "/check-runs") {
			return pendingRuns, nil
		}
		return inner(ctx, args)
	}
	h.engine.mu.Unlock()
	for _, n := range []string{"7", "8"} {
		if _, err := h.server.TeamWatch(ctx, TeamCall{Session: h.super.ID, Task: 1, CallID: "w" + n},
			team.WatchCall{Action: team.WatchAdd, Pull: "frostyard/example#" + n, Until: team.UntilChecks}); err != nil {
			t.Fatal(err)
		}
	}
	pending = false
	if err := h.server.watchPass(ctx); err != nil {
		t.Fatal(err)
	}
	if ci != 1 {
		t.Fatalf("the pass read CI %d times", ci)
	}
	for n, title := range map[string]string{"7": "(Fix CI on main)", "8": "(Fix CI on release)"} {
		w := h.watchOf(TeamCall{Session: h.super.ID, Task: 1, CallID: "w" + n})
		note, err := h.store.Note(ctx, w.NoteKey())
		if err != nil || !strings.Contains(note.Text, title) || !strings.Contains(note.Text, "failing: verify") {
			t.Errorf("#%s's note %q %v", n, note.Text, err)
		}
	}
}
