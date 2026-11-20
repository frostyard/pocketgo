package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/frostyard/pocketgo/proto"
	"github.com/frostyard/pocketgo/service/internal/github"
	"github.com/frostyard/pocketgo/service/internal/store"
	"github.com/frostyard/pocketgo/service/internal/team"
)

// The keys of a shift's messages in the outbox (ADR-0024): each round's,
// and each summary's, to the specialist; and the report pocketd files
// when a shift ends with none.
const (
	shiftRoundPrefix   = "shift-round:"
	shiftSummaryPrefix = "shift-summary:"
	shiftEndPrefix     = "shift-end:"
)

func shiftRoundKey(thread store.ThreadRow, n int) string {
	return shiftRoundPrefix + thread.Owner + ":" + strconv.Itoa(n)
}

func shiftSummaryKey(thread store.ThreadRow, n int) string {
	return shiftSummaryPrefix + thread.Owner + ":" + strconv.Itoa(n)
}

// isShiftMessage says whether key is a shift's message to its
// specialist, which a closed thread's outbox drops.
func isShiftMessage(key string) bool {
	return strings.HasPrefix(key, shiftRoundPrefix) || strings.HasPrefix(key, shiftSummaryPrefix)
}

// wireShift is a shift as the protocol carries it.
func wireShift(row store.ShiftRow) *proto.Shift {
	out := &proto.Shift{Stop: row.Stop, MaxRounds: row.MaxRounds, CI: row.CI, State: proto.ShiftState(row.State), Reason: row.Reason,
		Why: row.Why, Round: row.Round, Failures: row.Failures, EndedAt: row.EndedAt}
	if !row.Deadline.IsZero() {
		deadline := row.Deadline
		out.Deadline = &deadline
	}
	for _, r := range row.Rounds {
		out.Rounds = append(out.Rounds, proto.ShiftRound{N: r.N, Verdict: r.Verdict, Reason: r.Reason, At: r.At})
	}
	return out
}

// wireThreadOf is a thread as the protocol carries it, with its shift: one
// that cannot be read now is left out, and goes with the thread's next
// event.
func (s *Server) wireThreadOf(ctx context.Context, row store.ThreadRow) proto.Thread {
	out := wireThread(row)
	if sh, err := s.store.Shift(ctx, row.ID); err == nil {
		out.Shift = wireShift(sh)
	} else if !errors.Is(err, store.ErrNotFound) {
		s.log.Warn("read a thread's shift", "thread", row.ID, "err", err)
	}
	return out
}

// shiftSoon runs a shift pass off the caller's path: after a run of a
// specialist or of its worker that ended, after a pull request round, and
// at the service's start.
func (s *Server) shiftSoon() {
	s.sessionMu.Lock()
	defer s.sessionMu.Unlock()
	if s.stopped {
		return
	}
	s.passes.Add(1)
	go func() {
		defer s.passes.Done()
		s.shiftPass(s.passCtx)
	}()
}

// shiftPass drives each live shift one step, one pass at a time: what
// cannot be read now waits for the next pass.
func (s *Server) shiftPass(ctx context.Context) {
	s.shiftMu.Lock()
	defer s.shiftMu.Unlock()
	live, err := s.store.LiveShifts(ctx)
	if err != nil {
		s.log.Warn("read the live shifts", "err", err)
		return
	}
	for _, sh := range live {
		if err := s.driveShift(ctx, sh); err != nil {
			s.log.Warn("drive a shift", "thread", sh.ThreadID, "err", err)
		}
	}
	if s.afterShiftPass != nil {
		s.afterShiftPass()
	}
}

