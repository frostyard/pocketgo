package api

import (
	"context"
	"errors"
	"net/http"
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
	if ok, err := h.store.RecordSend(ctx, "o1", "send:"+itoa(paused.Session), "1", paused.ID, "the owner wrote"); err != nil || !ok {
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
// found by its message's request key. Its placement, once noted, wakes the
// driver (Codex review of #36).
func TestARoundBeginsWhereItsMessageWasPlaced(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	passes := make(chan struct{}, 64)
	h.server.afterShiftPass = func() {
		select {
		case passes <- struct{}{}:
		default:
		}
	}
	thread := shiftThread(t, h, "s1", team.Shift{Stop: "the tests pass"})
	h.failTo(thread.Session)
	h.answerRound(thread, thread.BriefSubmission, "STATUS: CONTINUE")
	h.server.shiftPass(ctx)
	h.failTo(0)
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
	deadline := time.After(10 * time.Second)
	for h.shift(thread).State != team.ShiftSummary {
		select {
		case <-passes:
		case <-deadline:
			t.Fatalf("the round did not end: %+v", h.shift(thread))
		}
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

// The owner's stop aborts the runs of the specialist until they are over,
// also when an abort fails: the shift owes it until it is settled. A
// resume drops what a stop owes, so its run is never aborted (Codex review
// of #36).
func TestAStopAbortsUntilItsRunsAreOver(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	thread := shiftThread(t, h, "s1", team.Shift{Stop: "the tests pass", MaxRounds: rounds(5)})
	h.engine.mu.Lock()
	h.engine.runFirst, h.engine.abortErr = 1, errors.New("PiG is busy")
	h.engine.mu.Unlock()
	if stopped, err := h.client.StopShift(ctx, thread.ID, proto.StopShift{RequestID: "x1"}); err != nil || stopped.Shift.State != proto.ShiftEnded {
		t.Fatalf("stopped %+v, %v", stopped.Shift, err)
	}
	if sh := h.shift(thread); !sh.StopDue {
		t.Fatalf("a stop whose abort failed owes nothing: %+v", sh)
	}
	// Tried again again with the same request, it aborts nothing more: the
	// driver does.
	if _, err := h.client.StopShift(ctx, thread.ID, proto.StopShift{RequestID: "x1"}); err != nil {
		t.Fatal(err)
	}
	h.server.shiftPass(ctx)
	h.engine.mu.Lock()
	aborted := append([]int64(nil), h.engine.aborted...)
	h.engine.mu.Unlock()
	if len(aborted) != 1 || aborted[0] != thread.Session {
		t.Fatalf("the stopped run was not aborted: %v", aborted)
	}
	h.server.shiftPass(ctx)
	if sh := h.shift(thread); sh.StopDue {
		t.Fatalf("a stop whose runs are over still owes: %+v", sh)
	}

	resumed := shiftThread(t, h, "s2", team.Shift{Stop: "the tests pass", MaxRounds: rounds(5)})
	h.engine.mu.Lock()
	h.engine.runFirst, h.engine.abortErr = 1, errors.New("PiG is busy")
	h.engine.mu.Unlock()
	if _, err := h.client.StopShift(ctx, resumed.ID, proto.StopShift{RequestID: "x2"}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.client.ResumeShift(ctx, resumed.ID, proto.ResumeShift{RequestID: "r2"}); err != nil {
		t.Fatal(err)
	}
	h.engine.mu.Lock()
	before := len(h.engine.aborted)
	h.engine.runFirst = 1 // the resumed round's run
	h.engine.mu.Unlock()
	h.server.shiftPass(ctx)
	h.engine.mu.Lock()
	after := len(h.engine.aborted)
	h.engine.runFirst = 0
	h.engine.mu.Unlock()
	if after != before {
		t.Fatalf("a resumed run was aborted: %v", h.engine.aborted)
	}
	if sh := h.shift(resumed); sh.StopDue || sh.State != team.ShiftRunning {
		t.Fatalf("the resumed shift %+v", sh)
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
