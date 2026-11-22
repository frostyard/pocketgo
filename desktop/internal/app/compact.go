package app

import (
	"github.com/egoist/mygo/ui"
	"github.com/egoist/mygo/ui/mobile"
	"github.com/frostyard/frostui"

	"github.com/frostyard/pocketgo/desktop/internal/state"
	"github.com/frostyard/pocketgo/proto"
)

// The compact window is a phone's (ADR-0044, chosen by state.Compact): one
// view at a time, full width, under a compact top bar, in place of the
// pane shell's sidebar beside the main pane, with a bar of tabs at the
// bottom: the Inbox, the work board, the sessions and More. A session
// shows in place of its tab's view, with a back button to it and no tabs.
// The views are the desktop's own. Its rules are state's (state.TabOf,
// state.BackTo, state.TabBarShows, state.InboxBadge); this file draws
// them, with MyGo's ui/mobile bars and lists.

// phoneNav is the compact window's navigation.
type phoneNav struct {
	// tab is the tab chosen: the one whose view shows, or that the session
	// shown shows over.
	tab state.Tab
	// activity is the Activity sheet's list. The sheet shows while the
	// console would (PaneShell.Console.Open), which its own Close, the
	// connecting screen's Details and the console's key set too, and the
	// About sheet while a desktop's About would (PaneShell.Info.Open), so
	// neither comes back unasked as the window's width changes.
	activity ui.ListState
	// newSession is New Session's sheet's flag: the window's own, as
	// mobile.Sheet keeps its state on the window's root by the flag's
	// address, and the form is new at each opening.
	newSession bool
	// from is the place the session shown was opened from, which its
	// back button goes to when it is the Search view (state.BackFrom).
	from string
	// prompts is the saved prompts' sheet's flag, and promptsFor the
	// session whose composer a prompt chosen there goes into (ADR-0051);
	// promptsShown says the last frame drew the sheet, open or sliding
	// away, which keeps the keys while it shows.
	prompts      bool
	promptsFor   int64
	promptsShown bool
	// changes is the Changes sheet's flag (changes.go, ADR-0053).
	changes bool
}

// tabIcons are the tabs' icons, of the window's set.
var tabIcons = map[state.Tab]string{
	state.TabInbox:    "mail-unread",
	state.TabBoard:    "view-grid",
	state.TabSessions: "pocket-session",
	state.TabMore:     "view-more-horizontal",
}

// widthAtFull is the window's width at 100%: its size comes in DIPs at
// the zoom the last frame set (applyZoom).
func (a *App) widthAtFull(c *ui.Context) float32 {
	w, _ := c.Size()
	if a.shownZoom > 0 {
		w *= a.shownZoom
	}
	return w
}

// gutter is the room between the window's sides and a view's content:
// less on a compact window.
func (a *App) gutter() float32 {
	if a.compact {
		return frostui.SpaceM
	}
	return frostui.SpaceXL
}

// fitWidth is the width of a dialog's content that wants want DIPs: want
// on a desktop's window, and within the screen on a compact one.
func (a *App) fitWidth(c *ui.Context, want float32) float32 {
	if !a.compact {
		return want
	}
	return dialogWidth(c, want)
}

// headTitle is the title of a card's head row, before its caption and
// its buttons: on one line on a desktop's window, and on as many as it
// takes on a compact one, where the row's buttons leave it little room.
func (a *App) headTitle(c *ui.Context, text string) ui.Element {
	e := frostui.Strong(c, text).Grow(1)
	if a.compact {
		return e.Wrap()
	}
	return e.SingleLine()
}

// headCaption is a card's caption that names where it comes from, as its
// thread, drawn where inRow says: in the card's head row, after its title,
// on a desktop's window, and on a line of its own under that row on a
// compact one, where the row has no room for it beside the title.
func (a *App) headCaption(c *ui.Context, text string, inRow bool) {
	if inRow != a.compact {
		frostui.Caption(c, text).SingleLine()
	}
}

