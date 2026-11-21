package app

import (
	"github.com/egoist/mygo/ui"
	"github.com/egoist/mygo/ui/mobile"
	"github.com/frostyard/frostui"

	"github.com/frostyard/pocketgo/desktop/internal/state"
	"github.com/frostyard/pocketgo/proto"
)

// The compact window is a phone's (ADR-0042, chosen by state.Compact): one
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
	// activity and about: their sheets are open.
	activity, about bool
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
	a.table.Bind(c)
	conn := a.connection()
	if conn.State == frostui.Connecting {
		frostui.ConnectingScreen(c, conn)
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
					if a.shownSession() != nil {
						a.backButton(c)
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
		ui.Column(c.Key(int(a.place))).Grow(1).Basis(0).MinHeight(0).AlignItems(ui.Stretch).Children(func() {
			switch {
			case a.shownSession() != nil:
				a.sessionPane(c)
			case a.place == placeBoard:
				a.boardPane(c)
			case a.place == placeSessions:
				a.phoneSessions(c)
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
	mobile.Sheet(c, &a.phone.activity, mobile.SheetOptions{Detents: []float32{0.6, 0.95}, Label: "Activity"}, func() {
		ui.Column(c).Grow(1).Basis(0).MinHeight(0).AlignItems(ui.Stretch).Children(func() { a.shell.Console.View(c, a.console) })
	})
	mobile.Sheet(c, &a.phone.about, mobile.SheetOptions{Detents: []float32{0.5}, Label: "About PocketGo"}, func() {
		ui.Column(c).Padding(frostui.SpaceS, frostui.SpaceL).Gap(frostui.SpaceM).AlignItems(ui.Stretch).Children(func() {
			frostui.AboutBody(a.about())(c)
		})
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
	return a.phone.tab.Title()
}

// compactNote is the line under the top bar, which a desktop's main bar
// holds under its title: what waits in the Inbox, what is on the board,
// or where the session works and its state.
func (a *App) compactNote(c *ui.Context) {
	o := a.shownSession()
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
		note = joinNonEmpty(shortPath(s.Cwd), s.Model.ID, thinking)
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

// backButton goes back from a session to its tab's view (state.BackTo).
func (a *App) backButton(c *ui.Context) {
	t := c.Theme()
	tab := a.phone.tab
	b := ui.ButtonBase(c).Row().Gap(2).AlignItems(ui.Center).Padding(0, frostui.SpaceS, 0, frostui.SpaceXS).Height(44).
		Cursor(ui.CursorPointer).Label("Back to " + tab.Title())
	if b.Pressed() {
		b.Opacity(0.4)
	}
	b.Children(func() {
		frostui.Icon(c, "go-previous").FontSize(20).TextColor(t.Accent)
		ui.Text(c, tab.Title()).FontSize(17).TextColor(t.Accent).SingleLine()
	})
	if b.Clicked() {
		a.goPlaceNamed(state.BackTo(tab))
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
	if r.Dim {
		row.Opacity(0.6)
	}
	if _, ok := state.PinOf(k); ok {
		row.ContextMenu(func(m *ui.Menu) { a.sidebarMenu(m, k, section) })
	}
	if row.Clicked() {
		switch k.Kind {
		case state.SidebarSession:
			a.openSession(k.Session)
		case state.SidebarGroup:
			if sb.Folds(k) {
				a.sidebar.SetOpen(section, k, !open)
			}
		}
	}
	if k.Kind == state.SidebarGroup && !open {
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
		}}).Clicked() {
			run()
		}
	}
	ui.Scroll(c).Grow(1).Fill().Children(func() {
		ui.Column(c).Gap(frostui.SpaceL).Padding(frostui.SpaceM, 0, frostui.SpaceL, 0).AlignItems(ui.Stretch).Children(func() {
			mobile.Section(c.Key("appearance"), "Appearance", func() { a.phoneZoom(c) })
			mobile.Section(c.Key("app"), "PocketGo", func() {
				item("activity", "Activity", "utilities-terminal", false, func() { a.phone.activity = true })
				switch {
				case a.gh.CanSignOut():
					item("github", "Sign Out of GitHub", "dialog-password", false, a.confirmGitHubSignOut)
				case a.gh.CanOpenSignIn():
					item("github", "Sign In to GitHub", "dialog-password", false, a.openGitHubSignIn)
				}
				item("about", "About PocketGo", "help-about", false, func() { a.phone.about = true })
				item("sign-out", "Sign Out", "application-exit", true, a.confirmSignOut)
			})
			ui.Row(c).Padding(0, frostui.SpaceL+frostui.SpaceL).Gap(frostui.SpaceS).AlignItems(ui.Center).Wrap().Children(func() {
				frostui.ConnectionStatus(c, a.connection())
				for _, it := range a.statusEnd() {
					frostui.Caption(c, it.Text).Wrap()
				}
			})
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
