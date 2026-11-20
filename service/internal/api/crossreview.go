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

func (g sandboxGit) fetchBranch(ctx context.Context, path, repository, branch string) error {
	_, err := g.s.engine.Git(ctx, path, append(slices.Clone(worktree.ReviewGitOptions), "fetch", "--no-tags", "--", g.s.repoURL(repository),
		"refs/heads/"+branch)...)
	return err
}

// reviewSoon runs a review pass on its own goroutine, after a report
// linked pull requests: a pass already running runs again after it.
func (s *Server) reviewSoon() {
	s.sessionMu.Lock()
	defer s.sessionMu.Unlock()
	if s.stopped {
		return
	}
	if s.reviewing {
		s.reviewAgain = true
		return
	}
	s.reviewing = true
	if !s.reviewAsync {
		// A test's pass runs in the call, so what it starts is there when
		// the call returns.
		s.sessionMu.Unlock()
		s.reviewPass(s.passCtx)
		s.sessionMu.Lock()
		s.reviewing = false
		return
	}
	go s.reviewLoop()
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
		s.passes.Add(1)
		s.sessionMu.Unlock()
		s.reviewPass(s.passCtx)
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

// reviewPass settles every review thread still open, whatever its pull
// request is now, then starts the reviews the open pull requests need, one
// pass at a time. What fails is logged and tried at the next round.
func (s *Server) reviewPass(ctx context.Context) {
	s.reviewMu.Lock()
	defer s.reviewMu.Unlock()
	for session := range s.abortsDue {
		s.stopReviewRun(ctx, session)
	}
	open, err := s.store.OpenReviewThreads(ctx)
	if err != nil {
		s.log.Warn("read the open reviews", "err", err)
		return
	}
	for _, t := range open {
		if err := s.settleReview(ctx, t); err != nil {
			s.log.Warn("cross-review", "thread", t.ID, "err", err)
		}
	}
	pulls, err := s.store.OpenPulls(ctx)
	if err != nil {
		s.log.Warn("read pull requests to review", "err", err)
		return
	}
	for _, p := range pulls {
		if err := s.startReview(ctx, p); err != nil {
			s.log.Warn("cross-review", "pull", p.Ref(), "err", err)
		}
	}
}

// settleReview moves an open review thread on: it ends when its pull
// request closed, merged or became a draft, or its head moved; one at work
// is followed, and one still starting is started again (Codex review of
// #26, round 1: a review never outlives its pull request's state).
func (s *Server) settleReview(ctx context.Context, t store.ThreadRow) error {
	p, err := s.store.PullByID(ctx, t.ReviewPull)
	if err != nil {
		return err
	}
	switch {
	case p.State != store.PullOpen:
		return s.endReview(ctx, t, "Ended: the pull request is "+p.State+".")
	case p.Draft:
		return s.endReview(ctx, t, "Ended: the pull request is a draft.")
	case t.Key != team.ReviewKey(p.Repository, p.Number, p.Base, p.HeadSHA):
		// Its head moved, or it was retargeted at the same head: its change
		// set is another (Codex review of #26, round 3).
		return s.endReview(ctx, t, "Superseded: the pull request is at "+shortSHA(p.HeadSHA)+" on "+p.Base+" now.")
	case t.State == store.ThreadOpen && t.Briefed:
		return s.followReview(ctx, t)
	}
	_, err = s.startThread(ctx, t)
	return err
}

// startReview starts the review open pull request p needs: none for a
// draft, a head that already has its review or a verdict, or a spent
// budget (ADR-0019).
func (s *Server) startReview(ctx context.Context, p store.PullRow) error {
	if p.Draft || !fullCommit.MatchString(p.HeadSHA) {
		return nil
	}
	key := team.ReviewKey(p.Repository, p.Number, p.Base, p.HeadSHA)
	if _, err := s.store.ThreadByKey(ctx, key); err == nil {
		return nil // settled above
	} else if !errors.Is(err, store.ErrNotFound) {
		return err
	}
	// A verdict at this head backs it already, filed before pocketd started
	// reviews: no second review of it (Codex review of #26, round 1).
	if _, found, err := s.store.LatestReview(ctx, p.ID, p.HeadSHA); err != nil || found {
		return err
	}
	threads, err := s.store.ReviewThreadsOf(ctx, p.ID)
	if err != nil {
		return err
	}
	if len(threads) >= team.MaxReviews {
		return nil // the budget is spent: the owner decides
	}
	created, ok, err := s.newReview(ctx, p, len(threads)+1, key)
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
		row, _, err := s.store.CreateReviewThread(ctx, store.ThreadRow{Key: key, Title: team.ReviewTitle(p.Repository, p.Number, p.HeadSHA, n),
			Agent: author.Agent, SupervisorSession: author.SupervisorSession, Repository: repository,
			Task: "Cross-review of " + p.Ref() + " at " + p.HeadSHA, ReviewPull: p.ID, ReviewHead: p.HeadSHA})
		return row, err
	})
	if errors.Is(err, store.ErrConflict) {
		return store.ThreadRow{}, false, nil // the pull request moved since this pass read it
	}
	return row, err == nil, err
}

// authorModel is the model of thread's session, which wrote its change:
// zero for a thread with no session.
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

// stopReviewRun stops a closed review's run, if one goes on: a stop that
// fails is tried again at each review pass (reviewPass holds reviewMu,
// which guards abortsDue).
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
	if err := team.CheckDestination(target, thread.Repository); err != nil {
		return SessionSpec{}, "", err
	}
	p, err := s.store.PullByID(ctx, thread.ReviewPull)
	if err != nil {
		return SessionSpec{}, "", err
	}
	checkout := target.Repositories[thread.Repository]
	cwd, _ := team.Worktree(checkout, target.ID, team.Slug(thread.Title, thread.ID, thread.Owner))
	gitCtx, cancel := s.withTimeout(ctx, worktreeTimeout)
	defer cancel()
	if err := s.reviewGit.add(gitCtx, checkout, cwd, thread.ReviewHead, p.Repository, p.Number); err != nil {
		return SessionSpec{}, "", fmt.Errorf("make the review's worktree: %w", err)
	}
	return SessionSpec{Title: "T" + strconv.FormatInt(thread.ID, 10) + " · " + thread.Title, Cwd: cwd, Model: choice.Model,
		Profile: &target, Route: string(choice.Rule), Thread: thread.ID, Review: true}, cwd, nil
}

// reviewChoice is the reviewer model of review thread thread, by the
// author's model (router.Choose with ReviewOf).
func (s *Server) reviewChoice(ctx context.Context, thread store.ThreadRow, target profiles.Profile, models []proto.Model) (router.Choice, error) {
	p, err := s.store.PullByID(ctx, thread.ReviewPull)
	if err != nil {
		return router.Choice{}, err
	}
	author, err := s.store.Thread(ctx, p.ThreadID)
	if err != nil {
		return router.Choice{}, err
	}
	model, err := s.authorModel(ctx, author)
	if err != nil {
		return router.Choice{}, err
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
	} else if found && last.HeadSHA != thread.ReviewHead {
		// A re-review reads from the last reviewed head only when the
		// worktree holds it; a force-push may have taken it, and then the
		// review is a full one (Codex review of #26, round 3).
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
		if !found || last.HeadSHA == thread.ReviewHead {
			return "", errors.New("no earlier review read another head: this is the first review")
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
