package state

import (
	"path"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/frostyard/pocketgo/proto"
)

// The sidebar's rules, ported from frostyard/pocket's
// internal/app/sidebar.go (ADR-0010): which rows it lists in which section
// and order, what each row says, and which sessions show NEW or dimmed.
// desktop/internal/app draws it with frostui's SourceList.

// SidebarKind is what a row of the sidebar stands for.
type SidebarKind int

const (
	// SidebarPlace is one of the window's places, as the Inbox.
	SidebarPlace SidebarKind = iota + 1
	// SidebarGroup is a group of sessions: an agent's, or the owner's own.
	SidebarGroup
	// SidebarSession is a session.
	SidebarSession
)

// SidebarKey names a row of the sidebar: a place by its key (Place), a
// group by its agent's profile id (Agent, "" for the owner's own
// sessions), or a session (Session).
type SidebarKey struct {
	Kind    SidebarKind
	Place   string
	Agent   string
	Session int64
}

// PlaceKey is the row of a place.
func PlaceKey(place string) SidebarKey { return SidebarKey{Kind: SidebarPlace, Place: place} }

// GroupKey is the row of agent's group of sessions, "" for the owner's own.
func GroupKey(agent string) SidebarKey { return SidebarKey{Kind: SidebarGroup, Agent: agent} }

// SessionKey is the row of a session.
func SessionKey(id int64) SidebarKey { return SidebarKey{Kind: SidebarSession, Session: id} }

// The sidebar's sections, by ID.
const (
	// SectionPlaces holds the window's places.
	SectionPlaces = "places"
	// SectionTeam holds the owner's own sessions, then each agent's, as
	// groups.
	SectionTeam = "team"
	// SectionRecent holds the sessions, newest first.
	SectionRecent = "recent"
	// SectionFound holds the sessions a filter found, in place of the
	// others.
	SectionFound = "found"
)

// The sidebar's limits and ages, pocket's.
const (
	// GroupSessions is how many of its sessions a group lists.
	GroupSessions = 30
	// RecentSessions is how many sessions Recent lists.
	RecentSessions = 40
	// FoundSessions is how many sessions a filter lists.
	FoundSessions = 100
	// NewFor is how long after its last update a session the owner has not
	// seen since shows NEW.
	NewFor = 30 * 24 * time.Hour
	// OldAfter is how long after its last update an idle session the owner
	// has seen shows dimmed.
	OldAfter = 7 * 24 * time.Hour
)

// UntitledSession is the title the sidebar shows for a session that has
// none, as pocket's.
const UntitledSession = "New session"

// SidebarSection is a titled section of the sidebar: the keys of the rows
// at its top, in order.
type SidebarSection struct {
	ID, Title string
	Items     []SidebarKey
}

// SessionMark is what a session's row says of it, in pocket's order: a
// session that waits for the owner says so before it says it runs, and
// either before NEW.
type SessionMark int

const (
	// MarkSeen: nothing to say; the owner saw what it holds.
	MarkSeen SessionMark = iota
	// MarkNew: it is idle, and was updated since the owner last saw it.
	MarkNew
	// MarkRunning: a run works in it.
	MarkRunning
	// MarkWaiting: a tool call of it waits for the owner's approval.
	MarkWaiting
)

// SessionRow is what a session's row shows.
type SessionRow struct {
	ID int64
	// Title is its title, UntitledSession for none.
	Title string
	// Tag names the agent the session belongs to (its profile's name),
	// unless its title starts with that name or its first word; for the
	// owner's own session, its folder's name.
	Tag  string
	Mark SessionMark
	// Waiting counts its approvals that wait for the owner; Waits is the
	// oldest one's tool and what it works on.
	Waiting int
	Waits   string
	// Dim: the owner saw it idle, and it was last updated more than
	// OldAfter ago.
	Dim bool
}

// GroupRow is what a group's row shows: an agent's, or the owner's own.
type GroupRow struct {
	// Agent is the agent's profile id, "" for the owner's own sessions.
	Agent string
	// Name is the agent's name, its id when its profile names none.
	Name string
	// Badge is its role when the role is supervisor or clerk, else "".
	Badge string
	// Known: the profiles list the agent (always, for the owner's group).
	Known bool
	// Problem is why its profile cannot start a session, "" for none.
	Problem string
	// Hint is what hovering its row says: who it is, and where it works.
	Hint string
	// Waiting counts its sessions that wait for the owner, Running those
	// a run works in.
	Waiting, Running int
}

// SidebarOptions are what the sidebar is made from besides the Data.
type SidebarOptions struct {
	// Places are the keys of the window's places, in the order the first
	// section lists them.
	Places []string
	// Filter, when not blank, lists the sessions whose title or folder
	// holds it, whatever its case, in place of every other section.
	Filter string
	// Seen are the owner's seen marks for the service signed in to.
	Seen Seen
	// Now is the time NEW and dimming are judged at.
	Now time.Time
}

// Sidebar is the sidebar as one pass over the Data made it: its sections,
// and what each of its rows shows.
type Sidebar struct {
	Sections []SidebarSection
	children map[SidebarKey][]SidebarKey
	sessions map[int64]SessionRow
	groups   map[string]GroupRow
}

