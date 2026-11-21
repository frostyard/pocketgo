package app

import (
	"strconv"

	"github.com/egoist/mygo/ui"
	"github.com/egoist/mygo/ui/mobile"
	"github.com/frostyard/frostui"

	"github.com/frostyard/pocketgo/desktop/internal/state"
	"github.com/frostyard/pocketgo/proto"
)

// The compact window is a phone's (the phone client's Phase 3, chosen by
// state.Compact): one view at a time, full width, under a compact top bar,
// in place of the pane shell's sidebar beside the main pane. The views are
// the desktop's own; only the way between them differs, by
// Options.CompactNav, as MyGo's ui/mobile draws a phone's bars, sheets and
// lists.

// The compact window's ways between its places and sessions
// (Options.CompactNav).
const (
	// NavTabs is a bar of tabs at the bottom: the Inbox, the Work Board,
	// the Sessions and More. A session opened from a tab shows in its
	// place, with a back button to the tab in the top bar.
	NavTabs = "tabs"
	// NavSheet is one view at a time, under a top bar whose button opens
	// the desktop's sidebar (its places, pinned sessions, team and recent
	// sessions) as a sheet over the view.
	NavSheet = "sheet"
)

// phoneNav is the compact window's navigation.
type phoneNav struct {
	// tab is the tab of phoneTabs shown (NavTabs): the one whose place
	// shows, or that the session shown was opened from.
	tab int
	// sheet: the sidebar's sheet is open (NavSheet); search shows its
	// filter.
	sheet, search bool
	// activity and about: their sheets are open.
	activity, about bool
}

// phoneTab is a tab of NavTabs: its place, and how its button shows.
type phoneTab struct {
	title, icon string
	place       place
}

var phoneTabs = []phoneTab{
	{"Inbox", "mail-unread", placeInbox},
	{"Board", "view-grid", placeBoard},
	{"Sessions", "pocket-session", placeSessions},
	{"More", "view-more-horizontal", placeMore},
}

// tabs reports whether the compact window goes by tabs, the default.
func (a *App) tabs() bool { return a.opts.CompactNav != NavSheet }

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
	tabs := a.tabs()
	if tabs {
		for i, tb := range phoneTabs {
			if tb.place == a.place {
				a.phone.tab = i
			}
		}
	}
	bar := frostui.Chrome(t)
	ui.Column(c).Fill().AlignItems(ui.Stretch).Background(t.Background).Children(func() {
		mobile.TopBar(c, mobile.TopBarOptions{Title: a.compactTitle(), Background: &bar,
			Leading: func() {
				switch {
				case !tabs:
					a.sheetButton(c)
				case a.shownSession() != nil:
					a.backButton(c)
				}
			},
			Trailing: func() { a.compactActions(c, tabs) },
		})
		a.compactNote(c)
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
		// A session is a conversation: its composer takes the bottom, as
		// a phone's messages do, and the back button goes to its tab.
		if tabs && a.shownSession() == nil {
			ui.Box(c).Height(1).Background(t.Border).Shrink(0)
			a.tabBar(c)
		}
	})
	if !tabs {
		a.sidebarSheet(c)
	}
	a.compactOverlays(c)
}

