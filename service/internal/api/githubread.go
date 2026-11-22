package api

import (
	"context"
	"errors"
	"fmt"

	"github.com/frostyard/pocketgo/service/internal/github"
	"github.com/frostyard/pocketgo/service/internal/store"
	"github.com/frostyard/pocketgo/service/internal/team"
)

// TeamGitHub is team_github (ADR-0036): a GET of GitHub's REST API, from a
// gather's proposal session, a consult's session, a plan's lead in its
// plan thread or a cross-review pocketd started (ADR-0057) alone, which
// roleTools offers it and this checks again.
// pocketd builds every argument of gh (github.Get) and runs it through
// Engine.GH, in the sandbox with the service user's sign-in, as it reads
// pull requests (ADR-0016); the read takes at most team.GitHubReadTime,
// and its answer is bounded (team.GitHubAnswer). A read keeps nothing, so
// a call run again reads again.
func (s *Server) TeamGitHub(ctx context.Context, call TeamCall, r team.GitHubRead) (string, error) {
	if _, err := github.Endpoint(r.Path, r.Query); err != nil {
		return "", err
	}
	if ok, err := s.readsGitHub(ctx, call.Session); err != nil {
		return "", err
	} else if !ok {
		return "", errors.New("team_github is for a gather's proposal session, a consult, a plan's lead or a cross-review, and this session is none")
	}
	// The call's own context bounds the read: an abort stops gh. Tests see
	// the bound it asks for (Codex review of #61, round 1).
	read, cancel := s.withTimeout(ctx, team.GitHubReadTime)
	defer cancel()
	out, err := github.Get(read, s.engine.GH, r.Path, r.Query)
	switch {
	case err != nil && ctx.Err() == nil && errors.Is(read.Err(), context.DeadlineExceeded):
		return "", fmt.Errorf("GitHub did not answer within %v: %w", team.GitHubReadTime, err)
	case err != nil:
		return "", err
	}
	return team.GitHubAnswer(out)
}

// readsGitHub says whether session is one roleTools gives team_github, by
// its records alone: a gather's ask's session, a consult's, or the
// specialist session of a plan's own thread or of a review thread pocketd
// started (ADR-0057), never another thread's, the author's of the pull
// request under review included. The state of its ask, consult, plan or
// review does not matter: a read writes nothing.
func (s *Server) readsGitHub(ctx context.Context, session int64) (bool, error) {
	if _, err := s.store.GatherAskOfSession(ctx, session); err == nil {
		return true, nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return false, err
	}
	if _, err := s.store.ConsultOfSession(ctx, session); err == nil {
		return true, nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return false, err
	}
	thread, err := s.store.ThreadOfSession(ctx, session)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return false, nil
	case err != nil:
		return false, err
	}
	return thread.IsPlanThread() || thread.ReviewPull != 0, nil
}
