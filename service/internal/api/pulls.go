package api

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/frostyard/pocketgo/proto"
	"github.com/frostyard/pocketgo/service/internal/github"
	"github.com/frostyard/pocketgo/service/internal/store"
	"github.com/frostyard/pocketgo/service/internal/team"
)

// pullTimeout bounds one read of a pull request on GitHub.
const pullTimeout = 30 * time.Second

// checkPulls reads each pull request a report names on GitHub and checks
// it belongs to thread (team.CheckPull), before the report is filed
// (ADR-0016). The first that fails refuses the report, with why.
func (s *Server) checkPulls(ctx context.Context, thread store.ThreadRow, refs []team.PullRef) ([]store.PullRow, []team.PullFacts, error) {
	if len(refs) == 0 {
		return nil, nil, nil
	}
	out := make([]store.PullRow, 0, len(refs))
	facts := make([]team.PullFacts, 0, len(refs))
	for _, ref := range refs {
		repo, err := team.PullRepository(ref, thread.Repository)
		if err != nil {
			return nil, nil, err
		}
		row, p, err := s.readGitHub(ctx, repo, ref.Number)
		if err != nil {
			return nil, nil, err
		}
		fact := team.PullFacts{Repository: p.Repository, Number: p.Number, Head: p.Head, HeadRepository: p.HeadRepository}
		if err := team.CheckPull(thread.Repository, thread.Branch, s.profileRepositories(thread.Agent), fact); err != nil {
			return nil, nil, err
		}
		facts = append(facts, fact)
		out = append(out, row)
	}
	return out, facts, nil
}

// recheckPulls checks facts against thread's profile as it is now: the
// caller holds workMu, right before the report is filed, so a repository
// taken off the profile while the reads ran, or while the report waited,
// takes no pull request.
func (s *Server) recheckPulls(thread store.ThreadRow, facts []team.PullFacts) error {
	repositories := s.profileRepositories(thread.Agent)
	for _, fact := range facts {
		if err := team.CheckPull(thread.Repository, thread.Branch, repositories, fact); err != nil {
			return err
		}
	}
	return nil
}

// profileRepositories are the repositories agent's profile names now, none
// when it cannot be read.
func (s *Server) profileRepositories(agent string) []string {
	var out []string
	if s.profiles != nil {
		if p, err := s.profiles.Find(agent); err == nil {
			for repo := range p.Repositories {
				out = append(out, repo)
			}
		}
	}
	return out
}

// lockPulls holds each pull request of refs, in one order, until the
// returned function runs (Server.pullLocks). A wait ends with ctx, and
// lets go of the ones it held.
func (s *Server) lockPulls(ctx context.Context, refs []string) (unlock func(), err error) {
	keys := make([]string, 0, len(refs))
	for _, ref := range refs {
		keys = append(keys, strings.ToLower(ref))
	}
	slices.Sort(keys)
	keys = slices.Compact(keys)
	unlocks := make([]func(), 0, len(keys))
	all := func() {
		for i := len(unlocks) - 1; i >= 0; i-- {
			unlocks[i]()
		}
	}
	for _, key := range keys {
		unlock, err := s.pullLocks.lock(ctx, key)
		if err != nil {
			all()
			return nil, err
		}
		unlocks = append(unlocks, unlock)
	}
	return all, nil
}

// ctxLocks holds keys, one holder each, with waits that end with their
// context.
type ctxLocks struct {
	mu   sync.Mutex
	held map[string]*ctxLock
}

type ctxLock struct {
	ch    chan struct{}
	users int
}

// lock holds key until the returned function runs, or fails when ctx ends
// first.
func (k *ctxLocks) lock(ctx context.Context, key string) (func(), error) {
	k.mu.Lock()
	if k.held == nil {
		k.held = map[string]*ctxLock{}
	}
	l := k.held[key]
	if l == nil {
		l = &ctxLock{ch: make(chan struct{}, 1)}
		k.held[key] = l
	}
	l.users++
	k.mu.Unlock()
	select {
	case l.ch <- struct{}{}:
		return func() {
			<-l.ch
			k.release(key, l)
		}, nil
	case <-ctx.Done():
		k.release(key, l)
		return nil, ctx.Err()
	}
}

func (k *ctxLocks) release(key string, l *ctxLock) {
	k.mu.Lock()
	defer k.mu.Unlock()
	l.users--
	if l.users == 0 {
		delete(k.held, key)
	}
}

