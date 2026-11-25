package api

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/frostyard/pocketgo/service/internal/github"
	"github.com/frostyard/pocketgo/service/internal/profiles"
	"github.com/frostyard/pocketgo/service/internal/store"
	"github.com/frostyard/pocketgo/service/internal/team"
)

// Watches (ADR-0073): team_watch keeps a session's watch of a pull
// request, and pocketd checks it in code in each pull round
// (Server.watchPass), reading the pull request, and CI at its head for
// checks, on GitHub through Engine.GH in the sandbox. A watch's end files
// its one note to its session in the transaction that ends it
// (store.EndWatch), which the delivery pass places, only to an idle
// session and only while its thread is open. A thread's close ends its
// specialist's watches with no note (store.threadClosed).

// TeamWatch runs team_watch for session call.Session: add, list or
// cancel. An add is recorded once per call (api.TeamCall), with its
// answer: run again, it answers as it did, whatever the rules or GitHub
// say by then. Its caller is checked with its profile as it is now and
// what the session works as (team.CheckWatchCaller).
func (s *Server) TeamWatch(ctx context.Context, call TeamCall, c team.WatchCall) (string, error) {
	if err := team.CheckWatchCall(c); err != nil {
		return "", err
	}
	db := context.WithoutCancel(ctx)
	if c.Action == team.WatchAdd {
		if row, err := s.store.WatchByKey(db, call.key()); err == nil {
			return row.Answer, nil
		} else if !errors.Is(err, store.ErrNotFound) {
			return "", err
		}
	}
	caller, role, err := s.sessionCaller(db, call.Session)
	if err != nil {
		return "", err
	}
	if err := team.CheckWatchCaller(caller, role); err != nil {
		return "", err
	}
	switch c.Action {
	case team.WatchList:
		rows, err := s.store.SessionWatches(db, call.Session, team.MaxWatchesListed)
		if err != nil {
			return "", err
		}
		lines := make([]team.WatchLine, len(rows))
		for i, w := range rows {
			lines[i] = team.WatchLine{ID: w.ID, Ref: w.Ref(), Until: w.Until, Live: w.State == store.WatchLive, Deadline: w.Deadline, Outcome: w.Outcome}
			if w.EndedAt != nil {
				lines[i].Ended = *w.EndedAt
			}
		}
		return team.WatchListText(lines), nil
	case team.WatchCancel:
		id, err := team.ParseWatchID(c.ID)
		if err != nil {
			return "", err
		}
		row, cancelled, err := s.store.CancelWatch(db, id, call.Session)
		if errors.Is(err, store.ErrNotFound) {
			return "", fmt.Errorf("this session has no watch %s", team.WatchID(id))
		} else if err != nil {
			return "", err
		}
		return team.WatchCancelledText(row.ID, cancelled, row.Outcome), nil
	}
	return s.addWatch(ctx, call, c)
}

