package team

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

// Cross-review verdicts (ADR-0017).
const (
	VerdictReady    = "ready"
	VerdictNotReady = "not-ready"
	VerdictUnclear  = "unclear"
)

// ReadyLabel is the label that tells the owner a pull request is ready
// for them: pocketd keeps it in step with the verdicts.
const ReadyLabel = "ready-for-human"

// Limits on a verdict.
const (
	MaxSummary  = 2000
	MaxFindings = 20
	MaxFinding  = 500
)

// Verdict is a team_verdict call: a cross-review of a linked pull request
// at the head the reviewer read.
type Verdict struct {
	PullRequest PullRef  `json:"pull_request"`
	Head        string   `json:"head"`
	Verdict     string   `json:"verdict"`
	Summary     string   `json:"summary"`
	Findings    []string `json:"findings,omitempty"`
}

var commit = regexp.MustCompile(`^[0-9a-f]{40}$`)

// CheckVerdict refuses a verdict that breaks a rule: a verdict it does not
// know, a head that is not a full commit id, a summary or findings out of
// their limits, or a not-ready verdict with no finding.
func CheckVerdict(v Verdict) error {
	switch v.Verdict {
	case VerdictReady, VerdictNotReady, VerdictUnclear:
	default:
		return fmt.Errorf("the verdict is ready, not-ready or unclear, not %q", v.Verdict)
	}
	if !commit.MatchString(v.Head) {
		return fmt.Errorf("head %q is not a commit: give the full 40-character id of the head you reviewed", v.Head)
	}
	if strings.TrimSpace(v.Summary) == "" {
		return errors.New("the summary is empty")
	}
	if n := utf8.RuneCountInString(v.Summary); n > MaxSummary {
		return fmt.Errorf("the summary has %d characters, more than %d", n, MaxSummary)
	}
	if len(v.Findings) > MaxFindings {
		return fmt.Errorf("%d findings, more than %d: give the most important", len(v.Findings), MaxFindings)
	}
	for i, f := range v.Findings {
		if strings.TrimSpace(f) == "" {
			return fmt.Errorf("finding %d is empty", i+1)
		}
		if n := utf8.RuneCountInString(f); n > MaxFinding {
			return fmt.Errorf("finding %d has %d characters, more than %d", i+1, n, MaxFinding)
		}
	}
	if v.Verdict == VerdictNotReady && len(v.Findings) == 0 {
		return errors.New("a not-ready verdict names at least one finding: what blocks the pull request")
	}
	return nil
}

// CheckReviewer refuses a reviewer who may not review the pull request
// thread author links: from that same thread, or of the author's family,
// or of a family code cannot tell (router.Family).
func CheckReviewer(author, reviewer int64, authorFamily, reviewerFamily string) error {
	switch {
	case author == reviewer:
		return fmt.Errorf("thread T%d links this pull request: a thread does not review its own work", author)
	case authorFamily == "":
		return fmt.Errorf("pocketd cannot tell the model family of T%d's specialist, so no cross-review of its work counts", author)
	case reviewerFamily == "":
		return errors.New("pocketd cannot tell your model's family, so your review cannot count as a cross-review")
	case authorFamily == reviewerFamily:
		return fmt.Errorf("a cross-review needs a reviewer of another model family: the author's model and yours are both %s", authorFamily)
	}
	return nil
}

// CheckHead refuses a verdict on a head that is not the pull request's
// head on GitHub now: a push after the review voids it.
func CheckHead(ref, reviewed, current string) error {
	if reviewed != current {
		return fmt.Errorf("%s's head is %s now, not %s: review the new head, and file the verdict for it", ref, short(current), short(reviewed))
	}
	return nil
}

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// VerdictText answers a team_verdict call that recorded review id.
func VerdictText(id int64, ref, head, verdict string) string {
	at := ref + " at " + short(head)
	switch verdict {
	case VerdictReady:
		return fmt.Sprintf("Recorded R%d: %s is ready. pocketd adds %s once CI at that head is green, and removes it if the head moves. "+
			"Report your review to your supervisor with team_message.", id, at, ReadyLabel)
	case VerdictNotReady:
		return fmt.Sprintf("Recorded R%d: %s is not ready. Report your findings to your supervisor with team_message: the author fixes them, "+
			"and a new head needs a new review.", id, at)
	default:
		return fmt.Sprintf("Recorded R%d: your review of %s is unclear. Report what is unclear to your supervisor with team_message.", id, at)
	}
}

// WantsReady says whether a linked pull request should carry ReadyLabel:
// open and no draft, with the newest verdict at its head ready and CI at
// that head green (ADR-0017).
func WantsReady(open, draft bool, verdict string, ciGreen bool) bool {
	return open && !draft && verdict == VerdictReady && ciGreen
}
