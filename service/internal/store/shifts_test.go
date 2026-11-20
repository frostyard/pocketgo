package store

import (
	"errors"
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
	sum, err := s.SummarizeShift(ctx, thread.ID, 3, team.VerdictContinue, "", 0, "", team.ShiftMaxRounds, why, ReportRow{Key: "shift-summary:" + thread.Owner, ThreadID: thread.ID, SessionID: 60, Text: "summary"})
	if err != nil || sum.State != team.ShiftSummary || sum.Reason != team.ShiftMaxRounds || sum.Why != why || len(sum.Rounds) != 3 || sum.Failures != 0 {
		t.Fatalf("the summary %+v, %v", sum, err)
	}
	ended, updated, message, replayed, err := s.EndShift(ctx, thread.ID, team.ShiftMaxRounds, why, false, nil, shiftReport(thread), team.ShiftSummary)
	if err != nil || replayed || ended.State != team.ShiftEnded || ended.EndedAt == nil || updated.State != ThreadBlocked || message.SessionID != 1 {
		t.Fatalf("the end %+v %+v %+v %v %v", ended, updated, message, replayed, err)
	}
	if _, _, _, _, err := s.EndShift(ctx, thread.ID, team.ShiftStopped, "x", false, nil, shiftReport(thread), team.ShiftRunning, team.ShiftSummary); !errors.Is(err, ErrConflict) {
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
	sum, err := s.SummarizeShift(ctx, thread.ID, 2, team.VerdictDone, "", 3, "lint", team.ShiftFailedCheck, "why", ReportRow{Key: "sum", ThreadID: thread.ID, SessionID: 60, Text: "summary"})
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
	if _, _, _, _, err := s.EndShift(ctx, thread.ID, team.ShiftStopped, "stopped", true, nil, shiftReport(thread), team.ShiftRunning); !errors.Is(err, ErrConflict) {
		t.Fatalf("an end whose report fails: %v", err)
	}
	if row, _ := s.Shift(ctx, thread.ID); row.State != team.ShiftRunning || row.StopDue {
		t.Fatalf("an end without its report changed the shift: %+v", row)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE threads SET state = ? WHERE id = ?`, ThreadOpen, thread.ID); err != nil {
		t.Fatal(err)
	}
	ended, updated, message, _, err := s.EndShift(ctx, thread.ID, team.ShiftStopped, "stopped", true, nil, shiftReport(thread), team.ShiftRunning)
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
	if _, _, _, _, err := s.EndShift(ctx, thread.ID, team.ShiftStopped, "stopped", true, nil, shiftReport(thread), team.ShiftRunning); err != nil {
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
	if _, _, _, _, err := s.EndShift(ctx, thread2.ID, team.ShiftStopped, "stopped", true, nil, shiftReport(thread2), team.ShiftRunning); err != nil {
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
	if _, err := s.RecordSend(ctx, "m1", "send:60", "5", thread.ID, "the owner wrote"); err != nil {
		t.Fatal(err)
	}
	claim := &ShiftClaim{Key: "r1", Kind: "shift-resume:1"}
	ok := func(r ShiftRow) bool { return r.State == team.ShiftPaused }
	message := func(r ShiftRow) ReportRow { return roundMessage(thread, r.Round) }
	resumed, replayed, err := s.ResumeShift(ctx, thread.ID, claim, ok, message)
	if err != nil || replayed || resumed.Round != 2 {
		t.Fatalf("resumed %+v %v %v", resumed, replayed, err)
	}
	if ok, err := s.RecordSend(ctx, "m2", "send:60", "6", thread.ID, "the owner wrote again"); err != nil || !ok {
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
	_, _, _, replayed, err = s.EndShift(ctx, thread.ID, team.ShiftStopped, "stopped", false, &ShiftClaim{Key: "r3", Kind: "shift-stop:1"}, shiftReport(thread), team.ShiftPaused)
	if err != nil || replayed {
		t.Fatalf("the stop %v %v", replayed, err)
	}
	_, _, _, replayed, err = s.EndShift(ctx, thread.ID, team.ShiftStopped, "stopped", false, &ShiftClaim{Key: "r3", Kind: "shift-stop:1"}, shiftReport(thread), team.ShiftPaused)
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
	if _, err := s.SummarizeShift(ctx, thread.ID, 1, team.VerdictBlocked, "no access", 0, "", team.ShiftBlocked, "blocked", ReportRow{Key: "sum", ThreadID: thread.ID, SessionID: 60, Text: "summary"}); err != nil {
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
	if ok, err := s.RecordSend(ctx, "m1", "send:60", "5", thread.ID, "the owner wrote"); err != nil || !ok {
		t.Fatalf("the owner's message %v, %v", ok, err)
	}
	if paused, _ := s.Shift(ctx, thread.ID); paused.State != team.ShiftPaused || paused.Reason != team.ShiftPerson || paused.Why != "the owner wrote" {
		t.Fatalf("paused %+v", paused)
	}
	if ok, err := s.RecordSend(ctx, "m2", "send:60", "6", thread.ID, "again"); err != nil || ok {
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
	if ok, err := s.RecordSend(ctx, "m1", "send:60", "5", thread.ID, "the owner wrote"); err != nil || !ok {
		t.Fatalf("the first record %v, %v", ok, err)
	}
	resume := func(r ShiftRow) bool { return r.State == team.ShiftPaused }
	if _, _, err := s.ResumeShift(ctx, thread.ID, nil, resume, func(r ShiftRow) ReportRow { return roundMessage(thread, r.Round) }); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.RecordSend(ctx, "m1", "send:60", "5", thread.ID, "the owner wrote"); err != nil || ok {
		t.Fatalf("the message tried again paused the resumed shift: %v, %v", ok, err)
	}
	if row, _ := s.Shift(ctx, thread.ID); row.State != team.ShiftRunning {
		t.Fatalf("the shift %+v", row)
	}
	if _, _, _, _, err := s.EndShift(ctx, thread.ID, team.ShiftStopped, "stopped", true, nil, shiftReport(thread), team.ShiftRunning); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.RecordSend(ctx, "m2", "send:60", "6", thread.ID, "the owner wrote"); err != nil || ok {
		t.Fatalf("a message after the stop: %v, %v", ok, err)
	}
	if row, _ := s.Shift(ctx, thread.ID); row.StopDue || row.State != team.ShiftEnded {
		t.Fatalf("the owner's message left the stop owing its abort: %+v", row)
	}
	if ok, err := s.RecordSend(ctx, "m3", "send:61", "7", 0, ""); err != nil || ok {
		t.Fatalf("a message to no shift: %v, %v", ok, err)
	}
	if _, existed, _ := s.LookupRequest(ctx, "m3", "send:61"); !existed {
		t.Fatal("a message to no shift was not recorded")
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
	if owed, err := s.AnswerOwed(ctx, 60); err != nil || owed {
		t.Fatalf("an open ask owes no answer: %v, %v", owed, err)
	}
	if _, _, err := s.AnswerAsk(ctx, "a1", asks[0].ID, "this one", "owner"); err != nil {
		t.Fatal(err)
	}
	if owed, err := s.AnswerOwed(ctx, 60); err != nil || !owed {
		t.Fatalf("an answer not delivered: %v, %v", owed, err)
	}
	if err := s.AnswerDelivered(ctx, asks[0].ID); err != nil {
		t.Fatal(err)
	}
	if owed, err := s.AnswerOwed(ctx, 60); err != nil || owed {
		t.Fatalf("a delivered answer: %v, %v", owed, err)
	}
}
