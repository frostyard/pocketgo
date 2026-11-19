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
		// The call ran before, and may have stopped before the label was
		// in step: the label is brought in step now, which writes nothing
		// when it is.
		s.resyncReady(ctx, prior.PullID)
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
	repo, err := team.VerdictRepository(v.PullRequest, thread.Repository)
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
	// The filing takes the call's own context: an abort before it commits
	// files nothing. Only the label owed after it is done whatever the call
	// does.
	s.workMu.Lock()
	review, row, changed, replayed, err := s.store.AddReview(callCtx, read, store.ReviewRow{Key: key, PullID: pull.ID, ThreadID: thread.ID,
		SessionID: call.Session, Agent: thread.Agent, Provider: model.Provider, Model: model.ID, Family: reviewerFamily, HeadSHA: read.HeadSHA,
		Verdict: v.Verdict, Summary: strings.TrimSpace(v.Summary), Findings: findings})
	if err == nil && (changed || !replayed) {
		s.publishPull(row)
	}
	s.workMu.Unlock()
	if errors.Is(err, store.ErrConflict) {
		return "", fmt.Errorf("thread T%d is closed and files no verdict", thread.ID)
	} else if err != nil {
		return "", err
	}
	// The verdict is recorded whatever GitHub does with the label: a label
	// that cannot be written now is written at the next round.
	if _, err := s.syncReady(ctx, row); err != nil {
		s.log.Warn("keep ready-for-human", "pull", row.Ref(), "err", err)
	}
	return team.VerdictText(review.ID, row.Ref(), review.HeadSHA, review.Verdict), nil
}