// driveShift ends the round under way of a live shift, if it ended, and
// decides what follows, in code (ADR-0024): the summary round, or the next
// round. A round whose answer waits on CI is left for a later pass.
func (s *Server) driveShift(ctx context.Context, sh store.ShiftRow) error {
	thread, err := s.store.Thread(ctx, sh.ThreadID)
	if err != nil {
		return err
	}
	if thread.State == store.ThreadClosed || thread.Session == 0 || !thread.Briefed {
		return nil
	}
	answer, ended, failure, err := s.roundAnswer(ctx, thread, sh)
	if err != nil || !ended {
		return err
	}
	if sh.State == team.ShiftSummary {
		// The summary round ended with no report: a report ends the shift
		// as it is filed (TeamMessage).
		return s.endShift(ctx, thread, sh, sh.Reason, sh.Why, true, team.ShiftSummary)
	}
	if strings.TrimSpace(answer) == "" && failure != "" {
		return s.endShift(ctx, thread, sh, team.ShiftFailed, team.ShiftWhy(team.ShiftFailed, failure, sh.Round, sh.MaxRounds), true, team.ShiftRunning)
	}
	verdict, reason := team.ParseShiftStatus(answer)
	switch verdict {
	case team.VerdictBlocked:
		return s.summarizeShift(ctx, thread, sh, verdict, reason, team.ShiftBlocked, reason)
	case team.VerdictDone:
		if !sh.CI {
			return s.summarizeShift(ctx, thread, sh, verdict, reason, team.ShiftDone, "")
		}
		state, failing, err := s.threadCI(ctx, thread)
		if err != nil {
			return err
		}
		switch state {
		case github.CIGreen:
			return s.summarizeShift(ctx, thread, sh, verdict, reason, team.ShiftDone, "")
		case github.CIPending:
			return nil // the next pass, after a round of the pull requests, reads CI again
		}
		fingerprint := team.ShiftFingerprint(failing)
		failures := 1
		if fingerprint == sh.Fingerprint {
			failures = sh.Failures + 1
		}
		if failures >= team.ShiftFailures {
			return s.summarizeShift(ctx, thread, sh, verdict, reason, team.ShiftFailedCheck, strings.Join(failing, ", "))
		}
		if stop, ok := s.shiftLimit(sh); ok {
			return s.summarizeShift(ctx, thread, sh, verdict, reason, stop, "")
		}
		return s.nextShiftRound(ctx, thread, sh, verdict, reason, failures, fingerprint, failing)
	}
	if stop, ok := s.shiftLimit(sh); ok {
		return s.summarizeShift(ctx, thread, sh, verdict, reason, stop, "")
	}
	return s.nextShiftRound(ctx, thread, sh, verdict, reason, sh.Failures, sh.Fingerprint, nil)
}

// shiftLimit says whether sh reached its round limit or its deadline,
// checked before a round starts, so a round that runs finishes.
func (s *Server) shiftLimit(sh store.ShiftRow) (string, bool) {
	switch {
	case sh.Round >= sh.MaxRounds:
		return team.ShiftMaxRounds, true
	case !sh.Deadline.IsZero() && !s.now().Before(sh.Deadline):
		return team.ShiftDeadline, true
	}
	return "", false
}

// roundAnswer reads the round under way of a live shift: whether it ended,
// its answer, the model's last text after the message that began it, and
// the failure of the run that ended it. A round ends when its message is
// in the specialist's transcript, the specialist and its worker are idle,
// and nothing is owed to either: a round that waits on its worker, or on a
// message pocketd still sends, is not over.
func (s *Server) roundAnswer(ctx context.Context, thread store.ThreadRow, sh store.ShiftRow) (answer string, ended bool, failure string, err error) {
	submission := thread.BriefSubmission
	if key := shiftMessageKey(thread, sh); key != "" {
		row, err := s.store.Report(ctx, key)
		if errors.Is(err, store.ErrNotFound) {
			return "", false, "", nil
		} else if err != nil {
			return "", false, "", err
		}
		if row.SentAt == nil {
			return "", false, "", nil
		}
		submission = row.Submission
	}
	unsent, err := s.store.UnsentReportsOf(ctx, thread.ID)
	if err != nil {
		return "", false, "", err
	}
	for _, r := range unsent {
		if r.SessionID == thread.Session || (thread.WorkerSession != 0 && r.SessionID == thread.WorkerSession) {
			return "", false, "", nil
		}
	}
	if thread.WorkerSession != 0 {
		if busy, err := s.engine.Busy(ctx, thread.WorkerSession); err != nil || busy {
			return "", false, "", err
		}
	}
	entries, live, err := s.engine.TranscriptAfter(ctx, thread.Session, 0)
	if err != nil || live.Busy {
		return "", false, "", err
	}
	anchor := -1
	for i, e := range entries {
		if e.Kind == proto.EntryUser && submission != 0 && e.SubmissionID == submission {
			anchor = i
		}
	}
	if anchor < 0 {
		return "", false, "", nil // its message is not in the transcript yet
	}
	for i := len(entries) - 1; i > anchor; i-- {
		if e := entries[i]; e.Kind == proto.EntryAssistant && strings.TrimSpace(e.Text) != "" {
			return e.Text, true, live.Failure, nil
		}
	}
	return "", true, live.Failure, nil
}

