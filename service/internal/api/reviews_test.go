package api

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/frostyard/pocketgo/proto"
	"github.com/frostyard/pocketgo/service/internal/store"
	"github.com/frostyard/pocketgo/service/internal/team"
)

var (
	headA = strings.Repeat("a", 40)
	headB = strings.Repeat("b", 40)
)

// fakeGitHub is GitHub as gh in the sandbox sees it, for one pull request:
// its head, its labels and CI at each head; it records the calls.
type fakeGitHub struct {
	mu     sync.Mutex
	repo   string
	number int64
	branch string
	head   string
	state  string
	labels map[string]bool
	ci     map[string]string // head → check runs answer
	calls  []string
	// The hooks run, under the lock, when gh reads the pull request, reads
	// CI, or adds the label; deleteErr refuses a removal.
	onRead, onCI, onAdd func(g *fakeGitHub)
	deleteErr           error
	// failReads fails the next reads of the pull request.
	failReads int
	// base is the merge base GitHub's compare names; reviewBase when "".
	base string
	// draft is the pull request's draft flag; baseRef its base branch,
	// main when "".
	draft   bool
	baseRef string
	// compareDeadline says the last compare read had a deadline.
	compareDeadline bool
	// failCompares fails the next compares.
	failCompares int
	// merged says a closed pull request merged.
	merged bool
}

func (g *fakeGitHub) pullJSON() string {
	var labels []map[string]string
	for name := range g.labels {
		labels = append(labels, map[string]string{"name": name})
	}
	if labels == nil {
		labels = []map[string]string{}
	}
	a := map[string]any{"id": 1000 + g.number, "number": g.number, "title": "Fix CI", "html_url": "https://github.com/" + g.repo + "/pull/" + itoa(g.number),
		"state": g.state, "draft": g.draft, "merged_at": mergedAt(g.merged), "labels": labels,
		"head": map[string]any{"ref": g.branch, "sha": g.head, "repo": map[string]any{"full_name": g.repo}},
		"base": map[string]any{"ref": cmp.Or(g.baseRef, "main"), "repo": map[string]any{"full_name": g.repo}}}
	out, _ := json.Marshal(a)
	return string(out)
}

// mergedAt is GitHub's merged_at for a pull request that merged or not.
func mergedAt(merged bool) any {
	if merged {
		return "2026-11-20T10:00:00Z"
	}
	return nil
}

func (g *fakeGitHub) gh(ctx context.Context, args []string) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	call := strings.Join(args, " ")
	g.calls = append(g.calls, call)
	if strings.Contains(call, "/compare/") {
		_, g.compareDeadline = ctx.Deadline()
	}
	issue := "repos/" + g.repo + "/issues/" + itoa(g.number) + "/labels"
	switch {
	case strings.Contains(call, "repos/"+g.repo+"/compare/"):
		if g.failCompares > 0 {
			g.failCompares--
			return "", errors.New("gh: HTTP 502")
		}
		base := g.base
		if base == "" {
			base = reviewBase
		}
		return `{"merge_base_commit":{"sha":"` + base + `"}}`, nil
	case strings.Contains(call, "--method POST "+issue):
		g.labels[team.ReadyLabel] = true
		if g.onAdd != nil {
			g.onAdd(g)
		}
		return `[{"name":"ready-for-human"}]`, nil
	case strings.Contains(call, "--method DELETE "+issue+"/"+team.ReadyLabel):
		if g.deleteErr != nil {
			return "", g.deleteErr
		}
		for name := range g.labels {
			if strings.EqualFold(name, team.ReadyLabel) {
				delete(g.labels, name)
				return "", nil
			}
		}
		return "", errors.New("gh: Label does not exist (HTTP 404)")
	case strings.HasSuffix(call, "repos/"+g.repo+"/pulls/"+itoa(g.number)):
		if g.failReads > 0 {
			g.failReads--
			return "", errors.New("gh: HTTP 502")
		}
		out := g.pullJSON()
		if g.onRead != nil {
			g.onRead(g)
		}
		return out, nil
	case strings.Contains(call, "/check-runs"):
		if g.onCI != nil {
			g.onCI(g)
		}
		for head, runs := range g.ci {
			if strings.Contains(call, "/commits/"+head+"/") {
				return runs, nil
			}
		}
		return `{"total_count":0,"check_runs":[]}`, nil
	case strings.Contains(call, "/status"):
		return `{"state":"pending","total_count":0,"statuses":[]}`, nil
	}
	return "", errors.New("gh: Not Found (HTTP 404)")
}

