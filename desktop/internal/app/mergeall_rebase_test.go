package app

import (
	"testing"

	"github.com/frostyard/pocketgo/desktop/internal/github"
	"github.com/frostyard/pocketgo/desktop/internal/state"
	"github.com/frostyard/pocketgo/proto"
)

// A Dependabot pull request the run's own merge left behind its base waits
// for Dependabot to rebase it: the summary counts it apart from the
// failures, and lists it after them, by the bot's name (ADR-0039, the
// owner's choice of 2026-11-21). One whose approval went in before the
// merge was refused says so (Codex review of #64, round 1).
func TestMergeAllReadyCountsRebaseWaitsApartSnapshot(t *testing.T) {
	auth := &ghAuth{wait: make(chan struct{})}
	close(auth.wait)
	x, api := newMergeApp(t, auth)
	porter54 := porterBump(54, "Bump golang.org/x/sys from 0.38.0 to 0.39.0", "dependabot/go_modules/golang.org/x/sys-0.39.0")
	porter55 := porterBump(55, "Bump actions/checkout from 6.0.0 to 6.0.1", "dependabot/github_actions/actions/checkout-6.0.1")
	api.mu.Lock()
	api.preview.RiskHigh, api.preview.DependencyBot, api.preview.Bot = true, true, "dependabot"
	api.bases = map[string]string{"frostyard/nsl:main": mergeBase, "frostyard/porter:main": mergeBase}
	api.strict = map[string]bool{"frostyard/porter#54": true}
	api.results = map[string]github.Result{
		"frostyard/nsl#31":    {Kind: "refused", Step: "read", Reason: "a review requests changes"},
		"frostyard/porter#55": {Kind: "approved", Step: "read 2", Reason: "the PR has conflicts", Head: mergeHead, Base: "main"},
	}
	api.mu.Unlock()
	for _, p := range []proto.PullRequest{porter53, porter54, porter55} {
		x.srv.AddPull(p)
	}
	x.a.st.Refresh()
	x.until(t, func() bool { return len(x.a.st.Data.PullsToReview()) == 4 })
	x.click(t, "Sign In to GitHub")
	x.until(t, func() bool { p, _, _ := x.a.gh.SignIn(); return p == state.SignedIn })
	x.click(t, "Merge All Ready")
	x.until(t, func() bool { return x.a.gh.MergeAll().Phase == state.MergeAllConfirming })
	x.click(t, "Approve + Merge 4")
	x.until(t, func() bool { return x.a.gh.MergeAll().Phase == state.MergeAllEnded })
	x.hasAll(t, "Merge All Ready: 1 merged, 2 wait for Dependabot to rebase, 1 stayed, 0 not tried; 2 checked again at a new base",
		"frostyard/nsl#31: Refused. a review requests changes",
		"frostyard/porter#54: waits for Dependabot to rebase it (the PR is behind its base)",
		"frostyard/porter#55: waits for Dependabot to rebase it (approved as bketelsen, not merged: the PR has conflicts)")
	x.snapshot(t, "inbox-merge-all-rebase-wait")
	if x.tt.HasText("frostyard/porter#54 (checked again at its new base): Refused. the PR is behind its base") {
		t.Error("the wait is listed among the failures")
	}
}
