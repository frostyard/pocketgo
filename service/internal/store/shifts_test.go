package store

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/frostyard/pocketgo/service/internal/team"
)

// newShift records a thread with a shift of max rounds, attached to
// session 60, as a delegation records it (ADR-0024).
func newShift(t *testing.T, s *Store, key string, max int) ThreadRow {
	t.Helper()
	deadline := time.Date(2026, 11, 21, 7, 0, 0, 0, time.UTC)
	thread, existed, err := s.CreateShiftThread(ctx, ThreadRow{Key: key, Title: "Night work", Agent: "vera", SupervisorSession: 1, Task: "Fix the flaky tests"},
		ShiftRow{Stop: "CI is green", MaxRounds: max, Deadline: deadline, CI: true})
	if err != nil || existed {
		t.Fatalf("create %v %v", existed, err)
	}
	if thread, err = s.AttachThread(ctx, thread.ID, 60, "/w", "", "brief"); err != nil {
		t.Fatal(err)
	}
	return thread
}

func roundMessage(thread ThreadRow, n int) ReportRow {
	return ReportRow{Key: "shift-round:" + thread.Owner + ":" + string(rune('0'+n)), ThreadID: thread.ID, SessionID: thread.Session, Text: "round"}
}

// A shift is recorded with its thread, at round 1, running; each round's
// verdict and the next round's message are kept together, once, and a
// change from a stale round is a conflict (ADR-0024).
func TestAShiftRunsRoundByRound(t *testing.T) {
	s, _ := open(t)
	thread := newShift(t, s, "d1", 3)
	row, err := s.Shift(ctx, thread.ID)
	if err != nil || row.State != team.ShiftRunning || row.Round != 1 || row.MaxRounds != 3 || !row.CI || row.Stop != "CI is green" || row.Deadline.IsZero() {
		t.Fatalf("the shift %+v, %v", row, err)
	}
	// The same key records nothing twice.
	if again, existed, err := s.CreateShiftThread(ctx, ThreadRow{Key: "d1", Title: "x", Agent: "vera", SupervisorSession: 1, Task: "x"}, ShiftRow{Stop: "other", MaxRounds: 9}); err != nil || !existed || again.ID != thread.ID {
		t.Fatalf("again %+v %v %v", again, existed, err)
	}
	if row, _ := s.Shift(ctx, thread.ID); row.Stop != "CI is green" {
		t.Fatalf("a retry changed the shift: %+v", row)
	}
	next, err := s.NextShiftRound(ctx, thread.ID, 1, team.VerdictContinue, "", 0, "", roundMessage(thread, 2))
	if err != nil || next.Round != 2 || len(next.Rounds) != 1 || next.Rounds[0].Verdict != team.VerdictContinue {
		t.Fatalf("round 2 %+v, %v", next, err)
	}
	if _, err := s.NextShiftRound(ctx, thread.ID, 1, team.VerdictContinue, "", 0, "", roundMessage(thread, 2)); !errors.Is(err, ErrConflict) {
		t.Fatalf("round 1 again: %v", err)
	}
	unsent, _ := s.UnsentReportsOf(ctx, thread.ID)
	if len(unsent) != 1 || unsent[0].SessionID != 60 || unsent[0].Key != roundMessage(thread, 2).Key {
		t.Fatalf("the round's message %+v", unsent)
	}
	if next, err = s.NextShiftRound(ctx, thread.ID, 2, team.VerdictDone, "", 1, "lint", roundMessage(thread, 3)); err != nil || next.Failures != 1 || next.Fingerprint != "lint" {
		t.Fatalf("a failing check %+v, %v", next, err)
	}
	why := team.ShiftWhy(team.ShiftMaxRounds, "", 3, 3)
	sum, err := s.SummarizeShift(ctx, thread.ID, 3, team.VerdictContinue, "", 0, "", team.ShiftMaxRounds, why, nil, ReportRow{Key: "shift-summary:" + thread.Owner, ThreadID: thread.ID, SessionID: 60, Text: "summary"})
	if err != nil || sum.State != team.ShiftSummary || sum.Reason != team.ShiftMaxRounds || sum.Why != why || len(sum.Rounds) != 3 || sum.Failures != 0 {
		t.Fatalf("the summary %+v, %v", sum, err)
	}
	ended, updated, message, replayed, err := s.EndShift(ctx, thread.ID, ShiftEnd{Reason: team.ShiftMaxRounds, Why: why, From: []string{team.ShiftSummary}, Report: shiftReport(thread)})
	if err != nil || replayed || ended.State != team.ShiftEnded || ended.EndedAt == nil || updated.State != ThreadBlocked || message.SessionID != 1 {
		t.Fatalf("the end %+v %+v %+v %v %v", ended, updated, message, replayed, err)
	}
	if _, _, _, _, err := s.EndShift(ctx, thread.ID, ShiftEnd{Reason: team.ShiftStopped, Why: "x", From: []string{team.ShiftRunning, team.ShiftSummary}, Report: shiftReport(thread)}); !errors.Is(err, ErrConflict) {
		t.Fatalf("an ended shift ended again: %v", err)
	}
	if live, _ := s.LiveShifts(ctx); len(live) != 0 {
		t.Fatalf("live %+v", live)
	}
}

