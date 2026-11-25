package nsgoclient

import (
	"io"
	"strings"
	"testing"
	"testing/iotest"
)

func TestRemoteOutputKeepsDisplayAndLinksLocalToTheSession(t *testing.T) {
	const visible = "界📝\x1b[32mhello\x1b[0m\x1b]8;;https://example.org\x07site\x1b]8;;\x07"
	r := newRemoteOutput(iotest.OneByteReader(strings.NewReader("\x1b]7;file://localhost/workspace\x07" + visible)))
	b, err := io.ReadAll(r)
	if err != nil || strings.Contains(string(b), "/workspace") || !strings.HasSuffix(string(b), visible) {
		t.Fatalf("filtered terminal output %q, %v", b, err)
	}
}

// TestRemoteOutputDropsClipboardSequences checks that what a shell on the
// service host prints to write or read the clipboard (OSC 52, and Kitty's
// 5522) does not reach the emulator, whole or a byte at a time, and that
// the text around it does.
func TestRemoteOutputDropsClipboardSequences(t *testing.T) {
	const payload = "ZnJvbS1zZXJ2ZXI=" // "from-server"
	for _, sequence := range []string{
		"\x1b]52;c;" + payload + "\x1b\\",
		"\x1b]52;c;" + payload + "\x07",
		"\x1b]52;c;?\x07",
		"\x1b]5522;type=write;" + payload + "\x1b\\",
	} {
		for _, read := range []func(io.Reader) io.Reader{func(r io.Reader) io.Reader { return r }, iotest.OneByteReader} {
			b, err := io.ReadAll(newRemoteOutput(read(strings.NewReader("before" + sequence + "after"))))
			out := string(b)
			if err != nil || !strings.HasPrefix(out, "before") || !strings.HasSuffix(out, "after") {
				t.Errorf("output around %q came out as %q, %v", sequence, out, err)
			}
			if strings.Contains(out, payload) || strings.Contains(out, ";") {
				t.Errorf("%q reached the emulator as %q", sequence, out)
			}
		}
	}
}

// TestTerminalViewLeavesTheClipboardOff checks the view's own rule, which
// holds if the filter above ever lets a sequence through.
func TestTerminalViewLeavesTheClipboardOff(t *testing.T) {
	if v := terminalView(nil); v.AllowClipboardRead || v.AllowClipboardWrite {
		t.Errorf("the terminal's view lets a program read (%v) or write (%v) the clipboard", v.AllowClipboardRead, v.AllowClipboardWrite)
	}
}
