package github

import (
	"context"
	"strings"
	"testing"
)

// The extra approval of require_extra_approval_for_unattributed_changes
// counts as GitHub counts it, by GitHub's own review decision (pocketgo
// ADR-0059, frostyard/pocketgo#52). GitHub documents it for a pull request
// Copilot opens under its own identity; frostyard/frostui#46, by
// Dependabot, under the org ruleset, merged with 1 approval and showed
// APPROVED.

// dependabotPR is botPR by Dependabot, as GitHub's GraphQL names it in both
// reads, which GitHub asks 1 approval of, as it did frostyard/frostui#46:
// the review is required and the merge blocked before the owner's
// approval, and approved and clean after it, with that approval alone.
func dependabotPR(t *testing.T) *fake {
	f := botPR(t)
	f.pr()["author"] = map[string]any{"__typename": "Bot", "login": "dependabot"}
	obj(f.bypassPR, "data", "repository", "pullRequest", "author")["login"] = "dependabot"
	f.afterApprove = func(pr map[string]any) {
		pr["reviewDecision"], pr["mergeStateStatus"] = "APPROVED", "CLEAN"
	}
	return f
}

// orgRules is the org ruleset's pull_request rule as Rules reads it on a
// base the owner can bypass: 1 approval, the extra one, and the last
// push's approval.
func orgRules(count int) RepoRules {
	return RepoRules{Bypass: "always", Proof: BypassRules{PullRequest: true, LastPush: true,
		ApprovalRules: []ApprovalRule{{Count: count, Extra: true}}}}
}

// approvalFacts is a read of a pull request by author whose head the
// approvers approved, with GitHub's review decision in both reads;
// Dependabot pushed last.
func approvalFacts(author, decision string, approvers ...string) PRFacts {
	head := strings.Repeat("a", 40)
	return PRFacts{Viewer: Login, HeadRefOid: head, BaseRefName: "main", ReviewDecision: decision,
		Bypass: &BypassFacts{Head: head, AuthorType: author, Approvers: approvers, LastPusher: "dependabot[bot]",
			ReviewDecision: decision}}
}

// frostyard/pocketgo#52: a Dependabot pull request under the org ruleset,
// with the owner's approval alone and no cross-review by pocketd, merges
// once GitHub's review decision says APPROVED: the preview offers it, and
// the read after the approval counts 1 of the 1 GitHub asks.
func TestADependabotPullRequestMergesWithOneApprovalWhenGitHubSaysApproved(t *testing.T) {
	f := dependabotPR(t)
	c := f.client()
	p, err := c.Preview(context.Background(), f.ref, CrossReview{})
	if err != nil || !p.Eligible || !p.DependencyBot {
		t.Fatalf("preview %+v, %v", p, err)
	}
	f.reset()
	res := c.Act(context.Background(), Intent{Preview: p, Mode: "single"}, "op-52", nil)
	if res.Kind != "merged" || strings.Join(f.mutations, ",") != approve+","+merge {
		t.Fatalf("%+v, mutations %v", res, f.mutations)
	}

	// Gate 11 on the read after the approval: 1 approval at the head, the
	// owner's, and GitHub's APPROVED.
	if why := bypassGate(approvalFacts("Bot", "APPROVED", Login), orgRules(1), AfterApproval, CrossReview{}); why != "" {
		t.Fatalf("gate 11 after the approval: %q", why)
	}
}

// After the owner's approval, the extra approval still counts wherever
// GitHub's own count does not show it met: a review still required (a pull
// request GitHub holds at 2, as it documents for an unattributed Copilot
// pull request), a decision that is null or missing, and one PocketGo does
// not know. The pull request stays approved and unmerged, with the count's
// reason: fail closed.
func TestTheExtraApprovalCountsAfterTheApprovalUnlessGitHubSaysApproved(t *testing.T) {
	for name, after := range map[string]func(pr map[string]any){
		"review required": func(pr map[string]any) { pr["reviewDecision"], pr["mergeStateStatus"] = "REVIEW_REQUIRED", "BLOCKED" },
		"null":            func(pr map[string]any) { pr["reviewDecision"], pr["mergeStateStatus"] = nil, "CLEAN" },
		"missing": func(pr map[string]any) {
			delete(pr, "reviewDecision")
			pr["mergeStateStatus"] = "CLEAN"
		},
		"unknown":    func(pr map[string]any) { pr["reviewDecision"], pr["mergeStateStatus"] = "SOMETHING_NEW", "CLEAN" },
		"lower case": func(pr map[string]any) { pr["reviewDecision"], pr["mergeStateStatus"] = "approved", "CLEAN" },
	} {
		t.Run(name, func(t *testing.T) {
			f := dependabotPR(t)
			f.afterApprove = after
			c := f.client()
			p, err := c.Preview(context.Background(), f.ref, CrossReview{})
			if err != nil || !p.Eligible {
				t.Fatalf("preview %+v, %v", p, err)
			}
			f.reset()
			res := c.Act(context.Background(), Intent{Preview: p, Mode: "single"}, "op-extra", nil)
			if res.Kind != "approved" || res.Step != "read 2" ||
				!strings.Contains(res.Reason, "the rules need 2 approvals at the head; PocketGo counts 1") ||
				strings.Join(f.mutations, ",") != approve {
				t.Fatalf("%+v, mutations %v", res, f.mutations)
			}
		})
	}
}

