// Package nsgoclient presents the existing pocketd store on NSGo. The store
// and its signals both belong to the NSGo engine goroutine.
package nsgoclient

import (
	"fmt"
	"maps"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/frostyard/frostui/workspaceui"
	"github.com/frostyard/pocketgo/desktop/internal/state"
	"github.com/frostyard/pocketgo/proto"
	"github.com/nsgo-dev/nsgo/base/terminal"
	"github.com/nsgo-dev/nsgo/core"
	"github.com/nsgo-dev/nsgo/icons/lucide"
	"github.com/nsgo-dev/nsgo/ui"
)

type client struct {
	zoom *core.Signal[float32]

	prefs   preferences
	groups  *core.Signal[map[string]bool]
	reasons map[string]string

	terminals      map[string]*terminal.Terminal
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
	shiftEnd       string
	noticeCard     *core.Signal[string]
	drafts         map[int64]*core.Signal[string]
	log            []string
}

func Root(cx *core.Scope) core.Node { return buildClient(cx, transport(), true) }

func newClient(cx *core.Scope, transport http.RoundTripper) core.Node {
	return buildClient(cx, transport, false)
}
func buildClient(cx *core.Scope, transport http.RoundTripper, production bool) core.Node {
	cx.SetTitle("PocketGo")
	// The Frostyard theme is set even when the platform refuses the bundled
	// font: the text then shows in the system's sans-serif face.
	fontErr := workspaceui.Apply(cx)
	c := &client{cx: cx, revision: core.NewSignal(cx, 0), page: core.NewSignal(cx, "Inbox"),
		session: core.NewSignal(cx, int64(0)), notices: core.NewSignal(cx, (*workspaceui.Notice)(nil)), filter: core.NewSignal(cx, ""), drafts: map[int64]*core.Signal[string]{}}
	c.seek = core.NewSignal(cx, int64(0))
	if fontErr != nil {
		c.log = append(c.log, "Register the Inter font: "+fontErr.Error())
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
	var zoom float32
	c.loadJSON("zoom", &zoom)
	c.zoom = core.NewSignal(cx, state.Zoom(zoom))
	c.termID = core.NewSignal(cx, "")
	c.terminals = map[string]*terminal.Terminal{}
	cx.Cleanup(c.closeTerminals)
	c.store = state.New(state.Options{Transport: transport, Post: func(fn func()) {
		cx.Post(func() { cx.Batch(func() { fn(); c.changed() }) })
	}, Notify: c.notify, KeptNotices: func(server string) []string { var keys []string; c.loadJSON("notified/"+server, &keys); return keys }, KeepNotices: func(server string, keys []string) { c.saveJSON("notified/"+server, keys) }, Log: func(level state.Level, text string) {
		c.log = append(c.log, text)
		if len(c.log) > 2000 {
			c.log = c.log[len(c.log)-2000:]
		}
		if level == state.LevelError {
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
	if production {
		cx.OnNotificationPress(c.openNotice)
		cx.Effect(func() { c.revision.Get(); cx.SetBadge(state.InboxCount(c.store.Data.Inbox())) })
		c.platformAccounts()
		c.restore()
	}
	// The frame paints the ground and sets the text's color and font; the
	// view inside it only zooms.
	return workspaceui.Frame(core.View{TestID: "zoom", Style: core.Style{TransformOrigin: core.ObjectPosition{X: core.PositionStart, Y: core.PositionStart}}, StyleFunc: func() core.Style {
		z := c.zoom.Get()
		return core.Style{Width: core.Pct(100 / z), Height: core.Pct(100 / z), Transform: core.Transform{Scale: z}}
	}, Children: []core.Node{
		core.Show{When: func() bool { c.revision.Get(); return c.store.SignedIn() }, Then: c.workspace, Else: c.signIn},
		core.Dynamic(func(*core.Scope) core.Node {
			if notice := c.notices.Get(); notice != nil {
				return notice
			}
			return nil
		}),
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
	c.shiftEnd = ""
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
	items := []ui.SidebarItem{
		{ID: "Inbox", Text: "Inbox", Icon: lucide.Inbox}, {ID: "Work board", Text: "Work board", Icon: lucide.Columns3},
		{ID: "Search", Text: "Search", Icon: lucide.Search}, {ID: "Files", Text: "Files", Icon: lucide.FolderOpen},
		{ID: "Terminal", Text: "Terminal", Icon: lucide.Terminal}, {ID: "Activity", Text: "Activity", Icon: lucide.Activity},
	}
	if badge := state.InboxBadge(c.store.Data.Inbox()); badge != "" {
		items[0].Text = "Inbox, " + state.InboxNote(c.store.Data.Inbox())
		items[0].Content = workspaceui.ChildLabel{Text: "Inbox", Icon: lucide.Inbox, Trailing: ui.Badge{Text: badge, Variant: ui.BadgeWarn}}
	}
	items = append(items, ui.SidebarItem{ID: "New session", Text: "New session", Icon: lucide.SquarePen})
	sections := []ui.SidebarSection{{Title: "Workspace", Items: items}}
	tree := c.store.Data.Sidebar(state.SidebarOptions{Seen: c.prefs.Seen, Pinned: c.prefs.Pinned, Filter: c.filter.Get(), Now: time.Now()})
	var add func(*ui.SidebarSection, state.SidebarKey, int, bool)
	add = func(section *ui.SidebarSection, k state.SidebarKey, depth int, last bool) {
		switch k.Kind {
		case state.SidebarSearch:
			section.Items = append(section.Items, ui.SidebarItem{ID: "search-filter", Text: "Search all sessions", Icon: lucide.Search})
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
			item.Content = workspaceui.BranchLabel{Text: name, Icon: lucide.Users, Expanded: item.Expanded, Trailing: activity}
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
				item.Text += ", waiting"
				status = ui.Badge{Text: "Waiting", Variant: ui.BadgeWarn}
			}
			if depth > 0 || status != nil {
				item.Content = workspaceui.ChildLabel{Text: label, Icon: item.Icon, Depth: depth, Last: last, Trailing: status}
			}
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
		}, SidebarHeader: []core.Node{core.View{Class: "p-2", Children: []core.Node{ui.Input{Label: "Filter sessions", Placeholder: "Filter sessions", Value: c.filter.Get, OnChange: c.filter.Set}}}},
		SidebarFooter: []core.Node{core.Text{Style: ui.TypeCaption, MaxLines: 1, ContentFunc: func() string {
			c.revision.Get()
			link := c.store.Conn
			switch {
			case link.Stopped:
				return "Connection stopped · " + link.Server
			case link.State == state.Reconnecting:
				return "Reconnecting · " + link.Server
			case link.State == state.Connecting:
				return "Connecting · " + link.Server
			default:
				return link.Server
			}
		}}},
		Header: []core.Node{core.Text{MaxLines: 1, Style: ui.TypeTitle, ContentFunc: func() string {
			if c.page.Get() == "Session" {
				c.revision.Get()
				s, _ := c.store.Data.Session(c.session.Get())
				return sessionName(s)
			}
			return c.page.Get()
		}}},
		Actions: []core.Node{
			ui.IconButton{Icon: lucide.ArrowLeft, Label: "Back", Disabled: func() bool { return !c.canMove(-1) }, OnPress: func() { c.move(-1) }},
			ui.IconButton{Icon: lucide.ArrowRight, Label: "Forward", Disabled: func() bool { return !c.canMove(1) }, OnPress: func() { c.move(1) }},
			ui.IconButton{Icon: lucide.Settings, Label: "Settings", OnPress: showSettings},
			ui.IconButton{Icon: lucide.Command, Label: "Commands", OnPress: launch},
			ui.IconButton{Icon: lucide.RefreshCw, Label: "Refresh", OnPress: func() {
				c.store.Refresh()
				c.fileRefresh.Update(func(i int) int { return i + 1 })
				if c.page.Get() == "Files" {
					c.store.LoadFileRoots()
				}
				c.changed()
			}}},
		Content: []core.Node{core.Dynamic(func(cx *core.Scope) core.Node {
			switch c.page.Get() {
			case "Session":
				return c.conversation(cx, c.session.Get())
			case "Work board":
				return c.board(cx)
			case "New session":
				return c.newSession(cx)
			case "Search":
				return c.search(cx)
			case "Files":
				return c.files(cx)
			case "Terminal":
				return c.terminalPage(cx)
			case "Activity":
				return core.Dynamic(func(*core.Scope) core.Node {
					c.revision.Get()
					return workspaceui.Page(core.Text{Content: strings.Join(c.log, "\n"), Selectable: core.SelectableOn})
				})
			default:
				return c.inbox(cx)
			}
		})},
	}
	return core.Group{shell, palette, c.settingsDialog(cx, settingsOpen)}
}

func (c *client) showNotice(kind ui.ToastType, title, detail string) {
	c.notices.Set(&workspaceui.Notice{Kind: kind, Title: title, Detail: detail, OnDismiss: func() { c.notices.Set(nil) }})
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

func (c *client) confirm(title, body, action string, fn func()) {
	gen := c.generation
	c.cx.ShowAlert(core.AlertConfig{Title: title, Message: body, Cancelable: true, Actions: []core.AlertAction{{Label: "Cancel"}, {Label: action}}}, func(i int) {
		if i == 1 && gen == c.generation {
			fn()
			c.changed()
		}
	})
}