// readGitHub reads one pull request on GitHub, bound by pullTimeout, as
// a row to record: in the order the store gives its read before it
// begins, so a row keeps the newest read whatever the clock does
// (store.UpdatePull), and with when it began, for display.
func (s *Server) readGitHub(ctx context.Context, repository string, number int64) (store.PullRow, github.Pull, error) {
	seq, err := s.store.NextPullRead(ctx)
	if err != nil {
		return store.PullRow{}, github.Pull{}, err
	}
	seen := s.now()
	read, cancel := s.withTimeout(ctx, pullTimeout)
	defer cancel()
	p, err := github.Read(read, s.engine.GH, repository, number)
	if err != nil {
		return store.PullRow{}, github.Pull{}, err
	}
	return store.PullRow{GitHubID: p.ID, ReadSeq: seq, Repository: p.Repository, Number: p.Number, Title: p.Title, URL: p.URL,
		State: p.State, Draft: p.Draft, Head: p.Head, HeadSHA: p.HeadSHA, Base: p.Base, Ready: slices.Contains(p.Labels, team.ReadyLabel),
		CheckedAt: seen}, p, nil
}

// readPull is readGitHub's row.
func (s *Server) readPull(ctx context.Context, repository string, number int64) (store.PullRow, error) {
	row, _, err := s.readGitHub(ctx, repository, number)
	return row, err
}

// pullNames are pulls as a report names them: repository#number (state).
func pullNames(pulls []store.PullRow) []string {
	out := make([]string, len(pulls))
	for i, p := range pulls {
		out[i] = p.Ref() + " (" + p.State + ")"
	}
	return out
}

// refreshPull reads p on GitHub again and records it, then keeps an open
// one's ready-for-human in step with its verdicts (syncReady), holding p
// from the read to the last record (lockPulls).
func (s *Server) refreshPull(ctx context.Context, p store.PullRow) (store.PullRow, error) {
	unlock, err := s.lockPulls(ctx, []string{p.Ref()})
	if err != nil {
		return store.PullRow{}, err
	}
	defer unlock()
	read, err := s.readPull(ctx, p.Repository, p.Number)
	if err != nil {
		return store.PullRow{}, err
	}
	row, err := s.recordPull(ctx, p.ID, read)
	if err != nil {
		return row, err
	}
	return s.syncReady(ctx, row)
}

// recordPull records what GitHub said of pull request id, and publishes
// it when it changed, under workMu.
func (s *Server) recordPull(ctx context.Context, id int64, p store.PullRow) (store.PullRow, error) {
	s.workMu.Lock()
	defer s.workMu.Unlock()
	row, changed, err := s.store.UpdatePull(ctx, id, p)
	if err == nil && changed {
		s.publishPull(row)
	}
	return row, err
}

// closeIfMerged closes thread once its pull requests merged (ADR-0016),
// and publishes it. A closed one may have reopened since it was read: each
// is read again first, and one open now keeps the thread. One that cannot
// be read keeps it too, until the next round.
func (s *Server) closeIfMerged(ctx context.Context, thread int64) error {
	pulls, err := s.store.PullsOfThread(ctx, thread)
	if err != nil {
		return err
	}
	merged := false
	for _, p := range pulls {
		switch p.State {
		case store.PullOpen:
			return nil
		case store.PullMerged:
			merged = true
		}
	}
	if !merged {
		return nil
	}
	rechecked := map[int64]bool{}
	for _, p := range pulls {
		if p.State != store.PullClosed {
			continue
		}
		row, err := s.refreshPull(ctx, p)
		if err != nil {
			return err
		}
		if row.State == store.PullOpen {
			return nil
		}
		rechecked[p.ID] = true
	}
	s.workMu.Lock()
	defer s.workMu.Unlock()
	// The close holds only for the pull requests just checked: one closed
	// that a report linked meanwhile waits for the next round.
	now, err := s.store.PullsOfThread(ctx, thread)
	if err != nil {
		return err
	}
	for _, p := range now {
		if p.State == store.PullClosed && !rechecked[p.ID] {
			return nil
		}
	}
	row, closed, err := s.store.CloseMergedThread(ctx, thread, func(merged []store.PullRow) string {
		refs := make([]string, len(merged))
		for i, p := range merged {
			refs[i] = p.Ref()
		}
		return team.MergedText(refs)
	})
	if err == nil && closed {
		s.publishThread(row)
	}
	return err
}