// shiftMessageKey is the key of the message that began sh's round under
// way: none for round 1, which its brief began.
func shiftMessageKey(thread store.ThreadRow, sh store.ShiftRow) string {
	switch {
	case sh.State == team.ShiftSummary:
		return shiftSummaryKey(thread, sh.Round)
	case sh.Round > 1:
		return shiftRoundKey(thread, sh.Round)
	}
	return ""
}

// threadCI is CI on the pull requests thread links that are not closed:
// green only with at least one and all green, failing with the checks that
// fail (each named with its pull request), else pending.
func (s *Server) threadCI(ctx context.Context, thread store.ThreadRow) (string, []string, error) {
	pulls, err := s.store.PullsOfThread(ctx, thread.ID)
	if err != nil {
		return "", nil, err
	}
	state, any := github.CIGreen, false
	var failing []string
	for _, p := range pulls {
		if p.State == store.PullClosed {
			continue
		}
		any = true
		checks, names, err := github.CIChecks(ctx, s.engine.GH, p.Repository, p.HeadSHA)
		if err != nil {
			return "", nil, err
		}
		ref := p.Repository + "#" + strconv.FormatInt(p.Number, 10)
		switch checks {
		case github.CIFailing:
			for _, n := range names {
				failing = append(failing, ref+" "+n)
			}
		case github.CINone:
			failing = append(failing, ref+" has no checks")
		case github.CIPending:
			if state == github.CIGreen {
				state = github.CIPending
			}
		}
	}
	switch {
	case !any:
		return github.CIFailing, []string{"no pull request is reported for the thread"}, nil
	case len(failing) > 0:
		return github.CIFailing, failing, nil
	}
	return state, nil, nil
}

// nextShiftRound ends sh's round with its verdict and starts the next: its
// message kept with the round's change, then sent.
func (s *Server) nextShiftRound(ctx context.Context, thread store.ThreadRow, sh store.ShiftRow, verdict, reason string, failures int, fingerprint string, failing []string) error {
	n := sh.Round + 1
	message := store.ReportRow{Key: shiftRoundKey(thread, n), ThreadID: thread.ID, SessionID: thread.Session,
		Text: team.ShiftRoundText(thread.ID, n, sh.MaxRounds, sh.Deadline, sh.Stop, failing)}
	s.workMu.Lock()
	_, err := s.store.NextShiftRound(ctx, thread.ID, sh.Round, verdict, reason, failures, fingerprint, message)
	if err == nil {
		s.publishThread(thread)
	}
	s.workMu.Unlock()
	if errors.Is(err, store.ErrConflict) {
		return nil // the shift moved meanwhile: the next pass reads it again
	} else if err != nil {
		return err
	}
	s.sendShiftMessages(ctx, thread.ID)
	return nil
}

// summarizeShift stops sh for stop, with detail, and asks the specialist
// for its report in a summary round.
func (s *Server) summarizeShift(ctx context.Context, thread store.ThreadRow, sh store.ShiftRow, verdict, reason, stop, detail string) error {
	why := team.ShiftWhy(stop, detail, sh.Round, sh.MaxRounds)
	message := store.ReportRow{Key: shiftSummaryKey(thread, sh.Round), ThreadID: thread.ID, SessionID: thread.Session,
		Text: team.ShiftSummaryText(thread.ID, why, sh.Round, sh.MaxRounds, stop)}
	s.workMu.Lock()
	_, err := s.store.SummarizeShift(ctx, thread.ID, sh.Round, verdict, reason, stop, why, message)
	if err == nil {
		s.publishThread(thread)
	}
	s.workMu.Unlock()
	if errors.Is(err, store.ErrConflict) {
		return nil
	} else if err != nil {
		return err
	}
	s.sendShiftMessages(ctx, thread.ID)
	return nil
}

