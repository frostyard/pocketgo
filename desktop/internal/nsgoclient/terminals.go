package nsgoclient

import (
	"io"
	"reflect"
	"runtime"
	"sync"

	"github.com/frostyard/frostui/workspaceui"
	"github.com/frostyard/pocketgo/desktop/internal/state"
	"github.com/frostyard/pocketgo/proto"
	"github.com/nsgo-dev/nsgo/base/terminal"
	"github.com/nsgo-dev/nsgo/core"
	"github.com/nsgo-dev/nsgo/icons/lucide"
	"github.com/nsgo-dev/nsgo/ui"
)

// The Terminal place (ADR-0025): the shells pocketd runs for the owner on
// the service host, each named by its shell's title, and the one chosen,
// in NSGo's terminal view over a state.TermConn. Which shells are listed,
// which one shows, what a shell is called and which links open are
// state's rules; this draws them.

// openTerm is a shell the window shows: its emulator, the title its shell
// set, and how its connection ended.
type openTerm struct {
	*terminal.Terminal
	title  string
	exited bool
	code   int
}

// Historical terminal queries go to a disposable emulator. Only its rendered
// screen and scrollback reach the live connection when a window reattaches.
type replaySink struct {
	done chan struct{}
	once sync.Once
}

func (s *replaySink) Read([]byte) (int, error)    { <-s.done; return 0, io.EOF }
func (s *replaySink) Write(p []byte) (int, error) { return len(p), nil }
func (s *replaySink) Close() error                { s.once.Do(func() { close(s.done) }); return nil }

func replay(data []byte, cols, rows int) ([]byte, error) {
	if len(data) == 0 {
		return nil, nil
	}
	tmp, err := terminal.New(terminal.Options{Conn: &replaySink{done: make(chan struct{})}})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tmp.Close() }()
	tmp.Resize(cols, rows)
	tmp.Feed(data)
	m, err := tmp.NewMirror()
	if err != nil {
		return nil, err
	}
	defer m.Close()
	frame, _ := m.Frame()
	return append(tmp.History(), frame...), nil
}

// closeTerminal stops showing shell id: pocketd ends it a minute later
// unless something polls it again.
func (c *client) closeTerminal(id string) {
	if ot := c.terminals[id]; ot != nil {
		_ = ot.Close()
		delete(c.terminals, id)
	}
}

func (c *client) closeTerminals() {
	for id := range c.terminals {
		c.closeTerminal(id)
	}
}

func (c *client) startTerminal(place state.TerminalPlace) {
	c.store.StartTerminal(place, 30, 100, func(id string, err error) {
		if err != nil {
			c.result(err)
			return
		}
		c.termID.Set(id)
		c.navigate("Terminal")
	})
	c.changed()
}

// attach shows shell id: a connection to it, and an emulator over that.
// The title its shell sets names its tab, one set in the history pocketd
// kept too, which the replay's frame carries; its connection's end shows
// under it.
func (c *client) attach(id string) (*openTerm, error) {
	conn := c.store.OpenTerminal(id)
	if conn == nil {
		return nil, nil
	}
	ot := &openTerm{code: -1}
	// The emulator calls back on its own goroutines: what it says is kept
	// on the engine's, and only while the window still shows this
	// connection.
	keep := func(change func()) {
		c.cx.Post(func() {
			if c.terminals[id] == ot {
				change()
				c.changed()
			}
		})
	}
	// A shell this window just started has no history to replay: its
	// login's queries are answered live (state.Store.TakeFresh).
	if !c.store.TakeFresh(id) {
		conn.Replay = replay
	}
	term, err := terminal.New(terminal.Options{Conn: remoteConn{TermConn: conn, output: newRemoteOutput(conn)},
		OnTitle: func(title string) { keep(func() { ot.title = title }) },
		OnExit: func(int) {
			code := conn.ExitCode()
			keep(func() { ot.exited, ot.code = true, code })
		}})
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	ot.Terminal = term
	c.terminals[id] = ot
	return ot, nil
}

// terminalHint says how to copy and paste in a terminal.
func terminalHint() string {
	if runtime.GOOS == "darwin" || runtime.GOOS == "ios" {
		return "⌘C copy · ⌘V paste"
	}
	return "Ctrl Shift C copy · Ctrl Shift V paste"
}

// linkRefused says that a link a shell printed was not opened
// (state.SafeLink).
func (c *client) linkRefused(string) {
	c.cx.Post(func() {
		c.showNotice(ui.ToastWarning, "Link not opened", "Only web and mail links open from a terminal")
	})
}

