package api

import (
	"context"
	"errors"
	"net/http"
	"slices"
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
		p, err := s.readPull(ctx, repo, ref.Number)
		if err != nil {
			return nil, err
		}
		if err := team.CheckPull(thread.Repository, thread.Branch, repositories,
			team.PullFacts{Repository: p.Repository, Number: p.Number, Head: p.Head, HeadRepository: p.HeadRepository}); err != nil {
			return nil, err
		}
		out = append(out, pullRow(p))
	}
	return out, nil
}

// readPull reads one pull request on GitHub, bound by pullTimeout.
func (s *Server) readPull(ctx context.Context, repository string, number int64) (github.Pull, error) {
	read, cancel := s.withTimeout(ctx, pullTimeout)
	defer cancel()
	return github.Read(read, s.engine.GH, repository, number)
}

func pullRow(p github.Pull) store.PullRow {
	return store.PullRow{Repository: p.Repository, Number: p.Number, Title: p.Title, URL: p.URL, State: p.State, Draft: p.Draft,
		Head: p.Head, HeadSHA: p.HeadSHA, Base: p.Base}
}

// pullNames are pulls as a report names them: repository#number (state).
func pullNames(pulls []store.PullRow) []string {
	out := make([]string, len(pulls))
	for i, p := range pulls {
		out[i] = p.Ref() + " (" + p.State + ")"
	}
	return out
}

// closeIfMerged closes thread once its pull requests merged (ADR-0016),
// and publishes it, under workMu.
func (s *Server) closeIfMerged(ctx context.Context, thread int64) error {
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
// publishes what changed, and closes a thread whose pull requests merged
// (ADR-0016). One that cannot be read keeps its last state; done is false
// then.
func (s *Server) RefreshPulls(ctx context.Context) (done bool, err error) {
	open, err := s.store.OpenPulls(ctx)
	if err != nil {
		return false, err
	}
	var failures []error
	threads := map[int64]bool{}
	for _, p := range open {
		read, err := s.readPull(ctx, p.Repository, p.Number)
		if err != nil {
			failures = append(failures, err)
			continue
		}
		s.workMu.Lock()
		row, changed, err := s.store.UpdatePull(ctx, p.ID, pullRow(read))
		if err == nil && changed {
			s.publishPull(row)
		}
		s.workMu.Unlock()
		if err != nil {
			failures = append(failures, err)
			continue
		}
		if row.State != store.PullOpen {
			threads[row.ThreadID] = true
		}
	}
	ids := make([]int64, 0, len(threads))
	for id := range threads {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
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
