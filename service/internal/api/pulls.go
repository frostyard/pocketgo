package api

import (
	"context"
	"errors"
	"net/http"
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
func (s *Server) checkPulls(ctx context.Context, thread store.ThreadRow, refs []team.PullRef) ([]store.PullRow, error) {
	if len(refs) == 0 {
		return nil, nil
	}
	var repositories []string
	if s.profiles != nil {
		if p, err := s.profiles.Find(thread.Agent); err == nil {
			for repo := range p.Repositories {
				repositories = append(repositories, repo)
			}
		}
	}
	out := make([]store.PullRow, 0, len(refs))
	for _, ref := range refs {
		repo, err := team.PullRepository(ref, thread.Repository)
		if err != nil {
			return nil, err
		}
		p, seen, err := s.readGitHub(ctx, repo, ref.Number)
		if err != nil {
			return nil, err
		}
		if err := team.CheckPull(thread.Repository, thread.Branch, repositories,
			team.PullFacts{Repository: p.Repository, Number: p.Number, Head: p.Head, HeadRepository: p.HeadRepository}); err != nil {
			return nil, err
		}
		out = append(out, pullRow(p, seen))
	}
	return out, nil
}

// readGitHub reads one pull request on GitHub, bound by pullTimeout, and
// says when the read began: a row keeps the newest read
// (store.UpdatePull).
func (s *Server) readGitHub(ctx context.Context, repository string, number int64) (github.Pull, time.Time, error) {
	seen := s.now()
	read, cancel := s.withTimeout(ctx, pullTimeout)
	defer cancel()
	p, err := github.Read(read, s.engine.GH, repository, number)
	return p, seen, err
}

// readPull is readGitHub as a row to record.
func (s *Server) readPull(ctx context.Context, repository string, number int64) (store.PullRow, error) {
	p, seen, err := s.readGitHub(ctx, repository, number)
	if err != nil {
		return store.PullRow{}, err
	}
	return pullRow(p, seen), nil
}

func pullRow(p github.Pull, seen time.Time) store.PullRow {
	return store.PullRow{Repository: p.Repository, Number: p.Number, Title: p.Title, URL: p.URL, State: p.State, Draft: p.Draft,
		Head: p.Head, HeadSHA: p.HeadSHA, Base: p.Base, CheckedAt: seen}
}

// pullNames are pulls as a report names them: repository#number (state).
func pullNames(pulls []store.PullRow) []string {
	out := make([]string, len(pulls))
	for i, p := range pulls {
		out[i] = p.Ref() + " (" + p.State + ")"
	}
	return out
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
	for _, p := range pulls {
		if p.State != store.PullClosed {
			continue
		}
		read, err := s.readPull(ctx, p.Repository, p.Number)
		if err != nil {
			return err
		}
		row, err := s.recordPull(ctx, p.ID, read)
		if err != nil {
			return err
		}
		if row.State == store.PullOpen {
			return nil
		}
	}
	s.workMu.Lock()
	defer s.workMu.Unlock()
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

// RefreshPulls reads each open pull request on GitHub again, records and
// publishes what changed, then closes each thread whose pull requests
// merged (ADR-0016), found from the work database. One that cannot be
// read keeps its last state; done is false then.
func (s *Server) RefreshPulls(ctx context.Context) (done bool, err error) {
	open, err := s.store.OpenPulls(ctx)
	if err != nil {
		return false, err
	}
	var failures []error
	for _, p := range open {
		read, err := s.readPull(ctx, p.Repository, p.Number)
		if err == nil {
			_, err = s.recordPull(ctx, p.ID, read)
		}
		if err != nil {
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
	pull := wirePull(row)
	s.emit(proto.Event{Type: proto.EventPull, Pull: &pull})
}

func wirePull(row store.PullRow) proto.PullRequest {
	return proto.PullRequest{ID: row.ID, Thread: row.ThreadID, Repository: row.Repository, Number: row.Number, Title: row.Title,
		URL: row.URL, State: proto.PullState(row.State), Draft: row.Draft, Head: row.Head, HeadSHA: row.HeadSHA, Base: row.Base,
		CheckedAt: row.CheckedAt, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt}
}

func (s *Server) listPulls(w http.ResponseWriter, r *http.Request) {
	rows, err := s.store.ListPulls(r.Context())
	if err != nil {
		s.internal(w, "list pull requests", err)
		return
	}
	out := make([]proto.PullRequest, 0, len(rows))
	for _, row := range rows {
		out = append(out, wirePull(row))
	}
	writeJSON(w, http.StatusOK, out)
}