// Children are the rows under k: a group's sessions, newest first, at
// most GroupSessions of them; nil for a row with none. A session holds no
// rows yet: a thread's worker nests under the session that started it in
// a later change.
func (sb *Sidebar) Children(k SidebarKey) []SidebarKey {
	if k.Kind != SidebarGroup {
		return nil
	}
	return sb.children[k]
}

// Session is what the row of session id shows; false for a session the
// Data does not hold.
func (sb *Sidebar) Session(id int64) (SessionRow, bool) {
	r, ok := sb.sessions[id]
	return r, ok
}

// Group is what the row of agent's group shows ("" for the owner's own).
func (sb *Sidebar) Group(agent string) (GroupRow, bool) {
	r, ok := sb.groups[agent]
	return r, ok
}

// Folds reports whether a click on row k folds or unfolds the rows under
// it, as its arrow does, and leaves the choice where it was: a group's row
// with sessions does, its agent's or the owner's own (the owner's choice:
// pocket's click shows the agent's page, which pocketgo does not have). A
// click on any other row shows what it stands for.
func (sb *Sidebar) Folds(k SidebarKey) bool {
	return k.Kind == SidebarGroup && sb.Children(k) != nil
}

// Shows reports whether choosing row k, by a click or the keys, shows
// what it stands for in the main pane: a place or a session does; a
// group's row has nothing of its own to show.
func Shows(k SidebarKey) bool {
	return k.Kind == SidebarPlace || k.Kind == SidebarSession
}

// SectionOf is the section that lists k at its top, the first that does:
// where the sidebar shows k chosen. A row no section lists at its top
// belongs to Recent, as pocket's.
func (sb *Sidebar) SectionOf(k SidebarKey) string {
	for _, sec := range sb.Sections {
		if slices.Contains(sec.Items, k) {
			return sec.ID
		}
	}
	return SectionRecent
}

// Sidebar makes the sidebar: the places, the team (the owner's own
// sessions as a group of their own, then each agent's group in the order
// GET /v1/profiles lists them, then those of the agents it no longer
// lists, by id) and the recent sessions; or, with a filter, the sessions
// it finds.
func (d *Data) Sidebar(o SidebarOptions) *Sidebar {
	sb := &Sidebar{children: map[SidebarKey][]SidebarKey{}, sessions: make(map[int64]SessionRow, len(d.sessions)),
		groups: map[string]GroupRow{"": {Known: true}}}
	waits := map[int64][]proto.Approval{} // oldest first, as d.approvals
	for _, a := range d.approvals {
		waits[a.SessionID] = append(waits[a.SessionID], a)
	}
	order := []string{""}
	for i := range d.profiles {
		p := &d.profiles[i]
		if _, dup := sb.groups[p.ID]; dup {
			continue
		}
		sb.groups[p.ID] = groupOf(p)
		order = append(order, p.ID)
	}
	var unlisted []string
	for _, s := range d.sessions {
		st := d.stateOf(s.ID, s.State, len(waits[s.ID]) > 0)
		sb.sessions[s.ID] = d.sessionRow(s, st, waits[s.ID], o)
		g, ok := sb.groups[s.Profile]
		if !ok {
			g = GroupRow{Agent: s.Profile, Name: s.Profile}
			unlisted = append(unlisted, s.Profile)
		}
		switch st {
		case proto.SessionWaiting:
			g.Waiting++
		case proto.SessionRunning:
			g.Running++
		}
		sb.groups[s.Profile] = g
		if k := GroupKey(s.Profile); len(sb.children[k]) < GroupSessions {
			sb.children[k] = append(sb.children[k], SessionKey(s.ID))
		}
	}
	if q := strings.ToLower(strings.TrimSpace(o.Filter)); q != "" {
		var hits []SidebarKey
		for _, s := range d.sessions {
			if strings.Contains(strings.ToLower(SessionTitle(s)+" "+s.Cwd), q) {
				hits = append(hits, SessionKey(s.ID))
				if len(hits) == FoundSessions {
					break
				}
			}
		}
		sb.Sections = []SidebarSection{{ID: SectionFound, Title: "Sessions", Items: hits}}
		return sb
	}
	sort.Strings(unlisted)
	order = append(order, unlisted...)
	places := make([]SidebarKey, len(o.Places))
	for i, p := range o.Places {
		places[i] = PlaceKey(p)
	}
	team := make([]SidebarKey, len(order))
	for i, agent := range order {
		team[i] = GroupKey(agent)
	}
	var recent []SidebarKey
	for _, s := range d.sessions[:min(len(d.sessions), RecentSessions)] {
		recent = append(recent, SessionKey(s.ID))
	}
	sb.Sections = []SidebarSection{
		{ID: SectionPlaces, Title: "PocketGo", Items: places},
		{ID: SectionTeam, Title: "Team", Items: team},
		{ID: SectionRecent, Title: "Recent", Items: recent},
	}
	return sb
}

