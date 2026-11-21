package app

import (
	"path"
	"slices"
	"strconv"
	"time"

	"github.com/egoist/mygo/ui"
	"github.com/frostyard/frostui"

	"github.com/frostyard/pocketgo/desktop/internal/state"
	"github.com/frostyard/pocketgo/proto"
)

// The sidebar is frostyard/pocket's (internal/app/sidebar.go), ported with
// its look: a filter, the places, the sessions pinned, the team (the
// owner's own sessions, then one group per agent, with its sessions) and
// the recent sessions. A session's row pins and unpins from its
// right-click menu, as pocket's (the owner's choice: no button, no
// command). A
// thread's worker is a child row under the session that started it, drawn
// as any session's row, as pocket draws the sessions a session started.
// Its rules are state.Sidebar's; this file only draws them.

// The places, by their sidebar keys, in the order the sidebar lists them:
// a new place adds its key here, its row in placeRow and its page in
// goPlace.
const (
	sidebarInbox    = "inbox"
	sidebarBoard    = "board"
	sidebarTerminal = "terminal"
)

var sidebarPlaces = []string{sidebarInbox, sidebarBoard, sidebarTerminal}

// places are the sidebar's places on the window's platform: the Terminal
// only where the platform has it (state.HasTerminal, ADR-0040).
func (a *App) places() []string {
	if state.HasTerminal(a.opts.Platform) {
		return sidebarPlaces
	}
	return slices.DeleteFunc(slices.Clone(sidebarPlaces), func(p string) bool { return p == sidebarTerminal })
}

// sidebarFor is what the sidebar was made from: it is made again when
// one of them moved.
type sidebarFor struct {
	rev    uint64
	filter string
	seen   uint64
	pins   uint64
	minute int64
	server string
}

func (a *App) sidebarBar(c *ui.Context) {
	ui.Spacer(c)
	if frostui.IconButton(c, "pocket-new", "New Session").Clicked() {
		a.startNewSession()
	}
}

// setUpSidebar sets the sidebar's list up, as a new sign-in starts.
func (a *App) setUpSidebar() {
	a.sidebar = frostui.SourceList[state.SidebarKey]{Label: "Sessions"}
	a.filter, a.sb, a.sbFor = "", nil, sidebarFor{}
	l := &a.sidebar
	l.Children = func(k state.SidebarKey) []state.SidebarKey { return a.sidebarData().Children(k) }
	l.Row = a.sidebarRow
	l.OnSelect = func(k state.SidebarKey) {
		if !state.Shows(k) {
			return
		}
		// The keys browse: a session chosen with them shows, and the focus
		// stays in the list.
		switch k.Kind {
		case state.SidebarPlace:
			a.goPlace(k.Place)
		case state.SidebarSession:
			a.showSession(k.Session)
		}
	}
	l.OnClick = a.clickInSidebar
	l.Menu = a.sidebarMenu
	a.showInSidebar()
}

// clickInSidebar handles a click on row k, or Enter on it. On a session it
// puts the keyboard focus in its message field, which the arrow keys do
// not. On a group it folds or unfolds its sessions, as its arrow does, and
// moves the choice as the arrow would (state.Sidebar.AfterGroupClick).
func (a *App) clickInSidebar(k state.SidebarKey, _ ui.Modifiers) {
	if k.Kind == state.SidebarSession {
		a.composer.focus = true
		return
	}
	if k.Kind != state.SidebarGroup {
		return
	}
	sb := a.sidebarData()
	g, ok := sb.TopChoice(k)
	if !ok {
		return
	}
	folded := false
	if sb.Folds(k) {
		folded = a.sidebar.IsOpen(g.Section, k)
		a.sidebar.SetOpen(g.Section, k, !folded)
	}
	var before *state.Choice
	if key, section, chosen := a.sidebar.Selected(); chosen {
		before = &state.Choice{Section: section, Key: key}
	}
	var shown *state.SidebarKey
	if key, ok := a.shownKey(); ok {
		shown = &key
	}
	c := sb.AfterGroupClick(k, folded, before, shown)
	a.sidebar.Select(c.Section, c.Key)
}

