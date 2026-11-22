package api

import (
	"context"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/frostyard/pocketgo/proto"
	"github.com/frostyard/pocketgo/service/internal/store"
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
		if _, err := h.server.TeamGitHub(ctx, TeamCall{Session: session, Task: 1, CallID: "r"}, read); err == nil || !strings.Contains(err.Error(), "this session is none") {
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

// deliverTask delivers the approved plan's task t1 to Vera, and is its
// thread, with its session.
func deliverTask(t *testing.T, h *planHarness) store.ThreadRow {
	t.Helper()
	ctx := context.Background()
	h.settled()
	h.approve("a1")
	if _, err := h.server.TeamDelegate(ctx, TeamCall{Session: h.super.ID, Task: 5, CallID: "d1"},
		team.Delegation{Agent: "vera", Task: "Build it.", Repository: "frostyard/example", Plan: &team.PlanRef{ID: h.plan, Task: "t1"}}); err != nil {
		t.Fatal(err)
	}
	threads, err := h.store.Threads(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, th := range threads {
		if th.PlanTask == "t1" && th.Session != 0 {
			return th
		}
	}
	t.Fatalf("no session for the plan task's thread: %+v", threads)
	return store.ThreadRow{}
}

// A consult's session and a plan's lead read GitHub with team_github too,
// as the owner chose on 2026-11-21; a plan task's specialist and the
// supervisor are refused before gh runs (ADR-0036).
func TestAConsultAndAPlanLeadReadGitHub(t *testing.T) {
	h := newPlanHarness(t)
	ctx := context.Background()
	h.engine.mu.Lock()
	h.engine.onGH = func(context.Context, []string) (string, error) { return `{"number":7}`, nil }
	h.engine.mu.Unlock()
	read := team.GitHubRead{Path: "repos/frostyard/example/issues/7"}
	if text, err := h.server.TeamGitHub(ctx, TeamCall{Session: h.thread.Session, Task: 3, CallID: "r1"}, read); err != nil || text != `{"number":7}` {
		t.Fatalf("the plan's lead: %q, %v", text, err)
	}
	if _, err := h.lead("k", team.PlanCall{Action: team.PlanConsult, Agents: []string{"irulan"}, Question: "Holds?"}); err != nil {
		t.Fatal(err)
	}
	h.runScheduled(5)
	consult := h.facts().Consults[0]
	if consult.Session == 0 {
		t.Fatalf("the consult has no session: %+v", consult)
	}
	if text, err := h.server.TeamGitHub(ctx, TeamCall{Session: consult.Session, Task: 3, CallID: "r2"}, read); err != nil || text != `{"number":7}` {
		t.Fatalf("the consult: %q, %v", text, err)
	}
	// A plan task's specialist, in another plan, and the supervisor are
	// refused, before gh runs.
	other := newPlanHarness(t)
	task := deliverTask(t, other)
	for name, session := range map[string]int64{"a plan task's specialist": task.Session, "the supervisor": other.super.ID} {
		if _, err := other.server.TeamGitHub(ctx, TeamCall{Session: session, Task: 3, CallID: "r"}, read); err == nil || !strings.Contains(err.Error(), "this session is none") {
			t.Errorf("%s: %v", name, err)
		}
	}
	other.engine.mu.Lock()
	defer other.engine.mu.Unlock()
	if len(other.engine.ghCalls) != 0 {
		t.Fatalf("a refused call ran gh: %q", other.engine.ghCalls)
	}
}

// A plan's lead and a consult whose session an earlier run may have made
// before team_github get it before their brief or question, which name
// it, is sent; a plan task's specialist is not given it (Codex review of
// #61, round 2).
func TestAPlanLeadAndAConsultGetTeamGitHubBeforeTheirBriefs(t *testing.T) {
	h := newPlanHarness(t)
	if _, err := h.lead("k", team.PlanCall{Action: team.PlanConsult, Agents: []string{"irulan"}, Question: "Holds?"}); err != nil {
		t.Fatal(err)
	}
	h.runScheduled(5)
	consult := h.facts().Consults[0]
	if consult.Session == 0 || len(h.sentTo(consult.Session)) != 1 || len(h.sentTo(h.thread.Session)) == 0 {
		t.Fatalf("the consult %+v got %q, the lead %q", consult, h.sentTo(consult.Session), h.sentTo(h.thread.Session))
	}
	h.engine.mu.Lock()
	given, late := maps.Clone(h.engine.githubTool), maps.Clone(h.engine.githubLate)
	h.engine.mu.Unlock()
	for name, session := range map[string]int64{"the plan's lead": h.thread.Session, "the consult": consult.Session} {
		if !given[session] || late[session] {
			t.Errorf("%s (session %d): given %v, after a message %v", name, session, given, late)
		}
	}
	other := newPlanHarness(t)
	task := deliverTask(t, other)
	other.engine.mu.Lock()
	defer other.engine.mu.Unlock()
	if other.engine.githubTool[task.Session] {
		t.Fatalf("a plan task's specialist was given team_github: %v", other.engine.githubTool)
	}
}

// The pin bump's two commits and the pinned repository, as on frostyard/std#122
// (2026-11-22): the one line the pull request changes moves core's
// reusable workflow from the old commit to the new one.
var (
	oldPin = "981e7ae" + strings.Repeat("1", 33)
	newPin = "f99acfd" + strings.Repeat("2", 33)
)

const pinnedCompare = `{"status":"ahead","ahead_by":1,"commits":[{"sha":"f99acfd","commit":{"message":"Triage drafts too"}}],` +
	`"files":[{"filename":"scripts/triage.mjs","status":"modified","patch":"@@ -1 +1 @@\n-if (pr.draft) return;\n+label(pr);"}]}`

// answerPinnedCompare has gh answer the pinned repository's compare
// between the pin's two commits, and every other call as h.gh does; it
// returns the compare's path.
func answerPinnedCompare(h *reviewHarness) string {
	compare := "repos/frostyard/core/compare/" + oldPin + "..." + newPin
	h.engine.mu.Lock()
	defer h.engine.mu.Unlock()
	h.engine.onGH = func(ctx context.Context, args []string) (string, error) {
		if slices.Contains(args, compare) {
			return pinnedCompare, nil
		}
		return h.gh.gh(ctx, args)
	}
	return compare
}

// A cross-review reads GitHub with team_github, as the owner's issue #105
// asked: a pin bump's reviewer reads the pinned repository's compare
// between the old and the new commit, with GET alone. The review's
// session gets the tool before its brief, which names it; the author's
// thread and a thread the supervisor delegated, though it reviews the same
// pull request, are refused before gh runs (ADR-0057).
func TestACrossReviewReadsGitHubAndNoOtherThreadOfItsPullRequestDoes(t *testing.T) {
	h := newReviewHarness(t)
	ctx := context.Background()
	h.engine.mu.Lock()
	given, late := h.engine.githubTool[h.teg.Session], h.engine.githubLate[h.teg.Session]
	h.engine.mu.Unlock()
	if !given || late {
		t.Fatalf("the review's session: given team_github %v, after its brief %v", given, late)
	}
	if !strings.Contains(h.teg.Brief, "team_github") || !strings.Contains(h.teg.Brief, "compare/<old>...<new>") {
		t.Fatalf("the brief does not name team_github for a pin bump:\n%s", h.teg.Brief)
	}
	compare := answerPinnedCompare(h)
	text, err := h.server.TeamGitHub(ctx, TeamCall{Session: h.teg.Session, Task: 4, CallID: "g1"}, team.GitHubRead{Path: compare})
	if err != nil || !strings.Contains(text, `"patch":"@@ -1 +1 @@\n-if (pr.draft) return;\n+label(pr);"`) {
		t.Fatalf("the review's read: %q, %v", text, err)
	}
	for name, session := range map[string]int64{"the author's thread": h.vera.Session, "a thread the supervisor delegated": h.duncan.Session} {
		if session == 0 {
			t.Fatalf("%s has no session", name)
		}
		if _, err := h.server.TeamGitHub(ctx, TeamCall{Session: session, Task: 4, CallID: "g2"}, team.GitHubRead{Path: compare}); err == nil ||
			!strings.Contains(err.Error(), "this session is none") {
			t.Errorf("%s: %v", name, err)
		}
	}
	h.engine.mu.Lock()
	calls := slices.Clone(h.engine.ghCalls)
	h.engine.mu.Unlock()
	var reads []string
	for _, c := range calls {
		if strings.Contains(c, "frostyard/core") {
			reads = append(reads, c)
		}
	}
	if want := "api --hostname github.com --method GET -- " + compare; len(reads) != 1 || reads[0] != want {
		t.Fatalf("gh read the pinned repository %q, want %q once", reads, want)
	}
}

// At start, the session of each review thread not closed is given
// team_github, as a review's team_verdict was; the author's thread, a
// thread the supervisor delegated and the supervisor are not, nor a review
// that closed (ADR-0057).
func TestAnOpenReviewGetsTeamGitHubAtStart(t *testing.T) {
	h := newReviewHarness(t)
	ctx := context.Background()
	// As sessions made before the tool were: none has it.
	h.engine.mu.Lock()
	h.engine.githubTool = nil
	h.engine.mu.Unlock()
	if err := h.server.UpgradeTeamTools(ctx); err != nil {
		t.Fatal(err)
	}
	h.engine.mu.Lock()
	given := maps.Clone(h.engine.githubTool)
	h.engine.githubTool = nil
	h.engine.mu.Unlock()
	if len(given) != 1 || !given[h.teg.Session] {
		t.Fatalf("given team_github: %v; want the review's session %d alone", given, h.teg.Session)
	}
	if _, err := h.verdict(h.teg.Session, "v1", headA, team.VerdictReady); err != nil {
		t.Fatal(err)
	}
	if review, err := h.store.Thread(ctx, h.teg.ID); err != nil || review.State != store.ThreadClosed {
		t.Fatalf("the review after its verdict: %+v, %v", review, err)
	}
	if err := h.server.UpgradeTeamTools(ctx); err != nil {
		t.Fatal(err)
	}
	h.engine.mu.Lock()
	defer h.engine.mu.Unlock()
	if len(h.engine.githubTool) != 0 {
		t.Fatalf("a closed review was given team_github: %v", h.engine.githubTool)
	}
}

// A pull request that changes only a pin, end to end with real git: the
// review's team_diff shows the one line that moves the pin and nothing of
// what it brings in, and its team_github reads the pinned repository's
// change between the two commits (ADR-0057; frostyard/pocketgo#105).
func TestAPinBumpsReviewReadsThePinnedChange(t *testing.T) {
	needsReviewGit(t)
	th := newTeamHarness(t)
	h := &reviewHarness{teamHarness: th}
	h.engine.more = []proto.Model{{ModelRef: proto.ModelRef{Provider: "anthropic", ID: "claude-opus-5-5"}},
		{ModelRef: proto.ModelRef{Provider: "openai-codex", ID: "gpt-6.1-sol"}}}
	h.writeProfiles(map[string]string{
		"odrade.md": "---\nrole: supervisor\nmodel: faux/faux-1\nhome: ~\n---\n# Odrade\n",
		"vera.md":   "---\nmodel: anthropic/claude-opus-5-5\nrepositories:\n  frostyard/example: " + h.checkout + "\n---\n# Vera\n",
	})
	h.server.reviewAsync, h.server.reviewsOn = false, true
	h.server.repoURL = func(string) string { return h.checkout }
	ctx := context.Background()
	git := func(dir string, args ...string) string {
		t.Helper()
		out, err := h.engine.Git(ctx, dir, args...)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	commit := func(dir, message string) {
		t.Helper()
		git(dir, "add", "-A")
		git(dir, "-c", "user.email=t@example.com", "-c", "user.name=t", "commit", "-q", "-m", message)
	}
	workflow := filepath.Join(".github", "workflows", "triage.yml")
	pinned := func(dir, sha string) {
		t.Helper()
		text := "on: pull_request\njobs:\n  triage:\n    uses: frostyard/core/.github/workflows/pr-triage.yml@" + sha + "\n"
		if err := os.MkdirAll(filepath.Join(dir, ".github", "workflows"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, workflow), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// main pins the old commit; the pull request moves the pin alone.
	pinned(h.checkout, oldPin)
	commit(h.checkout, "pin core")
	h.vera = veraThread(t, th)
	pinned(h.vera.Cwd, newPin)
	commit(h.vera.Cwd, "Bump core's pr-triage.yml")
	head := git(h.vera.Cwd, "rev-parse", "HEAD")
	base := git(h.checkout, "rev-parse", "main")
	h.gh = &fakeGitHub{repo: "frostyard/example", number: 3, branch: h.vera.Branch, head: head, state: "open", labels: map[string]bool{},
		ci: map[string]string{head: greenRuns}, base: base}
	compare := answerPinnedCompare(h)
	if _, err := h.server.TeamMessage(ctx, TeamCall{Session: h.vera.Session, Task: 1, CallID: "m1"},
		team.Report{Message: "PR #3 bumps core.", Status: team.StatusDone, PullRequests: []team.PullRef{{Number: 3}}}); err != nil {
		t.Fatal(err)
	}
	threads, err := h.store.ReviewThreadsOf(ctx, h.pull(t).ID)
	if err != nil || len(threads) != 1 || threads[0].Session == 0 {
		t.Fatalf("review threads %+v, %v", threads, err)
	}
	review := threads[0]
	diff, err := h.server.TeamDiff(ctx, TeamCall{Session: review.Session, Task: 1, CallID: "d1"}, team.DiffRequest{})
	if err != nil {
		t.Fatal(err)
	}
	var changed []string
	for _, line := range strings.Split(diff, "\n") {
		if (strings.HasPrefix(line, "-") || strings.HasPrefix(line, "+")) && !strings.HasPrefix(line, "---") && !strings.HasPrefix(line, "+++") {
			changed = append(changed, line)
		}
	}
	if want := []string{"-    uses: frostyard/core/.github/workflows/pr-triage.yml@" + oldPin, "+    uses: frostyard/core/.github/workflows/pr-triage.yml@" + newPin}; !slices.Equal(changed, want) {
		t.Fatalf("team_diff changed %q, want the pin alone %q:\n%s", changed, want, diff)
	}
	if strings.Contains(diff, "pr.draft") {
		t.Fatalf("team_diff showed the pinned repository's change:\n%s", diff)
	}
	text, err := h.server.TeamGitHub(ctx, TeamCall{Session: review.Session, Task: 2, CallID: "g1"}, team.GitHubRead{Path: compare})
	if err != nil || !strings.Contains(text, `-if (pr.draft) return;\n+label(pr);`) || !strings.Contains(text, `"ahead_by":1`) {
		t.Fatalf("the review's read of the pinned change: %q, %v", text, err)
	}
}

// At start, the lead's session of each plan thread not closed is given
// team_github, which a lead started before it lacks; a plan task's
// specialist and the supervisor are not, nor the lead of a plan whose
// thread closed (ADR-0036).
func TestAnOlderPlanLeadGetsTeamGitHubAtStart(t *testing.T) {
	h := newPlanHarness(t)
	ctx := context.Background()
	deliverTask(t, h)
	// As sessions made before the tool were: none has it.
	h.engine.mu.Lock()
	h.engine.githubTool = nil
	h.engine.mu.Unlock()
	if err := h.server.UpgradeTeamTools(ctx); err != nil {
		t.Fatal(err)
	}
	h.engine.mu.Lock()
	given := maps.Clone(h.engine.githubTool)
	h.engine.githubTool = nil
	h.engine.mu.Unlock()
	if len(given) != 1 || !given[h.thread.Session] {
		t.Fatalf("given team_github: %v; want the lead's session %d alone", given, h.thread.Session)
	}
	if _, err := h.client.CloseThread(ctx, h.thread.ID, proto.CloseThread{RequestID: "close"}); err != nil {
		t.Fatal(err)
	}
	if err := h.server.UpgradeTeamTools(ctx); err != nil {
		t.Fatal(err)
	}
	h.engine.mu.Lock()
	defer h.engine.mu.Unlock()
	if len(h.engine.githubTool) != 0 {
		t.Fatalf("a closed plan's lead was given team_github: %v", h.engine.githubTool)
	}
}
