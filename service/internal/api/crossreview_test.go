package api

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
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
	if review.Key != team.ReviewKey("frostyard/example", 3, "main", headA) || review.Agent != "vera" || review.ReviewHead != headA ||
		review.Repository != "frostyard/example" || review.SupervisorSession != h.super.ID {
		t.Fatalf("review thread %+v", review)
	}
	spec := h.specOf(t, review.ID)
	if spec.Model != (proto.ModelRef{Provider: "openai-codex", ID: "gpt-6.1-sol"}) || spec.Route != "cross-review" || !spec.Review {
		t.Fatalf("the review's session %+v", spec)
	}
	for _, want := range []string{"git diff " + reviewBase + " " + headA, "read-only", "team_diff",
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
	h.server.reviewAsync, h.server.reviewGit, h.server.reviewsOn = false, fakeReviewGit{}, true
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
	var fetched []string
	h.engine.mu.Lock()
	h.engine.onGit = func(args []string) {
		if slices.Contains(args, "fetch") {
			fetched = append(fetched, strings.Join(args, " "))
		}
	}
	h.engine.mu.Unlock()
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
		ci: map[string]string{head: greenRuns}, base: base}
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
	if !strings.Contains(review.Brief, "git diff "+base+" "+head) {
		t.Fatalf("the brief does not name the change set %s..%s:\n%s", base, head, review.Brief)
	}
	// The base came from GitHub's compare, and the head was in the
	// checkout: no git fetch ran (Codex review of #26, round 1).
	if len(fetched) != 0 {
		t.Fatalf("git fetched %q", fetched)
	}
	// team_diff reads that change set with git, in the review's folder.
	stat, err := h.server.TeamDiff(ctx, TeamCall{Session: review.Session, Task: 1, CallID: "d1"}, team.DiffRequest{Log: true})
	if err != nil || !strings.Contains(stat, "the change") || strings.Contains(stat, "[bytes") {
		t.Fatalf("team_diff: %q, %v", stat, err)
	}
	// A page from an offset says which bytes it holds (Codex review of
	// #26, round 2).
	page, err := h.server.TeamDiff(ctx, TeamCall{Session: review.Session, Task: 1, CallID: "d2"}, team.DiffRequest{Log: true, Offset: 10})
	if err != nil || !strings.Contains(page, "[bytes 10 to ") {
		t.Fatalf("a page: %q, %v", page, err)
	}
	// Its verdict counts: a ready verdict at the real head, CI green.
	if _, err := h.verdict(review.Session, "v1", head, team.VerdictReady); err != nil {
		t.Fatal(err)
	}
	if p := h.pull(t); !p.Ready {
		t.Fatalf("pull %+v", p)
	}
}

// A review ends when its pull request closes or becomes a draft, though no
// open pull request names it any more (Codex review of #26, round 1).
func TestAReviewEndsWithItsPullRequest(t *testing.T) {
	for name, change := range map[string]func(g *fakeGitHub){
		"closed": func(g *fakeGitHub) { g.state = "closed" },
	} {
		t.Run(name, func(t *testing.T) {
			h := newReviewHarness(t)
			ctx := context.Background()
			h.gh.set(change)
			if _, err := h.server.RefreshPulls(ctx); err != nil {
				t.Fatal(err)
			}
			h.server.reviewPass(ctx)
			review, err := h.store.Thread(ctx, h.teg.ID)
			if err != nil || review.State != store.ThreadClosed || !strings.Contains(review.Report, "the pull request is closed") {
				t.Fatalf("the review: %+v, %v", review, err)
			}
		})
	}
}

