package app

import (
	"io"
	"runtime"

	"github.com/egoist/mygo/plugins/terminal"
	"github.com/egoist/mygo/ui"
	"github.com/frostyard/frostui"

	"github.com/frostyard/pocketgo/desktop/internal/state"
	"github.com/frostyard/pocketgo/proto"
)

// The Terminal place (ADR-0025), frostyard/pocket's: the shells pocketd
// runs for the owner on the service host, a tab each, and the one chosen,
// with Ghostty's emulator (MyGo's terminal plugin) over a state.TermConn.
// Which shells are listed, which one shows, and which links open are
// state's rules; this draws them.

// terms is the Terminal place's state: the shells the window shows.
type terms struct {
	open map[string]*openTerm
	sel  string
	// lib is where the emulator's library stands, libErr why it did not
	// load.
	lib    libState
	libErr string
	// field is the shown terminal's view, for its focus; focused is
	// whether it has the keys, read each frame: the window's Ctrl
	// shortcuts that shells use then stand aside.
	field   ui.Handle
	focused bool
}

// libState is where the emulator's library stands: it loads once, off the
// window's thread, since a first run downloads it (Codex review of #41,
// round 1).
type libState int

const (
	libNone libState = iota
	libLoading
	libLoaded
	libFailed
)

// loadEmulator loads the emulator's library, on a goroutine of its own,
// and tells the window when it is done.
func (a *App) loadEmulator() {
	if a.terms.lib != libNone {
		return
	}
	a.terms.lib = libLoading
	load := a.loadLib
	go func() {
		err := load()
		a.opts.Post(func() {
			if a.terms.lib != libLoading {
				return
			}
			if err != nil {
				a.terms.lib, a.terms.libErr = libFailed, err.Error()
				return
			}
			a.terms.lib = libLoaded
		})
	}()
}

// openTerm is a shell the window shows.
type openTerm struct {
	conn   *state.TermConn
	term   *terminal.Terminal
	title  string
	exited bool
	code   int
}

// goTerminal shows the Terminal place.
func (a *App) goTerminal() {
	a.place = placeTerminal
	a.st.Open(0)
	if status, _ := a.st.Data.TerminalsStatus(); status != state.ListLoaded {
		a.st.LoadTerminals()
	}
}

// syncTerminals follows the list: a shell that left it (ended elsewhere,
// or exited and dropped) leaves the window too, with its scrollback, and
// the one shown is one listed.
func (a *App) syncTerminals() {
	open := make([]string, 0, len(a.terms.open))
	for id := range a.terms.open {
		open = append(open, id)
	}
	for _, id := range a.st.Data.TerminalsGone(open) {
		a.closeTerminal(id)
	}
	a.terms.sel = a.st.Data.TerminalSelection(a.terms.sel)
}

// attach shows shell id: a connection to it, and an emulator over that,
// once the emulator's library loaded.
func (a *App) attach(id string) *openTerm {
	if ot := a.terms.open[id]; ot != nil {
		return ot
	}
	if a.terms.lib != libLoaded {
		a.loadEmulator()
		return nil
	}
	conn := a.st.OpenTerminal(id)
	if conn == nil {
		return nil
	}
	conn.Replay = terminalReplay
	ot := &openTerm{conn: conn, code: -1}
	opts := a.termOptions(conn)
	opts.OnTitle = func(title string) {
		a.opts.Post(func() { ot.title = title })
	}
	opts.OnExit = func(int) {
		code := conn.ExitCode()
		a.opts.Post(func() { ot.exited, ot.code = true, code })
	}
	term, err := terminal.New(opts)
	if err != nil {
		_ = conn.Close()
		a.terms.lib, a.terms.libErr = libFailed, err.Error()
		return nil
	}
	ot.term = term
	if a.terms.open == nil {
		a.terms.open = map[string]*openTerm{}
	}
	a.terms.open[id] = ot
	return ot
}