// compactView draws the signed-in window as a phone's.
func (a *App) compactView(c *ui.Context) {
	t := frostui.Adapt(c)
	// A keyboard's shortcuts work as on a desktop: an iPad's, in Split
	// View.
	a.table.Editing = func(c *ui.Context) bool { return c.FocusWithin(a.composer.field) }
	// The pane shell's palette, which a wider window brought to the table,
	// does not show here: its keys go with it, and it closes. The shell
	// brings it back as the window widens (Codex review of #85, round 1).
	a.table.Remove(frostui.PaletteCommandID)
	a.shell.Palette.Open = false
	a.table.Bind(c)
	// The console's key, which the pane shell brings to the table, shows
	// the Activity here too, before any wide window brought it (Codex
	// review of #85, round 2).
	if _, ok := a.table.Command(frostui.ConsoleCommandID); !ok {
		a.table.BindKeys(c, a.shell.ConsoleCommand().Bindings, func() { a.shell.Console.Open = !a.shell.Console.Open })
	}
	// A keyboard reaches each tab: the Inbox's and the board's keys are
	// the window's commands, and Sessions and More take the next two. The
	// bar's buttons take no focus (Codex review of #85, round 3).
	for _, k := range []struct {
		tab state.Tab
		key ui.Key
	}{{state.TabSessions, ui.Key3}, {state.TabMore, ui.Key4}} {
		a.table.BindKeys(c, []frostui.KeyBinding{{Mods: ui.Cmd, Key: k.key, Scope: frostui.Everywhere}}, func() {
			a.phone.tab = k.tab
			a.goPlaceNamed(k.tab.Place())
		})
	}
	conn := a.connection()
	if conn.State == frostui.Connecting {
		ui.Column(c).Fill().AlignItems(ui.Stretch).Children(func() {
			// GitHub's sign-in opens without pocketd: its warning shows
			// here too (Codex review of #85, round 2).
			a.saveAgainBanner(c)
			frostui.ConnectingScreen(c, conn).Grow(1).Basis(0).MinHeight(0)
			// A saved service that no longer answers keeps the first
			// connection here: Sign Out goes back to the address form, as
			// the desktop's main menu does on this screen (Codex review of
			// #85, round 1).
			ui.Row(c).Justify(ui.Center).Padding(0, 0, frostui.SpaceXL, 0).Shrink(0).Children(func() {
				if ui.Button(c, "Sign Out").Clicked() {
					a.confirmSignOut()
				}
			})
		})
		a.compactOverlays(c)
		return
	}
	if tab, ok := state.TabOf(a.place.name()); ok {
		a.phone.tab = tab
	}
	bar := frostui.Chrome(t)
	top, _, _, _ := c.SafeArea()
	ui.Column(c).Fill().AlignItems(ui.Stretch).Background(t.Background).Children(func() {
		// The bar's color goes up under the system's status bar, which
		// the window's root keeps the content out from under.
		ui.Column(c).Shrink(0).AlignItems(ui.Stretch).Background(bar).Padding(top, 0, 0, 0).Margin(-top, 0, 0, 0).Children(func() {
			mobile.TopBar(c, mobile.TopBarOptions{Title: a.compactTitle(), Background: &bar,
				Leading: func() {
					switch {
					case a.shownSession() != nil:
						a.backButton(c)
					case a.place == placeSearch:
						a.backTo(c, state.PlaceSessions, state.TabSessions.Title())
					}
				},
				Trailing: func() { a.compactActions(c) },
			})
			a.compactNote(c)
		})
		ui.Box(c).Height(1).Background(t.Border).Shrink(0)
		if conn.State != frostui.Connected {
			ui.Row(c).Padding(frostui.SpaceXS, a.gutter()).Children(func() { frostui.ConnectionStatus(c, conn) })
		}
		a.saveAgainBanner(c)
		ui.Column(c.Key(int(a.place))).Grow(1).Basis(0).MinHeight(0).AlignItems(ui.Stretch).Children(func() {
			switch {
			case a.shownSession() != nil:
				a.sessionPane(c)
			case a.place == placeBoard:
				a.boardPane(c)
			case a.place == placeSessions:
				a.phoneSessions(c)
			case a.place == placeSearch:
				a.searchPane(c)
			case a.place == placeMore:
				a.phoneMore(c)
			default:
				a.inboxPane(c)
			}
		})
		if state.TabBarShows(a.shownSession() != nil) {
			ui.Box(c).Height(1).Background(t.Border).Shrink(0)
			a.tabBar(c)
		}
	})
	a.compactOverlays(c)
}