// RefreshPulls reads each pull request a round reads again on GitHub
// (store.PullsToRead: the open ones, and the closed ones of threads not
// closed), records and publishes what changed, then closes each thread whose pull requests
// merged (ADR-0016), found from the work database. One that cannot be
// read keeps its last state; done is false then.
func (s *Server) RefreshPulls(ctx context.Context) (done bool, err error) {
	toRead, err := s.store.PullsToRead(ctx)
	if err != nil {
		return false, err
	}
	var failures []error
	for _, p := range toRead {
		if _, err := s.refreshPull(ctx, p); err != nil {
			failures = append(failures, err)
		}
	}
	threads, err := s.store.ThreadsMergedOut(ctx)
	if err != nil {
		failures = append(failures, err)
	}
	for _, id := range threads {
		if err := s.closeIfMerged(ctx, id); err != nil {
			failures = append(failures, err)
		}
	}
	return len(failures) == 0, errors.Join(failures...)
}

// StartPulls reads the open pull requests again every interval, in rounds
// that Stop ends; 0 starts none. The service calls it once, after
// StartDelivery.
func (s *Server) StartPulls(every time.Duration) {
	if every <= 0 {
		return
	}
	s.sessionMu.Lock()
	defer s.sessionMu.Unlock()
	s.pullEvery = every
	s.schedulePullsLocked()
}

// schedulePullsLocked schedules the next round; sessionMu is held.
func (s *Server) schedulePullsLocked() {
	if s.stopped {
		return
	}
	s.pullStop = s.pullTimer(s.pullEvery, s.pullRound)
}

// pullRound is one round of RefreshPulls, then the next is scheduled.
func (s *Server) pullRound() {
	s.sessionMu.Lock()
	if s.stopped {
		s.sessionMu.Unlock()
		return
	}
	s.passes.Add(1)
	s.sessionMu.Unlock()
	defer s.passes.Done()
	if _, err := s.RefreshPulls(s.passCtx); err != nil {
		s.log.Warn("read pull requests", "err", err)
	}
	s.sessionMu.Lock()
	defer s.sessionMu.Unlock()
	s.schedulePullsLocked()
}

func (s *Server) publishPull(row store.PullRow) {
	pull, err := s.wirePull(context.Background(), row)
	if err != nil {
		// The pull request goes out without its verdict rather than not at
		// all; the next fetch brings the verdict.
		s.log.Warn("read a pull request's review", "pull", row.ID, "err", err)
	}
	s.emit(proto.Event{Type: proto.EventPull, Pull: &pull})
}

// wirePull is row as the protocol has it, with the newest verdict at its
// head (ADR-0017).
func (s *Server) wirePull(ctx context.Context, row store.PullRow) (proto.PullRequest, error) {
	out := proto.PullRequest{ID: row.ID, Thread: row.ThreadID, Repository: row.Repository, Number: row.Number, Title: row.Title,
		URL: row.URL, State: proto.PullState(row.State), Draft: row.Draft, Head: row.Head, HeadSHA: row.HeadSHA, Base: row.Base,
		Ready: row.Ready, Checks: proto.CheckState(row.Checks), CheckedAt: row.CheckedAt, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt}
	review, found, err := s.store.LatestReview(ctx, row.ID, row.HeadSHA)
	if err != nil || !found {
		return out, err
	}
	out.Review = &proto.PullReview{ID: review.ID, Thread: review.ThreadID, Agent: review.Agent,
		Model: proto.ModelRef{Provider: review.Provider, ID: review.Model}, Family: review.Family, Head: review.HeadSHA,
		Verdict: proto.ReviewVerdict(review.Verdict), Summary: review.Summary, Findings: review.Findings, CreatedAt: review.CreatedAt}
	return out, nil
}

func (s *Server) listPulls(w http.ResponseWriter, r *http.Request) {
	rows, err := s.store.ListPulls(r.Context())
	if err != nil {
		s.internal(w, "list pull requests", err)
		return
	}
	out := make([]proto.PullRequest, 0, len(rows))
	for _, row := range rows {
		pull, err := s.wirePull(r.Context(), row)
		if err != nil {
			s.internal(w, "list pull requests", err)
			return
		}
		out = append(out, pull)
	}
	writeJSON(w, http.StatusOK, out)
}