// A review's worktree is checked detached at its head when its brief is
// placed again or its start recovers: the check a delegation's branch
// takes would close it (Codex review of #26, round 1).
func TestARecoveredReviewIsCheckedDetached(t *testing.T) {
	h := newReviewHarness(t)
	review, err := h.store.Thread(context.Background(), h.teg.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.server.recheckFolder(context.Background(), review, review.Cwd, ""); err != nil {
		t.Fatalf("a review's folder failed its check: %v", err)
	}
}

// A review that cannot start takes no place on the desk: a reviewer model
// no sign-in serves starts none (Codex review of #26, round 1).
func TestAReviewThatCannotStartTakesNoPlace(t *testing.T) {
	h := newReviewHarness(t)
	ctx := context.Background()
	h.engine.mu.Lock()
	h.engine.more = h.engine.more[:2] // no gpt-6.1-sol now
	h.engine.mu.Unlock()
	onDesk, _ := h.store.OpenThreads(ctx)
	h.gh.set(func(g *fakeGitHub) { g.head = headB })
	if _, err := h.server.RefreshPulls(ctx); err != nil {
		t.Fatal(err)
	}
	h.server.reviewPass(ctx)
	threads, _ := h.store.ReviewThreadsOf(ctx, h.pull(t).ID)
	for _, th := range threads {
		if th.ReviewHead == headB {
			t.Fatalf("a review that cannot start was recorded: %+v", th)
		}
	}
	if now, _ := h.store.OpenThreads(ctx); now >= onDesk {
		t.Fatalf("the desk holds %d threads, had %d: the superseded review should have freed one, and none taken", now, onDesk)
	}
}

// A review that ends withdraws its brief if it still waits (Codex review
// of #26, round 1).
func TestAnEndedReviewWithdrawsItsBrief(t *testing.T) {
	h := newReviewHarness(t)
	ctx := context.Background()
	review, _ := h.store.Thread(ctx, h.teg.ID)
	h.gh.set(func(g *fakeGitHub) { g.head = headB })
	if _, err := h.server.RefreshPulls(ctx); err != nil {
		t.Fatal(err)
	}
	h.server.reviewPass(ctx)
	h.engine.mu.Lock()
	withdrawals := h.engine.withdrawals
	h.engine.mu.Unlock()
	if !slices.Contains(withdrawals, attemptKey(briefKey(review), 0)) {
		t.Fatalf("withdrawals %q lack the review's brief", withdrawals)
	}
}

// A head a verdict already backs, filed before pocketd started reviews, is
// not reviewed again (Codex review of #26, round 1).
func TestNoSecondReviewOfABackedHead(t *testing.T) {
	h := newReviewHarness(t)
	ctx := context.Background()
	h.gh.set(func(g *fakeGitHub) { g.head = headB })
	p, err := h.server.RefreshPulls(ctx)
	_ = p
	if err != nil {
		t.Fatal(err)
	}
	pull := h.pull(t)
	// A verdict at headB, as a thread filed it before ADR-0019.
	if _, _, _, _, err := h.store.AddReview(ctx, pull, store.ReviewRow{Key: "legacy", PullID: pull.ID, ThreadID: h.duncan.ID, SessionID: h.duncan.Session,
		Agent: "teg", Provider: "openai-codex", Model: "gpt-6-astra", Family: "openai", HeadSHA: headB, Verdict: team.VerdictReady, Summary: "s"}, nil); err != nil {
		t.Fatal(err)
	}
	h.server.reviewPass(ctx)
	threads, _ := h.store.ReviewThreadsOf(ctx, pull.ID)
	for _, th := range threads {
		if th.ReviewHead == headB {
			t.Fatalf("a backed head was reviewed again: %+v", th)
		}
	}
}

// A review's session files its verdict and nothing else, whatever tools
// it holds; team_diff is a review's alone (Codex review of #26, round 1).
func TestAReviewSessionDoesNothingElse(t *testing.T) {
	h := newReviewHarness(t)
	ctx := context.Background()
	if _, err := h.server.TeamMessage(ctx, TeamCall{Session: h.teg.Session, Task: 1, CallID: "m"},
		team.Report{Message: "Looks fine.", Status: team.StatusDone}); err == nil || !strings.Contains(err.Error(), "only with team_verdict") {
		t.Errorf("a review's report: %v", err)
	}
	if _, err := h.server.TeamMemory(ctx, TeamCall{Session: h.teg.Session, Task: 1, CallID: "l"},
		team.Memory{Action: team.MemoryAdd, Text: "Review faster."}); err == nil || !strings.Contains(err.Error(), "keeps no lessons") {
		t.Errorf("a review's lesson: %v", err)
	}
	if _, err := h.server.TeamDiff(ctx, TeamCall{Session: h.vera.Session, Task: 1, CallID: "d"}, team.DiffRequest{}); err == nil ||
		!strings.Contains(err.Error(), "for a cross-review pocketd started") {
		t.Errorf("the author's team_diff: %v", err)
	}
	// A verdict whose findings do not say how they block is refused, and
	// the review stays open for one that does.
	if _, err := h.verdict(h.teg.Session, "bad", headA, team.VerdictNotReady, "the retry never ends"); err == nil ||
		!strings.Contains(err.Error(), "does not begin with its severity") {
		t.Errorf("a finding with no severity: %v", err)
	}
}

// A run that won the brief's placement while a review ended is stopped:
// the brief is withdrawn first, then the run checked (Codex review of #26,
// round 2).
func TestAnEndedReviewStopsARunThatWonThePlacement(t *testing.T) {
	h := newReviewHarness(t)
	ctx := context.Background()
	h.engine.mu.Lock()
	h.engine.onWithdraw = func(string) {
		h.engine.mu.Lock()
		h.engine.runFirst = 11 // the brief was placed, and its run began
		h.engine.mu.Unlock()
	}
	h.engine.mu.Unlock()
	h.gh.set(func(g *fakeGitHub) { g.head = headB })
	if _, err := h.server.RefreshPulls(ctx); err != nil {
		t.Fatal(err)
	}
	h.server.reviewPass(ctx)
	h.engine.mu.Lock()
	aborted := slices.Clone(h.engine.aborted)
	h.engine.mu.Unlock()
	if !slices.Contains(aborted, h.teg.Session) {
		t.Fatalf("aborted %v: the old review's run went on", aborted)
	}
}

// A verdict on a pull request GitHub says is a draft is refused: its
// review ends with the draft (Codex review of #26, round 2).
func TestNoVerdictOnADraft(t *testing.T) {
	h := newReviewHarness(t)
	h.gh.set(func(g *fakeGitHub) { g.draft = true })
	if _, err := h.verdict(h.teg.Session, "v1", headA, team.VerdictReady); err == nil || !strings.Contains(err.Error(), "is a draft") {
		t.Fatalf("a verdict on a draft: %v", err)
	}
	if _, found, _ := h.store.LatestReview(context.Background(), h.pull(t)); found {
		t.Fatal("a verdict on a draft was recorded")
	}
}

// missingBase is fakeReviewGit whose checkout holds no base until a fetch
// of the base branch brings it.
type missingBase struct {
	fakeReviewGit
	fetched *int
}

func (m missingBase) has(context.Context, string, string) (bool, error) { return *m.fetched > 1, nil }
func (m missingBase) fetchBranch(context.Context, string, string, string) error {
	*m.fetched++
	return nil
}

// A review whose base the checkout does not hold fetches the base branch,
// and no reviewer session starts until the base is there (Codex review of
// #26, round 2).
func TestAReviewWaitsForItsBase(t *testing.T) {
	h := newReviewHarness(t)
	ctx := context.Background()
	fetched := 0
	h.server.reviewGit = missingBase{fetched: &fetched}
	h.gh.set(func(g *fakeGitHub) { g.head = headB })
	if _, err := h.server.RefreshPulls(ctx); err != nil {
		t.Fatal(err)
	}
	h.server.reviewPass(ctx) // one fetch: the base is still not there
	threads, _ := h.store.ReviewThreadsOf(ctx, h.pull(t).ID)
	var next store.ThreadRow
	for _, th := range threads {
		if th.ReviewHead == headB {
			next = th
		}
	}
	if next.ID == 0 || next.Session != 0 || fetched != 1 {
		t.Fatalf("with no base: review %+v, fetches %d", next, fetched)
	}
	h.server.reviewPass(ctx) // a second fetch brings it
	if got := h.reviewAt(t, headB); got.Session == 0 || fetched != 2 {
		t.Fatalf("with the base: review %+v, fetches %d", got, fetched)
	}
}

// A review's start checks its profile's tools as any start does: a tool
// pocketd does not have starts none (Codex review of #26, round 2).
func TestAReviewChecksItsProfilesTools(t *testing.T) {
	h := newReviewHarness(t)
	ctx := context.Background()
	h.writeProfiles(map[string]string{
		"odrade.md": "---\nrole: supervisor\nmodel: faux/faux-1\nhome: ~\n---\n# Odrade\n",
		"vera.md":   "---\nmodel: anthropic/claude-opus-5-5\ntools: [read, frobnicate]\nrepositories:\n  frostyard/example: " + h.checkout + "\n---\n# Vera\n",
		"duncan.md": "---\nmodel: anthropic/claude-opus-5-5\nhome: ~\n---\n# Duncan\n",
	})
	h.gh.set(func(g *fakeGitHub) { g.head = headB })
	if _, err := h.server.RefreshPulls(ctx); err != nil {
		t.Fatal(err)
	}
	h.server.reviewPass(ctx)
	threads, _ := h.store.ReviewThreadsOf(ctx, h.pull(t).ID)
	for _, th := range threads {
		if th.ReviewHead == headB {
			t.Fatalf("a review started for a profile with an unknown tool: %+v", th)
		}
	}
}

// A thread's start takes team_verdict from a session that is no review's,
// a recovered one included (Codex review of #26, round 2).
func TestAThreadsStartSetsItsVerdictTool(t *testing.T) {
	h := newReviewHarness(t)
	h.engine.mu.Lock()
	defer h.engine.mu.Unlock()
	if on, set := h.engine.verdictTool[h.duncan.Session]; !set || on {
		t.Fatalf("the delegated thread's start left team_verdict as %v (set %v)", on, set)
	}
	if !h.engine.verdictTool[h.teg.Session] {
		t.Fatal("the review's start did not give it team_verdict")
	}
}

// An ended review's run is stopped outside the team's locks: PiG's abort
// waits for the run, whose verdict may wait for workMu. A stop that fails
// is tried again at the next pass (Codex review of #26, round 3).
func TestAReviewsAbortHoldsNoTeamLock(t *testing.T) {
	h := newReviewHarness(t)
	ctx := context.Background()
	held := false
	h.engine.mu.Lock()
	h.engine.runFirst = 9
	h.engine.abortErr = errors.New("PiG is busy")
	h.engine.onAbort = func(int64) {
		if !h.server.workMu.TryLock() {
			held = true
			return
		}
		h.server.workMu.Unlock()
		if !h.server.outboxMu.TryLock() {
			held = true
			return
		}
		h.server.outboxMu.Unlock()
	}
	h.engine.mu.Unlock()
	h.gh.set(func(g *fakeGitHub) { g.head = headB })
	if _, err := h.server.RefreshPulls(ctx); err != nil {
		t.Fatal(err)
	}
	h.server.reviewPass(ctx) // the abort fails
	h.server.reviewPass(ctx) // and is tried again
	h.engine.mu.Lock()
	aborted := slices.Clone(h.engine.aborted)
	h.engine.mu.Unlock()
	if held {
		t.Fatal("the abort ran under a team lock")
	}
	if !slices.Contains(aborted, h.teg.Session) {
		t.Fatalf("aborted %v: a failed stop was not tried again", aborted)
	}
}

// A pull request retargeted at the same head is another change set: its
// review ends, and one starts at the new base (Codex review of #26,
// round 3).
func TestARetargetSupersedesTheReview(t *testing.T) {
	h := newReviewHarness(t)
	ctx := context.Background()
	h.gh.set(func(g *fakeGitHub) { g.baseRef = "release" })
	if _, err := h.server.RefreshPulls(ctx); err != nil {
		t.Fatal(err)
	}
	h.server.reviewPass(ctx)
	old, _ := h.store.Thread(ctx, h.teg.ID)
	if old.State != store.ThreadClosed || !strings.Contains(old.Report, "on release now") {
		t.Fatalf("the review of the old base: %+v", old)
	}
	threads, _ := h.store.ReviewThreadsOf(ctx, h.pull(t).ID)
	if len(threads) != 2 || threads[1].Key != team.ReviewKey("frostyard/example", 3, "release", headA) {
		t.Fatalf("review threads %+v", threads)
	}
}

// A verdict counts only on the change set the reviewer read: a merge base
// GitHub names differently now refuses it (Codex review of #26, round 3).
func TestAMovedMergeBaseRefusesTheVerdict(t *testing.T) {
	h := newReviewHarness(t)
	h.gh.set(func(g *fakeGitHub) { g.base = strings.Repeat("f", 40) })
	if _, err := h.verdict(h.teg.Session, "v1", headA, team.VerdictReady); err == nil || !strings.Contains(err.Error(), "no verdict on it counts") {
		t.Fatalf("a verdict on a moved base: %v", err)
	}
}

// lacksHead is fakeReviewGit whose checkout does not hold one commit.
type lacksHead struct {
	fakeReviewGit
	sha string
}

func (l lacksHead) has(_ context.Context, _, sha string) (bool, error) { return sha != l.sha, nil }

// A re-review whose last reviewed head the checkout lacks is a full
// review, and team_diff says so for since_last_review (Codex review of
// #26, round 3).
func TestAReReviewWithoutItsLastHeadIsAFullReview(t *testing.T) {
	h := newReviewHarness(t)
	ctx := context.Background()
	if _, err := h.verdict(h.teg.Session, "v1", headA, team.VerdictNotReady, "high blocking: no test"); err != nil {
		t.Fatal(err)
	}
	h.server.reviewGit = lacksHead{sha: headA}
	h.gh.set(func(g *fakeGitHub) { g.head = headB })
	if _, err := h.server.RefreshPulls(ctx); err != nil {
		t.Fatal(err)
	}
	next := h.reviewAt(t, headB)
	if strings.Contains(next.Brief, "re-review") {
		t.Fatalf("a re-review from a head the checkout lacks:\n%s", next.Brief)
	}
	if _, err := h.server.TeamDiff(ctx, TeamCall{Session: next.Session, Task: 1, CallID: "d"}, team.DiffRequest{SinceLastReview: true}); err == nil ||
		!strings.Contains(err.Error(), "not in your folder") {
		t.Fatalf("since_last_review: %v", err)
	}
}

// A retarget at the same head takes the label off: the verdict on the old
// base backs no merge of the new change set (Codex review of #26, round 4).
func TestARetargetTakesTheLabelOff(t *testing.T) {
	h := labelled(t)
	ctx := context.Background()
	h.gh.set(func(g *fakeGitHub) { g.baseRef = "release" })
	if _, err := h.server.RefreshPulls(ctx); err != nil {
		t.Fatal(err)
	}
	if p := h.pull(t); p.Ready || h.gh.labels[team.ReadyLabel] {
		t.Fatalf("after a retarget: %+v, labels %v", p, h.gh.labels)
	}
	if pulls, _ := h.client.Pulls(ctx); len(pulls) != 1 || pulls[0].Review != nil {
		t.Fatalf("the owner still sees the old base's verdict: %+v", pulls)
	}
	h.server.reviewPass(ctx)
	threads, _ := h.store.ReviewThreadsOf(ctx, h.pull(t).ID)
	if len(threads) != 2 {
		t.Fatalf("no review of the new base: %+v", threads)
	}
}

// A re-review reads from the last reviewed head only on the same baseline:
// after the merge base moved it is a full review (Codex review of #26,
// round 4).
func TestAReReviewOnAnotherBaselineIsAFullReview(t *testing.T) {
	h := newReviewHarness(t)
	ctx := context.Background()
	if _, err := h.verdict(h.teg.Session, "v1", headA, team.VerdictNotReady, "high blocking: no test"); err != nil {
		t.Fatal(err)
	}
	h.gh.set(func(g *fakeGitHub) { g.head, g.base = headB, strings.Repeat("f", 40) })
	if _, err := h.server.RefreshPulls(ctx); err != nil {
		t.Fatal(err)
	}
	next := h.reviewAt(t, headB)
	if strings.Contains(next.Brief, "re-review") {
		t.Fatalf("a re-review across baselines:\n%s", next.Brief)
	}
	if _, err := h.server.TeamDiff(ctx, TeamCall{Session: next.Session, Task: 1, CallID: "d"}, team.DiffRequest{SinceLastReview: true}); err == nil ||
		!strings.Contains(err.Error(), "from this base") {
		t.Fatalf("since_last_review across baselines: %v", err)
	}
}

// A review's session takes no message: pocketd runs it, so every run in it
// is the review's, and no stop of a review stops other work (Codex review
// of #26, round 5).
func TestAReviewSessionTakesNoMessage(t *testing.T) {
	h := newReviewHarness(t)
	_, err := h.client.Send(context.Background(), h.teg.Session, proto.SendMessage{RequestID: "m1", Text: "Also look at the docs."})
	var apiErr *proto.Error
	if !errors.As(err, &apiErr) || apiErr.Status != 409 || !strings.Contains(apiErr.Message, "takes no message") {
		t.Fatalf("a message to a review: %v", err)
	}
}

// A stop whose look at the session failed stays due, and the next pass
// stops the run (Codex review of #26, round 5).
func TestAFailedStopLookStaysDue(t *testing.T) {
	h := newReviewHarness(t)
	ctx := context.Background()
	h.engine.mu.Lock()
	h.engine.runFirst = 9
	h.engine.busyErr = errors.New("PiG cannot tell")
	h.engine.mu.Unlock()
	h.gh.set(func(g *fakeGitHub) { g.head = headB })
	if _, err := h.server.RefreshPulls(ctx); err != nil {
		t.Fatal(err)
	}
	h.server.reviewPass(ctx) // the look fails
	h.server.reviewPass(ctx)
	h.engine.mu.Lock()
	aborted := slices.Clone(h.engine.aborted)
	h.engine.mu.Unlock()
	if !slices.Contains(aborted, h.teg.Session) {
		t.Fatalf("aborted %v: a stop whose look failed was dropped", aborted)
	}
}

// No supervisor closes a review: pocketd started it, and ends it (Codex
// review of #26, round 5).
func TestNoSupervisorClosesAReview(t *testing.T) {
	h := newReviewHarness(t)
	if _, err := h.server.TeamClose(context.Background(), TeamCall{Session: h.super.ID, Task: 1, CallID: "c"}, h.teg.ID); err == nil ||
		!strings.Contains(err.Error(), "no supervisor closes it") {
		t.Fatalf("a supervisor's close of a review: %v", err)
	}
}

// A retarget under the label's write takes the label off again: the
// verdict is of the old base (Codex review of #26, round 5).
func TestARetargetUnderTheLabelTakesItOff(t *testing.T) {
	h := newReviewHarness(t)
	h.gh.set(func(g *fakeGitHub) { g.onAdd = func(g *fakeGitHub) { g.baseRef, g.onAdd = "release", nil } })
	if _, err := h.verdict(h.teg.Session, "v1", headA, team.VerdictReady); err != nil {
		t.Fatal(err)
	}
	writes := h.gh.labelWrites()
	if len(writes) != 2 || !strings.Contains(writes[1], "DELETE") || h.gh.labels[team.ReadyLabel] {
		t.Fatalf("label writes %q, labels %v", writes, h.gh.labels)
	}
}

// The verdict's compare has a deadline of its own while it holds the pull
// request's lock (Codex review of #26, round 4).
func TestTheVerdictsCompareHasADeadline(t *testing.T) {
	h := newReviewHarness(t)
	if _, err := h.verdict(h.teg.Session, "v1", headA, team.VerdictReady); err != nil {
		t.Fatal(err)
	}
	h.gh.mu.Lock()
	defer h.gh.mu.Unlock()
	if !h.gh.compareDeadline {
		t.Fatal("the verdict's compare ran with no deadline")
	}
}

// A verdict backs its pull request only while the merge base stays: a base
// branch reset after the verdict takes the label off (Codex review of #26,
// round 6).
func TestAMergeBaseMovedAfterTheVerdictTakesTheLabelOff(t *testing.T) {
	h := labelled(t)
	ctx := context.Background()
	h.gh.set(func(g *fakeGitHub) { g.base = strings.Repeat("f", 40) })
	if _, err := h.server.RefreshPulls(ctx); err != nil {
		t.Fatal(err)
	}
	if p := h.pull(t); p.Ready || h.gh.labels[team.ReadyLabel] || p.MergeBase != strings.Repeat("f", 40) {
		t.Fatalf("after the merge base moved: %+v, labels %v", p, h.gh.labels)
	}
	if pulls, _ := h.client.Pulls(ctx); len(pulls) != 1 || pulls[0].Review != nil {
		t.Fatalf("the owner still sees the old verdict: %+v", pulls)
	}
}

// A round starts no review of a pull request whose read just failed, and
// a pass reads one it did not before it starts it: its cached state may be
// gone (Codex review of #26, rounds 6 and 7).
func TestARoundStartsNoReviewOfAPullItCouldNotRead(t *testing.T) {
	h := newReviewHarness(t)
	ctx := context.Background()
	h.gh.set(func(g *fakeGitHub) { g.head = headB })
	if _, err := h.server.RefreshPulls(ctx); err != nil {
		t.Fatal(err)
	}
	// The head moved, and this round's read fails, and so does the pass's
	// own read of a pull request that needs a review (round 7).
	h.gh.set(func(g *fakeGitHub) { g.failReads = 2 })
	fresh, _, _ := h.server.refreshPulls(ctx)
	h.server.reviewPassOf(ctx, reviewWork{fresh: fresh})
	threads, _ := h.store.ReviewThreadsOf(ctx, h.pull(t).ID)
	for _, th := range threads {
		if th.ReviewHead == headB {
			t.Fatalf("a review started on a pull request the round could not read: %+v", th)
		}
	}
}

// A review's run that ends settles its review at once, whether or not pull
// requests are read in rounds; the service's start settles them too (Codex
// review of #26, round 6).
func TestAnEndedReviewRunSettlesWithoutPolling(t *testing.T) {
	h := newReviewHarness(t)
	ctx := context.Background()
	ended := make(chan int64, 1)
	h.server.afterRunEnded = func(id int64) { ended <- id }
	h.server.PublishLive(proto.Live{SessionID: h.teg.Session, Busy: true})
	h.server.PublishLive(proto.Live{SessionID: h.teg.Session, Busy: false})
	if got := <-ended; got != h.teg.Session {
		t.Fatalf("followed session %d", got)
	}
	review, err := h.store.Thread(ctx, h.teg.ID)
	if err != nil || review.State != store.ThreadClosed || !strings.Contains(review.Report, "no verdict") {
		t.Fatalf("the review after its run ended: %+v, %v", review, err)
	}
}

// The service's start settles the reviews a stop left open, with no round
// of pull requests (Codex review of #26, round 6).
func TestTheStartSettlesTheReviews(t *testing.T) {
	h := newReviewHarness(t)
	ctx := context.Background()
	h.server.reviewsOn = false
	h.server.StartPulls(0)
	review, err := h.store.Thread(ctx, h.teg.ID)
	if err != nil || review.State != store.ThreadClosed {
		t.Fatalf("the review after the start: %+v, %v", review, err)
	}
}

// A merge base GitHub does not name is unknown, and backs nothing: the
// label comes off, none goes on, and the read counts as failed, until
// GitHub names it again (Codex review of #26, round 7).
func TestAnUnreadMergeBaseBacksNothing(t *testing.T) {
	h := labelled(t)
	ctx := context.Background()
	for i := range 2 {
		h.gh.set(func(g *fakeGitHub) { g.failCompares = 100 })
		if done, err := h.server.RefreshPulls(ctx); done || err == nil {
			t.Fatalf("round %d: a read with no merge base was done: %v", i, err)
		}
		if p := h.pull(t); p.Ready || h.gh.labels[team.ReadyLabel] || p.MergeBase != "" {
			t.Fatalf("round %d: with no merge base: %+v, labels %v", i, p, h.gh.labels)
		}
		if pulls, _ := h.client.Pulls(ctx); len(pulls) != 1 || pulls[0].Review != nil {
			t.Fatalf("round %d: the owner sees a verdict: %+v", i, pulls)
		}
	}
	h.gh.set(func(g *fakeGitHub) { g.failCompares = 0 })
	if done, err := h.server.RefreshPulls(ctx); !done || err != nil {
		t.Fatal(err)
	}
	if p := h.pull(t); !p.Ready || !h.gh.labels[team.ReadyLabel] {
		t.Fatalf("once GitHub named it again: %+v, labels %v", p, h.gh.labels)
	}
}

// The reads around a label's write check the merge base too: a base branch
// reset while CI is read puts no label on, and one under the write takes
// it off again (Codex review of #26, round 7).
func TestABaseResetAroundTheLabelWriteLeavesNoLabel(t *testing.T) {
	moved := strings.Repeat("f", 40)
	for _, during := range []string{"ci", "write", "write-unread"} {
		h := newReviewHarness(t)
		h.gh.set(func(g *fakeGitHub) {
			reset := func(g *fakeGitHub) { g.base, g.onCI, g.onAdd = moved, nil, nil }
			switch during {
			case "ci":
				g.onCI = reset
			case "write":
				g.onAdd = reset
			default:
				// GitHub names no merge base after the write.
				g.onAdd = func(g *fakeGitHub) { g.failCompares, g.onAdd = 100, nil }
			}
		})
		if _, err := h.verdict(h.teg.Session, "v1", headA, team.VerdictReady); err != nil {
			t.Fatal(err)
		}
		want := moved
		if during == "write-unread" {
			want = ""
		}
		if p := h.pull(t); p.Ready || h.gh.labels[team.ReadyLabel] || p.MergeBase != want {
			t.Fatalf("a base reset during the %s: %+v, labels %v, writes %q", during, p, h.gh.labels, h.gh.labelWrites())
		}
	}
}

// The read before a label's removal decides with what GitHub says then: a
// merge base it names again, the verdict's, keeps the label (Codex review
// of #26, round 7).
func TestTheReadBeforeARemovalDecides(t *testing.T) {
	h := labelled(t)
	ctx := context.Background()
	writes := len(h.gh.labelWrites())
	h.gh.set(func(g *fakeGitHub) { g.failCompares = 1 })
	if _, err := h.server.RefreshPulls(ctx); err == nil {
		t.Fatal("a read whose merge base GitHub did not name was done")
	}
	if p := h.pull(t); !p.Ready || !h.gh.labels[team.ReadyLabel] || len(h.gh.labelWrites()) != writes {
		t.Fatalf("the label, though the read before its removal named the merge base: %+v, writes %q", p, h.gh.labelWrites()[writes:])
	}
}

// GET /v1/pulls/{id} reads the pull request on GitHub before it answers:
// a base branch reset since the last round leaves no verdict, a read it
// cannot make is an error, and an id it does not list is not found
// (rule 45; Codex review of #26, round 7).
func TestReadingOnePullReadsGitHubFirst(t *testing.T) {
	h := labelled(t)
	ctx := context.Background()
	id := h.pull(t).ID
	got, err := h.client.Pull(ctx, id)
	if err != nil || got.Review == nil || got.Review.Verdict != proto.ReviewReady || !got.Ready {
		t.Fatalf("before the reset: %+v, %v", got, err)
	}
	h.gh.set(func(g *fakeGitHub) { g.base = strings.Repeat("f", 40) })
	if got, err = h.client.Pull(ctx, id); err != nil || got.Review != nil || got.Ready {
		t.Fatalf("after a reset no round read: %+v, %v", got, err)
	}
	var pe *proto.Error
	h.gh.set(func(g *fakeGitHub) { g.failReads = 1 })
	if _, err := h.client.Pull(ctx, id); !errors.As(err, &pe) || pe.Code != proto.ErrorCodeInternal {
		t.Fatalf("a read GitHub did not answer: %v", err)
	}
	if _, err := h.client.Pull(ctx, id+100); !errors.As(err, &pe) || pe.Code != proto.ErrorCodeNotFound {
		t.Fatalf("an id not listed: %v", err)
	}
}

// A round that cannot list the pull requests to read read none: it gives
// a review pass no pull request to start, never every open one (Codex
// review of #26, round 7).
func TestARoundThatListsNothingFreshensNothing(t *testing.T) {
	h := newReviewHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	fresh, done, err := h.server.refreshPulls(ctx)
	if err == nil || done || fresh == nil || len(fresh) != 0 {
		t.Fatalf("fresh %v, done %v, %v", fresh, done, err)
	}
}

// A pass that finds a review's brief in the transcript, though its record
// says it is not delivered yet, follows it at once: its run may have
// ended, with no busy-to-idle change still to come (Codex review of #26,
// round 7).
func TestARecoveredBriefIsFollowed(t *testing.T) {
	h := newReviewHarness(t)
	ctx := context.Background()
	h.server.retry = func(func()) func() { return func() {} }
	h.engine.mu.Lock()
	h.engine.queued = map[int64]bool{}
	h.engine.mu.Unlock()
	h.engine.onSend = func(_ int64, key string) {
		if strings.HasSuffix(key, ":brief") {
			h.engine.mu.Lock()
			h.engine.queued[int64(len(h.engine.sent)+100)] = true
			h.engine.mu.Unlock()
		}
	}
	h.gh.set(func(g *fakeGitHub) { g.head = headB })
	fresh, _, err := h.server.refreshPulls(ctx)
	if err != nil {
		t.Fatal(err)
	}
	h.server.reviewPassOf(ctx, reviewWork{fresh: fresh})
	review, err := h.store.ThreadByKey(ctx, team.ReviewKey("frostyard/example", 3, "main", headB))
	if err != nil || review.State != store.ThreadOpen || review.Briefed {
		t.Fatalf("the review of the new head, its brief queued: %+v, %v", review, err)
	}
	// The brief reaches the transcript, and its run ends, before a pass
	// records it.
	h.engine.onSend = nil
	h.engine.mu.Lock()
	h.engine.queued = map[int64]bool{}
	h.engine.mu.Unlock()
	h.server.reviewPass(ctx)
	review, err = h.store.Thread(ctx, review.ID)
	if err != nil || review.State != store.ThreadClosed || !strings.Contains(review.Report, "no verdict") {
		t.Fatalf("the review after the pass: %+v, %v", review, err)
	}
}

// A pull request that needs its review by what pocketd holds gets it at
// the next pass, though the pass that its report asked for never ran, as
// when pocketd stopped between them: the pass reads it on GitHub first
// (Codex review of #26, round 7).
func TestAReviewLostToAStopStartsAtTheNextPass(t *testing.T) {
	h := newReviewHarness(t)
	ctx := context.Background()
	// The new head is recorded, as a report's link records it, and no pass
	// runs before the stop.
	h.gh.set(func(g *fakeGitHub) { g.head = headB })
	if _, err := h.server.RefreshPulls(ctx); err != nil {
		t.Fatal(err)
	}
	key := team.ReviewKey("frostyard/example", 3, "main", headB)
	if _, err := h.store.ThreadByKey(ctx, key); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a review started before the pass: %v", err)
	}
	reads := len(h.gh.calls)
	h.server.reviewPassOf(ctx, reviewWork{fresh: map[int64]int64{}})
	review, err := h.store.ThreadByKey(ctx, key)
	if err != nil || review.State == store.ThreadClosed {
		t.Fatalf("the review of the new head: %+v, %v", review, err)
	}
	read := false
	for _, call := range h.gh.calls[reads:] {
		read = read || strings.HasSuffix(call, "repos/frostyard/example/pulls/3")
	}
	if !read {
		t.Fatalf("the pass started it on what pocketd held: %q", h.gh.calls[reads:])
	}
}

