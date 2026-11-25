package nsgoclient

import (
	"fmt"
	"net/http"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/frostyard/pocketgo/desktop/internal/pocketdtest"
	"github.com/frostyard/pocketgo/desktop/internal/state"
	"github.com/frostyard/pocketgo/proto"
	"github.com/nsgo-dev/nsgo/core"
	"github.com/nsgo-dev/nsgo/nsgotest"
)

// The Terminal place says and does in the NSGo client what it says and
// does in the MyGo client: these hold, for the NSGo client, the cases and
// the texts of the MyGo client's terminals_test.go. The clipboard's rule
// is held in client_test.go and terminal_output_test.go.

// shell is a shell pocketd lists, running in cwd.
func shell(id, cwd string) proto.Terminal {
	return proto.Terminal{ID: id, Title: filepath.Base(cwd), Cwd: cwd, StartedAt: pocketdtest.Now}
}

// openTerminals goes to the Terminal place, and waits for the shell shown.
func openTerminals(app *nsgotest.App) {
	app.Press("sidebar-panel >> text:Terminal")
	app.WaitFor("id:remote-terminal-screen", 10*time.Second)
}

// drawn prints output to shell id, and waits until the emulator shown has
// read past it: it answers the cursor report that follows.
func drawn(app *nsgotest.App, s *pocketdtest.Server, id, output string) {
	reports := strings.Count(s.TerminalKeys(id), "R")
	s.PrintTerminal(id, output+"\x1b[6n")
	app.WaitUntil(5*time.Second, func() bool { return strings.Count(s.TerminalKeys(id), "R") > reports })
}

// The place shows a tab for each shell, the newest chosen, named by its
// folder until its shell sets a title, its folder as the tab's tooltip,
// and how to copy and paste.
func TestTheTerminalPlace(t *testing.T) {
	s := pocketdtest.New()
	s.AddTerminal(shell("s1", pocketdtest.HomeFolder))
	s.AddTerminal(shell("s2", "/home/team/projects/pocketgo"))
	s.PrintTerminal("s2", "team@team:~/projects/pocketgo$ ls\r\ndesktop  docs  proto  service\r\nteam@team:~/projects/pocketgo$ ")
	app := signedInApp(t, s, 1230, 800)
	openTerminals(app)
	app.Find("label:team")
	app.Find("label:pocketgo")
	hint := "Ctrl Shift C copy · Ctrl Shift V paste"
	if runtime.GOOS == "darwin" || runtime.GOOS == "ios" {
		hint = "⌘C copy · ⌘V paste"
	}
	app.Find("text:" + hint)
	// The newest shell shows: its keys go to it.
	app.Type("label:Terminal input", "a")
	app.WaitUntil(5*time.Second, func() bool { return strings.Contains(s.TerminalKeys("s2"), "a") })
	if keys := s.TerminalKeys("s1"); keys != "" {
		t.Errorf("the shell that does not show took %q", keys)
	}
	// The shell sets its title as it prompts: the tab takes it, cut to
	// the tab's room, and its tooltip says where the shell runs.
	const title = "team@team: ~/projects/pocketgo"
	s.PrintTerminal("s2", "\x1b]0;"+title+"\x07")
	tab := app.WaitFor("label:"+title, 5*time.Second)
	if cut := state.Truncate(title, 28, false); cut == title || len(app.Query(fmt.Sprintf("view:%d >> text:%s", tab.ID, cut))) != 1 {
		t.Errorf("the tab does not show its title cut to %q", cut)
	}
	if app.Exists("label:pocketgo") {
		t.Error("the tab keeps its folder's name beside the title its shell set")
	}
	// Its tooltip describes it to assistive technology too.
	if got := app.Find("label:" + title).Props.Description; got != "/home/team/projects/pocketgo" {
		t.Errorf("the tab's tooltip is %q, want its shell's folder", got)
	}
	app.Screenshot("terminal")
}

// The title a shell set in its history names its tab, though only the
// history's screen reaches the emulator.
func TestAReplayedTitleNamesTheTab(t *testing.T) {
	s := pocketdtest.New()
	s.AddTerminal(shell("s1", pocketdtest.HomeFolder))
	s.PrintTerminal("s1", "\x1b]0;team@team: ~/x\x07team@team:~/x$ ")
	app := signedInApp(t, s, 1230, 800)
	openTerminals(app)
	app.WaitFor("label:team@team: ~/x", 5*time.Second)
}

// A shell that exited says so, with its code, and Close ends it on
// pocketd.
func TestAShellThatExitedSaysSo(t *testing.T) {
	s := pocketdtest.New()
	s.AddTerminal(shell("s1", pocketdtest.HomeFolder))
	app := signedInApp(t, s, 1230, 800)
	openTerminals(app)
	if app.Exists("id:terminal-end") {
		t.Fatal("a shell that runs says it ended")
	}
	s.ExitTerminal("s1", 3)
	app.WaitFor("id:terminal-end >> text:"+state.TerminalEndNote(3), 5*time.Second)
	app.Find("text:The shell exited (code 3).")
	app.Screenshot("terminal-ended")
	app.Press("id:terminal-end >> role:button Close")
	app.WaitUntil(5*time.Second, func() bool { return len(s.Terminals()) == 0 })
	if got := s.RequestsTo(http.MethodDelete, proto.Path(proto.PathTerminal, "s1")); len(got) != 1 {
		t.Fatalf("%d ends of s1", len(got))
	}
	app.WaitFor("text:Start a terminal to open a shell on the service host.", 5*time.Second)
	app.WaitForGone("id:terminal-end", 5*time.Second)
}

