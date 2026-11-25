// Package nsgoclient presents the existing pocketd store on NSGo. The store
// and its signals both belong to the NSGo engine goroutine.
package nsgoclient

import (
	"fmt"
	"maps"
	"net/http"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/frostyard/frostui/workspaceui"
	"github.com/frostyard/pocketgo/desktop/internal/state"
	"github.com/frostyard/pocketgo/proto"
	"github.com/nsgo-dev/nsgo/core"
	"github.com/nsgo-dev/nsgo/icons/lucide"
	"github.com/nsgo-dev/nsgo/ui"
)

type client struct {
	// overTab is the tab a compact window's session shows over, and
	// openedFrom the place it was opened from, which its way back goes to.
	overTab    state.Tab
	openedFrom string
	// pendingNotice is a notification clicked before the kept sign-in
	// connected: what it is about opens once it has.
	pendingNotice *state.NoticeTarget
	zoom          *core.Signal[float32]

	prefs   preferences
	groups  *core.Signal[map[string]bool]
	reasons map[string]string

	terminals      map[string]*openTerm
	termID         *core.Signal[string]
	creds          credentials
	credentialDone chan struct{}
	gh             *state.GitHub
	generation     int

	cx             *core.Scope
	store          *state.Store
	revision       *core.Signal[int]
	page, filter   *core.Signal[string]
	notices        *core.Signal[*workspaceui.Notice]
	session        *core.Signal[int64]
	seek           *core.Signal[int64]
	nav            navigation
	browserHistory bool
	fileRoute      *core.Signal[destination]
	fileRefresh    *core.Signal[int]
	noticeCard     *core.Signal[string]
	drafts         map[int64]*core.Signal[string]
	log            []activityLine
	// asking is the confirmation shown, nil with none; asks holds it and
	// those that wait behind it.
	asking *core.Signal[*question]
	asks   asks
	// launchHours and launchClock are the end chooser of the board's
	// proposals: in state.LaunchChoiceHours hours, or until a clock time.
	launchHours bool
	launchClock string
	// newOpen says whether New session's dialog shows, and newForm counts
	// its openings: each has a form of its own.
	newOpen *core.Signal[bool]
	newForm *core.Signal[int]
	// titleEdit is the edit of the shown session's title, in place of
	// the title, and titleDraft what its field shows.
	titleEdit  state.TitleEdit
	titleDraft *core.Signal[string]
}

func Root(cx *core.Scope) core.Node { return buildClient(cx, transport(), true) }

func newClient(cx *core.Scope, transport http.RoundTripper) core.Node {
	return buildClient(cx, transport, false)
}
func buildClient(cx *core.Scope, transport http.RoundTripper, production bool) core.Node {
	var accounts func(*client)
	if production {
		accounts = (*client).platformAccounts
	}
	return startClient(cx, transport, production, accounts)
}