// At the preview, GitHub's answer decides the extra approval only where it
// can: a review required before the owner's approval waits for the read
// after it, and APPROVED drops it; a review still required with the
// owner's approval at the head, a decision that is null or missing, and
// one PocketGo does not know count it, and the card refuses with no
// mutation.
func TestThePreviewCountsTheExtraApprovalUnlessGitHubsAnswerCanCountIt(t *testing.T) {
	for _, c := range []struct {
		name   string
		change func(*fake)
		refuse bool
	}{
		{"review required before the owner's approval", func(*fake) {}, false},
		{"approved", func(f *fake) { f.pr()["reviewDecision"] = "APPROVED" }, false},
		{"review required with the owner's approval at the head", ownerApproved, true},
		{"review required with the owner's approval at an older commit", func(f *fake) {
			f.pr()["viewerLatestReview"] = map[string]any{"state": "APPROVED", "commit": map[string]any{"oid": newHead}}
		}, false},
		{"null", func(f *fake) { f.pr()["reviewDecision"] = nil }, true},
		{"missing", func(f *fake) { delete(f.pr(), "reviewDecision") }, true},
		{"unknown", func(f *fake) { f.pr()["reviewDecision"] = "SOMETHING_NEW" }, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := dependabotPR(t)
			c.change(f)
			p, err := f.client().Preview(context.Background(), f.ref, CrossReview{})
			if err != nil {
				t.Fatal(err)
			}
			if c.refuse != !p.Eligible || c.refuse && !strings.Contains(p.Reason, "the rules need 2 approvals at the head; PocketGo counts 1") {
				t.Fatalf("eligible %v, reason %q; refuse %v", p.Eligible, p.Reason, c.refuse)
			}
			if len(f.mutations) != 0 {
				t.Fatalf("a preview sent %v", f.mutations)
			}
		})
	}
}

// The decision gate 11 takes is the bypass read's, whose approvals it
// counts, and the PR query's must agree: a review dismissed between the
// two reads, after the PR query said APPROVED, leaves the bypass read's
// REVIEW_REQUIRED, and the merge is refused; so is an approval between
// them, the other way round.
func TestTheExtraApprovalCountsWhenTheTwoReadsDisagree(t *testing.T) {
	f := dependabotPR(t)
	c := f.client()
	p, err := c.Preview(context.Background(), f.ref, CrossReview{})
	if err != nil || !p.Eligible {
		t.Fatalf("preview %+v, %v", p, err)
	}
	f.reset()
	f.onBypass = func(repo map[string]any) { obj(repo, "pullRequest")["reviewDecision"] = "REVIEW_REQUIRED" }
	res := c.Act(context.Background(), Intent{Preview: p, Mode: "single"}, "op-race", nil)
	if res.Kind != "approved" || res.Step != "read 2" ||
		!strings.Contains(res.Reason, "the rules need 2 approvals at the head; PocketGo counts 1") ||
		strings.Join(f.mutations, ",") != approve {
		t.Fatalf("%+v, mutations %v", res, f.mutations)
	}

	for _, c := range []struct {
		pr, bypass string
		stage      Stage
	}{
		{"APPROVED", "REVIEW_REQUIRED", AfterApproval},
		{"REVIEW_REQUIRED", "APPROVED", AfterApproval},
		{"APPROVED", "REVIEW_REQUIRED", AtPreview},
		{"REVIEW_REQUIRED", "APPROVED", AtPreview},
		{"APPROVED", "", AfterApproval},
		{"", "APPROVED", AfterApproval},
	} {
		f := approvalFacts("Bot", c.pr, Login)
		f.Bypass.ReviewDecision = c.bypass
		if c.stage == AtPreview {
			f.Bypass.Approvers = nil
		}
		if !extraCounts(f, c.stage) {
			t.Errorf("the PR query %q, the bypass read %q, stage %d: the extra does not count", c.pr, c.bypass, c.stage)
		}
	}
}

