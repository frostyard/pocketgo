package api

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/frostyard/pocketgo/service/internal/github"
	"github.com/frostyard/pocketgo/service/internal/profiles"
	"github.com/frostyard/pocketgo/service/internal/store"
	"github.com/frostyard/pocketgo/service/internal/team"
)

// Watches (ADR-0072): team_watch keeps a session's watch of a pull
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
// kept.
func (s *Server) addWatch(ctx context.Context, call TeamCall, c team.WatchCall) (string, error) {
	plan, err := team.CheckWatch(c)
	if err != nil {
		return "", err
	}
	if err := github.CheckRepository(plan.Repository); err != nil {
		return "", err
	}
	s.sessionMu.Lock()
	started, every := s.pullsStarted, s.pullEvery
	s.sessionMu.Unlock()
	if started && every <= 0 {
		return "", errors.New(team.WatchesOff)
	}
	p, err := s.readWatched(ctx, plan.Repository, plan.Number, 0)
	if err != nil {
		return "", fmt.Errorf("pocketd cannot read %s#%d on GitHub, so it cannot watch it: %w", plan.Repository, plan.Number, err)
	}
	seen := watchSeen(p)
	if team.WatchNeedsCI(plan.Until, seen.Open) {
		// CI that cannot be read now leaves the watch to its rounds.
		s.readWatchedCI(ctx, &seen, p.Repository)
	}
	outcome := team.WatchDecide(plan.Until, seen)
	row := store.WatchRow{Key: call.key(), SessionID: call.Session, Repository: p.Repository, Number: p.Number, GitHubID: p.ID,
		Until: plan.Until, Hours: plan.Hours, Deadline: plan.Deadline(s.now())}
	if outcome != "" {
		row.State, row.Outcome, row.Seen = store.WatchEnded, outcome, team.WatchSeenText(seen)
	}
	made, _, err := s.store.AddWatch(context.WithoutCancel(ctx), row, team.MaxWatchesPerSession, team.MaxWatches, func(w store.WatchRow) string {
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

// readWatched reads pull request number of repository on GitHub, bound by
// pullTimeout. id, when set, is GitHub's own id of the pull request the
// watch read first: an answer of another one, as after a repository's
// name was reused, is an error.
func (s *Server) readWatched(ctx context.Context, repository string, number, id int64) (github.Pull, error) {
	read, cancel := s.withTimeout(ctx, pullTimeout)
	defer cancel()
	p, err := github.Read(read, s.engine.GH, repository, number)
	if err != nil {
		return github.Pull{}, err
	}
	if id != 0 && p.ID != id {
		return github.Pull{}, fmt.Errorf("GitHub answers %s#%d with another pull request than the one watched", repository, number)
	}
	return p, nil
}

// readWatchedCI reads CI at seen's head, bound by pullTimeout, into seen;
// one it cannot read leaves seen.CIRead false.
func (s *Server) readWatchedCI(ctx context.Context, seen *team.WatchSeen, repository string) {
	read, cancel := s.withTimeout(ctx, pullTimeout)
	defer cancel()
	state, failing, err := github.CIChecks(read, s.engine.GH, repository, seen.Head)
	if err != nil {
		s.log.Warn("read a watched pull request's CI", "repository", repository, "head", seen.Head, "err", err)
		return
	}
	seen.CIRead, seen.CI, seen.CIDone, seen.Failing = true, state, state != github.CIPending, failing
}

// watchSeen is what a read of p says to a watch, before its CI.
func watchSeen(p github.Pull) team.WatchSeen {
	return team.WatchSeen{Open: p.State == github.Open, Merged: p.State == github.Merged, Title: p.Title, Head: p.HeadSHA}
}

// watchRead is a pass's read of one pull request.
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
func (s *Server) watchPass(ctx context.Context) error {
	s.watchMu.Lock()
	defer s.watchMu.Unlock()
	rows, err := s.store.LiveWatches(ctx)
	if err != nil {
		return err
	}
	reads := map[string]watchRead{}
	cis := map[string]team.WatchSeen{}
	filed := false
	var failures []error
	for _, w := range rows {
		ref := strings.ToLower(w.Ref())
		r, done := reads[ref]
		if !done {
			r.pull, r.err = s.readWatched(ctx, w.Repository, w.Number, w.GitHubID)
			reads[ref] = r
		}
		var seen *team.WatchSeen
		outcome, readErr := "", ""
		if r.err == nil {
			got := watchSeen(r.pull)
			if team.WatchNeedsCI(w.Until, got.Open) {
				at := ref + "@" + got.Head
				ci, done := cis[at]
				if !done {
					ci = got
					s.readWatchedCI(ctx, &ci, r.pull.Repository)
					cis[at] = ci
				}
				got = ci
			}
			seen, outcome = &got, team.WatchDecide(w.Until, got)
		} else {
			readErr = r.err.Error()
		}
		// A stop ends the pass: a read it cut short is no answer, and ends
		// no watch past its deadline.
		if err := ctx.Err(); err != nil {
			failures = append(failures, err)
			break
		}
		if outcome == "" && !s.now().Before(w.Deadline) {
			outcome = team.WatchExpired
		}
		if outcome == "" {
			if r.err != nil {
				failures = append(failures, fmt.Errorf("watch %s: %w", team.WatchID(w.ID), r.err))
			}
			continue
		}
		seenText := "unread: " + readErr
		if seen != nil {
			seenText = team.WatchSeenText(*seen)
		}
		note := team.WatchNoteText(w.ID, w.Ref(), w.Until, outcome, seen, readErr, s.now().Sub(w.CreatedAt))
		_, ended, err := s.store.EndWatch(ctx, w.ID, outcome, seenText, note)
		if err != nil {
			failures = append(failures, fmt.Errorf("end watch %s: %w", team.WatchID(w.ID), err))
			continue
		}
		filed = filed || ended
	}
	if filed {
		s.deliverLater()
	}
	return errors.Join(failures...)
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
	filed := false
	var failures []error
	for _, w := range rows {
		note := team.WatchNoteText(w.ID, w.Ref(), w.Until, team.WatchUnchecked, nil, "", s.now().Sub(w.CreatedAt))
		_, ended, err := s.store.EndWatch(ctx, w.ID, team.WatchUnchecked, "", note)
		if err != nil {
			failures = append(failures, fmt.Errorf("end watch %s: %w", team.WatchID(w.ID), err))
			continue
		}
		filed = filed || ended
	}
	if filed {
		s.deliverLater()
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
