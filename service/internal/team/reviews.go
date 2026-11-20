package team

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
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

// VerdictRepository is the repository of the pull request a verdict names:
// the one it names, else its thread's (repository, "" for a thread in the
// specialist's home). A reviewer may review a pull request of another
// repository than its own, unlike an author (PullRepository).
func VerdictRepository(ref PullRef, repository string) (string, error) {
	switch {
	case ref.Number <= 0:
		return "", fmt.Errorf("%d is not a pull request number", ref.Number)
	case ref.Repository != "":
		return ref.Repository, nil
	case repository == "":
		return "", fmt.Errorf("pull request #%d needs its repository: your thread works in no repository", ref.Number)
	}
	return repository, nil
}

// MaxReviews is how many cross-reviews pocketd starts for one pull
// request: 1, then at most 3 re-reviews after fixes, the owner's budget
// (ADR-0019). Then the owner decides.
const MaxReviews = 4

// ReviewKey is the key of the cross-review of a pull request at head: one
// review a head, whoever asks how often (ADR-0019).
//
// GitHub compares names in any case, and pocketd keeps GitHub's latest
// spelling: the key does not depend on it (Codex review of #26, round 2).
//
// The base branch is part of it: a pull request retargeted at the same
// head has another change set (Codex review of #26, round 3).
func ReviewKey(repository string, number int64, base, head string) string {
	return fmt.Sprintf("review:%s#%d:%s:%s", strings.ToLower(repository), number, base, head)
}

// ReviewTitle is the title of the n-th review thread of a pull request.
func ReviewTitle(repository string, number int64, head string, n int) string {
	return fmt.Sprintf("Cross-review %d of %d: %s#%d at %s", n, MaxReviews, repository, number, short(head))
}

// PriorReview is the last review of a pull request, which a re-review
// starts from: the head it read and its verdict.
type PriorReview struct {
	Head     string
	Verdict  string
	Summary  string
	Findings []string
}

// ReviewSpec is what a review thread's brief names (ADR-0019).
type ReviewSpec struct {
	// Thread is the review thread; Author the author's agent's name.
	Thread int64
	Author string
	// Repository and Number name the pull request; Base is the commit its
	// change starts from (the merge base of its head and its base branch)
	// and BaseBranch that branch; Head is the commit reviewed.
	Repository string
	Number     int64
	BaseBranch string
	Base       string
	Head       string
	// Cwd is the worktree detached at Head.
	Cwd string
	// Prior is the last review, nil for the first.
	Prior *PriorReview
}

// ReviewBrief is the brief of a review thread: the change set, read-only,
// and how to file the verdict (ADR-0019). It is the reviewer's whole task.
func ReviewBrief(r ReviewSpec) string {
	ref := fmt.Sprintf("%s#%d", r.Repository, r.Number)
	var b strings.Builder
	fmt.Fprintf(&b, "pocketd started this cross-review in thread T%d: you review %s, which %s wrote, as a reviewer of another model family.\n\n", r.Thread, ref, r.Author)
	fmt.Fprintf(&b, "The change set is the pull request's: its base is %s (where it leaves the branch %s) and its head is %s. Your folder %s is a git worktree detached at the head.\n\n", r.Base, r.BaseBranch, r.Head, r.Cwd)
	// read is the profile's: one without it reviews with team_diff alone
	// (Codex review of #26, round 7).
	b.WriteString("The review is read-only: you have team_diff and team_verdict, read if your profile has it, and nothing that writes.\n\n")
	fmt.Fprintf(&b, "team_diff shows the change set, git diff %s %s: its stat with stat, its commits with log, and only the paths you name with paths. read, when you have it, reads any file in the folder.\n\n", r.Base, r.Head)
	if p := r.Prior; p != nil {
		fmt.Fprintf(&b, "This is a re-review. The last review read %s and found it %s: %s\n", p.Head, p.Verdict, strings.TrimSpace(p.Summary))
		for _, f := range p.Findings {
			fmt.Fprintf(&b, "  - %s\n", strings.TrimSpace(f))
		}
		fmt.Fprintf(&b, "Review the change since, git diff %s %s, with team_diff and since_last_review, and check each earlier finding.\n\n", p.Head, r.Head)
	}
	fmt.Fprintf(&b, "Then file exactly one team_verdict for %s at head %s: ready, not-ready or unclear, with a summary and one finding per point. Write each finding as \"<severity> <blocking or non-blocking>: <the point>\", its severity high, medium or low, as \"high blocking: the retry never ends\". A not-ready verdict names at least one blocking finding, a ready one none; unclear says what you could not check.\n\n", ref, r.Head)
	b.WriteString("pocketd closes this thread once it records your verdict, and tells the author.")
	return b.String()
}

