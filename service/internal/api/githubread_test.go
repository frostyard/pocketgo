package api

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/frostyard/pocketgo/service/internal/team"
)

// A gather's proposal session reads GitHub with team_github: pocketd runs
// gh api with its own arguments, GET alone, through the engine's gh in the
// sandbox, bounded in time, and answers with the compacted JSON
// (ADR-0036).
func TestAProposalSessionReadsGitHubWithGET(t *testing.T) {
	h := newGatherHarness(t, "")
	ctx := context.Background()
	h.gather("g1", team.Gather{Skill: "grooming", Agents: []string{"duncan"}})
	duncan := h.askOf(1, "duncan")
	bounded := false
	h.engine.mu.Lock()
	h.engine.onGH = func(ctx context.Context, args []string) (string, error) {
		_, bounded = ctx.Deadline()
		return "[\n  {\"number\": 7, \"title\": \"Flaky test\"}\n]\n", nil
	}
	h.engine.mu.Unlock()
	text, err := h.server.TeamGitHub(ctx, TeamCall{Session: duncan.Session, Task: 3, CallID: "r1"},
		team.GitHubRead{Path: "/repos/frostyard/example/issues", Query: map[string]string{"state": "open", "labels": "bug"}})
	if err != nil || text != `[{"number":7,"title":"Flaky test"}]` {
		t.Fatalf("the read: %q, %v", text, err)
	}
	h.engine.mu.Lock()
	calls := h.engine.ghCalls
	h.engine.mu.Unlock()
	if want := "api --hostname github.com --method GET -- repos/frostyard/example/issues?labels=bug&state=open"; len(calls) != 1 || calls[0] != want {
		t.Fatalf("gh ran %q, want %q", calls, want)
	}
	if !bounded {
		t.Fatal("the read ran with no deadline")
	}
	// Once its ask has ended the session still reads, as team_propose list
	// still lists.
	if _, err := h.server.TeamPropose(ctx, TeamCall{Session: duncan.Session, Task: 4, CallID: "d1"}, team.ProposeCall{Action: team.ProposeDone}); err != nil {
		t.Fatal(err)
	}
	if text, err := h.server.TeamGitHub(ctx, TeamCall{Session: duncan.Session, Task: 5, CallID: "r2"}, team.GitHubRead{Path: "rate_limit"}); err != nil || text == "" {
		t.Fatalf("a read after the ask's end: %q, %v", text, err)
	}
}

// team_github refuses, before gh runs, a caller that answers no gather
// (the supervisor, a plain session) and a path it does not take; gh's
// failure, a read past its time, and an abort come back as errors
// (ADR-0036).
func TestTeamGitHubRefuses(t *testing.T) {
	h := newGatherHarness(t, "")
	ctx := context.Background()
	h.gather("g1", team.Gather{Skill: "grooming", Agents: []string{"duncan"}})
	duncan := h.askOf(1, "duncan")
	h.engine.mu.Lock()
	h.engine.onGH = func(context.Context, []string) (string, error) { return "{}", nil }
	h.engine.mu.Unlock()
	read := team.GitHubRead{Path: "repos/frostyard/example/issues"}
	for name, session := range map[string]int64{"the supervisor": h.odrade, "another supervisor": h.leto, "no session": 999} {
		if _, err := h.server.TeamGitHub(ctx, TeamCall{Session: session, Task: 1, CallID: "r"}, read); err == nil || !strings.Contains(err.Error(), "answers no gather") {
			t.Errorf("%s: %v", name, err)
		}
	}
	for _, path := range []string{"graphql", "-f", "https://evil.example/x", "repos/../user", "repos/{owner}/x", "repos/a?x=1"} {
		if _, err := h.server.TeamGitHub(ctx, TeamCall{Session: duncan.Session, Task: 1, CallID: "r"}, team.GitHubRead{Path: path}); err == nil {
			t.Errorf("the path %q was read", path)
		}
	}
	h.engine.mu.Lock()
	calls := h.engine.ghCalls
	h.engine.mu.Unlock()
	if len(calls) != 0 {
		t.Fatalf("a refused call ran gh: %q", calls)
	}

	h.engine.mu.Lock()
	h.engine.onGH = func(context.Context, []string) (string, error) { return "", errors.New("gh: Not Found (HTTP 404)") }
	h.engine.mu.Unlock()
	if _, err := h.server.TeamGitHub(ctx, TeamCall{Session: duncan.Session, Task: 1, CallID: "r"}, read); err == nil || !strings.Contains(err.Error(), "HTTP 404") {
		t.Fatalf("gh's failure: %v", err)
	}
	h.engine.mu.Lock()
	h.engine.onGH = func(context.Context, []string) (string, error) { return "PK\x03\x04\xff", nil }
	h.engine.mu.Unlock()
	if _, err := h.server.TeamGitHub(ctx, TeamCall{Session: duncan.Session, Task: 1, CallID: "r"}, read); err == nil || !strings.Contains(err.Error(), "not text") {
		t.Fatalf("an answer that is not text: %v", err)
	}

	// An abort of the call stops the read, and is no timeout.
	aborted, abort := context.WithCancel(ctx)
	h.engine.mu.Lock()
	h.engine.onGH = func(ctx context.Context, _ []string) (string, error) { abort(); <-ctx.Done(); return "", ctx.Err() }
	h.engine.mu.Unlock()
	if _, err := h.server.TeamGitHub(aborted, TeamCall{Session: duncan.Session, Task: 1, CallID: "r"}, read); err == nil || !errors.Is(err, context.Canceled) || strings.Contains(err.Error(), "did not answer") {
		t.Fatalf("an aborted read: %v", err)
	}
}

// A read asks for team.GitHubReadTime, through the server's timeout hook,
// and one past it stops and says so: no wall clock decides either (Codex
// review of #61, round 1).
func TestAGitHubReadIsBoundedByItsTime(t *testing.T) {
	h := newGatherHarness(t, "")
	ctx := context.Background()
	var mu sync.Mutex
	var bounds []time.Duration
	expire := false
	h.server.withTimeout = func(c context.Context, d time.Duration) (context.Context, context.CancelFunc) {
		mu.Lock()
		defer mu.Unlock()
		bounds = append(bounds, d)
		if expire && d == team.GitHubReadTime {
			return context.WithDeadline(c, time.Unix(0, 0))
		}
		return context.WithTimeout(c, d)
	}
	h.gather("g1", team.Gather{Skill: "grooming", Agents: []string{"duncan"}})
	duncan := h.askOf(1, "duncan")
	h.engine.mu.Lock()
	h.engine.onGH = func(ctx context.Context, _ []string) (string, error) {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		return "{}", nil
	}
	h.engine.mu.Unlock()
	read := team.GitHubRead{Path: "repos/frostyard/example/issues"}
	mu.Lock()
	bounds = nil
	mu.Unlock()
	if text, err := h.server.TeamGitHub(ctx, TeamCall{Session: duncan.Session, Task: 1, CallID: "r1"}, read); err != nil || text != "{}" {
		t.Fatalf("the read: %q, %v", text, err)
	}
	mu.Lock()
	asked := slices.Clone(bounds)
	expire = true
	mu.Unlock()
	if !slices.Equal(asked, []time.Duration{team.GitHubReadTime}) {
		t.Fatalf("the read asked for the bounds %v, want [%v]", asked, team.GitHubReadTime)
	}
	if _, err := h.server.TeamGitHub(ctx, TeamCall{Session: duncan.Session, Task: 1, CallID: "r2"}, read); err == nil || !strings.Contains(err.Error(), "did not answer within 30s") {
		t.Fatalf("a read past its time: %v", err)
	}
}