// GET /v1/pulls/{id} reads a pull request pocketd holds as closed too: it
// may have reopened, at a merge base its old verdict does not back (Codex
// review of #26, round 8).
func TestReadingOnePullReadsOneHeldClosed(t *testing.T) {
	h := labelled(t)
	ctx := context.Background()
	id := h.pull(t).ID
	h.gh.set(func(g *fakeGitHub) { g.state = "closed" })
	if _, err := h.server.RefreshPulls(ctx); err != nil {
		t.Fatal(err)
	}
	if p := h.pull(t); p.State != store.PullClosed {
		t.Fatalf("held as %s", p.State)
	}
	h.gh.set(func(g *fakeGitHub) { g.state, g.base = "open", strings.Repeat("f", 40) })
	got, err := h.client.Pull(ctx, id)
	if err != nil || got.State != proto.PullOpen || got.Review != nil {
		t.Fatalf("a closed one reopened at another merge base: %+v, %v", got, err)
	}
}

// A head reported again after a base branch reset is read with its merge
// base before anything counts it fresh: its old verdict backs nothing
// (Codex review of #26, round 8).
func TestAReportReadsTheMergeBaseAgain(t *testing.T) {
	h := labelled(t)
	ctx := context.Background()
	h.gh.set(func(g *fakeGitHub) { g.base = strings.Repeat("f", 40) })
	if _, err := h.server.TeamMessage(ctx, TeamCall{Session: h.vera.Session, Task: 2, CallID: "m2"},
		team.Report{Message: "PR #3 again.", Status: team.StatusWorking, PullRequests: []team.PullRef{{Number: 3}}}); err != nil {
		t.Fatal(err)
	}
	if p := h.pull(t); p.Ready || h.gh.labels[team.ReadyLabel] || p.MergeBase != strings.Repeat("f", 40) {
		t.Fatalf("after the report: %+v, labels %v", p, h.gh.labels)
	}
}

