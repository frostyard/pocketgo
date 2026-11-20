package api

import (
	"context"
	"strings"
	"testing"

	"github.com/frostyard/pocketgo/proto"
	"github.com/frostyard/pocketgo/service/internal/store"
	"github.com/frostyard/pocketgo/service/internal/team"
)

// specOf is the spec the engine created thread's session from.
func (h *reviewHarness) specOf(t *testing.T, thread int64) SessionSpec {
	t.Helper()
	h.engine.mu.Lock()
	defer h.engine.mu.Unlock()
	for _, spec := range h.engine.specs {
		if spec.Thread == thread {
			return spec
		}
	}
	t.Fatalf("no session was created for T%d", thread)
	return SessionSpec{}
}

// sentTo are the texts sent to session, in no order.
func (h *reviewHarness) sentTo(session int64) []string {
	h.engine.mu.Lock()
	defer h.engine.mu.Unlock()
	var out []string
	for key, to := range h.engine.to {
		if to == session {
			out = append(out, h.engine.texts[key])
		}
	}
	return out
}

// A linked pull request gets one review, at its head, started by pocketd:
// the author's own agent on the reviewer model of another family, with
// read-only tools and pocketd's brief. More passes start none, and the
// supervisor is told nothing (ADR-0019).
func TestPocketdStartsOneReviewPerHead(t *testing.T) {
	h := newReviewHarness(t)
	ctx := context.Background()
	review, err := h.store.Thread(ctx, h.teg.ID)
	if err != nil {
		t.Fatal(err)
	}
	if review.Key != team.ReviewKey("frostyard/example", 3, headA) || review.Agent != "vera" || review.ReviewHead != headA ||
		review.Repository != "frostyard/example" || review.SupervisorSession != h.super.ID {
		t.Fatalf("review thread %+v", review)
	}
	spec := h.specOf(t, review.ID)
	if spec.Model != (proto.ModelRef{Provider: "openai-codex", ID: "gpt-6.1-sol"}) || spec.Route != "cross-review" || !spec.Review {
		t.Fatalf("the review's session %+v", spec)
	}
	for _, want := range []string{"git diff --no-ext-diff --no-textconv " + reviewBase + " " + headA, "read-only",
		"exactly one team_verdict for frostyard/example#3 at head " + headA} {
		if !strings.Contains(review.Brief, want) {
			t.Errorf("the brief lacks %q:\n%s", want, review.Brief)
		}
	}
	supervisor := len(h.sentTo(h.super.ID))
	h.server.reviewPass(ctx)
	h.server.reviewPass(ctx)
	if threads, err := h.store.ReviewThreadsOf(ctx, h.pull(t).ID); err != nil || len(threads) != 1 {
		t.Fatalf("review threads after more passes: %d, %v", len(threads), err)
	}
	if got := len(h.sentTo(h.super.ID)); got != supervisor {
		t.Fatalf("the supervisor was told of the review: %d messages, had %d", got, supervisor)
	}
}

// A verdict closes the review thread, and the author's thread hears it
// once; the supervisor hears nothing (ADR-0019).
func TestAVerdictEndsTheReviewAndTellsTheAuthorOnce(t *testing.T) {
	h := newReviewHarness(t)
	ctx := context.Background()
	supervisor := len(h.sentTo(h.super.ID))
	answer, err := h.verdict(h.teg.Session, "v1", headA, team.VerdictNotReady, "high blocking: the retry never ends")
	if err != nil || strings.Contains(answer, "team_message") {
		t.Fatalf("answer %q, %v", answer, err)
	}
	review, err := h.store.Thread(ctx, h.teg.ID)
	if err != nil || review.State != store.ThreadClosed || !strings.Contains(review.Report, "not-ready") {
		t.Fatalf("the review after its verdict: %+v, %v", review, err)
	}
	for range 2 {
		if _, err := h.server.deliverAll(ctx); err != nil {
			t.Fatal(err)
		}
	}
	var told []string
	for _, text := range h.sentTo(h.vera.Session) {
		if strings.Contains(text, "cross-review") {
			told = append(told, text)
		}
	}
	if len(told) != 1 || !strings.Contains(told[0], "[T1 · cross-review · not-ready]") || !strings.Contains(told[0], "the retry never ends") {
		t.Fatalf("the author was told %q", told)
	}
	if got := len(h.sentTo(h.super.ID)); got != supervisor {
		t.Fatalf("the supervisor was told: %d messages, had %d", got, supervisor)
	}
}