// saveAgainBanner warns of a refreshed GitHub sign-in the keyring did not
// keep, with Save Again: the desktop's status bar warns of it on every
// view, and so does a compact window, whatever the Inbox holds (Codex
// review of #85, round 1).
func (a *App) saveAgainBanner(c *ui.Context) {
	if !a.gh.CanSaveAgain() {
		return
	}
	ui.Row(c).Gap(frostui.SpaceS).AlignItems(ui.Center).Padding(frostui.SpaceXS, a.gutter()).Shrink(0).Children(func() {
		frostui.Problem(c, "GitHub sign-in not saved").Wrap().Grow(1)
		if ui.Button(c, "Save Again").Clicked() {
			a.saveGitHubAgain()
		}
	})
}

// compactOverlays are what shows over the compact window: its toasts, its
// confirmations, its dialogs and its sheets.
func (a *App) compactOverlays(c *ui.Context) {
	top, _, _, _ := c.SafeArea()
	compactToasts(c, top+44+frostui.SpaceS)
	a.dialogs.View(c, true)
	a.newSessionDialog(c)
	a.settingsDialog(c)
	a.githubSignInView(c)
	a.mergeAllConfirmView(c)
	a.changesSheet(c)
	mobile.Sheet(c, &a.shell.Console.Open, mobile.SheetOptions{Detents: []float32{activityDetent}, Label: "Activity"}, func() { a.phoneActivity(c) })
	// The saved prompts' sheet belongs to the composer it was opened for:
	// it closes when another session shows, or that composer no longer
	// writes a message, as when a question comes (ADR-0051, Codex review
	// of #108, round 1).
	if id, ok := a.promptsComposer(); a.phone.prompts && (!ok || id != a.phone.promptsFor) {
		a.phone.prompts = false
	}
	shown := false
	mobile.Sheet(c, &a.phone.prompts, mobile.SheetOptions{Detents: []float32{promptsDetent}, Label: "Saved Prompts"}, func() {
		shown = true
		a.phonePrompts(c)
	})
	// Once it is gone, the composer takes the keys a prompt chosen there
	// gave it, on the next frame.
	if a.phone.promptsShown && !shown {
		c.AnimationFrame()
	}
	a.phone.promptsShown = shown
	mobile.Sheet(c, &a.shell.Info.Open, mobile.SheetOptions{Detents: []float32{0.5}, Label: "About PocketGo"}, func() {
		ui.Column(c).Padding(frostui.SpaceS, frostui.SpaceL).Gap(frostui.SpaceM).AlignItems(ui.Stretch).Children(func() {
			frostui.AboutBody(a.about())(c)
		})
	})
}

// activityDetent is the Activity sheet's one height: a sheet lays what it
// holds out at its tallest height, so a list that follows its newest line
// would put it below the screen at a lower one (Codex review of #85,
// round 3).
const activityDetent = 0.75

// promptsDetent is the saved prompts' sheet's one height, for the same
// reason: its list scrolls within it.
const promptsDetent = 0.6

