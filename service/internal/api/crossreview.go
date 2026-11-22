package api

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"

	"github.com/frostyard/pocketgo/proto"
	"github.com/frostyard/pocketgo/service/internal/github"
	"github.com/frostyard/pocketgo/service/internal/profiles"
	"github.com/frostyard/pocketgo/service/internal/router"
	"github.com/frostyard/pocketgo/service/internal/store"
	"github.com/frostyard/pocketgo/service/internal/team"
	"github.com/frostyard/pocketgo/service/internal/worktree"
)

// Cross-reviews pocketd starts (ADR-0019): one owner, this code. A linked
// pull request that is open, not a draft and at a head pocketd read gets
// one review thread at that head while its budget lasts: the author's own
// agent on the reviewer model of another family, in a worktree detached at
// the head, with read-only tools and team_verdict. A verdict closes it and
// tells the author's thread once.

var fullCommit = regexp.MustCompile(`^[0-9a-f]{40}$`)

// reviewGit is the git a review thread's folder takes, in the sandbox
// (AGENTS.md: git on an agent's checkout runs there): its worktree
// detached at the head, the check of it again, and where the head leaves
// its base branch. Tests replace it.
type reviewGit interface {
	add(ctx context.Context, checkout, path, head, repository string, number int64) error
	usable(ctx context.Context, checkout, path, head string) error
	// has says whether the review's worktree at path holds commit sha;
	// fetchBranch fetches branch of repository there, in which it does.
	has(ctx context.Context, path, sha string) (bool, error)
	fetchBranch(ctx context.Context, path, repository, branch string) error
	// isolated checks, right before team_diff runs git at path, what no
	// option of git's turns off (worktree.AttributesIsolated).
	isolated(ctx context.Context, path string) error
}

// sandboxGit is reviewGit through the engine's sandboxed git.
type sandboxGit struct{ s *Server }

func (g sandboxGit) add(ctx context.Context, checkout, path, head, repository string, number int64) error {
	return worktree.AddDetached(ctx, g.s.worktreeTools(), checkout, path, head, g.s.repoURL(repository),
		"refs/pull/"+strconv.FormatInt(number, 10)+"/head")
}

func (g sandboxGit) usable(ctx context.Context, checkout, path, head string) error {
	return worktree.UsableDetached(ctx, g.s.worktreeTools(), checkout, path, head)
}

func (g sandboxGit) has(ctx context.Context, path, sha string) (bool, error) {
	_, err := g.s.engine.Git(ctx, path, append(slices.Clone(worktree.ReviewGitOptions), "cat-file", "-e", sha+"^{commit}")...)
	var exit *worktree.GitExit
	switch {
	case err == nil:
		return true, nil
	case errors.As(err, &exit):
		return false, nil
	}
	return false, err
}

func (g sandboxGit) isolated(ctx context.Context, path string) error {
	return worktree.AttributesIsolated(ctx, g.s.worktreeTools(), path)
}

func (g sandboxGit) fetchBranch(ctx context.Context, path, repository, branch string) error {
	_, err := g.s.engine.Git(ctx, path, append(slices.Clone(worktree.ReviewGitOptions), "fetch", "--no-tags", "--", g.s.repoURL(repository),
		"refs/heads/"+branch)...)
	return err
}

// reviewWork is what a review pass starts from: the pull requests read
// just now, each at the read that found it (fresh: its id's ReadSeq),
// those to read on GitHub first, whatever they need (read), and the
// sessions of review threads closed outside the pass whose runs it stops
// (stop), and the threads whose pull requests may have merged out (merged:
// a thread's id). A nil fresh is every open pull request as stored, which
// only tests ask for. The pass notes in failed the pull requests whose
// read failed in it: none of them is fresh, and none is read again in it;
// and in merged the threads of those its reads found merged or closed.
type reviewWork struct {
	fresh  map[int64]int64
	read   map[int64]bool
	stop   map[int64]bool
	failed map[int64]bool
	merged map[int64]bool
}

// newReviewWork is reviewWork with every map made: it asks for nothing.
func newReviewWork() reviewWork {
	return reviewWork{fresh: map[int64]int64{}, read: map[int64]bool{}, stop: map[int64]bool{}, failed: map[int64]bool{},
		merged: map[int64]bool{}}
}

// isFresh says whether p, as stored now, is the read w names: a later
// read, which may have failed, is not (Codex review of #26, round 9), and
// neither is one whose read failed in this pass (round 12).
func (w reviewWork) isFresh(p store.PullRow) bool {
	if w.failed[p.ID] {
		return false
	}
	if w.fresh == nil {
		return true
	}
	seq, ok := w.fresh[p.ID]
	return ok && seq == p.ReadSeq
}

