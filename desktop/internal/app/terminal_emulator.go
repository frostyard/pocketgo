//go:build !ios

package app

import (
	"io"

	"github.com/egoist/mygo/plugins/terminal"
	"github.com/egoist/mygo/ui"

	"github.com/frostyard/pocketgo/desktop/internal/state"
)

// The Terminal's emulator is Ghostty's, MyGo's terminal plugin (ADR-0025).
// Only this file and terminal_replay.go import the plugin. iOS builds leave
// both out for terminal_emulator_ios.go (ADR-0040): the plugin has no
// libghostty-vt for iOS, and MyGo's iOS build stops on a package that names
// a library it cannot bundle.

// termEmulator is a shell's emulator.
type termEmulator = *terminal.Terminal

// loadEmulatorLibrary loads libghostty-vt, once.
func loadEmulatorLibrary() error { return terminal.Load() }

// newEmulator is an emulator over conn, a shell on pocketd's host, with
// termOptions; onTitle gets the title the shell sets, onExit its end.
func (a *App) newEmulator(conn io.ReadWriteCloser, onTitle func(string), onExit func(int)) (termEmulator, error) {
	opts := a.termOptions(conn)
	opts.OnTitle, opts.OnExit = onTitle, onExit
	return terminal.New(opts)
}

// emulatorView draws term.
func emulatorView(c *ui.Context, term termEmulator) ui.Element { return terminal.View(c, term) }

// termOptions are the emulator's options for a shell on pocketd's host.
// What the shell prints is the server's: it may not write the desktop's
// clipboard (OSC 52), which the next paste into a local terminal would
// run, and a link from it opens only on the web or in mail, never as a
// file or another program's address (state.SafeLink). Reads of the
// clipboard stay refused (the plugin's default). The terminal's zoom (the
// wheel with Ctrl held, its font size keys) zooms the whole window, as the
// window's keys do, not one font.
func (a *App) termOptions(conn io.ReadWriteCloser) terminal.Options {
	return terminal.Options{
		Conn:             conn,
		NoClipboardWrite: true,
		OpenLink: func(link string) bool {
			if state.SafeLink(link) {
				a.openURL(link)
			} else {
				a.toast("Only web and mail links open from a terminal")
			}
			return true // handled: nothing else opens it
		},
		OnZoom: func(z terminal.Zoom) bool {
			if z.Reset {
				a.setZoom(1)
			} else {
				a.zoomBy(z.Steps)
			}
			return true
		},
	}
}
