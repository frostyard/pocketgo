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
	sum, err := s.SummarizeShift(ctx, thread.ID, 3, team.VerdictContinue, "", team.ShiftMaxRounds, why, ReportRow{Key: "shift-summary:" + thread.Owner, ThreadID: thread.ID, SessionID: 60, Text: "summary"})
	if err != nil || sum.State != team.ShiftSummary || sum.Reason != team.ShiftMaxRounds || sum.Why != why || len(sum.Rounds) != 3 {
		t.Fatalf("the summary %+v, %v", sum, err)
	}
	ended, err := s.EndShift(ctx, thread.ID, team.ShiftMaxRounds, why, team.ShiftSummary)
	if err != nil || ended.State != team.ShiftEnded || ended.EndedAt == nil {
		t.Fatalf("the end %+v, %v", ended, err)
	}
	if _, err := s.EndShift(ctx, thread.ID, team.ShiftStopped, "x", team.ShiftRunning, team.ShiftSummary); !errors.Is(err, ErrConflict) {
		t.Fatalf("an ended shift ended again: %v", err)
	}
	if live, _ := s.LiveShifts(ctx); len(live) != 0 {
		t.Fatalf("live %+v", live)
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
	paused, err := s.PauseShift(ctx, thread.ID, "the owner wrote")
	if err != nil || paused.State != team.ShiftPaused || paused.Reason != team.ShiftPerson {
		t.Fatalf("paused %+v, %v", paused, err)
	}
	if _, err := s.PauseShift(ctx, thread.ID, "again"); !errors.Is(err, ErrConflict) {
		t.Fatalf("a paused shift paused again: %v", err)
	}
	ok := func(r ShiftRow) bool { return r.State == team.ShiftPaused }
	resumed, err := s.ResumeShift(ctx, thread.ID, ok, func(r ShiftRow) ReportRow { return roundMessage(thread, r.Round) })
	if err != nil || resumed.State != team.ShiftRunning || resumed.Round != 3 || resumed.Reason != "" || resumed.Failures != 2 {
		t.Fatalf("resumed %+v, %v", resumed, err)
	}
	if filed, _ := s.ReportFiled(ctx, roundMessage(thread, 3).Key); !filed {
		t.Fatal("the resumed round's message is not kept")
	}
	if _, err := s.ResumeShift(ctx, thread.ID, ok, func(r ShiftRow) ReportRow { return roundMessage(thread, r.Round) }); !errors.Is(err, ErrConflict) {
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
	if _, err := s.ResumeShift(ctx, thread.ID, func(ShiftRow) bool { return true }, func(r ShiftRow) ReportRow { return roundMessage(thread, r.Round) }); !errors.Is(err, ErrConflict) {
		t.Fatalf("a closed thread's shift resumed: %v", err)
	}
}