// sidebarData is the sidebar as the store's data, the filter, the seen
// marks and the minute make it, made again only when one of them moved.
func (a *App) sidebarData() *state.Sidebar {
	now := a.opts.Now()
	key := sidebarFor{rev: a.st.Data.Rev(), filter: a.filter, seen: a.seenRev, pins: a.pinRev, minute: now.Unix() / 60, server: a.st.Conn.Server}
	if a.sb == nil || key != a.sbFor {
		a.sb = a.st.Data.Sidebar(state.SidebarOptions{Places: a.places(), Filter: a.filter, Seen: a.seenMarks(), Pinned: a.pins(), Now: now})
		a.sbFor = key
		// A new slice: the list may still read the one it drew with.
		a.sbSections = make([]frostui.SourceSection[state.SidebarKey], 0, len(a.sb.Sections))
		for _, sec := range a.sb.Sections {
			a.sbSections = append(a.sbSections, frostui.SourceSection[state.SidebarKey]{ID: sec.ID, Title: sec.Title, Items: sec.Items})
		}
	}
	return a.sb
}

// sidebarPane is the left pane: a filter and the list.
func (a *App) sidebarPane(c *ui.Context) {
	t := c.Theme()
	ui.Column(c).Grow(1).AlignItems(ui.Stretch).Background(frostui.Chrome(t)).Children(func() {
		ui.Row(c).Padding(frostui.SpaceS, frostui.SpaceS, 0, frostui.SpaceS).Children(func() {
			ui.SearchField(c, &a.filter).Placeholder("Filter sessions").Label("Filter sessions").Grow(1)
		})
		a.sessionsNote(c)
		a.profilesNote(c)
		// The row the main pane shows stays chosen, in Pinned or Recent
		// unless the owner chose it elsewhere: again once a filter that hid
		// it is gone.
		if _, _, chosen := a.sidebar.Selected(); !chosen {
			a.showInSidebar()
		}
		a.sidebarData()
		a.sidebar.View(c, a.sbSections).Grow(1).Padding(frostui.SpaceS)
	})
}

// sessionsNote says when the session list failed to load, is loading, or
// is empty: the sidebar says there is none only once it loaded.
func (a *App) sessionsNote(c *ui.Context) {
	d := &a.st.Data
	sessions := d.Sessions()
	status, problem := d.SessionsStatus()
	switch {
	case status == state.ListFailed:
		ui.Column(c).Padding(frostui.SpaceS, frostui.SpaceS, 0, frostui.SpaceS).Gap(frostui.SpaceXS).AlignItems(ui.Start).Children(func() {
			frostui.Problem(c, "Loading the sessions failed: "+problem).Wrap()
			if ui.Button(c, "Try Again").Clicked() {
				a.st.Refresh()
			}
		})
	case status == state.ListLoading && len(sessions) == 0:
		frostui.Loading(c, "Loading the sessions…").Padding(frostui.SpaceS, frostui.SpaceS, 0, frostui.SpaceS)
	case status == state.ListLoaded && len(sessions) == 0:
		frostui.Muted(c, "No sessions yet. Start one with New Session.").Wrap().Padding(frostui.SpaceS, frostui.SpaceS, 0, frostui.SpaceS)
	}
}

// profilesNote says when the profiles failed to load, and offers Try
// Again: until they load, the team names its agents by their ids, and
// lists only those with sessions (Codex review of #40, round 1).
func (a *App) profilesNote(c *ui.Context) {
	status, problem := a.st.Data.ProfilesStatus()
	if status != state.ListFailed {
		return
	}
	ui.Column(c).Padding(frostui.SpaceS, frostui.SpaceS, 0, frostui.SpaceS).Gap(frostui.SpaceXS).AlignItems(ui.Start).Children(func() {
		frostui.Problem(c, "Loading the agents' profiles failed: "+problem).Wrap()
		if ui.Button(c.Key("profiles"), "Try Again").Clicked() {
			a.st.LoadProfiles()
		}
	})
}