// The bypass read takes GitHub's review decision with the approvals: the
// value as GitHub gives it, and "" for a null or missing one.
func TestTheBypassReadTakesTheReviewDecision(t *testing.T) {
	for name, change := range map[string]func(*fake){
		"APPROVED": func(f *fake) { f.pr()["reviewDecision"] = "APPROVED" },
		"":         func(f *fake) { f.pr()["reviewDecision"] = nil },
		"missing":  func(f *fake) { delete(f.pr(), "reviewDecision") },
	} {
		f := dependabotPR(t)
		change(f)
		want := name
		if name == "missing" {
			want = ""
		}
		got, _, err := f.client().read(context.Background(), f.ref)
		if err != nil || got.Bypass == nil || got.Bypass.ReviewDecision != want || got.ReviewDecision != want {
			t.Fatalf("%s: %+v, %v", name, got.Bypass, err)
		}
	}
}

// extraCounts takes GitHub's review decision for the extra approval, and
// counts it on every answer that does not show it met.
func TestExtraCountsAsGitHubsReviewDecisionShows(t *testing.T) {
	for _, c := range []struct {
		author, decision string
		stage            Stage
		approvers        []string
		want             bool
	}{
		{"Bot", "APPROVED", AtPreview, nil, false},
		{"Bot", "APPROVED", AfterApproval, []string{Login}, false},
		{"Bot", "REVIEW_REQUIRED", AtPreview, nil, false},
		{"Bot", "REVIEW_REQUIRED", AtPreview, []string{"someone"}, false},
		{"Bot", "REVIEW_REQUIRED", AtPreview, []string{Login}, true},
		{"Bot", "REVIEW_REQUIRED", AfterApproval, []string{Login}, true},
		{"Bot", "", AtPreview, nil, true},
		{"Bot", "", AfterApproval, []string{Login}, true},
		{"Bot", "CHANGES_REQUESTED", AtPreview, nil, true},
		{"Bot", "SOMETHING_NEW", AtPreview, nil, true},
		{"Bot", "approved", AfterApproval, []string{Login}, true},
		// A null author is no User: GitHub's decision decides as for a Bot.
		{"", "APPROVED", AfterApproval, []string{Login}, false},
		{"", "", AfterApproval, []string{Login}, true},
		{"Mannequin", "REVIEW_REQUIRED", AfterApproval, []string{Login}, true},
		// A User's pull request never needs it, whatever GitHub says.
		{"User", "", AtPreview, nil, false},
		{"User", "REVIEW_REQUIRED", AfterApproval, []string{Login}, false},
		{"User", "SOMETHING_NEW", AfterApproval, []string{Login}, false},
	} {
		if got := extraCounts(approvalFacts(c.author, c.decision, c.approvers...), c.stage); got != c.want {
			t.Errorf("%q %q stage %d approvers %v: %v, want %v", c.author, c.decision, c.stage, c.approvers, got, c.want)
		}
	}
}

// Only the extra approval is GitHub's to count: each rule's own count stays
// PocketGo's, of approvals at the head by accounts that can push. A rule of
// 2 and the extra still needs 2 with GitHub's APPROVED (which may count an
// approval at an older commit), at the preview and after the approval.
func TestGitHubsApprovedNeverLowersARulesOwnCount(t *testing.T) {
	const want = "the rules need 2 approvals at the head; PocketGo counts 1"
	if why := bypassGate(approvalFacts("Bot", "APPROVED", Login), orgRules(2), AfterApproval, CrossReview{}); !strings.Contains(why, want) {
		t.Fatalf("after the approval: %q", why)
	}
	f := dependabotPR(t)
	f.rules = []any{rule("pull_request", prParams(map[string]any{
		"required_approving_review_count":                 2,
		"require_extra_approval_for_unattributed_changes": true,
		"require_last_push_approval":                      true,
		"dismiss_stale_reviews_on_push":                   true,
	}))}
	f.pr()["reviewDecision"] = "APPROVED"
	p, err := f.client().Preview(context.Background(), f.ref, CrossReview{})
	if err != nil || p.Eligible || !strings.Contains(p.Reason, want) {
		t.Fatalf("the preview: %+v, %v", p, err)
	}
}

