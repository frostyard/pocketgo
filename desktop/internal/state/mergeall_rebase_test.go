package state

import (
	"slices"
	"testing"

	"github.com/frostyard/pocketgo/desktop/internal/github"
	"github.com/frostyard/pocketgo/proto"
)

// The reasons a run counts as waiting for a rebase are the gates' own, as
// github.Check gives them for a pull request that conflicts with its base,
// or is behind it where the rules need it up to date: before the approval
// and after it, on a base with no bypass and on one the owner bypasses.
func TestTheRebaseReasonsAreTheGatesOwn(t *testing.T) {
	ok := github.PRFacts{Viewer: github.Login, DefaultMergeMethod: "SQUASH", SquashMergeAllowed: true, State: "OPEN",
		BaseRefName: "main", HeadRefOid: headX, CommitOid: headX, Mergeable: "MERGEABLE", MergeStateStatus: "CLEAN",
		Labels: []string{"ready-for-human"}}
	direct := github.RepoRules{Bypass: "never"}
	if pass, why := github.Check(ok, direct, github.AtPreview, github.CrossReview{}); !pass {
		t.Fatalf("the facts do not pass: %q", why)
	}
	approved := ok
	approved.ViewerLatestReview = &github.Review{State: "APPROVED", Author: github.Login, Commit: headX}
	approved.ReviewDecision = "APPROVED"
	strict := github.RepoRules{Bypass: "always", Proof: github.BypassRules{Strict: true}}
	behindStrict := ok
	behindStrict.Bypass = &github.BypassFacts{Head: headX, Behind: 2}

	var seen []string
	for name, c := range map[string]struct {
		f     func(f *github.PRFacts)
		r     github.RepoRules
		stage github.Stage
		base  github.PRFacts
	}{
		"conflicting":               {f: func(f *github.PRFacts) { f.Mergeable = "CONFLICTING" }, r: direct, base: ok},
		"dirty":                     {f: func(f *github.PRFacts) { f.MergeStateStatus = "DIRTY" }, r: direct, base: ok},
		"behind":                    {f: func(f *github.PRFacts) { f.MergeStateStatus = "BEHIND" }, r: direct, base: ok},
		"behind, a bypassed rule":   {f: func(*github.PRFacts) {}, r: strict, base: behindStrict},
		"dirty after the approval":  {f: func(f *github.PRFacts) { f.MergeStateStatus = "DIRTY" }, r: direct, stage: github.AfterApproval, base: approved},
		"behind after the approval": {f: func(f *github.PRFacts) { f.MergeStateStatus = "BEHIND" }, r: direct, stage: github.AfterApproval, base: approved},
	} {
		f := c.base
		c.f(&f)
		pass, why := github.Check(f, c.r, c.stage, github.CrossReview{})
		if pass || !slices.Contains(rebaseReasons, why) {
			t.Errorf("%s: %v %q is no rebase reason", name, pass, why)
		}
		seen = append(seen, why)
	}
	for _, want := range rebaseReasons {
		if !slices.Contains(seen, want) {
			t.Errorf("no gate gives %q", want)
		}
	}
}

// A dependency bot's pull request that the run's own merges left in
// conflict with its base, or behind it, waits for its bot to rebase it:
// the summary counts it apart from those that stayed, by the bot's name
// (the owner's choice of 2026-11-21).
func TestABotsPullTheRunLeftBehindWaitsForItsBot(t *testing.T) {
	h := botHarness(t, "frostyard/nsl:main")
	h.api.preview = botPreview
	conflicting, behind := botPreview, botPreview
	conflicting.Eligible, conflicting.Reason = false, "the PR has conflicts"
	behind.Eligible, behind.Reason = false, "the PR is behind its base"
	h.api.onMove = func(string) {
		if h.api.moves == 1 {
			h.api.previews = map[string]github.Preview{refOf(32).String(): conflicting, refOf(33).String(): behind}
		}
	}
	rows := []proto.PullRequest{backedPull(31), backedPull(32), backedPull(33), backedPull(34)}
	m := runAll(t, h, confirmAll(t, h, rows), newPocketdReads(h, rows...))
	if c := m.Counts(); c != (MergeAllCounts{Merged: 2, Rebase: 2, RebaseBy: "Dependabot", Rechecked: 3}) {
		t.Fatalf("counts %+v, items %+v", c, m.Items)
	}
	if m.Items[1].Rebase != "Dependabot" || m.Items[2].Rebase != "Dependabot" || m.Items[3].Rebase != "" {
		t.Fatalf("items %+v", m.Items)
	}
	want := "Merge all ready: 2 merged, 2 wait for Dependabot to rebase, 0 stayed, 0 not tried; 3 checked again at a new base"
	if last := h.logs[len(h.logs)-1]; last != want {
		t.Fatalf("the log says %q, want %q", last, want)
	}
}

