package github

import (
	"fmt"
	"strings"
)

// Stage is when the gates run: before the approval, or after it.
type Stage int

const (
	// AtPreview is gates 1 to 11: a card's button, and read 1 at the click.
	AtPreview Stage = iota
	// AfterApproval is gates 1 to 8, 10 and 11, the owner's approval at the
	// head, the review decision, and a merge state that merges.
	AfterApproval
)

const (
	labelReady = "ready-for-human"
	labelRisk  = "risk:high"
)

// VerdictReady is the verdict of a cross-review that finds a pull request
// ready (pocketgo ADR-0017).
const VerdictReady = "ready"

// CrossReview is pocketd's newest cross-review verdict on a pull request,
// as the desktop read it from pocketd: its verdict, the head it reviewed
// and the base branch it was filed on. pocketd shows only a verdict at the
// pull request's head, base branch and merge base. The zero value is none.
//
// The gates count it as 1 approval, next to GitHub's, only when it is
// ready at the head and base branch they read (pocketgo ADR-0031):
// pocketd's cross-review is the one agent review (pocketgo ADR-0019), and
// pocketd writes no review on GitHub (pocketgo ADR-0017).
type CrossReview struct {
	Verdict, Head, Base string
}

// counts says whether the gates count cr as an approval of f: ready, at
// the head and on the base branch this read of f shows.
func (cr CrossReview) counts(f PRFacts) bool {
	return cr.Verdict == VerdictReady && cr.Head != "" && cr.Head == f.HeadRefOid &&
		cr.Base != "" && cr.Base == f.BaseRefName
}

// queued says the base merges through a merge queue.
func queued(f PRFacts, r RepoRules) bool { return r.Queue || f.IsMergeQueueEnabled }

// repoGate is gates 1 and 2: the owner's token, then a bypass PocketGo does
// not handle, classic protection, and a rule of a bypassed ruleset that
// PocketGo does not check itself. The preflight of a repository runs it too.
func repoGate(f PRFacts, r RepoRules) string {
	if f.Viewer != Login {
		return fmt.Sprintf("this token is for %s, not %s", f.Viewer, Login)
	}
	switch r.Bypass {
	case "never", "always", "pull_requests_only":
		// Handled: no bypass, or a bypass PocketGo proves rule by rule.
	default:
		return fmt.Sprintf("you can bypass a rule here (%s): PocketGo refuses to merge", r.Bypass)
	}
	if r.Classic {
		return "classic branch protection: not supported"
	}
	if r.Bypass != "never" && r.Proof.Unchecked != "" {
		return "you can bypass rule " + r.Proof.Unchecked + " here, and PocketGo does not check it itself: PocketGo refuses to merge"
	}
	return ""
}

// bypassGate is gate 11: every rule of every ruleset the owner can bypass,
// proven by PocketGo's own reads (ADR-0011). It says nothing on a base with
// no bypass, and it proves the most refusals first. cr is pocketd's
// cross-review, which counts as 1 approval when it is ready at the head and
// base branch read (pocketgo ADR-0031).
func bypassGate(f PRFacts, r RepoRules, stage Stage, cr CrossReview) string {
	if r.Bypass == "never" {
		return ""
	}
	b := f.Bypass
	if b == nil || b.Head != f.HeadRefOid {
		return "PocketGo could not read what the rules you can bypass need"
	}
	// Another open PR at the same commit can block the merge on its reviews,
	// which PocketGo does not read: a pull_request rule needs it proven.
	if r.Proof.PullRequest && len(b.SameHead) > 0 {
		return fmt.Sprintf("open PR #%d has the same head, and PocketGo does not read its reviews", b.SameHead[0])
	}
	// The method the merge will use: the queue's on a queue base.
	m, _ := mergeMethod(f, r)
	if m == "queue" {
		m = r.queueMethod
	}
	if r.Proof.Linear && m == "merge" {
		return "the rules need a linear history: no merge commit"
	}
	if r.Proof.MethodsSet && !has(r.Proof.Methods, m) {
		return "the rules do not allow the " + m + " method"
	}
	if r.Proof.Threads && b.Unresolved > 0 {
		if b.Unresolved == 1 {
			return "1 review thread is not resolved"
		}
		return fmt.Sprintf("%d review threads are not resolved", b.Unresolved)
	}
	if r.Proof.Strict && !queued(f, r) && b.Behind != 0 {
		return "the PR is behind its base"
	}
	if r.Proof.LastPush {
		switch b.LastPusher {
		case "":
			return "PocketGo cannot prove who pushed last"
		case f.Viewer:
			return "you pushed last, so your approval does not count"
		}
	}
	// A review that asks for changes, seen by the read after the pull
	// request's, blocks: gate 8 read the reviews before it (pocketgo
	// ADR-0018, Codex review of #24, round 5).
	if b.ChangesRequested {
		return "a review requests changes"
	}
	if need, gh, verdict := approvalCount(f, r, stage, cr); need > 0 {
		have := gh
		if verdict {
			have++
		}
		if have < need {
			why := fmt.Sprintf("the rules need %d approvals at the head; PocketGo counts %d", need, have)
			switch {
			case verdict:
				return why + ", pocketd's ready cross-review among them"
			case r.reviewUnbypassed && cr.counts(f):
				return why + ": a ruleset you cannot bypass asks for reviews, so pocketd's cross-review does not count"
			}
			return why + ": no ready cross-review by pocketd at " + shortSHA(f.HeadRefOid)
		}
	}
	// After the approval, this later read must show it too: one dismissed
	// between the reads is no approval (Codex review of #24, round 6).
	if stage == AfterApproval && !has(b.Approvers, f.Viewer) {
		return "your approval is not at the head in the latest read"
	}
	return ""
}