// phoneActivity is the Activity sheet: the console's lines, newest last,
// under a header with Copy and Close. frostui's console pane lays its
// filters out for a desktop's width, which puts its Close past a phone's
// screen (Codex review of #85, round 1).
func (a *App) phoneActivity(c *ui.Context) {
	t := c.Theme()
	levels := frostui.DefaultLevels
	lines := a.console.Lines(&levels, "")
	ui.Column(c).Grow(1).Basis(0).MinHeight(0).AlignItems(ui.Stretch).Children(func() {
		ui.Row(c).Gap(frostui.SpaceXS).AlignItems(ui.Center).Padding(0, frostui.SpaceS, 0, frostui.SpaceL).Shrink(0).Children(func() {
			ui.Text(c, a.shell.Console.Title).FontSize(17).FontWeight(600).Grow(1)
			if barButton(c, "edit-copy", "Copy the Activity").Clicked() {
				c.WriteClipboard(frostui.Text(lines))
				c.Announce(a.shell.Console.Title + " copied")
			}
			if barButton(c, "window-close", "Close "+a.shell.Console.Title).Clicked() {
				a.shell.Console.Open = false
			}
		})
		ui.Box(c).Height(1).Background(t.Border).Shrink(0)
		if len(lines) == 0 {
			frostui.Empty(c, "utilities-terminal", "Nothing yet", a.shell.Console.EmptyNote).Grow(1)
			return
		}
		list := &a.phone.activity
		list.FollowEnd = true
		list.Key = func(i int) any { return lines[i].Seq }
		ui.List(c, list, len(lines), func(i int) {
			l := lines[i]
			color := t.Text
			switch l.Level {
			case frostui.LevelCommand, frostui.LevelInfo:
				color = t.Accent
			case frostui.LevelDryRun, frostui.LevelWarn:
				color = t.Warning
			case frostui.LevelError:
				color = t.Danger
			}
			text := l.Text
			if l.Level == frostui.LevelCommand {
				text = "$ " + text
			}
			ui.Column(c).Gap(2).Padding(frostui.SpaceXS, frostui.SpaceL).AlignItems(ui.Stretch).Children(func() {
				ui.Text(c, l.At.Format("15:04:05")+" · "+frostui.LevelNames[l.Level]).Font("monospace").FontSize(11).TextColor(t.TextMuted)
				ui.Text(c, text).Font("monospace").FontSize(12).TextColor(color).Wrap().Selectable()
			})
		}).Grow(1).Basis(0).MinHeight(0).Padding(frostui.SpaceXS, 0)
	})
}

// compactToasts draws the window's toasts as frostui.TopToasts does, top
// DIPs from the window's top: under the top bar, below the system's status
// bar, which an overlay is not kept out from under.
func compactToasts(c *ui.Context, top float32) {
	t := c.Theme()
	k := frostui.ColorsOf(t)
	ui.ToastViewportBase(c, func(viewport ui.Element, toasts []ui.Toast) {
		viewport.Justify(ui.Start).AlignItems(ui.Center).Padding(top, frostui.SpaceM, frostui.SpaceM, frostui.SpaceM).Gap(frostui.SpaceS)
		for _, toast := range toasts {
			p := ui.ToastBase(c, toast)
			p.Root.Row().Gap(frostui.SpaceM).Padding(8, 8, 8, 16).Radius(frostui.RadiusCard).MaxWidth(frostui.ToastWidth).AlignItems(ui.Center).
				Background(k.Panel).TextColor(t.Text).Border(1, k.LineStrong)
			p.Root.Children(func() {
				if toast.Type == "error" {
					frostui.Icon(c, "dialog-warning").TextColor(t.Warning)
				}
				ui.Text(c, toast.Title).FontSize(13).Grow(1).Shrink(1)
				p.CloseButton().Label("Close").Tooltip("Close").Padding(6).Radius(frostui.RadiusChip).Children(func() {
					frostui.Icon(c, "window-close").TextColor(t.Text)
				})
			})
		}
	})
}

// shownSession is the session the window shows, nil while it shows a
// place. A button drawn earlier in the frame may have left it.
func (a *App) shownSession() *state.OpenSession {
	if a.place != placeSession {
		return nil
	}
	return a.st.Data.Open()
}