// startClient builds the client. accounts, when set, gives it what a
// device keeps for it, the device token's store and the GitHub sign-in:
// the app's own is the platform's (platformAccounts), and a test gives the
// credentials a keyring would hold. With it the client takes the
// platform's notification presses and badge, and signs in with a kept
// token.
func startClient(cx *core.Scope, transport http.RoundTripper, production bool, accounts func(*client)) core.Node {
	cx.SetTitle("PocketGo")
	// The Frostyard theme is set even when the platform refuses the bundled
	// font: the text then shows in the system's sans-serif face.
	fontErr := workspaceui.Apply(cx)
	c := &client{cx: cx, revision: core.NewSignal(cx, 0), page: core.NewSignal(cx, "Inbox"),
		session: core.NewSignal(cx, int64(0)), notices: core.NewSignal(cx, (*workspaceui.Notice)(nil)), filter: core.NewSignal(cx, ""), drafts: map[int64]*core.Signal[string]{}}
	c.seek = core.NewSignal(cx, int64(0))
	c.asking = core.NewSignal(cx, (*question)(nil))
	c.newOpen, c.newForm = core.NewSignal(cx, false), core.NewSignal(cx, 0)
	c.titleDraft = core.NewSignal(cx, "")
	if fontErr != nil {
		c.logLine(state.LevelWarn, "Register the Inter font: "+fontErr.Error())
	}
	c.restoreAppearance()
	c.browserHistory = production && runtime.GOOS == "js"
	c.fileRoute = core.NewSignal(cx, destination{})
	c.fileRefresh = core.NewSignal(cx, 0)
	c.nav.revision = core.NewSignal(cx, 0)
	c.resetHistory()
	if c.browserHistory {
		cx.OnAppURLChange(c.onAddress)
	}
	c.noticeCard = core.NewSignal(cx, "")
	c.groups = core.NewSignalFunc(cx, map[string]bool{}, maps.Equal)
	c.reasons = map[string]string{}
	c.launchClock = proto.DefaultLaunchUntil
	var zoom float32
	c.loadJSON("zoom", &zoom)
	c.zoom = core.NewSignal(cx, state.Zoom(zoom))
	c.termID = core.NewSignal(cx, "")
	c.terminals = map[string]*openTerm{}
	cx.Cleanup(c.closeTerminals)
	c.store = state.New(state.Options{Transport: transport, Now: func() time.Time { return timeNow() }, Post: func(fn func()) {
		cx.Post(func() { cx.Batch(func() { fn(); c.changed() }) })
	}, Notify: c.notify, KeptNotices: func(server string) []string { var keys []string; c.loadJSON("notified/"+server, &keys); return keys }, KeepNotices: func(server string, keys []string) { c.saveJSON("notified/"+server, keys) }, Log: func(level state.Level, text string) {
		c.logLine(level, text)
		if level == state.LevelError && !shownInPlace(text) {
			c.showError(text)
		}
	}, SignInChanged: func() {
		c.generation++
		c.loadPrefs()
		if c.gh != nil {
			c.gh.PocketdSignInChanged()
		}
	}, SignedOut: func(server, reason string) {
		c.forget(server)
		c.reset()
		c.showNotice(ui.ToastWarning, "Signed out", reason)
	}})
	cx.Cleanup(c.store.Close)
	if accounts != nil {
		cx.OnNotificationPress(c.openNotice)
		cx.Effect(func() { c.revision.Get(); cx.SetBadge(state.InboxCount(c.store.Data.Inbox())) })
		accounts(c)
		c.restore()
	}
	c.zoomKeys(cx)
	// The frame paints the ground and sets the text's color and font; the
	// view inside it only zooms.
	return workspaceui.Frame(core.View{TestID: "zoom", Style: core.Style{TransformOrigin: core.ObjectPosition{X: core.PositionStart, Y: core.PositionStart}}, StyleFunc: func() core.Style {
		z := c.shownZoom()
		return core.Style{Width: core.Pct(100 / z), Height: core.Pct(100 / z), Transform: core.Transform{Scale: z}}
	}, Children: []core.Node{
		core.Show{When: func() bool { c.revision.Get(); return c.store.SignedIn() }, Then: c.workspace, Else: c.signIn},
		core.Dynamic(func(*core.Scope) core.Node {
			if notice := c.notices.Get(); notice != nil {
				return notice
			}
			return nil
		}),
		core.Dynamic(c.alert),
	}})
}

func (c *client) changed() {
	if len(c.terminals) > 0 {
		var ids []string
		for k := range c.terminals {
			ids = append(ids, k)
		}
		for _, id := range c.store.Data.TerminalsGone(ids) {
			_ = c.terminals[id].Close()
			delete(c.terminals, id)
		}
	}
	c.revision.Update(func(n int) int { return n + 1 })
}

func (c *client) reset() {
	c.closeTerminals()
	c.termID.Set("")
	c.seek.Set(0)
	c.resetHistory()
	c.launchHours, c.launchClock = false, proto.DefaultLaunchUntil
	c.asks.clear()
	c.asking.Set(nil)
	c.newOpen.Set(false)
	c.titleEdit.Cancel()
	c.noticeCard.Set("")
	clear(c.reasons)
	c.groups.Set(map[string]bool{})
	clear(c.drafts)
	c.page.Set("Inbox")
	c.session.Set(0)
	c.filter.Set("")
	c.log = nil
	c.changed()
}

func (c *client) openSession(id int64) {
	if id == 0 {
		return
	}
	c.goTo(destination{page: "Session", session: id})
}

func (c *client) navigate(id string) {
	if value, found := strings.CutPrefix(id, "session/"); found {
		n, err := strconv.ParseInt(value, 10, 64)
		if err == nil {
			c.openSession(n)
		}
		return
	}
	// New session is a dialog over the place shown, not a place.
	if id == "New session" {
		c.showNewSession()
		return
	}
	if id == "search-filter" {
		c.store.Search(c.filter.Get())
		id = "Search"
	}
	c.goTo(destination{page: id})
}

