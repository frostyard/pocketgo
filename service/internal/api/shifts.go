package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

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
	s.goShift(func() {
		for {
			// A pass counts for Stop once it runs, as a delivery pass does:
			// one that has not begun when the server stops runs none.
			s.sessionMu.Lock()
			if s.stopped {
				s.shifting = false
				s.sessionMu.Unlock()
				return
			}
			s.passes.Add(1)
			s.sessionMu.Unlock()
			s.shiftPass(s.passCtx)
			s.passes.Done()
			s.sessionMu.Lock()
			again := s.shiftAgain && !s.stopped
			s.shiftAgain, s.shifting = false, again
			s.sessionMu.Unlock()
			if !again {
				return
			}
		}
	})
}

// ciWait is when a round that waits on CI reads it again: no sooner,
// whatever passes run meanwhile. shiftMu guards Server.ciWaits.
type ciWait struct {
	round int
	at    time.Time
}

// shiftCIRecheck is how long a round that waits on CI waits for a pass of
// its own: GitHub's checks end with no event pocketd sees, and a service
// with no pull request rounds would never read them again (Codex review of
// #36).
const shiftCIRecheck = 2 * time.Minute

// wakeShift has a pass run for sh at at, or at its deadline if sooner,
// whatever else runs one: a round that waits on CI reads it again, and a
// shift whose brief or round message waits in a queue stops at its
// deadline (Codex review of #36). A thread keeps its soonest wake; one
// due later gives way to a sooner.
func (s *Server) wakeShift(sh store.ShiftRow, at time.Time) {
	if !sh.Deadline.IsZero() && sh.Deadline.Before(at) {
		at = sh.Deadline
	}
	s.sessionMu.Lock()
	defer s.sessionMu.Unlock()
	// A wake due no later is enough: when it comes, a round still waiting
	// sets the next (Codex review of #36).
	if due, ok := s.shiftWakes[sh.ThreadID]; s.stopped || ok && !due.After(at) {
		return
	}
	s.shiftWakes[sh.ThreadID] = at
	s.shiftTimer(max(at.Sub(s.now()), 0), func() {
		s.sessionMu.Lock()
		if due, ok := s.shiftWakes[sh.ThreadID]; ok && due.Equal(at) {
			delete(s.shiftWakes, sh.ThreadID)
		}
		s.sessionMu.Unlock()
		s.shiftSoon()
	})
}

// shiftLater runs a shift pass after retryDelay, one at a time: a stop
// whose abort is still owed is tried again until its runs are over, and a
// pass that could not read what it needed runs again (Codex review of
// #36).
func (s *Server) shiftLater() {
	s.sessionMu.Lock()
	defer s.sessionMu.Unlock()
	if s.stopped || s.shiftRetrying {
		return
	}
	s.shiftRetrying = true
	s.retry(func() {
		s.sessionMu.Lock()
		s.shiftRetrying = false
		s.sessionMu.Unlock()
		s.shiftSoon()
	})
}

