package api

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/frostyard/pocketgo/proto"
	"github.com/frostyard/pocketgo/service/internal/store"
	"github.com/frostyard/pocketgo/service/internal/team"
)

// The specialist's report after the owner's message waits for the close's
// worker stop: the thread reopens only once the run of the closed time is
// stopped, and the report is taken then (Codex review of #121, rounds 4
// to 6).
func TestAClosesWorkerStopTargetsTheRunItFound(t *testing.T) {
	h, thread := workerHarness(t)
	ctx := context.Background()
	worker := startedWorker(t, h, thread)
	h.engine.mu.Lock()
	if h.engine.busy == nil {
		h.engine.busy = map[int64]bool{}
	}
	h.engine.busy[worker] = true
	h.engine.mu.Unlock()
	row, err := h.server.closeThreadLocked(ctx, thread.ID, TeamCall{Session: h.super.ID, CallID: "close"}.key(), nil,
		func() (store.ThreadRow, error) { return h.store.CloseThread(ctx, thread.ID) })
	if err != nil || row.State != store.ThreadClosed {
		t.Fatalf("the close %+v, %v", row, err)
	}
	if _, err := h.client.Send(ctx, thread.Session, proto.SendMessage{RequestID: "m1", Text: "One more thing."}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.server.TeamMessage(ctx, TeamCall{Session: thread.Session, CallID: "early"}, team.Report{Status: team.StatusWorking, Message: "x"}); err == nil {
		t.Fatal("a report was taken before the worker's stop")
	}
	h.server.stopWorker(ctx, row.WorkerSession)
	h.engine.mu.Lock()
	delete(h.engine.busy, worker)
	h.engine.mu.Unlock()
	if _, err := h.server.TeamMessage(ctx, TeamCall{Session: thread.Session, CallID: "r1"}, team.Report{Status: team.StatusWorking, Message: "Pushed."}); err != nil {
		t.Fatalf("the report after the stop: %v", err)
	}
	if got, _ := h.store.Thread(ctx, thread.ID); got.State != store.ThreadOpen {
		t.Fatalf("the thread %+v", got)
	}
}

// team_send notes a closed thread's placed messages before it checks the
// closed thread's folder: a thread the owner's message reopened takes the
// supervisor's message as an open one, whatever its folder (Codex review
// of #121, round 4).
func TestTeamSendNotesBeforeTheClosedFolderCheck(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	thread := veraThread(t, h)
	queuedOverAClose(t, h, thread, "m1")
	// The reopened run's specialist moved its worktree to another branch.
	branchBaseGit(t, thread.Cwd, "checkout", "-q", "-b", "elsewhere")
	call := TeamCall{Session: h.super.ID, CallID: "s1"}
	if _, err := h.server.TeamSend(ctx, call, team.Send{Thread: thread.ID, Text: "And the nits."}); err != nil {
		t.Fatalf("the follow-up was refused: %v", err)
	}
	if row, err := h.store.Report(ctx, team.SendKey(call.Session, call.key())); err != nil || row.Reopens != 0 {
		t.Fatalf("the message %+v, %v", row, err)
	}
}

// The owner's message queued for a plan task's thread that closed is not
// followed further: no message reopens a plan's thread, so it holds
// neither the delivery pass nor the worktree (Codex review of #121, round
// 4).
func TestAClosedPlanThreadsMessagesAreNotFollowed(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	thread := homeThread(t, h, "d1")
	db, err := sql.Open("sqlite", h.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec(`UPDATE threads SET plan = 7, plan_task = 't1' WHERE id = ?`, thread.ID); err != nil {
		t.Fatal(err)
	}
	h.queueOwner(thread.Session)
	if _, err := h.client.Send(ctx, thread.Session, proto.SendMessage{RequestID: "m1", Text: "Before the close."}); err != nil {
		t.Fatal(err)
	}
	// Its plan closes it, as a plan's close does, outside this test's plan.
	if _, err := db.Exec(`UPDATE threads SET state = 'closed' WHERE id = ?`, thread.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.server.deliverAll(ctx); err != nil {
		t.Fatal(err)
	}
	if left, _ := h.store.OwnerMessagesOf(ctx, thread.ID); len(left) != 0 {
		t.Fatalf("still followed: %+v", left)
	}
	if waiting, _ := h.store.MessagesToReopen(ctx, thread.ID); waiting {
		t.Fatal("the plan thread's worktree waits for a message that reopens nothing")
	}
}

// The linked-pull check reads the pull request under its lock: a report
// that waited for the lock while another thread linked the pull request is
// told to report without it, before GitHub is read (Codex review of #121,
// round 4).
func TestTheLinkedPullCheckReadsUnderThePullsLock(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	first := veraThread(t, h)
	if _, err := h.server.TeamDelegate(ctx, TeamCall{Session: h.super.ID, CallID: "c2"},
		team.Delegation{Agent: "vera", Task: "The same fix", Repository: "frostyard/example"}); err != nil {
		t.Fatal(err)
	}
	second, err := h.store.ThreadByKey(ctx, TeamCall{Session: h.super.ID, CallID: "c2"}.key())
	if err != nil {
		t.Fatal(err)
	}
	answerGH(h, map[string]string{"repos/frostyard/example/pulls/3": githubPull("frostyard/example", 3, first.Branch, "frostyard/example", "open", false)})
	unlock, err := h.server.lockPulls(ctx, []string{"frostyard/example#3"})
	if err != nil {
		t.Fatal(err)
	}
	waiting := make(chan struct{}, 1)
	h.server.pullLocks.onWait = func(string) { waiting <- struct{}{} }
	answer := make(chan error, 1)
	go func() {
		_, err := h.server.TeamMessage(ctx, TeamCall{Session: second.Session, CallID: "r1"},
			team.Report{Status: team.StatusDone, Message: "Fixed.", PullRequests: []team.PullRef{{Number: 3}}})
		answer <- err
	}()
	<-waiting
	h.server.pullLocks.onWait = nil
	// The first thread's report links it while the second waits.
	linked := store.PullRow{GitHubID: 3, Repository: "frostyard/example", Number: 3, Title: "t", URL: "u", State: store.PullOpen,
		Head: first.Branch, HeadSHA: strings.Repeat("a", 40), Base: "main"}
	if _, _, _, _, err := h.store.FileReportWithPulls(ctx, "first-report", first.ID, store.ThreadOpen, "PR #3", nil, []store.PullRow{linked},
		func([]int64, []store.PullRow) string { return "r" }); err != nil {
		t.Fatal(err)
	}
	unlock()
	if err := <-answer; err == nil || !strings.Contains(err.Error(), "frostyard/example#3 is linked to thread T1: report again without it") {
		t.Fatalf("the waiting report: %v", err)
	}
}
