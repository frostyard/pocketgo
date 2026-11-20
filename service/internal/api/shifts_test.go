package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/frostyard/pocketgo/proto"
	"github.com/frostyard/pocketgo/service/internal/store"
	"github.com/frostyard/pocketgo/service/internal/team"
)

func rounds(n int) *int        { return &n }
func hours(h float64) *float64 { return &h }

// shiftThread delegates Duncan's thread with shift sh, as a supervisor
// does, and reads it back once its brief went.
func shiftThread(t *testing.T, h *teamHarness, callID string, sh team.Shift) store.ThreadRow {
	t.Helper()
	return shiftThreadOf(t, h, "duncan", "", callID, sh)
}

// shiftThreadOf is shiftThread for agent, in repository ("" for its home).
func shiftThreadOf(t *testing.T, h *teamHarness, agent, repository, callID string, sh team.Shift) store.ThreadRow {
	t.Helper()
	call := TeamCall{Session: h.super.ID, CallID: callID}
	if _, err := h.server.TeamDelegate(context.Background(), call, team.Delegation{Agent: agent, Task: "Fix the flaky tests.", Repository: repository, Shift: &sh}); err != nil {
		t.Fatal(err)
	}
	thread, err := h.store.ThreadByKey(context.Background(), call.key())
	if err != nil || thread.Session == 0 || thread.BriefSubmission == 0 {
		t.Fatalf("the shift's thread %+v, %v", thread, err)
	}
	return thread
}

// answerRound plays the specialist: the message of submission reaches its
// transcript, and its answer follows.
func (h *teamHarness) answerRound(thread store.ThreadRow, submission int64, answer string) {
	h.t.Helper()
	h.engine.mu.Lock()
	defer h.engine.mu.Unlock()
	if h.engine.transcripts == nil {
		h.engine.transcripts = map[int64][]proto.Entry{}
	}
	entries := h.engine.transcripts[thread.Session]
	next := int64(len(entries) + 1)
	entries = append(entries, proto.Entry{ID: next, SessionID: thread.Session, Kind: proto.EntryUser, SubmissionID: submission, Text: "the round"})
	if answer != "" {
		entries = append(entries, proto.Entry{ID: next + 1, SessionID: thread.Session, Kind: proto.EntryAssistant, Text: answer})
	}
	h.engine.transcripts[thread.Session] = entries
}

// submissionOf is the submission of the outbox message key.
func (h *teamHarness) submissionOf(key string) int64 {
	h.t.Helper()
	row, err := h.store.Report(context.Background(), key)
	if err != nil || row.Submission == 0 {
		h.t.Fatalf("the message %s: %+v, %v", key, row, err)
	}
	return row.Submission
}

func (h *teamHarness) shift(thread store.ThreadRow) store.ShiftRow {
	h.t.Helper()
	sh, err := h.store.Shift(context.Background(), thread.ID)
	if err != nil {
		h.t.Fatal(err)
	}
	return sh
}

// reportsTo are the thread's reports the supervisor got.
func (h *teamHarness) reportsTo(thread store.ThreadRow) []string {
	prefix := "[T" + itoa(thread.ID) + " · "
	var out []string
	for _, text := range h.sentTo(h.super.ID) {
		if strings.HasPrefix(text, prefix) {
			out = append(out, text)
		}
	}
	return out
}

// pocketd drives a shift in code: the brief is round 1 and says the rules;
// each answer's STATUS line ends its round; continue sends the next round,
// done its summary round; the summary round's report ends the shift, and
// the supervisor hears once (ADR-0024).
func TestAShiftRunsItsRoundsInCode(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	thread := shiftThread(t, h, "s1", team.Shift{Stop: "the tests pass", MaxRounds: rounds(5)})
	if brief := h.sentTo(thread.Session); len(brief) != 1 || !strings.Contains(brief[0], "This thread is a shift") || !strings.Contains(brief[0], "done when: the tests pass") {
		t.Fatalf("the brief %q", brief)
	}
	if sh := h.shift(thread); sh.State != team.ShiftRunning || sh.Round != 1 || sh.MaxRounds != 5 {
		t.Fatalf("the shift %+v", sh)
	}
	// Round 1 not answered yet: nothing moves.
	h.server.shiftPass(ctx)
	if sh := h.shift(thread); sh.Round != 1 {
		t.Fatalf("a round with no answer ended: %+v", sh)
	}
	h.answerRound(thread, thread.BriefSubmission, "Fixed one.\nSTATUS: CONTINUE")
	h.server.shiftPass(ctx)
	sh := h.shift(thread)
	if sh.Round != 2 || len(sh.Rounds) != 1 || sh.Rounds[0].Verdict != team.VerdictContinue {
		t.Fatalf("after round 1 %+v", sh)
	}
	sent := h.sentTo(thread.Session)
	if len(sent) != 2 || !strings.Contains(sent[1], "[shift] T"+itoa(thread.ID)+" · Round 2 of 5") {
		t.Fatalf("round 2's message %q", sent)
	}
	// A pass again sends nothing twice.
	h.server.shiftPass(ctx)
	if again := h.sentTo(thread.Session); len(again) != 2 {
		t.Fatalf("a pass sent again: %q", again)
	}
	h.answerRound(thread, h.submissionOf(shiftRoundKey(thread, 2)), "All fixed.\n**STATUS: DONE**")
	h.server.shiftPass(ctx)
	if sh = h.shift(thread); sh.State != team.ShiftSummary || sh.Reason != team.ShiftDone {
		t.Fatalf("after done %+v", sh)
	}
	if sent = h.sentTo(thread.Session); len(sent) != 3 || !strings.Contains(sent[2], "The shift has stopped: the stop condition holds") {
		t.Fatalf("the summary's message %q", sent)
	}
	h.summaryReached(thread)
	if _, err := h.server.TeamMessage(ctx, TeamCall{Session: thread.Session, CallID: "r1"}, team.Report{Status: team.StatusDone, Message: "Fixed the tests."}); err != nil {
		t.Fatal(err)
	}
	if sh = h.shift(thread); sh.State != team.ShiftEnded || sh.Reason != team.ShiftDone {
		t.Fatalf("after the report %+v", sh)
	}
	h.server.shiftPass(ctx)
	if reports := h.reportsTo(thread); len(reports) != 1 || !strings.Contains(reports[0], "Fixed the tests.") {
		t.Fatalf("the supervisor's reports %q", reports)
	}
	// The thread carries its shift over the protocol.
	threads, err := h.client.Threads(ctx)
	if err != nil || len(threads) == 0 || threads[0].Shift == nil || threads[0].Shift.State != proto.ShiftEnded || len(threads[0].Shift.Rounds) != 2 {
		t.Fatalf("the threads %+v, %v", threads, err)
	}
}

// The round limit and the deadline end a shift, checked before a round;
// a summary round that ends with no report gets pocketd's, once (ADR-0024).
func TestAShiftStopsAtItsLimits(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	thread := shiftThread(t, h, "s1", team.Shift{Stop: "the tests pass", MaxRounds: rounds(2)})
	h.answerRound(thread, thread.BriefSubmission, "STATUS: CONTINUE")
	h.server.shiftPass(ctx)
	h.answerRound(thread, h.submissionOf(shiftRoundKey(thread, 2)), "Still going.")
	h.server.shiftPass(ctx)
	sh := h.shift(thread)
	if sh.State != team.ShiftSummary || sh.Reason != team.ShiftMaxRounds || sh.Rounds[1].Verdict != team.VerdictNone {
		t.Fatalf("at the round limit %+v", sh)
	}
	h.answerRound(thread, h.submissionOf(shiftSummaryKey(thread, 2)), "I forgot to report.")
	h.server.shiftPass(ctx)
	if sh = h.shift(thread); sh.State != team.ShiftEnded || sh.Reason != team.ShiftMaxRounds {
		t.Fatalf("after the summary %+v", sh)
	}
	h.server.shiftPass(ctx)
	if reports := h.reportsTo(thread); len(reports) != 1 || !strings.Contains(reports[0], "The shift ended: it reached its 2 rounds. It took 2 of 2 rounds.") {
		t.Fatalf("pocketd's report %q", reports)
	}

	timed := shiftThread(t, h, "s2", team.Shift{Stop: "the tests pass", MaxRounds: rounds(10), Hours: hours(1)})
	h.answerRound(timed, timed.BriefSubmission, "STATUS: CONTINUE")
	h.clock.Advance(2 * time.Hour)
	h.server.shiftPass(ctx)
	if sh = h.shift(timed); sh.State != team.ShiftSummary || sh.Reason != team.ShiftDeadline {
		t.Fatalf("past the deadline %+v", sh)
	}
}

// BLOCKED ends a shift, and a run that failed with no answer ends it with
// pocketd's report (ADR-0024).
func TestAShiftEndsBlockedOrFailed(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	blocked := shiftThread(t, h, "s1", team.Shift{Stop: "deployed"})
	h.answerRound(blocked, blocked.BriefSubmission, "No key.\nSTATUS: BLOCKED: needs the deploy key")
	h.server.shiftPass(ctx)
	if sh := h.shift(blocked); sh.State != team.ShiftSummary || sh.Reason != team.ShiftBlocked || !strings.Contains(sh.Why, "needs the deploy key") {
		t.Fatalf("blocked %+v", sh)
	}
	failed := shiftThread(t, h, "s2", team.Shift{Stop: "deployed"})
	h.answerRound(failed, failed.BriefSubmission, "")
	h.engine.mu.Lock()
	if h.engine.transcriptLive == nil {
		h.engine.transcriptLive = map[int64]proto.Live{}
	}
	h.engine.transcriptLive[failed.Session] = proto.Live{Failure: "model_error: 500"}
	h.engine.mu.Unlock()
	h.server.shiftPass(ctx)
	if sh := h.shift(failed); sh.State != team.ShiftEnded || sh.Reason != team.ShiftFailed || !strings.Contains(sh.Why, "model_error") {
		t.Fatalf("failed %+v", sh)
	}
	if reports := h.reportsTo(failed); len(reports) != 1 || !strings.Contains(reports[0], "The shift ended: its run failed") {
		t.Fatalf("the failed shift's report %q", reports)
	}
}

// A round is not over while a message to the specialist or its worker is
// still owed: its answer is read once they went (ADR-0024).
func TestARoundWaitsForWhatIsOwed(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	thread := shiftThread(t, h, "s1", team.Shift{Stop: "the tests pass"})
	h.answerRound(thread, thread.BriefSubmission, "Waiting on my worker.\nSTATUS: CONTINUE")
	if _, err := h.store.QueueWorkerReport(ctx, store.ReportRow{Key: workerReplyPrefix + thread.Owner + ":9", ThreadID: thread.ID, SessionID: thread.Session, Text: "[T1 · worker] done"}, 0); err != nil {
		t.Fatal(err)
	}
	h.engine.mu.Lock()
	h.engine.failTo = map[int64]bool{thread.Session: true}
	h.engine.mu.Unlock()
	h.server.shiftPass(ctx)
	if sh := h.shift(thread); sh.Round != 1 {
		t.Fatalf("a round with a reply owed ended: %+v", sh)
	}
}

// The specialist reports done or blocked only in the summary round: a
// working report with asks goes as any (ADR-0024).
func TestAShiftsReportWaitsForItsSummary(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	thread := shiftThread(t, h, "s1", team.Shift{Stop: "the tests pass"})
	for _, status := range []string{team.StatusDone, team.StatusBlocked} {
		if _, err := h.server.TeamMessage(ctx, TeamCall{Session: thread.Session, CallID: "r-" + status}, team.Report{Status: status, Message: "Done."}); err == nil ||
			!strings.Contains(err.Error(), "runs a shift") {
			t.Fatalf("%s during a round: %v", status, err)
		}
	}
	if _, err := h.server.TeamMessage(ctx, TeamCall{Session: thread.Session, CallID: "w1"}, team.Report{Status: team.StatusWorking, Message: "Half way.",
		Asks: []team.Ask{{Question: "Which branch?"}}}); err != nil {
		t.Fatalf("a working report: %v", err)
	}
}

