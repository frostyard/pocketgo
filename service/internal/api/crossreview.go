package api

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/frostyard/pocketgo/proto"
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
	base(ctx context.Context, cwd, repository, branch, head string) (string, error)
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

func (g sandboxGit) base(ctx context.Context, cwd, repository, branch, head string) (string, error) {
	// FETCH_HEAD is the worktree's own: the checkout gets no new ref.
	if _, err := g.s.engine.Git(ctx, cwd, "fetch", "--no-tags", "--", g.s.repoURL(repository), "refs/heads/"+branch); err != nil {
		return "", fmt.Errorf("fetch the base branch %s: %w", branch, err)
	}
	out, err := g.s.engine.Git(ctx, cwd, "merge-base", head, "FETCH_HEAD")
	if err != nil {
		return "", fmt.Errorf("find where %s leaves %s: %w", shortSHA(head), branch, err)
	}
	return strings.TrimSpace(out), nil
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

// reviewPass starts, follows and ends the reviews of every open linked
// pull request, one pass at a time. What fails is logged and tried at the
// next round.
func (s *Server) reviewPass(ctx context.Context) {
	s.reviewMu.Lock()
	defer s.reviewMu.Unlock()
	pulls, err := s.store.OpenPulls(ctx)
	if err != nil {
		s.log.Warn("read pull requests to review", "err", err)
		return
	}
	for _, p := range pulls {
		if err := s.reviewPull(ctx, p); err != nil {
			s.log.Warn("cross-review", "pull", p.Ref(), "err", err)
		}
	}
}

// reviewPull settles pull request p's reviews: a review of an older head
// ends, superseded; the review at its head starts, or, once its session
// ended with no verdict, ends; a new one starts while the budget lasts.
func (s *Server) reviewPull(ctx context.Context, p store.PullRow) error {
	threads, err := s.store.ReviewThreadsOf(ctx, p.ID)
	if err != nil {
		return err
	}
	key := team.ReviewKey(p.Repository, p.Number, p.HeadSHA)
	var current *store.ThreadRow
	for i := range threads {
		t := threads[i]
		if t.Key == key {
			current = &threads[i]
			continue
		}
		if t.State != store.ThreadClosed {
			if err := s.endReview(ctx, t, "Superseded: the pull request's head moved to "+shortSHA(p.HeadSHA)+"."); err != nil {
				return err
			}
		}
	}
	if p.State != store.PullOpen || p.Draft || !fullCommit.MatchString(p.HeadSHA) {
		return nil
	}
	if current == nil {
		if len(threads) >= team.MaxReviews {
			return nil // the budget is spent: the owner decides
		}
		created, ok, err := s.newReview(ctx, p, len(threads)+1, key)
		if err != nil || !ok {
			return err
		}
		current = &created
	}
	switch {
	case current.State == store.ThreadClosed:
		return nil
	case current.State == store.ThreadOpen && current.Briefed:
		return s.followReview(ctx, *current)
	}
	_, err = s.startThread(ctx, *current)
	return err
}

// newReview records the n-th review thread of p at its head, under teamMu
// as a delegation is, so two never both take the desk's last place. ok is
// false when none starts now: an author with no session or of no known
// family, or a full desk (the next round tries again).
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
		row, _, err := s.store.CreateThread(ctx, store.ThreadRow{Key: key, Title: team.ReviewTitle(p.Repository, p.Number, p.HeadSHA, n),
			Agent: author.Agent, SupervisorSession: author.SupervisorSession, Repository: p.Repository,
			Task: "Cross-review of " + p.Ref() + " at " + p.HeadSHA, ReviewPull: p.ID, ReviewHead: p.HeadSHA})
		return row, err
	})
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
	if t.Session != 0 {
		if busy, err := s.engine.Busy(ctx, t.Session); err != nil {
			return err
		} else if busy {
			if err := s.engine.Abort(ctx, t.Session); err != nil {
				return fmt.Errorf("stop the review of T%d: %w", t.ID, err)
			}
		}
	}
	s.workMu.Lock()
	defer s.workMu.Unlock()
	closed, changed, err := s.store.CloseReview(ctx, t.ID, report, nil)
	if err == nil && changed {
		s.publishThread(closed)
	}
	return err
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
	if err := team.CheckTarget(target, nil); err != nil {
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
	gitCtx, cancel := s.withTimeout(ctx, worktreeTimeout)
	defer cancel()
	base, err := s.reviewGit.base(gitCtx, cwd, p.Repository, p.Base, thread.ReviewHead)
	if err != nil {
		return "", err
	}
	if !fullCommit.MatchString(base) {
		return "", fmt.Errorf("git named the merge base %q, which is no commit id", base)
	}
	spec := team.ReviewSpec{Thread: thread.ID, Author: s.agentName(author.Agent), Repository: p.Repository, Number: p.Number,
		BaseBranch: p.Base, Base: base, Head: thread.ReviewHead, Cwd: cwd}
	if last, found, err := s.store.LastReviewOf(ctx, p.ID); err != nil {
		return "", err
	} else if found && last.HeadSHA != thread.ReviewHead {
		spec.Prior = &team.PriorReview{Head: last.HeadSHA, Verdict: last.Verdict, Summary: last.Summary, Findings: last.Findings}
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