// shiftPass drives each live shift one step, one pass at a time: what
// cannot be read now waits for the next pass.
func (s *Server) shiftPass(ctx context.Context) {
	s.shiftMu.Lock()
	defer s.shiftMu.Unlock()
	live, err := s.store.LiveShifts(ctx)
	if err != nil {
		s.log.Warn("read the live shifts", "err", err)
		s.shiftLater()
		return
	}
	// A CI wait belongs to a live round: one of a shift stopped, closed or
	// moved on goes (Codex review of #36).
	rounds := make(map[int64]int, len(live))
	for _, sh := range live {
		rounds[sh.ThreadID] = sh.Round
	}
	for id, w := range s.ciWaits {
		if round, ok := rounds[id]; !ok || round != w.round {
			delete(s.ciWaits, id)
		}
	}
	for _, sh := range live {
		if err := s.driveShift(ctx, sh); err != nil {
			s.log.Warn("drive a shift", "thread", sh.ThreadID, "err", err)
			s.shiftLater()
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
	if sh.State != team.ShiftRunning && sh.State != team.ShiftSummary {
		return nil
	}
	// A pass at the deadline, whatever else runs one: a brief or a round's
	// message queued behind a run is withdrawn then, before PiG can place
	// it after the deadline (Codex review of #36).
	if !sh.Deadline.IsZero() && s.now().Before(sh.Deadline) {
		s.wakeShift(sh, sh.Deadline)
	}
	thread, err := s.store.Thread(ctx, sh.ThreadID)
	if err != nil {
		return err
	}
	// A shift whose brief has not reached the specialist by the deadline
	// never begins: its brief, queued behind a run or not, is withdrawn,
	// and the thread closes (Codex review of #36).
	if !thread.Briefed && thread.Session != 0 && thread.State != store.ThreadClosed && s.pastDeadline(sh) {
		return s.expireUnbriefed(ctx, thread.ID)
	}
	if thread.State == store.ThreadClosed || thread.Session == 0 || !thread.Briefed {
		return nil
	}
	// A round decided before the deadline whose message never went holds
	// no work past it: the shift stops for its deadline instead (Codex
	// review of #36). One that went finishes.
	if sh.State == team.ShiftRunning && sh.Round > 1 && s.pastDeadline(sh) {
		if expired, err := s.expireRound(ctx, thread, sh); err != nil || expired {
			return err
		}
	}
	read, err := s.readRound(ctx, thread, sh)
	if err != nil {
		return err
	}
	if read.owed {
		// What is owed goes now, or at a later delivery pass, whatever
		// thread filed it; the round is not over until it went.
		s.sendShiftMessages(ctx, thread.ID)
		s.deliverLater()
		return nil
	}
	if !read.ended {
		return nil
	}
	thread = read.thread
	if sh.State == team.ShiftSummary {
		// The summary round ended with no report: a report ends the shift
		// in the transaction that files it (store.FileReportWithPulls).
		return s.endShift(ctx, thread, sh, read, sh.Reason, sh.Why, team.ShiftSummary)
	}
	if strings.TrimSpace(read.answer) == "" && read.failure != "" {
		// The failed round is recorded, with no verdict, and ends the CI
		// failure streak (Codex review of #36).
		return s.decideRound(ctx, thread, sh, read, func() error {
			_, _, _, _, err := s.store.EndShift(ctx, thread.ID, store.ShiftEnd{Reason: team.ShiftFailed,
				Why: team.ShiftWhy(team.ShiftFailed, read.failure, sh.Round, sh.MaxRounds), From: []string{team.ShiftRunning}, Round: sh.Round,
				RecordRound: true, Report: s.shiftReport(thread), Quiet: read.quiet()})
			return err
		})
	}
	verdict, reason := team.ParseShiftStatus(read.answer)
	next := shiftDecision{verdict: verdict, reason: reason}
	switch verdict {
	case team.VerdictBlocked:
		next.stop, next.detail = team.ShiftBlocked, reason
		return s.summarizeShift(ctx, thread, sh, read, next)
	case team.VerdictDone:
		if !sh.CI {
			next.stop = team.ShiftDone
			return s.summarizeShift(ctx, thread, sh, read, next)
		}
		// A round that waits on CI reads it at most once a recheck,
		// whatever passes other shifts run, until its deadline (Codex
		// review of #36).
		if w, ok := s.ciWaits[thread.ID]; ok && w.round == sh.Round && s.now().Before(w.at) && !s.pastDeadline(sh) {
			// Its wake: an earlier round's, gone now, may have taken its
			// place (Codex review of #36).
			s.wakeShift(sh, w.at)
			return nil
		}
		delete(s.ciWaits, thread.ID)
		state, names, heads, err := s.threadCI(ctx, thread)
		if err != nil {
			// A pull request or its CI GitHub does not answer for holds no
			// shift past its deadline (Codex review of #36).
			if s.pastDeadline(sh) {
				next.failures, next.fingerprint = sh.Failures, sh.Fingerprint
				next.stop, next.detail = team.ShiftDeadline, "GitHub could not be read for CI"
				return s.summarizeShift(ctx, thread, sh, read, next)
			}
			// The CI wake reads it again: no quicker retry hammers GitHub
			// (Codex review of #36).
			s.log.Warn("read a shift's CI", "thread", thread.ID, "err", err)
			s.ciWaits[thread.ID] = ciWait{round: sh.Round, at: s.now().Add(shiftCIRecheck)}
			s.wakeShift(sh, s.ciWaits[thread.ID].at)
			return nil
		}
		switch state {
		case github.CIGreen:
			// The pull requests checked green, at their heads, are the ones
			// the summary's report may name (Codex review of #36).
			next.stop, next.ciHeads = team.ShiftDone, heads
			return s.summarizeShift(ctx, thread, sh, read, next)
		case github.CIPending:
			// The next pass, after a round of the pull requests, reads CI
			// again, until the deadline, which wakes a pass of its own: a
			// check that never ends holds no shift past it (Codex review
			// of #36).
			if s.pastDeadline(sh) {
				next.failures, next.fingerprint = sh.Failures, sh.Fingerprint
				next.stop, next.detail = team.ShiftDeadline, "CI had not finished"
				return s.summarizeShift(ctx, thread, sh, read, next)
			}
			s.ciWaits[thread.ID] = ciWait{round: sh.Round, at: s.now().Add(shiftCIRecheck)}
			s.wakeShift(sh, s.ciWaits[thread.ID].at)
			return nil
		}
		// The CI failure streak counts the rounds in a row whose done saw
		// the same checks fail: any other round ends it (Codex review of
		// #36).
		next.failing, next.fingerprint = names, team.ShiftFingerprint(names)
		next.failures = 1
		if next.fingerprint == sh.Fingerprint {
			next.failures = sh.Failures + 1
		}
		if next.failures >= team.ShiftFailures {
			next.stop, next.detail = team.ShiftFailedCheck, strings.Join(names, ", ")
			return s.summarizeShift(ctx, thread, sh, read, next)
		}
	}
	if stop, ok := s.shiftLimit(sh); ok {
		next.stop = stop
		return s.summarizeShift(ctx, thread, sh, read, next)
	}
	return s.nextShiftRound(ctx, thread, sh, read, next)
}

// expireRound stops sh for its deadline when its round's message never
// went: no attempt of it is queued or placed in PiG, read under the
// shift's lock and outboxMu, so none goes meanwhile. expired says it
// stopped; the message, no longer the round's, goes to no one.
func (s *Server) expireRound(ctx context.Context, thread store.ThreadRow, sh store.ShiftRow) (expired bool, err error) {
	unlock := s.lockShift(thread.ID)
	s.outboxMu.Lock()
	row, err := s.store.Report(ctx, shiftRoundKey(thread, sh.Round))
	if err == nil && row.SentAt == nil {
		// One queued, on an idle session after a failed run or behind a
		// run, is withdrawn: only one in the transcript began its round
		// (Codex review of #36).
		var placed bool
		if placed, err = s.attemptPlaced(ctx, thread.Session, row); err == nil && !placed {
			if _, err = s.withdrawReports(ctx, []store.ReportRow{row}); err == nil {
				// A run may have placed it as it was withdrawn.
				placed, err = s.attemptPlaced(ctx, thread.Session, row)
				expired = err == nil && !placed
			}
		}
	} else if errors.Is(err, store.ErrNotFound) {
		err = nil
	}
	if err == nil && expired {
		why := team.ShiftWhy(team.ShiftDeadline, "", sh.Round, sh.MaxRounds)
		message := store.ReportRow{Key: shiftSummaryKey(thread, sh.Round), ThreadID: thread.ID, SessionID: thread.Session,
			Text: team.ShiftSummaryText(thread.ID, why, sh.Round, sh.MaxRounds, team.ShiftDeadline)}
		s.workMu.Lock()
		_, err = s.store.SummarizeShift(ctx, thread.ID, sh.Round, "", "", sh.Failures, sh.Fingerprint, team.ShiftDeadline, why, nil, message)
		if err == nil {
			s.publishThreadNow(ctx, thread.ID)
		}
		s.workMu.Unlock()
	}
	s.outboxMu.Unlock()
	unlock()
	if errors.Is(err, store.ErrConflict) {
		return true, nil // it moved meanwhile: the next pass reads it again
	} else if err != nil || !expired {
		return false, err
	}
	s.sendShiftMessages(ctx, thread.ID)
	return true, nil
}

// attemptPlaced says whether an attempt of the outbox message row is in
// session's transcript. outboxMu is held.
func (s *Server) attemptPlaced(ctx context.Context, session int64, row store.ReportRow) (bool, error) {
	if row.SentAt != nil {
		return true, nil
	}
	for attempt := 0; attempt <= row.Attempt; attempt++ {
		where, err := s.engine.Placed(ctx, session, attemptKey(row.Key+":report", attempt))
		if err != nil {
			return false, err
		}
		if where == PlacementPlaced {
			return true, nil
		}
	}
	return false, nil
}

// shiftDecision is what a round's end decides: its verdict, the CI failure
// streak, and the stop (none for the next round), with what the summary
// says and the pull requests a done's CI was checked green on.
type shiftDecision struct {
	verdict, reason string
	failures        int
	fingerprint     string
	failing         []string
	stop, detail    string
	ciHeads         []string
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

// roundRead is the round under way of a live shift as a pass read it.
type roundRead struct {
	// thread is the thread as read once its specialist was idle.
	thread store.ThreadRow
	// ended says the round ended; owed that something is owed to the
	// specialist or its worker, which holds it.
	ended, owed bool
	// answer is the last run's text after its own input, failure the
	// failure of the run that ended it.
	answer, failure string
	// last is the newest entry the round was read through: a decision
	// holds only while it is still the newest (commitRound).
	last int64
}

// quiet are the sessions a decision of the round needs owed nothing, in
// the transaction that commits it: an answer or a verdict recorded after
// the round was read again holds it (Codex review of #36).
func (r roundRead) quiet() []int64 { return []int64{r.thread.Session, r.thread.WorkerSession} }

// readRound reads the round under way of a live shift. A round ends when
// its message is in the specialist's transcript, the specialist and its
// worker are idle, and nothing is owed to either, whatever thread filed
// it: a round that waits on its worker, on a reply of its worker's not
// kept yet, on an answer, on a cross-review's verdict, or on a message
// pocketd still sends, is not over (Codex review of #36). Only the entries
// after the round's message are read, and none while the specialist is at
// work. It sends nothing.
func (s *Server) readRound(ctx context.Context, thread store.ThreadRow, sh store.ShiftRow) (roundRead, error) {
	if busy, err := s.engine.Busy(ctx, thread.Session); err != nil || busy {
		return roundRead{}, err
	}
	// The thread as it is once its specialist is idle: a worker it started
	// in the run that just ended is its worker (Codex review of #36).
	thread, err := s.store.Thread(ctx, thread.ID)
	if err != nil {
		return roundRead{}, err
	}
	read := roundRead{thread: thread}
	anchor, ok, err := s.roundAnchor(ctx, thread, sh)
	if err != nil || !ok {
		return read, err
	}
	if thread.WorkerSession != 0 {
		if busy, err := s.engine.Busy(ctx, thread.WorkerSession); err != nil || busy {
			return read, err
		}
		// A reply of a worker's run that ended is kept now, if its run's
		// end was not followed yet: the specialist is owed it.
		if err := s.queueWorkerReplies(ctx, thread, thread.WorkerSession); err != nil {
			return read, err
		}
	}
	reports, answers, err := s.store.Owed(ctx, thread.Session, thread.WorkerSession)
	if err != nil {
		return read, err
	}
	if len(reports) > 0 || len(answers) > 0 {
		read.owed = true
		return read, nil
	}
	entries, live, err := s.engine.TranscriptAfter(ctx, thread.Session, anchor)
	if err != nil || live.Busy {
		return read, err
	}
	read.ended, read.failure, read.last = true, live.Failure, anchor
	if len(entries) > 0 {
		read.last = entries[len(entries)-1].ID
	}
	// The answer is the last run's final response, after its own input (a
	// worker's reply, an answer, the owner's message): a run that failed
	// with none has none, whatever a run before it said, and a model's
	// text beside a tool call is no final verdict when a response came
	// after it (Codex review of #36).
	for i := len(entries) - 1; i >= 0; i-- {
		e := entries[i]
		if e.Kind == proto.EntryUser {
			break
		}
		if e.Kind == proto.EntryAssistant {
			// A tool call's entry is no final response: its run ended
			// before the model answered (Codex review of #36).
			if e.StopReason != stopToolUse {
				read.answer = e.Text
			}
			break
		}
	}
	return read, nil
}

// commitRound commits a decision of sh's round, as read, under the
// shift's lock and outboxMu: no message is placed meanwhile, the owner's
// included, so no run the decision did not see begins between the round's
// read again and the commit, however long the
// decision took, a read of CI included (Codex review of #36). A round that
// moved since (a run, something owed, a newer entry, another worker) is
// left for the next pass: done is false.
func (s *Server) commitRound(ctx context.Context, sh store.ShiftRow, read roundRead, commit func() error) (done bool, err error) {
	// The shift's lock first: the owner's message to the specialist,
	// which goes under it and not under outboxMu, goes before the read
	// again or after the commit (Codex review of #36).
	defer s.lockShift(read.thread.ID)()
	s.outboxMu.Lock()
	defer s.outboxMu.Unlock()
	again, err := s.readRound(ctx, read.thread, sh)
	if err != nil {
		return false, err
	}
	if !again.ended || again.owed || again.last != read.last || again.thread.WorkerSession != read.thread.WorkerSession {
		return false, nil
	}
	return true, commit()
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
func (s *Server) threadCI(ctx context.Context, thread store.ThreadRow) (state string, failing, heads []string, err error) {
	pulls, err := s.store.PullsOfThread(ctx, thread.ID)
	if err != nil {
		return "", nil, nil, err
	}
	state = github.CIGreen
	any := false
	var checked []store.PullRow
	for _, p := range pulls {
		// Each is read first, a closed one included: one reopened since
		// the last read counts (Codex review of #36). The read is GitHub's
		// answer alone, bounded: its label is the pull request rounds' to
		// bring in step, and a label that cannot be written holds no shift.
		read, err := s.readPull(ctx, p.Repository, p.Number)
		if err != nil {
			return "", nil, nil, fmt.Errorf("read %s#%d: %w", p.Repository, p.Number, err)
		}
		// Another pull request at its name, as a repository named again
		// gives, is not the one linked: its CI counts for nothing, and
		// fails (Codex review of #36).
		if read.GitHubID != p.GitHubID {
			any = true
			failing = append(failing, p.Repository+"#"+strconv.FormatInt(p.Number, 10)+" is another pull request on GitHub now")
			continue
		}
		if read.State == store.PullClosed {
			continue
		}
		any = true
		checked = append(checked, read)
		heads = append(heads, store.ShiftHead(read))
		readCtx, cancel := s.withTimeout(ctx, pullTimeout)
		checks, names, err := github.CIChecks(readCtx, s.engine.GH, read.Repository, read.HeadSHA)
		cancel()
		if err != nil {
			return "", nil, nil, err
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
		return github.CIFailing, []string{"no pull request is reported for the thread"}, nil, nil
	case len(failing) > 0:
		return github.CIFailing, failing, nil, nil
	case state != github.CIGreen:
		return state, nil, nil, nil
	}
	// Green counts only at the heads it was read at: a push meanwhile
	// leaves CI to read again (Codex review of #36).
	for _, p := range checked {
		again, err := s.readPull(ctx, p.Repository, p.Number)
		if err != nil {
			return "", nil, nil, fmt.Errorf("read %s#%d again: %w", p.Repository, p.Number, err)
		}
		if again.GitHubID != p.GitHubID || again.HeadSHA != p.HeadSHA || again.State != p.State {
			return github.CIPending, nil, nil, nil
		}
	}
	return state, nil, heads, nil
}

// nextShiftRound ends sh's round with its verdict and starts the next: its
// message kept with the round's change, then sent.
func (s *Server) nextShiftRound(ctx context.Context, thread store.ThreadRow, sh store.ShiftRow, read roundRead, d shiftDecision) error {
	n := sh.Round + 1
	message := store.ReportRow{Key: shiftRoundKey(thread, n), ThreadID: thread.ID, SessionID: thread.Session,
		Text: team.ShiftRoundText(thread.ID, n, sh.MaxRounds, sh.Deadline, sh.Stop, d.failing)}
	return s.decideRound(ctx, thread, sh, read, func() error {
		_, err := s.store.NextShiftRound(ctx, thread.ID, sh.Round, d.verdict, d.reason, d.failures, d.fingerprint, message, read.quiet()...)
		return err
	})
}

// summarizeShift stops sh for d's stop, with its detail, and asks the
// specialist for its report in a summary round; the CI failure streak is
// kept as it stands then.
func (s *Server) summarizeShift(ctx context.Context, thread store.ThreadRow, sh store.ShiftRow, read roundRead, d shiftDecision) error {
	why := team.ShiftWhy(d.stop, d.detail, sh.Round, sh.MaxRounds)
	message := store.ReportRow{Key: shiftSummaryKey(thread, sh.Round), ThreadID: thread.ID, SessionID: thread.Session,
		Text: team.ShiftSummaryText(thread.ID, why, sh.Round, sh.MaxRounds, d.stop)}
	return s.decideRound(ctx, thread, sh, read, func() error {
		_, err := s.store.SummarizeShift(ctx, thread.ID, sh.Round, d.verdict, d.reason, d.failures, d.fingerprint, d.stop, why, d.ciHeads, message, read.quiet()...)
		return err
	})
}

// endShift ends a shift whose specialist did not report, decided at sh's
// round, for reason and why, from a state of from, and files pocketd's
// report in the same transaction (store.EndShift): the supervisor hears
// once why it ended.
func (s *Server) endShift(ctx context.Context, thread store.ThreadRow, sh store.ShiftRow, read roundRead, reason, why string, from ...string) error {
	return s.decideRound(ctx, thread, sh, read, func() error {
		_, _, _, _, err := s.store.EndShift(ctx, thread.ID, store.ShiftEnd{Reason: reason, Why: why, From: from, Round: sh.Round, Report: s.shiftReport(thread), Quiet: read.quiet()})
		return err
	})
}

// decideRound commits write, a decision of sh's round as read, with the
// thread's event, under workMu (commitRound), then sends what it queued.
// A round or shift that moved meanwhile is left for the next pass.
func (s *Server) decideRound(ctx context.Context, thread store.ThreadRow, sh store.ShiftRow, read roundRead, write func() error) error {
	done, err := s.commitRound(ctx, sh, read, func() error {
		s.workMu.Lock()
		defer s.workMu.Unlock()
		err := write()
		if err == nil {
			s.publishThreadNow(ctx, thread.ID)
		}
		return err
	})
	switch {
	case errors.Is(err, store.ErrConflict):
		return nil // the shift moved meanwhile, or its thread closed
	case err != nil || !done:
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
		// Its event is held back, and a delivery pass publishes it as it
		// is then, as one whose shift cannot be read (Codex review of #36).
		s.log.Warn("read a thread to publish", "thread", id, "err", err)
		s.threadRetryMu.Lock()
		s.threadRetry[id] = true
		s.threadRetryMu.Unlock()
		s.deliverLater()
		return
	}
	s.publishThread(row)
}

// specialistShift is the thread whose shift's specialist runs in session,
// 0 for a session that is none's.
func (s *Server) specialistShift(ctx context.Context, session int64) (int64, error) {
	thread, err := s.store.ThreadOfSession(ctx, session)
	if errors.Is(err, store.ErrNotFound) || err == nil && thread.Session != session {
		return 0, nil
	} else if err != nil {
		return 0, err
	}
	if _, err := s.store.Shift(ctx, thread.ID); errors.Is(err, store.ErrNotFound) {
		return 0, nil
	} else if err != nil {
		return 0, err
	}
	return thread.ID, nil
}

// lockShift holds thread's shift lock (shiftLocks) until the returned
// function runs.
func (s *Server) lockShift(thread int64) (unlock func()) {
	return s.shiftLocks.lock(strconv.FormatInt(thread, 10))
}

// claimSend claims the owner's message to the specialist of thread's
// shift (0 for a session that is no shift's specialist's) before it goes,
// and with a new claim pauses the running shift (store.ClaimSend): a retry
// of the message pauses nothing again, and a message PiG has never leaves
// its shift running (ADR-0024; Codex review of #36). The pause makes the
// round's message obsolete: one still queued is withdrawn before the
// pause commits, under outboxMu, since PiG places a queued message
// whatever pocketd's locks say. The pause's event goes with it under
// workMu. The shift's lock is held by the caller from before this until
// the message went.
func (s *Server) claimSend(ctx context.Context, thread int64, key, kind string) (existed, paused bool, err error) {
	if thread == 0 {
		_, existed, _, err := s.store.ClaimSend(ctx, key, kind, 0, "")
		return existed, false, err
	}
	s.outboxMu.Lock()
	defer s.outboxMu.Unlock()
	if _, existed, err := s.store.LookupRequest(ctx, key, kind); err != nil {
		return false, false, err
	} else if !existed {
		if sh, err := s.store.Shift(ctx, thread); err == nil && sh.State == team.ShiftRunning {
			if _, err := s.withdrawQueued(ctx, thread, func(row store.ReportRow) bool { return isShiftMessage(row.Key) }); err != nil {
				return false, false, err
			}
		} else if err != nil {
			return false, false, err
		}
	}
	s.workMu.Lock()
	defer s.workMu.Unlock()
	_, existed, paused, err = s.store.ClaimSend(ctx, key, kind, thread, team.ShiftWhy(team.ShiftPerson, "", 0, 0))
	if err == nil && paused {
		s.publishThreadNow(ctx, thread)
	}
	return existed, paused, err
}

// withdrawQueued withdraws every attempt still queued of thread's unsent
// messages that want takes, and says whether one withdrawal took effect.
// outboxMu is held.
func (s *Server) withdrawQueued(ctx context.Context, thread int64, want func(store.ReportRow) bool) (withdrew bool, err error) {
	rows, err := s.store.UnsentReportsOf(ctx, thread)
	if err != nil {
		return false, err
	}
	var wanted []store.ReportRow
	for _, row := range rows {
		if want(row) {
			wanted = append(wanted, row)
		}
	}
	return s.withdrawReports(ctx, wanted)
}

// withdrawReports withdraws every attempt still queued of rows, and says
// whether one withdrawal took effect. outboxMu is held.
func (s *Server) withdrawReports(ctx context.Context, rows []store.ReportRow) (withdrew bool, err error) {
	for _, row := range rows {
		for attempt := 0; attempt <= row.Attempt; attempt++ {
			done, err := s.engine.Withdraw(ctx, row.SessionID, attemptKey(row.Key+":report", attempt))
			if err != nil {
				return withdrew, fmt.Errorf("withdraw a queued message: %w", err)
			}
			withdrew = withdrew || done
		}
	}
	return withdrew, nil
}

// stopShift stops a thread's shift for the owner (ADR-0024): pocketd
// sends no more rounds, and files its report to the supervisor in the
// transaction that ends the shift. The shift's own message still queued,
// a round's or the summary's, is withdrawn before the stop commits, under
// the shift's lock and outboxMu, and goes to no one. The stop ends the
// shift only (the owner's choice, 2026-11-20): the specialist's and its
// worker's runs go on to their ends, and what pocketd owes them goes; the
// owner stops a session with its own abort. A shift that ended already
// answers as it is, and the stop's request is claimed all the same, so
// its retry never stops a later round.
func (s *Server) stopShift(w http.ResponseWriter, r *http.Request) {
	s.ownerShift(w, r, "stopShift", func(ctx context.Context, thread store.ThreadRow, claim *store.ShiftClaim) error {
		claim.KeepOnConflict = true
		unlock := s.lockShift(thread.ID)
		s.outboxMu.Lock()
		if err := s.beforeStop(ctx, thread.ID, claim); err != nil {
			s.outboxMu.Unlock()
			unlock()
			return err
		}
		s.workMu.Lock()
		_, updated, _, replayed, err := s.store.EndShift(ctx, thread.ID, store.ShiftEnd{Reason: team.ShiftStopped, Why: team.ShiftWhy(team.ShiftStopped, "", 0, 0),
			From: []string{team.ShiftRunning, team.ShiftSummary, team.ShiftPaused}, Claim: claim, Report: s.shiftReport(thread)})
		if err == nil && !replayed {
			s.publishThread(updated)
		}
		s.workMu.Unlock()
		s.outboxMu.Unlock()
		unlock()
		if errors.Is(err, store.ErrConflict) {
			return nil
		} else if err != nil {
			return err
		}
		s.sendShiftMessages(ctx, thread.ID)
		return nil
	})
}

// beforeStop withdraws the shift's own message still queued, of a live
// shift whose stop request is new. outboxMu is held.
func (s *Server) beforeStop(ctx context.Context, id int64, claim *store.ShiftClaim) error {
	if _, existed, err := s.store.LookupRequest(ctx, claim.Key, claim.Kind); err != nil || existed {
		return err
	}
	sh, err := s.store.Shift(ctx, id)
	if err != nil || sh.State == team.ShiftEnded {
		return err
	}
	_, err = s.withdrawQueued(ctx, id, func(row store.ReportRow) bool { return isShiftMessage(row.Key) })
	return err
}

// resumeShift goes on with a thread's shift for the owner (ADR-0024): a
// paused one, or one that ended for a reason other than done or the
// thread's close, with rounds and time left, at its next round, under the
// shift's lock.
func (s *Server) resumeShift(w http.ResponseWriter, r *http.Request) {
	s.ownerShift(w, r, "resumeShift", func(ctx context.Context, thread store.ThreadRow, claim *store.ShiftClaim) error {
		ok := func(row store.ShiftRow) bool {
			resumable := row.State == team.ShiftPaused ||
				row.State == team.ShiftEnded && row.Reason != team.ShiftDone && row.Reason != team.ShiftClosed
			_, limited := s.shiftLimit(row)
			return resumable && !limited
		}
		unlock := s.lockShift(thread.ID)
		s.workMu.Lock()
		_, replayed, err := s.store.ResumeShift(ctx, thread.ID, claim, ok, func(row store.ShiftRow) store.ReportRow {
			return store.ReportRow{Key: shiftRoundKey(thread, row.Round), ThreadID: thread.ID, SessionID: thread.Session,
				Text: team.ShiftRoundText(thread.ID, row.Round, row.MaxRounds, row.Deadline, row.Stop, nil)}
		})
		if err == nil && !replayed {
			s.publishThreadNow(ctx, thread.ID)
		}
		s.workMu.Unlock()
		unlock()
		if errors.Is(err, store.ErrConflict) {
			return errShiftNotResumable
		} else if err != nil {
			return err
		}
		s.sendShiftMessages(ctx, thread.ID)
		// A pass sets its deadline wake, whatever its round's message does
		// (Codex review of #36).
		s.shiftSoon()
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
	// The thread and its shift as one transaction has them (Codex review
	// of #36).
	thread, sh, err := s.store.ThreadWithShift(ctx, id)
	if err != nil {
		s.internal(w, "change a shift", err)
		return
	}
	out := wireThread(thread)
	if sh != nil {
		out.Shift = wireShift(*sh)
	}
	writeJSON(w, http.StatusOK, out)
}

// shiftExpired marks a shift's brief its deadline overtook before it
// reached the specialist: no work begins (Codex review of #36).
type shiftExpired struct{ error }

func (e shiftExpired) Unwrap() error { return e.error }

// briefInTime refuses to submit thread row's brief, again or for the first
// time, once its shift's deadline passed (shiftExpired): a brief already in
// the transcript began round 1, which finishes, and place never checks it.
func (s *Server) briefInTime(ctx context.Context, row store.ThreadRow) error {
	sh, err := s.store.Shift(ctx, row.ID)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	} else if err != nil {
		return err
	}
	if s.pastDeadline(sh) {
		return shiftExpired{errors.New(team.ShiftWhy(team.ShiftDeadline, "its brief had not reached the specialist", 0, 0))}
	}
	return nil
}

// expireBrief closes the thread whose shift's deadline overtook its brief
// (store.ExpireUnbriefed), and publishes it.
func (s *Server) expireBrief(ctx context.Context, id int64, expired shiftExpired) (store.ThreadRow, error) {
	closed, err := s.store.ExpireUnbriefed(ctx, id, team.Clip(expired.Error(), team.MaxFailure))
	if err == nil {
		s.publishThread(closed)
	}
	return closed, err
}

// errRoundExpired holds a round's message its shift's deadline overtook
// before it was submitted (sendReport).
var errRoundExpired = errors.New("the shift's deadline passed before the round's message went")

// expireUnbriefed closes thread id when its shift's deadline passed before
// its brief reached the specialist: under the shift's lock and outboxMu,
// which every placement of the brief takes, every attempt still queued is
// withdrawn, and the thread closes only if none is in the transcript
// (store.ExpireUnbriefed). One that is began round 1, which finishes.
func (s *Server) expireUnbriefed(ctx context.Context, id int64) error {
	defer s.lockShift(id)()
	s.outboxMu.Lock()
	defer s.outboxMu.Unlock()
	thread, err := s.store.Thread(ctx, id)
	if err != nil || thread.Briefed || thread.Session == 0 || thread.State == store.ThreadClosed {
		return err
	}
	placed := func() (bool, error) {
		for attempt := 0; attempt <= thread.BriefAttempt; attempt++ {
			where, err := s.engine.Placed(ctx, thread.Session, attemptKey(briefKey(thread), attempt))
			if err != nil || where == PlacementPlaced {
				return err == nil, err
			}
		}
		return false, nil
	}
	if began, err := placed(); err != nil || began {
		return err
	}
	for attempt := 0; attempt <= thread.BriefAttempt; attempt++ {
		if _, err := s.engine.Withdraw(ctx, thread.Session, attemptKey(briefKey(thread), attempt)); err != nil {
			return fmt.Errorf("withdraw the queued brief: %w", err)
		}
	}
	// A run may have placed it as it was withdrawn.
	if began, err := placed(); err != nil || began {
		return err
	}
	// The close and its event under workMu, as every thread change's
	// (Codex review of #36).
	s.workMu.Lock()
	closed, err := s.expireBrief(ctx, id, shiftExpired{errors.New(team.ShiftWhy(team.ShiftDeadline, "its brief had not reached the specialist", 0, 0))})
	s.workMu.Unlock()
	if err == nil && closed.State == store.ThreadClosed {
		// Its place on the desk is free: a review held back for it starts.
		s.reviewSoon()
	}
	return err
}