// The owner's message pauses a shift; the owner resumes it at its next
// round, and stops it, once per request: the stop ends the shift, aborts
// no run, and the supervisor hears once (ADR-0024).
func TestTheOwnerPausesResumesAndStopsAShift(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	thread := shiftThread(t, h, "s1", team.Shift{Stop: "the tests pass", MaxRounds: rounds(5)})
	if _, err := h.client.Send(ctx, thread.Session, proto.SendMessage{RequestID: "o1", Text: "Hold on, use the other fixture."}); err != nil {
		t.Fatal(err)
	}
	if sh := h.shift(thread); sh.State != team.ShiftPaused || sh.Reason != team.ShiftPerson {
		t.Fatalf("after the owner's message %+v", sh)
	}
	h.answerRound(thread, thread.BriefSubmission, "STATUS: CONTINUE")
	h.server.shiftPass(ctx)
	if sh := h.shift(thread); sh.Round != 1 || sh.State != team.ShiftPaused {
		t.Fatalf("a paused shift moved: %+v", sh)
	}
	resumed, err := h.client.ResumeShift(ctx, thread.ID, proto.ResumeShift{RequestID: "r1"})
	if err != nil || resumed.Shift == nil || resumed.Shift.State != proto.ShiftRunning || resumed.Shift.Round != 2 {
		t.Fatalf("resumed %+v, %v", resumed.Shift, err)
	}
	if again, err := h.client.ResumeShift(ctx, thread.ID, proto.ResumeShift{RequestID: "r1"}); err != nil || again.Shift.Round != 2 {
		t.Fatalf("the same resume again %+v, %v", again.Shift, err)
	}
	if sent := h.sentTo(thread.Session); !strings.Contains(sent[len(sent)-1], "Round 2 of 5") {
		t.Fatalf("the resumed round's message %q", sent)
	}
	var pe *proto.Error
	if _, err := h.client.ResumeShift(ctx, thread.ID, proto.ResumeShift{RequestID: "r2"}); !errors.As(err, &pe) || pe.Status != http.StatusConflict {
		t.Fatalf("a running shift resumed: %v", err)
	}
	h.engine.mu.Lock()
	h.engine.runFirst = 1
	h.engine.mu.Unlock()
	stopped, err := h.client.StopShift(ctx, thread.ID, proto.StopShift{RequestID: "x1"})
	if err != nil || stopped.Shift.State != proto.ShiftEnded || stopped.Shift.Reason != team.ShiftStopped {
		t.Fatalf("stopped %+v, %v", stopped.Shift, err)
	}
	// The stop ends the shift only: the specialist's run goes on to its
	// end (the owner's choice, 2026-11-20).
	h.engine.mu.Lock()
	aborted := append([]int64(nil), h.engine.aborted...)
	h.engine.runFirst = 0
	h.engine.mu.Unlock()
	if len(aborted) != 0 {
		t.Fatalf("the stop aborted runs: %v", aborted)
	}
	if _, err := h.client.StopShift(ctx, thread.ID, proto.StopShift{RequestID: "x1"}); err != nil {
		t.Fatal(err)
	}
	if reports := h.reportsTo(thread); len(reports) != 1 || !strings.Contains(reports[0], "the owner stopped it") {
		t.Fatalf("the supervisor's reports %q", reports)
	}
	if _, err := h.client.StopShift(ctx, 999, proto.StopShift{RequestID: "x2"}); !errors.As(err, &pe) || pe.Status != http.StatusNotFound {
		t.Fatalf("a thread that is not there: %v", err)
	}
}

// With ci, done needs CI green on the thread's pull requests: a failing
// check sends the next round, naming it; the same failure three rounds in
// a row ends the shift; green ends it done (ADR-0024).
func TestAShiftsDoneWaitsOnCI(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	head := strings.Repeat("a", 40)
	// A pass the shift's messages wake reads it meanwhile.
	var conclusion atomic.Value
	conclusion.Store("failure")
	heads := map[int64]string{9: head, 10: head, 11: strings.Repeat("b", 40)}
	h.engine.mu.Lock()
	h.engine.onGH = func(_ context.Context, args []string) (string, error) {
		call := strings.Join(args, " ")
		switch {
		case strings.Contains(call, "/commits/"+head+"/check-runs"):
			return `{"total_count":1,"check_runs":[{"name":"test","status":"completed","conclusion":"` + conclusion.Load().(string) + `"}]}`, nil
		case strings.Contains(call, "/commits/"+head+"/status"):
			return `{"total_count":0,"statuses":[]}`, nil
		}
		return ciGitHub(call, "frostyard/example", heads)
	}
	h.engine.mu.Unlock()
	thread := shiftThread(t, h, "s1", team.Shift{Stop: "the tests pass", MaxRounds: rounds(6), CI: true})
	// Done with no pull request is no done.
	h.answerRound(thread, thread.BriefSubmission, "STATUS: DONE")
	h.server.shiftPass(ctx)
	if sh := h.shift(thread); sh.Round != 2 || sh.Failures != 1 {
		t.Fatalf("done with no pull request %+v", sh)
	}
	if _, _, _, _, err := h.store.FileReportWithPulls(ctx, "pr", thread.ID, store.ThreadOpen, "PR", nil,
		[]store.PullRow{ciRow("frostyard/example", 9, head)},
		func([]int64, []store.PullRow) string { return "r" }); err != nil {
		t.Fatal(err)
	}
	for n := 2; n <= 4; n++ {
		h.answerRound(thread, h.submissionOf(shiftRoundKey(thread, n)), "STATUS: DONE")
		h.server.shiftPass(ctx)
	}
	sh := h.shift(thread)
	if sh.State != team.ShiftSummary || sh.Reason != team.ShiftFailedCheck || !strings.Contains(sh.Why, "frostyard/example#9 test") {
		t.Fatalf("after the same failure three times %+v", sh)
	}
	if sent := h.sentTo(thread.Session); !strings.Contains(strings.Join(sent, "\n"), "frostyard/example#9 test fail") {
		t.Fatalf("no round named the failing check: %q", sent)
	}

	conclusion.Store("success")
	green := shiftThread(t, h, "s2", team.Shift{Stop: "the tests pass", CI: true})
	if _, _, _, _, err := h.store.FileReportWithPulls(ctx, "pr2", green.ID, store.ThreadOpen, "PR", nil,
		[]store.PullRow{ciRow("frostyard/example", 10, head)},
		func([]int64, []store.PullRow) string { return "r" }); err != nil {
		t.Fatal(err)
	}
	h.answerRound(green, green.BriefSubmission, "STATUS: DONE")
	h.server.shiftPass(ctx)
	if sh := h.shift(green); sh.State != team.ShiftSummary || sh.Reason != team.ShiftDone {
		t.Fatalf("done with green CI %+v", sh)
	}

	// A pull request with no checks at all is no green CI.
	bare := shiftThread(t, h, "s3", team.Shift{Stop: "the tests pass", CI: true})
	if _, _, _, _, err := h.store.FileReportWithPulls(ctx, "pr3", bare.ID, store.ThreadOpen, "PR", nil,
		[]store.PullRow{ciRow("frostyard/example", 11, strings.Repeat("b", 40))},
		func([]int64, []store.PullRow) string { return "r" }); err != nil {
		t.Fatal(err)
	}
	h.engine.mu.Lock()
	onGH := h.engine.onGH
	h.engine.onGH = func(ctx context.Context, args []string) (string, error) {
		call := strings.Join(args, " ")
		switch {
		case strings.Contains(call, "/check-runs"):
			if !strings.Contains(call, "/commits/"+head+"/") {
				return `{"total_count":0,"check_runs":[]}`, nil
			}
		case strings.Contains(call, "/status"):
			if !strings.Contains(call, "/commits/"+head+"/") {
				return `{"total_count":0,"statuses":[]}`, nil
			}
		}
		return onGH(ctx, args)
	}
	h.engine.mu.Unlock()
	h.answerRound(bare, bare.BriefSubmission, "STATUS: DONE")
	h.server.shiftPass(ctx)
	if sh := h.shift(bare); sh.Round != 2 || sh.Failures != 1 {
		t.Fatalf("done with no checks %+v", sh)
	}
	if sent := h.sentTo(bare.Session); !strings.Contains(sent[len(sent)-1], "frostyard/example#11 has no checks") {
		t.Fatalf("the round after no checks %q", sent)
	}
}

// ciRow is open pull request number of repo at head sha, as a report
// links it.
func ciRow(repo string, number int64, sha string) store.PullRow {
	return store.PullRow{Repository: repo, Number: number, GitHubID: 1000 + number, Title: "Fix CI", URL: "https://github.com/" + repo + "/pull/" + itoa(number),
		State: store.PullOpen, Head: "h" + itoa(number), HeadSHA: sha, Base: "main"}
}

// ciGitHub answers gh's read of each open pull request of repo at its
// head in heads, and its merge base; anything else is not found.
func ciGitHub(call, repo string, heads map[int64]string) (string, error) {
	if strings.Contains(call, "repos/"+repo+"/compare/") {
		return `{"merge_base_commit":{"sha":"` + strings.Repeat("c", 40) + `"}}`, nil
	}
	for number, sha := range heads {
		if strings.HasSuffix(call, "repos/"+repo+"/pulls/"+itoa(number)) {
			row := ciRow(repo, number, sha)
			return `{"id":` + itoa(row.GitHubID) + `,"number":` + itoa(number) + `,"title":"Fix CI","html_url":"` + row.URL + `","state":"open","draft":false,"merged_at":null,"labels":[],` +
				`"head":{"ref":"` + row.Head + `","sha":"` + sha + `","repo":{"full_name":"` + repo + `"}},"base":{"ref":"main","repo":{"full_name":"` + repo + `"}}}`, nil
		}
	}
	return "", errors.New("gh: Not Found (HTTP 404)")
}

// Done is checked on CI at the head GitHub has now, read before CI: a
// head the specialist pushed after its report, whose checks fail, is no
// done, whatever the head pocketd recorded; a pull request GitHub cannot
// read leaves the decision to a later pass (Codex review of #36).
func TestAShiftsCIIsReadAtTheHeadPushedLast(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	old, pushed := strings.Repeat("a", 40), strings.Repeat("d", 40)
	heads := map[int64]string{9: pushed}
	var failRead atomic.Bool
	h.engine.mu.Lock()
	h.engine.onGH = func(_ context.Context, args []string) (string, error) {
		call := strings.Join(args, " ")
		switch {
		case failRead.Load() && strings.HasSuffix(call, "/pulls/9"):
			return "", errors.New("gh: HTTP 502")
		case strings.Contains(call, "/commits/"+old+"/check-runs"):
			return `{"total_count":1,"check_runs":[{"name":"test","status":"completed","conclusion":"success"}]}`, nil
		case strings.Contains(call, "/commits/"+pushed+"/check-runs"):
			return `{"total_count":1,"check_runs":[{"name":"lint","status":"completed","conclusion":"failure"}]}`, nil
		case strings.Contains(call, "/status"):
			return `{"total_count":0,"statuses":[]}`, nil
		}
		return ciGitHub(call, "frostyard/example", heads)
	}
	h.engine.mu.Unlock()
	thread := shiftThread(t, h, "s1", team.Shift{Stop: "the tests pass", CI: true})
	if _, _, _, _, err := h.store.FileReportWithPulls(ctx, "pr", thread.ID, store.ThreadOpen, "PR", nil, []store.PullRow{ciRow("frostyard/example", 9, old)},
		func([]int64, []store.PullRow) string { return "r" }); err != nil {
		t.Fatal(err)
	}
	failRead.Store(true)
	h.answerRound(thread, thread.BriefSubmission, "Pushed the fix.\nSTATUS: DONE")
	h.server.shiftPass(ctx)
	if sh := h.shift(thread); sh.Round != 1 || sh.State != team.ShiftRunning {
		t.Fatalf("a pull request GitHub could not read decided the round: %+v", sh)
	}
	failRead.Store(false)
	h.clock.Advance(shiftCIRecheck)
	h.server.shiftPass(ctx)
	sh := h.shift(thread)
	if sh.State != team.ShiftRunning || sh.Round != 2 || sh.Failures != 1 || sh.Fingerprint == "" {
		t.Fatalf("done on the old head's green CI: %+v", sh)
	}
	if sent := h.sentTo(thread.Session); !strings.Contains(sent[len(sent)-1], "frostyard/example#9 lint") {
		t.Fatalf("the next round does not name the failing check: %q", sent)
	}
}

// A plan's thread runs no shift, and a shift that breaks a rule is refused
// whole (ADR-0024).
func TestADelegationsShiftIsChecked(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	for name, tc := range map[string]struct {
		d    team.Delegation
		want string
	}{
		"no stop":   {team.Delegation{Agent: "duncan", Task: "x", Shift: &team.Shift{}}, "stop condition is empty"},
		"too long":  {team.Delegation{Agent: "duncan", Task: "x", Shift: &team.Shift{Stop: "x", Hours: hours(30)}}, "at most 24"},
		"no rounds": {team.Delegation{Agent: "duncan", Task: "x", Shift: &team.Shift{Stop: "x", MaxRounds: rounds(0)}}, "max_rounds is 0"},
		"on a plan": {team.Delegation{Agent: "duncan", Task: "x", Plan: &team.PlanRef{Start: true}, Shift: &team.Shift{Stop: "x"}}, "runs no shift"},
	} {
		if _, err := h.server.TeamDelegate(ctx, TeamCall{Session: h.super.ID, CallID: "bad-" + name}, tc.d); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want %q", name, err, tc.want)
		}
	}
	if threads, _ := h.store.Threads(ctx); len(threads) != 0 {
		t.Fatalf("a refused shift recorded %+v", threads)
	}
}

// summaryReached places the summary round's message in the specialist's
// transcript: its report answers it.
func (h *teamHarness) summaryReached(thread store.ThreadRow) {
	h.t.Helper()
	sh := h.shift(thread)
	h.answerRound(thread, h.submissionOf(shiftSummaryKey(thread, sh.Round)), "")
}