// resyncReady brings pull request id's label in step with its verdicts
// as refreshPull does: it reads the pull request on GitHub first, since
// its head may have moved while pocketd was stopped. What fails is logged
// and left to the next round.
func (s *Server) resyncReady(ctx context.Context, id int64) {
	p, err := s.store.PullByID(ctx, id)
	if err == nil {
		_, err = s.refreshPull(ctx, p)
	}
	if err != nil {
		s.log.Warn("keep ready-for-human", "pull", id, "err", err)
	}
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
// reads CI only for a ready verdict; CI it cannot read is not green, so
// the label comes off. It writes the label only when it must change: it
// goes on only after a read finds the reviewed head, open and no draft,
// and a read after the write finds it still, or it comes off again, since
// GitHub labels a number, not a commit. CI's state and the label are
// recorded apart, each as pocketd last saw it, and published. A closed or
// merged pull request keeps its labels. The caller holds row (lockPulls).
func (s *Server) syncReady(ctx context.Context, row store.PullRow) (store.PullRow, error) {
	return s.syncReadyAt(ctx, row, 0)
}

// syncReadyAt is syncReady; moved counts the heads it found moved under
// it, so a head that keeps moving ends it, for the next round.
func (s *Server) syncReadyAt(ctx context.Context, row store.PullRow, moved int) (store.PullRow, error) {
	if row.State != store.PullOpen {
		return row, nil
	}
	review, found, err := s.store.LatestReview(ctx, row.ID, row.HeadSHA)
	if err != nil {
		return row, err
	}
	verdict := ""
	if found {
		verdict = review.Verdict
	}
	green := false
	var ciErr error
	if verdict == team.VerdictReady && !row.Draft {
		read, cancel := s.withTimeout(ctx, pullTimeout)
		checks, err := github.CI(read, s.engine.GH, row.Repository, row.HeadSHA)
		cancel()
		if err != nil {
			ciErr = err
		} else {
			green = checks == github.CIGreen
			if row, err = s.recordGate(ctx, row, func() (store.PullRow, bool, error) {
				return s.store.SetPullChecks(ctx, row.ID, row.HeadSHA, checks)
			}); err != nil {
				return row, err
			}
		}
	}
	want := team.WantsReady(true, row.Draft, verdict, green)
	if want == row.Ready {
		return row, ciErr
	}
	if !want {
		// The pull request may have merged, closed or moved while CI was
		// read: a read first, so a merged or closed one keeps its label,
		// and a new head or a draft is decided for itself.
		fresh, err := s.readRecorded(ctx, row)
		if err != nil {
			return row, errors.Join(ciErr, err)
		}
		switch {
		case fresh.State != store.PullOpen:
			return fresh, ciErr
		case fresh.HeadSHA != row.HeadSHA || fresh.Draft != row.Draft:
			if moved >= 2 {
				return fresh, errors.Join(ciErr, fmt.Errorf("%s's head kept moving: the next round decides its label", fresh.Ref()))
			}
			out, err := s.syncReadyAt(ctx, fresh, moved+1)
			return out, errors.Join(ciErr, err)
		}
		row = fresh
		if !row.Ready {
			return row, ciErr
		}
		if err := s.setReady(ctx, row, false); err != nil {
			return row, errors.Join(ciErr, err)
		}
		row, err = s.recordGate(ctx, row, func() (store.PullRow, bool, error) { return s.store.SetPullReady(ctx, row.ID, row.HeadSHA, false) })
		return row, errors.Join(ciErr, err)
	}
	reviewed := row.HeadSHA
	fresh, same, err := s.readSame(ctx, row, reviewed)
	switch {
	case err != nil:
		return fresh, err
	case !same && fresh.State == store.PullOpen:
		// The head moved, or it became a draft, while CI was read: the
		// label is decided for what it is now.
		if moved >= 2 {
			return fresh, fmt.Errorf("%s's head kept moving: the next round decides its label", fresh.Ref())
		}
		return s.syncReadyAt(ctx, fresh, moved+1)
	case !same || fresh.Ready:
		// Merged or closed meanwhile, it keeps its labels; or someone put
		// the label on at the reviewed head: it is in step.
		return fresh, nil
	}
	if err := s.setReady(ctx, fresh, true); err != nil {
		return fresh, err
	}
	// GitHub confirmed the label: it is recorded before the read that
	// checks the head, which may fail and leave the rest to the next round.
	if fresh, err = s.recordGate(ctx, fresh, func() (store.PullRow, bool, error) {
		return s.store.SetPullReady(ctx, fresh.ID, fresh.HeadSHA, true)
	}); err != nil {
		return fresh, err
	}
	after, same, err := s.readSame(ctx, fresh, reviewed)
	if err != nil || same || after.State != store.PullOpen {
		// A pull request merged or closed under the write keeps its labels.
		return after, err
	}
	// The head moved, or it became a draft, under the write: the label
	// comes off again.
	if err := s.setReady(ctx, after, false); err != nil {
		return after, err
	}
	return s.recordGate(ctx, after, func() (store.PullRow, bool, error) { return s.store.SetPullReady(ctx, after.ID, after.HeadSHA, false) })
}

// readSame reads row on GitHub again, records the read, and says whether
// the pull request is still open, no draft, at head.
func (s *Server) readSame(ctx context.Context, row store.PullRow, head string) (store.PullRow, bool, error) {
	recorded, err := s.readRecorded(ctx, row)
	if err != nil {
		return row, false, err
	}
	return recorded, recorded.State == store.PullOpen && !recorded.Draft && recorded.HeadSHA == head, nil
}

// readRecorded reads row on GitHub again and records the read.
func (s *Server) readRecorded(ctx context.Context, row store.PullRow) (store.PullRow, error) {
	read, err := s.readPull(ctx, row.Repository, row.Number)
	if err != nil {
		return row, err
	}
	return s.recordPull(ctx, row.ID, read)
}

// setReady puts ready-for-human on row, or takes it off, on GitHub.
func (s *Server) setReady(ctx context.Context, row store.PullRow, on bool) error {
	write, cancel := s.withTimeout(ctx, pullTimeout)
	defer cancel()
	return github.SetLabel(write, s.engine.GH, row.Repository, row.Number, team.ReadyLabel, on)
}

// recordGate records one observation of row's gate under workMu, and
// publishes the row when it changed.
func (s *Server) recordGate(_ context.Context, row store.PullRow, set func() (store.PullRow, bool, error)) (store.PullRow, error) {
	s.workMu.Lock()
	defer s.workMu.Unlock()
	out, changed, err := set()
	if err != nil {
		return row, err
	}
	if changed {
		s.publishPull(out)
	}
	return out, nil
}

// UpgradeTeamTools gives team_verdict to the specialist session of each
// thread not closed that was started before the tool existed (ADR-0017,
// protocol rule 25). The service calls it once, after engine.Resume; a
// session it cannot change is logged, and keeps its tools.
func (s *Server) UpgradeTeamTools(ctx context.Context) error {
	threads, err := s.store.ThreadsOnDesk(ctx)
	if err != nil {
		return err
	}
	var failures []error
	for _, t := range threads {
		if t.Session == 0 {
			continue
		}
		if added, err := s.engine.AddVerdictTool(ctx, t.Session); err != nil {
			failures = append(failures, fmt.Errorf("thread T%d: %w", t.ID, err))
		} else if added {
			s.log.Info("gave team_verdict to a thread's session", "thread", t.ID, "session", t.Session)
		}
	}
	return errors.Join(failures...)
}
