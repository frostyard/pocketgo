package api

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/frostyard/pocketgo/proto"
	"github.com/frostyard/pocketgo/service/internal/store"
	"github.com/frostyard/pocketgo/service/internal/team"
)

// githubPull is GitHub's answer for pull request number of repo, from
// head in headRepo, open unless merged or closed.
func githubPull(repo string, number int64, head, headRepo, state string, merged bool) string {
	a := map[string]any{"number": number, "title": "Fix CI", "html_url": "https://github.com/" + repo + "/pull/" + itoa(number),
		"state": state, "draft": false, "merged_at": nil,
		"head": map[string]any{"ref": head, "sha": strings.Repeat("a", 40), "repo": map[string]any{"full_name": headRepo}},
		"base": map[string]any{"ref": "main", "repo": map[string]any{"full_name": repo}}}
	if merged {
		a["merged_at"] = "2026-11-19T12:00:00Z"
	}
	out, _ := json.Marshal(a)
	return string(out)
}

func itoa(n int64) string { b, _ := json.Marshal(n); return string(b) }

// veraThread delegates a worktree thread to vera and returns it, started.
func veraThread(t *testing.T, h *teamHarness) store.ThreadRow {
	t.Helper()
	if _, err := h.server.TeamDelegate(context.Background(), TeamCall{Session: h.super.ID, CallID: "c1"},
		team.Delegation{Agent: "vera", Task: "Fix CI", Repository: "frostyard/example"}); err != nil {
		t.Fatal(err)
	}
	thread, err := h.store.Thread(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	return thread
}

// answerGH has gh answer each pull request by its number.
func answerGH(h *teamHarness, answers map[string]string) {
	h.engine.mu.Lock()
	defer h.engine.mu.Unlock()
	h.engine.onGH = func(_ context.Context, args []string) (string, error) {
		if len(args) == 2 && args[0] == "api" {
			if out, ok := answers[args[1]]; ok {
				return out, nil
			}
		}
		return "", errors.New("gh: HTTP 404: Not Found")
	}
}

// A specialist's report links the pull requests it names, once each is
// read on GitHub and checked to be the thread's: in its repository, from
// its branch (ADR-0016).
func TestAReportLinksItsPullRequests(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	thread := veraThread(t, h)
	answerGH(h, map[string]string{"repos/frostyard/example/pulls/3": githubPull("frostyard/example", 3, thread.Branch, "frostyard/example", "open", false)})
	call := TeamCall{Session: thread.Session, Task: 5, CallID: "m1"}
	report := team.Report{Message: "PR #3 is open.", Status: team.StatusDone, PullRequests: []team.PullRef{{Number: 3}}}
	answer, err := h.server.TeamMessage(ctx, call, report)
	if err != nil {
		t.Fatal(err)
	}
	pulls, err := h.client.Pulls(ctx)
	if err != nil || len(pulls) != 1 || pulls[0].Thread != thread.ID || pulls[0].Number != 3 || pulls[0].State != proto.PullOpen || pulls[0].Head != thread.Branch {
		t.Fatalf("pulls %+v, %v", pulls, err)
	}
	h.mu.Lock()
	var events int
	for _, e := range h.events {
		if e.Type == proto.EventPull {
			events++
		}
	}
	h.mu.Unlock()
	if events != 1 {
		t.Fatalf("%d pull events", events)
	}
	reports, _ := h.store.UnsentReports(ctx)
	sent := h.engine.texts
	found := false
	for _, text := range sent {
		if strings.Contains(text, "Pull requests: frostyard/example#3 (open)") {
			found = true
		}
	}
	if !found && (len(reports) == 0 || !strings.Contains(reports[0].Text, "Pull requests: frostyard/example#3 (open)")) {
		t.Fatalf("the supervisor's message names no pull request: %v %+v", sent, reports)
	}
	// The call run again reads nothing on GitHub, and answers as it did.
	h.engine.mu.Lock()
	calls := len(h.engine.ghCalls)
	h.engine.mu.Unlock()
	again, err := h.server.TeamMessage(ctx, call, report)
	h.engine.mu.Lock()
	defer h.engine.mu.Unlock()
	if err != nil || again != answer || len(h.engine.ghCalls) != calls {
		t.Fatalf("the call run again: %q, %v, %d gh calls (was %d)", again, err, len(h.engine.ghCalls), calls)
	}
}

// A pull request that is not the thread's refuses the whole report, with
// why, and files nothing (ADR-0016).
func TestAReportWithAForeignPullRequestIsRefused(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	thread := veraThread(t, h)
	answerGH(h, map[string]string{
		"repos/frostyard/example/pulls/3": githubPull("frostyard/example", 3, "main", "frostyard/example", "open", false),
		"repos/frostyard/example/pulls/4": githubPull("frostyard/example", 4, thread.Branch, "evil/example", "open", false),
	})
	for i, c := range []struct {
		refs []team.PullRef
		want string
	}{
		{[]team.PullRef{{Number: 3}}, "not this thread's"},
		{[]team.PullRef{{Number: 4}}, "not from frostyard/example itself"},
		{[]team.PullRef{{Number: 9}}, "404"},
		{[]team.PullRef{{Repository: "frostyard/other", Number: 3}}, "not in this thread's repository"},
		{[]team.PullRef{{Number: 3}, {Number: 3}}, "named twice"},
	} {
		_, err := h.server.TeamMessage(ctx, TeamCall{Session: thread.Session, Task: 5, CallID: "m" + itoa(int64(i))},
			team.Report{Message: "x", Status: team.StatusDone, PullRequests: c.refs})
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%+v: %v, want %q", c.refs, err, c.want)
		}
	}
	if got, _ := h.store.Thread(ctx, thread.ID); got.State == store.ThreadDone || got.Report != "" {
		t.Fatalf("a refused report was filed: %+v", got)
	}
	if pulls, _ := h.store.PullsOfThread(ctx, thread.ID); len(pulls) != 0 {
		t.Fatalf("a refused pull request was linked: %+v", pulls)
	}
}

