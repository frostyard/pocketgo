package state

// The platform rules (ADR-0040): what the client offers on the system it
// runs on, by runtime.GOOS, which the program gives the window
// (app.Options.Platform). The iPad runs the desktop's own code on MyGo's
// iOS target.

// platformIOS is runtime.GOOS on an iPad or an iPhone.
const platformIOS = "ios"

// HasTerminal reports whether the Terminal place (ADR-0025) shows on goos.
// MyGo's terminal plugin has no libghostty-vt for iOS: its bindings are
// stubs there (vt.Supported is false), so terminal.Load fails, and MyGo's
// iOS build cannot bundle the library. iOS has no Terminal until the
// plugin supports it: the sidebar leaves the place out, and the emulator's
// library never loads.
func HasTerminal(goos string) bool { return goos != platformIOS }

// QuitsItself reports whether the app offers Quit on goos. iOS ends apps
// itself, and MyGo's App.Quit there stops the app's loop and leaves the
// process to the system, so the window would stop answering: the menu has
// no Quit on iOS.
func QuitsItself(goos string) bool { return goos != platformIOS }

// SavesOnSuspend reports whether the window keeps what it keeps between
// runs, as it closes, when the system suspends the app on goos, and holds
// the GitHub sign-in's refresh until the app comes back (ADR-0041). iOS
// may end a suspended app without closing its window, so App.Close may
// never run there: the pane widths, the session shown and the settings are
// saved as the app goes to the background, and a refresh in progress
// lands in the keychain before the app stops. A desktop is suspended when
// the computer sleeps, and goes on after it with its window open: it saves
// as its window closes, as it did.
func SavesOnSuspend(goos string) bool { return goos == platformIOS }

// CompactBelow is the desktop's smallest window width in DIPs, at 100%
// (cmd/pocket's MinWidth): a window narrower than it takes the compact
// layout.
const CompactBelow = 760

// Compact reports whether a window width DIPs wide at 100% on goos takes
// the compact layout (the phone client's Phase 3): one view at a time, full
// width, under a compact top bar, in place of the sidebar beside the main
// pane. On iOS a window narrower than the desktop's smallest does: an
// iPhone's, and an iPad's in Split View or Slide Over. An iPad's full
// window keeps the desktop's layout, and a desktop's window never gets
// that narrow. A width not known yet is not compact.
func Compact(goos string, width float32) bool {
	return goos == platformIOS && width > 0 && width < CompactBelow
}