func sessionName(s proto.Session) string {
	if s.Title != "" {
		return s.Title
	}
	return fmt.Sprintf("Session %d", s.ID)
}
func (c *client) discloseGroup(id string) bool {
	if strings.HasPrefix(id, "empty-group/") {
		return true
	}
	agent, ok := strings.CutPrefix(id, "group/")
	if !ok {
		return false
	}
	next := maps.Clone(c.groups.Get())
	if next[agent] {
		delete(next, agent)
	} else {
		next[agent] = true
	}
	c.groups.Set(next)
	return true
}

func (c *client) sections() []ui.SidebarSection {
	c.revision.Get()
	// A place's row is the kit's: its icon, its name, and for the Inbox how
	// much waits there.
	place := func(id string, icon func() *core.SVG) ui.SidebarItem {
		return ui.SidebarItem{ID: id, Text: id, Content: workspaceui.NavLabel{Icon: icon, Text: id}}
	}
	items := []ui.SidebarItem{
		place("Inbox", lucide.Inbox), place("Work board", lucide.Columns3), place("Search", lucide.Search),
		place("Files", lucide.FolderOpen), place("Terminal", lucide.Terminal), place("Activity", lucide.Activity),
	}
	// The Terminal place shows only where the platform has it.
	terminal := state.HasTerminal(c.goos())
	if inbox := c.store.Data.Inbox(); state.InboxBadge(inbox) != "" {
		items[0].Text = "Inbox, " + state.InboxNote(inbox)
		items[0].Content = workspaceui.NavLabel{Icon: lucide.Inbox, Text: "Inbox", Count: state.InboxCount(inbox), Unread: true}
	}
	// The Work board's row counts the threads on the desk, once they
	// loaded, and the Terminal's the shells that run.
	d := &c.store.Data
	if status, _ := d.ThreadsStatus(); status == state.ListLoaded && d.OnTheDesk() > 0 {
		items[1].Text = "Work board, " + counted(d.OnTheDesk(), "thread on the desk", "threads on the desk")
		items[1].Content = workspaceui.NavLabel{Icon: lucide.Columns3, Text: "Work board", Count: d.OnTheDesk()}
	}
	if n := d.LiveTerminals(); n > 0 {
		items[4].Text = "Terminal, " + counted(n, "shell running", "shells running")
		items[4].Content = workspaceui.NavLabel{Icon: lucide.Terminal, Text: "Terminal", Count: n}
	}
	if !terminal {
		items = slices.DeleteFunc(items, func(i ui.SidebarItem) bool { return i.ID == "Terminal" })
	}
	sections := []ui.SidebarSection{{Title: "Workspace", Items: items}}
	tree := c.store.Data.Sidebar(state.SidebarOptions{Seen: c.prefs.Seen, Pinned: c.prefs.Pinned, Filter: c.filter.Get(), Now: time.Now()})
	var add func(*ui.SidebarSection, state.SidebarKey, int, bool)
	add = func(section *ui.SidebarSection, k state.SidebarKey, depth int, last bool) {
		switch k.Kind {
		case state.SidebarSearch:
			// The row says what it searches for, as far as the words
			// make a search.
			offer := "Search all sessions"
			if text, ok := state.SearchOffer(c.filter.Get()); ok {
				offer = text
			}
			section.Items = append(section.Items, ui.SidebarItem{ID: "search-filter", Text: offer, Icon: lucide.Search})
		case state.SidebarGroup:
			g, _ := tree.Group(k.Agent)
			name := g.Name
			if name == "" {
				name = "Your sessions"
			}
			item := ui.SidebarItem{ID: "empty-group/" + k.Agent, Text: name}
			children := tree.Children(k)
			if len(children) > 0 {
				item.ID = "group/" + k.Agent
				open := c.groups.Get()[k.Agent]
				item.Expanded = func() bool { return open }
			}
			activity, description := agentActivity(g)
			if description != "" {
				item.Text = name + ", " + description
			}
			// Its role when it is a supervisor or a clerk, and why its
			// profile cannot start a session.
			var marks []core.Node
			if g.Badge != "" {
				item.Text += ", " + g.Badge
				marks = append(marks, ui.Badge{Text: g.Badge})
			}
			if g.Problem != "" {
				item.Text += ", " + g.Problem
				marks = append(marks, core.Func(func(cx *core.Scope) core.Node {
					return ui.Tooltip{Text: g.Problem, Trigger: ui.Icon(lucide.TriangleAlert, 14, ui.ColorsOf(cx).Warn)}
				}))
			}
			if activity != nil {
				marks = append(marks, activity)
			}
			var trailing core.Node
			if len(marks) > 0 {
				trailing = core.View{Style: core.Style{Direction: core.Row, AlignItems: core.AlignCenter, Gap: workspaceui.SpaceXS, FlexShrink: 0}, Children: marks}
			}
			item.Content = workspaceui.BranchLabel{Text: name, Icon: lucide.Users, Expanded: item.Expanded, Trailing: trailing}
			section.Items = append(section.Items, item)
			if item.Expanded != nil && item.Expanded() {
				for i, kid := range children {
					add(section, kid, depth+1, i == len(children)-1)
				}
			}
		case state.SidebarSession:
			r, _ := tree.Session(k.Session)
			label := r.Title
			if r.Badge != "" {
				label += " · " + r.Badge
			}
			item := ui.SidebarItem{ID: fmt.Sprintf("session/%d", r.ID), Text: label, Icon: lucide.MessageSquare}
			var status core.Node
			switch r.Mark {
			case state.MarkRunning:
				item.Text += ", running"
				status = ui.Badge{Text: "Running", Variant: ui.BadgeAccent}
			case state.MarkWaiting:
				// What it waits for, when the client knows.
				item.Text += ", waiting"
				if r.Waits != "" {
					item.Text += ": " + r.Waits
				}
				status = ui.Badge{Text: "Waiting", Variant: ui.BadgeWarn}
			case state.MarkNew:
				item.Text += ", finished, not read"
				status = ui.Badge{Text: "New", Variant: ui.BadgeSuccess}
			}
			if depth > 0 || status != nil {
				item.Content = workspaceui.ChildLabel{Text: label, Icon: item.Icon, Depth: depth, Last: last, Trailing: status}
			}
			// The row's menu pins and renames.
			item.Menu = c.sessionMenu(k)
			section.Items = append(section.Items, item)
			children := tree.Children(k)
			for i, kid := range children {
				add(section, kid, depth+1, i == len(children)-1)
			}
		}
	}
	for _, section := range tree.Sections {
		if section.ID == state.SectionPlaces {
			continue
		}
		group := ui.SidebarSection{Title: section.Title, Collapsible: true}
		for _, k := range section.Items {
			add(&group, k, 0, false)
		}
		sections = append(sections, group)
	}
	return sections
}