// compactTitle is the top bar's title: the session's, or its tab's.
func (a *App) compactTitle() string {
	if o := a.shownSession(); o != nil {
		s, _ := a.st.Data.Session(o.ID)
		return sessionTitle(s, o.ID)
	}
	if a.place == placeBoard {
		return "Work Board"
	}
	if a.place == placeSearch {
		return "Search"
	}
	return a.phone.tab.Title()
}

// compactNote is the line under the top bar, which a desktop's main bar
// holds under its title: what waits in the Inbox, what is on the board,
// or where the session works and its state.
func (a *App) compactNote(c *ui.Context) {
	o := a.shownSession()
	// The field of the session's title, while the owner renames it, in
	// the note's place: the top bar's title takes no field (ADR-0058). An
	// edit pocketd no longer takes ends, sending nothing.
	a.titleEdit.Check(&a.st.Data)
	if o != nil && a.titleEdit.Editing(o.ID) {
		ui.Row(c).Gap(frostui.SpaceS).AlignItems(ui.Center).Padding(0, a.gutter(), frostui.SpaceS, a.gutter()).Shrink(0).Children(func() {
			a.titleField(c)
		})
		return
	}
	note := ""
	switch {
	case o != nil:
		s, _ := a.st.Data.Session(o.ID)
		// The folder as the sidebar tells projects apart: a phone has room
		// for little more.
		thinking := ""
		if s.Thinking != "" {
			thinking = "thinking " + s.Thinking
		}
		// How full its context is, as the desktop's meter says it
		// (ADR-0052).
		fill := ""
		if label := a.st.Data.FillOf(o.ID).Label(); label != "" {
			fill = "context " + label
		}
		note = joinNonEmpty(shortPath(s.Cwd), s.Model.ID, thinking, fill)
	case a.place == placeBoard:
		note = a.st.Data.BoardNote()
	case a.place == placeInbox:
		note = state.InboxNote(a.st.Data.Inbox())
	}
	if note == "" {
		return
	}
	ui.Row(c).Gap(frostui.SpaceS).AlignItems(ui.Center).Padding(0, a.gutter(), frostui.SpaceS, a.gutter()).Shrink(0).Children(func() {
		frostui.Caption(c, note).SingleLine().Grow(1).Shrink(1).MinWidth(0)
		if o != nil {
			a.changesChip(c, o.ID)
			statePill(c, a.st.Data.StateOf(o.ID))
		}
	})
}

// compactActions are the top bar's buttons at its end: the session's, and
// New Session over the sessions.
func (a *App) compactActions(c *ui.Context) {
	ui.Row(c).Gap(frostui.SpaceXS).AlignItems(ui.Center).Children(func() {
		if o := a.shownSession(); o != nil {
			s, _ := a.st.Data.Session(o.ID)
			a.sessionActions(c, o, s)
			return
		}
		if a.place == placeSessions {
			if barButton(c, "pocket-new", "New Session").Clicked() {
				a.startNewSession()
			}
		}
	})
}

// backButton goes back from a session to its tab's view, or to the
// Search view it was opened from (state.BackFrom).
func (a *App) backButton(c *ui.Context) {
	tab, from := a.phone.tab, a.phone.from
	a.backTo(c, state.BackFrom(tab, from), state.BackTitle(tab, from))
}

// backTo is a back button to place, which title names.
func (a *App) backTo(c *ui.Context, place, title string) {
	t := c.Theme()
	b := ui.ButtonBase(c).Row().Gap(2).AlignItems(ui.Center).Padding(0, frostui.SpaceS, 0, frostui.SpaceXS).Height(44).
		Cursor(ui.CursorPointer).Label("Back to " + title)
	if b.Pressed() {
		b.Opacity(0.4)
	}
	b.Children(func() {
		frostui.Icon(c, "go-previous").FontSize(20).TextColor(t.Accent)
		ui.Text(c, title).FontSize(17).TextColor(t.Accent).SingleLine()
	})
	if b.Clicked() {
		if place == state.PlaceSearch {
			a.goSearch("")
			return
		}
		a.goPlaceNamed(place)
	}
}