// approvalCount is gate 11's count of approvals at the head: what the rules
// the owner can bypass need; GitHub's own, by others and by the owner (at
// the preview the owner's comes before the merge; after it, it counts only
// when GitHub shows it at the head); and whether pocketd's ready
// cross-review counts on top, once (pocketgo ADR-0031). It never counts
// where a ruleset the owner cannot bypass asks for reviews: GitHub enforces
// that rule at the merge, with GitHub's reviews alone.
func approvalCount(f PRFacts, r RepoRules, stage Stage, cr CrossReview) (need, gh int, verdict bool) {
	b := f.Bypass
	need = approvalsNeeded(r.Proof, b.AuthorType)
	for _, a := range b.Approvers {
		if a != f.Viewer {
			gh++
		}
	}
	if stage == AtPreview || has(b.Approvers, f.Viewer) {
		gh++
	}
	return need, gh, !r.reviewUnbypassed && cr.counts(f)
}

// verdictFills says GitHub's own approvals at the head fall short of what
// the rules the owner can bypass need, and pocketd's ready cross-review
// makes up the shortfall. GitHub then still says the review is required and
// the merge blocked after the owner's approval, and the owner's bypass
// merges it: every other rule of a ruleset the owner can bypass is proven
// by gate 11, and GitHub enforces each rule the owner cannot bypass at the
// merge itself (pocketgo ADR-0031, Codex review of #53, round 1).
func verdictFills(f PRFacts, r RepoRules, stage Stage, cr CrossReview) bool {
	if r.Bypass == "never" || f.Bypass == nil || f.Bypass.Head != f.HeadRefOid {
		return false
	}
	need, gh, verdict := approvalCount(f, r, stage, cr)
	return verdict && gh < need && gh+1 >= need
}

// approvalsNeeded is the most approvals any one pull_request rule needs:
// its count, and one more for an author that is not a User when it asks
// for the extra approval. A rule needing none adds none (pocketgo
// ADR-0018). Rules built without ApprovalRules count as one rule.
func approvalsNeeded(p BypassRules, authorType string) int {
	rules := p.ApprovalRules
	if len(rules) == 0 && p.Approvals > 0 {
		rules = []ApprovalRule{{Count: p.Approvals, Extra: p.ExtraUnattributed}}
	}
	need := 0
	for _, r := range rules {
		if r.Count == 0 {
			continue
		}
		n := r.Count
		if r.Extra && authorType != "User" {
			n++
		}
		need = max(need, n)
	}
	return need
}