// termOptions are the emulator's options for a shell on pocketd's host.
// What the shell prints is the server's: it may not write the desktop's
// clipboard (OSC 52), which the next paste into a local terminal would
// run, and a link from it opens only on the web or in mail, never as a
// file or another program's address (state.SafeLink). Reads of the
// clipboard stay refused (the plugin's default).
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
	}
}

// openURL opens a web or mail link with the system, off the UI thread.
func (a *App) openURL(link string) {
	opener := frostui.Opener{Schemes: []string{"https", "http", "mailto"}, Launch: a.opts.OpenURL}
	go func() {
		if err := opener.OpenURL(link); err != nil {
			a.opts.Post(func() { a.toast("Opening the link: " + err.Error()) })
		}
	}()
}

// newTerminal starts a shell in the service user's home and shows it.
func (a *App) newTerminal() { a.startTerminal(nil) }

// startTerminal is newTerminal; then, when set, gets the new shell's id.
func (a *App) startTerminal(then func(id string)) {
	a.st.StartTerminal(state.TerminalPlace{}, 30, 100, func(id string, err error) {
		if err != nil {
			a.toast("Starting a terminal: " + err.Error())
			return
		}
		a.place = placeTerminal
		a.st.Open(0)
		a.terms.sel = id
		if then != nil {
			then(id)
		}
	})
}

// endTerminal ends a shell on pocketd, and shows the newest other one.
func (a *App) endTerminal(id string) {
	a.closeTerminal(id)
	a.st.EndTerminal(id)
	if a.terms.sel == id {
		a.terms.sel = state.TerminalAfterEnd(a.st.Data.Terminals(), id)
	}
}

// closeTerminal stops showing shell id: pocketd ends it a minute later
// unless something polls it again.
func (a *App) closeTerminal(id string) {
	if ot := a.terms.open[id]; ot != nil {
		_ = ot.term.Close()
		delete(a.terms.open, id)
	}
}

// closeTerminals stops showing every shell, as the sign-in ends or the
// window closes.
func (a *App) closeTerminals() {
	for id := range a.terms.open {
		a.closeTerminal(id)
	}
}

// terminalPane is the Terminal place: a strip of the shells, and the one
// chosen.
func (a *App) terminalPane(c *ui.Context) {
	t := c.Theme()
	d := &a.st.Data
	canStart, why := a.st.CanStartTerminal()
	ui.Column(c).Grow(1).AlignItems(ui.Stretch).Background(t.Background).Children(func() {
		ui.Row(c).Gap(frostui.SpaceXS).AlignItems(ui.Center).Padding(frostui.SpaceS, frostui.SpaceM).Background(frostui.Chrome(t)).Children(func() {
			for _, info := range d.Terminals() {
				a.terminalTab(c.Key(info.ID), info)
			}
			starting := a.st.StartingTerminal().Busy
			if starting {
				ui.Spinner(c).Size(14, 14)
			}
			label := "New terminal in the server's home folder"
			if !canStart && !starting {
				label = why
			}
			if frostui.IconButton(c, "list-add", label).Disabled(!canStart).Clicked() {
				a.newTerminal()
			}
			ui.Spacer(c)
			frostui.Caption(c, terminalHint())
		})
		frostui.RowDivider(c)
		info, found := d.Terminal(a.terms.sel)
		status, problem := d.TerminalsStatus()
		switch {
		case a.terms.libErr != "":
			frostui.Problem(c, "The terminal emulator did not load: "+a.terms.libErr).Margin(frostui.SpaceL)
		case !found && status == state.ListFailed:
			ui.Column(c).Grow(1).Padding(frostui.SpaceL).Gap(frostui.SpaceS).Children(func() {
				frostui.Problem(c, "Loading the terminals failed: "+problem).Wrap()
				if ui.Button(c, "Try Again").Clicked() {
					a.st.LoadTerminals()
				}
			})
		case !found && status == state.ListLoading:
			ui.Column(c).Grow(1).Children(func() {
				frostui.Loading(c, "Loading the terminals…").Grow(1)
			})
		case !found:
			ui.Column(c).Grow(1).Children(func() {
				frostui.Empty(c, "utilities-terminal", "No terminal", "A shell on the server, as you. Start one here.").Grow(1)
			})
			if len(d.Terminals()) == 0 {
				ui.Row(c).Padding(0, 0, frostui.SpaceXL, 0).Children(func() {
					ui.Spacer(c)
					if ui.PrimaryButton(c, "New Terminal").Disabled(!canStart).Clicked() {
						a.newTerminal()
					}
					ui.Spacer(c)
				})
			}
		default:
			ot := a.attach(info.ID)
			if ot == nil {
				ui.Column(c).Grow(1).Children(func() {
					frostui.Loading(c, "Loading the terminal…").Grow(1)
				})
				return
			}
			ui.Box(c).Grow(1).Padding(frostui.SpaceS).AlignItems(ui.Stretch).Children(func() {
				terminal.View(c.Key(info.ID), ot.term).Grow(1).AutoFocus().Bind(&a.terms.field)
			})
			if ot.exited || info.Exited {
				code := ot.code
				if info.ExitCode != nil {
					code = *info.ExitCode
				}
				ui.Row(c).Gap(frostui.SpaceS).AlignItems(ui.Center).Padding(frostui.SpaceS, frostui.SpaceM).Children(func() {
					frostui.Toned(c, state.TerminalEndNote(code), frostui.Warning).Grow(1)
					if ui.Button(c, "Close").Clicked() {
						a.endTerminal(info.ID)
					}
				})
			}
		}
	})
}

