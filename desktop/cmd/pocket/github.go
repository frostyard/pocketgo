package main

import (
	"context"
	"errors"
	"sync"

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
	// lockDir is the sign-in lock's folder, "" for the user's; release
	// holds the lock, which the open file behind it keeps: dropped, the
	// file could be collected and the lock with it (Codex review of #24,
	// round 1).
	lockDir string
	// mu orders Open's lock with Close: a lock taken after Close began is
	// let go at once (Codex review of #24, round 2).
	mu      sync.Mutex
	closed  bool
	release func()
}

// newGitHubAuth builds the sign-in over the desktop keyring, PocketGo's own
// service: no file fallback.
func newGitHubAuth() *githubAuth {
	return newGitHubAuthWith(config.GitHubApp{Ring: secrets{}, NotFound: mygo.ErrSecretNotFound}, "")
}

func newGitHubAuthWith(store config.GitHubApp, lockDir string) *githubAuth {
	oauth := github.NewOAuth()
	return &githubAuth{oauth: oauth, src: github.NewTokenSource(oauth, store), lockDir: lockDir}
}

// Open takes the sign-in lock and loads the stored sign-in: one PocketGo
// at a time, and without the lock nothing is loaded and nothing is read
// on GitHub. The lock stays until Close.
func (g *githubAuth) Open() error {
	g.mu.Lock()
	if g.closed {
		g.mu.Unlock()
		return errors.New("the window is closing")
	}
	release, err := config.LockSignIn(g.lockDir)
	if err != nil {
		g.mu.Unlock()
		return err
	}
	g.release = release
	g.mu.Unlock()
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
func (g *githubAuth) BeginForget() (func() error, error)       { return g.src.BeginForget() }
func (g *githubAuth) DeleteAgain() error                       { return g.src.DeleteAgain() }
func (g *githubAuth) BeginSaveAgain() (func() error, error)    { return g.src.BeginSaveAgain() }
func (g *githubAuth) Status() github.AppStatus                 { return g.src.Status() }
func (g *githubAuth) Lease(ctx context.Context) (string, int, func(), error) {
	return g.src.Lease(ctx)
}

// Close stops new leases and waits for a refresh or a write in progress,
// then lets the sign-in lock go. When that wait runs out with the work
// still running, the lock stays until the process ends: no other PocketGo
// takes the pair while it is being written.
func (g *githubAuth) Close() {
	g.mu.Lock()
	g.closed = true
	g.mu.Unlock()
	ended := g.src.Close()
	g.mu.Lock()
	defer g.mu.Unlock()
	if ended && g.release != nil {
		g.release()
		g.release = nil
	}
}

// githubClient is a client of GitHub acting with token, as the owner.
func githubClient(token string) state.GitHubAPI { return github.New(token) }