// labelWrites are the label calls gh got.
func (g *fakeGitHub) labelWrites() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []string
	for _, c := range g.calls {
		if strings.Contains(c, "--method") {
			out = append(out, c)
		}
	}
	return out
}

func (g *fakeGitHub) set(f func(g *fakeGitHub)) {
	g.mu.Lock()
	defer g.mu.Unlock()
	f(g)
}

const (
	failingRuns = `{"total_count":1,"check_runs":[{"name":"verify","status":"completed","conclusion":"failure"}]}`
	greenRuns   = `{"total_count":2,"check_runs":[{"name":"verify","status":"completed","conclusion":"success"},{"name":"pr-triage","status":"in_progress","conclusion":null}]}`
	pendingRuns = `{"total_count":1,"check_runs":[{"name":"verify","status":"in_progress","conclusion":null}]}`
)

// fakeReviewGit makes a review's folder as a plain folder, and names
// reviewBase where every head leaves the base: the 6a tests' heads are no
// real commits. The real git is worktree's, and TestAReviewEndToEnd's.
// fakeReviewGit is reviewGit with no git; notIsolated is what isolated
// says, and onAdd runs when a review's worktree is made.
type fakeReviewGit struct {
	notIsolated error
	onAdd       func()
}

const reviewBase = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"

func (g fakeReviewGit) add(_ context.Context, _, path, _, _ string, _ int64) error {
	if g.onAdd != nil {
		g.onAdd()
	}
	return os.MkdirAll(path, 0o755)
}
func (fakeReviewGit) usable(context.Context, string, string, string) error      { return nil }
func (fakeReviewGit) has(context.Context, string, string) (bool, error)         { return true, nil }
func (fakeReviewGit) fetchBranch(context.Context, string, string, string) error { return nil }
func (g fakeReviewGit) isolated(context.Context, string) error                  { return g.notIsolated }

// reviewHarness is a team whose specialists run models of two families:
// vera and duncan on Claude, teg on GPT. Vera's thread T1 links
// frostyard/example#3; pocketd reviews it in thread T2, with vera on the
// reviewer model of another family (ADR-0019), which h.teg names. Duncan's
// T3 is a thread the supervisor delegated, which files no verdict.
type reviewHarness struct {
	*teamHarness
	gh                *fakeGitHub
	vera, teg, duncan store.ThreadRow
}

func newReviewHarness(t *testing.T) *reviewHarness {
	t.Helper()
	h := &reviewHarness{teamHarness: newTeamHarness(t)}
	h.engine.more = []proto.Model{{ModelRef: proto.ModelRef{Provider: "anthropic", ID: "claude-opus-5-5"}},
		{ModelRef: proto.ModelRef{Provider: "openai-codex", ID: "gpt-6-astra"}},
		{ModelRef: proto.ModelRef{Provider: "openai-codex", ID: "gpt-6.1-sol"}}}
	h.server.reviewAsync, h.server.reviewGit, h.server.reviewsOn = false, fakeReviewGit{}, true
	h.writeProfiles(map[string]string{
		"odrade.md": "---\nrole: supervisor\nmodel: faux/faux-1\nhome: ~\n---\n# Odrade\n",
		"vera.md":   "---\nmodel: anthropic/claude-opus-5-5\nrepositories:\n  frostyard/example: " + h.checkout + "\n---\n# Vera\n",
		"teg.md":    "---\nmodel: openai/gpt-6-astra\nhome: ~\n---\n# Teg\n",
		"duncan.md": "---\nmodel: anthropic/claude-opus-5-5\nhome: ~\n---\n# Duncan\n",
	})
	ctx := context.Background()
	h.vera = veraThread(t, h.teamHarness)
	h.gh = &fakeGitHub{repo: "frostyard/example", number: 3, branch: h.vera.Branch, head: headA, state: "open", labels: map[string]bool{},
		ci: map[string]string{headA: greenRuns, headB: greenRuns}}
	h.engine.mu.Lock()
	h.engine.onGH = h.gh.gh
	h.engine.mu.Unlock()
	if _, err := h.server.TeamMessage(ctx, TeamCall{Session: h.vera.Session, Task: 1, CallID: "m1"},
		team.Report{Message: "PR #3 is open.", Status: team.StatusDone, PullRequests: []team.PullRef{{Number: 3}}}); err != nil {
		t.Fatal(err)
	}
	// The report started the review (ADR-0019).
	review, err := h.store.Thread(ctx, 2)
	if err != nil || review.ReviewPull == 0 || review.Agent != "vera" || review.Session == 0 {
		t.Fatalf("review thread T2: %+v %v", review, err)
	}
	h.teg = review
	if _, err := h.server.TeamDelegate(ctx, TeamCall{Session: h.super.ID, CallID: "review-duncan"},
		team.Delegation{Agent: "duncan", Task: "Review frostyard/example#3."}); err != nil {
		t.Fatal(err)
	}
	if h.duncan, err = h.store.Thread(ctx, 3); err != nil || h.duncan.Agent != "duncan" {
		t.Fatalf("thread T3: %+v %v", h.duncan, err)
	}
	return h
}

