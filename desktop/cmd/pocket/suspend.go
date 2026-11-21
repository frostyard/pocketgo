package main

import "github.com/frostyard/pocketgo/desktop/internal/state"

// keeper is the window as the system suspends the app: it keeps what it
// keeps between runs (app.App.Suspend).
type keeper interface{ Suspend() }

// holder is the GitHub sign-in as the system suspends the app and as the
// app comes back (githubAuth).
type holder interface {
	Suspend()
	Resume()
}

// suspendHooks are the listeners of the system's suspend and resume on goos
// (ADR-0041), which MyGo calls on the window's thread. On iOS the window
// keeps what it keeps, then the GitHub sign-in holds its refresh and lets
// one in progress reach the keychain; coming back lets a refresh start
// again. Elsewhere there are none: a desktop saves as its window closes
// (state.SavesOnSuspend).
func suspendHooks(goos string, win keeper, auth holder) (onSuspend, onResume func()) {
	if !state.SavesOnSuspend(goos) {
		return nil, nil
	}
	return func() {
		win.Suspend()
		auth.Suspend()
	}, auth.Resume
}
