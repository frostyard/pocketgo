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
	"context"
	"net/http"
	"net/url"
	"runtime"
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
	// Now is the time the sidebar judges NEW and old sessions at, and a
	// launch's end is computed from, in the desktop's zone; tests give a
	// clock of their own. nil is time.Now.
	Now func() time.Time
	// GitHubAuth is the owner's GitHub App sign-in, GitHubClient a client
	// acting with its token (ADR-0018); a nil GitHubAuth turns approve and
	// merge off.
	GitHubAuth   state.GitHubAuth
	GitHubClient func(token string) state.GitHubAPI
	// OpenURL opens a web or mail link a terminal printed; nil is MyGo's
	// opener. Tests see what it is given.
	OpenURL func(url string) error
	// PickFiles asks the owner for files to attach to a message, with the
	// native chooser (ADR-0029); it runs off the window's thread. nil
	// hides Attach files.
	PickFiles func(context.Context) ([]string, error)
	// Platform is the system the window runs on, as runtime.GOOS names
	// it; "" is runtime.GOOS. state's platform rules read it (ADR-0040):
	// on iOS there is no Terminal and no Quit. Tests give "ios" to draw
	// the iPad's window.
	Platform string
}

// place is what the main pane shows.
type place int

const (
	placeInbox place = iota
	placeSession
	placeBoard
	// placeTerminal is the owner's shells on the service host (ADR-0025).
	placeTerminal
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

	// sidebar is the sidebar's list, filter what its filter holds, and sb
	// the sidebar as sbFor made it, with its sections as the list takes
	// them. seenRev counts the changes to the seen marks, pinRev those to
	// the pins.
	sidebar    frostui.SourceList[state.SidebarKey]
	filter     string
	sb         *state.Sidebar
	sbSections []frostui.SourceSection[state.SidebarKey]
	sbFor      sidebarFor
	seenRev    uint64
	pinRev     uint64

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
	newSession *newSessionForm

	// launchEnd is the board's end chooser's segment (endChoices), and
	// launchClock the clock time typed for Until.
	launchEnd   int
	launchClock string

	pendingToasts []string
	// gh is the owner's GitHub sign-in and merges; ghSignIn shows its
	// sign-in window.
	gh       *state.GitHub
	ghSignIn bool
	// prefs is the Settings window, with Zoom; zoomKeys its zoom
	// commands alone, bound on the sign-in screen and in Settings;
	// shownZoom the zoom the window shows, at most the one saved.
	prefs     prefs
	zoomKeys  *frostui.CommandTable
	shownZoom float32
	// terms are the shells the Terminal place shows (terminals.go);
	// loadLib loads the emulator's library (loadEmulatorLibrary, which is
	// terminal.Load; tests replace it).
	terms   terms
	loadLib func() error
	// goLoad runs the library's load off the window's thread (go f());
	// tests run it when they choose.
	goLoad func(f func())
}

