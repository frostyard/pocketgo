package nsgoclient

import (
	"io"
	"sync"

	"github.com/frostyard/pocketgo/desktop/internal/state"
	"github.com/frostyard/pocketgo/desktop/workspaceui"
	"github.com/nsgo-dev/nsgo/base/terminal"
	"github.com/nsgo-dev/nsgo/core"
	"github.com/nsgo-dev/nsgo/ui"
)

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
func (c *client) closeTerminals() {
	for id, t := range c.terminals {
		_ = t.Close()
		delete(c.terminals, id)
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
func (c *client) terminalPage(cx *core.Scope) core.Node {
	selected := core.NewMemo(cx, func() string { c.revision.Get(); return c.store.Data.TerminalSelection(c.termID.Get()) })
	ready := core.NewMemo(cx, func() string {
		c.revision.Get()
		id := selected.Get()
		if id != "" && c.store.AttachReady(id) {
			return id
		}
		return ""
	})
	return core.View{Style: ui.FillRest, Children: []core.Node{
		core.Dynamic(func(*core.Scope) core.Node {
			c.revision.Get()
			can, why := c.store.CanStartTerminal()
			items := []core.Node{ui.Button{Text: "New terminal", Disabled: func() bool { return !can }, OnPress: func() { c.startTerminal(state.TerminalPlace{}) }}}
			for _, t := range c.store.Data.Terminals() {
				items = append(items, c.button(t.Title, func() { c.termID.Set(t.ID) }))
			}
			if id := selected.Get(); id != "" {
				items = append(items, c.button("End terminal", func() {
					if t := c.terminals[id]; t != nil {
						_ = t.Close()
						delete(c.terminals, id)
					}
					c.store.EndTerminal(id)
				}))
			}
			if why != "" {
				items = append(items, body(why))
			}
			return workspaceui.Row(items...)
		}),
		core.Dynamic(func(cx *core.Scope) core.Node {
			cx.ProvideAccess(terminalAccess{})
			id := ready.Get()
			if id == "" {
				return workspaceui.Page(body("Start a terminal to open a shell on the service host."))
			}
			term := c.terminals[id]
			if term == nil {
				conn := c.store.OpenTerminal(id)
				if conn == nil {
					return core.View{}
				}
				if !c.store.TakeFresh(id) {
					conn.Replay = replay
				}
				var err error
				term, err = terminal.New(terminal.Options{Conn: remoteConn{TermConn: conn, output: newRemoteOutput(conn)}})
				if err != nil {
					_ = conn.Close()
					return body(err.Error())
				}
				c.terminals[id] = term
			}
			return terminal.View{Terminal: term, AutoFocus: true, Label: "Terminal input", Style: ui.FillRest, TestID: "remote-terminal"}
		}),
	}}
}
