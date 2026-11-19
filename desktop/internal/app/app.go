// Package app is the desktop client's window: the state of its views and
// the views that draw them with MyGo and frostui. What the client knows of
// pocketd, and every rule about it, lives in internal/state; this package
// only draws it and turns the owner's input into its calls (ADR-0010).
//
// App and everything in it belong to the UI thread. Work that waits on the
// network runs in state.Store, which posts its results back through
// Options.Post.
package app

import (
	"net/http"
	"net/url"
	"time"

	"github.com/egoist/mygo/ui"
	"github.com/egoist/mygo/ui/markdown"
	"github.com/frostyard/frostui"

	"github.com/frostyard/pocketgo/desktop/internal/config"
	"github.com/frostyard/pocketgo/desktop/internal/state"
	"github.com/frostyard/pocketgo/proto"
)

// Options are what the program gives the app.
type Options struct {
	Version string
	// Post runs a function on the UI thread; safe from any goroutine.
	Post  func(func())
	Store *config.Store
	Creds config.Credentials
	// Quit ends the program; nil hides the menu item.
	Quit func()
	// After and Transport go to state.Options: tests give a clock and a
	// fake pocketd of their own. nil is time.After and the default
	// transport.
	After     func(time.Duration) <-chan time.Time
	Transport http.RoundTripper
}

// place is what the main pane shows.
type place int

const (
	placeInbox place = iota
	placeSession
	placeBoard
)

// App is the window's state.
type App struct {
	opts     Options
	st       *state.Store
	settings config.Settings

	shell   frostui.PaneShell
	table   *frostui.CommandTable
	dialogs frostui.Dialogs
	console *frostui.Console
	md      markdown.Options

	signIn   signInForm
	place    place
	tr       transcriptView
	composer composer
	reasons  map[string]*string // deny reasons being written, by approval
	argsOpen map[string]bool    // approval cards showing their arguments
	// inForceOpen unfolds the Inbox's list of lessons in force.
	inForceOpen bool
	// closedOpen unfolds the board's closed threads; reportOpen the
	// reports shown whole, by thread card.
	closedOpen bool
	reportOpen map[string]bool
	argsAll    map[string]*string // approval cards showing all of their arguments
	answers    map[int64]*string  // answers being written, by ask
	newSession *newSessionForm

	pendingToasts []string
}

// New returns the app's state, with the settings read.
func New(opts Options) (*App, error) {
	settings, err := opts.Store.Load()
	if err != nil {
		return nil, err
	}
	con := frostui.NewConsole(2000)
	con.SetNotify(frostui.NewWaker(opts.Post, 80*time.Millisecond).Wake)
	a := &App{opts: opts, settings: settings, console: con, reasons: map[string]*string{}, argsOpen: map[string]bool{}, argsAll: map[string]*string{},
		answers: map[int64]*string{}, reportOpen: map[string]bool{}}
	a.st = state.New(state.Options{
		Post: opts.Post, After: opts.After, Transport: opts.Transport,
		Log:       a.logLine,
		SignedOut: a.signedOut,
	})
	a.shell.Dialogs = &a.dialogs
	a.shell.Console.Title = "Activity"
	a.shell.Console.Subtitle = "what PocketGo and pocketd did"
	a.shell.Console.EmptyNote = "The connection and every command PocketGo sends show here."
	a.shell.Panes.Layout = frostui.PaneLayout{Widths: settings.Widths, Collapsed: settings.Collapsed}
	a.md = markdown.Options{}
	a.composer.drafts = map[int64]*string{}
	a.signIn.server = settings.Server
	if a.signIn.server == "" {
		a.signIn.server = settings.LastServer
	}
	a.setUpCommands()
	return a, nil
}

// Store is the app's data layer, for the program and tests.
func (a *App) Store() *state.Store { return a.st }

// Start connects with the kept sign-in, or leaves the sign-in screen up.
func (a *App) Start() {
	a.opts.Post(func() {
		server := a.settings.Server
		if server == "" {
			return
		}
		token, err := a.opts.Creds.Token(server)
		if err != nil {
			a.signIn.err = "Reading the keyring: " + err.Error()
			return
		}
		if token == "" {
			return
		}
		if err := a.st.Connect(server, token); err != nil {
			a.signIn.err = err.Error()
			return
		}
		if id := a.settings.LastSession; id != 0 {
			a.openSession(id)
		}
	})
}

// Close stops the stream and keeps the layout and the session shown.
func (a *App) Close() {
	a.st.Close()
	a.settings.Widths = a.shell.Panes.Layout.Widths
	a.settings.Collapsed = a.shell.Panes.Layout.Collapsed
	a.settings.LastSession = 0
	if a.place == placeSession {
		a.settings.LastSession = a.st.Data.OpenID()
	}
	_ = a.saveSettings()
}

func (a *App) saveSettings() error {
	err := a.opts.Store.Save(a.settings)
	if err != nil {
		a.console.Add(frostui.LevelWarn, "app", "Saving the settings: "+err.Error())
	}
	return err
}