// reviewAt runs a review pass, as a round does, and is the review thread
// pocketd started at head (ADR-0019).
func (h *reviewHarness) reviewAt(t *testing.T, head string) store.ThreadRow {
	t.Helper()
	ctx := context.Background()
	h.server.reviewPass(ctx)
	threads, err := h.store.ReviewThreadsOf(ctx, h.pull(t).ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, th := range threads {
		if th.ReviewHead == head && th.Session != 0 {
			return th
		}
	}
	t.Fatalf("no review started at %s: %+v", head, threads)
	return store.ThreadRow{}
}

func (h *reviewHarness) verdict(session int64, id, head, verdict string, findings ...string) (string, error) {
	return h.server.TeamVerdict(context.Background(), TeamCall{Session: session, Task: 1, CallID: id},
		team.Verdict{PullRequest: team.PullRef{Repository: "frostyard/example", Number: 3}, Head: head, Verdict: verdict,
			Summary: "Read the diff and the tests.", Findings: findings})
}

func (h *reviewHarness) pull(t *testing.T) store.PullRow {
	t.Helper()
	p, found, err := h.store.PullByRef(context.Background(), "frostyard/example", 3)
	if err != nil || !found {
		t.Fatalf("pull %+v %v %v", p, found, err)
	}
	return p
}

// A ready verdict by a reviewer of another family, at the head GitHub
// has, with green CI, puts ready-for-human on the pull request, and the
// owner sees the verdict (ADR-0017). The call run again answers as it did.
func TestACrossReviewPutsReadyForHumanOn(t *testing.T) {
	h := newReviewHarness(t)
	ctx := context.Background()
	answer, err := h.verdict(h.teg.Session, "v1", headA, team.VerdictReady)
	if err != nil || !strings.Contains(answer, "Recorded R1: frostyard/example#3 at aaaaaaaaaaaa is ready") {
		t.Fatalf("verdict %q, %v", answer, err)
	}
	if got := h.gh.labelWrites(); len(got) != 1 || !strings.Contains(got[0], "--method POST repos/frostyard/example/issues/3/labels -f labels[]=ready-for-human") {
		t.Fatalf("label writes %q", got)
	}
	if p := h.pull(t); !p.Ready || p.Checks != "green" {
		t.Fatalf("pull %+v", p)
	}
	pulls, err := h.client.Pulls(ctx)
	if err != nil || len(pulls) != 1 || !pulls[0].Ready || pulls[0].Checks != proto.ChecksGreen || pulls[0].Review == nil {
		t.Fatalf("pulls %+v, %v", pulls, err)
	}
	r := pulls[0].Review
	// The author's own agent, on the reviewer model of another family
	// (ADR-0019).
	if r.Verdict != proto.ReviewReady || r.Agent != "vera" || r.Family != "openai" || r.Head != headA || r.Thread != h.teg.ID ||
		r.Model != (proto.ModelRef{Provider: "openai-codex", ID: "gpt-6.1-sol"}) {
		t.Fatalf("review %+v", r)
	}
	h.mu.Lock()
	var last *proto.PullRequest
	for _, e := range h.events {
		if e.Type == proto.EventPull {
			last = e.Pull
		}
	}
	h.mu.Unlock()
	if last == nil || !last.Ready || last.Review == nil || last.Review.Verdict != proto.ReviewReady {
		t.Fatalf("the last pull event %+v", last)
	}
	// Run again: the same answer, and nothing more written on GitHub (it
	// may read, to check the label is in step).
	writes := len(h.gh.labelWrites())
	again, err := h.verdict(h.teg.Session, "v1", headA, team.VerdictReady)
	if err != nil || again != answer || len(h.gh.labelWrites()) != writes {
		t.Fatalf("the call run again: %q, %v, label writes %q", again, err, h.gh.labelWrites())
	}
}

// A verdict is refused, and nothing recorded or labelled, from a thread
// pocketd did not start as a review (the author's own included), at a head
// other than its review's, on a pull request no thread links, from a
// session in no thread, or at a head GitHub has moved from (ADR-0017,
// ADR-0019).
func TestAVerdictMustCrossFamiliesAtTheCurrentHead(t *testing.T) {
	h := newReviewHarness(t)
	ctx := context.Background()
	for name, c := range map[string]struct {
		session int64
		head    string
		number  int64
		want    string
	}{
		"a thread the supervisor delegated": {h.duncan.Session, headA, 3, "is no cross-review"},
		"the author's thread":               {h.vera.Session, headA, 3, "is no cross-review"},
		"another head than its review's":    {h.teg.Session, headB, 3, "file the verdict on that pull request and head"},
		"no linked pull":                    {h.teg.Session, headA, 9, "linked to no thread"},
		"no thread":                         {h.super.ID, headA, 3, "thread's specialist"},
	} {
		_, err := h.server.TeamVerdict(ctx, TeamCall{Session: c.session, Task: 1, CallID: name},
			team.Verdict{PullRequest: team.PullRef{Repository: "frostyard/example", Number: c.number}, Head: c.head, Verdict: team.VerdictReady,
				Summary: "Fine."})
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", name, err, c.want)
		}
		if _, found, _ := h.store.ReviewCall(ctx, TeamCall{Session: c.session, Task: 1, CallID: name}.key()); found {
			t.Errorf("%s: a refused verdict was recorded", name)
		}
	}
	if got := h.gh.labelWrites(); len(got) != 0 {
		t.Fatalf("refused verdicts wrote %q", got)
	}
	// GitHub's head moved on since the review began: its verdict is of a
	// head that is gone.
	h.gh.set(func(g *fakeGitHub) { g.head = headB })
	if _, err := h.verdict(h.teg.Session, "moved", headA, team.VerdictReady); err == nil || !strings.Contains(err.Error(), "is at bbbbbbbbbbbb") {
		t.Fatalf("a moved head: %v", err)
	}
}