// terminalHint says how to copy and paste in a terminal.
func terminalHint() string {
	if runtime.GOOS == "darwin" {
		return "⌘C copy · ⌘V paste"
	}
	return "Ctrl Shift C copy · Ctrl Shift V paste"
}

func (a *App) terminalTab(c *ui.Context, info proto.Terminal) {
	t := c.Theme()
	sel := info.ID == a.terms.sel
	set := ""
	if ot := a.terms.open[info.ID]; ot != nil {
		set = ot.title
	}
	title := state.TerminalTitle(info, set)
	tab := ui.ButtonBase(c).Row().Gap(frostui.SpaceXS).AlignItems(ui.Center).Padding(4, frostui.SpaceS).Radius(6).
		Cursor(ui.CursorPointer).Label(title).AccessSelected(sel).Tooltip(info.Cwd)
	switch {
	case sel:
		tab.Background(t.Accent.Alpha(0.16))
	case tab.Hovered():
		tab.Background(t.SurfaceHover)
	}
	if tab.Clicked() {
		a.terms.sel = info.ID
	}
	tab.Children(func() {
		frostui.Icon(c, "utilities-terminal")
		if info.Exited {
			frostui.Muted(c, truncate(title, 28)).SingleLine()
		} else {
			ui.Text(c, truncate(title, 28)).SingleLine()
		}
		if frostui.IconButton(c, "window-close", "End this terminal").Clicked() {
			a.endTerminal(info.ID)
		}
	})
}

// shellKeys are the window's commands whose Ctrl keys shells use (Ctrl N
// for the next command in history, Ctrl Q): they stand aside while a
// terminal has the focus, where Ctrl stands for Cmd. On macOS they are Cmd
// keys, which never reach a shell.
var shellKeys = []string{"new", "sidebar", "quit"}

// guardShellKeys makes shellKeys stand aside while a terminal has the
// focus.
func (a *App) guardShellKeys() {
	if runtime.GOOS == "darwin" {
		return
	}
	for _, id := range shellKeys {
		cmd, found := a.table.Command(id)
		if !found {
			continue
		}
		enabled := cmd.Enabled
		cmd.Enabled = func() (bool, string) {
			if a.terms.focused {
				return false, "A terminal has the keys"
			}
			if enabled != nil {
				return enabled()
			}
			return true, ""
		}
		a.table.Add(cmd)
	}
}
