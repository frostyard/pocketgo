package api

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
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
	a := map[string]any{"id": 1000 + number, "number": number, "title": "Fix CI", "html_url": "https://github.com/" + repo + "/pull/" + itoa(number),
		"state": state, "draft": false, "merged_at": nil, "labels": []any{},
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
		if len(args) == 4 && args[0] == "api" && args[1] == "--hostname" && args[2] == "github.com" {
			if out, ok := answers[args[3]]; ok {
				return out, nil
			}
		}
		// A compare GitHub answers, unless a test names its own: an open
		// pull request's read reads its merge base too (ADR-0019).
		if len(args) == 6 && args[0] == "api" && strings.Contains(args[3], "/compare/") {
			if out, ok := answers[args[3]]; ok {
				return out, nil
			}
			return `{"merge_base_commit":{"sha":"` + strings.Repeat("c", 40) + `"}}`, nil
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
	// The link's event, then the read of its merge base, which may change
	// the verdict it shows (Codex review of #26, round 8).
	if events < 1 || events > 2 {
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

// A thread whose last pull request merged closes in the next round, also
// when pocketd stopped between recording the merge and closing it: the
// round finds it from the work database (Codex review of #20, round 1).
func TestAMergedThreadClosesAfterAStop(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	thread := veraThread(t, h)
	path := "repos/frostyard/example/pulls/3"
	answerGH(h, map[string]string{path: githubPull("frostyard/example", 3, thread.Branch, "frostyard/example", "open", false)})
	if _, err := h.server.TeamMessage(ctx, TeamCall{Session: thread.Session, Task: 5, CallID: "m1"},
		team.Report{Message: "PR #3 is open.", Status: team.StatusDone, PullRequests: []team.PullRef{{Number: 3}}}); err != nil {
		t.Fatal(err)
	}
	// The merge was recorded, then pocketd stopped before it closed the
	// thread.
	pulls, _ := h.store.PullsOfThread(ctx, thread.ID)
	merged := pulls[0]
	merged.State, merged.CheckedAt = store.PullMerged, h.clock.Now()
	if _, _, err := h.store.UpdatePull(ctx, merged.ID, merged); err != nil {
		t.Fatal(err)
	}
	if done, err := h.server.RefreshPulls(ctx); !done || err != nil {
		t.Fatalf("a round: %v %v", done, err)
	}
	if got, _ := h.store.Thread(ctx, thread.ID); got.State != store.ThreadClosed {
		t.Fatalf("the merged thread after a round: %+v", got)
	}
}

// A report of a merged pull request run again closes its thread when the
// first run stopped before it did (Codex review of #20, round 1).
func TestAReportRunAgainClosesItsMergedThread(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	thread := veraThread(t, h)
	answerGH(h, map[string]string{"repos/frostyard/example/pulls/3": githubPull("frostyard/example", 3, thread.Branch, "frostyard/example", "closed", true)})
	call := TeamCall{Session: thread.Session, Task: 5, CallID: "m1"}
	// The first run filed the report and its pull request, then stopped.
	row := store.PullRow{Repository: "frostyard/example", Number: 3, Title: "Fix CI", URL: "u", State: store.PullMerged,
		Head: thread.Branch, HeadSHA: strings.Repeat("a", 40), Base: "main", CheckedAt: h.clock.Now()}
	if _, _, _, _, err := h.store.FileReportWithPulls(ctx, call.key(), thread.ID, store.ThreadDone, "Merged.", nil, []store.PullRow{row},
		func([]int64, []store.PullRow) string { return "r" }); err != nil {
		t.Fatal(err)
	}
	if _, err := h.server.TeamMessage(ctx, call, team.Report{Message: "Merged.", Status: team.StatusDone, PullRequests: []team.PullRef{{Number: 3}}}); err != nil {
		t.Fatal(err)
	}
	if got, _ := h.store.Thread(ctx, thread.ID); got.State != store.ThreadClosed {
		t.Fatalf("thread %+v", got)
	}
}

// A closed pull request may reopen: it is read again before its thread
// closes, and one open now keeps the thread (Codex review of #20, round 1).
func TestAReopenedPullRequestKeepsItsThread(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	thread := veraThread(t, h)
	answerGH(h, map[string]string{
		"repos/frostyard/example/pulls/3": githubPull("frostyard/example", 3, thread.Branch, "frostyard/example", "closed", false),
		"repos/frostyard/example/pulls/4": githubPull("frostyard/example", 4, thread.Branch, "frostyard/example", "open", false),
	})
	if _, err := h.server.TeamMessage(ctx, TeamCall{Session: thread.Session, Task: 5, CallID: "m1"},
		team.Report{Message: "Two.", Status: team.StatusWorking, PullRequests: []team.PullRef{{Number: 3}, {Number: 4}}}); err != nil {
		t.Fatal(err)
	}
	// #4 merges; #3, closed when it was read, is open again on GitHub.
	answerGH(h, map[string]string{
		"repos/frostyard/example/pulls/3": githubPull("frostyard/example", 3, thread.Branch, "frostyard/example", "open", false),
		"repos/frostyard/example/pulls/4": githubPull("frostyard/example", 4, thread.Branch, "frostyard/example", "closed", true),
	})
	h.clock.Advance(time.Minute)
	if _, err := h.server.RefreshPulls(ctx); err != nil {
		t.Fatal(err)
	}
	if got, _ := h.store.Thread(ctx, thread.ID); got.State == store.ThreadClosed {
		t.Fatalf("a thread with a reopened pull request closed: %+v", got)
	}
	if open, _ := h.store.OpenPulls(ctx); len(open) != 1 || open[0].Number != 3 {
		t.Fatalf("open %+v", open)
	}
}

// An abort of team_message stops its GitHub reads, and files nothing
// (Codex review of #20, round 1).
func TestAnAbortStopsAReportsGitHubReads(t *testing.T) {
	h := newTeamHarness(t)
	thread := veraThread(t, h)
	ctx, cancel := context.WithCancel(context.Background())
	h.engine.mu.Lock()
	h.engine.onGH = func(read context.Context, args []string) (string, error) {
		cancel()
		if err := read.Err(); err != nil {
			return "", err
		}
		return githubPull("frostyard/example", 3, thread.Branch, "frostyard/example", "open", false), nil
	}
	h.engine.mu.Unlock()
	if _, err := h.server.TeamMessage(ctx, TeamCall{Session: thread.Session, Task: 5, CallID: "m1"},
		team.Report{Message: "x", Status: team.StatusDone, PullRequests: []team.PullRef{{Number: 3}}}); err == nil {
		t.Fatal("an aborted report was filed")
	}
	if filed, _ := h.store.ReportFiled(context.Background(), TeamCall{Session: thread.Session, Task: 5, CallID: "m1"}.key()); filed {
		t.Fatal("an aborted report was filed")
	}
}

// A closed pull request of a thread not closed is read again each round:
// one that reopens and merges closes its thread (Codex review of #20,
// round 2).
func TestAClosedPullRequestIsStillRead(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	thread := veraThread(t, h)
	path := "repos/frostyard/example/pulls/3"
	answerGH(h, map[string]string{path: githubPull("frostyard/example", 3, thread.Branch, "frostyard/example", "closed", false)})
	if _, err := h.server.TeamMessage(ctx, TeamCall{Session: thread.Session, Task: 5, CallID: "m1"},
		team.Report{Message: "Closed for now.", Status: team.StatusWorking, PullRequests: []team.PullRef{{Number: 3}}}); err != nil {
		t.Fatal(err)
	}
	answerGH(h, map[string]string{path: githubPull("frostyard/example", 3, thread.Branch, "frostyard/example", "closed", true)})
	h.clock.Advance(time.Minute)
	if _, err := h.server.RefreshPulls(ctx); err != nil {
		t.Fatal(err)
	}
	if got, _ := h.store.Thread(ctx, thread.ID); got.State != store.ThreadClosed {
		t.Fatalf("a thread whose closed pull request merged: %+v", got)
	}
}

// A report aborted while it waits to be filed files nothing (Codex review
// of #20, round 2).
func TestAnAbortBeforeTheFilingFilesNothing(t *testing.T) {
	h := newTeamHarness(t)
	thread := veraThread(t, h)
	answerGH(h, map[string]string{"repos/frostyard/example/pulls/3": githubPull("frostyard/example", 3, thread.Branch, "frostyard/example", "open", false)})
	ctx, cancel := context.WithCancel(context.Background())
	call := TeamCall{Session: thread.Session, Task: 5, CallID: "m1"}
	h.server.workMu.Lock()
	done := make(chan error, 1)
	go func() {
		_, err := h.server.TeamMessage(ctx, call, team.Report{Message: "x", Status: team.StatusDone, PullRequests: []team.PullRef{{Number: 3}}})
		done <- err
	}()
	for !waitsIn("(*Server).TeamMessage", "sync.(*Mutex).Lock") {
		runtime.Gosched()
	}
	cancel()
	h.server.workMu.Unlock()
	if err := <-done; err == nil {
		t.Fatal("a report aborted before its filing was filed")
	}
	if filed, _ := h.store.ReportFiled(context.Background(), call.key()); filed {
		t.Fatal("a report aborted before its filing was filed")
	}
}

// waitsIn says whether a goroutine is in function, blocked in block.
func waitsIn(function, block string) bool {
	buf := make([]byte, 1<<20)
	n := runtime.Stack(buf, true)
	for _, g := range strings.Split(string(buf[:n]), "\n\n") {
		if strings.Contains(g, function) && strings.Contains(g, block) {
			return true
		}
	}
	return false
}

// The profile is checked again after the reads: a repository taken off it
// while they ran takes no pull request (Codex review of #20, round 2).
func TestAProfileChangedDuringTheReadsRefusesTheReport(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	thread := veraThread(t, h)
	h.engine.mu.Lock()
	h.engine.onGH = func(context.Context, []string) (string, error) {
		if err := os.WriteFile(filepath.Join(h.profileDir, "vera.md"), []byte("---\nmodel: faux/faux-1\nhome: ~\n---\n# Vera\n"), 0o600); err != nil {
			t.Error(err)
		}
		return githubPull("frostyard/example", 3, thread.Branch, "frostyard/example", "open", false), nil
	}
	h.engine.mu.Unlock()
	if _, err := h.server.TeamMessage(ctx, TeamCall{Session: thread.Session, Task: 5, CallID: "m1"},
		team.Report{Message: "x", Status: team.StatusDone, PullRequests: []team.PullRef{{Number: 3}}}); err == nil || !strings.Contains(err.Error(), "does not name") {
		t.Fatalf("a repository taken off the profile during the reads: %v", err)
	}
}

// A thread closes only against the pull requests just checked: one closed
// that a report linked while a recheck ran waits for the next round
// (Codex review of #20, round 2).
func TestACloseHoldsOnlyForThePullRequestsChecked(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	thread := veraThread(t, h)
	answerGH(h, map[string]string{
		"repos/frostyard/example/pulls/3": githubPull("frostyard/example", 3, thread.Branch, "frostyard/example", "closed", true),
		"repos/frostyard/example/pulls/4": githubPull("frostyard/example", 4, thread.Branch, "frostyard/example", "closed", false),
	})
	if _, err := h.server.TeamMessage(ctx, TeamCall{Session: thread.Session, Task: 5, CallID: "m1"},
		team.Report{Message: "x", Status: team.StatusWorking, PullRequests: []team.PullRef{{Number: 4}}}); err != nil {
		t.Fatal(err)
	}
	// #3 merged reported next; while #4 is read again, another report links
	// #5, closed.
	h.engine.mu.Lock()
	h.engine.onGH = func(_ context.Context, args []string) (string, error) {
		switch args[3] {
		case "repos/frostyard/example/pulls/3":
			return githubPull("frostyard/example", 3, thread.Branch, "frostyard/example", "closed", true), nil
		case "repos/frostyard/example/pulls/4":
			row := store.PullRow{Repository: "frostyard/example", Number: 5, Title: "t", URL: "u", State: store.PullClosed,
				Head: thread.Branch, HeadSHA: strings.Repeat("a", 40), Base: "main", CheckedAt: h.clock.Now()}
			if _, _, _, _, err := h.store.FileReportWithPulls(context.Background(), "other", thread.ID, store.ThreadOpen, "y", nil,
				[]store.PullRow{row}, func([]int64, []store.PullRow) string { return "r" }); err != nil {
				t.Error(err)
			}
			return githubPull("frostyard/example", 4, thread.Branch, "frostyard/example", "closed", false), nil
		}
		return "", errors.New("404")
	}
	h.engine.mu.Unlock()
	h.clock.Advance(time.Minute)
	if _, err := h.server.TeamMessage(ctx, TeamCall{Session: thread.Session, Task: 5, CallID: "m2"},
		team.Report{Message: "Merged.", Status: team.StatusDone, PullRequests: []team.PullRef{{Number: 3}}}); err != nil {
		t.Fatal(err)
	}
	if got, _ := h.store.Thread(ctx, thread.ID); got.State == store.ThreadClosed {
		t.Fatalf("a thread closed over a pull request no one checked: %+v", got)
	}
}

// A round's read of a pull request waits while a report holds it, from
// its read to its record (Codex review of #20, round 2).
func TestARoundWaitsForAReportHoldingItsPullRequest(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	thread := veraThread(t, h)
	path := "repos/frostyard/example/pulls/3"
	answerGH(h, map[string]string{path: githubPull("frostyard/example", 3, thread.Branch, "frostyard/example", "open", false)})
	if _, err := h.server.TeamMessage(ctx, TeamCall{Session: thread.Session, Task: 5, CallID: "m1"},
		team.Report{Message: "x", Status: team.StatusWorking, PullRequests: []team.PullRef{{Number: 3}}}); err != nil {
		t.Fatal(err)
	}
	inRead, release := make(chan struct{}), make(chan struct{})
	h.engine.mu.Lock()
	h.engine.onGH = func(context.Context, []string) (string, error) {
		h.engine.mu.Lock()
		h.engine.onGH = nil
		h.engine.mu.Unlock()
		close(inRead)
		<-release
		return githubPull("frostyard/example", 3, thread.Branch, "frostyard/example", "open", false), nil
	}
	h.engine.mu.Unlock()
	reported := make(chan error, 1)
	go func() {
		_, err := h.server.TeamMessage(ctx, TeamCall{Session: thread.Session, Task: 5, CallID: "m2"},
			team.Report{Message: "y", Status: team.StatusWorking, PullRequests: []team.PullRef{{Number: 3}}})
		reported <- err
	}()
	<-inRead
	round := make(chan struct{})
	go func() {
		_, _ = h.server.RefreshPulls(ctx)
		close(round)
	}()
	for !waitsIn("(*Server).refreshPull", "(*ctxLocks).lock") {
		select {
		case <-round:
			t.Fatal("a round read the pull request a report held")
		default:
			runtime.Gosched()
		}
	}
	close(release)
	if err := <-reported; err != nil {
		t.Fatal(err)
	}
	<-round
}

// A report run again for a thread that closed already reads nothing on
// GitHub (Codex review of #20, round 2).
func TestAReportRunAgainOnAClosedThreadReadsNothing(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	thread := veraThread(t, h)
	answerGH(h, map[string]string{
		"repos/frostyard/example/pulls/3": githubPull("frostyard/example", 3, thread.Branch, "frostyard/example", "closed", true),
		"repos/frostyard/example/pulls/4": githubPull("frostyard/example", 4, thread.Branch, "frostyard/example", "closed", false),
	})
	call := TeamCall{Session: thread.Session, Task: 5, CallID: "m1"}
	report := team.Report{Message: "x", Status: team.StatusDone, PullRequests: []team.PullRef{{Number: 3}, {Number: 4}}}
	if _, err := h.server.TeamMessage(ctx, call, report); err != nil {
		t.Fatal(err)
	}
	if got, _ := h.store.Thread(ctx, thread.ID); got.State != store.ThreadClosed {
		t.Fatalf("thread %+v", got)
	}
	h.engine.mu.Lock()
	calls := len(h.engine.ghCalls)
	h.engine.mu.Unlock()
	if _, err := h.server.TeamMessage(ctx, call, report); err != nil {
		t.Fatal(err)
	}
	h.engine.mu.Lock()
	defer h.engine.mu.Unlock()
	if len(h.engine.ghCalls) != calls {
		t.Fatalf("a report run again on a closed thread read GitHub: %v", h.engine.ghCalls[calls:])
	}
}

// The profile is checked again under workMu, right before the filing: a
// repository taken off it while the report waited takes no pull request
// (Codex review of #20, round 3).
func TestAProfileChangedWhileTheReportWaitsRefusesIt(t *testing.T) {
	h := newTeamHarness(t)
	thread := veraThread(t, h)
	read := make(chan struct{})
	h.engine.mu.Lock()
	h.engine.onGH = func(context.Context, []string) (string, error) {
		defer close(read)
		return githubPull("frostyard/example", 3, thread.Branch, "frostyard/example", "open", false), nil
	}
	h.engine.mu.Unlock()
	h.server.workMu.Lock()
	done := make(chan error, 1)
	go func() {
		_, err := h.server.TeamMessage(context.Background(), TeamCall{Session: thread.Session, Task: 5, CallID: "m1"},
			team.Report{Message: "x", Status: team.StatusDone, PullRequests: []team.PullRef{{Number: 3}}})
		done <- err
	}()
	// The read is done, and the report waits for workMu to be filed.
	<-read
	for !waitsIn("(*Server).TeamMessage", "sync.(*Mutex).Lock") {
		runtime.Gosched()
	}
	if err := os.WriteFile(filepath.Join(h.profileDir, "vera.md"), []byte("---\nmodel: faux/faux-1\nhome: ~\n---\n# Vera\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.server.workMu.Unlock()
	if err := <-done; err == nil || !strings.Contains(err.Error(), "does not name") {
		t.Fatalf("a repository taken off the profile while the report waited: %v", err)
	}
}

// A wait for a pull request another holds ends with its context: Stop, or
// an abort, never waits for a batch of reads (Codex review of #20, round
// 3).
func TestAWaitForAPullRequestEndsWithItsContext(t *testing.T) {
	h := newTeamHarness(t)
	unlock, err := h.server.lockPulls(context.Background(), []string{"frostyard/example#3"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := h.server.refreshPull(ctx, store.PullRow{Repository: "frostyard/example", Number: 3})
		done <- err
	}()
	for !waitsIn("(*Server).refreshPull", "(*ctxLocks).lock") {
		runtime.Gosched()
	}
	cancel()
	for i := 0; ; i++ {
		select {
		case err := <-done:
			unlock()
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("the wait ended with %v", err)
			}
			return
		default:
		}
		if i > 1_000_000 {
			unlock()
			<-done
			t.Fatal("the wait outlived its context")
		}
		runtime.Gosched()
	}
}

// Reads are ordered by the store, not the clock: a closed pull request
// that reopened, read again after the clock moved back, keeps its thread
// open when another merges (Codex review of #20, round 3).
func TestAClockMovedBackDoesNotHideAReopen(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	thread := veraThread(t, h)
	answerGH(h, map[string]string{"repos/frostyard/example/pulls/4": githubPull("frostyard/example", 4, thread.Branch, "frostyard/example", "closed", false)})
	if _, err := h.server.TeamMessage(ctx, TeamCall{Session: thread.Session, Task: 5, CallID: "m1"},
		team.Report{Message: "x", Status: team.StatusWorking, PullRequests: []team.PullRef{{Number: 4}}}); err != nil {
		t.Fatal(err)
	}
	// The clock moves back; #4 reopened, and #3 merged.
	h.clock.Advance(-time.Hour)
	answerGH(h, map[string]string{
		"repos/frostyard/example/pulls/3": githubPull("frostyard/example", 3, thread.Branch, "frostyard/example", "closed", true),
		"repos/frostyard/example/pulls/4": githubPull("frostyard/example", 4, thread.Branch, "frostyard/example", "open", false),
	})
	if _, err := h.server.TeamMessage(ctx, TeamCall{Session: thread.Session, Task: 5, CallID: "m2"},
		team.Report{Message: "Merged #3.", Status: team.StatusDone, PullRequests: []team.PullRef{{Number: 3}}}); err != nil {
		t.Fatal(err)
	}
	if got, _ := h.store.Thread(ctx, thread.ID); got.State == store.ThreadClosed {
		t.Fatalf("a thread with a reopened pull request closed after the clock moved back: %+v", got)
	}
}

// A filed report goes to the supervisor before any read of GitHub its
// thread's close needs (Codex review of #20, round 3).
func TestAReportGoesBeforeTheRechecks(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	thread := veraThread(t, h)
	answerGH(h, map[string]string{"repos/frostyard/example/pulls/4": githubPull("frostyard/example", 4, thread.Branch, "frostyard/example", "closed", false)})
	if _, err := h.server.TeamMessage(ctx, TeamCall{Session: thread.Session, Task: 5, CallID: "m1"},
		team.Report{Message: "x", Status: team.StatusWorking, PullRequests: []team.PullRef{{Number: 4}}}); err != nil {
		t.Fatal(err)
	}
	sentFirst := false
	h.engine.mu.Lock()
	h.engine.onGH = func(_ context.Context, args []string) (string, error) {
		switch args[3] {
		case "repos/frostyard/example/pulls/3":
			return githubPull("frostyard/example", 3, thread.Branch, "frostyard/example", "closed", true), nil
		case "repos/frostyard/example/pulls/4":
			// The recheck of #4: the merged report must have gone.
			h.engine.mu.Lock()
			for _, text := range h.engine.texts {
				sentFirst = sentFirst || strings.Contains(text, "Merged #3.")
			}
			h.engine.mu.Unlock()
			return githubPull("frostyard/example", 4, thread.Branch, "frostyard/example", "closed", false), nil
		}
		return "", errors.New("404")
	}
	h.engine.mu.Unlock()
	if _, err := h.server.TeamMessage(ctx, TeamCall{Session: thread.Session, Task: 5, CallID: "m2"},
		team.Report{Message: "Merged #3.", Status: team.StatusDone, PullRequests: []team.PullRef{{Number: 3}}}); err != nil {
		t.Fatal(err)
	}
	if !sentFirst {
		t.Fatal("the report waited for the recheck of a closed pull request")
	}
}

// A read that GitHub answers with another pull request than the linked one,
// as after a repository's name was reused, changes nothing and closes
// nothing (Codex review of #20, round 3).
func TestAnotherPullRequestAtTheSameNameIsRefused(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	thread := veraThread(t, h)
	path := "repos/frostyard/example/pulls/3"
	answerGH(h, map[string]string{path: githubPull("frostyard/example", 3, thread.Branch, "frostyard/example", "open", false)})
	if _, err := h.server.TeamMessage(ctx, TeamCall{Session: thread.Session, Task: 5, CallID: "m1"},
		team.Report{Message: "x", Status: team.StatusWorking, PullRequests: []team.PullRef{{Number: 3}}}); err != nil {
		t.Fatal(err)
	}
	other := strings.Replace(githubPull("frostyard/example", 3, thread.Branch, "frostyard/example", "closed", true), `"id":1003`, `"id":7777`, 1)
	answerGH(h, map[string]string{path: other})
	if done, err := h.server.RefreshPulls(ctx); done || !errors.Is(err, store.ErrPullReplaced) {
		t.Fatalf("a round that read another pull request: %v %v", done, err)
	}
	if got, _ := h.store.Thread(ctx, thread.ID); got.State == store.ThreadClosed {
		t.Fatalf("another pull request's merge closed the thread: %+v", got)
	}
}

// A new report on a closed thread is refused before GitHub is read (Codex
// review of #20, round 3).
func TestAReportOnAClosedThreadReadsNothing(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	thread := veraThread(t, h)
	if _, err := h.client.CloseThread(ctx, thread.ID, proto.CloseThread{RequestID: "c"}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.server.TeamMessage(ctx, TeamCall{Session: thread.Session, Task: 5, CallID: "m1"},
		team.Report{Message: "x", Status: team.StatusDone, PullRequests: []team.PullRef{{Number: 3}}}); err == nil || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("a report on a closed thread: %v", err)
	}
	h.engine.mu.Lock()
	defer h.engine.mu.Unlock()
	if len(h.engine.ghCalls) != 0 {
		t.Fatalf("GitHub was read for a closed thread: %v", h.engine.ghCalls)
	}
}
