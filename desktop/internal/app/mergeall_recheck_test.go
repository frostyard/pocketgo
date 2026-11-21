package app

import (
	"slices"
	"testing"

	"github.com/frostyard/pocketgo/desktop/internal/github"
	"github.com/frostyard/pocketgo/desktop/internal/state"
	"github.com/frostyard/pocketgo/proto"
)

// porterBump is Dependabot's pull request n on frostyard/porter, linked
// to T2 as porter53 is, with pocketd's ready cross-review and green CI at
// mergeHead.
func porterBump(n int64, title, branch string) proto.PullRequest {
	p := porter53
	review := *p.Review
	review.ID = 100 + n
	p.ID, p.Number, p.Title, p.Head, p.Review = n, n, title, branch, &review
	p.URL = "https://github.com/frostyard/porter/pull/" + itoa(int(n))
	return p
}

// Dependabot's pull requests on one base all go in one run: once the first
// merged and moved the base, each other is checked again at the new base
// at its turn, which the bar shows, and merges there or stays with why
// (ADR-0038, the owner's choice of 2026-11-21).
func TestMergeAllReadyChecksAgainAtTheNewBaseSnapshot(t *testing.T) {
	auth := &ghAuth{wait: make(chan struct{})}
	close(auth.wait)
	x, api := newMergeApp(t, auth)
	porter54 := porterBump(54, "Bump golang.org/x/sys from 0.38.0 to 0.39.0", "dependabot/go_modules/golang.org/x/sys-0.39.0")
	porter55 := porterBump(55, "Bump actions/checkout from 6.0.0 to 6.0.1", "dependabot/github_actions/actions/checkout-6.0.1")
	held := make(chan struct{})
	api.mu.Lock()
	api.preview.RiskHigh, api.preview.DependencyBot = true, true
	api.bases = map[string]string{"frostyard/nsl:main": mergeBase, "frostyard/porter:main": mergeBase}
	api.strict = map[string]bool{"frostyard/porter#55": true}
	api.again = map[string]chan struct{}{"frostyard/porter#54": held}
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
	x.hasAll(t, "Approve and merge 4 pull requests?",
		"One listed without its diff opened whose base an earlier merge moved is checked again at the new base first.")
	var order []string
	for _, it := range x.a.gh.MergeAll().Items {
		order = append(order, it.Ref.String())
	}
	if want := []string{"frostyard/nsl#31", "frostyard/porter#53", "frostyard/porter#54", "frostyard/porter#55"}; !slices.Equal(order, want) {
		t.Fatalf("listed %v, want %v", order, want)
	}

	x.click(t, "Approve + Merge 4")
	x.until(t, func() bool { return x.a.gh.MergeAll().Rechecking })
	x.hasAll(t, "Approving and merging 3 of 4 as bketelsen: frostyard/porter#54 · checking it again at its new base…")
	x.snapshot(t, "inbox-merge-all-recheck")
	close(held)
	x.until(t, func() bool { return x.a.gh.MergeAll().Phase == state.MergeAllEnded })
	x.hasAll(t, "Merge All Ready: 3 merged, 1 stayed, 0 not tried; 2 checked again at a new base",
		"frostyard/porter#55 (checked again at its new base): Refused. the PR is behind its base")
	x.snapshot(t, "inbox-merge-all-recheck-ended")
	api.mu.Lock()
	defer api.mu.Unlock()
	if len(api.intents) != 3 {
		t.Fatalf("intents %+v", api.intents)
	}
	if in := api.intents[2]; in.Preview.Ref != (github.Ref{Owner: "frostyard", Name: "porter", Number: 54}) || in.Preview.BaseOid == mergeBase || !in.WaiveDiff {
		t.Fatalf("porter#54's intent %+v", in)
	}
	// pocketd read each on GitHub at its turn, after the check again.
	for _, id := range []int64{1, 53, 54} {
		if got := len(x.srv.RequestsTo("GET", proto.Path(proto.PathPull, id))); got != 1 {
			t.Errorf("pull %d read %d times from pocketd at its turn, want 1", id, got)
		}
	}
}