// A review its verdict closed freed a place: the end of its run runs a
// pass, which starts what waits (Codex review of #26, round 8).
func TestAVerdictsRunEndRunsAPass(t *testing.T) {
	h := newReviewHarness(t)
	ctx := context.Background()
	if _, err := h.verdict(h.teg.Session, "v1", headA, team.VerdictReady); err != nil {
		t.Fatal(err)
	}
	if got, _ := h.store.Thread(ctx, h.teg.ID); got.State != store.ThreadClosed {
		t.Fatalf("the review after its verdict: %+v", got)
	}
	// A new head waits, recorded by a read that ran no pass.
	h.gh.set(func(g *fakeGitHub) { g.head = headB })
	if _, err := h.server.RefreshPulls(ctx); err != nil {
		t.Fatal(err)
	}
	ended := make(chan int64, 1)
	h.server.afterRunEnded = func(id int64) { ended <- id }
	h.server.PublishLive(proto.Live{SessionID: h.teg.Session, Busy: true})
	h.server.PublishLive(proto.Live{SessionID: h.teg.Session, Busy: false})
	<-ended
	if _, err := h.store.ThreadByKey(ctx, team.ReviewKey("frostyard/example", 3, "main", headB)); err != nil {
		t.Fatalf("no review of the waiting head: %v", err)
	}
}

