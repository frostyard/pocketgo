//go:build unix

package config

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// onPlatform makes the lock's folder chosen as on goos, with the user
// database's home user and the app's home app, until the test ends.
func onPlatform(t *testing.T, goos, user, app string) {
	t.Helper()
	oldGOOS, oldUser, oldApp := lockGOOS, userHome, appHome
	t.Cleanup(func() { lockGOOS, userHome, appHome = oldGOOS, oldUser, oldApp })
	lockGOOS = goos
	userHome = func() (string, error) { return user, nil }
	appHome = func() (string, error) { return app, nil }
}

// On iOS the lock is in the app's container, the home folder iOS gives the
// app: the user database's home is outside it, where the sandbox lets the
// app write nothing, so the GitHub sign-in would never open and Approve and
// Merge would stay off (ADR-0040).
func TestTheSignInLockOnIOSIsInTheAppsContainer(t *testing.T) {
	user, container := t.TempDir(), t.TempDir()
	onPlatform(t, "ios", user, container)
	release, err := LockSignIn("")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	path := filepath.Join(container, "Library", "Application Support", AppID, "github-signin.lock")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("no lock in the app's container: %v", err)
	}
	if names, _ := os.ReadDir(user); len(names) != 0 {
		t.Errorf("the user database's home holds %v", names)
	}
	// It is one lock: a second PocketGo of the same container is refused.
	if _, err := LockSignIn(""); !errors.Is(err, ErrSignInHeld) {
		t.Errorf("a second lock in the container: %v", err)
	}
}

// A container iOS does not name gets no lock, and the desktops keep the
// user database's home whatever the app's home says.
func TestTheSignInLocksFolderByPlatform(t *testing.T) {
	for _, home := range []string{"", "relative/home"} {
		onPlatform(t, "ios", "/var/mobile", home)
		if dir, err := signInLockDir(); err == nil {
			t.Errorf("iOS with the app's home %q: a lock in %q", home, dir)
		}
	}
	for goos, want := range map[string]string{
		"darwin": filepath.Join("/Users/owner", "Library", "Application Support", AppID),
		"linux":  filepath.Join("/Users/owner", ".config", AppID),
	} {
		onPlatform(t, goos, "/Users/owner", "/elsewhere")
		if dir, err := signInLockDir(); err != nil || dir != want {
			t.Errorf("%s: %q, %v; want %q", goos, dir, err, want)
		}
	}
}
