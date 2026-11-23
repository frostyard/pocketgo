package nsgoclient

import (
	"io"

	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/ansi/parser"
	"github.com/frostyard/pocketgo/desktop/internal/state"
	"github.com/nsgo-dev/nsgo/core"
)

// A pocketd shell can report a directory on the service host. Feeding that
// directory to a desktop terminal would make Ctrl-click resolve paths locally.
type remoteOutput struct {
	io.Reader
	parser          *ansi.Parser
	header, discard bool
}

func newRemoteOutput(r io.Reader) *remoteOutput {
	p := ansi.NewParser()
	p.SetDataSize(32) // The parser needs only the numerical OSC selector.
	return &remoteOutput{Reader: r, parser: p}
}

func (r *remoteOutput) Read(buf []byte) (int, error) {
	for {
		n, err := r.Reader.Read(buf)
		w := 0
		for _, b := range buf[:n] {
			before := r.parser.State()
			r.parser.Advance(b)
			after := r.parser.State()
			switch {
			case before != parser.OscStringState && after == parser.OscStringState:
				r.header, r.discard = true, false
			case after != parser.OscStringState:
				r.discard = false
			case r.header && b == ';':
				r.header = false
				switch r.parser.Command() {
				case 0, 1, 2, 4, 8, 10, 11, 12, 104, 110, 111, 112:
					// Titles, palette and hyperlinks. Scope checks each opened URL.
				default:
					r.discard = true
					buf[w] = 0x18 // ANSI CAN cancels the buffered selector.
					w++
				}
			}
			if !r.discard {
				buf[w] = b
				w++
			}
		}
		if w > 0 || n == 0 || err != nil {
			return w, err
		}
	}
}

type remoteConn struct {
	*state.TermConn
	output *remoteOutput
}

func (c remoteConn) Read(p []byte) (int, error) { return c.output.Read(p) }

type terminalAccess struct{}

func (terminalAccess) Allows(r core.AccessRequest) bool {
	return r.Permission != core.AccessOpenURL || state.SafeLink(r.Resource)
}
func (terminalAccess) WebHosts() []string { return nil }