// ReviewThreadVerdictText answers a team_verdict call in a review thread
// pocketd started (ADR-0019): its reviewer has no team_message, and pocketd
// closes the thread and tells the author.
func ReviewThreadVerdictText(id int64, ref, head, verdict string) string {
	return fmt.Sprintf("Recorded R%d: %s at %s is %s. pocketd closes this review thread and tells the author; your review is done.", id, ref, short(head), verdict)
}

// AuthorVerdictText is the message that tells the author's thread a verdict
// on its pull request, once (ADR-0019).
func AuthorVerdictText(thread int64, ref, head, verdict, summary string, findings []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[T%d · cross-review · %s] %s at %s: %s", thread, verdict, ref, short(head), strings.TrimSpace(summary))
	for _, f := range findings {
		fmt.Fprintf(&b, "\n- %s", strings.TrimSpace(f))
	}
	if verdict != VerdictReady {
		b.WriteString("\nFix what blocks, push, and report the new head with team_message: pocketd reviews it again while the budget lasts.")
	}
	return b.String()
}

// findingForm is a cross-review finding as the brief asks for it:
// "<severity> <blocking or non-blocking>: <the point>" (ADR-0019).
var findingForm = regexp.MustCompile(`^(high|medium|low) (blocking|non-blocking): \S`)

// CheckReviewFindings refuses a review thread's verdict whose findings do
// not say their severity and whether they block, or contradict its
// verdict: a not-ready verdict names a blocking finding, a ready one none
// (ADR-0019). The author acts on what blocks.
func CheckReviewFindings(v Verdict) error {
	blocking := 0
	for _, f := range v.Findings {
		m := findingForm.FindStringSubmatch(strings.TrimSpace(f))
		if m == nil {
			return fmt.Errorf("finding %q does not begin with its severity and whether it blocks, as \"high blocking: <the point>\"", Clip(f, 60))
		}
		if m[2] == "blocking" {
			blocking++
		}
	}
	switch {
	case v.Verdict == VerdictNotReady && blocking == 0:
		return errors.New("a not-ready verdict names at least one blocking finding")
	case v.Verdict == VerdictReady && blocking > 0:
		return errors.New("a ready verdict names no blocking finding: file not-ready")
	}
	return nil
}

// DiffRequest is a team_diff call: what of its review's change set a
// reviewer reads (ADR-0019).
type DiffRequest struct {
	// Paths narrow it to these files or folders; none is the whole set.
	Paths []string `json:"paths,omitempty"`
	// Stat gives the stat alone; Log the commits with their patches.
	Stat bool `json:"stat,omitempty"`
	Log  bool `json:"log,omitempty"`
	// SinceLastReview reads from the head the last review read, for a
	// re-review, instead of the base.
	SinceLastReview bool `json:"since_last_review,omitempty"`
	// Offset is the byte the answer starts at: a change set longer than
	// one answer is read page by page (Codex review of #26, round 2).
	Offset int `json:"offset,omitempty"`
}

// DiffPage is how many bytes of a change set one team_diff answer holds.
const DiffPage = 48 << 10

// MaxDiffPaths is how many paths a team_diff call names at most.
const MaxDiffPaths = 20