// failTo has every send to session fail, or none with 0.
func (h *teamHarness) failTo(session int64) {
	h.engine.mu.Lock()
	defer h.engine.mu.Unlock()
	h.engine.failTo = map[int64]bool{session: session != 0}
}

// lastThread is the last event of thread id.
func (h *teamHarness) lastThread(id int64) proto.Thread {
	h.t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	for i := len(h.events) - 1; i >= 0; i-- {
		if e := h.events[i]; e.Type == proto.EventThread && e.Thread.ID == id {
			return *e.Thread
		}
	}
	h.t.Fatalf("no event of thread %d", id)
	return proto.Thread{}
}

// A round's message that has not reached the specialist when the owner
// stops or pauses the shift goes to no one: it is no longer the round's
// (Codex review of #36).
func TestAStoppedOrPausedShiftsRoundMessageGoesToNoOne(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	stopped := shiftThread(t, h, "s1", team.Shift{Stop: "the tests pass", MaxRounds: rounds(5)})
	h.failTo(stopped.Session)
	h.answerRound(stopped, stopped.BriefSubmission, "STATUS: CONTINUE")
	h.server.shiftPass(ctx)
	if sh := h.shift(stopped); sh.Round != 2 {
		t.Fatalf("round 2 %+v", sh)
	}
	if _, err := h.client.StopShift(ctx, stopped.ID, proto.StopShift{RequestID: "x1"}); err != nil {
		t.Fatal(err)
	}
	h.failTo(0)
	if err := h.server.DeliverPending(ctx); err != nil {
		t.Fatal(err)
	}
	for _, text := range h.sentTo(stopped.Session) {
		if strings.Contains(text, "Round 2 of 5") {
			t.Fatalf("a stopped shift's round message went: %q", text)
		}
	}
	if row, err := h.store.Report(ctx, shiftRoundKey(stopped, 2)); err != nil || row.SentAt == nil {
		t.Fatalf("the stopped round's message is still owed: %+v, %v", row, err)
	}

	paused := shiftThread(t, h, "s2", team.Shift{Stop: "the tests pass", MaxRounds: rounds(5)})
	h.failTo(paused.Session)
	h.answerRound(paused, paused.BriefSubmission, "STATUS: CONTINUE")
	h.server.shiftPass(ctx)
	// The owner's message, as its record pauses the shift.
	if _, _, ok, err := h.store.ClaimSend(ctx, "o1", "send:"+itoa(paused.Session), paused.ID, "the owner wrote"); err != nil || !ok {
		t.Fatal(ok, err)
	}
	h.failTo(0)
	if err := h.server.DeliverPending(ctx); err != nil {
		t.Fatal(err)
	}
	for _, text := range h.sentTo(paused.Session) {
		if strings.Contains(text, "Round 2 of 5") {
			t.Fatalf("a paused shift's round message went: %q", text)
		}
	}
}

// The summary round's report ends the shift as it is filed: the thread's
// event carries the report and the ended shift, and a stop that comes
// after files nothing more. A report with another status than the stop
// asks for is refused (Codex review of #36).
func TestASummarysReportEndsTheShiftOnce(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	thread := shiftThread(t, h, "s1", team.Shift{Stop: "deployed"})
	h.answerRound(thread, thread.BriefSubmission, "No key.\nSTATUS: BLOCKED: needs the deploy key")
	h.server.shiftPass(ctx)
	h.summaryReached(thread)
	if _, err := h.server.TeamMessage(ctx, TeamCall{Session: thread.Session, CallID: "r1"}, team.Report{Status: team.StatusDone, Message: "Deployed."}); err == nil ||
		!strings.Contains(err.Error(), "report with status blocked") {
		t.Fatalf("done for a blocked shift: %v", err)
	}
	if _, err := h.server.TeamMessage(ctx, TeamCall{Session: thread.Session, CallID: "r2"}, team.Report{Status: team.StatusBlocked, Message: "Needs the key."}); err != nil {
		t.Fatal(err)
	}
	last := h.lastThread(thread.ID)
	if last.State != proto.ThreadBlocked || last.Report != "Needs the key." || last.Shift == nil || last.Shift.State != proto.ShiftEnded {
		t.Fatalf("the thread's last event %+v (shift %+v)", last, last.Shift)
	}
	stopped, err := h.client.StopShift(ctx, thread.ID, proto.StopShift{RequestID: "x1"})
	if err != nil || stopped.Shift.Reason != team.ShiftBlocked {
		t.Fatalf("a stop after the report %+v, %v", stopped.Shift, err)
	}
	if reports := h.reportsTo(thread); len(reports) != 1 || !strings.Contains(reports[0], "Needs the key.") {
		t.Fatalf("the supervisor's reports %q", reports)
	}
}

// A round whose message PiG placed before pocketd noted its submission,
// as a stop between the two leaves it, ends all the same: the round is
// found by its message's request key. Its placement, once noted, starts a
// pass, which ends the round (Codex review of #36).
func TestARoundBeginsWhereItsMessageWasPlaced(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	thread := shiftThread(t, h, "s1", team.Shift{Stop: "the tests pass"})
	h.runShiftPasses()
	h.failTo(thread.Session)
	h.answerRound(thread, thread.BriefSubmission, "STATUS: CONTINUE")
	h.server.shiftPass(ctx)
	h.failTo(0)
	if n := h.runShiftPasses(); n != 0 {
		t.Fatalf("%d passes with no message placed", n)
	}
	// PiG has the message, and its run answered, before pocketd noted it.
	sub, err := h.engine.Send(ctx, thread.Session, shiftRoundKey(thread, 2)+":report", "round 2")
	if err != nil {
		t.Fatal(err)
	}
	h.answerRound(thread, sub, "All green.\nSTATUS: DONE")
	if err := h.server.DeliverPending(ctx); err != nil {
		t.Fatal(err)
	}
	if row, err := h.store.Report(ctx, shiftRoundKey(thread, 2)); err != nil || row.SentAt == nil || row.Submission != 0 {
		t.Fatalf("the round's message %+v, %v", row, err)
	}
	if n := h.runShiftPasses(); n == 0 {
		t.Fatal("the message's placement started no pass")
	}
	if sh := h.shift(thread); sh.State != team.ShiftSummary {
		t.Fatalf("the round did not end: %+v", sh)
	}
}

// A round is not over while its worker's run ended with a reply pocketd
// has not kept yet, as at a restart before the run's end was followed, nor
// while an answer to its ask has not reached the specialist (Codex review
// of #36).
func TestARoundWaitsForItsWorkersReplyAndItsAnswers(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	h.engine.mu.Lock()
	h.engine.keyModels = []proto.Model{{ModelRef: deepseekFlash, Name: "DeepSeek V4.1 Flash"}}
	h.engine.mu.Unlock()
	thread := shiftThread(t, h, "s1", team.Shift{Stop: "the tests pass"})
	if _, err := h.server.TeamWorker(ctx, TeamCall{Session: thread.Session, Task: 7, CallID: "w1"}, team.WorkerMessage{Message: "Fix the flaky test."}); err != nil {
		t.Fatal(err)
	}
	thread, _ = h.store.Thread(ctx, thread.ID)
	h.engine.mu.Lock()
	brief := h.engine.sent[workerMessagePrefix+TeamCall{Session: thread.Session, Task: 7, CallID: "w1"}.key()+":report"]
	if h.engine.transcripts == nil {
		h.engine.transcripts = map[int64][]proto.Entry{}
	}
	h.engine.transcripts[thread.WorkerSession] = []proto.Entry{
		{ID: 1, SessionID: thread.WorkerSession, Kind: proto.EntryUser, SubmissionID: brief, Text: "Fix the flaky test."},
		{ID: 2, SessionID: thread.WorkerSession, Kind: proto.EntryAssistant, Text: "Fixed it in ci_test.go."},
	}
	h.engine.mu.Unlock()
	h.answerRound(thread, thread.BriefSubmission, "Done, I think.\nSTATUS: DONE")
	h.server.shiftPass(ctx)
	if sh := h.shift(thread); sh.State != team.ShiftRunning || sh.Round != 1 {
		t.Fatalf("a round ended before its worker's reply: %+v", sh)
	}
	if sent := h.sentTo(thread.Session); !strings.Contains(sent[len(sent)-1], "Fixed it in ci_test.go.") {
		t.Fatalf("the worker's reply did not go: %q", sent)
	}

	asking := shiftThread(t, h, "s2", team.Shift{Stop: "the tests pass"})
	if _, err := h.server.TeamMessage(ctx, TeamCall{Session: asking.Session, CallID: "w1"}, team.Report{Status: team.StatusWorking, Message: "A question.",
		Asks: []team.Ask{{Question: "Which fixture?"}}}); err != nil {
		t.Fatal(err)
	}
	asks, err := h.client.Asks(ctx)
	if err != nil || len(asks) != 1 {
		t.Fatal(asks, err)
	}
	h.failTo(asking.Session)
	if _, err := h.client.AnswerAsk(ctx, asks[0].ID, proto.AnswerAsk{RequestID: "a1", Answer: "the new one"}); err != nil {
		t.Fatal(err)
	}
	h.answerRound(asking, asking.BriefSubmission, "Asked.\nSTATUS: CONTINUE")
	h.server.shiftPass(ctx)
	if sh := h.shift(asking); sh.Round != 1 {
		t.Fatalf("a round ended before its answer reached the specialist: %+v", sh)
	}
	h.failTo(0)
	if err := h.server.DeliverPending(ctx); err != nil {
		t.Fatal(err)
	}
	h.server.shiftPass(ctx)
	if sh := h.shift(asking); sh.Round != 2 {
		t.Fatalf("a round with its answer delivered did not end: %+v", sh)
	}
}

// A shift whose specialist does not have its brief yet is neither stopped
// nor resumed: its round 1 has not begun (Codex review of #36).
func TestAShiftBeforeItsBriefIsNotChanged(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	// The specialist is busy: its brief waits in its queue.
	h.engine.mu.Lock()
	h.engine.runFirst = 1
	h.engine.queued, h.engine.withdrawn = map[int64]bool{}, map[int64]bool{}
	h.engine.onSend = func(int64, string) {
		h.engine.mu.Lock()
		h.engine.queued[int64(len(h.engine.sent)+100)] = true
		h.engine.mu.Unlock()
	}
	h.engine.mu.Unlock()
	call := TeamCall{Session: h.super.ID, CallID: "s1"}
	if _, err := h.server.TeamDelegate(ctx, call, team.Delegation{Agent: "duncan", Task: "Fix the flaky tests.", Shift: &team.Shift{Stop: "the tests pass"}}); err != nil {
		t.Fatal(err)
	}
	thread, err := h.store.ThreadByKey(ctx, call.key())
	if err != nil || thread.Briefed || thread.State == store.ThreadClosed {
		t.Fatalf("the thread %+v, %v", thread, err)
	}
	var pe *proto.Error
	if _, err := h.client.StopShift(ctx, thread.ID, proto.StopShift{RequestID: "x1"}); !errors.As(err, &pe) || pe.Status != http.StatusConflict ||
		!strings.Contains(pe.Message, "does not have its brief yet") {
		t.Fatalf("a stop before the brief: %v", err)
	}
	if sh := h.shift(thread); sh.State != team.ShiftRunning {
		t.Fatalf("the shift %+v", sh)
	}
}