// The rounds keep the label in step: it waits for green CI, goes when the
// head moves, and does not come back without a ready verdict at the new
// head; a label someone else put on goes too.
func TestTheRoundsKeepReadyForHumanInStep(t *testing.T) {
	h := newReviewHarness(t)
	ctx := context.Background()
	h.gh.set(func(g *fakeGitHub) { g.ci[headA] = pendingRuns })
	if _, err := h.verdict(h.teg.Session, "v1", headA, team.VerdictReady); err != nil {
		t.Fatal(err)
	}
	if p := h.pull(t); p.Ready || p.Checks != "pending" || len(h.gh.labelWrites()) != 0 {
		t.Fatalf("with CI pending: %+v, writes %q", p, h.gh.labelWrites())
	}
	// CI turns green: the next round puts the label on.
	h.gh.set(func(g *fakeGitHub) { g.ci[headA] = greenRuns })
	if _, err := h.server.RefreshPulls(ctx); err != nil {
		t.Fatal(err)
	}
	if p := h.pull(t); !p.Ready || p.Checks != "green" {
		t.Fatalf("with CI green: %+v", p)
	}
	// A push: the label goes, and the old verdict brings nothing back.
	h.gh.set(func(g *fakeGitHub) { g.head = headB })
	if _, err := h.server.RefreshPulls(ctx); err != nil {
		t.Fatal(err)
	}
	if p := h.pull(t); p.Ready || p.HeadSHA != headB || h.gh.labels[team.ReadyLabel] {
		t.Fatalf("after a push: %+v, labels %v", p, h.gh.labels)
	}
	// A label put on by hand, with no ready verdict at the head, goes.
	h.gh.set(func(g *fakeGitHub) { g.labels[team.ReadyLabel] = true })
	if _, err := h.server.RefreshPulls(ctx); err != nil {
		t.Fatal(err)
	}
	if p := h.pull(t); p.Ready || h.gh.labels[team.ReadyLabel] {
		t.Fatalf("a label by hand: %+v, labels %v", p, h.gh.labels)
	}
	// A not-ready verdict at the new head, by the review pocketd started
	// there, keeps it off, and is what the owner sees.
	if _, err := h.verdict(h.reviewAt(t, headB).Session, "v2", headB, team.VerdictNotReady, "high blocking: The retry never ends."); err != nil {
		t.Fatal(err)
	}
	pulls, _ := h.client.Pulls(ctx)
	if len(pulls) != 1 || pulls[0].Ready || pulls[0].Review == nil || pulls[0].Review.Verdict != proto.ReviewNotReady ||
		pulls[0].Review.Findings[0] != "high blocking: The retry never ends." {
		t.Fatalf("pulls %+v", pulls)
	}
	writes := h.gh.labelWrites()
	if len(writes) != 3 || !strings.Contains(writes[0], "POST") || !strings.Contains(writes[1], "DELETE") || !strings.Contains(writes[2], "DELETE") {
		t.Fatalf("label writes %q, want add, remove after the push, remove the hand label", writes)
	}
}