// readFor reads pull request p on GitHub for a pass, as refreshPull does,
// and notes the outcome in w: the read is fresh, or it failed, and a read
// queued before it is not fresh either. A read that finds it merged or
// closed has its thread checked for a close once the pass's reads are
// done (closeMergedOut).
func (s *Server) readFor(ctx context.Context, p store.PullRow, w reviewWork) (store.PullRow, error) {
	read, err := s.refreshPull(ctx, p)
	if err != nil {
		delete(w.fresh, p.ID)
		if w.failed != nil {
			w.failed[p.ID] = true
		}
		return read, err
	}
	if w.fresh != nil {
		w.fresh[p.ID] = read.ReadSeq
	}
	if read.State != store.PullOpen && w.merged != nil {
		w.merged[read.ThreadID] = true
	}
	return read, nil
}

// closeMergedOut closes each thread of w.merged whose pull requests merged
// out (closeIfMerged), as the pass's reads left them recorded: a thread
// closes whatever order its pull requests were read in, and whichever
// read of the pass found them (Codex review of #32, round 1). One closed
// already is left alone.
func (s *Server) closeMergedOut(ctx context.Context, w reviewWork) {
	for id := range w.merged {
		thread, err := s.store.Thread(ctx, id)
		if err == nil && thread.State != store.ThreadClosed {
			err = s.closeIfMerged(ctx, id)
		}
		if err != nil {
			s.log.Warn("close a thread whose pull requests merged", "thread", id, "err", err)
		}
	}
}

// reviewSoon runs a review pass on its own goroutine, with the pull
// requests fresh read just now: a pass already running runs again after
// it.
func (s *Server) reviewSoon(fresh ...store.PullRow) {
	s.reviewWith(func(w *reviewWork) {
		for _, p := range fresh {
			w.fresh[p.ID] = p.ReadSeq
		}
	})
}

// stopReviewSoon has the next review pass stop the run of a review
// thread's session, which a close outside the pass ended: the pass scans
// only the review threads still open (Codex review of #26, round 12).
func (s *Server) stopReviewSoon(session int64) {
	s.reviewWith(func(w *reviewWork) { w.stop[session] = true })
}

// closeMergedSoon has the next review pass close thread when its pull
// requests merged out (closeMergedOut): a read outside the pass found one
// merged, and its caller does not wait for the close (Codex review of #32,
// round 1).
func (s *Server) closeMergedSoon(thread int64) {
	s.reviewWith(func(w *reviewWork) { w.merged[thread] = true })
}

// reviewAfterRead is reviewSoon for pull requests the pass reads on
// GitHub first, with their merge base and their labels, whatever their
// reviews need: a report's, off the author's tool call (Codex review of
// #26, rounds 8 and 9).
func (s *Server) reviewAfterRead(ids ...int64) {
	s.reviewWith(func(w *reviewWork) {
		for _, id := range ids {
			w.read[id] = true
		}
	})
}

// reviewWith adds to the next pass's work with add, then runs a pass.
func (s *Server) reviewWith(add func(w *reviewWork)) {
	s.sessionMu.Lock()
	defer s.sessionMu.Unlock()
	if s.stopped {
		return
	}
	if s.reviewWaiting.fresh == nil {
		s.reviewWaiting = newReviewWork()
	}
	add(&s.reviewWaiting)
	// Before the service started its rounds (StartPulls, after Resume), a
	// pass waits: it would start sessions and send briefs.
	if !s.reviewsOn || s.reviewing {
		s.reviewAgain = true
		return
	}
	s.reviewing = true
	if !s.reviewAsync {
		// A test's pass runs in the call, so what it starts is there when
		// the call returns.
		fresh := s.takeFreshLocked()
		s.sessionMu.Unlock()
		s.reviewPassOf(s.passCtx, fresh)
		s.sessionMu.Lock()
		s.reviewing = false
		return
	}
	go s.reviewLoop()
}

// takeFreshLocked is the work asked for since the last pass, and forgets
// it; sessionMu is held. Its fresh is never nil.
func (s *Server) takeFreshLocked() reviewWork {
	w := s.reviewWaiting
	if w.fresh == nil {
		w = newReviewWork()
	}
	s.reviewWaiting = reviewWork{}
	return w
}

func (s *Server) reviewLoop() {
	for {
		s.sessionMu.Lock()
		if s.stopped {
			s.reviewing = false
			s.sessionMu.Unlock()
			return
		}
		s.reviewAgain = false
		fresh := s.takeFreshLocked()
		s.passes.Add(1)
		s.sessionMu.Unlock()
		s.reviewPassOf(s.passCtx, fresh)
		s.passes.Done()
		s.sessionMu.Lock()
		if !s.reviewAgain {
			s.reviewing = false
			s.sessionMu.Unlock()
			return
		}
		s.sessionMu.Unlock()
	}
}

