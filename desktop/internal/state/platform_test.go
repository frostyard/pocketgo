package state

import "testing"

// The iPad and the iPhone (ADR-0071) have no Terminal, since NSGo's
// terminal view has not run on iOS, and no Quit, since iOS ends apps
// itself. Every desktop platform keeps both.
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

// An iPhone's window, and an iPad's narrower than the desktop's smallest,
// take the compact layout; an iPad's full window and every desktop's keep
// the desktop's.
func TestCompactIsIOSBelowTheDesktopsSmallestWindow(t *testing.T) {
	for _, tc := range []struct {
		goos  string
		width float32
		want  bool
	}{
		{"ios", 390, true},   // iPhone portrait
		{"ios", 320, true},   // iPad Slide Over
		{"ios", 759, true},   // just under the desktop's smallest window
		{"ios", 760, false},  // the desktop's smallest window
		{"ios", 820, false},  // iPad portrait
		{"ios", 1180, false}, // iPad landscape
		{"ios", 0, false},    // not laid out yet
		{"darwin", 390, false},
		{"linux", 390, false},
		{"windows", 390, false},
	} {
		if got := Compact(tc.goos, tc.width); got != tc.want {
			t.Errorf("Compact(%q, %v) = %v, want %v", tc.goos, tc.width, got, tc.want)
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

// Slim controls take a finger's size on iOS alone (ADR-0067).
func TestMinTargetIsTouchSizedOnlyOnIOS(t *testing.T) {
	for _, c := range []struct {
		goos string
		want float32
	}{
		{"ios", 44}, {"linux", 0}, {"darwin", 0}, {"windows", 0}, {"", 0},
	} {
		if got := MinTarget(c.goos); got != c.want {
			t.Errorf("MinTarget(%q) = %v, want %v", c.goos, got, c.want)
		}
	}
}
