package state

// The platform rules (ADR-0040, ADR-0071): what the client offers on the
// system it runs on, by Go's name of it, which the NSGo client takes from
// its environment. The iPad and the iPhone run the NSGo client.

// platformIOS is runtime.GOOS on an iPad or an iPhone.
const platformIOS = "ios"

// HasTerminal reports whether the Terminal place (ADR-0025) shows on goos.
// The removed MyGo client's terminal plugin had no libghostty-vt for iOS.
// NSGo's terminal view has an iOS library, which has not run there, so iOS
// has no Terminal until it has (ADR-0071): the client leaves the place
// out.
func HasTerminal(goos string) bool { return goos != platformIOS }

// QuitsItself reports whether the app offers Quit on goos. iOS ends apps
// itself: the client offers no Quit on iOS.
func QuitsItself(goos string) bool { return goos != platformIOS }

// SavesOnSuspend reports whether the client holds the GitHub sign-in's
// refresh while the system has the app in the background on goos, until
// the app comes back (ADR-0041). iOS may end a suspended app without
// closing its window, so a refresh in progress lands in the keychain
// before the app stops; the NSGo client keeps each preference as it
// changes, so it has nothing else to save then. A desktop is suspended
// when the computer sleeps, and goes on after it with its window open.
func SavesOnSuspend(goos string) bool { return goos == platformIOS }

// CompactBelow is the desktop's smallest window width in DIPs, at 100%
// (the removed MyGo client's MinWidth): a window narrower than it takes
// the compact layout.
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

// MinTouchTarget is the smallest side, in DIPs, of a control a finger
// presses on iOS: Apple's Human Interface Guidelines' 44 points.
const MinTouchTarget = 44

// MinTarget is the smallest side a slim control takes on goos: MinTouchTarget
// on iOS, the iPad's full window and the iPhone's alike, whose controls a
// finger presses; 0 elsewhere, where a control keeps the kit's size.
func MinTarget(goos string) float32 {
	if goos == platformIOS {
		return MinTouchTarget
	}
	return 0
}