// A shell whose connection ended while pocketd still runs it says it is
// disconnected, and connects again when the owner chooses it, rather
// than leaving them only its end.
func TestAShellWhoseConnectionEndedConnectsAgain(t *testing.T) {
	s := pocketdtest.New()
	s.AddTerminal(shell("s1", pocketdtest.HomeFolder))
	app := signedInApp(t, s, 1230, 800)
	openTerminals(app)
	// pocketd refuses the keys for good (409, as for a 17th client) while
	// the shell runs on.
	s.FailOnce(http.MethodPost, proto.Path(proto.PathTerminalInput, "s1"), http.StatusConflict)
	app.Type("label:Terminal input", "x")
	app.WaitFor("id:terminal-end >> text:"+state.TerminalEndNote(-1), 5*time.Second)
	app.Find("text:Disconnected from the shell.")
	app.Press("label:team")
	app.WaitForGone("id:terminal-end", 5*time.Second)
	app.WaitFor("id:remote-terminal-screen", 10*time.Second)
	app.Type("label:Terminal input", "y")
	app.WaitUntil(5*time.Second, func() bool { return strings.Contains(s.TerminalKeys("s1"), "y") })
	if len(s.Terminals()) != 1 || len(s.RequestsTo(http.MethodDelete, proto.Path(proto.PathTerminal, "s1"))) != 0 {
		t.Fatal("the shell did not connect again, or it ended")
	}
}

// A shell ended elsewhere leaves the window, and the newest other shows;
// End terminal ends the one shown on pocketd, and the newest other shows
// at once, before pocketd answers.
func TestEndingATerminal(t *testing.T) {
	s := pocketdtest.New()
	for _, id := range []string{"s1", "s2", "s3"} {
		s.AddTerminal(shell(id, pocketdtest.HomeFolder+"/"+id))
	}
	end := proto.Path(proto.PathTerminal, "s2")
	held := &heldRequest{RoundTripper: s.Transport(), method: http.MethodDelete, path: end, release: make(chan struct{})}
	app := signedInOver(t, held, 1230, 800)
	openTerminals(app)
	// shown checks that shell id shows: the keys typed go to it alone.
	typed := 0
	shown := func(id string) {
		t.Helper()
		typed++
		key := string(rune('a' + typed))
		app.WaitFor("id:remote-terminal-screen", 10*time.Second)
		app.Type("label:Terminal input", key)
		app.WaitUntil(5*time.Second, func() bool { return strings.Contains(s.TerminalKeys(id), key) })
		for _, other := range s.Terminals() {
			if other.ID != id && strings.Contains(s.TerminalKeys(other.ID), key) {
				t.Fatalf("%s took the keys typed while %s shows", other.ID, id)
			}
		}
	}
	shown("s3")
	s.EndTerminal("s3")
	app.WaitForGone("label:s3", 5*time.Second)
	shown("s2")
	app.Press("text:End terminal")
	// pocketd has not heard of the end yet, and lists s2: s1 shows.
	shown("s1")
	if got := s.RequestsTo(http.MethodDelete, end); len(got) != 0 || len(s.Terminals()) != 2 {
		t.Fatalf("pocketd heard %d ends of s2 and lists %d shells while the end is held back", len(got), len(s.Terminals()))
	}
	close(held.release)
	app.WaitUntil(5*time.Second, func() bool { return len(s.Terminals()) == 1 })
	if got := s.RequestsTo(http.MethodDelete, end); len(got) != 1 {
		t.Fatalf("%d ends of s2", len(got))
	}
	app.WaitForGone("label:s2", 5*time.Second)
	app.Press("text:End terminal")
	app.WaitUntil(5*time.Second, func() bool { return len(s.Terminals()) == 0 })
	app.WaitFor("text:Start a terminal to open a shell on the service host.", 5*time.Second)
}

// A link a shell printed opens only when it is a web or a mail link
// (state.SafeLink): another is refused, and the window says so.
func TestOnlyWebAndMailLinksOpenFromATerminal(t *testing.T) {
	var refused []string
	access := terminalAccess{refused: func(link string) { refused = append(refused, link) }}
	for link, opens := range map[string]bool{
		"https://example.org/x": true, "mailto:team@example.org": true,
		"gemini://example.org/x": false, "ssh://example.org": false, "smb://example.org/share": false,
	} {
		if got := access.Allows(core.AccessRequest{Permission: core.AccessOpenURL, Resource: link}); got != opens {
			t.Errorf("%s opens: %v, want %v", link, got, opens)
		}
	}
	if len(refused) != 3 {
		t.Errorf("the window heard of %q, want the 3 links refused", refused)
	}
	// Nothing else a view asks is refused here.
	if !access.Allows(core.AccessRequest{Permission: core.AccessClipboard}) {
		t.Error("the terminal's access refuses the person's own copy and paste")
	}

	s := pocketdtest.New()
	s.AddTerminal(shell("s1", pocketdtest.HomeFolder))
	app := signedInApp(t, s, 1230, 800)
	openTerminals(app)
	// No app has gemini's scheme on a test machine, and the client asks
	// none to open it.
	drawn(app, s, "s1", "\x1b[2J\x1b[Hgemini://example.org/x")
	// The link's first cells, in the screen's first row; the key that
	// opens links is held, Control or Command. A Mac's window takes a
	// press with Control held as a secondary click, which opens the
	// terminal's menu, so there Command alone is held.
	held := core.Modifiers{Control: true, Platform: true}
	if nsgotest.Target() == "macos" {
		held = core.Modifiers{Platform: true}
	}
	app.PressAtWith("id:remote-terminal-screen", core.Point{X: 30, Y: 10}, held)
	app.WaitFor("text:Only web and mail links open from a terminal", 5*time.Second)
	if len(app.OpenedURLs) != 0 {
		t.Errorf("the client opened %q", app.OpenedURLs)
	}
}