// A confirmation's read that finds a new head goes to the review pass, as
// a round's read does (Codex review of #26, round 8).
func TestAConfirmationsReadStartsTheReview(t *testing.T) {
	h := newReviewHarness(t)
	ctx := context.Background()
	h.gh.set(func(g *fakeGitHub) { g.head = headB })
	if _, err := h.client.Pull(ctx, h.pull(t).ID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.ThreadByKey(ctx, team.ReviewKey("frostyard/example", 3, "main", headB)); err != nil {
		t.Fatalf("no review of the head the read found: %v", err)
	}
}

// needsReviewGit skips a test on a host whose git cannot make a review's
// worktree (worktree.AttributesIsolated): one older than 2.43, or with a
// system attributes file. The refusals are tested in package worktree
// (Codex review of #26, round 9).
func needsReviewGit(t *testing.T) {
	t.Helper()
	cmd := exec.Command("git", "version")
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
	out, err := cmd.Output()
	m := regexp.MustCompile(`^git version ([0-9]+)\.([0-9]+)`).FindStringSubmatch(string(out))
	if err != nil || m == nil {
		t.Skipf("no git: %v", err)
	}
	major, _ := strconv.Atoi(m[1])
	minor, _ := strconv.Atoi(m[2])
	if major < 2 || major == 2 && minor < 43 {
		t.Skipf("%s: a review's worktree needs git 2.43 or later", strings.TrimSpace(string(out)))
	}
	system, err := exec.Command("git", "var", "GIT_ATTR_SYSTEM").Output()
	if err != nil {
		t.Skipf("git var GIT_ATTR_SYSTEM: %v", err)
	}
	if path := strings.TrimSpace(string(system)); path != "" {
		if _, err := os.Stat(path); err == nil {
			t.Skipf("the system attributes file %s refuses a review's worktree", path)
		}
	}
}

// team_diff checks, right before it runs git, the attributes no option
// turns off: one that came since the brief stops the diff (Codex review
// of #26, round 9).
func TestTeamDiffChecksIsolationWhenItRuns(t *testing.T) {
	h := newReviewHarness(t)
	ctx := context.Background()
	h.server.reviewGit = fakeReviewGit{notIsolated: errors.New("git reads the attributes in .git/info/attributes")}
	ran := false
	h.engine.mu.Lock()
	h.engine.onGit = func([]string) { ran = true }
	h.engine.mu.Unlock()
	if _, err := h.server.TeamDiff(ctx, TeamCall{Session: h.teg.Session, Task: 1, CallID: "d1"}, team.DiffRequest{}); err == nil ||
		!strings.Contains(err.Error(), "info/attributes") {
		t.Fatalf("a diff with the attributes no longer isolated: %v", err)
	}
	if ran {
		t.Fatal("git ran")
	}
}

// A pass trusts a pull request as read only at the read that found it: a
// later read, whose merge base GitHub did not name, is not fresh, and
// starts nothing (Codex review of #26, round 9).
func TestAFreshReadCountsOnlyAtItsSequence(t *testing.T) {
	h := newReviewHarness(t)
	ctx := context.Background()
	p := h.pull(t)
	stale := reviewWork{fresh: map[int64]int64{p.ID: p.ReadSeq}}
	h.gh.set(func(g *fakeGitHub) { g.head, g.failCompares = headB, 100 })
	if _, err := h.server.RefreshPulls(ctx); err == nil {
		t.Fatal("a read with no merge base was done")
	}
	if now := h.pull(t); now.HeadSHA != headB || now.ReadSeq == p.ReadSeq {
		t.Fatalf("the later read: %+v", now)
	}
	h.server.reviewPassOf(ctx, stale)
	if _, err := h.store.ThreadByKey(ctx, team.ReviewKey("frostyard/example", 3, "main", headB)); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a review started on a read that failed: %v", err)
	}
}

// A review not briefed yet reads its pull request on GitHub before it is
// started again: one merged meanwhile ends it, with no reviewer started
// (Codex review of #26, round 9).
func TestAnUnbriefedReviewReadsItsPullFirst(t *testing.T) {
	h := newReviewHarness(t)
	ctx := context.Background()
	h.server.retry = func(func()) func() { return func() {} }
	h.engine.mu.Lock()
	h.engine.queued = map[int64]bool{}
	h.engine.mu.Unlock()
	h.engine.onSend = func(_ int64, key string) {
		if strings.HasSuffix(key, ":brief") {
			h.engine.mu.Lock()
			h.engine.queued[int64(len(h.engine.sent)+100)] = true
			h.engine.mu.Unlock()
		}
	}
	h.gh.set(func(g *fakeGitHub) { g.head = headB })
	fresh, _, err := h.server.refreshPulls(ctx)
	if err != nil {
		t.Fatal(err)
	}
	h.server.reviewPassOf(ctx, reviewWork{fresh: fresh})
	review, err := h.store.ThreadByKey(ctx, team.ReviewKey("frostyard/example", 3, "main", headB))
	if err != nil || review.Briefed {
		t.Fatalf("the review, its brief queued: %+v, %v", review, err)
	}
	// It closes on GitHub while pocketd is away.
	h.gh.set(func(g *fakeGitHub) { g.state = "closed" })
	h.server.reviewPassOf(ctx, reviewWork{fresh: map[int64]int64{}})
	review, err = h.store.Thread(ctx, review.ID)
	if err != nil || review.State != store.ThreadClosed || !strings.Contains(review.Report, "closed") {
		t.Fatalf("the review after the pass: %+v, %v", review, err)
	}
}

// A thread's close frees a place on the desk: a pass runs, and starts the
// review that waited (Codex review of #26, round 9).
func TestAClosedThreadRunsAPass(t *testing.T) {
	h := newReviewHarness(t)
	ctx := context.Background()
	h.gh.set(func(g *fakeGitHub) { g.head = headB })
	if _, err := h.server.RefreshPulls(ctx); err != nil {
		t.Fatal(err)
	}
	key := team.ReviewKey("frostyard/example", 3, "main", headB)
	if _, err := h.store.ThreadByKey(ctx, key); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a review started before the close: %v", err)
	}
	if _, err := h.server.TeamClose(ctx, TeamCall{Session: h.super.ID, CallID: "close-duncan"}, h.duncan.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.ThreadByKey(ctx, key); err != nil {
		t.Fatalf("no review after the close: %v", err)
	}
}