// A push ends the review of the old head, stopping its session, and the
// next pass reviews the new head (ADR-0019).
func TestAPushSupersedesTheReview(t *testing.T) {
	h := newReviewHarness(t)
	ctx := context.Background()
	h.engine.mu.Lock()
	h.engine.runFirst = 9 // the review is at work
	h.engine.mu.Unlock()
	h.gh.set(func(g *fakeGitHub) { g.head = headB })
	if _, err := h.server.RefreshPulls(ctx); err != nil {
		t.Fatal(err)
	}
	next := h.reviewAt(t, headB)
	old, err := h.store.Thread(ctx, h.teg.ID)
	if err != nil || old.State != store.ThreadClosed || !strings.Contains(old.Report, "Superseded") {
		t.Fatalf("the old review: %+v, %v", old, err)
	}
	h.engine.mu.Lock()
	aborted := h.engine.aborted
	h.engine.mu.Unlock()
	if len(aborted) != 1 || aborted[0] != h.teg.Session {
		t.Fatalf("aborted %v, want the old review's session %d", aborted, h.teg.Session)
	}
	if next.ID == h.teg.ID || next.Title != team.ReviewTitle("frostyard/example", 3, headB, 2) {
		t.Fatalf("the new review %+v", next)
	}
}

// The owner's budget: 4 reviews of a pull request, then none (ADR-0019).
func TestTheReviewBudgetIsFour(t *testing.T) {
	h := newReviewHarness(t)
	ctx := context.Background()
	for _, head := range []string{headB, strings.Repeat("c", 40), strings.Repeat("d", 40), strings.Repeat("f", 40)} {
		h.gh.set(func(g *fakeGitHub) { g.head = head })
		if _, err := h.server.RefreshPulls(ctx); err != nil {
			t.Fatal(err)
		}
		h.server.reviewPass(ctx)
	}
	threads, err := h.store.ReviewThreadsOf(ctx, h.pull(t).ID)
	if err != nil || len(threads) != team.MaxReviews {
		t.Fatalf("%d review threads, %v, want %d", len(threads), err, team.MaxReviews)
	}
	for _, th := range threads {
		if th.ReviewHead == strings.Repeat("f", 40) {
			t.Fatalf("a fifth review started: %+v", th)
		}
	}
}

// A review whose session ended with no verdict ends, and counts: no other
// starts at the same head (ADR-0019).
func TestAReviewWithNoVerdictEnds(t *testing.T) {
	h := newReviewHarness(t)
	ctx := context.Background()
	if review, err := h.store.Thread(ctx, h.teg.ID); err != nil || review.State != store.ThreadOpen || !review.Briefed {
		t.Fatalf("the review before: %+v, %v", review, err)
	}
	h.server.reviewPass(ctx) // its session is idle, with no verdict
	review, err := h.store.Thread(ctx, h.teg.ID)
	if err != nil || review.State != store.ThreadClosed || !strings.Contains(review.Report, "no verdict") {
		t.Fatalf("the review after: %+v, %v", review, err)
	}
	h.server.reviewPass(ctx)
	if threads, err := h.store.ReviewThreadsOf(ctx, h.pull(t).ID); err != nil || len(threads) != 1 {
		t.Fatalf("review threads %d, %v", len(threads), err)
	}
}