// shownKey is the sidebar's row of what the main pane shows.
func (a *App) shownKey() (state.SidebarKey, bool) {
	switch a.place {
	case placeInbox:
		return state.PlaceKey(sidebarInbox), true
	case placeBoard:
		return state.PlaceKey(sidebarBoard), true
	case placeTerminal:
		return state.PlaceKey(sidebarTerminal), true
	}
	if id := a.st.Data.OpenID(); id != 0 {
		return state.SessionKey(id), true
	}
	return state.SidebarKey{}, false
}

// showInSidebar chooses in the sidebar what the main pane shows, unless
// a row of it is chosen already: a session the owner chose under its
// agent stays chosen there, and a worker chosen under the session that
// started it stays chosen there. When no row shows it, no row stays
// chosen: one that did would stand for something else, and a click on it
// would not go there (state.Sidebar.ChoiceOf).
func (a *App) showInSidebar() {
	k, ok := a.shownKey()
	if !ok {
		return
	}
	if cur, _, chosen := a.sidebar.Selected(); chosen && cur.StandsFor() == k {
		return
	}
	if c, ok := a.sidebarData().ChoiceOf(k); ok {
		a.sidebar.Select(c.Section, c.Key)
		return
	}
	a.sidebar.ClearSelection()
}

// goPlace shows the place with the sidebar key.
func (a *App) goPlace(key string) {
	switch key {
	case sidebarInbox:
		a.goInbox()
	case sidebarBoard:
		a.goBoard()
	case sidebarTerminal:
		a.goTerminal()
	}
}

// sidebarRow is how the sidebar shows row k.
func (a *App) sidebarRow(k state.SidebarKey) frostui.SourceRow {
	switch k.Kind {
	case state.SidebarPlace:
		return a.placeRow(k.Place)
	case state.SidebarGroup:
		return a.groupRow(k.Agent)
	}
	return a.sessionRow(k.Session)
}

// placeRow is one of the window's places, with what waits there.
func (a *App) placeRow(key string) frostui.SourceRow {
	d := &a.st.Data
	switch key {
	case sidebarInbox:
		row := frostui.SourceRow{Title: "Inbox", Icon: "mail-unread"}
		n := 0
		for _, k := range d.Inbox() {
			n += k.Count
			if k.Status == state.ListFailed && row.Alert == "" {
				row.Alert = "Loading the " + k.List + " failed"
			}
		}
		if n > 0 {
			row.Count, row.CountTone = strconv.Itoa(n), frostui.Warning
			row.Spoken = plural(n, "thing waits for you", "things wait for you")
		}
		return row
	case sidebarBoard:
		row := frostui.SourceRow{Title: "Work Board", Icon: "view-grid"}
		threads, problem := d.ThreadsStatus()
		switch {
		case threads == state.ListFailed:
			row.Alert = "Loading the threads failed: " + problem
		case threads == state.ListLoaded && d.OnTheDesk() > 0:
			row.Count = strconv.Itoa(d.OnTheDesk())
			row.Spoken = plural(d.OnTheDesk(), "thread on the desk", "threads on the desk")
			row.Tooltip = row.Spoken
		}
		return row
	case sidebarTerminal:
		row := frostui.SourceRow{Title: "Terminal", Icon: "pocket-terminal"}
		if n := d.LiveTerminals(); n > 0 {
			row.Count = strconv.Itoa(n)
			row.Spoken = plural(n, "shell running", "shells running")
		}
		return row
	}
	return frostui.SourceRow{Title: key}
}