// sessionRow is what s's row shows, in state st, with its approvals that
// wait, oldest first.
func (d *Data) sessionRow(s proto.Session, st proto.SessionState, waits []proto.Approval, o SidebarOptions) SessionRow {
	r := SessionRow{ID: s.ID, Title: SessionTitle(s), Tag: d.sessionTag(s), Mark: d.markOf(s, st, o.Seen, o.Now), Waiting: len(waits)}
	if len(waits) > 0 {
		a := waits[0]
		r.Waits = strings.TrimSpace(a.Tool + " " + ToolSummary(a.Tool, a.Arguments))
	}
	r.Dim = r.Mark == MarkSeen && o.Now.Sub(s.UpdatedAt) > OldAfter
	return r
}

// markOf is what s's row says of it in state st: that it waits, or runs;
// else NEW when it was updated since the owner last saw it (or ever, for
// one never seen), less than NewFor ago, and the window does not show it.
func (d *Data) markOf(s proto.Session, st proto.SessionState, seen Seen, now time.Time) SessionMark {
	switch st {
	case proto.SessionWaiting:
		return MarkWaiting
	case proto.SessionRunning:
		return MarkRunning
	}
	if s.ID != d.OpenID() && s.UpdatedAt.After(seen[s.ID]) && now.Sub(s.UpdatedAt) < NewFor {
		return MarkNew
	}
	return MarkSeen
}

// sessionTag names the agent s belongs to, unless its title starts with
// the agent's name or that name's first word, as "Odrade: plan the week"
// does; for the owner's own session, the folder it works in.
func (d *Data) sessionTag(s proto.Session) string {
	if s.Profile == "" {
		if s.Cwd == "" {
			return ""
		}
		return path.Base(s.Cwd)
	}
	name := d.AgentName(s.Profile)
	if strings.HasPrefix(s.Title, name) {
		return ""
	}
	if first := strings.Fields(name); len(first) > 0 && strings.HasPrefix(s.Title, first[0]) {
		return ""
	}
	return name
}

// SessionTitle is s's title, UntitledSession for none.
func SessionTitle(s proto.Session) string {
	if s.Title == "" {
		return UntitledSession
	}
	return s.Title
}

// AgentName is the name of the agent with profile id, as the profiles
// list it; its id when they do not name it.
func (d *Data) AgentName(id string) string {
	for _, p := range d.profiles {
		if p.ID == id && strings.TrimSpace(p.Name) != "" {
			return p.Name
		}
	}
	return id
}

// groupOf is the row of p's group, before its sessions are counted.
func groupOf(p *proto.Profile) GroupRow {
	g := GroupRow{Agent: p.ID, Name: p.ID, Known: true, Problem: p.Problem}
	if strings.TrimSpace(p.Name) != "" {
		g.Name = p.Name
	}
	if p.Role == proto.RoleSupervisor || p.Role == proto.RoleClerk {
		g.Badge = string(p.Role)
	}
	if p.Problem == "" {
		g.Hint = AgentHint(p)
	}
	return g
}

// RoleTitle names a role as a row's hint does.
func RoleTitle(r proto.Role) string {
	switch r {
	case proto.RoleSupervisor:
		return "Supervisor"
	case proto.RoleClerk:
		return "Clerk"
	}
	return "Specialist"
}

// AgentHint is what hovering an agent's row says: who it is, the model a
// session from it runs, and where it works.
func AgentHint(p *proto.Profile) string {
	name := p.ID
	if strings.TrimSpace(p.Name) != "" {
		name = p.Name
	}
	lines := []string{name + " · " + RoleTitle(p.Role)}
	switch {
	case p.Runs.ID != "":
		lines = append(lines, p.Runs.ID)
	case p.Model.ID != "":
		lines = append(lines, p.Model.ID)
	}
	if p.Home != "" {
		lines = append(lines, "Home: "+p.Home)
	}
	if p.Cwd != "" && p.Cwd != p.Home {
		lines = append(lines, "Workspace: "+p.Cwd)
	}
	if len(p.Repositories) > 0 {
		repos := slices.Clone(p.Repositories)
		slices.Sort(repos)
		lines = append(lines, "Repositories:")
		for _, r := range repos {
			lines = append(lines, "  "+r)
		}
	}
	return strings.Join(lines, "\n")
}

// Seen are the owner's seen marks: for each session, by id, the
// updatedAt it had when the owner last had it open. pocket keeps the time
// the owner saw it, by the server's clock; a session's own updatedAt
// needs no clock.
type Seen map[int64]time.Time

// Saw returns sn with session s marked seen as it is now, and whether its
// mark moved. A mark never moves back.
func (sn Seen) Saw(s proto.Session) (Seen, bool) {
	if !s.UpdatedAt.After(sn[s.ID]) {
		return sn, false
	}
	if sn == nil {
		sn = Seen{}
	}
	sn[s.ID] = s.UpdatedAt
	return sn, true
}

// Prune drops the marks made NewFor or longer before now: a session seen
// that long ago shows NEW, or not, as one never seen does.
func (sn Seen) Prune(now time.Time) {
	for id, at := range sn {
		if now.Sub(at) >= NewFor {
			delete(sn, id)
		}
	}
}