// startReviews turns review passes on, once the service's rounds start
// (after Resume), and runs one: it settles the reviews a stop left open,
// and runs what waited. That pass reads every open pull request on GitHub
// first, as a report's pass reads its own: a stop between a report and its
// pass, or between a read and its pass, leaves no verdict or label on what
// pocketd last knew (Codex review of #26, round 12). It also stops any run
// of a closed review's session that Resume brought back: a stop that a
// close asked for, or one that failed, is kept in memory alone (Codex
// review of #32, round 1).
func (s *Server) startReviews() {
	s.sessionMu.Lock()
	s.reviewsOn = true
	s.sessionMu.Unlock()
	pulls, err := s.store.OpenPulls(s.passCtx)
	if err != nil {
		s.log.Warn("read the pull requests to read again", "err", err)
	}
	ids := make([]int64, len(pulls))
	for i, p := range pulls {
		ids[i] = p.ID
	}
	sessions, err := s.store.ClosedReviewSessions(s.passCtx)
	if err != nil {
		s.log.Warn("read the closed reviews' sessions", "err", err)
	}
	s.reviewWith(func(w *reviewWork) {
		for _, id := range ids {
			w.read[id] = true
		}
		for _, session := range sessions {
			w.stop[session] = true
		}
	})
}

// runEnded follows a session's run that ended: a review's session that
// ended is settled now, whether or not pull requests are read in rounds
// (Codex review of #26, round 6). One its verdict closed freed a place on
// the desk: the pass starts what waited for it (round 8).
func (s *Server) runEnded(id int64) {
	if t, err := s.store.ThreadOfSession(s.passCtx, id); err == nil && t.ReviewPull != 0 {
		s.reviewSoon()
	}
	// A worker's run that ended brings its reply to its specialist
	// (ADR-0021).
	s.followWorker(id)
	// A shift's round may have ended (ADR-0024).
	s.shiftSoon()
	// A proposal session's run that ended ends its ask (ADR-0033).
	if _, err := s.store.GatherAskOfSession(s.passCtx, id); err == nil {
		s.deliverLater()
	}
	// A standing session follows its profile, and compacts early
	// (ADR-0026).
	s.standingRunEnded(id)
	// A question whose task ended with its run is skipped, as an abort
	// skips it: one whose abort a restart finished never waits again
	// (ADR-0048; Codex review of #95, round 1).
	s.questionsRunEnded(id)
	if s.afterRunEnded != nil {
		s.afterRunEnded(id)
	}
}

// reviewPass is reviewPassOf for every open pull request: tests start the
// reviews they read for themselves.
func (s *Server) reviewPass(ctx context.Context) { s.reviewPassOf(ctx, reviewWork{}) }

// reviewPassOf settles every review thread still open, whatever its pull
// request is now, then starts the reviews that the pull requests read just
// now need (w), one pass at a time: a pull request whose read failed, or
// that no round or report read, starts none on a cached state (Codex
// review of #26, round 6). What fails is logged and tried at the next
// round.
func (s *Server) reviewPassOf(ctx context.Context, w reviewWork) {
	s.reviewMu.Lock()
	defer s.reviewMu.Unlock()
	defer s.cleanPass(ctx)
	if w.failed == nil {
		w.failed = map[int64]bool{}
	}
	if w.merged == nil {
		w.merged = map[int64]bool{}
	}
	for session := range w.stop {
		if s.abortsDue == nil {
			s.abortsDue = map[int64]bool{}
		}
		s.abortsDue[session] = true
	}
	for session := range s.abortsDue {
		s.stopReviewRun(ctx, session)
	}
	// The rereads asked for come first, so nothing below settles or starts
	// on what they replace: an unbriefed review's brief never goes again at
	// the head they moved from, and one of a pull request last recorded
	// closed that reopened is read too. A reread that fails wins over a
	// fresh read queued with it, which the one who asked found out of date
	// (Codex review of #26, rounds 11 and 12). One that finds its pull
	// request merged closes its author's thread, as a round does, once the
	// pass's reads are done (ADR-0016; round 12).
	for id := range w.read {
		p, err := s.store.PullByID(ctx, id)
		if err != nil {
			w.failed[id] = true
			s.log.Warn("read a pull request to review", "pull", id, "err", err)
			continue
		}
		if _, err := s.readFor(ctx, p, w); err != nil {
			s.log.Warn("read a pull request to review", "pull", p.Ref(), "err", err)
		}
	}
	open, err := s.store.OpenReviewThreads(ctx)
	if err != nil {
		s.log.Warn("read the open reviews", "err", err)
		return
	}
	for _, t := range open {
		if err := s.settleReview(ctx, t, w); err != nil {
			s.log.Warn("cross-review", "thread", t.ID, "err", err)
		}
	}
	pulls, err := s.store.OpenPulls(ctx)
	if err != nil {
		s.log.Warn("read pull requests to review", "err", err)
		return
	}
	var starts []store.PullRow
	for _, p := range pulls {
		if !w.isFresh(p) {
			if w.failed[p.ID] {
				continue
			}
			// One no round or report read just now, which needs a review
			// by what pocketd holds, is read first: a stop between a
			// report's link and its pass, or a full desk, loses no review,
			// and none starts on a cached state (Codex review of #26,
			// rounds 6 and 7).
			if _, _, due, err := s.reviewDue(ctx, p); err != nil || !due {
				if err != nil {
					s.log.Warn("cross-review", "pull", p.Ref(), "err", err)
				}
				continue
			}
			read, readErr := s.readFor(ctx, p, w)
			if readErr != nil {
				s.log.Warn("read a pull request to review", "pull", p.Ref(), "err", readErr)
			}
			// Its reviews still open are settled against what is recorded
			// now, before another starts: a report or a confirmation may
			// have recorded a new head since the settling above, and one
			// of the old head ends, and frees its place (Codex review of
			// #26, round 10; of #32, round 1), also when this read failed
			// after it recorded the change (#26, round 12).
			threads, err := s.store.ReviewThreadsOf(ctx, p.ID)
			if err != nil {
				s.log.Warn("cross-review", "pull", p.Ref(), "err", err)
				continue
			}
			for _, t := range threads {
				if t.State == store.ThreadClosed {
					continue
				}
				if err := s.settleReview(ctx, t, w); err != nil {
					s.log.Warn("cross-review", "thread", t.ID, "err", err)
				}
			}
			if readErr != nil {
				continue
			}
			p = read
		}
		starts = append(starts, p)
	}
	// Every read of the pass is done: the threads whose pull requests they
	// found merged out close, before the starts, which take the places
	// they free.
	s.closeMergedOut(ctx, w)
	for _, p := range starts {
		if err := s.startReview(ctx, p); err != nil {
			s.log.Warn("cross-review", "pull", p.Ref(), "err", err)
		}
	}
}