// An author of no model family pocketd knows gets no review: the owner
// decides (ADR-0019).
func TestNoReviewForAnAuthorOfNoKnownFamily(t *testing.T) {
	h := newTeamHarness(t) // vera runs faux/faux-1
	h.server.reviewAsync, h.server.reviewGit = false, fakeReviewGit{}
	ctx := context.Background()
	vera := veraThread(t, h)
	gh := &fakeGitHub{repo: "frostyard/example", number: 3, branch: vera.Branch, head: headA, state: "open", labels: map[string]bool{},
		ci: map[string]string{headA: greenRuns}}
	h.engine.mu.Lock()
	h.engine.onGH = gh.gh
	h.engine.mu.Unlock()
	if _, err := h.server.TeamMessage(ctx, TeamCall{Session: vera.Session, Task: 1, CallID: "m1"},
		team.Report{Message: "PR #3 is open.", Status: team.StatusDone, PullRequests: []team.PullRef{{Number: 3}}}); err != nil {
		t.Fatal(err)
	}
	p, found, err := h.store.PullByRef(ctx, "frostyard/example", 3)
	if err != nil || !found {
		t.Fatalf("pull %v %v", found, err)
	}
	h.server.reviewPass(ctx)
	if threads, err := h.store.ReviewThreadsOf(ctx, p.ID); err != nil || len(threads) != 0 {
		t.Fatalf("review threads %+v, %v", threads, err)
	}
}

// A review end to end with real git: the folder is a worktree of the
// author's checkout detached at the pull request's real head, and the brief
// names where that head leaves the base branch (ADR-0019).
func TestAReviewEndToEndWithRealGit(t *testing.T) {
	th := newTeamHarness(t)
	h := &reviewHarness{teamHarness: th}
	h.engine.more = []proto.Model{{ModelRef: proto.ModelRef{Provider: "anthropic", ID: "claude-opus-5-5"}},
		{ModelRef: proto.ModelRef{Provider: "openai-codex", ID: "gpt-6.1-sol"}}}
	h.writeProfiles(map[string]string{
		"odrade.md": "---\nrole: supervisor\nmodel: faux/faux-1\nhome: ~\n---\n# Odrade\n",
		"vera.md":   "---\nmodel: anthropic/claude-opus-5-5\nrepositories:\n  frostyard/example: " + h.checkout + "\n---\n# Vera\n",
	})
	h.server.reviewAsync = false
	h.server.repoURL = func(string) string { return h.checkout }
	ctx := context.Background()
	h.vera = veraThread(t, th)
	git := func(dir string, args ...string) string {
		t.Helper()
		out, err := h.engine.Git(ctx, dir, args...)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	git(h.vera.Cwd, "-c", "user.email=t@example.com", "-c", "user.name=t", "commit", "-q", "--allow-empty", "-m", "the change")
	head := git(h.vera.Cwd, "rev-parse", "HEAD")
	base := git(h.checkout, "rev-parse", "main")
	h.gh = &fakeGitHub{repo: "frostyard/example", number: 3, branch: h.vera.Branch, head: head, state: "open", labels: map[string]bool{},
		ci: map[string]string{head: greenRuns}}
	h.engine.mu.Lock()
	h.engine.onGH = h.gh.gh
	h.engine.mu.Unlock()
	if _, err := h.server.TeamMessage(ctx, TeamCall{Session: h.vera.Session, Task: 1, CallID: "m1"},
		team.Report{Message: "PR #3 is open.", Status: team.StatusDone, PullRequests: []team.PullRef{{Number: 3}}}); err != nil {
		t.Fatal(err)
	}
	threads, err := h.store.ReviewThreadsOf(ctx, h.pull(t).ID)
	if err != nil || len(threads) != 1 || threads[0].Session == 0 {
		t.Fatalf("review threads %+v, %v", threads, err)
	}
	review := threads[0]
	if got := git(review.Cwd, "rev-parse", "HEAD"); got != head {
		t.Fatalf("the review's folder is at %s, want %s", got, head)
	}
	if got := git(review.Cwd, "rev-parse", "--symbolic-full-name", "HEAD"); got != "HEAD" {
		t.Fatalf("the review's folder is on %s, not detached", got)
	}
	if !strings.Contains(review.Brief, "git diff --no-ext-diff --no-textconv "+base+" "+head) {
		t.Fatalf("the brief does not name the change set %s..%s:\n%s", base, head, review.Brief)
	}
	// Its verdict counts: a ready verdict at the real head, CI green.
	if _, err := h.verdict(review.Session, "v1", head, team.VerdictReady); err != nil {
		t.Fatal(err)
	}
	if p := h.pull(t); !p.Ready {
		t.Fatalf("pull %+v", p)
	}
}