// sidebarNotes say when the sessions or the agents' profiles failed to
// load, with a way to load them again, and when the sessions are loading
// or there is none: the sidebar says there is none only once they loaded.
// Without the profiles the team's agents show by their ids, and only
// those with sessions.
func (c *client) sidebarNotes() core.Node {
	return core.Dynamic(func(cx *core.Scope) core.Node {
		c.revision.Get()
		d := &c.store.Data
		var out []core.Node
		note := func(text string) core.Node {
			return core.Text{Content: text, Style: ui.TypeCaption, StyleFunc: ui.Colored(ui.ColorsOf(cx).Muted)}
		}
		switch status, problem := d.SessionsStatus(); {
		case status == state.ListFailed:
			out = append(out, c.listProblem("the sessions", problem, "", c.store.Refresh))
		case status == state.ListLoading && len(d.Sessions()) == 0:
			out = append(out, note("Loading the sessions…"))
		case status == state.ListLoaded && len(d.Sessions()) == 0:
			out = append(out, note("No sessions yet. Start one with New session."))
		}
		if status, problem := d.ProfilesStatus(); status == state.ListFailed {
			out = append(out, c.listProblem("the agents' profiles", problem, "", c.store.LoadProfiles))
		}
		return core.Group(out)
	})
}

func (c *client) workspace(cx *core.Scope) core.Node {
	settingsOpen := core.NewSignal(cx, false)
	showSettings := func() {
		c.store.WantSignIns()
		settingsOpen.Set(true)
	}
	palette, launch := c.commands(cx, showSettings)
	var widths workspaceui.PaneWidths
	c.loadJSON("pane-widths", &widths)
	shell := workspaceui.Shell{Widths: widths, OnWidthsChange: func(w workspaceui.PaneWidths) { c.saveJSON("pane-widths", w) }, Brand: "PocketGo", Sections: c.sections, Navigate: c.navigate, Disclose: c.discloseGroup,
		Auxiliary: &workspaceui.Inspector{Title: "Session files", Icon: lucide.FolderTree, Content: c.sessionFiles},
		Inspector: &workspaceui.Inspector{Title: "Session details", Available: func() bool { return c.page.Get() == "Session" }, Content: c.sessionDetails},
		Selected: func() string {
			if c.page.Get() == "Session" {
				return fmt.Sprintf("session/%d", c.session.Get())
			}
			return c.page.Get()
		}, SidebarHeader: []core.Node{core.View{Style: core.Style{Padding: core.All(workspaceui.SpaceS), Gap: workspaceui.SpaceS}, Children: []core.Node{
			core.View{Style: core.Style{Direction: core.Row, AlignItems: core.AlignCenter, Gap: workspaceui.SpaceS}, Children: []core.Node{
				core.View{Style: core.Style{FlexGrow: 1, FlexShrink: 1, MinWidth: core.Pt(0)}, Children: []core.Node{
					workspaceui.SearchField{Placeholder: "Filter sessions", Value: c.filter.Get, OnChange: c.filter.Set}}},
				ui.IconButton{Label: "New session", Icon: lucide.SquarePen, OnPress: c.showNewSession}}},
			c.sidebarNotes()}}},
		SidebarFooter: []core.Node{c.sidebarFoot(showSettings)},
		Header:        []core.Node{c.contentHeader(cx)},
		Actions:       []core.Node{c.toolbar(cx, launch)},
		Content:       []core.Node{core.View{Style: ui.FillRest, Children: []core.Node{core.View{Style: ui.FillRest.Merge(core.Style{MinHeight: core.Pt(0)}), Children: []core.Node{core.Dynamic(func(cx *core.Scope) core.Node { return c.placeView(cx, c.page.Get()) })}}, c.statusBar()}}},
	}
	// A phone's window, and a tablet's narrow one, go by a bar of tabs.
	compact := core.NewMemo(cx, c.compact)
	window := core.Show{When: compact.Get, Then: func(cx *core.Scope) core.Node { return c.compactWorkspace(cx, showSettings) },
		Else: func(*core.Scope) core.Node { return shell }}
	return core.Group{window, palette, c.settingsDialog(cx, settingsOpen), c.newSessionDialog(cx)}
}