// reviewObsolete says why open review thread t ends with its pull request
// p as recorded, "" when it does not: p closed, merged or became a draft,
// its head or base branch moved, or its merge base moved off the one t
// pinned. A merge base not known now moves nothing: it only keeps a brief
// from going (attachBriefOpen).
func reviewObsolete(t store.ThreadRow, p store.PullRow) string {
	switch {
	case p.State != store.PullOpen:
		return "Ended: the pull request is " + p.State + "."
	case p.Draft:
		return "Ended: the pull request is a draft."
	case t.Key != team.ReviewKey(p.Repository, p.Number, p.Base, p.HeadSHA):
		// Its head moved, or it was retargeted at the same head: its change
		// set is another (Codex review of #26, round 3).
		return "Superseded: the pull request is at " + shortSHA(p.HeadSHA) + " on " + p.Base + " now."
	case t.ReviewBase != "" && p.MergeBase != "" && t.ReviewBase != p.MergeBase:
		// Its merge base moved under the same head and branch: no verdict
		// on the base it pinned counts (Codex review of #26, round 11).
		return "Superseded: the pull request leaves " + p.Base + " at " + shortSHA(p.MergeBase) + " now."
	}
	return ""
}

// settleReview moves an open review thread on: it ends when its pull
// request closed, merged or became a draft, or its head moved; one at work
// is followed, and one still starting is started again (Codex review of
// #26, round 1: a review never outlives its pull request's state). One
// not briefed yet reads its pull request on GitHub first, unless w just
// did, so no reviewer starts on what pocketd held before a stop; a read
// that fails waits for the next pass (round 9), and still ends the review
// when what it recorded before it failed does (round 12).
func (s *Server) settleReview(ctx context.Context, t store.ThreadRow, w reviewWork) error {
	p, err := s.store.PullByID(ctx, t.ReviewPull)
	if err != nil {
		return err
	}
	if (t.State != store.ThreadOpen || !t.Briefed) && p.State == store.PullOpen && !w.isFresh(p) {
		var readErr error
		if w.failed[p.ID] {
			readErr = fmt.Errorf("%s could not be read in this pass", p.Ref())
		} else if read, err := s.readFor(ctx, p, w); err != nil {
			readErr = err
		} else {
			p = read
		}
		if readErr != nil {
			// A failed read keeps a review from starting, not from ending.
			if p, err = s.store.PullByID(ctx, t.ReviewPull); err != nil {
				return errors.Join(readErr, err)
			}
			if why := reviewObsolete(t, p); why != "" {
				return s.endReview(ctx, t, why)
			}
			return readErr
		}
	}
	if why := reviewObsolete(t, p); why != "" {
		return s.endReview(ctx, t, why)
	}
	if t.State == store.ThreadOpen && t.Briefed {
		return s.followReview(ctx, t)
	}
	row, err := s.startThread(ctx, t)
	if err != nil {
		return err
	}
	// A start that found its brief in the transcript may find the run of
	// it ended, with no busy-to-idle change still to come: it is followed
	// now (Codex review of #26, round 7).
	if row.State == store.ThreadOpen && row.Briefed {
		return s.followReview(ctx, row)
	}
	return nil
}

// reviewDue says whether pull request p, as given, needs its review
// started, with its key and its number among p's reviews: none for one not
// open, a draft, a head that already has its review or a verdict, or a
// spent budget (ADR-0019).
func (s *Server) reviewDue(ctx context.Context, p store.PullRow) (key string, n int, due bool, err error) {
	if p.State != store.PullOpen || p.Draft || !fullCommit.MatchString(p.HeadSHA) {
		return "", 0, false, nil
	}
	key = team.ReviewKey(p.Repository, p.Number, p.Base, p.HeadSHA)
	if _, err := s.store.ThreadByKey(ctx, key); err == nil {
		return "", 0, false, nil // settled above
	} else if !errors.Is(err, store.ErrNotFound) {
		return "", 0, false, err
	}
	// A verdict at this head backs it already, filed before pocketd started
	// reviews: no second review of it (Codex review of #26, round 1).
	if _, found, err := s.store.LatestReview(ctx, p); err != nil || found {
		return "", 0, false, err
	}
	threads, err := s.store.ReviewThreadsOf(ctx, p.ID)
	if err != nil {
		return "", 0, false, err
	}
	if len(threads) >= team.MaxReviews {
		return "", 0, false, nil // the budget is spent: the owner decides
	}
	return key, len(threads) + 1, true, nil
}