// addWatch keeps an add's watch. The pull request is read on GitHub once,
// so one pocketd cannot read is refused at once, and one at its condition,
// or past reaching it, is answered at once, with no watch to check and no
// note. With no pull rounds nothing would check a watch, and none is
// kept. A call stopped while it read keeps none either.
func (s *Server) addWatch(ctx context.Context, call TeamCall, c team.WatchCall) (string, error) {
	plan, err := team.CheckWatch(c)
	if err != nil {
		return "", err
	}
	if err := github.CheckRepository(plan.Repository); err != nil {
		return "", err
	}
	if s.watchesStopped() {
		return "", errors.New(team.WatchesOff)
	}
	p, err := s.readWatched(ctx, s.engine.GH, plan.Repository, plan.Number)
	if err != nil {
		return "", fmt.Errorf("pocketd cannot read %s#%d on GitHub, so it cannot watch it: %w", plan.Repository, plan.Number, err)
	}
	seen := watchSeen(p)
	if team.WatchNeedsCI(plan.Until, seen.Open) {
		// CI that cannot be read now leaves the watch to its rounds.
		ci := s.readWatchedCI(ctx, s.engine.GH, p.Repository, seen.Head)
		ci.apply(&seen)
	}
	// A call stopped while it read keeps no watch: CI its abort cut short
	// is no unreadable CI, which the read's own timeout or GitHub's
	// failure would be (Codex review of #173, round 1).
	if err := ctx.Err(); err != nil {
		return "", fmt.Errorf("the watch was stopped: %w", err)
	}
	if s.afterWatchRead != nil {
		s.afterWatchRead()
	}
	// A closed thread's specialist may run on a message that reopens the
	// thread once noted: its placed messages are noted first, under
	// outboxMu, as a report's are, and the insert's own transaction still
	// checks the thread against a close (Codex review of #173, round 3).
	if thread, err := s.store.ThreadOfSession(context.WithoutCancel(ctx), call.Session); err == nil && thread.State == store.ThreadClosed {
		s.outboxMu.Lock()
		err := s.noteSendsPlaced(context.WithoutCancel(ctx), thread.ID)
		s.outboxMu.Unlock()
		if err != nil {
			return "", err
		}
	} else if err != nil && !errors.Is(err, store.ErrNotFound) {
		return "", err
	}
	outcome := team.WatchDecide(plan.Until, seen)
	row := store.WatchRow{Key: call.key(), SessionID: call.Session, Repository: p.Repository, Number: p.Number, GitHubID: p.ID,
		Until: plan.Until, Hours: plan.Hours, Deadline: plan.Deadline(s.now())}
	if outcome != "" {
		row.State, row.Outcome, row.Seen = store.WatchEnded, outcome, team.WatchSeenText(seen)
	}
	s.sessionMu.Lock()
	every := s.pullEvery
	s.sessionMu.Unlock()
	// The last check and the insert are one step against StartPulls(0):
	// a watch it would not sweep is never kept (Codex review of #173,
	// round 1).
	s.watchAdmitMu.Lock()
	defer s.watchAdmitMu.Unlock()
	if s.watchesOff {
		return "", errors.New(team.WatchesOff)
	}
	// A call stopped while it waited for the admission keeps no watch
	// either, and its transaction takes the call's context: one stopped
	// before it commits keeps nothing (Codex review of #173, round 2).
	if err := ctx.Err(); err != nil {
		return "", fmt.Errorf("the watch was stopped: %w", err)
	}
	made, _, err := s.store.AddWatch(ctx, row, team.MaxWatchesPerSession, team.MaxWatches, func(w store.WatchRow) string {
		if outcome != "" {
			return team.WatchAtOnceText(w.Ref(), w.Until, outcome, seen)
		}
		return team.WatchAddedText(w.ID, w.Ref(), w.Until, w.Hours, w.Deadline, every)
	})
	if errors.Is(err, store.ErrConflict) {
		return "", errors.New(strings.TrimPrefix(err.Error(), store.ErrConflict.Error()+": "))
	} else if err != nil {
		return "", err
	}
	return made.Answer, nil
}

// watchesStopped says pocketd reads no pull requests in rounds: StartPulls
// ran with none, or is ending the watches for it.
func (s *Server) watchesStopped() bool {
	s.watchAdmitMu.Lock()
	defer s.watchAdmitMu.Unlock()
	return s.watchesOff
}

// readWatched reads pull request number of repository on GitHub with gh,
// bound by pullTimeout.
func (s *Server) readWatched(ctx context.Context, gh github.GH, repository string, number int64) (github.Pull, error) {
	read, cancel := s.withTimeout(ctx, pullTimeout)
	defer cancel()
	return github.Read(read, gh, repository, number)
}

// watchCI is CI at one commit as a watch reads it: read says it was, and
// err why it was not.
type watchCI struct {
	read    bool
	state   string
	failing []string
	err     string
}

// apply puts ci into seen, which keeps its own pull request's state and
// title.
func (ci watchCI) apply(seen *team.WatchSeen) {
	if ci.read {
		seen.CIRead, seen.CI, seen.CIDone, seen.Failing = true, ci.state, ci.state != github.CIPending, ci.failing
		return
	}
	seen.CIError = ci.err
}

// readWatchedCI reads CI at commit head of repository with gh, bound by
// pullTimeout; one it cannot read is not read, and says why.
func (s *Server) readWatchedCI(ctx context.Context, gh github.GH, repository, head string) watchCI {
	read, cancel := s.withTimeout(ctx, pullTimeout)
	defer cancel()
	state, failing, err := github.CIChecks(read, gh, repository, head)
	if err != nil {
		if !errors.Is(err, errWatchReadsSpent) {
			s.log.Warn("read a watched pull request's CI", "repository", repository, "head", head, "err", err)
		}
		return watchCI{err: err.Error()}
	}
	return watchCI{read: true, state: state, failing: failing}
}

// errWatchReadsSpent refuses a watch pass's GitHub request once the
// watches' budget is spent (team.ReadBudget).
var errWatchReadsSpent = errors.New(team.WatchReadsSpent)

