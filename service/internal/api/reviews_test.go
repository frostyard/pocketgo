package api

import (
	"context"
	"encoding/json"
	"errors"
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
		"state": g.state, "draft": false, "merged_at": nil, "labels": labels,
		"head": map[string]any{"ref": g.branch, "sha": g.head, "repo": map[string]any{"full_name": g.repo}},
		"base": map[string]any{"ref": "main", "repo": map[string]any{"full_name": g.repo}}}
	out, _ := json.Marshal(a)
	return string(out)
}

func (g *fakeGitHub) gh(_ context.Context, args []string) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	call := strings.Join(args, " ")
	g.calls = append(g.calls, call)
	issue := "repos/" + g.repo + "/issues/" + itoa(g.number) + "/labels"
	switch {
	case strings.Contains(call, "--method POST "+issue):
		g.labels[team.ReadyLabel] = true
		return `[{"name":"ready-for-human"}]`, nil
	case strings.Contains(call, "--method DELETE "+issue+"/"+team.ReadyLabel):
		if !g.labels[team.ReadyLabel] {
			return "", errors.New("gh: Label does not exist (HTTP 404)")
		}
		delete(g.labels, team.ReadyLabel)
		return "", nil
	case strings.HasSuffix(call, "repos/"+g.repo+"/pulls/"+itoa(g.number)):
		return g.pullJSON(), nil
	case strings.Contains(call, "/check-runs"):
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
	greenRuns   = `{"total_count":2,"check_runs":[{"name":"verify","status":"completed","conclusion":"success"},{"name":"pr-triage","status":"in_progress","conclusion":null}]}`
	pendingRuns = `{"total_count":1,"check_runs":[{"name":"verify","status":"in_progress","conclusion":null}]}`
)

// reviewHarness is a team whose specialists run models of two families:
// vera and duncan on Claude, teg on GPT. Vera's thread T1 links
// frostyard/example#3, and teg reviews it in thread T2, duncan in T3.
type reviewHarness struct {
	*teamHarness
	gh                *fakeGitHub
	vera, teg, duncan store.ThreadRow
}

func newReviewHarness(t *testing.T) *reviewHarness {
	t.Helper()
	h := &reviewHarness{teamHarness: newTeamHarness(t)}
	h.engine.more = []proto.Model{{ModelRef: proto.ModelRef{Provider: "anthropic", ID: "claude-opus-5-5"}},
		{ModelRef: proto.ModelRef{Provider: "openai-codex", ID: "gpt-6-astra"}}}
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
	for i, agent := range []string{"teg", "duncan"} {
		if _, err := h.server.TeamDelegate(ctx, TeamCall{Session: h.super.ID, CallID: "review-" + agent},
			team.Delegation{Agent: agent, Task: "Review frostyard/example#3."}); err != nil {
			t.Fatal(err)
		}
		thread, err := h.store.Thread(ctx, int64(i+2))
		if err != nil || thread.Agent != agent {
			t.Fatalf("thread T%d: %+v %v", i+2, thread, err)
		}
		if agent == "teg" {
			h.teg = thread
		} else {
			h.duncan = thread
		}
	}
	return h
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
	if r.Verdict != proto.ReviewReady || r.Agent != "teg" || r.Family != "openai" || r.Head != headA || r.Thread != h.teg.ID ||
		r.Model != (proto.ModelRef{Provider: "openai-codex", ID: "gpt-6-astra"}) {
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
	// Run again: the same answer, and nothing more on GitHub.
	h.gh.mu.Lock()
	calls := len(h.gh.calls)
	h.gh.mu.Unlock()
	again, err := h.verdict(h.teg.Session, "v1", headA, team.VerdictReady)
	h.gh.mu.Lock()
	defer h.gh.mu.Unlock()
	if err != nil || again != answer || len(h.gh.calls) != calls {
		t.Fatalf("the call run again: %q, %v, %d gh calls (was %d)", again, err, len(h.gh.calls), calls)
	}
}

// A verdict is refused, and nothing recorded or labelled, from the
// author's own thread, from the author's family, at a head that is not
// GitHub's, on a pull request no thread links, or from a session in no
// thread.
func TestAVerdictMustCrossFamiliesAtTheCurrentHead(t *testing.T) {
	h := newReviewHarness(t)
	ctx := context.Background()
	for name, c := range map[string]struct {
		session int64
		head    string
		number  int64
		want    string
	}{
		"the author's family": {h.duncan.Session, headA, 3, "another model family"},
		"the author's thread": {h.vera.Session, headA, 3, "does not review its own work"},
		"an old head":         {h.teg.Session, headB, 3, "head is aaaaaaaaaaaa now"},
		"no linked pull":      {h.teg.Session, headA, 9, "linked to no thread"},
		"no thread":           {h.super.ID, headA, 3, "thread's specialist"},
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
	// A not-ready verdict at the new head keeps it off, and is what the
	// owner sees.
	if _, err := h.verdict(h.teg.Session, "v2", headB, team.VerdictNotReady, "The retry never ends."); err != nil {
		t.Fatal(err)
	}
	pulls, _ := h.client.Pulls(ctx)
	if len(pulls) != 1 || pulls[0].Ready || pulls[0].Review == nil || pulls[0].Review.Verdict != proto.ReviewNotReady ||
		pulls[0].Review.Findings[0] != "The retry never ends." {
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
	if _, err := h.verdict(h.teg.Session, "v2", headA, team.VerdictReady); err == nil || !strings.Contains(err.Error(), "only an open pull request") {
		t.Fatalf("a verdict on a closed pull request: %v", err)
	}
	if _, err := h.server.RefreshPulls(ctx); err != nil {
		t.Fatal(err)
	}
	if got := h.gh.labelWrites(); len(got) != 1 {
		t.Fatalf("label writes %q: a closed pull request lost its label", got)
	}
}
