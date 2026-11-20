package app

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/egoist/mygo/plugins/terminal"
	"github.com/egoist/mygo/ui"

	"github.com/frostyard/pocketgo/desktop/internal/pocketdtest"
	"github.com/frostyard/pocketgo/proto"
)

// TestMain points the terminal plugin at a libghostty-vt already in the
// user's cache, or at none: unit tests download nothing, and the tests
// that need the emulator skip without it (go run ./cmd/pocket downloads
// it once).
func TestMain(m *testing.M) {
	if os.Getenv("MYGO_GHOSTTY_VT") == "" {
		lib := filepath.Join(os.TempDir(), "pocketgo-tests-have-no-libghostty-vt")
		if dir, err := os.UserCacheDir(); err == nil {
			if found, _ := filepath.Glob(filepath.Join(dir, "mygo", "natives", "*", "libghostty-vt.*")); len(found) > 0 {
				lib = found[0]
			}
		}
		_ = os.Setenv("MYGO_GHOSTTY_VT", lib)
	}
	os.Exit(m.Run())
}

// emulator skips a test when the terminal's library is not there.
func emulator(t *testing.T) {
	t.Helper()
	if err := terminal.Load(); err != nil {
		t.Skipf("the terminal library does not load: %v", err)
	}
}

// untilDrawn runs the loop and draws until cond holds: what the window
// derives as it draws, as the shell shown, follows the data.
func (x *testApp) untilDrawn(t *testing.T, cond func() bool) {
	t.Helper()
	x.loop.RunUntil(t, func() bool {
		x.tt.Advance(20 * time.Millisecond)
		x.tt.Frame()
		return cond()
	})
	x.settle()
}

func shell(id, cwd string) proto.Terminal {
	return proto.Terminal{ID: id, Title: filepath.Base(cwd), Cwd: cwd, StartedAt: pocketdtest.Now}
}

// TestTerminalOptionsKeepTheServerOffTheDesktop is frostyard/pocket#3's
// guard, blocking point 1: what a shell on the server prints may not write
// the desktop's clipboard (OSC 52), nor open links other than web and mail
// ones. It needs no emulator library.
func TestTerminalOptionsKeepTheServerOffTheDesktop(t *testing.T) {
	x := newTestApp(t)
	opened := make(chan string, 4)
	x.a.opts.OpenURL = func(u string) error { opened <- u; return nil }
	opts := x.a.termOptions(nil)
	if !opts.NoClipboardWrite {
		t.Error("the server's output may write the desktop's clipboard")
	}
	if opts.ClipboardRead != terminal.ClipboardDeny {
		t.Error("the server's programs may read the desktop's clipboard")
	}
	if opts.OpenLink == nil {
		t.Fatal("links from the server open with the system, whatever their scheme")
	}
	if !opts.OpenLink("file:///etc/passwd") {
		t.Error("a file link is left to the system to open")
	}
	if !opts.OpenLink("https://example.org/x") {
		t.Error("a web link is left to the system")
	}
	if got := <-opened; got != "https://example.org/x" {
		t.Errorf("opened %q", got)
	}
	select {
	case got := <-opened:
		t.Errorf("opened %q too", got)
	default:
	}
	x.settle()
	if !x.tt.HasText("Only web and mail links open from a terminal") {
		t.Errorf("no word of the refused link in %q", x.tt.Texts())
	}
}

// The sidebar's Terminal entry counts the shells that run; the place shows
// a tab for each, the newest chosen, its title the shell's own, + for a
// new shell, and how to copy and paste.
func TestTheTerminalPlaceSnapshot(t *testing.T) {
	x := newTestApp(t)
	x.seed()
	x.srv.AddTerminal(shell("s1", pocketdtest.HomeFolder))
	x.srv.AddTerminal(shell("s2", "/home/team/projects/pocketgo"))
	x.srv.PrintTerminal("s2", "team@team:~/projects/pocketgo$ ls\r\ndesktop  docs  proto  service\r\nteam@team:~/projects/pocketgo$ ")
	x.connect(t)
	if !x.tt.HasText("Terminal") || !x.tt.HasText("2") {
		t.Fatalf("the sidebar lacks the Terminal entry and its count: %q", x.tt.Texts())
	}
	x.click(t, "Terminal")
	if x.a.place != placeTerminal || x.a.terms.sel != "s2" {
		t.Fatalf("place %v, shown %q", x.a.place, x.a.terms.sel)
	}
	hint := "Ctrl Shift C copy · Ctrl Shift V paste"
	if runtime.GOOS == "darwin" {
		hint = "⌘C copy · ⌘V paste"
	}
	x.hasAll(t, "team", "pocketgo", hint)
	if terminal.Load() != nil {
		x.hasAll(t, "The terminal emulator did not load: "+x.a.terms.libErr)
		x.snapshot(t, "terminal")
		return
	}
	x.untilDrawn(t, func() bool {
		ot := x.a.terms.open["s2"]
		return ot != nil && strings.Contains(ot.term.Text(), "desktop  docs  proto  service")
	})
	// The shell sets its title as it prompts: the tab takes it.
	x.srv.PrintTerminal("s2", "\x1b]0;team@team: ~/projects/pocketgo\x07")
	x.untilDrawn(t, func() bool { return x.a.terms.open["s2"].title != "" })
	x.hasAll(t, "team@team: ~/projects/pocke…")
	x.snapshot(t, "terminal")
}