// New returns the app's state, with the settings read.
func New(opts Options) (*App, error) {
	settings, err := opts.Store.Load()
	if err != nil {
		return nil, err
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Platform == "" {
		opts.Platform = runtime.GOOS
	}
	// A platform whose system ends apps itself offers no Quit, as when
	// the program gives none (state.QuitsItself, ADR-0040).
	if !state.QuitsItself(opts.Platform) {
		opts.Quit = nil
	}
	con := frostui.NewConsole(2000)
	con.SetNotify(frostui.NewWaker(opts.Post, 80*time.Millisecond).Wake)
	a := &App{opts: opts, settings: settings, console: con, reasons: map[string]*string{}, argsOpen: map[string]bool{}, argsAll: map[string]*string{},
		reportOpen: map[string]bool{}}
	a.st = state.New(state.Options{
		Post: opts.Post, After: opts.After, Transport: opts.Transport, Now: opts.Now,
		Log:       a.logLine,
		SignedOut: a.signedOut,
		// Merge All Ready belongs to the sign-in it began under (ADR-0031).
		SignInChanged: func() {
			if a.gh != nil {
				a.gh.PocketdSignInChanged()
			}
		},
	})
	a.loadLib = loadEmulatorLibrary
	a.goLoad = func(f func()) { go f() }
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
	a.gh = state.NewGitHub(state.GitHubOptions{Post: opts.Post, Auth: opts.GitHubAuth, NewClient: opts.GitHubClient,
		Owners: githubOwners, Log: func(l state.Level, text string) { a.logLine(l, text) }})
	a.setUpCommands()
	a.setUpSidebar()
	return a, nil
}

// Store is the app's data layer, for the program and tests.
func (a *App) Store() *state.Store { return a.st }

// Start connects with the kept sign-in, or leaves the sign-in screen up.
func (a *App) Start() {
	a.opts.Post(func() {
		a.gh.Open(nil)
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

// Close stops the stream and the shells shown, and keeps the layout and
// the session shown.
func (a *App) Close() {
	a.closeTerminals()
	a.leaveSession()
	a.st.Close()
	a.keep()
	_ = a.saveSettings()
}

// Suspend is the system suspending the app (ADR-0041). Where it may end
// the app there without closing its window, as iOS does
// (state.SavesOnSuspend), the window saves what Close saves: the session
// shown, seen as it is, the panes' layout and the rest of the settings.
// Unlike Close it stops nothing: the app goes on as it comes back.
// Elsewhere it does nothing.
func (a *App) Suspend() {
	if !state.SavesOnSuspend(a.opts.Platform) {
		return
	}
	a.leaveSession()
	a.keep()
	_ = a.saveSettings()
}

// keep puts in the settings what the window keeps between runs: the panes'
// layout and the session shown, none while it shows a place.
func (a *App) keep() {
	a.settings.Widths = a.shell.Panes.Layout.Widths
	a.settings.Collapsed = a.shell.Panes.Layout.Collapsed
	a.settings.LastSession = 0
	if a.place == placeSession {
		a.settings.LastSession = a.st.Data.OpenID()
	}
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
	a.reportOpen = map[string]bool{}
	a.launchEnd, a.launchClock = 0, ""
	a.newSession = nil
	// A confirmation belongs to the sign-in it was asked under: one left
	// open would act on the next sign-in's service.
	a.dialogs = frostui.Dialogs{}
	// So do the sidebar's open groups and its filter.
	a.setUpSidebar()
	// So does a shell: its connection keeps the sign-in it was opened with.
	// The emulator's library stays as it loaded.
	a.closeTerminals()
	a.shellKeysAside(false)
	a.terms = terms{lib: a.terms.lib, libErr: a.terms.libErr, keyed: a.terms.keyed}
}

// goInbox shows the Inbox.
func (a *App) goInbox() {
	a.leaveSession()
	a.place = placeInbox
	a.st.Open(0)
	a.showInSidebar()
}

// goBoard shows the work board.
func (a *App) goBoard() {
	a.leaveSession()
	a.place = placeBoard
	a.st.Open(0)
	a.showInSidebar()
}

// openSession shows session id, with the keyboard focus in its message
// field.
func (a *App) openSession(id int64) {
	a.showSession(id)
	a.composer.focus = true
}

// showSession shows session id, which the owner has now seen as it is,
// and leaves the keyboard focus where it is: the sidebar's arrow keys go
// on to the next session (Codex review of #40, round 1).
func (a *App) showSession(id int64) {
	if a.place != placeSession || a.st.Data.OpenID() != id {
		a.leaveSession()
		a.tr = transcriptView{}
	}
	a.place = placeSession
	a.st.Open(id)
	a.markSeen(id)
	a.showInSidebar()
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
	a.applyZoom(c)
	if !a.st.SignedIn() {
		// The zoom keys work before signing in too, so a zoom saved
		// too large can be undone there (Codex review of #39).
		a.zoomKeys.Bind(c)
		frostui.Adapt(c)
		a.signInView(c)
		// GitHub's sign-out asks here too (Codex review of #24, round 7),
		// and a device flow begun before pocketd signed out stays in view
		// (round 9).
		a.dialogs.View(c)
		a.githubSignInView(c)
		return
	}
	conn := a.connection()
	// A terminal takes keys as an editor does, and guardShellKeys keeps
	// the shells' Ctrl keys for them.
	a.syncTerminals()
	a.terms.focused = a.place == placeTerminal && c.FocusWithin(a.terms.field)
	a.shellKeysAside(a.terms.focused)
	a.table.Editing = func(c *ui.Context) bool {
		return c.FocusWithin(a.composer.field) || (a.place == placeTerminal && c.FocusWithin(a.terms.field))
	}
	a.shell.View(c, frostui.PaneFrame{
		SidebarPane: "sidebar",
		Panes: []frostui.Pane{
			{ID: "sidebar", Label: "Sessions", Width: 300, Min: sidebarMin, Max: 460, Collapsible: true,
				Bar: func() { a.sidebarBar(c) }, Content: func() { a.sidebarPane(c) }},
			{ID: "main", Label: "Session", Min: mainMin,
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
	a.settingsDialog(c)
	a.githubSignInView(c)
	a.mergeAllConfirmView(c)
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
	case a.place == placeTerminal:
		a.terminalPane(c)
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
	if a.place == placeTerminal {
		ui.Text(c, "Terminal").Bold().SingleLine().Grow(1)
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
	// A refreshed GitHub sign-in the keyring did not keep, whatever the
	// Inbox holds: closing the window would lose it (Codex review of #24,
	// round 4).
	if a.gh.CanSaveAgain() {
		items = append(items, frostui.StatusItem{Icon: "dialog-warning", Text: "GitHub sign-in not saved", Tone: frostui.Danger,
			Tooltip: "Save the GitHub sign-in again", OnClick: func() {
				a.gh.SaveAgain(func(err error) {
					if err != nil {
						a.toast("Saving the GitHub sign-in: " + err.Error())
						return
					}
					a.toast("Saved the GitHub sign-in")
				})
			}})
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
	a.table.Menu("new", "inbox", "board", "abort", "", "sidebar", "zoom-in", "zoom-out", "zoom-reset", "")(m)
	a.table.MenuItem(m, "settings")
	a.table.MenuItem(m, "about")
	m.Separator()
	a.table.MenuItem(m, "github-sign-in")
	a.table.MenuItem(m, "github-sign-out")
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
		frostui.Command{ID: "github-sign-in", Title: "Sign In to GitHub", Group: "GitHub", Icon: "dialog-password",
			Enabled: func() (bool, string) { return a.gh.CanOpenSignIn(), a.githubSignInOff() }, Run: a.openGitHubSignIn},
		frostui.Command{ID: "github-sign-out", Title: "Sign Out of GitHub", Group: "GitHub", Icon: "application-exit",
			Enabled: func() (bool, string) { return a.gh.CanSignOut(), a.githubSignOutOff() },
			Run:     a.confirmGitHubSignOut},
		frostui.Command{ID: "quit", Title: "Quit", Group: "App", Icon: "application-exit",
			Bindings: []frostui.KeyBinding{{Mods: ui.Cmd, Key: ui.KeyQ, Scope: frostui.Everywhere}},
			Enabled:  func() (bool, string) { return a.opts.Quit != nil, "" },
			Run: func() {
				if a.opts.Quit != nil {
					a.opts.Quit()
				}
			}},
	)
	a.table.Add(a.settingsCommand())
	a.table.Add(a.zoomCommands()...)
	a.zoomKeys = frostui.NewCommandTable(a.zoomCommands()...)
	a.guardShellKeys()
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