// A report's pull requests are read with their merge base by the review
// pass, not by the author's team_message, which waits for no GitHub read
// past the report's own (Codex review of #26, round 9).
func TestAReportsReadsAreThePasss(t *testing.T) {
	h := labelled(t)
	ctx := context.Background()
	h.server.reviewsOn = false
	calls := len(h.gh.calls)
	if _, err := h.server.TeamMessage(ctx, TeamCall{Session: h.vera.Session, Task: 2, CallID: "m2"},
		team.Report{Message: "PR #3 again.", Status: team.StatusWorking, PullRequests: []team.PullRef{{Number: 3}}}); err != nil {
		t.Fatal(err)
	}
	for _, call := range h.gh.calls[calls:] {
		if strings.Contains(call, "/compare/") {
			t.Fatalf("team_message read a merge base: %q", h.gh.calls[calls:])
		}
	}
	calls = len(h.gh.calls)
	h.server.startReviews()
	compared := false
	for _, call := range h.gh.calls[calls:] {
		compared = compared || strings.Contains(call, "/compare/")
	}
	if !compared {
		t.Fatalf("the pass did not read the reported pull request: %q", h.gh.calls[calls:])
	}
}

// A review's brief is the review pass's alone: the delivery pass sends no
// review's brief again, and the review pass reads the pull request first,
// so a read that fails starts no reviewer (Codex review of #26, round 10).
func TestTheDeliveryPassSendsNoReviewBrief(t *testing.T) {
	h := newReviewHarness(t)
	ctx := context.Background()
	h.server.retry = func(func()) func() { return func() {} }
	h.engine.mu.Lock()
	h.engine.queued, h.engine.withdrawn = map[int64]bool{}, map[int64]bool{}
	h.engine.mu.Unlock()
	h.engine.onSend = func(_ int64, key string) {
		if strings.HasSuffix(key, ":brief") {
			h.engine.mu.Lock()
			h.engine.queued[int64(len(h.engine.sent)+100)] = true
			h.engine.mu.Unlock()
		}
	}
	h.gh.set(func(g *fakeGitHub) { g.head = headB })
	fresh, _, err := h.server.refreshPulls(ctx)
	if err != nil {
		t.Fatal(err)
	}
	h.server.reviewPassOf(ctx, reviewWork{fresh: fresh})
	review, err := h.store.ThreadByKey(ctx, team.ReviewKey("frostyard/example", 3, "main", headB))
	if err != nil || review.Briefed || review.BriefSubmission == 0 {
		t.Fatalf("the review, its brief queued: %+v, %v", review, err)
	}
	// Its brief is withdrawn, as a restart may leave it, and GitHub cannot
	// be read.
	h.engine.onSend = nil
	h.engine.mu.Lock()
	h.engine.withdrawn[review.BriefSubmission] = true
	sent := len(h.engine.sent)
	h.engine.mu.Unlock()
	h.gh.set(func(g *fakeGitHub) { g.failReads = 100 })
	calls := len(h.gh.calls)
	done, err := h.server.deliverAll(ctx)
	if err != nil {
		t.Log(err)
	}
	h.engine.mu.Lock()
	again := len(h.engine.sent) != sent
	h.engine.mu.Unlock()
	if again {
		t.Fatalf("a review's brief went again without its pull request read: %v", h.engine.order)
	}
	// The delivery pass, which runs every second while anything is left,
	// leaves a review alone: nothing is left for it, and it reads nothing
	// on GitHub (Codex review of #26, round 11).
	if !done || len(h.gh.calls) != calls {
		t.Fatalf("the delivery pass kept a review: done %v, GitHub calls %q", done, h.gh.calls[calls:])
	}
}

// A pass's own read of a pull request settles its reviews again before
// another starts: a busy review of the old head ends (Codex review of #26,
// round 10).
func TestAPassSettlesAfterItsRead(t *testing.T) {
	h := newReviewHarness(t)
	ctx := context.Background()
	h.engine.mu.Lock()
	h.engine.runFirst = 1
	h.engine.mu.Unlock()
	h.gh.set(func(g *fakeGitHub) { g.head = headB })
	h.server.reviewAfterRead(h.pull(t).ID)
	if got, _ := h.store.Thread(ctx, h.teg.ID); got.State != store.ThreadClosed || !strings.Contains(got.Report, "Superseded") {
		t.Fatalf("the busy review of the old head: %+v", got)
	}
}

// A verdict refused for a moved pull request sends it to the review pass,
// which reads it: the review ends, and the next head's starts (Codex
// review of #26, round 10).
func TestARefusedVerdictRereadsThePull(t *testing.T) {
	h := newReviewHarness(t)
	ctx := context.Background()
	h.gh.set(func(g *fakeGitHub) { g.head = headB })
	if _, err := h.verdict(h.teg.Session, "v1", headA, team.VerdictReady); err == nil {
		t.Fatal("a verdict on a moved head was filed")
	}
	if _, err := h.store.ThreadByKey(ctx, team.ReviewKey("frostyard/example", 3, "main", headB)); err != nil {
		t.Fatalf("no review of the head the verdict's read found: %v", err)
	}
}