// budgetedGH is gh for a watch pass: each request takes one from the
// watches' budget, and one it has none for is refused before gh runs.
// watchMu is held.
func (s *Server) budgetedGH() github.GH {
	return func(ctx context.Context, args ...string) (string, error) {
		if !s.watchBudget.Take(s.now()) {
			return "", errWatchReadsSpent
		}
		return s.engine.GH(ctx, args...)
	}
}

// watchSeen is what a read of p says to a watch, before its CI.
func watchSeen(p github.Pull) team.WatchSeen {
	return team.WatchSeen{Open: p.State == github.Open, Merged: p.State == github.Merged, Title: p.Title, Head: p.HeadSHA}
}

// watchRead is a pass's read of one pull request, whichever watch asked:
// each watch checks GitHub's id of it against its own.
type watchRead struct {
	pull github.Pull
	err  error
}

// watchPass checks every live watch once, one pass at a time: it reads
// each watched pull request on GitHub, once a pass whichever watches name
// it, and CI at its head for a watch until checks, once a pass for that
// head. A watch whose condition holds, whose pull request can no longer
// reach it, or whose time is up ends, and files its one note in the
// transaction that ends it (store.EndWatch); a delivery pass places it. A
// read that fails leaves the watch for the next pass, until its time is
// up. Each pull round runs it after its reads (pullRound).
//
// Its GitHub requests come from the watches' budget (team.ReadBudget),
// however many pages CI has (Codex review of #173, round 2): once it is
// spent, the rest wait for a later pass, which starts at the first watch
// this one could not read, so none waits behind the others for good. A
// note filed wakes the delivery pass at once, never after the reads still
// to come.
func (s *Server) watchPass(ctx context.Context) error {
	s.watchMu.Lock()
	defer s.watchMu.Unlock()
	rows, err := s.store.LiveWatches(ctx)
	if err != nil {
		return err
	}
	// From the watch the last pass could not read on, then the others.
	if at := slices.IndexFunc(rows, func(w store.WatchRow) bool { return w.ID >= s.watchNext }); at > 0 {
		rows = slices.Concat(rows[at:], rows[:at])
	}
	s.watchNext = 0
	gh := s.budgetedGH()
	reads := map[string]watchRead{}
	cis := map[string]watchCI{}
	var failures []error
	// held says the budget held back a watch's read: no later watch of the
	// pass makes a request, so the budget builds up for that whole read,
	// which the next pass starts with (Codex review of #173, round 3). A
	// watch the pass's reads answer whole still decides from them (round
	// 4).
	held := false
	for _, w := range rows {
		ref := strings.ToLower(w.Ref())
		r, done := reads[ref]
		// ciHeld says only this watch's CI waits: the pass read its pull
		// request already, and that read still answers it (Codex review of
		// #173, round 5).
		ciHeld := false
		if need := s.watchReadNeed(w, r, done, cis); need > 0 && (held || !s.watchBudget.Has(s.now(), need)) {
			held = true
			if done && r.err == nil {
				ciHeld = true
			} else {
				done, r = true, watchRead{err: errWatchReadsSpent}
			}
		}
		if !done {
			r.pull, r.err = s.readWatched(ctx, gh, w.Repository, w.Number)
			reads[ref] = r
		}
		spent := errors.Is(r.err, errWatchReadsSpent)
		// GitHub's answer is this watch's only for the pull request the add
		// read, by its id: a repository's name reused answers another one
		// (Codex review of #173, round 1).
		readErr := r.err
		if readErr == nil && w.GitHubID != 0 && r.pull.ID != w.GitHubID {
			readErr = fmt.Errorf("GitHub answers %s with another pull request than the one watched", w.Ref())
		}
		var seen *team.WatchSeen
		outcome := ""
		if readErr == nil {
			got := watchSeen(r.pull)
			if team.WatchNeedsCI(w.Until, got.Open) {
				// CI is a commit's, read once a pass for its repository and
				// commit, whichever pull request has it at its head.
				at := strings.ToLower(r.pull.Repository) + "@" + got.Head
				ci, done := cis[at]
				switch {
				case done:
				case ciHeld:
					ci = watchCI{err: errWatchReadsSpent.Error()}
				default:
					ci = s.readWatchedCI(ctx, gh, r.pull.Repository, got.Head)
					cis[at] = ci
				}
				ci.apply(&got)
				spent = spent || (!ci.read && strings.Contains(ci.err, team.WatchReadsSpent))
			}
			seen, outcome = &got, team.WatchDecide(w.Until, got)
		}
		// A stop ends the pass: a read it cut short is no answer, and ends
		// no watch past its deadline.
		if err := ctx.Err(); err != nil {
			failures = append(failures, err)
			break
		}
		if spent && s.watchNext == 0 {
			s.watchNext = w.ID
		}
		if outcome == "" && !s.now().Before(w.Deadline) {
			outcome = team.WatchExpired
		}
		if outcome == "" {
			// A read the budget held back is no failure: the watch waits.
			if readErr != nil && !spent {
				failures = append(failures, fmt.Errorf("watch %s: %w", team.WatchID(w.ID), readErr))
			}
			continue
		}
		seenText, why := "", ""
		if seen != nil {
			seenText = team.WatchSeenText(*seen)
		} else {
			why = readErr.Error()
			seenText = "unread: " + why
		}
		note := team.WatchNoteText(w.ID, w.Ref(), w.Until, outcome, seen, why, s.now().Sub(w.CreatedAt))
		_, ended, err := s.store.EndWatch(ctx, w.ID, outcome, seenText, note)
		if err != nil {
			failures = append(failures, fmt.Errorf("end watch %s: %w", team.WatchID(w.ID), err))
			continue
		}
		// Its note goes now, not after the reads still to come (Codex
		// review of #173, round 2): deliverLater runs one pass at a time.
		if ended {
			s.deliverLater()
		}
	}
	return errors.Join(failures...)
}