// groupRow is a group of sessions: an agent's, with its role, or the
// owner's own. It counts the sessions that wait for the owner, or spins
// while one runs.
func (a *App) groupRow(agent string) frostui.SourceRow {
	g, ok := a.sidebarData().Group(agent)
	if !ok {
		return frostui.SourceRow{Title: agent, Icon: "pocket-agent", Dim: true}
	}
	row := frostui.SourceRow{Title: g.Name, Icon: "pocket-agent", Tag: g.Badge, Tooltip: g.Hint, Alert: g.Problem, Dim: g.Unlisted}
	if agent == "" {
		row.Title, row.Icon = "You", "avatar-default"
	}
	switch {
	case g.Waiting > 0:
		row.Count, row.CountTone = strconv.Itoa(g.Waiting), frostui.Warning
		row.Spoken = strconv.Itoa(g.Waiting) + " waiting for you"
	case g.Running > 0:
		row.Busy = true
	}
	return row
}

// sessionRow is a session: its title, the agent it belongs to, and what
// it waits for, that it runs, or that it is NEW; dimmed when old.
func (a *App) sessionRow(id int64) frostui.SourceRow {
	r, ok := a.sidebarData().Session(id)
	if !ok {
		return frostui.SourceRow{Title: "…", Dim: true}
	}
	row := frostui.SourceRow{Title: r.Title, Icon: "pocket-session", Tag: r.Tag, Count: r.Badge, Dim: r.Dim}
	switch r.Mark {
	case state.MarkWaiting:
		// The raised hand says it waits, with or without a count.
		row.Icon, row.CountTone = "pocket-waiting", frostui.Warning
		row.Spoken = "waits for you"
		if r.Waits != "" {
			row.Spoken += ": " + r.Waits
		}
	case state.MarkRunning:
		row.Busy = true
	case state.MarkNew:
		row.CountTone = frostui.Success
		row.Spoken = "finished, not read"
	}
	return row
}

// seenMarks are the owner's seen marks for the service signed in to.
func (a *App) seenMarks() state.Seen { return a.settings.Seen[a.st.Conn.Server] }

// markSeen notes that the owner saw session id as it is now, and keeps
// the mark when it moved.
func (a *App) markSeen(id int64) {
	s, ok := a.st.Data.Session(id)
	server := a.st.Conn.Server
	if !ok || server == "" {
		return
	}
	marks, moved := a.seenMarks().Saw(s)
	if !moved {
		return
	}
	marks.Prune(a.opts.Now())
	if a.settings.Seen == nil {
		a.settings.Seen = map[string]map[int64]time.Time{}
	}
	a.settings.Seen[server] = marks
	a.seenRev++
	_ = a.saveSettings()
}

// pins are the owner's pins for the service signed in to.
func (a *App) pins() state.Pins { return a.settings.Pinned[a.st.Conn.Server] }

// sidebarMenu is the right-click menu of row k: a session's row pins its
// session, or unpins it, with a check that says whether it is pinned, as
// pocket's. Another row has no menu.
func (a *App) sidebarMenu(m *ui.Menu, k state.SidebarKey, _ string) {
	id, ok := state.PinOf(k)
	if !ok {
		return
	}
	if m.Item("Pinned").Checked(a.pins().Has(id)).Chosen() {
		a.togglePin(id)
	}
}

// togglePin pins session id, or unpins it, for the service signed in to,
// and keeps the pins. The session's rows move between Pinned and Recent:
// a row of them chosen there is chosen again where the session shows now.
func (a *App) togglePin(id int64) {
	server := a.st.Conn.Server
	if server == "" {
		return
	}
	if a.settings.Pinned == nil {
		a.settings.Pinned = map[string][]int64{}
	}
	a.settings.Pinned[server] = a.pins().Toggle(id)
	a.pinRev++
	_ = a.saveSettings()
	if k, section, chosen := a.sidebar.Selected(); chosen && !a.sidebarData().Lists(state.Choice{Section: section, Key: k}) {
		a.sidebar.ClearSelection()
		a.showInSidebar()
	}
}

// leaveSession notes that the owner saw the session shown as it is now, as
// the main pane leaves it.
func (a *App) leaveSession() {
	if a.place == placeSession {
		a.markSeen(a.st.Data.OpenID())
	}
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
