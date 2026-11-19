//go:build windows

package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
)

// userHome is the user's roaming AppData folder as the user's profile
// names it (SHGetKnownFolderPath), never %AppData%: the Credential
// Manager entry is the user's, so every PocketGo of the user takes one
// lock (Codex review of #24, round 2). Tests replace it.
var userHome = func() (string, error) {
	return windows.KnownFolderPath(windows.FOLDERID_RoamingAppData, windows.KF_FLAG_DEFAULT)
}

// signInLockDir is the lock's folder in the user's roaming AppData.
func signInLockDir() (string, error) {
	base, err := userHome()
	if err != nil {
		return "", err
	}
	if base == "" || !filepath.IsAbs(base) {
		return "", fmt.Errorf("the user's profile names no AppData folder: %q", base)
	}
	return filepath.Join(base, AppID), nil
}

// lockFile takes an exclusive share lock on the whole of f through
// LockFileEx, and fails at once when another process holds it.
func lockFile(f *os.File) error {
	ol := new(windows.Overlapped)
	err := windows.LockFileEx(windows.Handle(f.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, ol)
	if err != nil {
		if errors.Is(err, windows.ERROR_LOCK_VIOLATION) || errors.Is(err, windows.ERROR_IO_PENDING) {
			return ErrSignInHeld
		}
		return fmt.Errorf("the GitHub sign-in lock: %w", err)
	}
	return nil
}

// unlockFile releases the lock on f.
func unlockFile(f *os.File) {
	ol := new(windows.Overlapped)
	_ = windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, ol)
}