// endShift ends sh for reason and why, from a state of from. With report,
// pocketd files the report the specialist did not: the supervisor hears
// once why the shift ended.
func (s *Server) endShift(ctx context.Context, thread store.ThreadRow, sh store.ShiftRow, reason, why string, report bool, from ...string) error {
	s.workMu.Lock()
	ended, err := s.store.EndShift(ctx, thread.ID, reason, why, from...)
	if err == nil {
		s.publishThread(thread)
	}
	s.workMu.Unlock()
	if errors.Is(err, store.ErrConflict) || err != nil && !report {
		return nil
	} else if err != nil {
		return err
	}
	if !report {
		return nil
	}
	return s.fileShiftReport(ctx, thread, ended)
}

// fileShiftReport files pocketd's report of an ended shift on its thread,
// once, and sends it to the supervisor.
func (s *Server) fileShiftReport(ctx context.Context, thread store.ThreadRow, sh store.ShiftRow) error {
	status := team.StatusBlocked
	if sh.Reason == team.ShiftDone {
		status = team.StatusDone
	}
	message := team.ShiftEndedReport(sh.Why, sh.Round, sh.MaxRounds)
	name := s.agentName(thread.Agent)
	key := shiftEndPrefix + thread.Owner + ":" + strconv.Itoa(sh.Round) + ":" + sh.Reason
	s.workMu.Lock()
	updated, _, _, err := s.store.FileReport(ctx, key, thread.ID, team.ThreadState(status), message, nil,
		func(asks []int64) string { return team.ReportText(thread.ID, name, status, message, asks) })
	if err == nil {
		s.publishThread(updated)
	}
	s.workMu.Unlock()
	if errors.Is(err, store.ErrConflict) {
		return nil // the thread closed meanwhile
	} else if err != nil {
		return err
	}
	s.sendShiftMessages(ctx, thread.ID)
	return nil
}

// sendShiftMessages sends thread's queued messages now; what cannot go
// now goes at a later delivery pass.
func (s *Server) sendShiftMessages(ctx context.Context, thread int64) {
	if done, err := s.sendReports(ctx, thread); err != nil || !done {
		if err != nil {
			s.log.Warn("send a shift's message", "thread", thread, "err", err)
		}
		s.deliverLater()
	}
}

// ownerWroteToShift pauses the running shift of the thread whose
// specialist's session the owner wrote to (ADR-0024): pocketd's own
// messages never come this way.
func (s *Server) ownerWroteToShift(ctx context.Context, session int64) {
	thread, err := s.store.ThreadOfSession(ctx, session)
	if err != nil || thread.Session != session {
		return
	}
	s.workMu.Lock()
	_, err = s.store.PauseShift(ctx, thread.ID, team.ShiftWhy(team.ShiftPerson, "", 0, 0))
	if err == nil {
		s.publishThread(thread)
	}
	s.workMu.Unlock()
	if err != nil && !errors.Is(err, store.ErrNotFound) && !errors.Is(err, store.ErrConflict) {
		s.log.Warn("pause a shift", "thread", thread.ID, "err", err)
	}
}

// stopShift stops a thread's shift for the owner (ADR-0024): its round's
// run is aborted, and the supervisor hears once that the owner stopped
// it. A shift that ended answers as it is.
func (s *Server) stopShift(w http.ResponseWriter, r *http.Request) {
	s.ownerShift(w, r, "stopShift", func(ctx context.Context, thread store.ThreadRow, sh store.ShiftRow) error {
		why := team.ShiftWhy(team.ShiftStopped, "", sh.Round, sh.MaxRounds)
		s.workMu.Lock()
		ended, err := s.store.EndShift(ctx, thread.ID, team.ShiftStopped, why, team.ShiftRunning, team.ShiftSummary, team.ShiftPaused)
		if err == nil {
			s.publishThread(thread)
		}
		s.workMu.Unlock()
		if errors.Is(err, store.ErrConflict) {
			return nil
		} else if err != nil {
			return err
		}
		for _, session := range []int64{thread.Session, thread.WorkerSession} {
			if session != 0 {
				if busy, err := s.engine.Busy(ctx, session); err == nil && busy {
					if err := s.engine.Abort(ctx, session); err != nil {
						s.log.Warn("stop a shift's run", "thread", thread.ID, "session", session, "err", err)
					}
				}
			}
		}
		return s.fileShiftReport(ctx, thread, ended)
	})
}