// watchSoon runs a watch pass on its own goroutine (goWatch), one at a
// time: a pass asked for while one runs runs once after it. A pull round
// starts it after its reads and never waits for it, so slow reads of
// watched pull requests never put off the next round (Codex review of
// #173, round 4). A pass counts for Stop once it runs.
func (s *Server) watchSoon() {
	s.sessionMu.Lock()
	defer s.sessionMu.Unlock()
	if s.stopped {
		return
	}
	if s.watching {
		s.watchAgain = true
		return
	}
	s.watching = true
	s.goWatch(func() {
		for {
			s.sessionMu.Lock()
			if s.stopped {
				s.watching = false
				s.sessionMu.Unlock()
				return
			}
			s.passes.Add(1)
			s.sessionMu.Unlock()
			if err := s.watchPass(s.passCtx); err != nil {
				s.log.Warn("check watches", "err", err)
			}
			s.passes.Done()
			s.sessionMu.Lock()
			again := s.watchAgain && !s.stopped
			s.watchAgain, s.watching = false, again
			s.sessionMu.Unlock()
			if !again {
				return
			}
		}
	})
}

// watchReadNeed is how many GitHub requests w's whole read may still take
// in a pass, at most: its pull request's read, unless the pass read it
// (r, done), and for checks every page of CI, unless the pass read CI at
// its head already (cis) or the pull request is not open.
func (s *Server) watchReadNeed(w store.WatchRow, r watchRead, done bool, cis map[string]watchCI) int {
	need := 0
	if !done {
		need++
	}
	if w.Until != team.UntilChecks {
		return need
	}
	if done {
		if r.err != nil || r.pull.State != github.Open {
			return need
		}
		if _, read := cis[strings.ToLower(r.pull.Repository)+"@"+r.pull.HeadSHA]; read {
			return need
		}
	}
	return need + github.MaxCIRequests
}

// uncheckedPass ends the live watches no round checks
// (endUncheckedWatches), and runs again after retryDelay until it has: a
// store that failed leaves no watch live for good, and admissions stay
// closed (Codex review of #173, round 2). Stop ends its retries.
func (s *Server) uncheckedPass() {
	s.sessionMu.Lock()
	if s.stopped {
		s.sessionMu.Unlock()
		return
	}
	s.passes.Add(1)
	s.sessionMu.Unlock()
	defer s.passes.Done()
	err := s.endUncheckedWatches(s.passCtx)
	if err == nil {
		return
	}
	s.log.Warn("end the watches no round checks", "err", err)
	s.sessionMu.Lock()
	defer s.sessionMu.Unlock()
	if !s.stopped {
		s.retry(s.uncheckedPass)
	}
}

