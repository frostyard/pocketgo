//go:build unix

package config

import (
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"

	"golang.org/x/sys/unix"
)

// userHome is the desktop user's home from the system's user database, by
// the process's user id: never $HOME, and no fallback to it (Codex review
// of #24, rounds 1 and 2). The keyring entry is the user's, so every
// PocketGo of the user takes one lock, whatever its environment says; a
// user the database does not name gets no lock, and so no approve and
// merge. Tests replace it.
var userHome = func() (string, error) {
	u, err := user.LookupId(strconv.Itoa(os.Getuid()))
	if err != nil {
		return "", fmt.Errorf("the user database does not name user %d: %w", os.Getuid(), err)
	}
	return u.HomeDir, nil
}

// signInLockDir is the lock's folder: the user's configuration folder as
// the user database places it, whatever XDG_CONFIG_HOME or HOME say.
func signInLockDir() (string, error) {
	home, err := userHome()
	if err != nil {
		return "", err
	}
	if home == "" || !filepath.IsAbs(home) {
		return "", fmt.Errorf("the user database names no home folder: %q", home)
	}
	if runtime.GOOS == "darwin" {
		return filepath.Join(home, "Library", "Application Support", AppID), nil
	}
	return filepath.Join(home, ".config", AppID), nil
}

// lockFile takes an exclusive flock on f, and fails at once when another
// process holds it.
func lockFile(f *os.File) error {
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			return ErrSignInHeld
		}
		return fmt.Errorf("the GitHub sign-in lock: %w", err)
	}
	return nil
}

// unlockFile releases the flock on f.
func unlockFile(f *os.File) {
	_ = unix.Flock(int(f.Fd()), unix.LOCK_UN)
}
