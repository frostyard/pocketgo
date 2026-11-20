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
		// when it is. So is a review thread that may not have closed on it
		// (ADR-0019).
		s.resyncReady(ctx, prior.PullID)
		if t, err := s.store.Thread(ctx, prior.ThreadID); err == nil && t.ReviewPull != 0 {
			if err := s.finishReview(ctx, t, prior); err != nil {
				s.log.Warn("close a review thread", "thread", t.ID, "err", err)
			}
			return s.reviewThreadText(ctx, prior)
		}
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
	// pocketd starts every cross-review, and nothing else does: only its
	// review threads file a verdict (ADR-0019).
	if thread.ReviewPull == 0 {
		return "", fmt.Errorf("thread T%d is no cross-review: pocketd starts every review of a linked pull request, and only its review threads file a verdict", thread.ID)
	}
	if err := team.CheckVerdict(v); err != nil {
		return "", err
	}
	// Each finding says its severity and whether it blocks, and agrees with
	// the verdict: the author acts on what blocks (ADR-0019).
	if err := team.CheckReviewFindings(v); err != nil {
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
	// A review thread pocketd started files its verdict on the pull
	// request and the head it reviews, and on no other (ADR-0019).
	if thread.ReviewPull != 0 && (pull.ID != thread.ReviewPull || v.Head != thread.ReviewHead) {
		return "", fmt.Errorf("thread T%d reviews %s at %s: file the verdict on that pull request and head", thread.ID, s.pullName(ctx, thread.ReviewPull), thread.ReviewHead)
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
	// A change of the pull request the verdict's read finds goes to the
	// review pass, which reads it again after the pull request's lock: the
	// review ends, and the next starts (Codex review of #26, round 10).
	moved := false
	defer func() {
		if moved {
			s.reviewAfterRead(pull.ID)
		}
	}()
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
		moved = true
		return "", fmt.Errorf("%s is %s on GitHub: only an open pull request is reviewed", pull.Ref(), read.State)
	}
	// A draft's review ends with it, as a review pass ends it: no verdict
	// is filed on one (Codex review of #26, round 2).
	if read.Draft {
		moved = true
		return "", fmt.Errorf("%s is a draft on GitHub: its review ends; marking it ready again does not review this head again, a push or the owner does", pull.Ref())
	}
	// The change set the reviewer read is the one GitHub has now: a pull
	// request retargeted, or whose base branch took part of its history,
	// has another at the same head (Codex review of #26, round 3).
	if thread.Key != team.ReviewKey(read.Repository, read.Number, read.Base, read.HeadSHA) {
		moved = true
		return "", fmt.Errorf("%s is at %s on %s on GitHub now, not the change set thread T%d reviews: its review ends", pull.Ref(), shortSHA(read.HeadSHA), read.Base, thread.ID)
	}
	// The compare has a deadline of its own, as readGitHub's read: the
	// pull request's lock is held (Codex review of #26, round 4).
	compareCtx, cancel := s.withTimeout(callCtx, pullTimeout)
	base, err := github.MergeBase(compareCtx, s.engine.GH, read.Repository, read.Base, read.HeadSHA)
	cancel()
	if err != nil {
		return "", err
	} else if base != thread.ReviewBase {
		moved = true
		return "", fmt.Errorf("%s leaves %s at %s now, not at %s, the base thread T%d read: its change set moved, and no verdict on it counts", pull.Ref(), read.Base, shortSHA(base), shortSHA(thread.ReviewBase), thread.ID)
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
	// The verdict ends its review: the review thread closes and the
	// author's thread gets the verdict in the transaction that records it,
	// so a close or a stop between them can lose neither (ADR-0019).
	summary := strings.TrimSpace(v.Summary)
	finish := &store.ReviewFinish{
		Report: func(id int64) string {
			return fmt.Sprintf("Verdict R%d: %s at %s is %s.", id, pull.Ref(), shortSHA(read.HeadSHA), v.Verdict)
		},
		Author: author.ID, MessageKey: "review-verdict:" + thread.Owner,
		MessageText: team.AuthorVerdictText(author.ID, pull.Ref(), read.HeadSHA, v.Verdict, summary, findings),
	}
	s.workMu.Lock()
	review, row, changed, replayed, err := s.store.AddReview(callCtx, read, store.ReviewRow{Key: key, PullID: pull.ID, ThreadID: thread.ID,
		SessionID: call.Session, Agent: thread.Agent, Provider: model.Provider, Model: model.ID, Family: reviewerFamily, HeadSHA: read.HeadSHA,
		Verdict: v.Verdict, Summary: summary, Findings: findings, BaseRef: read.Base, MergeBase: thread.ReviewBase}, finish)
	if err == nil && (changed || !replayed) {
		s.publishPull(row)
	}
	if err == nil && !replayed {
		if closed, terr := s.store.Thread(ctx, thread.ID); terr == nil && closed.State == store.ThreadClosed {
			s.publishThread(closed)
		}
	}
	s.workMu.Unlock()
	if errors.Is(err, store.ErrConflict) {
		return "", fmt.Errorf("thread T%d is closed and files no verdict", thread.ID)
	} else if err != nil {
		return "", err
	}
	// The pull request's merge base is the one just read, now its read is
	// recorded: the verdict backs it while that stays (Codex review of #26,
	// round 6).
	if thread.ReviewPull != 0 && fullCommit.MatchString(thread.ReviewBase) {
		if got, err := s.setMergeBase(ctx, row, thread.ReviewBase); err != nil {
			s.log.Warn("record a pull request's merge base", "pull", row.Ref(), "err", err)
		} else {
			row = got
		}
	}
	// The verdict is recorded whatever GitHub does with the label: a label
	// that cannot be written now is written at the next round.
	if _, err := s.syncReady(ctx, row); err != nil {
		s.log.Warn("keep ready-for-human", "pull", row.Ref(), "err", err)
	}
	// The author's message goes now, or with the next delivery pass.
	s.deliverLater()
	return team.ReviewThreadVerdictText(review.ID, row.Ref(), review.HeadSHA, review.Verdict), nil
}

// reviewThreadText answers a review thread's call that recorded r already.
func (s *Server) reviewThreadText(ctx context.Context, r store.ReviewRow) (string, error) {
	p, err := s.store.PullByID(ctx, r.PullID)
	if err != nil {
		return "", err
	}
	return team.ReviewThreadVerdictText(r.ID, p.Ref(), r.HeadSHA, r.Verdict), nil
}

// pullName is linked pull request id's owner/name#number, or its id when
// it cannot be read.
func (s *Server) pullName(ctx context.Context, id int64) string {
	if p, err := s.store.PullByID(ctx, id); err == nil {
		return p.Ref()
	}
	return fmt.Sprintf("pull request %d", id)
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
	review, found, err := s.store.LatestReview(ctx, row)
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
		// and a new head, base branch or merge base, or a draft, is
		// decided for itself. A merge base GitHub does not name now is
		// unknown, and the label comes off all the same (Codex review of
		// #26, round 7).
		fresh, err := s.readRecorded(ctx, row)
		var baseErr error
		if errors.Is(err, errNoMergeBase) {
			baseErr, err = err, nil
		}
		if err != nil {
			return row, errors.Join(ciErr, err)
		}
		switch {
		case fresh.State != store.PullOpen:
			return fresh, errors.Join(ciErr, baseErr)
		case fresh.HeadSHA != row.HeadSHA || fresh.Draft != row.Draft || fresh.Base != row.Base || fresh.MergeBase != row.MergeBase:
			if moved >= 2 {
				return fresh, errors.Join(ciErr, baseErr, fmt.Errorf("%s's head kept moving: the next round decides its label", fresh.Ref()))
			}
			out, err := s.syncReadyAt(ctx, fresh, moved+1)
			return out, errors.Join(ciErr, baseErr, err)
		}
		row = fresh
		if !row.Ready {
			return row, errors.Join(ciErr, baseErr)
		}
		if err := s.setReady(ctx, row, false); err != nil {
			return row, errors.Join(ciErr, baseErr, err)
		}
		row, err = s.recordGate(ctx, row, func() (store.PullRow, bool, error) { return s.store.SetPullReady(ctx, row.ID, row.HeadSHA, false) })
		return row, errors.Join(ciErr, baseErr, err)
	}
	// The verdict is of a head on a base branch and merge base: a retarget
	// or a base branch reset under the CI read or the write is decided for
	// itself (Codex review of #26, rounds 5 and 7). A verdict filed before
	// ADR-0019 names no merge base, and counts by its head.
	reviewed, base, mergeBase := row.HeadSHA, row.Base, ""
	if review.BaseRef != "" {
		mergeBase = review.MergeBase
	}
	fresh, same, err := s.readSame(ctx, row, reviewed, base, mergeBase)
	switch {
	case errors.Is(err, errNoMergeBase) && fresh.State == store.PullOpen && fresh.Ready:
		// A merge base GitHub does not name now backs nothing, and a label
		// someone put on meanwhile comes off (Codex review of #26, round
		// 10).
		if offErr := s.setReady(ctx, fresh, false); offErr != nil {
			return fresh, errors.Join(err, offErr)
		}
		out, offErr := s.recordGate(ctx, fresh, func() (store.PullRow, bool, error) { return s.store.SetPullReady(ctx, fresh.ID, fresh.HeadSHA, false) })
		return out, errors.Join(err, offErr)
	case err != nil:
		// A merge base GitHub does not name now backs nothing: no label.
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
	after, same, err := s.readSame(ctx, fresh, reviewed, base, mergeBase)
	if (err != nil && !errors.Is(err, errNoMergeBase)) || same || after.State != store.PullOpen {
		// A pull request merged or closed under the write keeps its labels.
		return after, err
	}
	// The head, base branch or merge base moved, or it became a draft,
	// under the write, or GitHub no longer names the merge base: the label
	// comes off again.
	if offErr := s.setReady(ctx, after, false); offErr != nil {
		return after, errors.Join(err, offErr)
	}
	out, offErr := s.recordGate(ctx, after, func() (store.PullRow, bool, error) { return s.store.SetPullReady(ctx, after.ID, after.HeadSHA, false) })
	return out, errors.Join(err, offErr)
}

// readSame reads row on GitHub again, with its merge base, records the
// read, and says whether the pull request is still open, no draft, at
// head, on base, and at mergeBase when it names one. A merge base GitHub
// does not name is an error that wraps errNoMergeBase, with the row read.
func (s *Server) readSame(ctx context.Context, row store.PullRow, head, base, mergeBase string) (store.PullRow, bool, error) {
	recorded, err := s.readRecorded(ctx, row)
	if err != nil {
		return recorded, false, err
	}
	return recorded, recorded.State == store.PullOpen && !recorded.Draft && recorded.HeadSHA == head && recorded.Base == base &&
		(mergeBase == "" || recorded.MergeBase == mergeBase), nil
}

// readRecorded reads row on GitHub again and records the read, with an
// open one's merge base (readMergeBase). When the pull request's read
// fails, it answers row.
func (s *Server) readRecorded(ctx context.Context, row store.PullRow) (store.PullRow, error) {
	read, err := s.readPull(ctx, row.Repository, row.Number)
	if err != nil {
		return row, err
	}
	recorded, err := s.recordPull(ctx, row.ID, read)
	if err != nil {
		return recorded, err
	}
	return s.readMergeBase(ctx, recorded)
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
		// Only a review thread pocketd started has team_verdict; a session
		// that had it before ADR-0019 loses it (Codex review of #26,
		// round 1).
		review := t.ReviewPull != 0
		if changed, err := s.engine.SetVerdictTool(ctx, t.Session, review); err != nil {
			failures = append(failures, fmt.Errorf("thread T%d: %w", t.ID, err))
		} else if changed {
			s.log.Info("set a thread's team_verdict", "thread", t.ID, "session", t.Session, "on", review)
		}
	}
	return errors.Join(failures...)
}