// logLine puts a line of the data layer's log in the activity console.
func (a *App) logLine(level state.Level, text string) {
	l := frostui.LevelInfo
	switch level {
	case state.LevelWarn:
		l = frostui.LevelWarn
	case state.LevelError:
		l = frostui.LevelError
	case state.LevelCommand:
		l = frostui.LevelCommand
	}
	a.console.Add(l, "pocketd", text)
}

func (a *App) resetViews() {
	a.place = placeInbox
	a.tr = transcriptView{}
	a.composer = composer{drafts: map[int64]*string{}}
	a.reasons, a.argsOpen, a.argsAll = map[string]*string{}, map[string]bool{}, map[string]*string{}
	a.answers = map[int64]*string{}
	a.reportOpen = map[string]bool{}
	a.newSession = nil
}

// goInbox shows the Inbox.
func (a *App) goInbox() {
	a.place = placeInbox
	a.st.Open(0)
}

// goBoard shows the work board.
func (a *App) goBoard() {
	a.place = placeBoard
	a.st.Open(0)
}

// openSession shows session id.
func (a *App) openSession(id int64) {
	if a.place != placeSession || a.st.Data.OpenID() != id {
		a.tr = transcriptView{}
	}
	a.place = placeSession
	a.st.Open(id)
	a.composer.focus = true
}

// toast shows a short message at the top of the window on the next frame.
func (a *App) toast(msg string) { a.pendingToasts = append(a.pendingToasts, msg) }

// host is the host part of a service URL, for the status bar.
func host(server string) string {
	if u, err := url.Parse(server); err == nil && u.Host != "" {
		return u.Host
	}
	return server
}

// View draws the window.
func (a *App) View(c *ui.Context) {
	for _, msg := range a.pendingToasts {
		c.Toast(msg)
	}
	a.pendingToasts = nil
	if !a.st.SignedIn() {
		frostui.Adapt(c)
		a.signInView(c)
		return
	}
	conn := a.connection()
	a.table.Editing = func(c *ui.Context) bool { return c.FocusWithin(a.composer.field) }
	a.shell.View(c, frostui.PaneFrame{
		SidebarPane: "sidebar",
		Panes: []frostui.Pane{
			{ID: "sidebar", Label: "Sessions", Width: 300, Min: 220, Max: 460, Collapsible: true,
				Bar: func() { a.sidebarBar(c) }, Content: func() { a.sidebarPane(c) }},
			{ID: "main", Label: "Session", Min: 420,
				Bar: func() { a.mainBar(c) }, Content: func() { a.mainPane(c) }},
		},
		Menu:        a.menu,
		Table:       a.table,
		Connection:  &conn,
		Status:      a.status(),
		StatusEnd:   a.statusEnd(),
		ConsoleData: a.console,
	})
	a.newSessionDialog(c)
}

// connection is the store's connection as the status bar shows it.
func (a *App) connection() frostui.Connection {
	conn := a.st.Conn
	out := frostui.Connection{Daemon: "pocketd", Host: host(conn.Server), Reason: conn.Reason,
		OnRetry: a.st.RetryNow, OnDetails: func() { a.shell.Console.Open = true }}
	switch conn.State {
	case state.Connected:
		out.State = frostui.Connected
	case state.Connecting:
		out.State = frostui.Connecting
	default:
		out.State = frostui.Reconnecting
	}
	return out
}

func (a *App) mainPane(c *ui.Context) {
	switch {
	case a.place == placeSession && a.st.Data.Open() != nil:
		a.sessionPane(c)
	case a.place == placeBoard:
		a.boardPane(c)
	default:
		a.inboxPane(c)
	}
}

func (a *App) mainBar(c *ui.Context) {
	if a.place == placeSession && a.st.Data.Open() != nil {
		a.sessionBar(c)
		return
	}
	if a.place == placeBoard {
		ui.Column(c).Grow(1).Children(func() {
			ui.Text(c, "Work Board").Bold().SingleLine()
			frostui.Caption(c, a.st.Data.BoardNote()).SingleLine()
		})
		return
	}
	ui.Column(c).Grow(1).Children(func() {
		ui.Text(c, "Inbox").Bold().SingleLine()
		frostui.Caption(c, state.InboxNote(a.st.Data.Inbox())).SingleLine()
	})
}

