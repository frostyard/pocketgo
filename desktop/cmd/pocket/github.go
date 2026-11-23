package main

import (
	"github.com/egoist/mygo"
	"github.com/frostyard/pocketgo/desktop/internal/config"
	"github.com/frostyard/pocketgo/desktop/internal/github"
	"github.com/frostyard/pocketgo/desktop/internal/ownerauth"
	"github.com/frostyard/pocketgo/desktop/internal/state"
)

func newGitHubAuth() *ownerauth.Auth {
	return ownerauth.New(config.GitHubApp{Ring: secrets{}, NotFound: mygo.ErrSecretNotFound}, "")
}

// githubClient is a client of GitHub acting with token, as the owner.
func githubClient(token string) state.GitHubAPI { return github.New(token) }
