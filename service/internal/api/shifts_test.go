package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/frostyard/pocketgo/proto"
	"github.com/frostyard/pocketgo/service/internal/store"
	"github.com/frostyard/pocketgo/service/internal/team"
)

// shiftThread delegates Duncan's thread with shift sh, as a supervisor
// does, and reads it back once its brief went.
func shiftThread(t *testing.T, h *teamHarness, callID string, sh team.Shift) store.ThreadRow {
	t.Helper()
	call := TeamCall{Session: h.super.ID, CallID: callID}
	if _, err := h.server.TeamDelegate(context.Background(), call, team.Delegation{Agent: "duncan", Task: "Fix the flaky tests.", Shift: &sh}); err != nil {
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
	thread := shiftThread(t, h, "s1", team.Shift{Stop: "the tests pass", MaxRounds: 5})
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
	if _, err := h.server.TeamMessage(ctx, TeamCall{Session: thread.Session, CallID: "r1"}, team.Report{Status: team.StatusDone, Message: "Fixed the tests."}); err != nil {
		t.Fatal(err)
	}
	if sh = h.shift(thread); sh.State != team.ShiftEnded || sh.Reason != team.ShiftDone {
		t.Fatalf("after the report %+v", sh)
	}
	h.answerRound(thread, h.submissionOf(shiftSummaryKey(thread, 2)), "Reported.")
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
	thread := shiftThread(t, h, "s1", team.Shift{Stop: "the tests pass", MaxRounds: 2})
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

	timed := shiftThread(t, h, "s2", team.Shift{Stop: "the tests pass", MaxRounds: 10, Hours: 1})
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
	if _, err := h.store.QueueWorkerReport(ctx, store.ReportRow{Key: workerReplyPrefix + thread.Owner + ":9", ThreadID: thread.ID, SessionID: thread.Session, Text: "[T1 · worker] done"}); err != nil {
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
// round, and stops it, once per request: the stop aborts the round's run
// and the supervisor hears once (ADR-0024).
func TestTheOwnerPausesResumesAndStopsAShift(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	thread := shiftThread(t, h, "s1", team.Shift{Stop: "the tests pass", MaxRounds: 5})
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
	h.engine.mu.Lock()
	aborted := append([]int64(nil), h.engine.aborted...)
	h.engine.runFirst = 0
	h.engine.mu.Unlock()
	if len(aborted) == 0 || aborted[0] != thread.Session {
		t.Fatalf("the round's run was not aborted: %v", aborted)
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
	conclusion := "failure"
	h.engine.mu.Lock()
	h.engine.onGH = func(_ context.Context, args []string) (string, error) {
		call := strings.Join(args, " ")
		switch {
		case strings.Contains(call, "/commits/"+head+"/check-runs"):
			return `{"total_count":1,"check_runs":[{"name":"test","status":"completed","conclusion":"` + conclusion + `"}]}`, nil
		case strings.Contains(call, "/commits/"+head+"/status"):
			return `{"total_count":0,"statuses":[]}`, nil
		}
		return "", errors.New("gh: Not Found (HTTP 404)")
	}
	h.engine.mu.Unlock()
	thread := shiftThread(t, h, "s1", team.Shift{Stop: "the tests pass", MaxRounds: 6, CI: true})
	// Done with no pull request is no done.
	h.answerRound(thread, thread.BriefSubmission, "STATUS: DONE")
	h.server.shiftPass(ctx)
	if sh := h.shift(thread); sh.Round != 2 || sh.Failures != 1 {
		t.Fatalf("done with no pull request %+v", sh)
	}
	if _, _, _, _, err := h.store.FileReportWithPulls(ctx, "pr", thread.ID, store.ThreadOpen, "PR", nil,
		[]store.PullRow{{Repository: "frostyard/example", Number: 9, GitHubID: 1, Title: "t", URL: "u", State: store.PullOpen, Head: "h", HeadSHA: head, Base: "main"}},
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

	conclusion = "success"
	green := shiftThread(t, h, "s2", team.Shift{Stop: "the tests pass", CI: true})
	if _, _, _, _, err := h.store.FileReportWithPulls(ctx, "pr2", green.ID, store.ThreadOpen, "PR", nil,
		[]store.PullRow{{Repository: "frostyard/example", Number: 10, GitHubID: 2, Title: "t", URL: "u2", State: store.PullOpen, Head: "h2", HeadSHA: head, Base: "main"}},
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
		[]store.PullRow{{Repository: "frostyard/example", Number: 11, GitHubID: 3, Title: "t", URL: "u3", State: store.PullOpen, Head: "h3", HeadSHA: strings.Repeat("b", 40), Base: "main"}},
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
		"too long":  {team.Delegation{Agent: "duncan", Task: "x", Shift: &team.Shift{Stop: "x", Hours: 30}}, "at most 24"},
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