// CheckDiff refuses a team_diff call that breaks a rule: a path that is
// absolute, leaves the folder, looks like an option or is empty, too many
// paths, or both stat and log.
func CheckDiff(d DiffRequest) error {
	if len(d.Paths) > MaxDiffPaths {
		return fmt.Errorf("%d paths: name at most %d", len(d.Paths), MaxDiffPaths)
	}
	if d.Stat && d.Log {
		return errors.New("ask for stat or for log, not both")
	}
	if d.Offset < 0 {
		return fmt.Errorf("offset %d is negative", d.Offset)
	}
	for _, p := range d.Paths {
		switch {
		case p == "" || len(p) > 400 || strings.ContainsRune(p, 0):
			return fmt.Errorf("path %q is empty, too long or holds a NUL", Clip(p, 60))
		case strings.HasPrefix(p, "/") || strings.HasPrefix(p, "-") || strings.HasPrefix(p, ":"):
			return fmt.Errorf("path %q is absolute, an option or a pathspec magic: name a path in the folder", Clip(p, 60))
		}
		for _, part := range strings.Split(p, "/") {
			if part == ".." {
				return fmt.Errorf("path %q leaves the folder", Clip(p, 60))
			}
		}
	}
	return nil
}

// ReviewGitOptions are git's options for every command on a review's
// worktree and its checkout, team_diff's included (ADR-0019; Codex review
// of #26, rounds 2, 6, 7 and 8): replacement objects are ignored, so the
// commit read is GitHub's own, and no hook, fsmonitor, signature program,
// pager or external diff of the checkout runs.
//
// Attributes are read from the empty tree, and from no file the
// configuration names, so no filter (smudge, clean, process), line-ending
// conversion or binary marking the checkout selects runs or changes what
// git shows, and autocrlf is off: the worktree holds the commit's blobs as
// GitHub has them, and a diff shows them. worktree.attributesIsolated
// checks what no option turns off. The size above which git treats a
// file as binary is git's default, so a checkout that lowers it hides no
// change behind "Binary files differ" (Codex review of #26, round 12).
var ReviewGitOptions = []string{"--no-replace-objects", "-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false",
	"-c", "log.showSignature=false", "-c", "core.pager=cat", "-c", "diff.external=",
	"-c", "attr.tree=" + EmptyTree, "-c", "core.attributesFile=/dev/null", "-c", "core.autocrlf=false",
	"-c", "core.bigFileThreshold=" + BigFileThreshold}

// BigFileThreshold is git's default core.bigFileThreshold, which a
// review's git pins.
const BigFileThreshold = "512m"

// EmptyTree is git's empty tree, which holds no .gitattributes.
const EmptyTree = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"

// DiffArgs are the git arguments of a checked team_diff call between from
// and to: ReviewGitOptions, no external diff, no text conversion, no
// pager, so nothing the checkout configures runs (ADR-0019).
func DiffArgs(from, to string, d DiffRequest) []string {
	// A log shows no signature, so no verification program of the
	// checkout runs (Codex review of #26, round 2). Paths are literal: a
	// name holding [ or * names itself alone (round 6).
	args := append(slices.Clone(ReviewGitOptions), "--literal-pathspecs", "--no-pager")
	if d.Log {
		// A merge commit shows what it changed against all its parents
		// (dense combined), so a conflict's resolution is read (Codex
		// review of #26, round 10).
		args = append(args, "log", "--no-show-signature", "--format=commit %H%nAuthor: %an%nDate:   %ad%n%n%w(0,4,4)%B",
			"--no-ext-diff", "--no-textconv", "--no-color", "--ignore-submodules=none", "-p", "--diff-merges=dense-combined", from+".."+to)
	} else {
		// A submodule's new pointer shows, whatever the checkout ignores
		// (Codex review of #26, round 3).
		args = append(args, "diff", "--no-ext-diff", "--no-textconv", "--no-color", "--ignore-submodules=none")
		if d.Stat {
			args = append(args, "--stat")
		}
		args = append(args, from, to)
	}
	if len(d.Paths) > 0 {
		args = append(args, "--")
		args = append(args, d.Paths...)
	}
	return args
}