// barButton is a button of the compact top bar: its icon alone, in the
// accent's color, as large as a finger.
func barButton(c *ui.Context, icon, label string) ui.Element {
	t := c.Theme()
	b := ui.ButtonBase(c).Size(44, 44).Center().Cursor(ui.CursorPointer).Label(label).Tooltip(label)
	if b.Pressed() {
		b.Opacity(0.4)
	}
	b.Children(func() { frostui.Icon(c, icon).FontSize(20).TextColor(t.Accent) })
	return b
}

// tabBar is the bar of tabs, the Inbox's with its badge.
func (a *App) tabBar(c *ui.Context) {
	buttons := make([]mobile.BarButton, len(state.Tabs))
	chosen := 0
	for i, tab := range state.Tabs {
		buttons[i] = mobile.BarButton{Label: tab.Title(), Icon: barIcon(tabIcons[tab])}
		if tab == state.TabInbox {
			buttons[i].Badge = state.InboxBadge(a.st.Data.Inbox())
		}
		if tab == a.phone.tab {
			chosen = i
		}
	}
	if mobile.BottomBar(c, &chosen, buttons...).Changed() {
		a.phone.tab = state.Tabs[chosen]
		a.goPlaceNamed(a.phone.tab.Place())
	}
}

// barIcon draws the named icon of the window's set on a bar's button.
func barIcon(name string) func(p *ui.Painter, r ui.Rect, color ui.Color) {
	return func(p *ui.Painter, r ui.Rect, color ui.Color) {
		if svg := frostui.IconSVG(name); svg != nil {
			p.Icon(svg, r, color)
		}
	}
}

// goPlaceNamed shows the place named so (state.PlaceInbox and the rest).
func (a *App) goPlaceNamed(name string) {
	switch p := placeNamed(name); p {
	case placeInbox:
		a.goInbox()
	case placeBoard:
		a.goBoard()
	case placeTerminal:
		a.goTerminal()
	default:
		a.leaveSession()
		a.place = p
		a.st.Open(0)
		a.showInSidebar()
	}
}

// phoneSessions is the Sessions tab: the sidebar's filter and its
// sessions, pinned, by the team and recent.
func (a *App) phoneSessions(c *ui.Context) {
	ui.Scroll(c).Grow(1).Fill().Children(func() {
		ui.Column(c).Gap(frostui.SpaceL).Padding(frostui.SpaceM, 0, frostui.SpaceL, 0).AlignItems(ui.Stretch).Children(func() {
			ui.Row(c).Padding(0, frostui.SpaceL).Children(func() {
				ui.SearchField(c, &a.filter).Placeholder("Filter sessions").Label("Filter sessions").Grow(1)
			})
			a.sessionsNote(c)
			a.profilesNote(c)
			a.phoneList(c)
		})
	})
}

// phoneList lists the sidebar's sections but its places, as a phone's
// grouped lists: the pinned sessions, the team's groups and the recent
// sessions, or the sessions its filter found.
func (a *App) phoneList(c *ui.Context) {
	sb := a.sidebarData()
	for _, sec := range sb.Sections {
		if sec.ID == state.SectionPlaces || len(sec.Items) == 0 {
			continue
		}
		mobile.Section(c.Key(sec.ID), sec.Title, func() {
			for _, k := range sec.Items {
				a.phoneRow(c, sb, sec.ID, k, 0)
			}
		})
	}
}