// A bot's pull request that is no risk:high one is not checked again, and
// Act's own read finds the conflict: it waits for its bot all the same,
// Renovate here, and an approval that went before the merge was refused
// counts too.
func TestABotsPullActFindsInConflictWaitsForItsBot(t *testing.T) {
	h := botHarness(t, "frostyard/nsl:main")
	p := botPreview
	p.RiskHigh, p.Bot = false, "renovate"
	h.api.preview = p
	h.api.results = map[string]github.Result{
		refOf(32).String(): {Kind: "refused", Step: "read", Reason: "the PR has conflicts", Head: headX, Base: "main"},
		refOf(33).String(): {Kind: "approved", Step: "read 2", Reason: "the PR is behind its base", Head: headX, Base: "main"},
	}
	rows := []proto.PullRequest{backedPull(31), backedPull(32), backedPull(33)}
	m := runAll(t, h, confirmAll(t, h, rows), newPocketdReads(h, rows...))
	if c := m.Counts(); c != (MergeAllCounts{Merged: 1, Rebase: 2, RebaseBy: "Renovate"}) || m.Items[1].Rechecked {
		t.Fatalf("counts %+v, items %+v", c, m.Items)
	}
	if got := (MergeAllCounts{Merged: 1, Rebase: 1, RebaseBy: "Renovate"}).String(); got != "1 merged, 1 waits for Renovate to rebase, 0 stayed, 0 not tried" {
		t.Fatalf("one waits: %q", got)
	}
}

// What the run's merges did not cause stays a failure: a conflict before
// any merge of the run, one on a base no merge of the run moved, a
// person's pull request, and a bot's that fails for another reason. So do
// a conflict after a merge whose end is unknown, which may not have landed,
// and one a read found on another branch than the confirmed one, as after
// a retarget (Codex review of #64, round 1).
func TestAConflictTheRunDidNotCauseStaysAFailure(t *testing.T) {
	conflict := github.Result{Kind: "refused", Step: "read", Reason: "the PR has conflicts", Head: headX, Base: "main"}
	retargeted := conflict
	retargeted.Base = "release"
	unknown := github.Result{Kind: "unknown", Step: "reconcile merge", Reason: "merge may be pending"}
	porter := github.Ref{Owner: "frostyard", Name: "porter", Number: 53}
	for name, tc := range map[string]struct {
		person  bool
		results map[string]github.Result
		rows    []proto.PullRequest
		stays   int // the item that stays
		merged  int
	}{
		"before any merge": {results: map[string]github.Result{refOf(31).String(): conflict},
			rows: []proto.PullRequest{backedPull(31), backedPull(32)}, stays: 0, merged: 1},
		"on another base": {results: map[string]github.Result{refOf(31).String(): conflict},
			rows: []proto.PullRequest{pullIn("porter", 53, 53), backedPull(31)}, stays: 1, merged: 1},
		"a person's": {person: true, results: map[string]github.Result{refOf(32).String(): conflict},
			rows: []proto.PullRequest{backedPull(31), backedPull(32)}, stays: 1, merged: 1},
		"another reason": {results: map[string]github.Result{refOf(32).String(): {Kind: "refused", Step: "read", Reason: "a check is failing",
			Head: headX, Base: "main"}}, rows: []proto.PullRequest{backedPull(31), backedPull(32)}, stays: 1, merged: 1},
		"after a merge whose end is unknown": {results: map[string]github.Result{refOf(31).String(): unknown, refOf(32).String(): conflict},
			rows: []proto.PullRequest{backedPull(31), backedPull(32)}, stays: 1},
		"on the branch it was retargeted to": {results: map[string]github.Result{refOf(32).String(): retargeted},
			rows: []proto.PullRequest{backedPull(31), backedPull(32)}, stays: 1, merged: 1},
	} {
		t.Run(name, func(t *testing.T) {
			h := botHarness(t, "frostyard/nsl:main", "frostyard/porter:main")
			p := botPreview
			p.RiskHigh = false
			if tc.person {
				p.DependencyBot, p.Bot = false, ""
			}
			h.api.preview, h.api.results = p, tc.results
			m := runAll(t, h, confirmAll(t, h, tc.rows), newPocketdReads(h, tc.rows...))
			if c := m.Counts(); c.Stayed != 1 || c.Rebase != 0 || c.Merged != tc.merged || m.Items[tc.stays].Rebase != "" ||
				m.Items[tc.stays].Result.Kind != tc.results[m.Items[tc.stays].Ref.String()].Kind {
				t.Fatalf("counts %+v, items %+v", c, m.Items)
			}
			if name == "on another base" && previewsOf(h, porter) != 1 {
				t.Fatal("porter#53 read again")
			}
		})
	}
}

// A run's waits name each bot once, in the order the items came.
func TestTheRebaseCountNamesEachBotOnce(t *testing.T) {
	waits := func(bots ...string) MergeAll {
		var m MergeAll
		for _, b := range bots {
			m.Items = append(m.Items, MergeAllItem{Result: github.Result{Kind: "refused"}, Rebase: b})
		}
		return m
	}
	c := waits("Dependabot", "Renovate", "Dependabot").Counts()
	if c != (MergeAllCounts{Rebase: 3, RebaseBy: "Dependabot or Renovate"}) {
		t.Fatalf("counts %+v", c)
	}
	if got := c.String(); got != "0 merged, 3 wait for Dependabot or Renovate to rebase, 0 stayed, 0 not tried" {
		t.Fatalf("%q", got)
	}
}