// resumeShift goes on with a thread's shift for the owner (ADR-0024): a
// paused one, or one that ended for a reason other than done or the
// thread's close, with rounds and time left, at its next round.
func (s *Server) resumeShift(w http.ResponseWriter, r *http.Request) {
	s.ownerShift(w, r, "resumeShift", func(ctx context.Context, thread store.ThreadRow, sh store.ShiftRow) error {
		ok := func(row store.ShiftRow) bool {
			resumable := row.State == team.ShiftPaused ||
				row.State == team.ShiftEnded && row.Reason != team.ShiftDone && row.Reason != team.ShiftClosed
			_, limited := s.shiftLimit(row)
			return resumable && !limited
		}
		s.workMu.Lock()
		_, err := s.store.ResumeShift(ctx, thread.ID, ok, func(row store.ShiftRow) store.ReportRow {
			return store.ReportRow{Key: shiftRoundKey(thread, row.Round), ThreadID: thread.ID, SessionID: thread.Session,
				Text: team.ShiftRoundText(thread.ID, row.Round, row.MaxRounds, row.Deadline, row.Stop, nil)}
		})
		if err == nil {
			s.publishThread(thread)
		}
		s.workMu.Unlock()
		if errors.Is(err, store.ErrConflict) {
			return errShiftNotResumable
		} else if err != nil {
			return err
		}
		s.sendShiftMessages(ctx, thread.ID)
		return nil
	})
}

var errShiftNotResumable = errors.New("the shift cannot go on: it is running, ended done or with its thread, or has no rounds or time left")

// ownerShift runs an owner's change of a thread's shift once per request
// id, and answers with the thread as it is then.
func (s *Server) ownerShift(w http.ResponseWriter, r *http.Request, kind string, change func(ctx context.Context, thread store.ThreadRow, sh store.ShiftRow) error) {
	id, ok := pathIDOf(w, r, "thread")
	if !ok {
		return
	}
	var request proto.StopShift
	if !decode(w, r, &request) || !validRequestID(w, request.RequestID) {
		return
	}
	ctx := context.WithoutCancel(r.Context())
	key := requestKey(r, request.RequestID)
	defer s.keys.lock(key)()
	kind += ":" + strconv.FormatInt(id, 10)
	_, existed, err := s.store.LookupRequest(ctx, key, kind)
	if errors.Is(err, store.ErrRequestReused) {
		reused(w)
		return
	} else if err != nil {
		s.internal(w, "change a shift", err)
		return
	}
	thread, err := s.store.Thread(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, proto.ErrorCodeNotFound, fmt.Sprintf("no thread %d", id))
		return
	} else if err != nil {
		s.internal(w, "change a shift", err)
		return
	}
	sh, err := s.store.Shift(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, proto.ErrorCodeNotFound, fmt.Sprintf("thread T%d runs no shift", id))
		return
	} else if err != nil {
		s.internal(w, "change a shift", err)
		return
	}
	if !existed {
		if err := change(ctx, thread, sh); errors.Is(err, errShiftNotResumable) {
			writeError(w, http.StatusConflict, proto.ErrorCodeConflict, err.Error())
			return
		} else if err != nil {
			s.internal(w, "change a shift", err)
			return
		}
		if err := s.store.RecordRequest(ctx, key, kind, "done"); err != nil {
			s.internal(w, "change a shift", err)
			return
		}
	}
	thread, err = s.store.Thread(ctx, id)
	if err != nil {
		s.internal(w, "change a shift", err)
		return
	}
	writeJSON(w, http.StatusOK, s.wireThreadOf(ctx, thread))
}