// phoneRow is row k of the sidebar as a phone's list row, and the rows
// under it: a group's sessions once it is open, and the workers a session
// started.
func (a *App) phoneRow(c *ui.Context, sb *state.Sidebar, section string, k state.SidebarKey, depth int) {
	t := c.Theme()
	r := a.sidebarRow(k)
	open := k.Kind == state.SidebarGroup && a.sidebar.IsOpen(section, k)
	subtitle := r.Tag
	if k.Kind == state.SidebarSession {
		if s, ok := sb.Session(k.Session); ok && s.Waits != "" {
			subtitle = joinNonEmpty(subtitle, "waits: "+s.Waits)
		}
	}
	icon := r.Icon
	row := mobile.ListRow(c.Key(k), mobile.RowOptions{Title: r.Title, Subtitle: subtitle,
		Disclosure: k.Kind != state.SidebarGroup,
		Leading: func() {
			if depth > 0 {
				ui.Box(c).Width(float32(depth) * 20)
			}
			ic := frostui.Icon(c, icon).FontSize(20)
			if icon == "pocket-waiting" {
				ic.TextColor(frostui.ToneColor(t, frostui.Warning))
			} else {
				ic.TextColor(t.Accent)
			}
		},
		Trailing: func() {
			ui.Row(c).Gap(frostui.SpaceS).AlignItems(ui.Center).Children(func() {
				if r.Alert != "" {
					frostui.Icon(c, "dialog-warning").TextColor(t.Warning).Tooltip(r.Alert)
				}
				if r.Busy {
					ui.Spinner(c).Size(14, 14).Label("running")
				}
				if r.Count != "" {
					frostui.Pill(c, r.Count, r.CountTone)
				}
				if k.Kind == state.SidebarGroup && sb.Children(k) != nil {
					frostui.Icon(c, chevron(open)).TextColor(t.TextMuted)
				}
			})
		},
	})
	// A keyboard reaches it with Tab, and opens it with Enter (Codex
	// review of #85, round 3).
	row.Focusable()
	if r.Dim {
		row.Opacity(0.6)
	}
	if _, ok := state.PinOf(k); ok {
		row.ContextMenu(func(m *ui.Menu) { a.sidebarMenu(m, k, section) })
	}
	if row.Clicked() {
		switch {
		case state.Searches(k):
			a.goSearch(a.filter)
		case k.Kind == state.SidebarSession:
			a.openSession(k.Session)
		case k.Kind == state.SidebarGroup:
			if sb.Folds(k) {
				a.sidebar.SetOpen(section, k, !open)
			}
		}
	}
	// The filter's results are the sessions it found, each on its own:
	// the sidebar left out the workers it did not find (Codex review of
	// #85, round 2).
	if (k.Kind == state.SidebarGroup && !open) || section == state.SectionFound {
		return
	}
	for _, child := range sb.Children(k) {
		a.phoneRow(c, sb, section, child, depth+1)
	}
}

// phoneMore is the More tab: the Settings (Appearance's Zoom, as a
// phone's list rather than the desktop's window), then what a desktop's
// main menu and status bar hold: the activity, the GitHub sign-in, About
// and Sign Out, the connection, the service's version and the device's
// name.
func (a *App) phoneMore(c *ui.Context) {
	t := c.Theme()
	item := func(key, title, icon string, danger bool, run func()) {
		if mobile.ListRow(c.Key(key), mobile.RowOptions{Title: title, Disclosure: !danger, Leading: func() {
			ic := frostui.Icon(c, icon).FontSize(20).TextColor(t.Accent)
			if danger {
				ic.TextColor(t.Danger)
			}
		}}).Focusable().Clicked() {
			run()
		}
	}
	ui.Scroll(c).Grow(1).Fill().Children(func() {
		ui.Column(c).Gap(frostui.SpaceL).Padding(frostui.SpaceM, 0, frostui.SpaceL, 0).AlignItems(ui.Stretch).Children(func() {
			// What the sessions used today and in the last 7 days, by
			// provider, once the week used something (ADR-0052): the
			// desktop's status bar holds it.
			if today, week, shown := a.st.UsageTotals(); shown {
				mobile.Section(c.Key("usage"), "Usage", func() {
					phoneUsage(c, "usage-today", "Today", today)
					phoneUsage(c, "usage-week", "Last "+itoa(state.WeekDays)+" days", week)
				})
			}
			mobile.Section(c.Key("appearance"), "Appearance", func() { a.phoneZoom(c) })
			mobile.Section(c.Key("app"), "PocketGo", func() {
				item("activity", "Activity", "utilities-terminal", false, func() { a.shell.Console.Open = true })
				switch {
				case a.gh.CanSignOut():
					item("github", "Sign Out of GitHub", "dialog-password", false, a.confirmGitHubSignOut)
				case a.gh.CanOpenSignIn():
					item("github", "Sign In to GitHub", "dialog-password", false, a.openGitHubSignIn)
				}
				item("about", "About PocketGo", "help-about", false, a.showAbout)
				item("sign-out", "Sign Out", "application-exit", true, a.confirmSignOut)
			})
			ui.Row(c).Padding(0, frostui.SpaceL+frostui.SpaceL).Gap(frostui.SpaceS).AlignItems(ui.Center).Wrap().Children(func() {
				frostui.ConnectionStatus(c, a.connection())
				for _, it := range a.serviceStatus() {
					frostui.Caption(c, it.Text).Wrap()
				}
			})
		})
	})
}