// With no shell, the place says so and starts one in the home folder; the
// start carries its request id, and the new shell shows at once, whether
// its event or the start's answer comes first.
func TestANewTerminalStartsFromTheEmptyPlaceSnapshot(t *testing.T) {
	x := newTestApp(t)
	x.connect(t)
	x.click(t, "Terminal")
	x.hasAll(t, "No terminal", "A shell on the server, as you. Start one here.", "New Terminal")
	x.snapshot(t, "terminal-empty")
	release := x.srv.HoldAnswerOnce(http.MethodPost, proto.PathTerminals)
	x.click(t, "New Terminal")
	release()
	x.untilDrawn(t, func() bool { return x.a.terms.sel == "s1" && len(x.a.st.Data.Terminals()) == 1 })
	posts := x.srv.RequestsTo(http.MethodPost, proto.PathTerminals)
	var start proto.StartTerminal
	if len(posts) != 1 || json.Unmarshal(posts[0].Body, &start) != nil || start.RequestID == "" ||
		start.Folder != proto.TerminalHome || start.Rows != 30 || start.Cols != 100 {
		t.Fatalf("started with %s", posts[0].Body)
	}
	if x.tt.HasText("No terminal") {
		t.Error("the place says no shell runs")
	}
}

// A place whose list did not load says so, never that no shell runs, and
// loads it again.
func TestATerminalListThatFailedToLoadSaysSo(t *testing.T) {
	x := newTestApp(t)
	x.srv.FailOnce(http.MethodGet, proto.PathTerminals, http.StatusBadGateway)
	x.connect(t)
	// The place loads the list again as it opens; that fails too.
	x.srv.FailOnce(http.MethodGet, proto.PathTerminals, http.StatusBadGateway)
	x.click(t, "Terminal")
	x.untilDrawn(t, func() bool { return x.a.st.InFlight() == 0 })
	x.hasAll(t, "Try Again")
	if x.tt.HasText("No terminal") || !strings.Contains(strings.Join(x.tt.Texts(), " "), "Loading the terminals failed") {
		t.Fatalf("the place shows %q", x.tt.Texts())
	}
	x.srv.AddTerminal(shell("s1", pocketdtest.HomeFolder))
	x.click(t, "Try Again")
	x.untilDrawn(t, func() bool { return x.a.terms.sel == "s1" })
	if x.tt.HasText("Try Again") {
		t.Error("the failure still shows after the list loaded")
	}
}

// A shell ended elsewhere leaves the window, and the newest other shows; a
// tab's end ends its shell on pocketd.
func TestEndingATerminal(t *testing.T) {
	x := newTestApp(t)
	x.srv.AddTerminal(shell("s1", pocketdtest.HomeFolder))
	x.srv.AddTerminal(shell("s2", pocketdtest.HomeFolder))
	x.connect(t)
	x.click(t, "Terminal")
	if x.a.terms.sel != "s2" {
		t.Fatalf("shown %q", x.a.terms.sel)
	}
	x.srv.EndTerminal("s2")
	x.untilDrawn(t, func() bool { return x.a.terms.sel == "s1" })
	if _, open := x.a.terms.open["s2"]; open {
		t.Error("an ended shell stays open")
	}
	x.click(t, "End this terminal")
	x.untilDrawn(t, func() bool { return len(x.a.st.Data.Terminals()) == 0 && x.a.st.InFlight() == 0 })
	if got := x.srv.RequestsTo(http.MethodDelete, proto.Path(proto.PathTerminal, "s1")); len(got) != 1 {
		t.Fatalf("%d ends of s1", len(got))
	}
	// Without the emulator, the place says so first, as pocket's does.
	if terminal.Load() == nil {
		x.hasAll(t, "No terminal")
	}
	if len(x.a.terms.open) != 0 || x.a.terms.sel != "" {
		t.Errorf("open %v, shown %q", x.a.terms.open, x.a.terms.sel)
	}
}