func shiftReport(thread ThreadRow) func(ShiftRow) ShiftReport {
	return func(row ShiftRow) ShiftReport {
		return ShiftReport{Key: "shift-end:" + thread.Owner + ":" + row.Reason, State: ThreadBlocked, Message: "ended",
			Text: func([]int64) string { return "T1's shift ended: " + row.Why }}
	}
}

// The summary round keeps the CI failure streak as it stands then: the
// third failure in a row that stops a shift is counted in it (Codex review
// of #36).
func TestASummaryKeepsItsFailureStreak(t *testing.T) {
	s, _ := open(t)
	thread := newShift(t, s, "d1", 9)
	if _, err := s.NextShiftRound(ctx, thread.ID, 1, team.VerdictDone, "", 2, "lint", roundMessage(thread, 2)); err != nil {
		t.Fatal(err)
	}
	sum, err := s.SummarizeShift(ctx, thread.ID, 2, team.VerdictDone, "", 3, "lint", team.ShiftFailedCheck, "why", nil, ReportRow{Key: "sum", ThreadID: thread.ID, SessionID: 60, Text: "summary"})
	if err != nil || sum.Failures != 3 || sum.Fingerprint != "lint" {
		t.Fatalf("the summary %+v, %v", sum, err)
	}
}

// The report pocketd files when a shift ends goes in the transaction that
// ends it: an end that fails keeps no report, and a shift that ended has
// its report, whatever stops pocketd after (Codex review of #36).
func TestAShiftEndsWithItsReportInOneTransaction(t *testing.T) {
	s, _ := open(t)
	thread := newShift(t, s, "d1", 9)
	// A report on a closed thread fails: the shift stays as it was.
	if _, err := s.db.ExecContext(ctx, `UPDATE threads SET state = ? WHERE id = ?`, ThreadClosed, thread.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, _, _, err := s.EndShift(ctx, thread.ID, ShiftEnd{Reason: team.ShiftStopped, Why: "stopped", From: []string{team.ShiftRunning}, Stop: true, Report: shiftReport(thread)}); !errors.Is(err, ErrConflict) {
		t.Fatalf("an end whose report fails: %v", err)
	}
	if row, _ := s.Shift(ctx, thread.ID); row.State != team.ShiftRunning || row.StopDue {
		t.Fatalf("an end without its report changed the shift: %+v", row)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE threads SET state = ? WHERE id = ?`, ThreadOpen, thread.ID); err != nil {
		t.Fatal(err)
	}
	ended, updated, message, _, err := s.EndShift(ctx, thread.ID, ShiftEnd{Reason: team.ShiftStopped, Why: "stopped", From: []string{team.ShiftRunning}, Stop: true, Report: shiftReport(thread)})
	if err != nil || ended.State != team.ShiftEnded || !ended.StopDue || updated.State != ThreadBlocked {
		t.Fatalf("the end %+v %+v %v", ended, updated, err)
	}
	if got, err := s.Report(ctx, message.Key); err != nil || got.Text != "T1's shift ended: stopped" {
		t.Fatalf("the end's report %+v, %v", got, err)
	}
}

// The owner's stop owes the abort of the runs it stopped until they are
// over: the shift stays live for the driver, and a resume drops what is
// owed, since its run is no stop's (Codex review of #36).
func TestAStopOwesItsAbortUntilSettled(t *testing.T) {
	s, _ := open(t)
	thread := newShift(t, s, "d1", 9)
	if _, _, _, _, err := s.EndShift(ctx, thread.ID, ShiftEnd{Reason: team.ShiftStopped, Why: "stopped", From: []string{team.ShiftRunning}, Stop: true, Report: shiftReport(thread)}); err != nil {
		t.Fatal(err)
	}
	if live, _ := s.LiveShifts(ctx); len(live) != 1 || !live[0].StopDue {
		t.Fatalf("a stop owing its abort is not live: %+v", live)
	}
	if err := s.StopSettled(ctx, thread.ID); err != nil {
		t.Fatal(err)
	}
	if live, _ := s.LiveShifts(ctx); len(live) != 0 {
		t.Fatalf("a settled stop is live: %+v", live)
	}
	thread2 := newShift(t, s, "d2", 9)
	if _, _, _, _, err := s.EndShift(ctx, thread2.ID, ShiftEnd{Reason: team.ShiftStopped, Why: "stopped", From: []string{team.ShiftRunning}, Stop: true, Report: shiftReport(thread2)}); err != nil {
		t.Fatal(err)
	}
	ok := func(r ShiftRow) bool { return r.State == team.ShiftEnded }
	resumed, _, err := s.ResumeShift(ctx, thread2.ID, nil, ok, func(r ShiftRow) ReportRow { return roundMessage(thread2, r.Round) })
	if err != nil || resumed.StopDue || resumed.State != team.ShiftRunning {
		t.Fatalf("resumed %+v, %v", resumed, err)
	}
}

// The owner's request is claimed in the transaction that changes the
// shift: the same request again changes nothing and reads the shift as it
// is, and one whose change failed claims nothing (Codex review of #36).
func TestTheOwnersShiftRequestIsClaimedWithItsChange(t *testing.T) {
	s, _ := open(t)
	thread := newShift(t, s, "d1", 9)
	if _, _, _, err := s.ClaimSend(ctx, "m1", "send:60", thread.ID, "the owner wrote", true); err != nil {
		t.Fatal(err)
	}
	claim := &ShiftClaim{Key: "r1", Kind: "shift-resume:1"}
	ok := func(r ShiftRow) bool { return r.State == team.ShiftPaused }
	message := func(r ShiftRow) ReportRow { return roundMessage(thread, r.Round) }
	resumed, replayed, err := s.ResumeShift(ctx, thread.ID, claim, ok, message)
	if err != nil || replayed || resumed.Round != 2 {
		t.Fatalf("resumed %+v %v %v", resumed, replayed, err)
	}
	if _, _, ok, err := s.ClaimSend(ctx, "m2", "send:60", thread.ID, "the owner wrote again", true); err != nil || !ok {
		t.Fatal(ok, err)
	}
	again, replayed, err := s.ResumeShift(ctx, thread.ID, claim, ok, message)
	if err != nil || !replayed || again.State != team.ShiftPaused || again.Round != 2 {
		t.Fatalf("the same request again %+v %v %v", again, replayed, err)
	}
	if _, _, err := s.ResumeShift(ctx, thread.ID, &ShiftClaim{Key: "r1", Kind: "shift-stop:1"}, ok, message); !errors.Is(err, ErrRequestReused) {
		t.Fatalf("a reused key: %v", err)
	}
	// A request refused claims nothing.
	running := func(r ShiftRow) bool { return r.State == team.ShiftRunning }
	if _, _, err := s.ResumeShift(ctx, thread.ID, &ShiftClaim{Key: "r2", Kind: "shift-resume:1"}, running, message); !errors.Is(err, ErrConflict) {
		t.Fatalf("a refused resume: %v", err)
	}
	if _, existed, _ := s.LookupRequest(ctx, "r2", "shift-resume:1"); existed {
		t.Fatal("a refused request was claimed")
	}
	_, _, _, replayed, err = s.EndShift(ctx, thread.ID, ShiftEnd{Reason: team.ShiftStopped, Why: "stopped", From: []string{team.ShiftPaused}, Claim: &ShiftClaim{Key: "r3", Kind: "shift-stop:1"}, Report: shiftReport(thread)})
	if err != nil || replayed {
		t.Fatalf("the stop %v %v", replayed, err)
	}
	_, _, _, replayed, err = s.EndShift(ctx, thread.ID, ShiftEnd{Reason: team.ShiftStopped, Why: "stopped", From: []string{team.ShiftPaused}, Claim: &ShiftClaim{Key: "r3", Kind: "shift-stop:1"}, Report: shiftReport(thread)})
	if err != nil || !replayed {
		t.Fatalf("the stop again %v %v", replayed, err)
	}
}

// While a shift runs rounds, its thread takes no final report; in its
// summary round, only the status its stop asks for, which ends the shift
// in the report's transaction (Codex review of #36).
func TestAShiftTakesOnlyItsSummarysReport(t *testing.T) {
	s, _ := open(t)
	thread := newShift(t, s, "d1", 9)
	text := func([]int64) string { return "report" }
	if _, _, _, err := s.FileReport(ctx, "r1", thread.ID, ThreadDone, "done", nil, text); !errors.Is(err, ErrShiftReport) {
		t.Fatalf("a final report mid-shift: %v", err)
	}
	if _, _, _, err := s.FileReport(ctx, "r2", thread.ID, ThreadOpen, "a question", nil, text); err != nil {
		t.Fatalf("a working report mid-shift: %v", err)
	}
	if _, err := s.SummarizeShift(ctx, thread.ID, 1, team.VerdictBlocked, "no access", 0, "", team.ShiftBlocked, "blocked", nil, ReportRow{Key: "sum", ThreadID: thread.ID, SessionID: 60, Text: "summary"}); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := s.FileReport(ctx, "r3", thread.ID, ThreadDone, "done", nil, text); !errors.Is(err, ErrShiftReport) {
		t.Fatalf("done for a blocked shift: %v", err)
	}
	if row, _ := s.Shift(ctx, thread.ID); row.State != team.ShiftSummary {
		t.Fatalf("a refused report ended the shift: %+v", row)
	}
	updated, _, _, err := s.FileReport(ctx, "r4", thread.ID, ThreadBlocked, "blocked", nil, text)
	if err != nil || updated.State != ThreadBlocked {
		t.Fatalf("the summary's report %+v, %v", updated, err)
	}
	if row, _ := s.Shift(ctx, thread.ID); row.State != team.ShiftEnded || row.Reason != team.ShiftBlocked || row.EndedAt == nil {
		t.Fatalf("the summary's report did not end the shift: %+v", row)
	}
	// The same report again replays, and checks nothing again.
	if _, _, _, err := s.FileReport(ctx, "r4", thread.ID, ThreadBlocked, "blocked", nil, text); err != nil {
		t.Fatalf("the report again: %v", err)
	}
}

// The owner's message pauses a running shift; a resume goes on at the next
// round, with its message, keeping the failure count; a closed thread
// ends its shift in the transaction that closes it (ADR-0024).
func TestAShiftPausesResumesAndEndsWithItsThread(t *testing.T) {
	s, _ := open(t)
	thread := newShift(t, s, "d1", 5)
	if _, err := s.NextShiftRound(ctx, thread.ID, 1, team.VerdictDone, "", 2, "lint", roundMessage(thread, 2)); err != nil {
		t.Fatal(err)
	}
	if _, _, ok, err := s.ClaimSend(ctx, "m1", "send:60", thread.ID, "the owner wrote", true); err != nil || !ok {
		t.Fatalf("the owner's message %v, %v", ok, err)
	}
	if paused, _ := s.Shift(ctx, thread.ID); paused.State != team.ShiftPaused || paused.Reason != team.ShiftPerson || paused.Why != "the owner wrote" {
		t.Fatalf("paused %+v", paused)
	}
	if _, _, ok, err := s.ClaimSend(ctx, "m2", "send:60", thread.ID, "again", true); err != nil || ok {
		t.Fatalf("a paused shift paused again: %v, %v", ok, err)
	}
	ok := func(r ShiftRow) bool { return r.State == team.ShiftPaused }
	resumed, _, err := s.ResumeShift(ctx, thread.ID, nil, ok, func(r ShiftRow) ReportRow { return roundMessage(thread, r.Round) })
	if err != nil || resumed.State != team.ShiftRunning || resumed.Round != 3 || resumed.Reason != "" || resumed.Failures != 2 {
		t.Fatalf("resumed %+v, %v", resumed, err)
	}
	if filed, _ := s.ReportFiled(ctx, roundMessage(thread, 3).Key); !filed {
		t.Fatal("the resumed round's message is not kept")
	}
	if _, _, err := s.ResumeShift(ctx, thread.ID, nil, ok, func(r ShiftRow) ReportRow { return roundMessage(thread, r.Round) }); !errors.Is(err, ErrConflict) {
		t.Fatalf("a running shift resumed: %v", err)
	}
	if _, err := s.CloseThreadAs(ctx, "close", thread.ID); err != nil {
		t.Fatal(err)
	}
	closed, err := s.Shift(ctx, thread.ID)
	if err != nil || closed.State != team.ShiftEnded || closed.Reason != team.ShiftClosed || closed.EndedAt == nil {
		t.Fatalf("the closed thread's shift %+v, %v", closed, err)
	}
	// A closed thread takes no message.
	if _, _, err := s.ResumeShift(ctx, thread.ID, nil, func(ShiftRow) bool { return true }, func(r ShiftRow) ReportRow { return roundMessage(thread, r.Round) }); !errors.Is(err, ErrConflict) {
		t.Fatalf("a closed thread's shift resumed: %v", err)
	}
}

// The owner's message pauses a running shift with its first record only:
// the same message tried again, after a resume, pauses nothing; and it
// owes a stop nothing, since the owner directs the session now. A message
// to no shift's session is recorded all the same (Codex review of #36).
func TestTheOwnersMessagePausesOnce(t *testing.T) {
	s, _ := open(t)
	thread := newShift(t, s, "d1", 9)
	if _, _, ok, err := s.ClaimSend(ctx, "m1", "send:60", thread.ID, "the owner wrote", true); err != nil || !ok {
		t.Fatalf("the first record %v, %v", ok, err)
	}
	resume := func(r ShiftRow) bool { return r.State == team.ShiftPaused }
	if _, _, err := s.ResumeShift(ctx, thread.ID, nil, resume, func(r ShiftRow) ReportRow { return roundMessage(thread, r.Round) }); err != nil {
		t.Fatal(err)
	}
	if _, _, ok, err := s.ClaimSend(ctx, "m1", "send:60", thread.ID, "the owner wrote", true); err != nil || ok {
		t.Fatalf("the message tried again paused the resumed shift: %v, %v", ok, err)
	}
	if row, _ := s.Shift(ctx, thread.ID); row.State != team.ShiftRunning {
		t.Fatalf("the shift %+v", row)
	}
	if _, _, _, _, err := s.EndShift(ctx, thread.ID, ShiftEnd{Reason: team.ShiftStopped, Why: "stopped", From: []string{team.ShiftRunning}, Stop: true, Report: shiftReport(thread)}); err != nil {
		t.Fatal(err)
	}
	if _, _, ok, err := s.ClaimSend(ctx, "m2", "send:60", thread.ID, "the owner wrote", true); err != nil || ok {
		t.Fatalf("a message after the stop: %v, %v", ok, err)
	}
	// Only a message that went ends what the stop owes.
	if row, _ := s.Shift(ctx, thread.ID); !row.StopDue {
		t.Fatalf("a claim alone ended what the stop owes: %+v", row)
	}
	if err := s.CompleteSend(ctx, "m2", "8", thread.ID); err != nil {
		t.Fatal(err)
	}
	if row, _ := s.Shift(ctx, thread.ID); row.StopDue || row.State != team.ShiftEnded {
		t.Fatalf("the owner's message left the stop owing its abort: %+v", row)
	}
	if result, _, _ := s.LookupRequest(ctx, "m2", "send:60"); result != "8" {
		t.Fatalf("the message's result %q", result)
	}
	if _, _, ok, err := s.ClaimSend(ctx, "m3", "send:61", 0, "", true); err != nil || ok {
		t.Fatalf("a message to no shift: %v, %v", ok, err)
	}
	if _, existed, _ := s.LookupRequest(ctx, "m3", "send:61"); !existed {
		t.Fatal("a message to no shift was not claimed")
	}
}

// An answer that has not reached its asking session is owed to it.
func TestAnAnswerIsOwedUntilDelivered(t *testing.T) {
	s, _ := open(t)
	thread := newShift(t, s, "d1", 9)
	_, asks, _, err := s.FileReport(ctx, "r1", thread.ID, ThreadOpen, "a question", []AskRow{{Key: "r1:ask:0", SessionID: 60, Question: "Which?"}},
		func([]int64) string { return "report" })
	if err != nil || len(asks) != 1 {
		t.Fatal(asks, err)
	}
	if _, owed, err := s.Owed(ctx, 60); err != nil || len(owed) != 0 {
		t.Fatalf("an open ask owes no answer: %v, %v", owed, err)
	}
	if _, _, err := s.AnswerAsk(ctx, "a1", asks[0].ID, "this one", "owner"); err != nil {
		t.Fatal(err)
	}
	if _, owed, err := s.Owed(ctx, 60); err != nil || len(owed) != 1 {
		t.Fatalf("an answer not delivered: %v, %v", owed, err)
	}
	if err := s.AnswerDelivered(ctx, asks[0].ID); err != nil {
		t.Fatal(err)
	}
	if _, owed, err := s.Owed(ctx, 60); err != nil || len(owed) != 0 {
		t.Fatalf("a delivered answer: %v, %v", owed, err)
	}
}

// The owner's stop drops every message still owed to the specialist and
// its worker in the transaction that ends the shift, and keeps the
// supervisor's report owed (Codex review of #36).
func TestAStopDropsTheWorkOwedToTheSpecialistAndItsWorker(t *testing.T) {
	s, _ := open(t)
	thread := newShift(t, s, "d1", 9)
	if _, err := s.RecordWorkerSession(ctx, "w", SessionRow{ID: 61, Title: "worker", Provider: "faux", Model: "faux-1", CreatedBy: "t"}, thread.ID,
		ReportRow{Key: "worker-msg:1", Text: "Fix it."}); err != nil {
		t.Fatal(err)
	}
	for _, r := range []ReportRow{{Key: "worker-reply:1", SessionID: 60, Text: "Fixed."}, roundMessage(thread, 2)} {
		r.ThreadID = thread.ID
		if _, err := s.QueueWorkerReport(ctx, r, 0); err != nil {
			t.Fatal(err)
		}
	}
	_, _, message, _, err := s.EndShift(ctx, thread.ID, ShiftEnd{Reason: team.ShiftStopped, Why: "stopped", From: []string{team.ShiftRunning}, Stop: true, Report: shiftReport(thread)})
	if err != nil {
		t.Fatal(err)
	}
	unsent, _ := s.UnsentReportsOf(ctx, thread.ID)
	if len(unsent) != 1 || unsent[0].Key != message.Key || unsent[0].SessionID != 1 {
		t.Fatalf("after the stop, owed %+v", unsent)
	}
}

// A stop of a shift that ended already succeeds and claims its request:
// the same request again, after a resume, stops nothing. An end decided at
// a round the shift left is a conflict (Codex review of #36).
func TestAStopOfAnEndedShiftClaimsItsRequest(t *testing.T) {
	s, _ := open(t)
	thread := newShift(t, s, "d1", 9)
	stop := func(key string) (ShiftRow, bool, error) {
		row, _, _, replayed, err := s.EndShift(ctx, thread.ID, ShiftEnd{Reason: team.ShiftStopped, Why: "stopped", From: []string{team.ShiftRunning, team.ShiftPaused},
			Stop: true, Claim: &ShiftClaim{Key: key, Kind: "stopShift:1", KeepOnConflict: true}, Report: shiftReport(thread)})
		return row, replayed, err
	}
	if _, _, err := stop("x1"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := stop("x2"); !errors.Is(err, ErrConflict) {
		t.Fatalf("a stop of an ended shift: %v", err)
	}
	ok := func(r ShiftRow) bool { return r.State == team.ShiftEnded }
	if _, _, err := s.ResumeShift(ctx, thread.ID, nil, ok, func(r ShiftRow) ReportRow { return roundMessage(thread, r.Round) }); err != nil {
		t.Fatal(err)
	}
	row, replayed, err := stop("x2")
	if err != nil || !replayed || row.State != team.ShiftRunning {
		t.Fatalf("the no-op stop tried again: %+v %v %v", row, replayed, err)
	}
	if _, _, _, _, err := s.EndShift(ctx, thread.ID, ShiftEnd{Reason: team.ShiftFailed, Why: "failed", From: []string{team.ShiftRunning}, Round: 1, Report: shiftReport(thread)}); !errors.Is(err, ErrConflict) {
		t.Fatalf("an end decided at round 1 ended round 2: %v", err)
	}
	if _, _, _, _, err := s.EndShift(ctx, thread.ID, ShiftEnd{Reason: team.ShiftFailed, Why: "failed", From: []string{team.ShiftRunning}, Round: 2, Report: shiftReport(thread)}); err != nil {
		t.Fatalf("an end at its own round: %v", err)
	}
}

// A thread that closes while its shift is not ended tells the supervisor
// why the shift ended, by a note in the transaction that closes it; one
// whose shift ended already gets none (Codex review of #36).
func TestAClosedShiftTellsItsSupervisor(t *testing.T) {
	s, _ := open(t)
	live := newShift(t, s, "d1", 9)
	if _, err := s.CloseThreadAs(ctx, "c1", live.ID); err != nil {
		t.Fatal(err)
	}
	note, err := s.Note(ctx, "shift-closed:"+live.Owner+":1")
	if err != nil || note.SessionID != 1 || !strings.Contains(note.Text, "The shift ended: its thread closed. It took 1 of 9 rounds.") {
		t.Fatalf("the note %+v, %v", note, err)
	}
	ended := newShift(t, s, "d2", 9)
	if _, _, _, _, err := s.EndShift(ctx, ended.ID, ShiftEnd{Reason: team.ShiftStopped, Why: "stopped", From: []string{team.ShiftRunning}, Report: shiftReport(ended)}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CloseThreadAs(ctx, "c2", ended.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Note(ctx, "shift-closed:"+ended.Owner+":1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an ended shift's close noted: %v", err)
	}
}

// A final report the summary round admitted is filed only while the shift
// is still in that round: a stop between the check and the filing wins. A
// report checked after the shift ended goes as any (Codex review of #36).
func TestASummaryReportOvertakenByAStopIsRefused(t *testing.T) {
	s, _ := open(t)
	thread := newShift(t, s, "d1", 9)
	if _, err := s.SummarizeShift(ctx, thread.ID, 1, team.VerdictDone, "", 0, "", team.ShiftDone, "done", nil, ReportRow{Key: "sum", ThreadID: thread.ID, SessionID: 60, Text: "summary"}); err != nil {
		t.Fatal(err)
	}
	seen := ShiftSeen{State: team.ShiftSummary, Round: 1}
	if _, _, _, _, err := s.EndShift(ctx, thread.ID, ShiftEnd{Reason: team.ShiftStopped, Why: "stopped", From: []string{team.ShiftSummary}, Report: shiftReport(thread)}); err != nil {
		t.Fatal(err)
	}
	text := func([]int64, []PullRow) string { return "report" }
	if _, _, _, _, err := s.FileReportOnShift(ctx, "r1", thread.ID, ThreadDone, "done", nil, nil, seen, text); !errors.Is(err, ErrShiftReport) {
		t.Fatalf("a summary report after the stop: %v", err)
	}
	if updated, _, _, _, err := s.FileReportOnShift(ctx, "r2", thread.ID, ThreadDone, "done", nil, nil, ShiftSeen{State: team.ShiftEnded, Round: 1}, text); err != nil || updated.State != ThreadDone {
		t.Fatalf("a report after the shift ended: %+v, %v", updated, err)
	}
}

// A shift's change moves its thread's updated_at, which orders the
// threads; the list reads each thread with its shift in one transaction
// (Codex review of #36).
func TestAShiftChangeMovesItsThread(t *testing.T) {
	s, _ := open(t)
	thread := newShift(t, s, "d1", 9)
	if _, err := s.NextShiftRound(ctx, thread.ID, 1, team.VerdictContinue, "", 0, "", roundMessage(thread, 2)); err != nil {
		t.Fatal(err)
	}
	threads, shifts, err := s.ThreadsWithShifts(ctx)
	if err != nil || len(threads) != 1 || !threads[0].UpdatedAt.After(thread.UpdatedAt) || shifts[thread.ID].Round != 2 {
		t.Fatalf("threads %+v shifts %+v, %v", threads, shifts, err)
	}
}

// A thread is read with its shift in one transaction, and with none for a
// thread that runs no shift (Codex review of #36).
func TestAThreadIsReadWithItsShift(t *testing.T) {
	s, _ := open(t)
	thread := newShift(t, s, "d1", 9)
	got, sh, err := s.ThreadWithShift(ctx, thread.ID)
	if err != nil || got.ID != thread.ID || sh == nil || sh.MaxRounds != 9 {
		t.Fatalf("%+v %+v %v", got, sh, err)
	}
	plain, _, err := s.CreateThread(ctx, ThreadRow{Key: "d2", Title: "t", Agent: "vera", SupervisorSession: 1, Task: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if _, sh, err := s.ThreadWithShift(ctx, plain.ID); err != nil || sh != nil {
		t.Fatalf("a thread with no shift %+v %v", sh, err)
	}
	if _, _, err := s.ThreadWithShift(ctx, 99); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
}

// The live shifts are read through their partial index, never every shift
// that ended (Codex review of #36).
func TestLiveShiftsUseTheirIndex(t *testing.T) {
	s, _ := open(t)
	if plan := planOf(t, s, `SELECT thread_id FROM shifts WHERE `+liveShiftPredicate+` ORDER BY thread_id`); !strings.Contains(plan, "shifts_live") {
		t.Fatalf("the plan %q", plan)
	}
}

// A shift's round moves only while its sessions are owed nothing, checked
// in the transaction that moves it: an answer or a message recorded after
// the round was read holds it (Codex review of #36).
func TestARoundMovesOnlyWhileItsSessionsAreOwedNothing(t *testing.T) {
	s, _ := open(t)
	thread := newShift(t, s, "d1", 9)
	other := newShift(t, s, "d2", 9)
	if _, err := s.QueueWorkerReport(ctx, ReportRow{Key: "verdict", ThreadID: other.ID, SessionID: 60, Text: "changes requested"}, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.NextShiftRound(ctx, thread.ID, 1, team.VerdictContinue, "", 0, "", roundMessage(thread, 2), 60); !errors.Is(err, ErrConflict) {
		t.Fatalf("a round moved with a message owed: %v", err)
	}
	if _, err := s.SummarizeShift(ctx, thread.ID, 1, team.VerdictDone, "", 0, "", team.ShiftDone, "done", nil, roundMessage(thread, 2), 60); !errors.Is(err, ErrConflict) {
		t.Fatalf("a summary with a message owed: %v", err)
	}
	if _, _, _, _, err := s.EndShift(ctx, thread.ID, ShiftEnd{Reason: team.ShiftFailed, Why: "failed", From: []string{team.ShiftRunning}, Round: 1, Quiet: []int64{60},
		Report: shiftReport(thread)}); !errors.Is(err, ErrConflict) {
		t.Fatalf("an end with a message owed: %v", err)
	}
	if err := s.ReportSent(ctx, "verdict"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.NextShiftRound(ctx, thread.ID, 1, team.VerdictContinue, "", 0, "", roundMessage(thread, 2), 60); err != nil {
		t.Fatalf("a round owed nothing: %v", err)
	}
}

// A stop owes its abort until the owner's message it recorded as claimed
// goes: a message tried again after it went before the stop ends nothing,
// and one found in PiG after a stop of pocketd ends it (Codex review of
// #36).
func TestOnlyTheMessageAStopRecordedEndsIt(t *testing.T) {
	s, _ := open(t)
	thread := newShift(t, s, "d1", 9)
	if _, _, _, err := s.ClaimSend(ctx, "m1", "send:60", thread.ID, "the owner wrote", true); err != nil {
		t.Fatal(err)
	}
	if err := s.CompleteSend(ctx, "m1", "5", thread.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, _, _, err := s.EndShift(ctx, thread.ID, ShiftEnd{Reason: team.ShiftStopped, Why: "stopped", From: []string{team.ShiftPaused}, Stop: true, Report: shiftReport(thread)}); err != nil {
		t.Fatal(err)
	}
	// m1 tried again: it went before the stop.
	if _, _, _, err := s.ClaimSend(ctx, "m1", "send:60", thread.ID, "the owner wrote", true); err != nil {
		t.Fatal(err)
	}
	if err := s.CompleteSend(ctx, "m1", "5", thread.ID); err != nil {
		t.Fatal(err)
	}
	if row, _ := s.Shift(ctx, thread.ID); !row.StopDue || row.StopSend != "" {
		t.Fatalf("a message tried again ended the stop: %+v", row)
	}
	// m2 is claimed, and pocketd stops before it records that it went.
	if _, _, _, err := s.ClaimSend(ctx, "m2", "send:60", thread.ID, "the owner wrote", true); err != nil {
		t.Fatal(err)
	}
	if row, _ := s.Shift(ctx, thread.ID); row.StopSend != "m2" {
		t.Fatalf("the claim was not recorded: %+v", row)
	}
	if err := s.DropStopSend(ctx, thread.ID, "m2"); err != nil {
		t.Fatal(err)
	}
	if row, _ := s.Shift(ctx, thread.ID); !row.StopDue || row.StopSend != "" {
		t.Fatalf("a message that did not go: %+v", row)
	}
	if _, _, _, err := s.ClaimSend(ctx, "m3", "send:60", thread.ID, "the owner wrote", true); err != nil {
		t.Fatal(err)
	}
	if err := s.StopSuperseded(ctx, thread.ID, "m3"); err != nil {
		t.Fatal(err)
	}
	if row, _ := s.Shift(ctx, thread.ID); row.StopDue || row.StopSend != "" {
		t.Fatalf("a message PiG has did not end the stop: %+v", row)
	}
}

// The run that filed the summary's report files no second final one; a
// later run reports as any (Codex review of #36).
func TestTheSummarysRunFilesOneFinalReport(t *testing.T) {
	s, _ := open(t)
	thread := newShift(t, s, "d1", 9)
	if _, err := s.SummarizeShift(ctx, thread.ID, 1, team.VerdictBlocked, "no key", 0, "", team.ShiftBlocked, "blocked", nil, ReportRow{Key: "sum", ThreadID: thread.ID, SessionID: 60, Text: "summary"}); err != nil {
		t.Fatal(err)
	}
	text := func([]int64, []PullRow) string { return "report" }
	if _, _, _, _, err := s.FileReportOnShift(ctx, "r1", thread.ID, ThreadBlocked, "blocked", nil, nil, ShiftSeen{State: team.ShiftSummary, Round: 1, Run: 7}, text); err != nil {
		t.Fatal(err)
	}
	if _, _, _, _, err := s.FileReportOnShift(ctx, "r2", thread.ID, ThreadDone, "done", nil, nil, ShiftSeen{State: team.ShiftEnded, Round: 1, Run: 7}, text); !errors.Is(err, ErrShiftReport) {
		t.Fatalf("a second final report from the summary's run: %v", err)
	}
	if updated, _, _, _, err := s.FileReportOnShift(ctx, "r3", thread.ID, ThreadDone, "done", nil, nil, ShiftSeen{State: team.ShiftEnded, Round: 1, Run: 9}, text); err != nil || updated.State != ThreadDone {
		t.Fatalf("a later run's report: %+v %v", updated, err)
	}
}

// A report checked in the summary round, a working one included, goes only
// while the shift is still in that round; the run that filed the summary's
// report files no other (Codex review of #36).
func TestAWorkingReportOvertakenByTheSummarysIsRefused(t *testing.T) {
	s, _ := open(t)
	thread := newShift(t, s, "d1", 9)
	if _, err := s.SummarizeShift(ctx, thread.ID, 1, team.VerdictBlocked, "no key", 0, "", team.ShiftBlocked, "blocked", nil, ReportRow{Key: "sum", ThreadID: thread.ID, SessionID: 60, Text: "summary"}); err != nil {
		t.Fatal(err)
	}
	text := func([]int64, []PullRow) string { return "report" }
	if _, _, _, _, err := s.FileReportOnShift(ctx, "r1", thread.ID, ThreadBlocked, "blocked", nil, nil, ShiftSeen{State: team.ShiftSummary, Round: 1, Run: 7}, text); err != nil {
		t.Fatal(err)
	}
	for name, seen := range map[string]ShiftSeen{
		"checked in the summary": {State: team.ShiftSummary, Round: 1, Run: 8},
		"the summary's run":      {State: team.ShiftEnded, Round: 1, Run: 7},
	} {
		if _, _, _, _, err := s.FileReportOnShift(ctx, "w-"+name, thread.ID, ThreadOpen, "half way", nil, nil, seen, text); !errors.Is(err, ErrShiftReport) {
			t.Errorf("%s: a working report after the summary's: %v", name, err)
		}
	}
	if row, _ := s.Thread(ctx, thread.ID); row.State != ThreadBlocked {
		t.Fatalf("a working report undid the summary's: %+v", row)
	}
	if _, _, _, _, err := s.FileReportOnShift(ctx, "w-later", thread.ID, ThreadOpen, "back at it", nil, nil, ShiftSeen{State: team.ShiftEnded, Round: 1, Run: 9}, text); err != nil {
		t.Fatalf("a later run's working report: %v", err)
	}
}

// A worker started by a call a stop overtook records nothing (Codex
// review of #36).
func TestAStopOvertakesAWorkerStart(t *testing.T) {
	s, _ := open(t)
	thread := newShift(t, s, "d1", 9)
	stops, err := s.ShiftStops(ctx, thread.ID)
	if err != nil || stops != 0 {
		t.Fatal(stops, err)
	}
	if _, _, _, _, err := s.EndShift(ctx, thread.ID, ShiftEnd{Reason: team.ShiftStopped, Why: "stopped", From: []string{team.ShiftRunning}, Stop: true, Report: shiftReport(thread)}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecordWorkerSessionAt(ctx, "w", SessionRow{ID: 61, Title: "worker", Provider: "faux", Model: "faux-1", CreatedBy: "t"}, thread.ID,
		ReportRow{Key: "worker-msg:1", Text: "Fix it."}, stops); !errors.Is(err, ErrShiftStopped) {
		t.Fatalf("a worker started after the stop: %v", err)
	}
	if now, _ := s.ShiftStops(ctx, thread.ID); now != 1 {
		t.Fatalf("stops %d", now)
	}
}