// startReview starts the review pull request p needs (reviewDue).
func (s *Server) startReview(ctx context.Context, p store.PullRow) error {
	key, n, due, err := s.reviewDue(ctx, p)
	if err != nil || !due {
		return err
	}
	created, ok, err := s.newReview(ctx, p, n, key)
	if err != nil || !ok {
		return err
	}
	_, err = s.startThread(ctx, created)
	return err
}

// newReview records the n-th review thread of p at its head, under teamMu
// as a delegation is, so two never both take the desk's last place. What a
// start needs is checked first, so a review that cannot start takes no
// place: the author's session and family, its profile's checkout of the
// repository, and the reviewer model on a sign-in (Codex review of #26,
// round 1). ok is false when none starts now, as for a full desk; the next
// round tries again.
func (s *Server) newReview(ctx context.Context, p store.PullRow, n int, key string) (store.ThreadRow, bool, error) {
	author, err := s.store.Thread(ctx, p.ThreadID)
	if err != nil {
		return store.ThreadRow{}, false, err
	}
	// The author's model is read, as PiG runs it, and recorded with the
	// review while no change of that session's model goes: the review
	// keeps the model the author ran, whatever came after (Codex review of
	// #48, round 3).
	if author.Session != 0 {
		defer s.lockModel(author.Session)()
	}
	model, err := s.authorModel(ctx, author)
	if err != nil || model == (proto.ModelRef{}) {
		return store.ThreadRow{}, false, err
	}
	if _, ok := router.ReviewerOf(model); !ok {
		return store.ThreadRow{}, false, nil // no family: the owner decides
	}
	if s.profiles == nil {
		return store.ThreadRow{}, false, nil
	}
	target, err := s.profiles.Find(author.Agent)
	if err != nil {
		return store.ThreadRow{}, false, err
	}
	repository, ok := team.CheckoutKey(target, p.Repository)
	if !ok {
		return store.ThreadRow{}, false, fmt.Errorf("%s's profile names no checkout of %s, so it cannot review %s", target.ID, p.Repository, p.Ref())
	}
	// Its profile's tools are checked as any start's: only the model is
	// the reviewer's (Codex review of #26, round 2).
	if err := team.CheckTarget(target, func() error { return s.checkTools(target) }); err != nil {
		return store.ThreadRow{}, false, err
	}
	models, err := s.engine.Models(ctx)
	if err != nil {
		return store.ThreadRow{}, false, err
	}
	if _, err := router.Choose(router.Request{Profile: &target, ReviewOf: model}, models); err != nil {
		return store.ThreadRow{}, false, err
	}
	s.teamMu.Lock()
	defer s.teamMu.Unlock()
	onDesk, err := s.store.OpenThreads(ctx)
	if err != nil {
		return store.ThreadRow{}, false, err
	}
	if onDesk >= s.deskCap {
		return store.ThreadRow{}, false, nil
	}
	row, err := s.changeThread(func() (store.ThreadRow, error) {
		// The review keeps the author's model its reviewer is chosen by,
		// for its start and its verdict: a change of the author's model
		// counts from the next review (Codex review of #48, round 1).
		row, _, err := s.store.CreateReviewThread(ctx, store.ThreadRow{Key: key, Title: team.ReviewTitle(p.Repository, p.Number, p.HeadSHA, n),
			Agent: author.Agent, SupervisorSession: author.SupervisorSession, Repository: repository,
			Task: "Cross-review of " + p.Ref() + " at " + p.HeadSHA, ReviewPull: p.ID, ReviewHead: p.HeadSHA, ReviewAuthor: model}, p.Base, p.ReadSeq)
		return row, err
	})
	if errors.Is(err, store.ErrConflict) {
		return store.ThreadRow{}, false, nil // the pull request moved since this pass read it
	}
	return row, err == nil, err
}

// authorModel is the model of thread's session, which wrote its change,
// as PiG runs it: a change of it whose record a stop of pocketd lost
// counts (Codex review of #48, round 3); the recorded one for an agent
// that names none. Zero for a thread with no session.
func (s *Server) authorModel(ctx context.Context, thread store.ThreadRow) (proto.ModelRef, error) {
	if thread.Session == 0 {
		return proto.ModelRef{}, nil
	}
	row, err := s.store.Session(ctx, thread.Session)
	if errors.Is(err, store.ErrNotFound) {
		return proto.ModelRef{}, nil
	} else if err != nil {
		return proto.ModelRef{}, err
	}
	runs, err := s.engine.SessionModel(ctx, thread.Session)
	if err != nil {
		return proto.ModelRef{}, err
	}
	if runs.Model != (proto.ModelRef{}) {
		return runs.Model, nil
	}
	return proto.ModelRef{Provider: row.Provider, ID: row.Model}, nil
}

