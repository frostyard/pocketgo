package main

import (
	"context"

	"github.com/egoist/mygo"

	"github.com/frostyard/pocketgo/desktop/internal/config"
	"github.com/frostyard/pocketgo/desktop/internal/github"
	"github.com/frostyard/pocketgo/desktop/internal/state"
)

// githubAuth joins the owner's GitHub App sign-in for the window (ADR-0018,
// after frostyard/pocket's): the sign-in lock, one keyring entry, the
// device flow and the leased token. It is the seam state.GitHub calls; the
// lease and the refresh live in github.TokenSource.
type githubAuth struct {
	oauth *github.OAuth
	src   *github.TokenSource
}

// newGitHubAuth builds the sign-in over the desktop keyring, PocketGo's own
// service: no file fallback.
func newGitHubAuth() *githubAuth {
	store := config.GitHubApp{Ring: secrets{}, NotFound: mygo.ErrSecretNotFound}
	oauth := github.NewOAuth()
	return &githubAuth{oauth: oauth, src: github.NewTokenSource(oauth, store)}
}

// Open takes the sign-in lock and loads the stored sign-in: one PocketGo
// at a time, and without the lock nothing is loaded and nothing is read
// on GitHub. The lock stays until the process ends.
func (g *githubAuth) Open() error {
	if _, err := config.LockSignIn(""); err != nil {
		return err
	}
	return g.src.Load()
}

func (g *githubAuth) Start(ctx context.Context) (github.DeviceCode, error) { return g.oauth.Start(ctx) }
func (g *githubAuth) Poll(ctx context.Context, dc github.DeviceCode) (github.AppToken, error) {
	return g.oauth.Poll(ctx, dc)
}
func (g *githubAuth) BeginSignIn() int                         { return g.src.BeginSignIn() }
func (g *githubAuth) CancelSignIn(gen int)                     { g.src.CancelSignIn(gen) }
func (g *githubAuth) Adopt(tok github.AppToken, gen int) error { return g.src.Adopt(tok, gen) }
func (g *githubAuth) Invalidate(tokenGen int)                  { g.src.Invalidate(tokenGen) }
func (g *githubAuth) Forget() error                            { return g.src.Forget() }
func (g *githubAuth) Status() github.AppStatus                 { return g.src.Status() }
func (g *githubAuth) Lease(ctx context.Context) (string, int, func(), error) {
	return g.src.Lease(ctx)
}

// Close stops new leases and waits for a refresh or a write in progress.
func (g *githubAuth) Close() { g.src.Close() }

// githubClient is a client of GitHub acting with token, as the owner.
func githubClient(token string) state.GitHubAPI { return github.New(token) }