// A thread that could not start frees its place: a pass runs, and starts
// the review that waited (Codex review of #26, round 10).
func TestAFailedStartRunsAPass(t *testing.T) {
	h := newReviewHarness(t)
	ctx := context.Background()
	h.gh.set(func(g *fakeGitHub) { g.head = headB })
	if _, err := h.server.RefreshPulls(ctx); err != nil {
		t.Fatal(err)
	}
	key := team.ReviewKey("frostyard/example", 3, "main", headB)
	starting, _, err := h.store.CreateThread(ctx, store.ThreadRow{Key: "fails", Title: "x", Agent: "duncan", SupervisorSession: h.super.ID, Task: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.ThreadByKey(ctx, key); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a review started before the failure: %v", err)
	}
	if err := h.server.failThread(ctx, starting.ID, "its folder moved"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.ThreadByKey(ctx, key); err != nil {
		t.Fatalf("no review after the failed start: %v", err)
	}
}

// A thread the delivery pass closes, its delegation ended before it
// started, frees its place: a review pass runs (Codex review of #26,
// round 10).
func TestADeliveryPassCloseRunsAPass(t *testing.T) {
	h := newReviewHarness(t)
	ctx := context.Background()
	h.gh.set(func(g *fakeGitHub) { g.head = headB })
	if _, err := h.server.RefreshPulls(ctx); err != nil {
		t.Fatal(err)
	}
	key := team.ReviewKey("frostyard/example", 3, "main", headB)
	if _, _, err := h.store.CreateThread(ctx, store.ThreadRow{Key: "ended", Title: "x", Agent: "duncan", SupervisorSession: h.super.ID, Task: "x", TaskID: 77}); err != nil {
		t.Fatal(err)
	}
	h.engine.mu.Lock()
	if h.engine.ended == nil {
		h.engine.ended = map[int64]bool{}
	}
	h.engine.ended[77] = true
	h.engine.mu.Unlock()
	if _, err := h.server.deliverAll(ctx); err != nil {
		t.Log(err)
	}
	if _, err := h.store.ThreadByKey(ctx, key); err != nil {
		t.Fatalf("no review after the delivery pass closed a thread: %v", err)
	}
}

// A label someone put on while CI was read comes off when GitHub then
// names no merge base: nothing backs it (Codex review of #26, round 10).
func TestALabelSeenWithNoMergeBaseComesOff(t *testing.T) {
	h := newReviewHarness(t)
	h.gh.set(func(g *fakeGitHub) {
		g.onCI = func(g *fakeGitHub) {
			g.labels[team.ReadyLabel], g.failCompares, g.onCI = true, 100, nil
		}
	})
	if _, err := h.verdict(h.teg.Session, "v1", headA, team.VerdictReady); err != nil {
		t.Fatal(err)
	}
	if p := h.pull(t); p.Ready || h.gh.labels[team.ReadyLabel] {
		t.Fatalf("a label with no merge base: %+v, labels %v", p, h.gh.labels)
	}
}

// A confirmation answers the read it checked, or none: another read
// recorded meanwhile is a conflict (Codex review of #26, round 11).
func TestAConfirmationAnswersTheReadItChecked(t *testing.T) {
	h := labelled(t)
	ctx := context.Background()
	h.server.afterConfirmRead = func() {
		h.server.afterConfirmRead = nil
		if _, err := h.server.RefreshPulls(ctx); err != nil {
			t.Error(err)
		}
	}
	var pe *proto.Error
	if _, err := h.client.Pull(ctx, h.pull(t).ID); !errors.As(err, &pe) || pe.Code != proto.ErrorCodeConflict {
		t.Fatalf("an answer of a read the confirmation did not check: %v", err)
	}
}

// A reread asked for wins over a fresh read queued with it (Codex review
// of #26, round 11).
func TestAnAskedRereadWinsOverAFreshRead(t *testing.T) {
	h := newReviewHarness(t)
	ctx := context.Background()
	p := h.pull(t)
	h.gh.set(func(g *fakeGitHub) { g.head = headB })
	h.server.reviewPassOf(ctx, reviewWork{fresh: map[int64]int64{p.ID: p.ReadSeq}, read: map[int64]bool{p.ID: true}})
	if _, err := h.store.ThreadByKey(ctx, team.ReviewKey("frostyard/example", 3, "main", headB)); err != nil {
		t.Fatalf("no review of the head the reread found: %v", err)
	}
}

// A review not briefed yet whose pinned merge base moved under the same
// head and branch ends (Codex review of #26, round 11).
func TestAnUnbriefedReviewEndsWhenItsMergeBaseMoves(t *testing.T) {
	h := newReviewHarness(t)
	ctx := context.Background()
	h.server.retry = func(func()) func() { return func() {} }
	h.engine.mu.Lock()
	h.engine.queued = map[int64]bool{}
	h.engine.mu.Unlock()
	h.engine.onSend = func(_ int64, key string) {
		if strings.HasSuffix(key, ":brief") {
			h.engine.mu.Lock()
			h.engine.queued[int64(len(h.engine.sent)+100)] = true
			h.engine.mu.Unlock()
		}
	}
	h.gh.set(func(g *fakeGitHub) { g.head = headB })
	fresh, _, err := h.server.refreshPulls(ctx)
	if err != nil {
		t.Fatal(err)
	}
	h.server.reviewPassOf(ctx, reviewWork{fresh: fresh})
	review, err := h.store.ThreadByKey(ctx, team.ReviewKey("frostyard/example", 3, "main", headB))
	if err != nil || review.Briefed || review.ReviewBase == "" {
		t.Fatalf("the review, its brief queued: %+v, %v", review, err)
	}
	h.gh.set(func(g *fakeGitHub) { g.base = strings.Repeat("f", 40) })
	h.server.reviewPassOf(ctx, reviewWork{fresh: map[int64]int64{}})
	review, err = h.store.Thread(ctx, review.ID)
	if err != nil || review.State != store.ThreadClosed || !strings.Contains(review.Report, "Superseded") {
		t.Fatalf("the review after its merge base moved: %+v, %v", review, err)
	}
}

// A review's pull request is checked again right before its session
// starts: one closed while its worktree was made starts none (Codex review
// of #26, round 11).
func TestAReviewChecksItsPullBeforeItsSession(t *testing.T) {
	h := newReviewHarness(t)
	ctx := context.Background()
	h.server.reviewGit = fakeReviewGit{onAdd: func() {
		h.gh.set(func(g *fakeGitHub) { g.state = "closed" })
		if _, err := h.server.RefreshPulls(ctx); err != nil {
			t.Error(err)
		}
	}}
	h.gh.set(func(g *fakeGitHub) { g.head = headB })
	fresh, _, err := h.server.refreshPulls(ctx)
	if err != nil {
		t.Fatal(err)
	}
	h.server.reviewPassOf(ctx, reviewWork{fresh: fresh})
	review, err := h.store.ThreadByKey(ctx, team.ReviewKey("frostyard/example", 3, "main", headB))
	if err != nil {
		t.Fatal(err)
	}
	if review.Session != 0 {
		t.Fatalf("a session started for a pull request closed meanwhile: %+v", review)
	}
}

// The service's start reads every open pull request again: a report's
// reread that a stop lost leaves no verdict or label on the merge base
// pocketd last knew (Codex review of #26, round 12).
func TestTheStartRereadsTheOpenPulls(t *testing.T) {
	h := labelled(t)
	h.server.reviewsOn = false
	// The base branch was reset, and the report that named it again
	// committed before the stop: no read recorded it.
	h.gh.set(func(g *fakeGitHub) { g.base = strings.Repeat("f", 40) })
	h.server.StartPulls(0)
	if p := h.pull(t); p.Ready || h.gh.labels[team.ReadyLabel] || p.MergeBase != strings.Repeat("f", 40) {
		t.Fatalf("after the start: %+v, labels %v", p, h.gh.labels)
	}
}

// failingAdd is fakeReviewGit whose worktree is not made while *fails
// counts down.
type failingAdd struct {
	fakeReviewGit
	fails *int
}

func (g failingAdd) add(ctx context.Context, checkout, path, head, repository string, number int64) error {
	if *g.fails > 0 {
		*g.fails--
		return errors.New("git worktree add failed")
	}
	return g.fakeReviewGit.add(ctx, checkout, path, head, repository, number)
}

// A review whose start failed starts at a later pass after its author's
// profile spelled the repository's key in another case: the key is found
// as GitHub names repositories, ignoring case (Codex review of #26, round
// 12).
func TestARetriedReviewFindsItsCheckoutInAnyCase(t *testing.T) {
	h := newReviewHarness(t)
	ctx := context.Background()
	fails := 1
	h.server.reviewGit = failingAdd{fails: &fails}
	h.gh.set(func(g *fakeGitHub) { g.head = headB })
	fresh, _, err := h.server.refreshPulls(ctx)
	if err != nil {
		t.Fatal(err)
	}
	h.server.reviewPassOf(ctx, reviewWork{fresh: fresh})
	key := team.ReviewKey("frostyard/example", 3, "main", headB)
	review, err := h.store.ThreadByKey(ctx, key)
	if err != nil || review.Session != 0 || review.Repository != "frostyard/example" {
		t.Fatalf("the review whose start failed: %+v, %v", review, err)
	}
	h.writeProfiles(map[string]string{
		"odrade.md": "---\nrole: supervisor\nmodel: faux/faux-1\nhome: ~\n---\n# Odrade\n",
		"vera.md":   "---\nmodel: anthropic/claude-opus-5-5\nrepositories:\n  Frostyard/Example: " + h.checkout + "\n---\n# Vera\n",
		"duncan.md": "---\nmodel: anthropic/claude-opus-5-5\nhome: ~\n---\n# Duncan\n",
	})
	h.server.reviewPassOf(ctx, reviewWork{fresh: map[int64]int64{}})
	if review, err = h.store.Thread(ctx, review.ID); err != nil || review.Session == 0 || !review.Briefed {
		t.Fatalf("the review retried after the key's case changed: %+v, %v", review, err)
	}
}

// queueBriefs has every brief sent to h's engine wait in its session's
// queue, as a busy session's does, and nothing retried on a timer.
func (h *reviewHarness) queueBriefs() {
	h.server.retry = func(func()) func() { return func() {} }
	h.engine.mu.Lock()
	h.engine.queued, h.engine.withdrawn = map[int64]bool{}, map[int64]bool{}
	h.engine.mu.Unlock()
	h.engine.onSend = func(_ int64, key string) {
		if strings.HasSuffix(key, ":brief") {
			h.engine.mu.Lock()
			h.engine.queued[int64(len(h.engine.sent)+100)] = true
			h.engine.mu.Unlock()
		}
	}
}

// unbriefedReview is the review pocketd starts at headB, its brief still
// queued.
func (h *reviewHarness) unbriefedReview(t *testing.T) store.ThreadRow {
	t.Helper()
	ctx := context.Background()
	h.queueBriefs()
	h.gh.set(func(g *fakeGitHub) { g.head = headB })
	fresh, _, err := h.server.refreshPulls(ctx)
	if err != nil {
		t.Fatal(err)
	}
	h.server.reviewPassOf(ctx, reviewWork{fresh: fresh})
	review, err := h.store.ThreadByKey(ctx, team.ReviewKey("frostyard/example", 3, "main", headB))
	if err != nil || review.Briefed || review.BriefSubmission == 0 {
		t.Fatalf("the review, its brief queued: %+v, %v", review, err)
	}
	return review
}

// A reread asked for runs before the pass settles the reviews: a review
// not briefed yet, its brief withdrawn, is not sent its brief again at the
// head the reread moved from, on a fresh read queued with the reread
// (Codex review of #26, round 12).
func TestARereadComesBeforeAnUnbriefedReviewsBrief(t *testing.T) {
	h := newReviewHarness(t)
	ctx := context.Background()
	review := h.unbriefedReview(t)
	queued := h.pull(t)
	h.engine.onSend = nil
	h.engine.mu.Lock()
	h.engine.withdrawn[review.BriefSubmission] = true
	h.engine.mu.Unlock()
	sent := len(h.sentTo(review.Session))
	h.gh.set(func(g *fakeGitHub) { g.head = strings.Repeat("c", 40) })
	h.server.reviewPassOf(ctx, reviewWork{fresh: map[int64]int64{queued.ID: queued.ReadSeq}, read: map[int64]bool{queued.ID: true}})
	if again := h.sentTo(review.Session); len(again) != sent {
		t.Fatalf("the review's brief went again at the old head: %d sends, had %d", len(again), sent)
	}
	if got, err := h.store.Thread(ctx, review.ID); err != nil || got.State != store.ThreadClosed || !strings.Contains(got.Report, "Superseded") {
		t.Fatalf("the review of the old head: %+v, %v", got, err)
	}
}

// A reread asked for reads a pull request last recorded closed: one that
// reopened is reviewed (Codex review of #26, round 12).
func TestARereadFindsAReopenedPull(t *testing.T) {
	h := newReviewHarness(t)
	ctx := context.Background()
	h.gh.set(func(g *fakeGitHub) { g.state = "closed" })
	if _, err := h.server.RefreshPulls(ctx); err != nil {
		t.Fatal(err)
	}
	if p := h.pull(t); p.State != store.PullClosed {
		t.Fatalf("the pull request recorded: %+v", p)
	}
	h.gh.set(func(g *fakeGitHub) { g.state, g.head = "open", headB })
	h.server.reviewAfterRead(h.pull(t).ID)
	if _, err := h.store.ThreadByKey(ctx, team.ReviewKey("frostyard/example", 3, "main", headB)); err != nil {
		t.Fatalf("no review of the reopened pull request: %v", err)
	}
}

// A reread asked for that fails starts nothing from a fresh read queued
// with it: the one who asked found that read out of date (Codex review of
// #26, round 12).
func TestAFailedRereadStartsNothingFromAQueuedRead(t *testing.T) {
	h := newReviewHarness(t)
	ctx := context.Background()
	h.gh.set(func(g *fakeGitHub) { g.head = headB })
	fresh, _, err := h.server.refreshPulls(ctx)
	if err != nil {
		t.Fatal(err)
	}
	h.gh.set(func(g *fakeGitHub) { g.failReads = 100 })
	h.server.reviewPassOf(ctx, reviewWork{fresh: fresh, read: map[int64]bool{h.pull(t).ID: true}})
	if _, err := h.store.ThreadByKey(ctx, team.ReviewKey("frostyard/example", 3, "main", headB)); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a review started from the queued read: %v", err)
	}
}

// hookedHas is fakeReviewGit that runs onHas, once, when a review's base
// is looked for in its worktree, after the brief pinned it.
type hookedHas struct {
	fakeReviewGit
	onHas *func()
}

func (g hookedHas) has(context.Context, string, string) (bool, error) {
	if f := *g.onHas; f != nil {
		*g.onHas = nil
		f()
	}
	return true, nil
}

// A review's brief goes only while the pull request's merge base is
// known and the one it pinned: a read whose compare failed meanwhile left
// it unknown, and no session starts (Codex review of #26, round 12).
func TestABriefWaitsForAKnownMergeBase(t *testing.T) {
	h := newReviewHarness(t)
	ctx := context.Background()
	h.gh.set(func(g *fakeGitHub) { g.head = headB })
	fresh, _, err := h.server.refreshPulls(ctx)
	if err != nil {
		t.Fatal(err)
	}
	onHas := func() {
		h.gh.set(func(g *fakeGitHub) { g.failCompares = 100 })
		if _, err := h.server.RefreshPulls(ctx); err == nil {
			t.Error("a read whose compare failed was done")
		}
	}
	h.server.reviewGit = hookedHas{onHas: &onHas}
	h.server.reviewPassOf(ctx, reviewWork{fresh: fresh})
	if p := h.pull(t); p.MergeBase != "" || p.HeadSHA != headB {
		t.Fatalf("the pull request after the failed compare: %+v", p)
	}
	review, err := h.store.ThreadByKey(ctx, team.ReviewKey("frostyard/example", 3, "main", headB))
	if err != nil || review.ReviewBase != reviewBase {
		t.Fatalf("the review: %+v, %v", review, err)
	}
	if review.Session != 0 {
		t.Fatalf("a session started on an unknown merge base: %+v", review)
	}
}

// A review not briefed yet ends on what a read recorded before it failed:
// a failed read keeps a review from starting, not from ending (Codex
// review of #26, round 12).
func TestAFailedReadStillEndsAnUnbriefedReview(t *testing.T) {
	h := newReviewHarness(t)
	ctx := context.Background()
	review := h.unbriefedReview(t)
	h.gh.set(func(g *fakeGitHub) { g.draft, g.failCompares = true, 100 })
	h.server.reviewPassOf(ctx, reviewWork{fresh: map[int64]int64{}})
	if p := h.pull(t); !p.Draft {
		t.Fatalf("the read recorded no draft: %+v", p)
	}
	if got, err := h.store.Thread(ctx, review.ID); err != nil || got.State != store.ThreadClosed || !strings.Contains(got.Report, "draft") {
		t.Fatalf("the review after a read that failed: %+v, %v", got, err)
	}
}

// A confirmation's read that records a moved head and then fails still
// ends the review it made obsolete, and stops its run (Codex review of
// #26, round 12).
func TestAFailedConfirmationReadStillEndsTheReview(t *testing.T) {
	h := newReviewHarness(t)
	ctx := context.Background()
	h.engine.mu.Lock()
	h.engine.runFirst = 1
	h.engine.mu.Unlock()
	h.gh.set(func(g *fakeGitHub) { g.head, g.failCompares = headB, 100 })
	var pe *proto.Error
	if _, err := h.client.Pull(ctx, h.pull(t).ID); !errors.As(err, &pe) || pe.Code != proto.ErrorCodeInternal {
		t.Fatalf("a read whose compare failed: %v", err)
	}
	if got, err := h.store.Thread(ctx, h.teg.ID); err != nil || got.State != store.ThreadClosed || !strings.Contains(got.Report, "Superseded") {
		t.Fatalf("the review of the old head: %+v, %v", got, err)
	}
	h.engine.mu.Lock()
	aborted := slices.Clone(h.engine.aborted)
	h.engine.mu.Unlock()
	if !slices.Contains(aborted, h.teg.Session) {
		t.Fatalf("aborted %v: the old review's run went on", aborted)
	}
	if _, err := h.store.ThreadByKey(ctx, team.ReviewKey("frostyard/example", 3, "main", headB)); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a review started from a read that failed: %v", err)
	}
}