// followReview ends an open review thread whose session ended with no
// verdict: it counts against the budget (ADR-0019). One that filed its
// verdict and is still open is finished now.
func (s *Server) followReview(ctx context.Context, t store.ThreadRow) error {
	if review, found, err := s.store.ReviewOfThread(ctx, t.ID); err != nil {
		return err
	} else if found {
		return s.finishReview(ctx, t, review)
	}
	busy, err := s.engine.Busy(ctx, t.Session)
	if err != nil || busy {
		return err
	}
	return s.endReview(ctx, t, "Ended with no verdict; it counts against the pull request's review budget.")
}

// endReview closes review thread t with report and tells nobody: pocketd
// started it, and the owner sees it on the board. A session still at
// work is stopped first.
func (s *Server) endReview(ctx context.Context, t store.ThreadRow, report string) error {
	session, err := s.closeReviewWithdrawn(ctx, t, report)
	if err != nil || session == 0 {
		return err
	}
	// Outside the team's locks: PiG's abort waits for the run, and a team
	// call of that run (a verdict) may wait for workMu (Codex review of
	// #26, round 3). The thread is closed already, so no brief goes again
	// and no verdict of the run is filed.
	s.stopReviewRun(ctx, session)
	return nil
}

// closeReviewWithdrawn closes review thread t with report, under outboxMu
// and workMu, after withdrawing every attempt of its brief still queued:
// no delivery pass sends it again meanwhile (Codex review of #26, rounds 1
// and 2). It gives the review's session, 0 when there is none to stop.
func (s *Server) closeReviewWithdrawn(ctx context.Context, t store.ThreadRow, report string) (int64, error) {
	s.outboxMu.Lock()
	defer s.outboxMu.Unlock()
	s.workMu.Lock()
	defer s.workMu.Unlock()
	current, err := s.store.Thread(ctx, t.ID)
	if err != nil || current.State == store.ThreadClosed {
		return 0, err
	}
	if current.Session != 0 && current.Brief != "" {
		for attempt := 0; attempt <= current.BriefAttempt; attempt++ {
			if _, err := s.engine.Withdraw(ctx, current.Session, attemptKey(briefKey(current), attempt)); err != nil {
				return 0, fmt.Errorf("withdraw the review's queued brief: %w", err)
			}
		}
	}
	closed, changed, err := s.store.CloseReview(ctx, t.ID, report, nil)
	if err != nil {
		return 0, err
	}
	if changed {
		s.publishThread(closed)
	}
	return current.Session, nil
}

// stopReviewRun stops a closed review's run, if one goes on. A review's
// session takes no message but pocketd's, so every run in it is the
// review's (Codex review of #26, round 5). A stop that fails, or a look
// that cannot tell, is tried again at each review pass; reviewPass holds
// reviewMu, which guards abortsDue.
func (s *Server) stopReviewRun(ctx context.Context, session int64) {
	busy, err := s.engine.Busy(ctx, session)
	if err == nil && busy {
		err = s.engine.Abort(ctx, session)
	}
	if err != nil {
		s.log.Warn("stop an ended review's run", "session", session, "err", err)
		if s.abortsDue == nil {
			s.abortsDue = map[int64]bool{}
		}
		s.abortsDue[session] = true
		return
	}
	delete(s.abortsDue, session)
}

// finishReview closes review thread t on its verdict r and tells the
// author's thread, once, when it is open (ADR-0019). Run again, it does
// nothing.
func (s *Server) finishReview(ctx context.Context, t store.ThreadRow, r store.ReviewRow) error {
	p, err := s.store.PullByID(ctx, r.PullID)
	if err != nil {
		return err
	}
	author, err := s.store.Thread(ctx, p.ThreadID)
	if err != nil {
		return err
	}
	var message *store.ReportRow
	if author.State != store.ThreadClosed && author.Session != 0 {
		message = &store.ReportRow{Key: "review-verdict:" + t.Owner, ThreadID: t.ID, SessionID: author.Session,
			Text: team.AuthorVerdictText(author.ID, p.Ref(), r.HeadSHA, r.Verdict, r.Summary, r.Findings)}
	}
	report := fmt.Sprintf("Verdict R%d: %s at %s is %s.", r.ID, p.Ref(), shortSHA(r.HeadSHA), r.Verdict)
	s.outboxMu.Lock()
	s.workMu.Lock()
	closed, changed, err := s.store.CloseReview(ctx, t.ID, report, message)
	if err == nil && changed {
		s.publishThread(closed)
	}
	s.workMu.Unlock()
	s.outboxMu.Unlock()
	if err == nil && changed && message != nil {
		s.deliverLater()
	}
	return err
}

