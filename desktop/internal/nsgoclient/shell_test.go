package nsgoclient

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/frostyard/pocketgo/desktop/internal/pocketdtest"
	"github.com/frostyard/pocketgo/proto"
)

// TestShellIsTheKitsStandard checks the window's shell: the navigation's
// foot says where the app is connected and leads to Settings, the toolbar
// is NSGo's with the way back and forward and Refresh, and the palette is
// a field that opens it.
func TestShellIsTheKitsStandard(t *testing.T) {
	service := pocketdtest.New()
	service.AddSession(proto.Session{ID: 7, Title: "Release checklist", State: proto.SessionIdle})
	app := signedInApp(t, service, 1230, 800)
	app.Find("id:connection >> text:" + pocketdtest.URL)
	for _, sel := range []string{"role:toolbar >> label:Back", "role:toolbar >> label:Forward", "role:toolbar >> label:Refresh", "label:Filter sessions", "label:Settings"} {
		app.Find(sel)
	}
	if app.Exists("role:toolbar >> label:Settings") {
		t.Error("Settings is in the toolbar, not at the navigation's foot")
	}
	app.Press("label:" + paletteLabel)
	app.WaitFor("label:Find command", 3*time.Second)
	app.Key("Escape")
	app.WaitForGone("label:Find command", 3*time.Second)
	app.Key("mod+k")
	app.WaitFor("label:Find command", 3*time.Second)
}

// TestNarrowWindowKeepsThePalette checks that a window with no room for
// the palette's field keeps the palette as a button of the same name.
func TestNarrowWindowKeepsThePalette(t *testing.T) {
	app := signedInApp(t, pocketdtest.New(), 760, 730)
	if width := app.Find("label:" + paletteLabel).Bounds().Size.Width; width > 44 {
		t.Errorf("the palette's control is %v wide in a window 760 wide: want an icon button", width)
	}
	app.Press("label:" + paletteLabel)
	app.WaitFor("label:Find command", 3*time.Second)
}

// TestConfirmationStartsOnCancel checks a confirmation: NSGo's alert
// dialog, whose keyboard starts on Cancel, so Return alone does not run
// the action, and whose action does what was asked.
func TestConfirmationStartsOnCancel(t *testing.T) {
	app := signedInApp(t, pocketdtest.New(), 1230, 800)
	ask := func() {
		t.Helper()
		app.Press("label:Settings")
		app.Press("id:settings-nav-connection")
		app.Press("text:Disconnect")
		app.WaitFor("role:alertdialog >> text:Disconnect?", 3*time.Second)
	}
	ask()
	app.Key("Enter")
	app.WaitForGone("role:alertdialog", 3*time.Second)
	if app.Exists("label:Service URL") {
		t.Fatal("Return in a confirmation ran its action: the client signed out")
	}
	app.Find("text:Reconnect")
	app.Press("label:Close settings")
	ask()
	app.Press("role:alertdialog >> role:button Disconnect")
	app.WaitFor("label:Service URL", 5*time.Second)
}

// TestNoLiteralLooks holds rule 2 of the kit's standard for an app
// (frostui's docs/specs/workspaceui.md) for this client: its code takes a
// kit part, an NSGo control or a token, and writes no look as a number: no
// color, text size, corner or padding. It reads the package's own files.
func TestNoLiteralLooks(t *testing.T) {
	if runtime.GOOS == "js" {
		t.Skip("a browser has no files of the client to read; the Linux runs hold the rule")
	}
	never := []struct {
		what string
		re   *regexp.Regexp
	}{
		{"a color written as a number", regexp.MustCompile(`core\.RGBA?\(|\b0x[0-9a-fA-F]{6,8}\b`)},
		{"a text size written as a number", regexp.MustCompile(`FontSize\s*[:=]\s*[0-9]|ui\.Text\(.*,\s*[0-9]+,\s*ui\.`)},
		{"a corner written as a number", regexp.MustCompile(`CornerRadius:\s*[0-9]`)},
		{"a padding written as a number", regexp.MustCompile(`Padding:\s*core\.(All|Symmetric|Edges)[({][^)}]*\b[0-9]`)},
	}
	// The working directory is the package's under go test, and a
	// directory below it on a target: find the repository from it.
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.work")); err == nil {
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.work above the working directory")
		}
		dir = parent
	}
	files, err := filepath.Glob(filepath.Join(dir, "desktop", "internal", "nsgoclient", "*.go"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no source files of the client under %s: %v", dir, err)
	}
	for _, path := range files {
		name := filepath.Base(path)
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for n, line := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "//") {
				continue
			}
			for _, rule := range never {
				if rule.re.MatchString(line) {
					t.Errorf("%s:%d: %s: take a token of the kit or of NSGo", name, n+1, rule.what)
				}
			}
		}
	}
	for _, bad := range []string{"Padding: core.All(8)", "Padding: core.Symmetric(workspaceui.SpaceXS, 6)", "CornerRadius: 7", "ui.Text(label, 11, ui.Semibold, ink)"} {
		refused := false
		for _, rule := range never {
			refused = refused || rule.re.MatchString(bad)
		}
		if !refused {
			t.Errorf("the rules miss %q", bad)
		}
	}
}
