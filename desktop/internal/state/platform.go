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