// endUncheckedWatches ends every live watch with its note when pocketd
// reads no pull requests in rounds (-pr-every 0), so nothing would check
// it: one kept before this start, or by a call before StartPulls.
func (s *Server) endUncheckedWatches(ctx context.Context) error {
	s.watchMu.Lock()
	defer s.watchMu.Unlock()
	rows, err := s.store.LiveWatches(ctx)
	if err != nil {
		return err
	}
	var failures []error
	for _, w := range rows {
		if s.beforeUncheckedEnd != nil {
			s.beforeUncheckedEnd(w.ID)
		}
		note := team.WatchNoteText(w.ID, w.Ref(), w.Until, team.WatchUnchecked, nil, "", s.now().Sub(w.CreatedAt))
		_, ended, err := s.store.EndWatch(ctx, w.ID, team.WatchUnchecked, "", note)
		if err != nil {
			failures = append(failures, fmt.Errorf("end watch %s: %w", team.WatchID(w.ID), err))
			continue
		}
		// Its note goes now, not after the other watches' ends, as a
		// pass's does (Codex review of #173, round 5).
		if ended {
			s.deliverLater()
		}
	}
	return errors.Join(failures...)
}

// errWatchClosed is a watch's note to the specialist of a thread that
// closed: it goes to no one.
var errWatchClosed = errors.New("the watching session's thread is closed")

// watchNoteGoes says whether a watch's note may go to session now, right
// before it is submitted: not to a closed thread's specialist, whose
// close dropped it (store.endThreadWatches), also one that closed since
// the note was read.
func (s *Server) watchNoteGoes(ctx context.Context, session int64) error {
	thread, err := s.store.ThreadOfSession(ctx, session)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return nil
	case err != nil:
		return err
	case thread.State == store.ThreadClosed:
		return errWatchClosed
	}
	return nil
}

// recoveredWatchTool gives team_watch to thread's specialist session, one
// an earlier run made from profile, when it gets it now (team.MayWatch,
// with the thread's role read now: a shift's gets none). A profile pocketd
// cannot read now leaves the session as it is, for the next start's
// upgrade, rather than holding back the thread's start.
func (s *Server) recoveredWatchTool(ctx context.Context, session int64, thread store.ThreadRow, profile string) error {
	if s.profiles == nil || profile == "" {
		return nil
	}
	p, err := s.profiles.Find(profile)
	if err != nil {
		if profiles.Unreadable(err) {
			s.log.Warn("read a recovered specialist's profile for team_watch", "thread", thread.ID, "err", err)
		}
		return nil
	}
	role := team.SessionRole{Thread: true, Review: thread.ReviewPull != 0, Plan: thread.IsPlanThread()}
	if _, err := s.store.Shift(ctx, thread.ID); err == nil {
		role.Shift = true
	} else if !errors.Is(err, store.ErrNotFound) {
		return err
	}
	if !team.MayWatch(&p, role) {
		return nil
	}
	_, err = s.engine.SetWatchTool(ctx, session)
	return err
}

// upgradeWatchTools gives team_watch to every session that gets it now
// (team.MayWatch) and lacks it, but a standing one, which takes its tools
// at its own refresh, when idle and under refreshMu (ADR-0026). A profile
// it cannot read now, or a session it cannot change, is an error, and the
// others go on. Each profile is read once, and a session's role only for
// a profile whose sessions may watch.
func (s *Server) upgradeWatchTools(ctx context.Context) error {
	if s.profiles == nil {
		return nil
	}
	rows, err := s.store.Sessions(ctx)
	if err != nil {
		return err
	}
	read := map[string]*profiles.Profile{}
	unreadable := map[string]bool{}
	var failures []error
	for _, row := range rows {
		if row.Profile == "" || row.Standing || unreadable[row.Profile] {
			continue
		}
		p, known := read[row.Profile]
		if !known {
			found, err := s.profiles.Find(row.Profile)
			if profiles.Unreadable(err) {
				unreadable[row.Profile] = true
				failures = append(failures, fmt.Errorf("profile %s: %w", row.Profile, err))
				continue
			}
			if err == nil {
				p = &found
			}
			read[row.Profile] = p
		}
		// The roles matter only for a profile whose sessions may watch.
		if !team.MayWatch(p, team.SessionRole{}) {
			continue
		}
		role, err := s.sessionRole(ctx, row.ID)
		if err != nil {
			failures = append(failures, fmt.Errorf("session %d: %w", row.ID, err))
			continue
		}
		if !team.MayWatch(p, role) {
			continue
		}
		if changed, err := s.engine.SetWatchTool(ctx, row.ID); err != nil {
			failures = append(failures, fmt.Errorf("session %d: %w", row.ID, err))
		} else if changed {
			s.log.Info("gave a session team_watch", "session", row.ID)
		}
	}
	return errors.Join(failures...)
}
