package state

import "testing"

// The iPad runs the desktop's code (ADR-0040): it has no Terminal, since
// MyGo's terminal plugin has no library for iOS, and no Quit, since iOS
// ends apps itself. Every desktop platform keeps both.
func TestThePlatformsTerminalAndQuit(t *testing.T) {
	for _, tc := range []struct {
		goos           string
		terminal, quit bool
	}{
		{goos: "ios"},
		{goos: "darwin", terminal: true, quit: true},
		{goos: "linux", terminal: true, quit: true},
		{goos: "windows", terminal: true, quit: true},
	} {
		if got := HasTerminal(tc.goos); got != tc.terminal {
			t.Errorf("HasTerminal(%q) = %v, want %v", tc.goos, got, tc.terminal)
		}
		if got := QuitsItself(tc.goos); got != tc.quit {
			t.Errorf("QuitsItself(%q) = %v, want %v", tc.goos, got, tc.quit)
		}
	}
}

// iOS may end a suspended app without closing its window, so the iPad
// saves as it goes to the background; a desktop saves as its window
// closes, as before (ADR-0041).
func TestOnlyIOSSavesOnSuspend(t *testing.T) {
	for goos, want := range map[string]bool{"ios": true, "darwin": false, "linux": false, "windows": false} {
		if got := SavesOnSuspend(goos); got != want {
			t.Errorf("SavesOnSuspend(%q) = %v, want %v", goos, got, want)
		}
	}
}
