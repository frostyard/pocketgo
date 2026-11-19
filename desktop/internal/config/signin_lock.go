package config

import (
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
)

// The sign-in lock is 1 PocketGo at a time on GitHub: the keyring entry is 1
// per desktop user, whatever POCKETGO_CONFIG_DIR says, so 2 Pockets would spend
// each other's refresh tokens. The lock is an operating-system lock on
// <os.UserConfigDir()>/org.frostyard.PocketGo/github-signin.lock, never under
// POCKETGO_CONFIG_DIR. PocketGo takes
// it at start and holds it until the process ends: the operating system
// releases it then, also after a crash
// (frostyard/pocket's docs/adr/0010-act-as-the-owner-through-a-github-app-user-token.md).

// userHome is the desktop user's home from the system's user database,
// never $HOME: the keyring entry is the user's, so every PocketGo of the
// user takes one lock, whatever its environment says (Codex review of #24,
// round 1). Tests replace it.
var userHome = func() (string, error) {
	u, err := user.Current()
	if err != nil {
		return "", err
	}
	return u.HomeDir, nil
}

// signInLockDir is the lock's folder: the user's configuration folder as
// the system's user database places it, whatever XDG_CONFIG_HOME or HOME
// say. On Windows it is the roaming AppData folder.
func signInLockDir() (string, error) {
	if runtime.GOOS == "windows" {
		base, err := os.UserConfigDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(base, AppID), nil
	}
	home, err := userHome()
	if err != nil {
		return "", err
	}
	if home == "" {
		return "", errors.New("the user database names no home folder")
	}
	if runtime.GOOS == "darwin" {
		return filepath.Join(home, "Library", "Application Support", AppID), nil
	}
	return filepath.Join(home, ".config", AppID), nil
}

// ErrSignInHeld is another PocketGo that holds the sign-in lock. Settings shows
// "another PocketGo holds the GitHub sign-in", and PocketGo loads no sign-in,
// refreshes nothing and reads nothing on GitHub.
var ErrSignInHeld = errors.New("another PocketGo holds the GitHub sign-in")

// signInLockName is the lock file inside the folder.
const signInLockName = "github-signin.lock"

// LockSignIn takes the sign-in lock and gives the release for this process;
// dir "" is <os.UserConfigDir()>/org.frostyard.PocketGo, the folder of the
// keyring's service. It creates the folder (mode 0700) and the file (mode
// 0600). A lock another PocketGo holds is ErrSignInHeld at once: it never
// waits.
func LockSignIn(dir string) (release func(), err error) {
	if dir == "" {
		if dir, err = signInLockDir(); err != nil {
			return nil, fmt.Errorf("no folder for the GitHub sign-in lock: %w", err)
		}
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("the GitHub sign-in lock's folder: %w", err)
	}
	path := filepath.Join(dir, signInLockName)
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("the GitHub sign-in lock file: %w", err)
	}
	if err := lockFile(f); err != nil {
		_ = f.Close()
		return nil, err
	}
	return func() {
		unlockFile(f)
		_ = f.Close()
	}, nil
}
