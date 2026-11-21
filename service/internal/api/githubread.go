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
// gather's proposal session alone, which roleTools offers it and this
// checks again. pocketd builds every argument of gh (github.Get) and runs
// it through Engine.GH, in the sandbox with the service user's sign-in, as
// it reads pull requests (ADR-0016); the read takes at most
// team.GitHubReadTime, and its answer is bounded (team.GitHubAnswer). A
// read keeps nothing, so a call run again reads again.
func (s *Server) TeamGitHub(ctx context.Context, call TeamCall, r team.GitHubRead) (string, error) {
	if _, err := github.Endpoint(r.Path, r.Query); err != nil {
		return "", err
	}
	if _, err := s.store.GatherAskOfSession(ctx, call.Session); errors.Is(err, store.ErrNotFound) {
		return "", errors.New("team_github is a gather's proposal session's: this session answers no gather")
	} else if err != nil {
		return "", err
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