func (c *client) terminalPage(cx *core.Scope) core.Node {
	d := &c.store.Data
	selected := core.NewMemo(cx, func() string { c.revision.Get(); return d.TerminalSelection(c.termID.Get()) })
	ready := core.NewMemo(cx, func() string {
		c.revision.Get()
		id := selected.Get()
		if id != "" && c.store.AttachReady(id) {
			return id
		}
		return ""
	})
	// attached counts the connections made anew to a shell that shows.
	attached := core.NewSignal(cx, 0)
	// end ends a shell on pocketd, and shows the newest other one.
	end := func(id string) {
		c.closeTerminal(id)
		c.store.EndTerminal(id)
		if selected.Get() == id {
			c.termID.Set(state.TerminalAfterEnd(d.Terminals(), id))
		}
		c.changed()
	}
	// choose shows shell t, with a new connection when its own ended
	// while pocketd still runs it (state.TerminalReconnects).
	choose := func(t proto.Terminal) {
		c.termID.Set(t.ID)
		if ot := c.terminals[t.ID]; ot != nil && state.TerminalReconnects(t, ot.exited) {
			c.closeTerminal(t.ID)
			attached.Update(func(n int) int { return n + 1 })
		}
		c.changed()
	}
	// The strip is drawn again only when what it shows changes, so a
	// press that began on a tab ends on it.
	type tab struct {
		info  proto.Terminal
		title string
	}
	type strip struct {
		tabs     []tab
		selected string
		can      bool
		why      string
	}
	shown := core.NewMemoFunc(cx, func() strip {
		c.revision.Get()
		v := strip{selected: selected.Get()}
		v.can, v.why = c.store.CanStartTerminal()
		for _, t := range d.Terminals() {
			set := ""
			if ot := c.terminals[t.ID]; ot != nil {
				set = ot.title
			}
			v.tabs = append(v.tabs, tab{t, state.TerminalTitle(t, set)})
		}
		return v
	}, func(a, b strip) bool { return reflect.DeepEqual(a, b) })
	// A shell that ended says how, with Close: the shell exited with a
	// code, or the connection to it was lost.
	ended := core.NewMemo(cx, func() string {
		c.revision.Get()
		id := ready.Get()
		ot := c.terminals[id]
		info, _ := d.Terminal(id)
		if ot == nil || !ot.exited && !info.Exited {
			return ""
		}
		code := ot.code
		if info.ExitCode != nil {
			code = *info.ExitCode
		}
		return state.TerminalEndNote(code)
	})
	return core.View{Style: ui.FillRest, Children: []core.Node{
		core.Dynamic(func(cx *core.Scope) core.Node {
			v := shown.Get()
			items := []core.Node{ui.Button{Text: "New terminal", Disabled: func() bool { return !v.can }, OnPress: func() { c.startTerminal(state.TerminalPlace{}) }}}
			for _, t := range v.tabs {
				variant := ui.ButtonGhost
				if t.info.ID == v.selected {
					variant = ui.ButtonSecondary
				}
				var named core.Node = ui.Button{Text: state.Truncate(t.title, 28, false), Label: t.title, Icon: lucide.Terminal, Variant: variant, OnPress: func() { choose(t.info) }}
				if t.info.Cwd != "" {
					named = ui.Tooltip{Text: t.info.Cwd, Trigger: named}
				}
				items = append(items, named)
			}
			if id := v.selected; id != "" {
				items = append(items, ui.Button{Text: "End terminal", Variant: ui.ButtonOutline, OnPress: func() { end(id) }})
			}
			if v.why != "" {
				items = append(items, body(v.why))
			}
			items = append(items, ui.Spacer{}, muted(cx, terminalHint))
			return core.View{TestID: "terminal-strip", Style: core.Style{Padding: core.Symmetric(workspaceui.SpaceS, workspaceui.SpaceM)}, Children: []core.Node{workspaceui.Row(items...)}}
		}),
		core.Dynamic(func(cx *core.Scope) core.Node {
			cx.ProvideAccess(terminalAccess{refused: c.linkRefused})
			attached.Get()
			id := ready.Get()
			if id == "" {
				// No shell to show: one is starting, the list failed or is
				// loading, or there is none.
				c.revision.Get()
				switch status, problem := d.TerminalsStatus(); {
				case d.TerminalAwaited(selected.Get()) || selected.Get() != "":
					return workspaceui.Page(body("Starting the terminal…"))
				case status == state.ListFailed:
					return workspaceui.Page(c.listProblem("the terminals", problem, "", c.store.LoadTerminals))
				case status == state.ListLoading:
					return workspaceui.Page(body("Loading the terminals…"))
				}
				return workspaceui.Page(body("Start a terminal to open a shell on the service host."))
			}
			ot := c.terminals[id]
			if ot == nil {
				var err error
				if ot, err = c.attach(id); err != nil {
					return body(err.Error())
				}
				if ot == nil {
					return core.View{}
				}
			}
			return terminalView(ot.Terminal)
		}),
		core.Show{When: func() bool { return ended.Get() != "" }, Then: func(cx *core.Scope) core.Node {
			note := core.Text{ContentFunc: ended.Get, Style: ui.TypeBody.Merge(core.Style{FlexGrow: 1, FlexShrink: 1, MinWidth: core.Pt(0)}), StyleFunc: ui.Colored(ui.ColorsOf(cx).Warn), Selectable: core.SelectableOn}
			return core.View{TestID: "terminal-end", Style: core.Style{Direction: core.Row, AlignItems: core.AlignCenter, Gap: workspaceui.SpaceS, Padding: core.Symmetric(workspaceui.SpaceS, workspaceui.SpaceM)},
				Children: []core.Node{note, c.button("Close", func() { end(ready.Get()) })}}
		}},
	}}
}

// terminalView is the view of a shell on the service host. It leaves
// NSGo's clipboard permissions off, so a program there can neither read
// nor write this device's clipboard (OSC 52); copy and paste are the
// person's own keys.
func terminalView(term *terminal.Terminal) terminal.View {
	return terminal.View{Terminal: term, AutoFocus: true, Label: "Terminal input", Style: ui.FillRest, TestID: "remote-terminal"}
}