// compactOverlays are what shows over the compact window: its toasts, its
// confirmations and its dialogs and sheets.
func (a *App) compactOverlays(c *ui.Context) {
	frostui.TopToasts(c, "Close")
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

// shownSession is the session the window shows, nil while it shows a
// place. A button drawn earlier in the frame may have left it.
func (a *App) shownSession() *state.OpenSession {
	if a.place != placeSession {
		return nil
	}
	return a.st.Data.Open()
}

// compactTitle is the top bar's title: the place's, or the session's.
func (a *App) compactTitle() string {
	switch a.place {
	case placeSession:
		if o := a.st.Data.Open(); o != nil {
			s, _ := a.st.Data.Session(o.ID)
			return sessionTitle(s, o.ID)
		}
	case placeBoard:
		return "Work Board"
	case placeSessions:
		return "Sessions"
	case placeMore:
		return "More"
	}
	return "Inbox"
}

// compactNote is the line under the top bar, which a desktop's main bar
// holds under its title: what waits in the Inbox, what is on the board,
// or where the session works and its state.
func (a *App) compactNote(c *ui.Context) {
	t := c.Theme()
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
	ui.Row(c).Gap(frostui.SpaceS).AlignItems(ui.Center).Padding(0, a.gutter(), frostui.SpaceS, a.gutter()).
		Background(frostui.Chrome(t)).Shrink(0).Children(func() {
		frostui.Caption(c, note).SingleLine().Grow(1).Shrink(1).MinWidth(0)
		if o != nil {
			statePill(c, a.st.Data.StateOf(o.ID))
		}
	})
}

// compactActions are the top bar's buttons at its end: the session's,
// and New Session where the sessions are.
func (a *App) compactActions(c *ui.Context, tabs bool) {
	ui.Row(c).Gap(frostui.SpaceXS).AlignItems(ui.Center).Children(func() {
		if o := a.shownSession(); o != nil {
			s, _ := a.st.Data.Session(o.ID)
			a.sessionActions(c, o, s)
			return
		}
		if !tabs || a.place == placeSessions {
			if barButton(c, "pocket-new", "New Session").Clicked() {
				a.startNewSession()
			}
		}
	})
}

// backButton goes back from a session to the tab it was opened from.
func (a *App) backButton(c *ui.Context) {
	t := c.Theme()
	tab := phoneTabs[a.phone.tab]
	b := ui.ButtonBase(c).Row().Gap(2).AlignItems(ui.Center).Padding(0, frostui.SpaceS, 0, frostui.SpaceXS).Height(44).
		Cursor(ui.CursorPointer).Label("Back to " + tab.title)
	b.Children(func() {
		frostui.Icon(c, "go-previous").FontSize(20).TextColor(t.Accent)
		ui.Text(c, tab.title).FontSize(17).TextColor(t.Accent).SingleLine()
	})
	if b.Clicked() {
		a.goTab(a.phone.tab)
	}
}

// sheetButton opens the sidebar's sheet (NavSheet), with what waits in
// the Inbox while another place shows.
func (a *App) sheetButton(c *ui.Context) {
	t := c.Theme()
	ui.Box(c).Children(func() {
		if barButton(c, "sidebar-show", "Places and Sessions").Clicked() {
			a.phone.sheet, a.phone.search = true, false
		}
		if n := a.inboxCount(); n > 0 && a.place != placeInbox {
			ui.Text(c, strconv.Itoa(n)).Absolute().Top(4).Right(2).FontSize(10).PaddingX(4).Radius(8).
				Background(t.Danger).TextColor(ui.RGB(255, 255, 255))
		}
	})
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

// inboxCount is how many things wait in the Inbox, as its row counts them.
func (a *App) inboxCount() int {
	n := 0
	for _, k := range a.st.Data.Inbox() {
		n += k.Count
	}
	return n
}

// tabBar is NavTabs' bar of tabs, the Inbox's with what waits there.
func (a *App) tabBar(c *ui.Context) {
	buttons := make([]mobile.BarButton, len(phoneTabs))
	for i, tb := range phoneTabs {
		buttons[i] = mobile.BarButton{Label: tb.title, Icon: barIcon(tb.icon)}
		if tb.place == placeInbox {
			if n := a.inboxCount(); n > 0 {
				buttons[i].Badge = strconv.Itoa(n)
			}
		}
	}
	tab := a.phone.tab
	if mobile.BottomBar(c, &tab, buttons...).Changed() {
		a.goTab(tab)
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

// goTab shows tab i's place.
func (a *App) goTab(i int) {
	a.phone.tab = i
	switch p := phoneTabs[i].place; p {
	case placeInbox:
		a.goInbox()
	case placeBoard:
		a.goBoard()
	default:
		a.leaveSession()
		a.place = p
		a.st.Open(0)
		a.showInSidebar()
	}
}

// sidebarSheet is NavSheet's sheet: the sidebar's places and sessions,
// then what a desktop's main menu holds. Choosing a place or a session
// closes it and shows that. The filter waits behind its button: the sheet
// takes the focus as it opens, and a field with it would bring a phone's
// keyboard up over the list.
func (a *App) sidebarSheet(c *ui.Context) {
	done := func() { a.phone.sheet, a.phone.search = false, false }
	mobile.Sheet(c, &a.phone.sheet, mobile.SheetOptions{Detents: []float32{0.8, 0.95}, Label: "Places and Sessions"}, func() {
		ui.Column(c).Grow(1).Basis(0).MinHeight(0).AlignItems(ui.Stretch).Children(func() {
			ui.Row(c).Gap(frostui.SpaceS).AlignItems(ui.Center).Padding(0, frostui.SpaceL, frostui.SpaceS, frostui.SpaceL).Children(func() {
				ui.Text(c, "Go to").FontSize(17).FontWeight(600).Grow(1)
				if barButton(c, "edit-find", "Filter Sessions").Clicked() {
					a.phone.search = !a.phone.search
					if !a.phone.search {
						a.filter = ""
					}
				}
				if barButton(c, "pocket-new", "New Session").Clicked() {
					done()
					a.startNewSession()
				}
			})
			if a.phone.search || a.filter != "" {
				ui.Row(c).Padding(0, frostui.SpaceL, frostui.SpaceS, frostui.SpaceL).Children(func() {
					ui.SearchField(c, &a.filter).Placeholder("Filter sessions").Label("Filter sessions").AutoFocus().Grow(1)
				})
			}
			ui.Scroll(c).Grow(1).Basis(0).MinHeight(0).Children(func() {
				ui.Column(c).Gap(frostui.SpaceL).Padding(0, 0, frostui.SpaceL, 0).AlignItems(ui.Stretch).Children(func() {
					a.sessionsNote(c)
					a.profilesNote(c)
					a.phoneList(c, true, done)
					a.phoneMenu(c, done)
				})
			})
		})
	})
}

// phoneSessions is NavTabs' Sessions tab: the sidebar's filter and its
// sessions, pinned, by the team and recent.
func (a *App) phoneSessions(c *ui.Context) {
	ui.Scroll(c).Grow(1).Fill().Children(func() {
		ui.Column(c).Gap(frostui.SpaceL).Padding(frostui.SpaceM, 0, frostui.SpaceL, 0).AlignItems(ui.Stretch).Children(func() {
			ui.Row(c).Padding(0, frostui.SpaceL).Children(func() {
				ui.SearchField(c, &a.filter).Placeholder("Filter sessions").Label("Filter sessions").Grow(1)
			})
			a.sessionsNote(c)
			a.profilesNote(c)
			a.phoneList(c, false, func() {})
		})
	})
}

// phoneMore is NavTabs' More tab: what a desktop's main menu and status
// bar hold.
func (a *App) phoneMore(c *ui.Context) {
	ui.Scroll(c).Grow(1).Fill().Children(func() {
		ui.Column(c).Gap(frostui.SpaceL).Padding(frostui.SpaceM, 0, frostui.SpaceL, 0).AlignItems(ui.Stretch).Children(func() {
			a.phoneMenu(c, func() {})
		})
	})
}

// phoneList lists the sidebar's sections as a phone's grouped lists: its
// places when places, then the pinned sessions, the team's groups and the
// recent sessions. Choosing a place or a session shows it, then calls
// chose.
func (a *App) phoneList(c *ui.Context, places bool, chose func()) {
	sb := a.sidebarData()
	for _, sec := range sb.Sections {
		if (sec.ID == state.SectionPlaces && !places) || len(sec.Items) == 0 {
			continue
		}
		mobile.Section(c.Key(sec.ID), sec.Title, func() {
			for _, k := range sec.Items {
				a.phoneRow(c, sb, sec.ID, k, 0, chose)
			}
		})
	}
}

// phoneRow is row k of the sidebar as a phone's list row, and the rows
// under it: a group's sessions once it is open, and the workers a session
// started.
func (a *App) phoneRow(c *ui.Context, sb *state.Sidebar, section string, k state.SidebarKey, depth int, chose func()) {
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
		case state.SidebarPlace:
			a.goPlace(k.Place)
			chose()
		case state.SidebarSession:
			// No keyboard yet: a phone's comes up over half the
			// transcript.
			a.showSession(k.Session)
			chose()
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
		a.phoneRow(c, sb, section, child, depth+1, chose)
	}
}

// phoneMenu is what a desktop's main menu and status bar hold, as a
// phone's list: Settings, the activity, the GitHub sign-in, About and
// Sign Out, then the service's version and the device's name.
func (a *App) phoneMenu(c *ui.Context, chose func()) {
	t := c.Theme()
	item := func(key, title, icon string, danger bool, run func()) {
		if mobile.ListRow(c.Key(key), mobile.RowOptions{Title: title, Disclosure: !danger, Leading: func() {
			ic := frostui.Icon(c, icon).FontSize(20).TextColor(t.Accent)
			if danger {
				ic.TextColor(t.Danger)
			}
		}}).Clicked() {
			chose()
			run()
		}
	}
	mobile.Section(c.Key("menu"), "App", func() {
		item("settings", "Settings", "preferences-system", false, func() { a.openSettings("") })
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
	ui.Row(c).Padding(0, frostui.SpaceL+frostui.SpaceL).Gap(frostui.SpaceS).AlignItems(ui.Center).Children(func() {
		frostui.ConnectionStatus(c, a.connection())
		for _, it := range a.statusEnd() {
			frostui.Caption(c, it.Text).Wrap().Grow(1)
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