// reviewSpecialist is a review thread's session, as newSpecialist makes a
// thread's: its folder a worktree detached at the head it reviews, its
// model the reviewer model of the author's family, read-only tools and
// team_verdict (ADR-0019). A profile whose own model pocketd cannot run
// still reviews: its model does not run here.
func (s *Server) reviewSpecialist(ctx context.Context, thread store.ThreadRow, target profiles.Profile, models []proto.Model) (SessionSpec, string, error) {
	choice, err := s.reviewChoice(ctx, thread, target, models)
	if err != nil {
		return SessionSpec{}, "", err
	}
	if err := team.CheckTarget(target, func() error { return s.checkTools(target) }); err != nil {
		return SessionSpec{}, "", err
	}
	repository, checkout := threadCheckout(target, thread)
	if err := team.CheckDestination(target, repository); err != nil {
		return SessionSpec{}, "", err
	}
	p, err := s.store.PullByID(ctx, thread.ReviewPull)
	if err != nil {
		return SessionSpec{}, "", err
	}
	cwd, _ := team.Worktree(checkout, target.ID, team.Slug(thread.Title, thread.ID, thread.Owner))
	gitCtx, cancel := s.withTimeout(ctx, worktreeTimeout)
	defer cancel()
	if err := s.reviewGit.add(gitCtx, checkout, cwd, thread.ReviewHead, p.Repository, p.Number); err != nil {
		return SessionSpec{}, "", fmt.Errorf("make the review's worktree: %w", err)
	}
	return SessionSpec{Title: "T" + strconv.FormatInt(thread.ID, 10) + " · " + thread.Title, Cwd: cwd, Model: choice.Model,
		Profile: &target, Route: string(choice.Rule), Thread: thread.ID, Review: true}, cwd, nil
}

// threadCheckout is the key and the checkout of thread's repository in
// profile target. A review's key is found ignoring case, as GitHub names
// repositories (team.CheckoutKey): the thread keeps the key as its
// profile spelled it when the review started, and the profile may have
// changed its case since, which a retry of its start then still finds
// (Codex review of #26, round 12). A delegation's is the key it names.
func threadCheckout(target profiles.Profile, thread store.ThreadRow) (key, checkout string) {
	key = thread.Repository
	if thread.ReviewPull != 0 {
		if found, ok := team.CheckoutKey(target, thread.Repository); ok {
			key = found
		}
	}
	return key, target.Repositories[key]
}

// reviewChoice is the reviewer model of review thread thread, by the
// author's model (router.Choose with ReviewOf): the one the review kept
// when pocketd recorded it, else, for a review recorded before it kept
// one, the author's session's now.
func (s *Server) reviewChoice(ctx context.Context, thread store.ThreadRow, target profiles.Profile, models []proto.Model) (router.Choice, error) {
	p, err := s.store.PullByID(ctx, thread.ReviewPull)
	if err != nil {
		return router.Choice{}, err
	}
	model := thread.ReviewAuthor
	if model == (proto.ModelRef{}) {
		author, err := s.store.Thread(ctx, p.ThreadID)
		if err != nil {
			return router.Choice{}, err
		}
		if model, err = s.authorModel(ctx, author); err != nil {
			return router.Choice{}, err
		}
	}
	if model == (proto.ModelRef{}) {
		return router.Choice{}, fmt.Errorf("the author of %s has no session, so no reviewer model is chosen", p.Ref())
	}
	return router.Choose(router.Request{Profile: &target, ReviewOf: model}, models)
}

// reviewBrief is review thread thread's brief: the change set from where
// its head leaves the base branch, read in the worktree after a fetch of
// that branch, and the last review for a re-review (ADR-0019).
func (s *Server) reviewBrief(ctx context.Context, thread store.ThreadRow, cwd string) (string, error) {
	p, err := s.store.PullByID(ctx, thread.ReviewPull)
	if err != nil {
		return "", err
	}
	author, err := s.store.Thread(ctx, p.ThreadID)
	if err != nil {
		return "", err
	}
	// The base is GitHub's merge base, read with the service user's
	// sign-in: no fetch runs, so a private repository needs no git
	// credential. One kept already stays, so the brief and team_diff name
	// one change set.
	base := thread.ReviewBase
	if base == "" {
		readCtx, cancel := s.withTimeout(ctx, worktreeTimeout)
		defer cancel()
		read, err := github.MergeBase(readCtx, s.engine.GH, p.Repository, p.Base, thread.ReviewHead)
		if err != nil {
			return "", err
		}
		kept, err := s.store.SetReviewBase(ctx, thread.ID, read)
		if err != nil {
			return "", err
		}
		base = kept.ReviewBase
	}
	// Both ends are readable in the worktree before the reviewer is briefed:
	// a checkout may hold the head without its base. A missing base comes
	// with a fetch of the base branch; without it no session is spent on a
	// diff that cannot be read (Codex review of #26, round 2).
	if err := s.reviewReadable(ctx, cwd, p.Repository, p.Base, base); err != nil {
		return "", err
	}
	spec := team.ReviewSpec{Thread: thread.ID, Author: s.agentName(author.Agent), Repository: p.Repository, Number: p.Number,
		BaseBranch: p.Base, Base: base, Head: thread.ReviewHead, Cwd: cwd}
	if last, found, err := s.store.LastReviewOf(ctx, p.ID); err != nil {
		return "", err
	} else if found && last.HeadSHA != thread.ReviewHead && last.MergeBase == base {
		// A re-review reads from the last reviewed head only when that
		// review read the same baseline (a retarget brings other commits
		// into scope: Codex review of #26, round 4) and the worktree holds
		// it; a force-push may have taken it (round 3). Else it is a full
		// review.
		gitCtx, cancel := s.withTimeout(ctx, worktreeTimeout)
		ok, err := s.reviewGit.has(gitCtx, cwd, last.HeadSHA)
		cancel()
		if err != nil {
			return "", err
		}
		if ok {
			spec.Prior = &team.PriorReview{Head: last.HeadSHA, Verdict: last.Verdict, Summary: last.Summary, Findings: last.Findings}
		}
	}
	return team.ReviewBrief(spec), nil
}