// phoneUsage is a total of More's Usage: its span and total, and a line
// for each provider under them. A row as a list row looks, but no button.
func phoneUsage(c *ui.Context, key, title string, total state.UsageTotal) {
	t := c.Theme()
	ui.Row(c.Key(key)).FillWidth().MinHeight(44).AlignItems(ui.Center).Padding(10, 16).Gap(12).
		Role(ui.RoleGroup).Label(total.Line(title)).Children(func() {
		frostui.Icon(c, "pocket-usage").FontSize(20).TextColor(t.Accent)
		ui.Column(c).Grow(1).Basis(0).Gap(2).Children(func() {
			ui.Row(c).Gap(frostui.SpaceS).AlignItems(ui.Center).Children(func() {
				ui.Text(c, title).Grow(1)
				ui.Text(c, total.Amount()).TextColor(t.TextMuted)
			})
			for _, p := range total.Providers {
				if line := state.UsageLine(p.Usage); line != "" {
					ui.Text(c, p.Provider+": "+line).FontSize(14).TextColor(t.TextMuted)
				}
			}
		})
	})
}

// phoneZoom is Settings' Zoom as a row of More's list, which says when
// the window shows less than the zoom chosen, as the Settings window does.
func (a *App) phoneZoom(c *ui.Context) {
	t := c.Theme()
	a.prefs.zoom = state.ZoomLabel(a.zoom())
	subtitle := "Text, icons and spacing together"
	if a.shownZoom < a.zoom() {
		subtitle = "Shown at " + state.ZoomLabel(a.shownZoom) + ": the window is too narrow for more"
	}
	// A row as a list row looks, but no button: the choice in it is the
	// control.
	ui.Row(c.Key("zoom")).FillWidth().MinHeight(44).AlignItems(ui.Center).Padding(10, 16).Gap(12).Children(func() {
		frostui.Icon(c, "zoom-in").FontSize(20).TextColor(t.Accent)
		ui.Column(c).Grow(1).Basis(0).Gap(2).Children(func() {
			ui.Text(c, "Zoom")
			ui.Text(c, subtitle).FontSize(14).TextColor(t.TextMuted)
		})
		if ui.Select(c, &a.prefs.zoom, state.ZoomLabels()).Label("Zoom").MinWidth(100).Changed() {
			if z, ok := state.ZoomOfLabel(a.prefs.zoom); ok {
				a.setZoom(z)
			}
		}
	})
}

// about is what About PocketGo says.
func (a *App) about() frostui.About {
	return frostui.About{
		Name: "PocketGo", Version: a.opts.Version,
		Summary: "The desktop client of pocketd, protocol version " + itoa(proto.Version) + ".",
		Website: "https://github.com/frostyard/pocketgo", License: "MIT",
	}
}

// joinNonEmpty joins the words given that are not empty with " · ".
func joinNonEmpty(parts ...string) string {
	out := ""
	for _, p := range parts {
		if p == "" {
			continue
		}
		if out != "" {
			out += " · "
		}
		out += p
	}
	return out
}