// A confirmation's read that finds the pull request merged closes its
// author's thread, as a round does: with -pr-every=0 none comes (ADR-0016;
// Codex review of #26, round 12).
func TestReadingOnePullClosesAMergedThread(t *testing.T) {
	h := newReviewHarness(t)
	ctx := context.Background()
	h.gh.set(func(g *fakeGitHub) { g.state, g.merged = "closed", true })
	got, err := h.client.Pull(ctx, h.pull(t).ID)
	if err != nil || got.State != proto.PullState(store.PullMerged) {
		t.Fatalf("the read: %+v, %v", got, err)
	}
	if vera, err := h.store.Thread(ctx, h.vera.ID); err != nil || vera.State != store.ThreadClosed {
		t.Fatalf("the author's thread after its pull request merged: %+v, %v", vera, err)
	}
}

// A report's pass that finds its pull request merged closes the author's
// thread (ADR-0016; Codex review of #26, round 12).
func TestAReportsPassClosesAMergedThread(t *testing.T) {
	h := newReviewHarness(t)
	ctx := context.Background()
	// The report's own read finds it open; it merges before the pass's.
	h.gh.set(func(g *fakeGitHub) {
		g.onRead = func(g *fakeGitHub) { g.state, g.merged, g.onRead = "closed", true, nil }
	})
	if _, err := h.server.TeamMessage(ctx, TeamCall{Session: h.vera.Session, Task: 2, CallID: "m2"},
		team.Report{Message: "PR #3 again.", Status: team.StatusWorking, PullRequests: []team.PullRef{{Number: 3}}}); err != nil {
		t.Fatal(err)
	}
	if p := h.pull(t); p.State != store.PullMerged {
		t.Fatalf("the pass's read: %+v", p)
	}
	if vera, err := h.store.Thread(ctx, h.vera.ID); err != nil || vera.State != store.ThreadClosed {
		t.Fatalf("the author's thread after its pull request merged: %+v, %v", vera, err)
	}
}

// The owner's close of a review whose brief went stops its run, and a stop
// that fails is tried again at the next pass (Codex review of #26, round
// 12).
func TestAnOwnerCloseStopsTheReviewersRun(t *testing.T) {
	h := newReviewHarness(t)
	ctx := context.Background()
	h.engine.mu.Lock()
	h.engine.runFirst, h.engine.abortErr = 1, errors.New("abort failed")
	h.engine.mu.Unlock()
	if _, err := h.client.CloseThread(ctx, h.teg.ID, proto.CloseThread{RequestID: "c"}); err != nil {
		t.Fatal(err)
	}
	aborted := func() []int64 {
		h.engine.mu.Lock()
		defer h.engine.mu.Unlock()
		return slices.Clone(h.engine.aborted)
	}
	if got, _ := h.store.Thread(ctx, h.teg.ID); got.State != store.ThreadClosed || len(aborted()) != 0 {
		t.Fatalf("the review after the close: %+v, aborted %v", got, aborted())
	}
	h.server.reviewPass(ctx)
	if !slices.Contains(aborted(), h.teg.Session) {
		t.Fatalf("aborted %v: the closed review's run went on", aborted())
	}
}
