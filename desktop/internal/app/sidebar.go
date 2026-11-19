package app

import (
	"path"
	"strconv"

	"github.com/egoist/mygo/ui"
	"github.com/frostyard/frostui"

	"github.com/frostyard/pocketgo/proto"
)

func (a *App) sidebarBar(c *ui.Context) {
	ui.Text(c, "PocketGo").Bold().SingleLine().Grow(1)
	if frostui.IconButton(c, "list-add", "New Session").Clicked() {
		a.startNewSession()
	}
}

// sidebarPane lists the places: the Inbox, then every session with its
// state.
func (a *App) sidebarPane(c *ui.Context) {
	t := c.Theme()
	d := &a.st.Data
	ui.Scroll(c).Grow(1).Fill().Background(frostui.Chrome(t)).Children(func() {
		ui.Column(c).Padding(frostui.SpaceS).Gap(2).AlignItems(ui.Stretch).Children(func() {
			n := len(d.Approvals())
			a.placeRow(c, "inbox", "mail-unread", "Inbox", a.place == placeInbox, func() {
				if n > 0 {
					frostui.Pill(c, strconv.Itoa(n), frostui.Warning)
				}
			}, a.goInbox)
			frostui.Caption(c, "SESSIONS").Padding(frostui.SpaceM, frostui.SpaceS, frostui.SpaceXS, frostui.SpaceS)
			sessions := d.Sessions()
			if len(sessions) == 0 {
				frostui.Muted(c, "No sessions yet. Start one with New Session.").Wrap().Padding(frostui.SpaceS)
			}
			for _, s := range sessions {
				a.sessionRow(c, s)
			}
		})
	})
}

// placeRow is a row of the sidebar that shows a place when clicked.
func (a *App) placeRow(c *ui.Context, key, icon, title string, selected bool, trailing func(), open func()) {
	t := c.Theme()
	row := ui.ButtonBase(c.Key(key)).Row().Gap(frostui.SpaceS).Padding(6, frostui.SpaceS).Radius(6).
		AlignItems(ui.Center).Cursor(ui.CursorPointer).Label(title).AccessSelected(selected)
	switch {
	case selected:
		row.Background(frostui.ColorsOf(t).SelectedFill)
	case row.Hovered():
		row.Background(t.SurfaceHover)
	}
	if row.Clicked() {
		open()
	}
	row.Children(func() {
		frostui.Icon(c, icon)
		frostui.Strong(c, title).Grow(1).SingleLine()
		trailing()
	})
}

// sessionRow shows a session: its title, where it works, and its state.
func (a *App) sessionRow(c *ui.Context, s proto.Session) {
	t := c.Theme()
	selected := a.place == placeSession && a.st.Data.OpenID() == s.ID
	st := a.st.Data.StateOf(s.ID)
	row := ui.ButtonBase(c.Key(s.ID)).Column().Gap(2).Padding(6, frostui.SpaceS).Radius(6).
		AlignItems(ui.Stretch).Cursor(ui.CursorPointer).Label(s.Title + ", " + string(st)).AccessSelected(selected)
	switch {
	case selected:
		row.Background(frostui.ColorsOf(t).SelectedFill)
	case row.Hovered():
		row.Background(t.SurfaceHover)
	}
	if row.Clicked() {
		a.openSession(s.ID)
	}
	row.Children(func() {
		ui.Row(c).Gap(frostui.SpaceS).AlignItems(ui.Center).Children(func() {
			frostui.Strong(c, s.Title).Grow(1).SingleLine()
			statePill(c, st)
		})
		frostui.Caption(c, shortPath(s.Cwd)+" · "+s.Model.ID).SingleLine()
	})
}

// statePill shows a session's state in its tone.
func statePill(c *ui.Context, st proto.SessionState) {
	switch st {
	case proto.SessionWaiting:
		frostui.Pill(c, "waiting", frostui.Warning)
	case proto.SessionRunning:
		frostui.Pill(c, "running", frostui.Accent)
	default:
		frostui.Pill(c, "idle", frostui.Neutral)
	}
}

// shortPath is the last two elements of a path, which tell projects apart.
func shortPath(p string) string {
	dir, base := path.Split(path.Clean(p))
	parent := path.Base(dir)
	if parent == "/" || parent == "." || parent == "" {
		return base
	}
	return parent + "/" + base
}