// A merged pull request keeps its labels: pocketd keeps the label of open
// ones only.
func TestAMergedPullRequestKeepsItsLabels(t *testing.T) {
	h := newReviewHarness(t)
	ctx := context.Background()
	if _, err := h.verdict(h.teg.Session, "v1", headA, team.VerdictReady); err != nil {
		t.Fatal(err)
	}
	h.gh.set(func(g *fakeGitHub) { g.state = "closed" })
	if _, err := h.server.RefreshPulls(ctx); err != nil {
		t.Fatal(err)
	}
	if got := h.gh.labelWrites(); len(got) != 1 {
		t.Fatalf("label writes %q: a closed pull request lost its label", got)
	}
	// A review of a pull request closed meanwhile files nothing.
	other := newReviewHarness(t)
	other.gh.set(func(g *fakeGitHub) { g.state = "closed" })
	if _, err := other.verdict(other.teg.Session, "v2", headA, team.VerdictReady); err == nil || !strings.Contains(err.Error(), "only an open pull request") {
		t.Fatalf("a verdict on a closed pull request: %v", err)
	}
}

// labelled is the pull request after a ready verdict at headA with green CI.
func labelled(t *testing.T) *reviewHarness {
	t.Helper()
	h := newReviewHarness(t)
	if _, err := h.verdict(h.teg.Session, "v1", headA, team.VerdictReady); err != nil {
		t.Fatal(err)
	}
	if p := h.pull(t); !p.Ready || p.Checks != "green" {
		t.Fatalf("not labelled: %+v", p)
	}
	return h
}

// GitHub labels a number, not a commit: a push while CI is read puts no
// label on the new head (Codex review of #23, round 1).
func TestAPushDuringTheCIReadGetsNoLabel(t *testing.T) {
	h := newReviewHarness(t)
	h.gh.set(func(g *fakeGitHub) { g.onCI = func(g *fakeGitHub) { g.head, g.onCI = headB, nil } })
	if _, err := h.verdict(h.teg.Session, "v1", headA, team.VerdictReady); err != nil {
		t.Fatal(err)
	}
	if got := h.gh.labelWrites(); len(got) != 0 {
		t.Fatalf("label writes %q for a head nobody reviewed", got)
	}
	if p := h.pull(t); p.Ready || p.HeadSHA != headB {
		t.Fatalf("pull %+v", p)
	}
}

// A push between the label's write and the read after it takes the label
// off again.
func TestAPushUnderTheLabelTakesItOff(t *testing.T) {
	h := newReviewHarness(t)
	h.gh.set(func(g *fakeGitHub) { g.onAdd = func(g *fakeGitHub) { g.head, g.onAdd = headB, nil } })
	if _, err := h.verdict(h.teg.Session, "v1", headA, team.VerdictReady); err != nil {
		t.Fatal(err)
	}
	writes := h.gh.labelWrites()
	if len(writes) != 2 || !strings.Contains(writes[0], "POST") || !strings.Contains(writes[1], "DELETE") || h.gh.labels[team.ReadyLabel] {
		t.Fatalf("label writes %q, labels %v", writes, h.gh.labels)
	}
	if p := h.pull(t); p.Ready || p.HeadSHA != headB {
		t.Fatalf("pull %+v", p)
	}
}