// pocketd reads each open pull request again in rounds: a change is
// recorded and published, one that cannot be read keeps its state, and a
// thread whose pull requests merged closes (ADR-0016).
func TestPullRoundsKeepPullRequestsCurrent(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	thread := veraThread(t, h)
	path := "repos/frostyard/example/pulls/3"
	answerGH(h, map[string]string{path: githubPull("frostyard/example", 3, thread.Branch, "frostyard/example", "open", false)})
	if _, err := h.server.TeamMessage(ctx, TeamCall{Session: thread.Session, Task: 5, CallID: "m1"},
		team.Report{Message: "PR #3 is open.", Status: team.StatusDone, PullRequests: []team.PullRef{{Number: 3}}}); err != nil {
		t.Fatal(err)
	}
	// GitHub cannot be read: the pull request stays as it was.
	answerGH(h, nil)
	if done, err := h.server.RefreshPulls(ctx); done || err == nil {
		t.Fatalf("a round GitHub did not answer: %v %v", done, err)
	}
	if pulls, _ := h.store.OpenPulls(ctx); len(pulls) != 1 {
		t.Fatalf("open %+v", pulls)
	}
	answerGH(h, map[string]string{path: githubPull("frostyard/example", 3, thread.Branch, "frostyard/example", "closed", true)})
	if done, err := h.server.RefreshPulls(ctx); !done || err != nil {
		t.Fatalf("a round: %v %v", done, err)
	}
	got, _ := h.store.Thread(ctx, thread.ID)
	if got.State != store.ThreadClosed || got.Report != team.MergedText([]string{"frostyard/example#3"}) {
		t.Fatalf("the thread after its pull request merged: %+v", got)
	}
	if states := h.threadStates(thread.ID); states[len(states)-1] != proto.ThreadClosed {
		t.Fatalf("thread events %v", states)
	}
	if open, _ := h.store.OpenPulls(ctx); len(open) != 0 {
		t.Fatalf("still open %+v", open)
	}
}

// A report of a pull request merged already closes its thread at once.
func TestAReportOfAMergedPullRequestClosesTheThread(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	thread := veraThread(t, h)
	answerGH(h, map[string]string{"repos/frostyard/example/pulls/3": githubPull("frostyard/example", 3, thread.Branch, "frostyard/example", "closed", true)})
	if _, err := h.server.TeamMessage(ctx, TeamCall{Session: thread.Session, Task: 5, CallID: "m1"},
		team.Report{Message: "Merged.", Status: team.StatusDone, PullRequests: []team.PullRef{{Number: 3}}}); err != nil {
		t.Fatal(err)
	}
	if got, _ := h.store.Thread(ctx, thread.ID); got.State != store.ThreadClosed {
		t.Fatalf("thread %+v", got)
	}
}

// Rounds run every interval until the server stops; none runs after.
func TestPullRoundsRunUntilStop(t *testing.T) {
	h := newTeamHarness(t)
	var scheduled []func()
	var every []time.Duration
	h.server.pullTimer = func(d time.Duration, f func()) func() {
		every = append(every, d)
		scheduled = append(scheduled, f)
		return func() {}
	}
	h.server.StartPulls(5 * time.Minute)
	if len(scheduled) != 1 || every[0] != 5*time.Minute {
		t.Fatalf("scheduled %d, every %v", len(scheduled), every)
	}
	scheduled[0]()
	if len(scheduled) != 2 {
		t.Fatalf("a round did not schedule the next: %d", len(scheduled))
	}
	h.server.Stop()
	scheduled[1]()
	if len(scheduled) != 2 {
		t.Fatal("a round ran after Stop")
	}
	h.server.StartPulls(0)
}
