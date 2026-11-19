package api

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/frostyard/pocketgo/proto"
	"github.com/frostyard/pocketgo/service/internal/github"
	"github.com/frostyard/pocketgo/service/internal/router"
	"github.com/frostyard/pocketgo/service/internal/store"
	"github.com/frostyard/pocketgo/service/internal/team"
)

// TeamVerdict is the team_verdict tool: a cross-review verdict on a linked
// pull request (ADR-0017). Code checks the reviewer and the head before it
// records the verdict, then brings the pull request's ready-for-human in
// step with it. A call run again answers as it did.
func (s *Server) TeamVerdict(ctx context.Context, call TeamCall, v team.Verdict) (string, error) {
	// The call's own context bounds the GitHub read before the verdict is
	// recorded: an abort stops it, and records nothing.
	callCtx := ctx
	ctx = context.WithoutCancel(ctx)
	key := call.key()
	if prior, found, err := s.store.ReviewCall(ctx, key); err != nil {
		return "", err
	} else if found {
		return s.verdictText(ctx, prior)
	}
	thread, err := s.store.ThreadOfSession(ctx, call.Session)
	if errors.Is(err, store.ErrNotFound) {
		return "", errors.New("team_verdict is for a thread's specialist: a cross-review is a thread the supervisor delegates")
	} else if err != nil {
		return "", err
	}
	if thread.State == store.ThreadClosed {
		return "", fmt.Errorf("thread T%d is closed and files no verdict", thread.ID)
	}
	if err := team.CheckVerdict(v); err != nil {
		return "", err
	}
	repo, err := team.PullRepository(v.PullRequest, thread.Repository)
	if err != nil {
		return "", err
	}
	if err := github.CheckRepository(repo); err != nil {
		return "", err
	}
	ref := fmt.Sprintf("%s#%d", repo, v.PullRequest.Number)
	pull, found, err := s.store.PullByRef(ctx, repo, v.PullRequest.Number)
	if err != nil {
		return "", err
	} else if !found {
		return "", fmt.Errorf("%s is linked to no thread: its author reports it with team_message first", ref)
	}
	author, err := s.store.Thread(ctx, pull.ThreadID)
	if err != nil {
		return "", err
	}
	authorFamily, err := s.sessionFamily(ctx, author.Session)
	if err != nil {
		return "", err
	}
	reviewer, err := s.store.Session(ctx, call.Session)
	if err != nil {
		return "", err
	}
	model := proto.ModelRef{Provider: reviewer.Provider, ID: reviewer.Model}
	reviewerFamily := router.Family(model)
	if err := team.CheckReviewer(author.ID, thread.ID, authorFamily, reviewerFamily); err != nil {
		return "", err
	}
	unlock, err := s.lockPulls(callCtx, []string{pull.Ref()})
	if err != nil {
		return "", fmt.Errorf("the verdict was stopped: %w", err)
	}
	defer unlock()
	read, gp, err := s.readGitHub(callCtx, pull.Repository, pull.Number)
	if err != nil {
		return "", err
	}
	if gp.ID != pull.GitHubID {
		return "", fmt.Errorf("%w: %s", store.ErrPullReplaced, pull.Ref())
	}
	if read.State != store.PullOpen {
		return "", fmt.Errorf("%s is %s on GitHub: only an open pull request is reviewed", pull.Ref(), read.State)
	}
	if err := team.CheckHead(pull.Ref(), v.Head, read.HeadSHA); err != nil {
		return "", err
	}
	findings := make([]string, len(v.Findings))
	for i, f := range v.Findings {
		findings[i] = strings.TrimSpace(f)
	}
	s.workMu.Lock()
	review, row, changed, replayed, err := s.store.AddReview(ctx, read, store.ReviewRow{Key: key, PullID: pull.ID, ThreadID: thread.ID,
		SessionID: call.Session, Agent: thread.Agent, Provider: model.Provider, Model: model.ID, Family: reviewerFamily, HeadSHA: read.HeadSHA,
		Verdict: v.Verdict, Summary: strings.TrimSpace(v.Summary), Findings: findings})
	if err == nil && (changed || !replayed) {
		s.publishPull(row)
	}
	s.workMu.Unlock()
	if err != nil {
		return "", err
	}
	// The verdict is recorded whatever GitHub does with the label: a label
	// that cannot be written now is written at the next round.
	if _, err := s.syncReady(ctx, row); err != nil {
		s.log.Warn("keep ready-for-human", "pull", row.Ref(), "err", err)
	}
	return team.VerdictText(review.ID, row.Ref(), review.HeadSHA, review.Verdict), nil
}

// verdictText answers a call that recorded review r already.
func (s *Server) verdictText(ctx context.Context, r store.ReviewRow) (string, error) {
	p, err := s.store.PullByID(ctx, r.PullID)
	if err != nil {
		return "", err
	}
	return team.VerdictText(r.ID, p.Ref(), r.HeadSHA, r.Verdict), nil
}

// sessionFamily is the model family of session id, "" for a thread whose
// session has not started or a model router.Family cannot tell.
func (s *Server) sessionFamily(ctx context.Context, id int64) (string, error) {
	if id == 0 {
		return "", nil
	}
	row, err := s.store.Session(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return "", nil
	} else if err != nil {
		return "", err
	}
	return router.Family(proto.ModelRef{Provider: row.Provider, ID: row.Model}), nil
}

// syncReady brings row's ready-for-human in step with its verdicts
// (ADR-0017): an open pull request that is no draft has the label exactly
// when the newest verdict at its head is ready and CI there is green. It
// reads CI only for a ready verdict, writes the label only when it must
// change, and records and publishes what it made of it. A closed or merged
// pull request keeps its labels. The caller holds row (lockPulls).
func (s *Server) syncReady(ctx context.Context, row store.PullRow) (store.PullRow, error) {
	if row.State != store.PullOpen {
		return row, nil
	}
	review, found, err := s.store.LatestReview(ctx, row.ID, row.HeadSHA)
	if err != nil {
		return row, err
	}
	verdict, checks := "", ""
	if found {
		verdict = review.Verdict
	}
	if verdict == team.VerdictReady && !row.Draft {
		read, cancel := s.withTimeout(ctx, pullTimeout)
		checks, err = github.CI(read, s.engine.GH, row.Repository, row.HeadSHA)
		cancel()
		if err != nil {
			return row, err
		}
	}
	want := team.WantsReady(true, row.Draft, verdict, checks == github.CIGreen)
	if want != row.Ready {
		write, cancel := s.withTimeout(ctx, pullTimeout)
		err := github.SetLabel(write, s.engine.GH, row.Repository, row.Number, team.ReadyLabel, want)
		cancel()
		if err != nil {
			return row, err
		}
	}
	s.workMu.Lock()
	defer s.workMu.Unlock()
	out, changed, err := s.store.SetPullGate(ctx, row.ID, row.HeadSHA, want, checks)
	if err == nil && changed {
		s.publishPull(out)
	}
	return out, err
}
