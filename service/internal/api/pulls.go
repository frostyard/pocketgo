package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
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
	// onWait, in tests, sees each lock call that waits for another
	// holder of its key.
	onWait func(key string)
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
	waits := l.users > 1 && k.onWait != nil
	k.mu.Unlock()
	if waits {
		k.onWait(key)
	}
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
		State: p.State, Draft: p.Draft, Head: p.Head, HeadSHA: p.HeadSHA, Base: p.Base, Ready: slices.ContainsFunc(p.Labels, isReadyLabel),
		CheckedAt: seen}, p, nil
}

// isReadyLabel says whether name is ReadyLabel: GitHub's label names
// ignore case.
func isReadyLabel(name string) bool { return strings.EqualFold(name, team.ReadyLabel) }

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
	// Its merge base is read with it: a verdict backs it only on the one
	// it was filed on (ADR-0019; Codex review of #26, round 6). One GitHub
	// does not name is unknown, and backs nothing: the label is brought in
	// step with that, and the read counts as failed (round 7).
	row, err := s.readRecorded(ctx, p)
	if err != nil && !errors.Is(err, errNoMergeBase) {
		return row, err
	}
	out, syncErr := s.syncReady(ctx, row)
	return out, errors.Join(err, syncErr)
}

// errNoMergeBase marks a read of an open pull request GitHub answered but
// whose merge base it did not name: the merge base is then unknown, and
// no verdict filed on one backs the pull request (Codex review of #26,
// round 7).
var errNoMergeBase = errors.New("GitHub did not name the pull request's merge base")

// readMergeBase reads open pull request row's merge base on GitHub and
// records it, publishing the row when it changed. One it cannot read is
// recorded as unknown, and the row it answers has none whatever the
// record did, with an error that wraps errNoMergeBase.
func (s *Server) readMergeBase(ctx context.Context, row store.PullRow) (store.PullRow, error) {
	if row.State != store.PullOpen || !fullCommit.MatchString(row.HeadSHA) {
		return row, nil
	}
	read, cancel := s.withTimeout(ctx, pullTimeout)
	base, err := github.MergeBase(read, s.engine.GH, row.Repository, row.Base, row.HeadSHA)
	cancel()
	if err != nil {
		err = fmt.Errorf("%w: %w", errNoMergeBase, err)
		if got, recErr := s.setMergeBase(ctx, row, ""); recErr != nil {
			err = errors.Join(err, recErr)
		} else if got.ID == row.ID {
			row = got
		}
		row.MergeBase = ""
		return row, err
	}
	return s.setMergeBase(ctx, row, base)
}