// A reviewer's thread closed while its verdict was checked on GitHub files
// none, and labels nothing.
func TestAReviewerClosedDuringTheReadFilesNoVerdict(t *testing.T) {
	h := newReviewHarness(t)
	closed := false
	h.gh.set(func(g *fakeGitHub) {
		g.onRead = func(g *fakeGitHub) {
			if !closed {
				closed = true
				if _, err := h.store.CloseThread(context.Background(), h.teg.ID); err != nil {
					t.Error(err)
				}
			}
		}
	})
	if _, err := h.verdict(h.teg.Session, "v1", headA, team.VerdictReady); err == nil || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("a closed reviewer's verdict: %v", err)
	}
	if _, found, _ := h.store.ReviewCall(context.Background(), TeamCall{Session: h.teg.Session, Task: 1, CallID: "v1"}.key()); found {
		t.Fatal("it was recorded")
	}
	if got := h.gh.labelWrites(); len(got) != 0 {
		t.Fatalf("label writes %q", got)
	}
}

// An abort after GitHub answered, before the filing commits, files
// nothing.
func TestAnAbortBeforeTheVerdictCommitsFilesNothing(t *testing.T) {
	h := newReviewHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.gh.set(func(g *fakeGitHub) { g.onRead = func(*fakeGitHub) { cancel() } })
	call := TeamCall{Session: h.teg.Session, Task: 1, CallID: "v1"}
	_, err := h.server.TeamVerdict(ctx, call, team.Verdict{PullRequest: team.PullRef{Repository: "frostyard/example", Number: 3}, Head: headA,
		Verdict: team.VerdictReady, Summary: "Fine."})
	if err == nil {
		t.Fatal("an aborted verdict was taken")
	}
	if _, found, _ := h.store.ReviewCall(context.Background(), call.key()); found {
		t.Fatal("an aborted verdict was recorded")
	}
	if got := h.gh.labelWrites(); len(got) != 0 {
		t.Fatalf("label writes %q", got)
	}
}

// CI that cannot be read is not green: the label comes off, and the last
// CI state read stays.
func TestUnreadableCITakesTheLabelOff(t *testing.T) {
	h := labelled(t)
	h.gh.set(func(g *fakeGitHub) { g.ci[headA] = `{"total_count":1}` })
	if _, err := h.server.RefreshPulls(context.Background()); err == nil {
		t.Fatal("a round that could not read CI says it is done")
	}
	if p := h.pull(t); p.Ready || p.Checks != "green" || h.gh.labels[team.ReadyLabel] {
		t.Fatalf("pull %+v, labels %v", p, h.gh.labels)
	}
}

// CI read failing is recorded even when GitHub refuses the label's
// removal; the label stays as GitHub has it.
func TestFailingCIIsRecordedWhenTheRemovalFails(t *testing.T) {
	h := labelled(t)
	h.gh.set(func(g *fakeGitHub) { g.ci[headA], g.deleteErr = failingRuns, errors.New("gh: Forbidden (HTTP 403)") })
	if _, err := h.server.RefreshPulls(context.Background()); err == nil {
		t.Fatal("a refused removal says it is done")
	}
	if p := h.pull(t); !p.Ready || p.Checks != "failing" {
		t.Fatalf("pull %+v", p)
	}
}

// A later not-ready verdict at the same head takes the label off, and the
// last CI state read stays.
func TestANotReadyVerdictKeepsTheLastCIState(t *testing.T) {
	h := labelled(t)
	// A push: the review pocketd starts at the new head finds it not ready.
	h.gh.set(func(g *fakeGitHub) { g.head = headB })
	if _, err := h.server.RefreshPulls(context.Background()); err != nil {
		t.Fatal(err)
	}
	before := h.pull(t).Checks
	if _, err := h.verdict(h.reviewAt(t, headB).Session, "v2", headB, team.VerdictNotReady, "high blocking: A test is missing."); err != nil {
		t.Fatal(err)
	}
	if p := h.pull(t); p.Ready || p.Checks != before {
		t.Fatalf("pull %+v, checks before the verdict %q", p, before)
	}
}

// A review thread names its pull request by number alone: its repository
// is the thread's, the pull request's own (ADR-0019). A specialist the
// supervisor delegated in another repository files no verdict.
func TestAReviewThreadNamesItsPullByNumber(t *testing.T) {
	h := newReviewHarness(t)
	ctx := context.Background()
	if _, err := h.server.TeamVerdict(ctx, TeamCall{Session: h.teg.Session, Task: 1, CallID: "v1"},
		team.Verdict{PullRequest: team.PullRef{Number: 3}, Head: headA, Verdict: team.VerdictReady, Summary: "Fine."}); err != nil {
		t.Fatalf("a verdict by number: %v", err)
	}
	if p := h.pull(t); !p.Ready {
		t.Fatalf("pull %+v", p)
	}
	if _, err := h.verdict(h.duncan.Session, "v2", headA, team.VerdictReady); err == nil || !strings.Contains(err.Error(), "is no cross-review") {
		t.Fatalf("a delegated reviewer: %v", err)
	}
}