// placeView is the view of a page of the window.
func (c *client) placeView(cx *core.Scope, page string) core.Node {
	switch page {
	case "Session":
		return c.conversation(cx, c.session.Get())
	case "Work board":
		return c.board(cx)
	case "Search":
		return c.search(cx)
	case "Files":
		return c.files(cx)
	case "Terminal":
		return c.terminalPage(cx)
	case "Activity":
		return c.activity(cx)
	}
	return c.inbox(cx)
}

func (c *client) showNotice(kind ui.ToastType, title, detail string) {
	c.notices.Set(&workspaceui.Notice{Kind: kind, Title: title, Detail: detail, OnDismiss: func() { c.notices.Set(nil) }})
}

// loadsShownInPlace are the loads whose failure a view shows where the
// list is, with Try Again, and the new session's start, whose failure its
// dialog shows. Their error gets no toast as well: it would say the same,
// and lie over the banner's button. The Activity page keeps the line.
var loadsShownInPlace = []string{
	"Loading the sessions", "Loading the approvals", "Loading the asks", "Loading the lessons", "Loading the threads",
	"Loading the pull requests", "Loading the plans", "Loading the proposals", "Loading the schedules", "Loading the terminals",
	"Loading the profiles", "Loading session ", "Searching the sessions", "Starting a session",
}

func shownInPlace(message string) bool {
	return slices.ContainsFunc(loadsShownInPlace, func(load string) bool { return strings.HasPrefix(message, load) })
}

func (c *client) showError(message string) {
	title := "PocketGo error"
	if detail, ok := strings.CutPrefix(message, "Read folder: "); ok {
		title, message = "Couldn’t open folder", detail
	} else if detail, ok := strings.CutPrefix(message, "Read file: "); ok {
		title, message = "Couldn’t open file", detail
	}
	c.showNotice(ui.ToastError, title, message)
}