// The owner's message pauses a shift once: the same message tried again,
// after the owner resumed the shift, pauses nothing (Codex review of #36).
func TestTheOwnersMessageTriedAgainPausesNothing(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	thread := shiftThread(t, h, "s1", team.Shift{Stop: "the tests pass", MaxRounds: rounds(5)})
	if _, err := h.client.Send(ctx, thread.Session, proto.SendMessage{RequestID: "o1", Text: "Hold on."}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.client.ResumeShift(ctx, thread.ID, proto.ResumeShift{RequestID: "r1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.client.Send(ctx, thread.Session, proto.SendMessage{RequestID: "o1", Text: "Hold on."}); err != nil {
		t.Fatal(err)
	}
	if sh := h.shift(thread); sh.State != team.ShiftRunning || sh.Round != 2 {
		t.Fatalf("the message tried again paused the shift: %+v", sh)
	}
}

// The CI failure streak counts done rounds in a row: a round between that
// is no done ends it; the third failure in a row is counted in the
// summary. A deadline ends a wait on CI that does not finish (Codex review
// of #36).
func TestTheCIStreakIsInARowAndCIWaitsUntilTheDeadline(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	head, pending := strings.Repeat("a", 40), strings.Repeat("e", 40)
	h.engine.mu.Lock()
	h.engine.onGH = func(_ context.Context, args []string) (string, error) {
		call := strings.Join(args, " ")
		switch {
		case strings.Contains(call, "/commits/"+head+"/check-runs"):
			return `{"total_count":1,"check_runs":[{"name":"test","status":"completed","conclusion":"failure"}]}`, nil
		case strings.Contains(call, "/commits/"+pending+"/check-runs"):
			return `{"total_count":1,"check_runs":[{"name":"test","status":"in_progress","conclusion":null}]}`, nil
		case strings.Contains(call, "/status"):
			return `{"total_count":0,"statuses":[]}`, nil
		}
		return ciGitHub(call, "frostyard/example", map[int64]string{9: head, 12: pending})
	}
	h.engine.mu.Unlock()
	thread := shiftThread(t, h, "s1", team.Shift{Stop: "the tests pass", MaxRounds: rounds(9), CI: true})
	if _, _, _, _, err := h.store.FileReportWithPulls(ctx, "pr", thread.ID, store.ThreadOpen, "PR", nil, []store.PullRow{ciRow("frostyard/example", 9, head)},
		func([]int64, []store.PullRow) string { return "r" }); err != nil {
		t.Fatal(err)
	}
	h.answerRound(thread, thread.BriefSubmission, "STATUS: DONE")
	h.server.shiftPass(ctx)
	for _, step := range []struct {
		n      int
		answer string
	}{{2, "STATUS: CONTINUE"}, {3, "STATUS: DONE"}, {4, "STATUS: DONE"}} {
		h.answerRound(thread, h.submissionOf(shiftRoundKey(thread, step.n)), step.answer)
		h.server.shiftPass(ctx)
	}
	if sh := h.shift(thread); sh.State != team.ShiftRunning || sh.Round != 5 || sh.Failures != 2 {
		t.Fatalf("a failure streak with a continue between %+v", sh)
	}
	h.answerRound(thread, h.submissionOf(shiftRoundKey(thread, 5)), "STATUS: DONE")
	h.server.shiftPass(ctx)
	if sh := h.shift(thread); sh.State != team.ShiftSummary || sh.Reason != team.ShiftFailedCheck || sh.Failures != 3 {
		t.Fatalf("the third failure in a row %+v", sh)
	}

	timed := shiftThread(t, h, "s2", team.Shift{Stop: "the tests pass", Hours: hours(1), CI: true})
	if _, _, _, _, err := h.store.FileReportWithPulls(ctx, "pr2", timed.ID, store.ThreadOpen, "PR", nil, []store.PullRow{ciRow("frostyard/example", 12, pending)},
		func([]int64, []store.PullRow) string { return "r" }); err != nil {
		t.Fatal(err)
	}
	h.answerRound(timed, timed.BriefSubmission, "STATUS: DONE")
	h.server.shiftPass(ctx)
	if sh := h.shift(timed); sh.State != team.ShiftRunning || sh.Round != 1 {
		t.Fatalf("pending CI decided the round: %+v", sh)
	}
	h.clock.Advance(2 * time.Hour)
	h.server.shiftPass(ctx)
	if sh := h.shift(timed); sh.State != team.ShiftSummary || sh.Reason != team.ShiftDeadline || !strings.Contains(sh.Why, "CI had not finished") {
		t.Fatalf("CI pending past the deadline %+v", sh)
	}
}

// A round's answer is read from its message on, and not at all while the
// specialist is at work (Codex review of #36).
func TestARoundReadsOnlyItsOwnEntries(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	thread := shiftThread(t, h, "s1", team.Shift{Stop: "the tests pass", MaxRounds: rounds(5)})
	h.answerRound(thread, thread.BriefSubmission, "STATUS: CONTINUE")
	h.server.shiftPass(ctx)
	// Round 2's message is in the transcript, and its run goes on.
	h.engine.mu.Lock()
	h.engine.runFirst = 1
	reads := len(h.engine.readAfter)
	h.engine.mu.Unlock()
	h.answerRound(thread, h.submissionOf(shiftRoundKey(thread, 2)), "")
	h.server.shiftPass(ctx)
	h.engine.mu.Lock()
	busyReads := len(h.engine.readAfter) - reads
	h.engine.mu.Unlock()
	if busyReads != 0 {
		t.Fatalf("a busy specialist's transcript was read %d times", busyReads)
	}
	if sh := h.shift(thread); sh.Round != 2 {
		t.Fatalf("a round at work ended: %+v", sh)
	}
	anchor, ok, err := h.engine.PlacedEntry(ctx, thread.Session, shiftRoundKey(thread, 2)+":report")
	if err != nil || !ok || anchor < 2 {
		t.Fatal(anchor, ok, err)
	}
	h.engine.mu.Lock()
	h.engine.runFirst = 0
	h.engine.transcripts[thread.Session] = append(h.engine.transcripts[thread.Session],
		proto.Entry{ID: anchor + 1, SessionID: thread.Session, Kind: proto.EntryAssistant, Text: "STATUS: CONTINUE"})
	h.engine.mu.Unlock()
	h.server.shiftPass(ctx)
	h.engine.mu.Lock()
	last := h.engine.readAfter[len(h.engine.readAfter)-1]
	h.engine.mu.Unlock()
	if last != anchor {
		t.Fatalf("the round read from entry %d, not its message's %d", last, anchor)
	}
	if sh := h.shift(thread); sh.Round != 3 {
		t.Fatalf("round 3 %+v", sh)
	}
}

// queueRoundMessage has the specialist busy and its round n message wait
// in its queue once sent: PiG places it when the run ends, whatever
// pocketd does meanwhile.
func (h *teamHarness) queueRoundMessage(thread store.ThreadRow, n int) {
	h.engine.mu.Lock()
	defer h.engine.mu.Unlock()
	if h.engine.queued == nil {
		h.engine.queued = map[int64]bool{}
	}
	key := shiftRoundKey(thread, n) + ":report"
	h.engine.onSend = func(_ int64, sent string) {
		if sent != key {
			return
		}
		h.engine.mu.Lock()
		defer h.engine.mu.Unlock()
		h.engine.runFirst = 1
		h.engine.queued[int64(len(h.engine.sent)+100)] = true // the id Send gives it
	}
}

// A round message still queued in PiG when the owner stops or pauses the
// shift is withdrawn before the change commits, so no run places it after
// (Codex review of #36).
func TestAQueuedRoundMessageIsWithdrawnBeforeAStopOrPause(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	for _, how := range []string{"stop", "pause"} {
		thread := shiftThread(t, h, how, team.Shift{Stop: "the tests pass", MaxRounds: rounds(5)})
		h.queueRoundMessage(thread, 2)
		h.answerRound(thread, thread.BriefSubmission, "STATUS: CONTINUE")
		h.server.shiftPass(ctx)
		key := shiftRoundKey(thread, 2) + ":report"
		h.engine.mu.Lock()
		sub := h.engine.sent[key]
		queued := h.engine.queued[sub]
		h.engine.mu.Unlock()
		if !queued {
			t.Fatalf("%s: round 2's message is not queued", how)
		}
		// The shift as the queued message is withdrawn: still running, so
		// the withdrawal comes before the change commits.
		var at string
		h.engine.mu.Lock()
		h.engine.onWithdraw = func(withdrawing string) {
			if withdrawing == key && at == "" {
				if sh, err := h.store.Shift(ctx, thread.ID); err == nil {
					at = sh.State
				}
			}
		}
		h.engine.mu.Unlock()
		if how == "stop" {
			if _, err := h.client.StopShift(ctx, thread.ID, proto.StopShift{RequestID: "x-" + how}); err != nil {
				t.Fatal(err)
			}
		} else if _, err := h.client.Send(ctx, thread.Session, proto.SendMessage{RequestID: "o-" + how, Text: "Hold on."}); err != nil {
			t.Fatal(err)
		}
		h.engine.mu.Lock()
		withdrawn := h.engine.withdrawn[sub]
		h.engine.runFirst, h.engine.onSend, h.engine.onWithdraw = 0, nil, nil
		h.engine.mu.Unlock()
		if !withdrawn || at != team.ShiftRunning {
			t.Fatalf("%s: the queued round message was withdrawn %v, with the shift %q", how, withdrawn, at)
		}
		if err := h.server.DeliverPending(ctx); err != nil {
			t.Fatal(err)
		}
		h.engine.mu.Lock()
		_, again := h.engine.sent[attemptKey(key, 1)]
		h.engine.mu.Unlock()
		if again {
			t.Fatalf("%s: the withdrawn round message was sent again", how)
		}
	}
}

// The owner's message pauses the shift before it goes, so a message PiG
// has never leaves its shift running; one that fails frees its request id
// and leaves the shift paused (Codex review of #36).
func TestTheOwnersMessagePausesBeforeItGoes(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	thread := shiftThread(t, h, "s1", team.Shift{Stop: "the tests pass"})
	var at string
	h.engine.mu.Lock()
	h.engine.onSend = func(_ int64, key string) {
		if strings.HasSuffix(key, "o1") {
			if sh, err := h.store.Shift(ctx, thread.ID); err == nil {
				at = sh.State
			}
		}
	}
	h.engine.mu.Unlock()
	h.failTo(thread.Session)
	if _, err := h.client.Send(ctx, thread.Session, proto.SendMessage{RequestID: "o1", Text: "Hold on."}); err == nil {
		t.Fatal("a send that failed succeeded")
	}
	if at != team.ShiftPaused {
		t.Fatalf("the shift was %q as the message went", at)
	}
	if sh := h.shift(thread); sh.State != team.ShiftPaused {
		t.Fatalf("the shift after a failed message %+v", sh)
	}
	h.failTo(0)
	if _, err := h.client.Send(ctx, thread.Session, proto.SendMessage{RequestID: "o1", Text: "Hold on."}); err != nil {
		t.Fatalf("the message tried again: %v", err)
	}
}

// A linked pull request recorded closed is read again: one reopened with
// failing CI is no done, whatever another's green says (Codex review of
// #36).
func TestAReopenedPullRequestCountsForCI(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	green, red := strings.Repeat("a", 40), strings.Repeat("f", 40)
	h.engine.mu.Lock()
	h.engine.onGH = func(_ context.Context, args []string) (string, error) {
		call := strings.Join(args, " ")
		switch {
		case strings.Contains(call, "/commits/"+green+"/check-runs"):
			return `{"total_count":1,"check_runs":[{"name":"test","status":"completed","conclusion":"success"}]}`, nil
		case strings.Contains(call, "/commits/"+red+"/check-runs"):
			return `{"total_count":1,"check_runs":[{"name":"lint","status":"completed","conclusion":"failure"}]}`, nil
		case strings.Contains(call, "/status"):
			return `{"total_count":0,"statuses":[]}`, nil
		}
		return ciGitHub(call, "frostyard/example", map[int64]string{9: green, 13: red})
	}
	h.engine.mu.Unlock()
	thread := shiftThread(t, h, "s1", team.Shift{Stop: "the tests pass", CI: true})
	closed := ciRow("frostyard/example", 13, red)
	closed.State = store.PullClosed
	if _, _, _, _, err := h.store.FileReportWithPulls(ctx, "pr", thread.ID, store.ThreadOpen, "PRs", nil, []store.PullRow{ciRow("frostyard/example", 9, green), closed},
		func([]int64, []store.PullRow) string { return "r" }); err != nil {
		t.Fatal(err)
	}
	h.answerRound(thread, thread.BriefSubmission, "STATUS: DONE")
	h.server.shiftPass(ctx)
	if sh := h.shift(thread); sh.State != team.ShiftRunning || sh.Round != 2 || !strings.Contains(sh.Fingerprint, "#13 lint") {
		t.Fatalf("done with a reopened pull request failing: %+v", sh)
	}
}

// A stop of a shift that ended already succeeds, and claims its request:
// tried again after the owner resumed the shift, it stops nothing (Codex
// review of #36).
func TestAStopOfAnEndedShiftIsClaimed(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	thread := shiftThread(t, h, "s1", team.Shift{Stop: "deployed", MaxRounds: rounds(5)})
	h.answerRound(thread, thread.BriefSubmission, "STATUS: BLOCKED: needs the key")
	h.server.shiftPass(ctx)
	h.summaryReached(thread)
	if _, err := h.server.TeamMessage(ctx, TeamCall{Session: thread.Session, CallID: "r1"}, team.Report{Status: team.StatusBlocked, Message: "Needs the key."}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.client.StopShift(ctx, thread.ID, proto.StopShift{RequestID: "x1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.client.ResumeShift(ctx, thread.ID, proto.ResumeShift{RequestID: "r1"}); err != nil {
		t.Fatal(err)
	}
	again, err := h.client.StopShift(ctx, thread.ID, proto.StopShift{RequestID: "x1"})
	if err != nil || again.Shift.State != proto.ShiftRunning {
		t.Fatalf("the stop tried again %+v, %v", again.Shift, err)
	}
}

// A done whose CI GitHub cannot read, or that has not finished, stops at
// the shift's deadline, by a wake of its own, with no pull request round
// or run's end to start a pass; each CI read is bounded (Codex review of
// #36).
func TestCIThatCannotDecideStopsAtTheDeadline(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	pending := strings.Repeat("e", 40)
	var unbounded atomic.Bool
	var failReads atomic.Bool
	h.engine.mu.Lock()
	h.engine.onGH = func(ctx context.Context, args []string) (string, error) {
		call := strings.Join(args, " ")
		switch {
		case strings.Contains(call, "/check-runs") || strings.Contains(call, "/status"):
			if _, ok := ctx.Deadline(); !ok {
				unbounded.Store(true)
			}
		}
		switch {
		case failReads.Load() && strings.HasSuffix(call, "/pulls/14"):
			return "", errors.New("gh: HTTP 401")
		case strings.Contains(call, "/commits/"+pending+"/check-runs"):
			return `{"total_count":1,"check_runs":[{"name":"test","status":"in_progress","conclusion":null}]}`, nil
		case strings.Contains(call, "/status"):
			return `{"total_count":0,"statuses":[]}`, nil
		}
		return ciGitHub(call, "frostyard/example", map[int64]string{12: pending, 14: pending})
	}
	h.engine.mu.Unlock()
	for _, tc := range []struct {
		number int64
		fail   bool
		why    string
	}{{12, false, "CI had not finished"}, {14, true, "GitHub could not be read for CI"}} {
		h.runShiftPasses()
		failReads.Store(tc.fail)
		thread := shiftThread(t, h, "s"+itoa(tc.number), team.Shift{Stop: "the tests pass", Hours: hours(1), CI: true})
		failReads.Store(false)
		if _, _, _, _, err := h.store.FileReportWithPulls(ctx, "pr"+itoa(tc.number), thread.ID, store.ThreadOpen, "PR", nil, []store.PullRow{ciRow("frostyard/example", tc.number, pending)},
			func([]int64, []store.PullRow) string { return "r" }); err != nil {
			t.Fatal(err)
		}
		failReads.Store(tc.fail)
		h.answerRound(thread, thread.BriefSubmission, "STATUS: DONE")
		h.server.shiftPass(ctx)
		h.runShiftPasses()
		if sh := h.shift(thread); sh.State != team.ShiftRunning || sh.Round != 1 {
			t.Fatalf("%d: CI that cannot decide decided the round: %+v", tc.number, sh)
		}
		h.clock.Advance(2 * time.Hour)
		h.fireShiftWakes()
		h.runShiftPasses()
		if sh := h.shift(thread); sh.State != team.ShiftSummary || sh.Reason != team.ShiftDeadline || !strings.Contains(sh.Why, tc.why) {
			t.Fatalf("%d: past the deadline %+v", tc.number, sh)
		}
	}
	if unbounded.Load() {
		t.Fatal("a CI read had no deadline")
	}
}

// A summary report checked in the summary round, whose filing a stop
// overtook, files nothing: the stop's report is the one (Codex review of
// #36).
func TestASummaryReportOvertakenByAStopFilesNothing(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	thread := shiftThreadOf(t, h, "vera", "frostyard/example", "s1", team.Shift{Stop: "the tests pass"})
	h.answerRound(thread, thread.BriefSubmission, "STATUS: DONE")
	h.server.shiftPass(ctx)
	stopped := false
	h.engine.mu.Lock()
	h.engine.onGH = func(_ context.Context, args []string) (string, error) {
		call := strings.Join(args, " ")
		switch {
		case strings.HasSuffix(call, "/pulls/9"):
			if !stopped {
				stopped = true
				// The owner stops the shift while the report's pull request
				// is read.
				if _, err := h.client.StopShift(ctx, thread.ID, proto.StopShift{RequestID: "x1"}); err != nil {
					t.Error(err)
				}
			}
			return githubPull("frostyard/example", 9, thread.Branch, "frostyard/example", "open", false), nil
		case strings.Contains(call, "/compare/"):
			return `{"merge_base_commit":{"sha":"` + strings.Repeat("c", 40) + `"}}`, nil
		}
		return "", errors.New("gh: Not Found (HTTP 404)")
	}
	h.engine.mu.Unlock()
	h.summaryReached(thread)
	_, err := h.server.TeamMessage(ctx, TeamCall{Session: thread.Session, CallID: "r1"}, team.Report{Status: team.StatusDone, Message: "All done.",
		PullRequests: []team.PullRef{{Number: 9}}})
	if !stopped {
		t.Fatalf("the report read no pull request: %v", err)
	}
	if err == nil || !strings.Contains(err.Error(), "not in the summary round") {
		t.Fatalf("a summary report after the stop: %v", err)
	}
	if reports := h.reportsTo(thread); len(reports) != 1 || !strings.Contains(reports[0], "the owner stopped it") {
		t.Fatalf("the supervisor's reports %q", reports)
	}
}

// A round's answer is its last run's: a run after a worker's reply that
// failed with no text ends the shift failed, whatever an earlier run said
// (Codex review of #36).
func TestAFailedFollowUpRunIsNoDone(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	thread := shiftThread(t, h, "s1", team.Shift{Stop: "the tests pass"})
	h.answerRound(thread, thread.BriefSubmission, "STATUS: DONE")
	h.engine.mu.Lock()
	entries := h.engine.transcripts[thread.Session]
	next := entries[len(entries)-1].ID + 1
	h.engine.transcripts[thread.Session] = append(entries, proto.Entry{ID: next, SessionID: thread.Session, Kind: proto.EntryUser, Text: "[T1 · worker] done"})
	if h.engine.transcriptLive == nil {
		h.engine.transcriptLive = map[int64]proto.Live{}
	}
	h.engine.transcriptLive[thread.Session] = proto.Live{Failure: "model_error: 500"}
	h.engine.mu.Unlock()
	h.server.shiftPass(ctx)
	if sh := h.shift(thread); sh.State != team.ShiftEnded || sh.Reason != team.ShiftFailed {
		t.Fatalf("a failed run after a done %+v", sh)
	}
}

// The worker a round waits on is the thread's as it is once the specialist
// is idle: one it started in the run that just ended holds the round
// (Codex review of #36).
func TestARoundSeesAWorkerStartedInItsLastRun(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	h.engine.mu.Lock()
	h.engine.keyModels = []proto.Model{{ModelRef: deepseekFlash, Name: "DeepSeek V4.1 Flash"}}
	h.engine.mu.Unlock()
	thread := shiftThread(t, h, "s1", team.Shift{Stop: "the tests pass"})
	h.answerRound(thread, thread.BriefSubmission, "Started my worker.\nSTATUS: DONE")
	started := false
	h.engine.mu.Lock()
	h.engine.onBusy = func(session int64) {
		if session != thread.Session || started {
			return
		}
		started = true
		// The run that just ended started a worker, now at work.
		if _, err := h.server.TeamWorker(ctx, TeamCall{Session: thread.Session, Task: 7, CallID: "w1"}, team.WorkerMessage{Message: "Fix it."}); err != nil {
			t.Error(err)
		}
		th, _ := h.store.Thread(ctx, thread.ID)
		h.engine.mu.Lock()
		h.engine.busy = map[int64]bool{th.WorkerSession: true}
		h.engine.mu.Unlock()
	}
	h.engine.mu.Unlock()
	h.server.shiftPass(ctx)
	if !started {
		t.Fatal("no worker started")
	}
	if sh := h.shift(thread); sh.State != team.ShiftRunning || sh.Round != 1 {
		t.Fatalf("a round ended while its new worker works: %+v", sh)
	}
}

// A thread that closes while its shift runs tells its supervisor why the
// shift ended (Codex review of #36).
func TestAClosedShiftsSupervisorIsTold(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	thread := shiftThread(t, h, "s1", team.Shift{Stop: "the tests pass"})
	if _, err := h.client.CloseThread(ctx, thread.ID, proto.CloseThread{RequestID: "c1"}); err != nil {
		t.Fatal(err)
	}
	if err := h.server.DeliverPending(ctx); err != nil {
		t.Fatal(err)
	}
	if sent := strings.Join(h.sentTo(h.super.ID), "\n"); !strings.Contains(sent, "The shift ended: its thread closed") {
		t.Fatalf("the supervisor got %q", sent)
	}
}

// pullAt is GitHub's answer for open pull request number of repo, from
// branch at head sha, with labels.
func pullAt(repo string, number int64, branch, sha string, labels ...string) string {
	named := make([]map[string]string, len(labels))
	for i, l := range labels {
		named[i] = map[string]string{"name": l}
	}
	out, _ := json.Marshal(map[string]any{"id": 1000 + number, "number": number, "title": "Fix CI", "html_url": "https://github.com/" + repo + "/pull/" + itoa(number),
		"state": "open", "draft": false, "merged_at": nil, "labels": named,
		"head": map[string]any{"ref": branch, "sha": sha, "repo": map[string]any{"full_name": repo}},
		"base": map[string]any{"ref": "main", "repo": map[string]any{"full_name": repo}}})
	return string(out)
}

// fileFor keeps a message for session filed by another thread, as a
// cross-review files its verdict for the author's session.
func (h *teamHarness) fileFor(session int64, key, text string) {
	h.t.Helper()
	other := shiftThread(h.t, h, "other-"+key, team.Shift{Stop: "x"})
	if _, err := h.store.QueueWorkerReport(context.Background(), store.ReportRow{Key: key, ThreadID: other.ID, SessionID: session, Text: text}, 0); err != nil {
		h.t.Fatal(err)
	}
}

// What is owed to the specialist holds its round whatever thread filed
// it, a cross-review's verdict included; the owner's stop, which ends the
// shift only, leaves it, and an answer to its ask, to go (Codex review of
// #36; the owner's choice, 2026-11-20).
func TestWhatIsOwedToTheSpecialistIsOwedByItsSession(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	thread := shiftThread(t, h, "s1", team.Shift{Stop: "the tests pass"})
	h.failTo(thread.Session)
	h.fileFor(thread.Session, "verdict-1", "[review] Changes requested: the test still flakes.")
	h.answerRound(thread, thread.BriefSubmission, "STATUS: DONE")
	h.server.shiftPass(ctx)
	if sh := h.shift(thread); sh.State != team.ShiftRunning || sh.Round != 1 {
		t.Fatalf("a round ended before a verdict owed to its specialist went: %+v", sh)
	}
	h.failTo(0)
	if _, err := h.server.TeamMessage(ctx, TeamCall{Session: thread.Session, CallID: "w1"}, team.Report{Status: team.StatusWorking, Message: "A question.",
		Asks: []team.Ask{{Question: "Which fixture?"}}}); err != nil {
		t.Fatal(err)
	}
	asks, _ := h.client.Asks(ctx)
	h.failTo(thread.Session)
	if _, err := h.client.AnswerAsk(ctx, asks[0].ID, proto.AnswerAsk{RequestID: "a1", Answer: "the new one"}); err != nil {
		t.Fatal(err)
	}
	// The stop ends the shift only: what pocketd owes the specialist still
	// goes (the owner's choice, 2026-11-20).
	if _, err := h.client.StopShift(ctx, thread.ID, proto.StopShift{RequestID: "x1"}); err != nil {
		t.Fatal(err)
	}
	h.failTo(0)
	if err := h.server.DeliverPending(ctx); err != nil {
		t.Fatal(err)
	}
	sent := strings.Join(h.sentTo(thread.Session), "\n")
	if !strings.Contains(sent, "Changes requested") || !strings.Contains(sent, "the new one") {
		t.Fatalf("a stopped shift's specialist did not get what it was owed: %q", sent)
	}
}

// A decision is committed only while the round is as it was read: an
// answer that started another run while CI was read leaves the round for
// the next pass, which decides on the new answer (Codex review of #36).
func TestARoundThatMovedWhileCIWasReadIsNotDecided(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	head := strings.Repeat("a", 40)
	var thread store.ThreadRow
	var moved atomic.Bool
	h.engine.mu.Lock()
	h.engine.onGH = func(_ context.Context, args []string) (string, error) {
		call := strings.Join(args, " ")
		if strings.Contains(call, "/check-runs") && !moved.Load() {
			moved.Store(true)
			// An answer reached the specialist meanwhile, and its run said
			// to go on.
			h.engine.mu.Lock()
			entries := h.engine.transcripts[thread.Session]
			next := entries[len(entries)-1].ID + 1
			h.engine.transcripts[thread.Session] = append(entries,
				proto.Entry{ID: next, SessionID: thread.Session, Kind: proto.EntryUser, Text: "[answer] the new one"},
				proto.Entry{ID: next + 1, SessionID: thread.Session, Kind: proto.EntryAssistant, Text: "Using it.\nSTATUS: CONTINUE"})
			h.engine.mu.Unlock()
		}
		switch {
		case strings.Contains(call, "/check-runs"):
			return `{"total_count":1,"check_runs":[{"name":"test","status":"completed","conclusion":"success"}]}`, nil
		case strings.Contains(call, "/status"):
			return `{"total_count":0,"statuses":[]}`, nil
		}
		return ciGitHub(call, "frostyard/example", map[int64]string{9: head})
	}
	h.engine.mu.Unlock()
	thread = shiftThread(t, h, "s1", team.Shift{Stop: "the tests pass", CI: true})
	if _, _, _, _, err := h.store.FileReportWithPulls(ctx, "pr", thread.ID, store.ThreadOpen, "PR", nil, []store.PullRow{ciRow("frostyard/example", 9, head)},
		func([]int64, []store.PullRow) string { return "r" }); err != nil {
		t.Fatal(err)
	}
	h.answerRound(thread, thread.BriefSubmission, "STATUS: DONE")
	h.server.shiftPass(ctx)
	if sh := h.shift(thread); sh.State != team.ShiftRunning || sh.Round != 1 {
		t.Fatalf("a done decided after the round moved: %+v", sh)
	}
	h.server.shiftPass(ctx)
	if sh := h.shift(thread); sh.State != team.ShiftRunning || sh.Round != 2 || sh.Rounds[0].Verdict != team.VerdictContinue {
		t.Fatalf("the next pass %+v", sh)
	}
}

// A message that paused its shift and failed to go keeps its claim: tried
// again after the owner resumed the shift, it goes, and pauses nothing
// (Codex review of #36).
func TestAFailedMessageThatPausedPausesOnce(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	thread := shiftThread(t, h, "s1", team.Shift{Stop: "the tests pass", MaxRounds: rounds(5)})
	h.failTo(thread.Session)
	if _, err := h.client.Send(ctx, thread.Session, proto.SendMessage{RequestID: "o1", Text: "Hold on."}); err == nil {
		t.Fatal("a send that failed succeeded")
	}
	h.failTo(0)
	if _, err := h.client.ResumeShift(ctx, thread.ID, proto.ResumeShift{RequestID: "r1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.client.Send(ctx, thread.Session, proto.SendMessage{RequestID: "o1", Text: "Hold on."}); err != nil {
		t.Fatal(err)
	}
	if sh := h.shift(thread); sh.State != team.ShiftRunning || sh.Round != 2 {
		t.Fatalf("the message tried again paused the resumed shift: %+v", sh)
	}
}

// The delivery of the last thing a round owed starts a pass, which ends
// the round (Codex review of #36).
func TestTheLastDeliveryOwedStartsAPass(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	thread := shiftThread(t, h, "s1", team.Shift{Stop: "the tests pass"})
	if _, err := h.server.TeamMessage(ctx, TeamCall{Session: thread.Session, CallID: "w1"}, team.Report{Status: team.StatusWorking, Message: "A question.",
		Asks: []team.Ask{{Question: "Which fixture?"}}}); err != nil {
		t.Fatal(err)
	}
	asks, _ := h.client.Asks(ctx)
	h.failTo(thread.Session)
	if _, err := h.client.AnswerAsk(ctx, asks[0].ID, proto.AnswerAsk{RequestID: "a1", Answer: "the new one"}); err != nil {
		t.Fatal(err)
	}
	h.answerRound(thread, thread.BriefSubmission, "STATUS: CONTINUE")
	h.server.shiftPass(ctx)
	h.runShiftPasses()
	if sh := h.shift(thread); sh.Round != 1 {
		t.Fatalf("a round ended with its answer owed: %+v", sh)
	}
	h.failTo(0)
	if err := h.server.DeliverPending(ctx); err != nil {
		t.Fatal(err)
	}
	if n := h.runShiftPasses(); n == 0 {
		t.Fatal("the answer's delivery started no pass")
	}
	if sh := h.shift(thread); sh.Round != 2 {
		t.Fatalf("the round did not end once its answer went: %+v", sh)
	}
}

// A shift's CI is read from GitHub's answer alone: a ready-for-human label
// pocketd cannot bring in step holds no done (Codex review of #36).
func TestALabelThatCannotBeWrittenHoldsNoDone(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	head := strings.Repeat("a", 40)
	h.engine.mu.Lock()
	h.engine.onGH = func(_ context.Context, args []string) (string, error) {
		call := strings.Join(args, " ")
		switch {
		case strings.Contains(call, "--method"):
			return "", errors.New("gh: Resource not accessible by integration (HTTP 403)")
		case strings.HasSuffix(call, "/pulls/9"):
			return pullAt("frostyard/example", 9, "h9", head, team.ReadyLabel), nil
		case strings.Contains(call, "/compare/"):
			return `{"merge_base_commit":{"sha":"` + strings.Repeat("c", 40) + `"}}`, nil
		case strings.Contains(call, "/check-runs"):
			return `{"total_count":1,"check_runs":[{"name":"test","status":"completed","conclusion":"success"}]}`, nil
		case strings.Contains(call, "/status"):
			return `{"total_count":0,"statuses":[]}`, nil
		}
		return "", errors.New("gh: Not Found (HTTP 404)")
	}
	h.engine.mu.Unlock()
	thread := shiftThread(t, h, "s1", team.Shift{Stop: "the tests pass", CI: true})
	if _, _, _, _, err := h.store.FileReportWithPulls(ctx, "pr", thread.ID, store.ThreadOpen, "PR", nil, []store.PullRow{ciRow("frostyard/example", 9, head)},
		func([]int64, []store.PullRow) string { return "r" }); err != nil {
		t.Fatal(err)
	}
	h.answerRound(thread, thread.BriefSubmission, "STATUS: DONE")
	h.server.shiftPass(ctx)
	if sh := h.shift(thread); sh.State != team.ShiftSummary || sh.Reason != team.ShiftDone {
		t.Fatalf("a label that cannot be written held the done: %+v", sh)
	}
}

// A done whose CI was checked green takes a summary report that links only
// the pull requests checked, at the heads checked (Codex review of #36).
func TestADoneReportLinksOnlyThePullRequestsCheckedGreen(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	checked, pushed := strings.Repeat("a", 40), strings.Repeat("b", 40)
	var thread store.ThreadRow
	var head atomic.Value
	head.Store(checked)
	h.engine.mu.Lock()
	h.engine.onGH = func(_ context.Context, args []string) (string, error) {
		call := strings.Join(args, " ")
		switch {
		case strings.HasSuffix(call, "/pulls/9"):
			return pullAt("frostyard/example", 9, thread.Branch, head.Load().(string)), nil
		case strings.HasSuffix(call, "/pulls/10"):
			return pullAt("frostyard/example", 10, thread.Branch, strings.Repeat("d", 40)), nil
		case strings.Contains(call, "/compare/"):
			return `{"merge_base_commit":{"sha":"` + strings.Repeat("c", 40) + `"}}`, nil
		case strings.Contains(call, "/check-runs"):
			return `{"total_count":1,"check_runs":[{"name":"test","status":"completed","conclusion":"success"}]}`, nil
		case strings.Contains(call, "/status"):
			return `{"total_count":0,"statuses":[]}`, nil
		}
		return "", errors.New("gh: Not Found (HTTP 404)")
	}
	h.engine.mu.Unlock()
	thread = shiftThreadOf(t, h, "vera", "frostyard/example", "s1", team.Shift{Stop: "the tests pass", CI: true})
	if _, err := h.server.TeamMessage(ctx, TeamCall{Session: thread.Session, CallID: "p1"}, team.Report{Status: team.StatusWorking, Message: "PR open.",
		PullRequests: []team.PullRef{{Number: 9}}}); err != nil {
		t.Fatal(err)
	}
	h.answerRound(thread, thread.BriefSubmission, "STATUS: DONE")
	h.server.shiftPass(ctx)
	if sh := h.shift(thread); sh.State != team.ShiftSummary || len(sh.CIHeads) != 1 || sh.CIHeads[0] != "frostyard/example#9@"+checked {
		t.Fatalf("the summary %+v", sh)
	}
	h.summaryReached(thread)
	// A working report links no pull request the done was not checked on.
	if _, err := h.server.TeamMessage(ctx, TeamCall{Session: thread.Session, CallID: "w2"}, team.Report{Status: team.StatusWorking, Message: "Another PR.",
		PullRequests: []team.PullRef{{Number: 10}}}); err == nil || !strings.Contains(err.Error(), "checked CI green on") {
		t.Fatalf("a working report linking an unchecked pull request: %v", err)
	}
	head.Store(pushed)
	if _, err := h.server.TeamMessage(ctx, TeamCall{Session: thread.Session, CallID: "r1"}, team.Report{Status: team.StatusDone, Message: "Done.",
		PullRequests: []team.PullRef{{Number: 9}}}); err == nil || !strings.Contains(err.Error(), "checked CI green on") {
		t.Fatalf("a done naming a head pushed after CI: %v", err)
	}
	head.Store(checked)
	if _, err := h.server.TeamMessage(ctx, TeamCall{Session: thread.Session, CallID: "r2"}, team.Report{Status: team.StatusDone, Message: "Done.",
		PullRequests: []team.PullRef{{Number: 9}}}); err != nil {
		t.Fatalf("a done naming the head checked: %v", err)
	}
}

// A round's decision commits only while its specialist is owed nothing,
// checked in the transaction that commits it: a verdict filed after the
// round was read again holds the round (Codex review of #36).
func TestAVerdictFiledAsTheRoundIsDecidedHoldsIt(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	thread := shiftThread(t, h, "s1", team.Shift{Stop: "the tests pass"})
	review := shiftThread(t, h, "s2", team.Shift{Stop: "x"})
	h.runShiftPasses()
	h.answerRound(thread, thread.BriefSubmission, "STATUS: CONTINUE")
	reads := 0
	h.engine.mu.Lock()
	h.engine.onTranscriptAfter = func(id int64) {
		if id != thread.Session {
			return
		}
		if reads++; reads == 2 {
			// The round is read again for its commit: a verdict for the
			// specialist is filed right then, before the commit.
			if _, err := h.store.QueueWorkerReport(ctx, store.ReportRow{Key: "verdict-1", ThreadID: review.ID, SessionID: thread.Session, Text: "[review] Changes requested."}, 0); err != nil {
				t.Error(err)
			}
		}
	}
	h.engine.mu.Unlock()
	h.failTo(thread.Session)
	h.server.shiftPass(ctx)
	if reads < 2 {
		t.Fatalf("the round was read %d times", reads)
	}
	if sh := h.shift(thread); sh.Round != 1 {
		t.Fatalf("a round moved with a verdict owed to its specialist: %+v", sh)
	}
}

// The summary's report is taken once the summary round's message is in
// the specialist's transcript, never before (Codex review of #36).
func TestASummaryReportWaitsForItsMessage(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	thread := shiftThread(t, h, "s1", team.Shift{Stop: "the tests pass"})
	h.failTo(thread.Session)
	h.answerRound(thread, thread.BriefSubmission, "STATUS: DONE")
	h.server.shiftPass(ctx)
	if sh := h.shift(thread); sh.State != team.ShiftSummary {
		t.Fatalf("the summary %+v", sh)
	}
	h.failTo(0)
	if _, err := h.server.TeamMessage(ctx, TeamCall{Session: thread.Session, CallID: "r1"}, team.Report{Status: team.StatusDone, Message: "Done."}); err == nil ||
		!strings.Contains(err.Error(), "has not reached you yet") {
		t.Fatalf("a report before the summary's message: %v", err)
	}
	if err := h.server.DeliverPending(ctx); err != nil {
		t.Fatal(err)
	}
	h.summaryReached(thread)
	if _, err := h.server.TeamMessage(ctx, TeamCall{Session: thread.Session, CallID: "r2"}, team.Report{Status: team.StatusDone, Message: "Done."}); err != nil {
		t.Fatalf("the report once the summary's message is in: %v", err)
	}
}

// Another pull request on GitHub at the linked one's name, as a
// repository named again gives, is not the linked one: its green CI is no
// done (Codex review of #36).
func TestAnotherPullRequestAtTheLinkedNameIsNoDone(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	head := strings.Repeat("a", 40)
	h.engine.mu.Lock()
	h.engine.onGH = func(_ context.Context, args []string) (string, error) {
		call := strings.Join(args, " ")
		switch {
		case strings.Contains(call, "/check-runs"):
			return `{"total_count":1,"check_runs":[{"name":"test","status":"completed","conclusion":"success"}]}`, nil
		case strings.Contains(call, "/status"):
			return `{"total_count":0,"statuses":[]}`, nil
		}
		return ciGitHub(call, "frostyard/example", map[int64]string{9: head})
	}
	h.engine.mu.Unlock()
	thread := shiftThread(t, h, "s1", team.Shift{Stop: "the tests pass", CI: true})
	linked := ciRow("frostyard/example", 9, head)
	linked.GitHubID = 77
	if _, _, _, _, err := h.store.FileReportWithPulls(ctx, "pr", thread.ID, store.ThreadOpen, "PR", nil, []store.PullRow{linked},
		func([]int64, []store.PullRow) string { return "r" }); err != nil {
		t.Fatal(err)
	}
	h.answerRound(thread, thread.BriefSubmission, "STATUS: DONE")
	h.server.shiftPass(ctx)
	if sh := h.shift(thread); sh.State != team.ShiftRunning || sh.Round != 2 || !strings.Contains(sh.Fingerprint, "another pull request") {
		t.Fatalf("done on another pull request's CI: %+v", sh)
	}
}

// A shift's lock holds that shift alone: the owner writes to a session
// that is no shift's specialist, and to another shift's, while one
// shift's lock is held (Codex review of #36).
func TestAShiftsLockHoldsNoOtherSession(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	thread := shiftThread(t, h, "s1", team.Shift{Stop: "the tests pass"})
	other := shiftThread(t, h, "s2", team.Shift{Stop: "the tests pass"})
	unlock := h.server.lockShift(thread.ID)
	defer unlock()
	if _, err := h.client.Send(ctx, h.super.ID, proto.SendMessage{RequestID: "o1", Text: "Status?"}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.client.Send(ctx, other.Session, proto.SendMessage{RequestID: "o2", Text: "Hold on."}); err != nil {
		t.Fatal(err)
	}
}

// A round that waits on CI gets a pass of its own, with no deadline and
// no pull request round: CI that went green ends it done (Codex review of
// #36).
func TestAShiftWaitingOnCIReadsItAgain(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	head := strings.Repeat("e", 40)
	var green atomic.Bool
	h.engine.mu.Lock()
	h.engine.onGH = func(_ context.Context, args []string) (string, error) {
		call := strings.Join(args, " ")
		switch {
		case strings.Contains(call, "/check-runs") && green.Load():
			return `{"total_count":1,"check_runs":[{"name":"test","status":"completed","conclusion":"success"}]}`, nil
		case strings.Contains(call, "/check-runs"):
			return `{"total_count":1,"check_runs":[{"name":"test","status":"in_progress","conclusion":null}]}`, nil
		case strings.Contains(call, "/status"):
			return `{"total_count":0,"statuses":[]}`, nil
		}
		return ciGitHub(call, "frostyard/example", map[int64]string{9: head})
	}
	h.engine.mu.Unlock()
	thread := shiftThread(t, h, "s1", team.Shift{Stop: "the tests pass", CI: true})
	if _, _, _, _, err := h.store.FileReportWithPulls(ctx, "pr", thread.ID, store.ThreadOpen, "PR", nil, []store.PullRow{ciRow("frostyard/example", 9, head)},
		func([]int64, []store.PullRow) string { return "r" }); err != nil {
		t.Fatal(err)
	}
	h.answerRound(thread, thread.BriefSubmission, "STATUS: DONE")
	h.server.shiftPass(ctx)
	h.runShiftPasses()
	if sh := h.shift(thread); sh.State != team.ShiftRunning {
		t.Fatalf("pending CI decided: %+v", sh)
	}
	green.Store(true)
	h.clock.Advance(shiftCIRecheck)
	h.fireShiftWakes()
	h.runShiftPasses()
	if sh := h.shift(thread); sh.State != team.ShiftSummary || sh.Reason != team.ShiftDone {
		t.Fatalf("CI that went green: %+v", sh)
	}
}

// A round's decision reads the round again under the shift's lock, which
// the owner's message holds as it goes (Codex review of #36).
func TestARoundIsReadAgainUnderItsShiftsLock(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	thread := shiftThread(t, h, "s1", team.Shift{Stop: "the tests pass"})
	h.answerRound(thread, thread.BriefSubmission, "STATUS: CONTINUE")
	reads, held := 0, false
	h.engine.mu.Lock()
	h.engine.onTranscriptAfter = func(id int64) {
		if id != thread.Session {
			return
		}
		if reads++; reads == 2 {
			h.server.shiftLocks.mu.Lock()
			_, held = h.server.shiftLocks.held[strconv.FormatInt(thread.ID, 10)]
			h.server.shiftLocks.mu.Unlock()
		}
	}
	h.engine.mu.Unlock()
	h.server.shiftPass(ctx)
	if reads < 2 || !held {
		t.Fatalf("the round was read again %d times, under its lock: %v", reads, held)
	}
}

// A round decided before the deadline whose message never went holds no
// work past it: the shift stops for its deadline, and the message goes to
// no one (Codex review of #36).
func TestARoundThatNeverWentStopsAtTheDeadline(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	thread := shiftThread(t, h, "s1", team.Shift{Stop: "the tests pass", Hours: hours(1)})
	h.failTo(thread.Session)
	h.answerRound(thread, thread.BriefSubmission, "STATUS: CONTINUE")
	h.server.shiftPass(ctx)
	if sh := h.shift(thread); sh.Round != 2 {
		t.Fatalf("round 2 %+v", sh)
	}
	h.clock.Advance(2 * time.Hour)
	h.failTo(0)
	if err := h.server.DeliverPending(ctx); err != nil {
		t.Fatal(err)
	}
	for _, text := range h.sentTo(thread.Session) {
		if strings.Contains(text, "Round 2 of") {
			t.Fatalf("a round's message went past the deadline: %q", text)
		}
	}
	h.runShiftPasses()
	h.server.shiftPass(ctx)
	if sh := h.shift(thread); sh.State != team.ShiftSummary || sh.Reason != team.ShiftDeadline {
		t.Fatalf("a round that never went, past the deadline: %+v", sh)
	}
	if err := h.server.DeliverPending(ctx); err != nil {
		t.Fatal(err)
	}
	for _, text := range h.sentTo(thread.Session) {
		if strings.Contains(text, "Round 2 of") {
			t.Fatalf("the expired round's message went: %q", text)
		}
	}
}

// A round's verdict is its last run's final response: text beside a tool
// call, with an empty response after it, is no verdict (Codex review of
// #36).
func TestTheVerdictIsTheFinalResponses(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	thread := shiftThread(t, h, "s1", team.Shift{Stop: "the tests pass"})
	h.answerRound(thread, thread.BriefSubmission, "STATUS: DONE")
	h.engine.mu.Lock()
	entries := h.engine.transcripts[thread.Session]
	next := entries[len(entries)-1].ID + 1
	h.engine.transcripts[thread.Session] = append(entries,
		proto.Entry{ID: next, SessionID: thread.Session, Kind: proto.EntryOther, Text: "tool result"},
		proto.Entry{ID: next + 1, SessionID: thread.Session, Kind: proto.EntryAssistant, Text: ""})
	h.engine.mu.Unlock()
	h.server.shiftPass(ctx)
	if sh := h.shift(thread); sh.State != team.ShiftRunning || sh.Round != 2 || sh.Rounds[0].Verdict != team.VerdictNone {
		t.Fatalf("a verdict beside a tool call: %+v", sh)
	}
}

// The run that filed the summary's report files no second final one
// (Codex review of #36).
func TestTheSummarysRunReportsOnce(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	thread := shiftThread(t, h, "s1", team.Shift{Stop: "deployed"})
	h.answerRound(thread, thread.BriefSubmission, "STATUS: BLOCKED: needs the key")
	h.server.shiftPass(ctx)
	h.summaryReached(thread)
	h.engine.mu.Lock()
	h.engine.runFirst = 40
	h.engine.mu.Unlock()
	if _, err := h.server.TeamMessage(ctx, TeamCall{Session: thread.Session, CallID: "r1"}, team.Report{Status: team.StatusBlocked, Message: "Needs the key."}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.server.TeamMessage(ctx, TeamCall{Session: thread.Session, CallID: "r2"}, team.Report{Status: team.StatusDone, Message: "Done after all."}); err == nil ||
		!strings.Contains(err.Error(), "took this run's report already") {
		t.Fatalf("a second final report from the summary's run: %v", err)
	}
	h.engine.mu.Lock()
	h.engine.runFirst = 0
	h.engine.mu.Unlock()
	if reports := h.reportsTo(thread); len(reports) != 1 {
		t.Fatalf("the supervisor's reports %q", reports)
	}
}

// A pass that could not read what it needed runs again by itself (Codex
// review of #36).
func TestAFailedPassRunsAgain(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	thread := shiftThread(t, h, "s1", team.Shift{Stop: "the tests pass"})
	h.runShiftPasses()
	h.answerRound(thread, thread.BriefSubmission, "STATUS: CONTINUE")
	h.engine.mu.Lock()
	h.engine.busyErr = errors.New("PiG is not answering")
	h.engine.mu.Unlock()
	h.server.shiftPass(ctx)
	if sh := h.shift(thread); sh.Round != 1 {
		t.Fatalf("a pass that failed moved: %+v", sh)
	}
	h.runScheduled(5)
	h.runShiftPasses()
	if sh := h.shift(thread); sh.Round != 2 {
		t.Fatalf("the pass did not run again: %+v", sh)
	}
}

// A tool call's entry is no final response: a run that ended in a tool
// call has no verdict, whatever text it wrote beside the call (Codex
// review of #36).
func TestAToolCallsTextIsNoVerdict(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	thread := shiftThread(t, h, "s1", team.Shift{Stop: "the tests pass"})
	h.answerRound(thread, thread.BriefSubmission, "")
	h.engine.mu.Lock()
	entries := h.engine.transcripts[thread.Session]
	next := entries[len(entries)-1].ID + 1
	h.engine.transcripts[thread.Session] = append(entries,
		proto.Entry{ID: next, SessionID: thread.Session, Kind: proto.EntryAssistant, Text: "STATUS: DONE", StopReason: stopToolUse},
		proto.Entry{ID: next + 1, SessionID: thread.Session, Kind: proto.EntryOther, Text: "tool result"})
	if h.engine.transcriptLive == nil {
		h.engine.transcriptLive = map[int64]proto.Live{}
	}
	h.engine.transcriptLive[thread.Session] = proto.Live{Failure: "aborted"}
	h.engine.mu.Unlock()
	h.server.shiftPass(ctx)
	if sh := h.shift(thread); sh.State != team.ShiftEnded || sh.Reason != team.ShiftFailed {
		t.Fatalf("a tool call's text as a verdict: %+v", sh)
	}
}

// A round's message queued on an idle session past the deadline is not
// submitted again: it is withdrawn, and the shift stops for its deadline
// (Codex review of #36).
func TestAStrandedRoundPastTheDeadlineIsNotSentAgain(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	thread := shiftThread(t, h, "s1", team.Shift{Stop: "the tests pass", Hours: hours(1)})
	// Round 2's message waits in the queue of a busy specialist, whose run
	// then ends with it still there: stranded on an idle session.
	h.queueRoundMessage(thread, 2)
	h.answerRound(thread, thread.BriefSubmission, "STATUS: CONTINUE")
	h.server.shiftPass(ctx)
	key := shiftRoundKey(thread, 2) + ":report"
	h.engine.mu.Lock()
	sub := h.engine.sent[key]
	queued := h.engine.queued[sub]
	h.engine.runFirst, h.engine.onSend = 0, nil
	h.engine.mu.Unlock()
	if !queued {
		t.Fatal("round 2's message is not queued")
	}
	h.clock.Advance(2 * time.Hour)
	if err := h.server.DeliverPending(ctx); err != nil {
		t.Fatal(err)
	}
	h.engine.mu.Lock()
	_, again := h.engine.sent[attemptKey(key, 1)]
	h.engine.mu.Unlock()
	if again {
		t.Fatal("a stranded round's message was submitted again past the deadline")
	}
	h.runShiftPasses()
	h.server.shiftPass(ctx)
	h.engine.mu.Lock()
	withdrawn := h.engine.withdrawn[sub]
	h.engine.mu.Unlock()
	if sh := h.shift(thread); sh.State != team.ShiftSummary || sh.Reason != team.ShiftDeadline || !withdrawn {
		t.Fatalf("a stranded round past the deadline: %+v (withdrawn %v)", sh, withdrawn)
	}
}

// A CI GitHub cannot read waits for the CI wake, never for a quicker
// retry (Codex review of #36).
func TestAnUnreadableCIWaitsForItsWake(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	h.engine.mu.Lock()
	h.engine.onGH = func(context.Context, []string) (string, error) { return "", errors.New("gh: HTTP 401") }
	h.engine.mu.Unlock()
	thread := shiftThread(t, h, "s1", team.Shift{Stop: "the tests pass", CI: true})
	if _, _, _, _, err := h.store.FileReportWithPulls(ctx, "pr", thread.ID, store.ThreadOpen, "PR", nil, []store.PullRow{ciRow("frostyard/example", 9, strings.Repeat("a", 40))},
		func([]int64, []store.PullRow) string { return "r" }); err != nil {
		t.Fatal(err)
	}
	h.runShiftPasses()
	h.retryMu.Lock()
	before := len(h.scheduled)
	h.retryMu.Unlock()
	h.answerRound(thread, thread.BriefSubmission, "STATUS: DONE")
	h.server.shiftPass(ctx)
	h.retryMu.Lock()
	after := len(h.scheduled)
	h.retryMu.Unlock()
	h.shiftMu.Lock()
	wakes := len(h.shiftWakes)
	h.shiftMu.Unlock()
	if after != before || wakes != 1 {
		t.Fatalf("retries %d → %d, wakes %d", before, after, wakes)
	}
}

// A shift's brief, once placed, starts a pass (Codex review of #36).
func TestAShiftsBriefStartsAPass(t *testing.T) {
	h := newTeamHarness(t)
	shiftThread(t, h, "s1", team.Shift{Stop: "the tests pass"})
	if n := h.runShiftPasses(); n == 0 {
		t.Fatal("the brief's placement started no pass")
	}
}

// A thread whose event a failed read of its shift held back is published
// whole by the next delivery pass (Codex review of #36).
func TestAThreadEventHeldBackIsPublishedWhole(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	thread := shiftThread(t, h, "s1", team.Shift{Stop: "the tests pass"})
	h.server.threadRetryMu.Lock()
	h.server.threadRetry[thread.ID] = true
	h.server.threadRetryMu.Unlock()
	h.mu.Lock()
	h.events = nil
	h.mu.Unlock()
	if err := h.server.DeliverPending(ctx); err != nil {
		t.Fatal(err)
	}
	if last := h.lastThread(thread.ID); last.Shift == nil || last.Shift.State != proto.ShiftRunning {
		t.Fatalf("the event held back %+v", last)
	}
}

// The owner's stop ends the shift only: no run is aborted, the shift's own
// round message still queued is withdrawn, and the worker's work goes on
// (the owner's choice, 2026-11-20).
func TestAStopEndsTheShiftOnly(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	h.engine.mu.Lock()
	h.engine.keyModels = []proto.Model{{ModelRef: deepseekFlash, Name: "DeepSeek V4.1 Flash"}}
	h.engine.mu.Unlock()
	thread := shiftThread(t, h, "s1", team.Shift{Stop: "the tests pass", MaxRounds: rounds(5)})
	if _, err := h.server.TeamWorker(ctx, TeamCall{Session: thread.Session, Task: 7, CallID: "w1"}, team.WorkerMessage{Message: "Fix it."}); err != nil {
		t.Fatal(err)
	}
	thread, _ = h.store.Thread(ctx, thread.ID)
	h.failTo(thread.WorkerSession)
	if _, err := h.server.TeamWorker(ctx, TeamCall{Session: thread.Session, Task: 8, CallID: "w2"}, team.WorkerMessage{Message: "Also run make ci."}); err != nil {
		t.Fatal(err)
	}
	h.engine.mu.Lock()
	h.engine.busy = map[int64]bool{thread.Session: true, thread.WorkerSession: true}
	h.engine.mu.Unlock()
	if _, err := h.client.StopShift(ctx, thread.ID, proto.StopShift{RequestID: "x1"}); err != nil {
		t.Fatal(err)
	}
	h.server.shiftPass(ctx)
	h.failTo(0)
	if err := h.server.DeliverPending(ctx); err != nil {
		t.Fatal(err)
	}
	h.engine.mu.Lock()
	aborted := append([]int64(nil), h.engine.aborted...)
	_, sent := h.engine.sent[workerMessagePrefix+TeamCall{Session: thread.Session, Task: 8, CallID: "w2"}.key()+":report"]
	h.engine.busy = nil
	h.engine.mu.Unlock()
	if len(aborted) != 0 {
		t.Fatalf("the stop aborted runs: %v", aborted)
	}
	if !sent {
		t.Fatal("the worker's work did not go on after the stop")
	}
	if sh := h.shift(thread); sh.State != team.ShiftEnded || sh.Reason != team.ShiftStopped {
		t.Fatalf("the shift %+v", sh)
	}
}

// A round that waits on CI reads it at most once a recheck, whatever
// passes other shifts run meanwhile (Codex review of #36).
func TestCIWaitsItsRecheckWhateverPassesRun(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	head := strings.Repeat("e", 40)
	var reads atomic.Int32
	h.engine.mu.Lock()
	h.engine.onGH = func(_ context.Context, args []string) (string, error) {
		call := strings.Join(args, " ")
		switch {
		case strings.Contains(call, "/check-runs"):
			reads.Add(1)
			return `{"total_count":1,"check_runs":[{"name":"test","status":"in_progress","conclusion":null}]}`, nil
		case strings.Contains(call, "/status"):
			return `{"total_count":0,"statuses":[]}`, nil
		}
		return ciGitHub(call, "frostyard/example", map[int64]string{9: head})
	}
	h.engine.mu.Unlock()
	thread := shiftThread(t, h, "s1", team.Shift{Stop: "the tests pass", CI: true})
	if _, _, _, _, err := h.store.FileReportWithPulls(ctx, "pr", thread.ID, store.ThreadOpen, "PR", nil, []store.PullRow{ciRow("frostyard/example", 9, head)},
		func([]int64, []store.PullRow) string { return "r" }); err != nil {
		t.Fatal(err)
	}
	h.answerRound(thread, thread.BriefSubmission, "STATUS: DONE")
	for range 3 {
		h.server.shiftPass(ctx)
	}
	if n := reads.Load(); n != 1 {
		t.Fatalf("CI read %d times before its recheck", n)
	}
	h.clock.Advance(shiftCIRecheck)
	h.server.shiftPass(ctx)
	if n := reads.Load(); n != 2 {
		t.Fatalf("CI read %d times after its recheck", n)
	}
}

// A round that waits on CI keeps a wake of its own, also when an earlier
// round's wake, due sooner, took its place: when that one comes, the round
// sets its own (Codex review of #36).
func TestACIWakeFollowsTheRoundUnderWay(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	head := strings.Repeat("e", 40)
	var green atomic.Bool
	h.engine.mu.Lock()
	h.engine.onGH = func(_ context.Context, args []string) (string, error) {
		call := strings.Join(args, " ")
		switch {
		case strings.Contains(call, "/check-runs") && green.Load():
			return `{"total_count":1,"check_runs":[{"name":"test","status":"completed","conclusion":"success"}]}`, nil
		case strings.Contains(call, "/check-runs"):
			return `{"total_count":1,"check_runs":[{"name":"test","status":"in_progress","conclusion":null}]}`, nil
		case strings.Contains(call, "/status"):
			return `{"total_count":0,"statuses":[]}`, nil
		}
		return ciGitHub(call, "frostyard/example", map[int64]string{9: head})
	}
	h.engine.mu.Unlock()
	thread := shiftThread(t, h, "s1", team.Shift{Stop: "the tests pass", MaxRounds: rounds(5), CI: true})
	if _, _, _, _, err := h.store.FileReportWithPulls(ctx, "pr", thread.ID, store.ThreadOpen, "PR", nil, []store.PullRow{ciRow("frostyard/example", 9, head)},
		func([]int64, []store.PullRow) string { return "r" }); err != nil {
		t.Fatal(err)
	}
	h.runShiftPasses()
	// Round 1 waits on CI: its wake is due in two minutes.
	h.answerRound(thread, thread.BriefSubmission, "STATUS: DONE")
	h.server.shiftPass(ctx)
	h.clock.Advance(time.Minute)
	// The owner pauses and resumes: round 2 begins, and waits on CI too.
	if _, err := h.client.Send(ctx, thread.Session, proto.SendMessage{RequestID: "o1", Text: "Hold on."}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.client.ResumeShift(ctx, thread.ID, proto.ResumeShift{RequestID: "r1"}); err != nil {
		t.Fatal(err)
	}
	h.answerRound(thread, h.submissionOf(shiftRoundKey(thread, 2)), "STATUS: DONE")
	h.server.shiftPass(ctx)
	h.runShiftPasses()
	if sh := h.shift(thread); sh.Round != 2 || sh.State != team.ShiftRunning {
		t.Fatalf("round 2 %+v", sh)
	}
	// Round 1's wake comes: round 2's recheck is not due yet, and it sets
	// its own wake.
	h.clock.Advance(time.Minute)
	h.fireShiftWakes()
	h.runShiftPasses()
	green.Store(true)
	h.clock.Advance(time.Minute)
	h.fireShiftWakes()
	h.runShiftPasses()
	if sh := h.shift(thread); sh.State != team.ShiftSummary || sh.Reason != team.ShiftDone {
		t.Fatalf("round 2 lost its wake: %+v", sh)
	}
}

// A shift's brief that has not reached the specialist by the deadline is
// never sent: the thread closes, the shift ends for its deadline, and the
// supervisor hears why (Codex review of #36).
func TestABriefPastItsShiftsDeadlineIsNotSent(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	// The specialist is busy as its brief goes: the brief waits in its
	// queue, and stays there once the run ends.
	h.engine.mu.Lock()
	h.engine.runFirst = 1
	h.engine.queued, h.engine.withdrawn = map[int64]bool{}, map[int64]bool{}
	h.engine.onSend = func(int64, string) {
		h.engine.mu.Lock()
		h.engine.queued[int64(len(h.engine.sent)+100)] = true
		h.engine.mu.Unlock()
	}
	h.engine.mu.Unlock()
	call := TeamCall{Session: h.super.ID, CallID: "s1"}
	if _, err := h.server.TeamDelegate(ctx, call, team.Delegation{Agent: "duncan", Task: "Fix the flaky tests.", Shift: &team.Shift{Stop: "the tests pass", Hours: hours(1)}}); err != nil {
		t.Fatal(err)
	}
	thread, err := h.store.ThreadByKey(ctx, call.key())
	if err != nil || thread.Briefed {
		t.Fatalf("the thread %+v, %v", thread, err)
	}
	h.engine.mu.Lock()
	h.engine.runFirst, h.engine.onSend = 0, nil
	h.engine.mu.Unlock()
	h.clock.Advance(2 * time.Hour)
	if err := h.server.DeliverPending(ctx); err != nil {
		t.Fatal(err)
	}
	h.engine.mu.Lock()
	_, again := h.engine.sent[attemptKey(briefKey(thread), 1)]
	h.engine.mu.Unlock()
	if again {
		t.Fatal("a brief was sent again past its shift's deadline")
	}
	thread, _ = h.store.Thread(ctx, thread.ID)
	if sh := h.shift(thread); thread.State != store.ThreadClosed || sh.State != team.ShiftEnded || sh.Reason != team.ShiftDeadline {
		t.Fatalf("the thread %s, its shift %+v", thread.State, sh)
	}
	if err := h.server.DeliverPending(ctx); err != nil {
		t.Fatal(err)
	}
	if sent := strings.Join(h.sentTo(h.super.ID), "\n"); !strings.Contains(sent, "The shift ended before it began") {
		t.Fatalf("the supervisor got %q", sent)
	}
}

// A run that failed with no answer ends the shift with its round recorded
// and the CI failure streak ended (Codex review of #36).
func TestAFailedRunEndsTheCIStreak(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	head := strings.Repeat("a", 40)
	h.engine.mu.Lock()
	h.engine.onGH = func(_ context.Context, args []string) (string, error) {
		call := strings.Join(args, " ")
		switch {
		case strings.Contains(call, "/check-runs"):
			return `{"total_count":1,"check_runs":[{"name":"test","status":"completed","conclusion":"failure"}]}`, nil
		case strings.Contains(call, "/status"):
			return `{"total_count":0,"statuses":[]}`, nil
		}
		return ciGitHub(call, "frostyard/example", map[int64]string{9: head})
	}
	h.engine.mu.Unlock()
	thread := shiftThread(t, h, "s1", team.Shift{Stop: "the tests pass", MaxRounds: rounds(9), CI: true})
	if _, _, _, _, err := h.store.FileReportWithPulls(ctx, "pr", thread.ID, store.ThreadOpen, "PR", nil, []store.PullRow{ciRow("frostyard/example", 9, head)},
		func([]int64, []store.PullRow) string { return "r" }); err != nil {
		t.Fatal(err)
	}
	h.answerRound(thread, thread.BriefSubmission, "STATUS: DONE")
	h.server.shiftPass(ctx)
	h.answerRound(thread, h.submissionOf(shiftRoundKey(thread, 2)), "STATUS: DONE")
	h.server.shiftPass(ctx)
	if sh := h.shift(thread); sh.Failures != 2 {
		t.Fatalf("two failures %+v", sh)
	}
	h.answerRound(thread, h.submissionOf(shiftRoundKey(thread, 3)), "")
	h.engine.mu.Lock()
	if h.engine.transcriptLive == nil {
		h.engine.transcriptLive = map[int64]proto.Live{}
	}
	h.engine.transcriptLive[thread.Session] = proto.Live{Failure: "model_error: 500"}
	h.engine.mu.Unlock()
	h.server.shiftPass(ctx)
	sh := h.shift(thread)
	if sh.State != team.ShiftEnded || sh.Reason != team.ShiftFailed || sh.Failures != 0 || sh.Fingerprint != "" || len(sh.Rounds) != 3 || sh.Rounds[2].Verdict != team.VerdictNone {
		t.Fatalf("a failed run's end %+v", sh)
	}
}

// A failed message's claim that a release could not free is freed by a
// delivery pass (Codex review of #36).
func TestAFailedMessagesClaimIsFreedLater(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	failRelease := true
	h.server.releaseRequest = func(ctx context.Context, key, kind string) error {
		if failRelease {
			failRelease = false
			return errors.New("the work database is busy")
		}
		return h.store.ReleaseRequest(ctx, key, kind)
	}
	h.failTo(h.super.ID)
	if _, err := h.client.Send(ctx, h.super.ID, proto.SendMessage{RequestID: "o1", Text: "Status?"}); err == nil {
		t.Fatal("a send that failed succeeded")
	}
	key := "u:" + h.device + ":o1"
	if _, existed, _ := h.store.LookupRequest(ctx, key, "send:"+itoa(h.super.ID)); !existed {
		t.Fatal("the claim is gone already: the test needs a release that failed")
	}
	if err := h.server.DeliverPending(ctx); err != nil {
		t.Fatal(err)
	}
	if _, existed, _ := h.store.LookupRequest(ctx, key, "send:"+itoa(h.super.ID)); existed {
		t.Fatal("a failed message's claim was never freed")
	}
}