// status is the status bar's start: what waits and what runs.
func (a *App) status() []frostui.StatusItem {
	waiting, running := a.st.Data.Counts()
	var items []frostui.StatusItem
	if waiting > 0 {
		items = append(items, frostui.StatusItem{Icon: "dialog-warning", Text: plural(waiting, "session waits", "sessions wait"),
			Tone: frostui.Warning, Progress: -1, OnClick: a.goInbox, Tooltip: "Open the Inbox"})
	}
	if asks := len(a.st.Data.Asks()); asks > 0 {
		items = append(items, frostui.StatusItem{Icon: "help-browser", Text: plural(asks, "question waits", "questions wait"),
			Tone: frostui.Warning, OnClick: a.goInbox, Tooltip: "Open the Inbox"})
	}
	if lessons := len(a.st.Data.Lessons()); lessons > 0 {
		items = append(items, frostui.StatusItem{Icon: "accessories-text-editor", Text: plural(lessons, "lesson waits", "lessons wait"),
			Tone: frostui.Warning, OnClick: a.goInbox, Tooltip: "Open the Inbox"})
	}
	if pulls := len(a.st.Data.PullsToReview()); pulls > 0 {
		items = append(items, frostui.StatusItem{Icon: "insert-link", Text: plural(pulls, "pull request waits", "pull requests wait"),
			Tone: frostui.Warning, OnClick: a.goInbox, Tooltip: "Open the Inbox"})
	}
	if running > 0 {
		items = append(items, frostui.StatusItem{Icon: "content-loading", Text: plural(running, "session runs", "sessions run"), Progress: -1})
	}
	return items
}

func (a *App) statusEnd() []frostui.StatusItem {
	var items []frostui.StatusItem
	if h := a.st.Hello(); h != nil {
		items = append(items, frostui.StatusItem{Text: "pocketd " + h.Server + " · " + h.Device, Progress: -1,
			Tooltip: "The service's version and this device's name"})
	}
	return items
}

func (a *App) menu(m *ui.Menu) {
	a.table.Menu("new", "inbox", "board", "abort", "", "sidebar", "")(m)
	a.table.MenuItem(m, "about")
	m.Separator()
	a.table.MenuItem(m, "sign-out")
	if a.opts.Quit != nil {
		a.table.MenuItem(m, "quit")
	}
}

func (a *App) setUpCommands() {
	busy := func() (bool, string) {
		o := a.st.Data.Open()
		return a.place == placeSession && o != nil && o.Live.Busy, "No run works in the session shown"
	}
	a.table = frostui.NewCommandTable(
		frostui.Command{ID: "new", Title: "New Session", Group: "Sessions", Icon: "list-add",
			Bindings: []frostui.KeyBinding{{Mods: ui.Cmd, Key: ui.KeyN, Scope: frostui.Everywhere}},
			Run:      a.startNewSession},
		frostui.Command{ID: "inbox", Title: "Inbox", Group: "Go to", Icon: "mail-unread",
			Bindings: []frostui.KeyBinding{{Mods: ui.Cmd, Key: ui.Key1, Scope: frostui.Everywhere}},
			Run:      a.goInbox},
		frostui.Command{ID: "board", Title: "Work Board", Group: "Go to", Icon: "view-grid",
			Bindings: []frostui.KeyBinding{{Mods: ui.Cmd, Key: ui.Key2, Scope: frostui.Everywhere}},
			Run:      a.goBoard},
		frostui.Command{ID: "abort", Title: "Abort the Run", Group: "Sessions", Icon: "process-stop", Enabled: busy,
			Bindings: []frostui.KeyBinding{{Mods: ui.Cmd, Key: ui.KeyPeriod, Scope: frostui.Everywhere}},
			Run: func() {
				if id := a.st.Data.OpenID(); id != 0 {
					a.st.Abort(id)
				}
			}},
		frostui.Command{ID: "sidebar", Title: "Show or Hide the Sessions", Group: "View", Icon: "sidebar-show",
			Run: func() { a.shell.Panes.Toggle("sidebar") }},
		frostui.Command{ID: "about", Title: "About PocketGo", Group: "App", Icon: "help-about", Run: a.showAbout},
		frostui.Command{ID: "sign-out", Title: "Sign Out", Group: "App", Icon: "application-exit", Run: a.confirmSignOut},
		frostui.Command{ID: "quit", Title: "Quit", Group: "App", Icon: "application-exit",
			Bindings: []frostui.KeyBinding{{Mods: ui.Cmd, Key: ui.KeyQ, Scope: frostui.Everywhere}},
			Enabled:  func() (bool, string) { return a.opts.Quit != nil, "" },
			Run: func() {
				if a.opts.Quit != nil {
					a.opts.Quit()
				}
			}},
	)
}

func (a *App) confirmSignOut() {
	a.dialogs.Ask(frostui.Confirm{
		Title:  "Sign out of pocketd?",
		Body:   "PocketGo forgets this device's token. Signing in again needs a token from `pocketd device add NAME`.",
		Action: "Sign Out", Destructive: true, OnConfirm: a.signOut,
	})
}

func (a *App) showAbout() {
	a.shell.Info.Show("About PocketGo", frostui.AboutBody(frostui.About{
		Name: "PocketGo", Version: a.opts.Version,
		Summary: "The desktop client of pocketd, protocol version " + itoa(proto.Version) + ".",
		Website: "https://github.com/frostyard/pocketgo", License: "MIT",
	}))
}
