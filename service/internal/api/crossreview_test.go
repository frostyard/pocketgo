package api

import (
	"context"
	"slices"
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
	if _, found, _ := h.store.LatestReview(context.Background(), h.pull(t).ID, headA); found {
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
