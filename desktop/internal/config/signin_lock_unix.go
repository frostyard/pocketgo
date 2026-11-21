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
	"strings"

	"golang.org/x/sys/unix"
)

// userHome is the desktop user's home from the system's user database, by
// the process's user id: never $HOME, and no fallback to it (Codex review
// of #24, rounds 1 and 2). The keyring entry is the user's, so every
// PocketGo of the user takes one lock, whatever its environment says; a
// user the database does not name gets no lock, and so no approve and
// merge. iOS reads appHome instead (signInLockDir). Tests replace it.
var userHome = func() (string, error) {
	if runtime.GOOS == "darwin" {
		// macOS asks the system's directory, through libSystem.
		u, err := user.LookupId(strconv.Itoa(os.Getuid()))
		if err != nil {
			return "", fmt.Errorf("the user database does not name user %d: %w", os.Getuid(), err)
		}
		return u.HomeDir, nil
	}
	// Without cgo, os/user falls back to $HOME for a user /etc/passwd does
	// not name (Codex review of #24, round 3): the file is read here, and
	// a user it does not name gets no lock.
	return passwdHome(passwdPath, os.Getuid())
}

// passwdPath is the user database pocketgo reads on Linux and the BSDs.
var passwdPath = "/etc/passwd"

// passwdHome is the home folder path names for uid: name:password:uid:gid:
// gecos:home:shell, one user a line.
func passwdHome(path string, uid int) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("the user database: %w", err)
	}
	want := strconv.Itoa(uid)
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Split(line, ":")
		if len(fields) >= 7 && fields[2] == want {
			return fields[5], nil
		}
	}
	return "", fmt.Errorf("%s does not name user %d", path, uid)
}

// lockGOOS is the system the lock's folder is chosen for: runtime.GOOS.
// Tests replace it, to check iOS's folder on Linux.
var lockGOOS = runtime.GOOS

// appHome is the home folder iOS gives the app: its data container, which
// the system names in the app's HOME (ADR-0040). Only iOS reads it. Tests
// replace it.
var appHome = func() (string, error) { return os.Getenv("HOME"), nil }

// signInLockDir is the lock's folder:
//   - on iOS, Application Support in the app's container, the home folder
//     iOS gives the app (ADR-0040). The sandbox lets the app write nowhere
//     else. Its keychain entry is the app's own, not the user's, and the
//     system, not the user, sets its environment and runs one process of
//     it, so one lock in the container is one lock per GitHub credential;
//   - on macOS, the user's Application Support, and elsewhere the user's
//     configuration folder, as the user database places them, whatever
//     XDG_CONFIG_HOME or HOME say.
func signInLockDir() (string, error) {
	if lockGOOS == "ios" {
		home, err := appHome()
		if err != nil {
			return "", err
		}
		if home == "" || !filepath.IsAbs(home) {
			return "", fmt.Errorf("iOS names no container for the app: %q", home)
		}
		return filepath.Join(home, "Library", "Application Support", AppID), nil
	}
	home, err := userHome()
	if err != nil {
		return "", err
	}
	if home == "" || !filepath.IsAbs(home) {
		return "", fmt.Errorf("the user database names no home folder: %q", home)
	}
	if lockGOOS == "darwin" {
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