// repositoryURL is the HTTPS URL of a repository GitHub names owner/name,
// checked before it is linked (github.CheckRepository).
func repositoryURL(repository string) string { return "https://github.com/" + repository + ".git" }

func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// TeamDiff is the team_diff tool: the change set of the caller's review,
// read with git in the sandbox, in its worktree, with nothing the checkout
// configures run (ADR-0019). A review has no shell: this, read and
// team_verdict are all it has.
func (s *Server) TeamDiff(ctx context.Context, call TeamCall, d team.DiffRequest) (string, error) {
	thread, err := s.store.ThreadOfSession(ctx, call.Session)
	if errors.Is(err, store.ErrNotFound) || (err == nil && thread.ReviewPull == 0) {
		return "", errors.New("team_diff is for a cross-review pocketd started")
	} else if err != nil {
		return "", err
	}
	if err := team.CheckDiff(d); err != nil {
		return "", err
	}
	if !fullCommit.MatchString(thread.ReviewBase) {
		return "", fmt.Errorf("thread T%d's base is not known yet", thread.ID)
	}
	from := thread.ReviewBase
	if d.SinceLastReview {
		last, found, err := s.store.LastReviewOf(ctx, thread.ReviewPull)
		if err != nil {
			return "", err
		}
		if !found || last.HeadSHA == thread.ReviewHead || last.MergeBase != thread.ReviewBase {
			return "", errors.New("no earlier review read another head from this base: review the whole change set from the base")
		}
		gitCtx, cancel := s.withTimeout(ctx, worktreeTimeout)
		ok, err := s.reviewGit.has(gitCtx, thread.Cwd, last.HeadSHA)
		cancel()
		if err != nil {
			return "", err
		} else if !ok {
			return "", fmt.Errorf("the last reviewed head %s is not in your folder: review the whole change set from the base", shortSHA(last.HeadSHA))
		}
		from = last.HeadSHA
	}
	gitCtx, cancel := s.withTimeout(ctx, worktreeTimeout)
	defer cancel()
	// Attributes no option turns off may have come since the brief: the
	// diff runs only while none would change what it shows (Codex review
	// of #26, round 9).
	if err := s.reviewGit.isolated(gitCtx, thread.Cwd); err != nil {
		return "", err
	}
	page, more, err := s.engine.GitPage(gitCtx, thread.Cwd, d.Offset, team.DiffPage, team.DiffArgs(from, thread.ReviewHead, d)...)
	if err != nil {
		return "", err
	}
	switch {
	case page == "" && d.Offset == 0:
		return "No change between " + shortSHA(from) + " and " + shortSHA(thread.ReviewHead) + " in what you named.", nil
	case page == "":
		return "", fmt.Errorf("offset %d is past the end of the answer", d.Offset)
	}
	// A page that is not the whole says where the next one starts.
	if end := d.Offset + len(page); d.Offset > 0 || more {
		page += fmt.Sprintf("\n[bytes %d to %d", d.Offset, end)
		if more {
			page += fmt.Sprintf("; ask again with offset %d for the rest", end)
		} else {
			page += ", the end"
		}
		page += "]"
	}
	return page, nil
}

// reviewReadable makes sure base is readable in the review's worktree at
// cwd, fetching branch of repository when it is not.
func (s *Server) reviewReadable(ctx context.Context, cwd, repository, branch, base string) error {
	gitCtx, cancel := s.withTimeout(ctx, worktreeTimeout)
	defer cancel()
	ok, err := s.reviewGit.has(gitCtx, cwd, base)
	if err != nil || ok {
		return err
	}
	if err := s.reviewGit.fetchBranch(gitCtx, cwd, repository, branch); err != nil {
		return fmt.Errorf("the review's base %s is not in the checkout, and fetching %s failed: %w", shortSHA(base), branch, err)
	}
	if ok, err := s.reviewGit.has(gitCtx, cwd, base); err != nil {
		return err
	} else if !ok {
		return fmt.Errorf("the review's base %s is not in %s's branch %s", shortSHA(base), repository, branch)
	}
	return nil
}