// The window's Ctrl keys that shells use stand aside while a terminal has
// the keys: Ctrl N is a shell's next command, not a new session.
func TestTheWindowsCtrlKeysStandAsideInATerminal(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("Cmd keys never reach a shell")
	}
	x := newTestApp(t)
	x.a.terms.focused = true
	for _, id := range shellKeys {
		if ok, why := x.a.table.Enabled(id); ok || why != "A terminal has the keys" {
			t.Errorf("%s: %v %q while a terminal has the keys", id, ok, why)
		}
	}
	x.a.terms.focused = false
	if ok, _ := x.a.table.Enabled("new"); !ok {
		t.Error("New Session stays off once the terminal lost the keys")
	}
}

// TestServerOutputCannotWriteTheClipboard feeds an OSC 52 write through a
// TermConn into Ghostty's emulator, as a shell on the server can print
// one, and checks that the desktop's clipboard keeps what it had with the
// options the app gives its terminals. A control run without the guard
// shows that the test sees such a write.
func TestServerOutputCannotWriteTheClipboard(t *testing.T) {
	emulator(t)
	x := newTestApp(t)
	x.connect(t)
	const write = "\x1b]52;c;ZnJvbS1zZXJ2ZXI=\x1b\\é🙂 printed\r\n" // "from-server"
	run := func(id string, control bool) string {
		x.srv.AddTerminal(shell(id, pocketdtest.HomeFolder))
		conn := x.a.st.OpenTerminal(id)
		opts := x.a.termOptions(conn)
		if control {
			opts.NoClipboardWrite = false
		}
		term, err := terminal.New(opts)
		if err != nil {
			t.Fatal(err)
		}
		defer term.Close()
		tt := ui.NewTester(func(c *ui.Context) { terminal.View(c, term).Fill().AutoFocus() }, 640, 400)
		// The shell prints once the desktop's clipboard holds something:
		// the Tester's first frame already reads the terminal.
		tt.SetClipboard("local-secret")
		x.srv.PrintTerminal(id, write)
		x.loop.RunUntil(t, func() bool {
			tt.Advance(20 * time.Millisecond)
			tt.Frame()
			return strings.Contains(term.Text(), "é🙂 printed")
		})
		for range 5 {
			tt.Advance(20 * time.Millisecond)
			tt.Frame()
		}
		return tt.Clipboard()
	}
	if got := run("control", true); got != "from-server" {
		t.Fatalf("control: without the guard the clipboard is %q; the test does not see an OSC 52 write", got)
	}
	if got := run("guarded", false); got != "local-secret" {
		t.Errorf("the server's output wrote the desktop's clipboard: %q", got)
	}
}

// The history pocketd kept is drawn off-screen first: the queries in it
// (a cursor report, the colors) send no answer to the live shell, and a
// query that comes live is answered.
func TestTerminalReplayDoesNotWriteHistoricalReplies(t *testing.T) {
	emulator(t)
	x := newTestApp(t)
	x.srv.AddTerminal(shell("s1", pocketdtest.HomeFolder))
	x.srv.PrintTerminal("s1", "history prompt\r\n\x1b[6n\x1b[c\x1b]10;?\x07\x1b[>0q\x1b[H")
	x.connect(t)
	ot := x.a.attach("s1")
	if ot == nil {
		t.Fatalf("attach failed: %s", x.a.terms.libErr)
	}
	t.Cleanup(func() { _ = ot.term.Close() })
	output := proto.Path(proto.PathTerminalOutput, "s1")
	// A second poll means the emulator took all of the history.
	x.loop.RunUntil(t, func() bool { return len(x.srv.RequestsTo(http.MethodGet, output)) >= 2 })
	if !strings.Contains(ot.term.Text(), "history prompt") {
		t.Fatal("history was not restored")
	}
	ot.term.Send([]byte("typed"))
	x.loop.RunUntil(t, func() bool { return strings.Contains(x.srv.TerminalKeys("s1"), "typed") })
	if got := x.srv.TerminalKeys("s1"); got != "typed" {
		t.Fatalf("history wrote into the live shell: %q", got)
	}
	x.srv.PrintTerminal("s1", "\x1b[6n")
	x.loop.RunUntil(t, func() bool { return strings.HasPrefix(x.srv.TerminalKeys("s1"), "typed\x1b[") })
	if keys := x.srv.TerminalKeys("s1"); !strings.HasSuffix(keys, "R") {
		t.Fatalf("a live query went unanswered: %q", keys)
	}
}
