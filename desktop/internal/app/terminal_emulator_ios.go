//go:build ios

package app

import (
	"errors"
	"io"

	"github.com/egoist/mygo/ui"
)

// iOS has no Terminal (state.HasTerminal, ADR-0040): MyGo's terminal plugin
// has no libghostty-vt for iOS, and MyGo's iOS build stops on a package
// that names a library it cannot bundle. These stand in for
// terminal_emulator.go and terminal_replay.go, which iOS builds leave out,
// so the plugin is not in the app. The window never goes to the Terminal
// place there, so none of them runs.

// errNoTerminal is what the stand-ins fail with.
var errNoTerminal = errors.New("the Terminal is not available on iOS")

// noEmulator is the emulator iOS has: none.
type noEmulator struct{}

// Close does nothing.
func (*noEmulator) Close() error { return nil }

// termEmulator is a shell's emulator.
type termEmulator = *noEmulator

func loadEmulatorLibrary() error { return errNoTerminal }

func (a *App) newEmulator(io.ReadWriteCloser, func(string), func(int)) (termEmulator, error) {
	return nil, errNoTerminal
}

func emulatorView(c *ui.Context, _ termEmulator) ui.Element { return ui.Box(c) }

func terminalReplay([]byte, int, int) ([]byte, string, error) { return nil, "", errNoTerminal }