// has says the list holds want.
func has(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// Check runs the gates of ADR-0008 on 1 read, and gate 11 of ADR-0011, and
// gives the first that fails with a short reason. cr is pocketd's
// cross-review, which gate 11 counts as an approval (pocketgo ADR-0031).
func Check(f PRFacts, r RepoRules, stage Stage, cr CrossReview) (ok bool, reason string) {
	for _, gate := range []func() string{
		// 1. The owner's token, and 2. the bypass, the classic protection,
		// and a bypassed rule PocketGo does not check itself.
		func() string { return repoGate(f, r) },
		// 3. Open, not a draft, not the viewer's, its head is its last commit.
		func() string {
			switch {
			case f.State != "OPEN":
				return "the PR is " + strings.ToLower(f.State)
			case f.IsDraft:
				return "the PR is a draft"
			case f.ViewerDidAuthor:
				return "you authored this PR"
			case f.CommitOid != f.HeadRefOid:
				return "the last commit is not the head"
			case f.IsInMergeQueue:
				// Check Again never offers a second enqueue (pocketgo
				// ADR-0018, Codex review of #24, round 8).
				return "the PR is already in the merge queue"
			}
			return ""
		},
		// 4. Ready for the owner.
		func() string {
			if !f.hasLabel(labelReady) {
				return "no " + labelReady + " label"
			}
			return ""
		},
		// 5. Mergeable.
		func() string {
			switch f.Mergeable {
			case "MERGEABLE":
				return ""
			case "UNKNOWN":
				return "GitHub has not computed mergeability"
			case "CONFLICTING":
				return "the PR has conflicts"
			}
			return "mergeable is " + f.Mergeable
		},
		// 6. Every required check is on the head.
		func() string {
			for _, rc := range r.Required {
				if !present(rc, f.Checks) {
					return "required check " + rc.Context + " has not run"
				}
			}
			return ""
		},
		// 7. Every check and status is complete and green.
		func() string {
			for _, c := range f.Checks {
				if c.Type == "StatusContext" {
					switch c.State {
					case "SUCCESS":
					case "PENDING", "EXPECTED":
						return "status " + c.Name + " has not finished"
					default:
						return "status " + c.Name + ": " + strings.ToLower(c.State)
					}
					continue
				}
				if c.Status != "COMPLETED" {
					return "check " + c.Name + " has not finished"
				}
				switch c.Conclusion {
				case "SUCCESS", "NEUTRAL", "SKIPPED":
				default:
					return "check " + c.Name + ": " + strings.ToLower(c.Conclusion)
				}
			}
			return ""
		},
		// 8. No blocking review.
		func() string {
			for _, rv := range f.Reviews {
				if rv.State == "CHANGES_REQUESTED" {
					return rv.Author + " requested changes"
				}
			}
			if f.ReviewDecision == "CHANGES_REQUESTED" {
				return "changes are requested"
			}
			return ""
		},
		// 9. The merge state, before the approval.
		func() string {
			if stage != AtPreview {
				return ""
			}
			switch f.MergeStateStatus {
			case "BLOCKED", "CLEAN", "HAS_HOOKS":
				return ""
			case "BEHIND":
				if queued(f, r) {
					return ""
				}
			}
			return mergeStateReason(f.MergeStateStatus)
		},
		// 10. The merge method.
		func() string {
			_, why := mergeMethod(f, r)
			return why
		},
		// 11. Every rule of every ruleset the owner can bypass, proven by
		// PocketGo's own reads.
		func() string { return bypassGate(f, r, stage, cr) },
		// After the approval: the owner's approval at the head, the review
		// decision, and a merge state that merges.
		func() string {
			if stage != AfterApproval {
				return ""
			}
			v := f.ViewerLatestReview
			if v == nil || v.State != "APPROVED" || v.Commit != f.HeadRefOid {
				return "your approval at " + shortSHA(f.HeadRefOid) + " is not there"
			}
			// GitHub counts its own reviews only: where pocketd's
			// cross-review makes up the approvals, GitHub still says the
			// review is required and blocks the merge, which the owner's
			// bypass merges (pocketgo ADR-0031, Codex review of #53, round 1).
			short := verdictFills(f, r, AfterApproval, cr)
			if f.ReviewDecision != "" && f.ReviewDecision != "APPROVED" && (!short || f.ReviewDecision != "REVIEW_REQUIRED") {
				return "the review decision is " + strings.ToLower(strings.ReplaceAll(f.ReviewDecision, "_", " "))
			}
			// An explicit allow list: a state PocketGo does not know refuses.
			if queued(f, r) {
				switch f.MergeStateStatus {
				case "CLEAN", "HAS_HOOKS", "BLOCKED", "BEHIND":
					return ""
				}
				return mergeStateReason(f.MergeStateStatus)
			}
			switch f.MergeStateStatus {
			case "CLEAN", "HAS_HOOKS":
				return ""
			case "BLOCKED":
				// Only a block GitHub puts down to the missing review: one
				// it blocks while it calls the review approved is another
				// (Codex review of #53, round 2).
				if short && f.ReviewDecision == "REVIEW_REQUIRED" {
					return ""
				}
			}
			return mergeStateReason(f.MergeStateStatus)
		},
	} {
		if why := gate(); why != "" {
			return false, why
		}
	}
	return true, ""
}

func mergeStateReason(s string) string {
	switch s {
	case "BEHIND":
		return "the PR is behind its base"
	case "DIRTY":
		return "the PR has conflicts"
	case "UNSTABLE":
		return "a check is failing"
	case "DRAFT":
		return "the PR is a draft"
	case "UNKNOWN":
		return "GitHub has not computed the merge state"
	case "BLOCKED":
		return "GitHub still blocks the merge"
	}
	return "the merge state is " + s
}

// present says a required check is on the head. A check with an
// integration ID is met only by a CheckRun of that app; a status shows no
// app, so it never meets one.
func present(rc RequiredCheck, checks []CheckContext) bool {
	for _, c := range checks {
		if c.Name != rc.Context {
			continue
		}
		if rc.IntegrationID == 0 {
			return true
		}
		if c.Type == "CheckRun" && c.App && c.AppID == rc.IntegrationID {
			return true
		}
	}
	return false
}

// mergeMethod is the method PocketGo uses: the queue on a merge-queue base,
// else the owner's default in the repository, when it is allowed there.
func mergeMethod(f PRFacts, r RepoRules) (method, reason string) {
	if queued(f, r) {
		if r.queueMethod == "" {
			return "", "the merge queue's rule names no method PocketGo can read"
		}
		return "queue", ""
	}
	var allowed bool
	switch f.DefaultMergeMethod {
	case "MERGE":
		method, allowed = "merge", f.MergeCommitAllowed
	case "SQUASH":
		method, allowed = "squash", f.SquashMergeAllowed
	case "REBASE":
		method, allowed = "rebase", f.RebaseMergeAllowed
	default:
		return "", "unknown merge method " + f.DefaultMergeMethod
	}
	if !allowed {
		return "", "your default merge method (" + method + ") is not allowed here"
	}
	return method, ""
}