// A User's pull request counts as before, whatever GitHub's review decision
// says of the extra: 1 under the org ruleset with the owner's approval,
// and 2 under a rule of 2; and it merges from the desktop as before.
func TestAUsersPullRequestCountsAsBefore(t *testing.T) {
	for _, decision := range []string{"", "REVIEW_REQUIRED", "APPROVED", "SOMETHING_NEW"} {
		if why := bypassGate(approvalFacts("User", decision), orgRules(1), AtPreview, CrossReview{}); why != "" {
			t.Errorf("%q, the preview under the org ruleset: %q", decision, why)
		}
		if why := bypassGate(approvalFacts("User", decision, Login), orgRules(1), AfterApproval, CrossReview{}); why != "" {
			t.Errorf("%q, after the approval under the org ruleset: %q", decision, why)
		}
		if why := bypassGate(approvalFacts("User", decision, Login), orgRules(2), AfterApproval, CrossReview{}); !strings.Contains(why, "the rules need 2 approvals at the head; PocketGo counts 1") {
			t.Errorf("%q, after the approval under a rule of 2: %q", decision, why)
		}
	}

	f := dependabotPR(t)
	f.pr()["author"] = map[string]any{"__typename": "User", "login": "frostyard-machines"}
	obj(f.bypassPR, "data", "repository", "pullRequest", "author")["__typename"] = "User"
	obj(f.bypassPR, "data", "repository", "pullRequest", "author")["login"] = "frostyard-machines"
	c := f.client()
	p, err := c.Preview(context.Background(), f.ref, CrossReview{})
	if err != nil || !p.Eligible {
		t.Fatalf("preview %+v, %v", p, err)
	}
	f.reset()
	if res := c.Act(context.Background(), Intent{Preview: p, Mode: "single"}, "op-user", nil); res.Kind != "merged" ||
		strings.Join(f.mutations, ",") != approve+","+merge {
		t.Fatalf("%+v, mutations %v", res, f.mutations)
	}
}

// Merge All Ready's turn of a Dependabot pull request, risk:high and listed
// without its diff opened, takes the same gates on each read as a card's
// merge (pocketgo ADR-0031, ADR-0038): it merges once GitHub says APPROVED
// after the owner's approval, with pocketd's verdict counted on top and
// not needed; where GitHub still requires a review, the verdict makes up
// the extra approval as before (ADR-0031); and with neither, it stays
// approved and unmerged.
func TestMergeAllReadysTurnCountsTheExtraApprovalAsGitHubDoes(t *testing.T) {
	for _, c := range []struct {
		name     string
		decision string
		verdict  bool
		kind     string
	}{
		{"GitHub approved, with the verdict", "APPROVED", true, "merged"},
		{"GitHub still requires a review, with the verdict", "REVIEW_REQUIRED", true, "merged"},
		{"GitHub still requires a review, no verdict", "REVIEW_REQUIRED", false, "approved"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := dependabotPR(t)
			f.pr()["labels"] = labels(labelReady, labelRisk)
			state := "CLEAN"
			if c.decision != "APPROVED" {
				state = "BLOCKED"
			}
			f.afterApprove = func(pr map[string]any) { pr["reviewDecision"], pr["mergeStateStatus"] = c.decision, state }
			var cr CrossReview
			if c.verdict {
				cr = CrossReview{Verdict: VerdictReady, Head: f.head(), Base: "main"}
			}
			cl := f.client()
			p, err := cl.Preview(context.Background(), f.ref, cr)
			if err != nil || !p.Eligible || !p.RiskHigh || !p.DependencyBot {
				t.Fatalf("preview %+v, %v", p, err)
			}
			f.reset()
			res := cl.Act(context.Background(), Intent{Preview: p, Mode: "single", CrossReview: cr, WaiveDiff: true}, "op-all", nil)
			if res.Kind != c.kind {
				t.Fatalf("%+v, mutations %v", res, f.mutations)
			}
			if c.kind == "approved" && (res.Step != "read 2" || !strings.Contains(res.Reason, "the rules need 2 approvals at the head; PocketGo counts 1")) {
				t.Fatalf("%+v", res)
			}
		})
	}
}