// setMergeBase records row's merge base, and publishes the row when it
// changed, under workMu.
func (s *Server) setMergeBase(ctx context.Context, row store.PullRow, base string) (store.PullRow, error) {
	s.workMu.Lock()
	defer s.workMu.Unlock()
	got, changed, err := s.store.SetPullMergeBase(ctx, row.ID, row.HeadSHA, row.Base, base)
	if err != nil {
		return row, err
	}
	if changed {
		s.publishPull(got)
	}
	return got, nil
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

// mergedOf counts thread's pulls that merged, and those of them a read
// after its last reopen found open (ADR-0061); open says one is open.
func mergedOf(pulls []store.PullRow, thread store.ThreadRow) (merged, sinceOpen int, open bool) {
	for _, p := range pulls {
		switch p.State {
		case store.PullOpen:
			open = true
		case store.PullMerged:
			merged++
			if p.OpenSeq > thread.ReopenSeq {
				sinceOpen++
			}
		}
	}
	return merged, sinceOpen, open
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
	// A thread that reopened closes so only by a merge a read after its
	// reopen saw open (ADR-0061).
	row, err := s.store.Thread(ctx, thread)
	if err != nil {
		return err
	}
	if merged, sinceOpen, open := mergedOf(pulls, row); open || !team.MergedOut(0, merged, sinceOpen, row.Reopened > 0) {
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
	// Its callers, a round and a report, run a review pass after it: a
	// place it frees goes to what waited (Codex review of #26, round 9).
	// Its worker stops after the team's locks (ADR-0021). The close holds
	// outboxMu, which a message to its worker takes, as an explicit close
	// does: no worker run starts after it (Codex review of #29, round 2).
	worker := int64(0)
	defer func() {
		if worker != 0 {
			s.stopWorker(ctx, worker)
		}
	}()
	if s.beforeMergeClose != nil {
		s.beforeMergeClose(thread)
	}
	s.outboxMu.Lock()
	defer s.outboxMu.Unlock()
	s.workMu.Lock()
	defer s.workMu.Unlock()
	// The close holds only for the pull requests just checked: one closed
	// that a report linked meanwhile waits for the next round.
	now, err := s.store.PullsOfThread(ctx, thread)
	if err != nil {
		return err
	}
	for _, p := range now {
		// One linked meanwhile, open or closed, keeps the thread: nothing
		// is withdrawn from a thread that stays open (Codex review of #29,
		// round 3).
		if p.State == store.PullOpen || (p.State == store.PullClosed && !rechecked[p.ID]) {
			return nil
		}
	}
	// So does a thread a close and a reopen moved past these merges since
	// they were counted: read again under the locks, before anything is
	// withdrawn (Codex review of #121, round 3).
	if current, err := s.store.Thread(ctx, thread); err != nil {
		return err
	} else if merged, sinceOpen, _ := mergedOf(now, current); current.State == store.ThreadClosed || !team.MergedOut(0, merged, sinceOpen, current.Reopened > 0) {
		return nil
	}
	// Its worker's replies still queued are withdrawn before the close
	// commits; a withdrawal that fails leaves it for the next round.
	if _, err := s.withdrawWorkerReplies(ctx, thread); err != nil {
		return err
	}
	// The supervisor's messages are settled first, as an explicit close
	// settles them (ADR-0032).
	before, err := s.store.Thread(ctx, thread)
	if err != nil {
		return err
	}
	lead := int64(0)
	if before.State != store.ThreadClosed {
		if lead, _, err = s.sendsBeforeClose(ctx, before); err != nil {
			s.sendsAfterClose(ctx, lead)
			return err
		}
	}
	defer s.sendsAfterClose(ctx, lead)
	row, closed, err := s.store.CloseMergedThread(ctx, thread, func(merged []store.PullRow) string {
		refs := make([]string, len(merged))
		for i, p := range merged {
			refs[i] = p.Ref()
		}
		return team.MergedText(refs)
	})
	if err == nil && closed {
		s.publishThread(row)
		worker = row.WorkerSession
		if err := s.dropWorkerReplies(ctx, thread); err != nil {
			s.log.Warn("drop a closed thread's worker replies", "thread", thread, "err", err)
			s.deliverLater()
		}
	}
	return err
}

// RefreshPulls reads each pull request a round reads again on GitHub
// (store.PullsToRead: the open ones, and the closed ones of threads not
// closed), records and publishes what changed, then closes each thread whose pull requests
// merged (ADR-0016), found from the work database. One that cannot be
// read keeps its last state; done is false then.
func (s *Server) RefreshPulls(ctx context.Context) (done bool, err error) {
	_, done, err = s.refreshPulls(ctx)
	return done, err
}

// refreshPulls is RefreshPulls, and the pull requests it read: a round
// starts reviews of those alone (ADR-0019).
func (s *Server) refreshPulls(ctx context.Context) (fresh map[int64]int64, done bool, err error) {
	// fresh is never nil, which would mean every open pull request: a
	// round that read none starts no review (Codex review of #26, round 7).
	// Each is the read that found it (round 9).
	fresh = map[int64]int64{}
	toRead, err := s.store.PullsToRead(ctx)
	if err != nil {
		return fresh, false, err
	}
	var failures []error
	for _, p := range toRead {
		if read, err := s.refreshPull(ctx, p); err != nil {
			failures = append(failures, err)
		} else {
			fresh[p.ID] = read.ReadSeq
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
	return fresh, len(failures) == 0, errors.Join(failures...)
}

// StartPulls reads the open pull requests again every interval, in rounds
// that Stop ends; 0 starts none, and ends every live watch with its note,
// since nothing would check it (ADR-0072). The service calls it once,
// after StartDelivery.
func (s *Server) StartPulls(every time.Duration) {
	if every <= 0 {
		// No add is admitted from here on, and one admitted before has
		// committed, so the sweep ends it (Codex review of #173, round 1).
		s.watchAdmitMu.Lock()
		s.watchesOff = true
		s.watchAdmitMu.Unlock()
		// A sweep that fails runs again until it ends them (Codex review
		// of #173, round 2).
		s.uncheckedPass()
	}
	// Reviews start, and settle, whether or not pull requests are read in
	// rounds (ADR-0019; Codex review of #26, round 6).
	s.startReviews()
	// A worker's reply that a stop left unsent goes now (ADR-0021).
	s.settleWorkers()
	s.sessionMu.Lock()
	defer s.sessionMu.Unlock()
	s.pullsStarted = true
	if every <= 0 {
		return
	}
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
	fresh, _, err := s.refreshPulls(s.passCtx)
	if err != nil {
		s.log.Warn("read pull requests", "err", err)
	}
	// Each round starts, follows and ends the cross-reviews, after the
	// heads were read, starting reviews only of those read (ADR-0019).
	s.reviewPassOf(s.passCtx, reviewWork{fresh: fresh, read: map[int64]bool{}})
	// Each round checks the watches, after its reads (ADR-0072).
	if err := s.watchPass(s.passCtx); err != nil {
		s.log.Warn("check watches", "err", err)
	}
	// A shift whose done waits on CI reads it again (ADR-0024).
	s.shiftSoon()
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
	out := wireRow(row)
	review, found, err := s.store.LatestReview(ctx, row)
	if err != nil || !found {
		return out, err
	}
	return withReview(out, review), nil
}

// withReview is pull with review as its verdict.
func withReview(out proto.PullRequest, review store.ReviewRow) proto.PullRequest {
	out.Review = &proto.PullReview{ID: review.ID, Thread: review.ThreadID, Agent: review.Agent,
		Model: proto.ModelRef{Provider: review.Provider, ID: review.Model}, Family: review.Family, Head: review.HeadSHA,
		Verdict: proto.ReviewVerdict(review.Verdict), Summary: review.Summary, Findings: review.Findings, CreatedAt: review.CreatedAt}
	return out
}

// wireRow is row as the protocol has it, without its verdict.
func wireRow(row store.PullRow) proto.PullRequest {
	return proto.PullRequest{ID: row.ID, Thread: row.ThreadID, Repository: row.Repository, Number: row.Number, Title: row.Title,
		URL: row.URL, State: proto.PullState(row.State), Draft: row.Draft, Head: row.Head, HeadSHA: row.HeadSHA, Base: row.Base,
		Ready: row.Ready, Checks: proto.CheckState(row.Checks), CheckedAt: row.CheckedAt, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt}
}

// readOnePull reads listed pull request id on GitHub now, whatever state
// pocketd last saw (a closed one may have reopened), an open one with its
// merge base; records it and brings its label in step (refreshPull); then
// answers it with its verdict as the database then holds them. A merge's
// confirmation checks pocketd's backing as GitHub has the pull request at
// the owner's click, not at the last round, which -pr-every may never run;
// one pocketd cannot read now backs nothing, and is an error. A change the
// read found goes to the review pass, as a round's does (rule 45; Codex
// review of #26, rounds 7 and 8), and so does one a failed read recorded;
// a merged one has the pass close its author's thread after the answer
// (round 12).
func (s *Server) readOnePull(w http.ResponseWriter, r *http.Request) {
	id, ok := pathIDOf(w, r, "pull request")
	if !ok {
		return
	}
	row, _, _, found, err := s.store.PullWithReview(r.Context(), id)
	if err != nil {
		s.internal(w, "read the pull request", err)
		return
	}
	if !found {
		writeError(w, http.StatusNotFound, proto.ErrorCodeNotFound, "no pull request "+strconv.FormatInt(id, 10))
		return
	}
	read, err := s.refreshPull(r.Context(), row)
	if err != nil {
		// What the read recorded before it failed still ends the reviews
		// it made obsolete: the pass settles them against it, and starts
		// none from this read (Codex review of #26, round 12).
		s.reviewSoon()
		s.internal(w, "read "+row.Ref()+" on GitHub", err)
		return
	}
	s.reviewSoon(read)
	if s.afterConfirmRead != nil {
		s.afterConfirmRead()
	}
	row, review, reviewed, found, err := s.store.PullWithReview(r.Context(), id)
	// Once the answer is read, a read that found the pull request merged
	// has the review pass close its author's thread, as a round does: with
	// -pr-every=0 no round may come (ADR-0016; Codex review of #26, round
	// 12). A closed thread's merged pull request is listed no more, so the
	// answer is read first, and the answer does not wait for the close,
	// which may read the thread's other pull requests (Codex review of #32,
	// round 1).
	if read.State == store.PullMerged {
		s.closeMergedSoon(read.ThreadID)
	}
	if err != nil {
		s.internal(w, "read the pull request", err)
		return
	}
	if !found {
		writeError(w, http.StatusNotFound, proto.ErrorCodeNotFound, "no pull request "+strconv.FormatInt(id, 10))
		return
	}
	// The answer is the read this request checked, or none: another read
	// recorded since, which it did not check, may show backing (Codex
	// review of #26, round 11).
	if row.ReadSeq != read.ReadSeq {
		writeError(w, http.StatusConflict, proto.ErrorCodeConflict, row.Ref()+" was read again while pocketd checked it: check it again")
		return
	}
	pull := wireRow(row)
	if reviewed {
		pull = withReview(pull, review)
	}
	writeJSON(w, http.StatusOK, pull)
}

func (s *Server) listPulls(w http.ResponseWriter, r *http.Request) {
	// Each pull request with its verdict as the database held them
	// together (store.PullsWithReviews).
	rows, reviews, err := s.store.PullsWithReviews(r.Context())
	if err != nil {
		s.internal(w, "list pull requests", err)
		return
	}
	out := make([]proto.PullRequest, 0, len(rows))
	for _, row := range rows {
		pull := wireRow(row)
		if review, ok := reviews[row.ID]; ok {
			pull = withReview(pull, review)
		}
		out = append(out, pull)
	}
	writeJSON(w, http.StatusOK, out)
}