// A pull request merged, or closed, under the label's write keeps the
// label (Codex review of #23, round 2).
func TestAMergeUnderTheLabelKeepsIt(t *testing.T) {
	h := newReviewHarness(t)
	h.gh.set(func(g *fakeGitHub) { g.onAdd = func(g *fakeGitHub) { g.state, g.onAdd = "closed", nil } })
	if _, err := h.verdict(h.teg.Session, "v1", headA, team.VerdictReady); err != nil {
		t.Fatal(err)
	}
	if writes := h.gh.labelWrites(); len(writes) != 1 || !strings.Contains(writes[0], "POST") || !h.gh.labels[team.ReadyLabel] {
		t.Fatalf("label writes %q, labels %v", writes, h.gh.labels)
	}
}

// A label GitHub confirmed is recorded even when the read after it fails.
func TestAConfirmedLabelIsRecordedWhenTheReadAfterFails(t *testing.T) {
	h := newReviewHarness(t)
	h.gh.set(func(g *fakeGitHub) { g.onAdd = func(g *fakeGitHub) { g.failReads, g.onAdd = 1, nil } })
	if _, err := h.verdict(h.teg.Session, "v1", headA, team.VerdictReady); err != nil {
		t.Fatal(err)
	}
	if p := h.pull(t); !p.Ready {
		t.Fatalf("pull %+v: the label GitHub confirmed is not recorded", p)
	}
}

// GitHub's label names ignore case: a Ready-For-Human that no verdict
// backs is seen, and taken off.
func TestTheLabelIsReadInAnyCase(t *testing.T) {
	h := newReviewHarness(t)
	h.gh.set(func(g *fakeGitHub) { g.labels["Ready-For-Human"] = true })
	if _, err := h.server.RefreshPulls(context.Background()); err != nil {
		t.Fatal(err)
	}
	if writes := h.gh.labelWrites(); len(writes) != 1 || !strings.Contains(writes[0], "DELETE") || len(h.gh.labels) != 0 {
		t.Fatalf("label writes %q, labels %v", writes, h.gh.labels)
	}
}

// At start, the specialist session of each thread not closed is given
// team_verdict, once (Codex review of #23, round 2).
func TestOlderThreadSessionsGetTheVerdictTool(t *testing.T) {
	h := newReviewHarness(t)
	ctx := context.Background()
	if _, err := h.store.CloseThread(ctx, h.duncan.ID); err != nil {
		t.Fatal(err)
	}
	// The review's start gave its session the tool already (a recovered
	// one needs it before its brief), and no other thread's: only a review
	// pocketd started files a verdict (ADR-0019).
	h.engine.mu.Lock()
	if !h.engine.verdictTool[h.teg.Session] || h.engine.verdictTool[h.vera.Session] || h.engine.verdictTool[h.duncan.Session] {
		t.Errorf("the starts gave the tool to %v; want only the review's session %d", h.engine.verdictTool, h.teg.Session)
	}
	// A session made before ADR-0019 had it: the author's, on the desk.
	h.engine.verdictTool = map[int64]bool{h.vera.Session: true}
	h.engine.mu.Unlock()
	if err := h.server.UpgradeTeamTools(ctx); err != nil {
		t.Fatal(err)
	}
	h.engine.mu.Lock()
	defer h.engine.mu.Unlock()
	if !h.engine.verdictTool[h.teg.Session] {
		t.Errorf("the review's session %d was not given the tool", h.teg.Session)
	}
	if on, set := h.engine.verdictTool[h.vera.Session]; !set || on {
		t.Errorf("the author's session kept it: %v", h.engine.verdictTool)
	}
	if h.engine.verdictTool[h.duncan.Session] || h.engine.verdictTool[h.super.ID] {
		t.Errorf("a session that is no review's was given it: %v", h.engine.verdictTool)
	}
}

