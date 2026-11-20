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
// specialist, which goes only while it is the one its shift's round under
// way is owed (shiftMessageCurrent).
func isShiftMessage(key string) bool {
	return strings.HasPrefix(key, shiftRoundPrefix) || strings.HasPrefix(key, shiftSummaryPrefix)
}

// shiftMessageCurrent says whether the shift message key is the one that
// begins sh's round under way: a message of a round that ended, or of a
// shift that paused or ended, goes to no one (Codex review of #36).
func shiftMessageCurrent(thread store.ThreadRow, sh store.ShiftRow, key string) bool {
	switch sh.State {
	case team.ShiftRunning:
		return key == shiftRoundKey(thread, sh.Round)
	case team.ShiftSummary:
		return key == shiftSummaryKey(thread, sh.Round)
	}
	return false
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
// specialist or of its worker that ended, after a round's message or a
// shift's brief reached the specialist, after a pull request round, and
// at the service's start. One pass runs at a time; one asked for while it
// runs runs once after it.
func (s *Server) shiftSoon() {
	s.sessionMu.Lock()
	defer s.sessionMu.Unlock()
	if s.stopped {
		return
	}
	if s.shifting {
		s.shiftAgain = true
		return
	}
	s.shifting = true
	s.passes.Add(1)
	go func() {
		defer s.passes.Done()
		for {
			s.shiftPass(s.passCtx)
			s.sessionMu.Lock()
			again := s.shiftAgain && !s.stopped
			s.shiftAgain, s.shifting = false, again
			s.sessionMu.Unlock()
			if !again {
				return
			}
		}
	}()
}

// shiftLater runs a shift pass after retryDelay: a stop whose abort is
// still owed is tried again until its runs are over.
func (s *Server) shiftLater() {
	s.sessionMu.Lock()
	defer s.sessionMu.Unlock()
	if s.stopped {
		return
	}
	s.retry(s.shiftSoon)
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
// round. A round whose answer waits on CI is left for a later pass. A
// stopped shift whose abort is owed has its runs aborted until they are
// over (settleStop).
func (s *Server) driveShift(ctx context.Context, sh store.ShiftRow) error {
	if sh.State == team.ShiftEnded {
		return s.settleStop(ctx, sh.ThreadID)
	}
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
		// in the transaction that files it (store.FileReportWithPulls).
		return s.endShift(ctx, thread, sh.Reason, sh.Why, team.ShiftSummary)
	}
	if strings.TrimSpace(answer) == "" && failure != "" {
		return s.endShift(ctx, thread, team.ShiftFailed, team.ShiftWhy(team.ShiftFailed, failure, sh.Round, sh.MaxRounds), team.ShiftRunning)
	}
	verdict, reason := team.ParseShiftStatus(answer)
	// The CI failure streak counts the rounds in a row whose done saw the
	// same checks fail: any other round ends it (Codex review of #36).
	failures, fingerprint := 0, ""
	var failing []string
	switch verdict {
	case team.VerdictBlocked:
		return s.summarizeShift(ctx, thread, sh, verdict, reason, 0, "", team.ShiftBlocked, reason)
	case team.VerdictDone:
		if !sh.CI {
			return s.summarizeShift(ctx, thread, sh, verdict, reason, 0, "", team.ShiftDone, "")
		}
		state, names, err := s.threadCI(ctx, thread)
		if err != nil {
			return err
		}
		switch state {
		case github.CIGreen:
			return s.summarizeShift(ctx, thread, sh, verdict, reason, 0, "", team.ShiftDone, "")
		case github.CIPending:
			// The next pass, after a round of the pull requests, reads CI
			// again, until the deadline: a check that never ends holds no
			// shift past it (Codex review of #36).
			if s.pastDeadline(sh) {
				return s.summarizeShift(ctx, thread, sh, verdict, reason, sh.Failures, sh.Fingerprint, team.ShiftDeadline, "CI had not finished")
			}
			return nil
		}
		failing, fingerprint = names, team.ShiftFingerprint(names)
		failures = 1
		if fingerprint == sh.Fingerprint {
			failures = sh.Failures + 1
		}
		if failures >= team.ShiftFailures {
			return s.summarizeShift(ctx, thread, sh, verdict, reason, failures, fingerprint, team.ShiftFailedCheck, strings.Join(names, ", "))
		}
	}
	if stop, ok := s.shiftLimit(sh); ok {
		return s.summarizeShift(ctx, thread, sh, verdict, reason, failures, fingerprint, stop, "")
	}
	return s.nextShiftRound(ctx, thread, sh, verdict, reason, failures, fingerprint, failing)
}

// shiftLimit says whether sh reached its round limit or its deadline,
// checked before a round starts, so a round that runs finishes.
func (s *Server) shiftLimit(sh store.ShiftRow) (string, bool) {
	switch {
	case sh.Round >= sh.MaxRounds:
		return team.ShiftMaxRounds, true
	case s.pastDeadline(sh):
		return team.ShiftDeadline, true
	}
	return "", false
}

func (s *Server) pastDeadline(sh store.ShiftRow) bool {
	return !sh.Deadline.IsZero() && !s.now().Before(sh.Deadline)
}

// roundAnswer reads the round under way of a live shift: whether it ended,
// its answer, the model's last text after the message that began it, and
// the failure of the run that ended it. A round ends when its message is
// in the specialist's transcript, the specialist and its worker are idle,
// and nothing is owed to either: a round that waits on its worker, on a
// reply of its worker's not kept yet, on an answer, or on a message pocketd
// still sends, is not over. Only the entries after the round's message are
// read, and none while the specialist is at work (Codex review of #36).
func (s *Server) roundAnswer(ctx context.Context, thread store.ThreadRow, sh store.ShiftRow) (answer string, ended bool, failure string, err error) {
	if busy, err := s.engine.Busy(ctx, thread.Session); err != nil || busy {
		return "", false, "", err
	}
	anchor, ok, err := s.roundAnchor(ctx, thread, sh)
	if err != nil || !ok {
		return "", false, "", err
	}
	if thread.WorkerSession != 0 {
		if busy, err := s.engine.Busy(ctx, thread.WorkerSession); err != nil || busy {
			return "", false, "", err
		}
		// A reply of a worker's run that ended is kept now, if its run's
		// end was not followed yet: the specialist is owed it.
		if err := s.queueWorkerReplies(ctx, thread, thread.WorkerSession); err != nil {
			return "", false, "", err
		}
	}
	unsent, err := s.store.UnsentReportsOf(ctx, thread.ID)
	if err != nil {
		return "", false, "", err
	}
	for _, r := range unsent {
		if r.SessionID == thread.Session || (thread.WorkerSession != 0 && r.SessionID == thread.WorkerSession) {
			// What is owed goes now, or at a later delivery pass.
			s.sendShiftMessages(ctx, thread.ID)
			return "", false, "", nil
		}
	}
	if owed, err := s.store.AnswerOwed(ctx, thread.Session); err != nil || owed {
		return "", false, "", err
	}
	entries, live, err := s.engine.TranscriptAfter(ctx, thread.Session, anchor)
	if err != nil || live.Busy {
		return "", false, "", err
	}
	for i := len(entries) - 1; i >= 0; i-- {
		if e := entries[i]; e.Kind == proto.EntryAssistant && strings.TrimSpace(e.Text) != "" {
			return e.Text, true, live.Failure, nil
		}
	}
	return "", true, live.Failure, nil
}

// roundAnchor is the transcript entry of the message that began sh's
// round under way: the brief for round 1, else the round's or the
// summary's message. It is found by the message's request key in the
// durable store, each attempt's from the latest, whatever pocketd noted of
// its submission, which a stop may have lost (Codex review of #36). ok is
// false while it is not in the transcript.
func (s *Server) roundAnchor(ctx context.Context, thread store.ThreadRow, sh store.ShiftRow) (int64, bool, error) {
	key, attempt := briefKey(thread), thread.BriefAttempt
	if message := shiftMessageKey(thread, sh); message != "" {
		row, err := s.store.Report(ctx, message)
		if errors.Is(err, store.ErrNotFound) {
			return 0, false, nil
		} else if err != nil {
			return 0, false, err
		}
		key, attempt = row.Key+":report", row.Attempt
	}
	for a := attempt; a >= 0; a-- {
		if entry, ok, err := s.engine.PlacedEntry(ctx, thread.Session, attemptKey(key, a)); err != nil || ok {
			return entry, ok, err
		}
	}
	return 0, false, nil
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

// threadCI is CI on the pull requests thread links that are not closed,
// each read on GitHub first, so its head is the one pushed last (Codex
// review of #36): green only with at least one and all green, failing
// with the checks that fail (each named with its pull request), else
// pending. One that cannot be read now leaves the decision to a later
// pass.
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
		read, err := s.refreshPull(ctx, p)
		if err != nil {
			return "", nil, fmt.Errorf("read %s#%d: %w", p.Repository, p.Number, err)
		}
		if read.State == store.PullClosed {
			continue
		}
		any = true
		checks, names, err := github.CIChecks(ctx, s.engine.GH, read.Repository, read.HeadSHA)
		if err != nil {
			return "", nil, err
		}
		ref := read.Repository + "#" + strconv.FormatInt(read.Number, 10)
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
		s.publishThreadNow(ctx, thread.ID)
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
// for its report in a summary round; the CI failure streak is kept as it
// stands then.
func (s *Server) summarizeShift(ctx context.Context, thread store.ThreadRow, sh store.ShiftRow, verdict, reason string, failures int, fingerprint, stop, detail string) error {
	why := team.ShiftWhy(stop, detail, sh.Round, sh.MaxRounds)
	message := store.ReportRow{Key: shiftSummaryKey(thread, sh.Round), ThreadID: thread.ID, SessionID: thread.Session,
		Text: team.ShiftSummaryText(thread.ID, why, sh.Round, sh.MaxRounds, stop)}
	s.workMu.Lock()
	_, err := s.store.SummarizeShift(ctx, thread.ID, sh.Round, verdict, reason, failures, fingerprint, stop, why, message)
	if err == nil {
		s.publishThreadNow(ctx, thread.ID)
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

// endShift ends a shift whose specialist did not report, for reason and
// why, from a state of from, and files pocketd's report in the same
// transaction (store.EndShift): the supervisor hears once why it ended.
func (s *Server) endShift(ctx context.Context, thread store.ThreadRow, reason, why string, from ...string) error {
	s.workMu.Lock()
	_, updated, _, _, err := s.store.EndShift(ctx, thread.ID, reason, why, false, nil, s.shiftReport(thread), from...)
	if err == nil {
		s.publishThread(updated)
	}
	s.workMu.Unlock()
	if errors.Is(err, store.ErrConflict) {
		return nil // it moved meanwhile, or its thread closed
	} else if err != nil {
		return err
	}
	s.sendShiftMessages(ctx, thread.ID)
	return nil
}

// shiftReport is pocketd's report of thread's shift as it ended, keyed by
// the round and the reason it ended at.
func (s *Server) shiftReport(thread store.ThreadRow) func(store.ShiftRow) store.ShiftReport {
	name := s.agentName(thread.Agent)
	return func(sh store.ShiftRow) store.ShiftReport {
		status := team.ShiftReportStatus(sh.Reason)
		message := team.ShiftEndedReport(sh.Why, sh.Round, sh.MaxRounds)
		return store.ShiftReport{Key: shiftEndPrefix + thread.Owner + ":" + strconv.Itoa(sh.Round) + ":" + sh.Reason, State: team.ThreadState(status), Message: message,
			Text: func(asks []int64) string { return team.ReportText(thread.ID, name, status, message, asks) }}
	}
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

// publishThreadNow publishes thread id as the store has it now, with its
// shift: called under workMu right after a change, so its event carries
// the change, never a thread read before it (Codex review of #36).
func (s *Server) publishThreadNow(ctx context.Context, id int64) {
	row, err := s.store.Thread(ctx, id)
	if err != nil {
		s.log.Warn("read a thread to publish", "thread", id, "err", err)
		return
	}
	s.publishThread(row)
}

// recordSend records the owner's message to session, which took effect,
// and with its first record pauses the running shift of the thread whose
// specialist's session it is (store.RecordSend): a retry of the message
// pauses nothing again (ADR-0024; Codex review of #36). The pause makes
// the round's message obsolete, so it commits under outboxMu, which no
// placement holds across (sendReport); its event goes with it under
// workMu. stopMu is held by the caller from before the message went.
func (s *Server) recordSend(ctx context.Context, session int64, key, kind, result string) error {
	thread, err := s.store.ThreadOfSession(ctx, session)
	if errors.Is(err, store.ErrNotFound) || err == nil && thread.Session != session {
		return s.store.RecordRequest(ctx, key, kind, result)
	} else if err != nil {
		return err
	}
	s.outboxMu.Lock()
	defer s.outboxMu.Unlock()
	s.workMu.Lock()
	defer s.workMu.Unlock()
	paused, err := s.store.RecordSend(ctx, key, kind, result, thread.ID, team.ShiftWhy(team.ShiftPerson, "", 0, 0))
	if err == nil && paused {
		s.publishThreadNow(ctx, thread.ID)
	}
	return err
}

// settleStop aborts the runs of a stopped shift's specialist and worker
// while the stop owes it, until both are idle, and then notes it settled
// (ADR-0024; Codex review of #36). What it owes is read again under
// stopMu, which the owner's message and a resume hold from before their
// run begins until they clear what the stop owes: no abort reaches their
// runs. One run still at work is tried again later.
func (s *Server) settleStop(ctx context.Context, id int64) error {
	s.stopMu.Lock()
	defer s.stopMu.Unlock()
	sh, err := s.store.Shift(ctx, id)
	if err != nil || !sh.StopDue {
		return err
	}
	thread, err := s.store.Thread(ctx, id)
	if err != nil {
		return err
	}
	idle := true
	for _, session := range []int64{thread.Session, thread.WorkerSession} {
		if session == 0 {
			continue
		}
		busy, err := s.engine.Busy(ctx, session)
		if err != nil {
			s.shiftLater()
			return err
		}
		if busy {
			idle = false
			if err := s.engine.Abort(ctx, session); err != nil {
				s.shiftLater()
				return fmt.Errorf("stop the run of session %d: %w", session, err)
			}
		}
	}
	if !idle {
		// The run settles after its abort: a pass after it notes the stop
		// settled.
		s.shiftLater()
		return nil
	}
	return s.store.StopSettled(ctx, id)
}

// stopShift stops a thread's shift for the owner (ADR-0024): pocketd's
// report goes to the supervisor in the transaction that ends it, and the
// runs of its specialist and its worker are aborted until they are over
// (settleStop). The round's message, if it has not reached the specialist,
// goes to no one: the stop commits under outboxMu. A shift that ended
// already answers as it is.
func (s *Server) stopShift(w http.ResponseWriter, r *http.Request) {
	s.ownerShift(w, r, "stopShift", func(ctx context.Context, thread store.ThreadRow, claim *store.ShiftClaim) error {
		s.outboxMu.Lock()
		s.workMu.Lock()
		_, updated, _, replayed, err := s.store.EndShift(ctx, thread.ID, team.ShiftStopped, team.ShiftWhy(team.ShiftStopped, "", 0, 0), true, claim,
			s.shiftReport(thread), team.ShiftRunning, team.ShiftSummary, team.ShiftPaused)
		if err == nil && !replayed {
			s.publishThread(updated)
		}
		s.workMu.Unlock()
		s.outboxMu.Unlock()
		if errors.Is(err, store.ErrConflict) {
			return nil
		} else if err != nil {
			return err
		}
		if err := s.settleStop(ctx, thread.ID); err != nil {
			s.log.Warn("stop a shift's runs", "thread", thread.ID, "err", err)
		}
		s.sendShiftMessages(ctx, thread.ID)
		return nil
	})
}

// resumeShift goes on with a thread's shift for the owner (ADR-0024): a
// paused one, or one that ended for a reason other than done or the
// thread's close, with rounds and time left, at its next round. What a
// stop still owed is dropped with it, under stopMu: the resumed run is no
// stop's.
func (s *Server) resumeShift(w http.ResponseWriter, r *http.Request) {
	s.ownerShift(w, r, "resumeShift", func(ctx context.Context, thread store.ThreadRow, claim *store.ShiftClaim) error {
		ok := func(row store.ShiftRow) bool {
			resumable := row.State == team.ShiftPaused ||
				row.State == team.ShiftEnded && row.Reason != team.ShiftDone && row.Reason != team.ShiftClosed
			_, limited := s.shiftLimit(row)
			return resumable && !limited
		}
		s.stopMu.Lock()
		s.workMu.Lock()
		_, replayed, err := s.store.ResumeShift(ctx, thread.ID, claim, ok, func(row store.ShiftRow) store.ReportRow {
			return store.ReportRow{Key: shiftRoundKey(thread, row.Round), ThreadID: thread.ID, SessionID: thread.Session,
				Text: team.ShiftRoundText(thread.ID, row.Round, row.MaxRounds, row.Deadline, row.Stop, nil)}
		})
		if err == nil && !replayed {
			s.publishThreadNow(ctx, thread.ID)
		}
		s.workMu.Unlock()
		s.stopMu.Unlock()
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

// ownerShift runs an owner's change of a thread's shift, which claims the
// request in the transaction that changes it (store.ShiftClaim): the same
// request again changes nothing (Codex review of #36). It answers with the
// thread as it is then. A thread whose specialist does not have its brief
// yet has no shift to change: its round 1 has not begun (Codex review of
// #36).
func (s *Server) ownerShift(w http.ResponseWriter, r *http.Request, kind string, change func(ctx context.Context, thread store.ThreadRow, claim *store.ShiftClaim) error) {
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
	if _, _, err := s.store.LookupRequest(ctx, key, kind); errors.Is(err, store.ErrRequestReused) {
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
	if _, err := s.store.Shift(ctx, id); errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, proto.ErrorCodeNotFound, fmt.Sprintf("thread T%d runs no shift", id))
		return
	} else if err != nil {
		s.internal(w, "change a shift", err)
		return
	}
	if thread.State == store.ThreadStarting || thread.Session == 0 || !thread.Briefed {
		writeError(w, http.StatusConflict, proto.ErrorCodeConflict, fmt.Sprintf("thread T%d's specialist does not have its brief yet: change its shift once it has", id))
		return
	}
	if err := change(ctx, thread, &store.ShiftClaim{Key: key, Kind: kind}); errors.Is(err, errShiftNotResumable) {
		writeError(w, http.StatusConflict, proto.ErrorCodeConflict, err.Error())
		return
	} else if errors.Is(err, store.ErrRequestReused) {
		reused(w)
		return
	} else if err != nil {
		s.internal(w, "change a shift", err)
		return
	}
	thread, err = s.store.Thread(ctx, id)
	if err != nil {
		s.internal(w, "change a shift", err)
		return
	}
	writeJSON(w, http.StatusOK, s.wireThreadOf(ctx, thread))
}