// A verdict whose call stopped before the label was in step finishes it,
// and closes its review thread, when the call runs again (Codex review of
// #23, round 3; ADR-0019).
func TestAReplayedVerdictFinishesTheLabel(t *testing.T) {
	h := newReviewHarness(t)
	ctx := context.Background()
	call := TeamCall{Session: h.teg.Session, Task: 1, CallID: "v1"}
	p := h.pull(t)
	// The ready verdict committed; the stop came before the label and the
	// review's close.
	if _, _, _, _, err := h.store.AddReview(ctx, p, store.ReviewRow{Key: call.key(), PullID: p.ID, ThreadID: h.teg.ID, SessionID: h.teg.Session,
		Agent: "vera", Provider: "openai-codex", Model: "gpt-6.1-sol", Family: "openai", HeadSHA: headA, Verdict: team.VerdictReady,
		Summary: "s"}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := h.verdict(h.teg.Session, "v1", headA, team.VerdictReady); err != nil {
		t.Fatal(err)
	}
	if p := h.pull(t); !p.Ready || !h.gh.labels[team.ReadyLabel] {
		t.Fatalf("the replay left no label: %+v, labels %v", p, h.gh.labels)
	}
	if th, err := h.store.Thread(ctx, h.teg.ID); err != nil || th.State != store.ThreadClosed {
		t.Fatalf("the replay left the review open: %+v, %v", th, err)
	}
	// Run again, in step: nothing more is written.
	writes := len(h.gh.labelWrites())
	if _, err := h.verdict(h.teg.Session, "v1", headA, team.VerdictReady); err != nil {
		t.Fatal(err)
	}
	if got := len(h.gh.labelWrites()); got != writes {
		t.Fatalf("a replay in step wrote %d more", got-writes)
	}
}

// A pull request merged or closed while CI was read keeps its label, also
// when that CI failed (Codex review of #23, round 3).
func TestAMergeDuringTheCIReadKeepsTheLabel(t *testing.T) {
	h := labelled(t)
	h.gh.set(func(g *fakeGitHub) {
		g.ci[headA] = failingRuns
		g.onCI = func(g *fakeGitHub) { g.state, g.onCI = "closed", nil }
	})
	if _, err := h.server.RefreshPulls(context.Background()); err != nil {
		t.Fatal(err)
	}
	if writes := h.gh.labelWrites(); len(writes) != 1 || !h.gh.labels[team.ReadyLabel] {
		t.Fatalf("label writes %q, labels %v: a closed pull request lost its label", writes, h.gh.labels)
	}
}

// A replay reads the pull request on GitHub before it reconciles: a push
// while pocketd was stopped, after the label went on, takes the label off
// (Codex review of #23, round 4).
func TestAReplayReadsTheHeadFirst(t *testing.T) {
	h := labelled(t)
	h.gh.set(func(g *fakeGitHub) { g.head = headB })
	if _, err := h.verdict(h.teg.Session, "v1", headA, team.VerdictReady); err != nil {
		t.Fatal(err)
	}
	if p := h.pull(t); p.Ready || p.HeadSHA != headB || h.gh.labels[team.ReadyLabel] {
		t.Fatalf("pull %+v, labels %v: the label stayed on a head nobody reviewed", p, h.gh.labels)
	}
}

// A label someone put on while CI was read, at the reviewed head, is in
// step: nothing is written.
func TestALabelPutOnMeanwhileIsNotWrittenAgain(t *testing.T) {
	h := newReviewHarness(t)
	h.gh.set(func(g *fakeGitHub) { g.onCI = func(g *fakeGitHub) { g.labels[team.ReadyLabel], g.onCI = true, nil } })
	if _, err := h.verdict(h.teg.Session, "v1", headA, team.VerdictReady); err != nil {
		t.Fatal(err)
	}
	if writes := h.gh.labelWrites(); len(writes) != 0 {
		t.Fatalf("label writes %q", writes)
	}
	if p := h.pull(t); !p.Ready {
		t.Fatalf("pull %+v", p)
	}
}

// A head that moved, carrying a label someone put on, while CI was read
// for the reviewed head is decided for itself: the label comes off
// (Codex review of #23, round 5).
func TestAMovedLabelledHeadFoundBeforeTheWriteLosesTheLabel(t *testing.T) {
	h := newReviewHarness(t)
	h.gh.set(func(g *fakeGitHub) {
		g.onCI = func(g *fakeGitHub) { g.head, g.labels[team.ReadyLabel], g.onCI = headB, true, nil }
	})
	if _, err := h.verdict(h.teg.Session, "v1", headA, team.VerdictReady); err != nil {
		t.Fatal(err)
	}
	if p := h.pull(t); p.Ready || p.HeadSHA != headB || h.gh.labels[team.ReadyLabel] {
		t.Fatalf("pull %+v, labels %v", p, h.gh.labels)
	}
}
