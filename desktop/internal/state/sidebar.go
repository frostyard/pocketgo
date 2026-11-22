package state

import (
	"path"
	"slices"
	"sort"
	"strconv"
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
// sessions), or a session (Session). Under is, for a worker's row nested
// under the row of the session that started it, that session's id; 0 for
// every other row. A worker shows nested in a section that also lists it
// at its top, as Recent does, and the two rows are two keys: choosing one
// never chooses, nor unfolds, the other.
type SidebarKey struct {
	Kind    SidebarKind
	Place   string
	Agent   string
	Session int64
	Under   int64
}

// PlaceKey is the row of a place.
func PlaceKey(place string) SidebarKey { return SidebarKey{Kind: SidebarPlace, Place: place} }

// GroupKey is the row of agent's group of sessions, "" for the owner's own.
func GroupKey(agent string) SidebarKey { return SidebarKey{Kind: SidebarGroup, Agent: agent} }

// SessionKey is the row of a session, at the top of a section or under its
// group.
func SessionKey(id int64) SidebarKey { return SidebarKey{Kind: SidebarSession, Session: id} }

// NestedKey is the row of worker id nested under the row of session under,
// which started it.
func NestedKey(id, under int64) SidebarKey {
	return SidebarKey{Kind: SidebarSession, Session: id, Under: under}
}

// StandsFor is the key of what row k stands for, as the main pane shows
// it: k itself, but for a nested worker's row, the worker's SessionKey.
func (k SidebarKey) StandsFor() SidebarKey {
	k.Under = 0
	return k
}

// The sidebar's sections, by ID.
const (
	// SectionPlaces holds the window's places.
	SectionPlaces = "places"
	// SectionPinned holds the sessions the owner pinned, in the order
	// pinned, after the places; the sidebar shows it only when it lists
	// one (Pins).
	SectionPinned = "pinned"
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
	// MarkWaiting: a tool call of it waits for the owner's approval, or
	// its question for the owner's answer (ADR-0048).
	MarkWaiting
)

// SessionRow is what a session's row shows.
type SessionRow struct {
	ID int64
	// Agent is the profile id of the agent it belongs to, "" for the
	// owner's own: its group's. A worker that nests belongs to the agent
	// of the session that started it, whose worker it is: its session
	// names no profile.
	Agent string
	// Under is the session whose row a worker's row nests under: the
	// session that started it (protocol rule 23a), when the Data holds
	// that session and it is no worker; 0 for a session that nests under
	// none.
	Under int64
	// Title is its title, UntitledSession for none.
	Title string
	// Tag names the agent the session belongs to (its profile's name),
	// unless its title starts with that name or its first word; for the
	// owner's own session, its folder's name.
	Tag  string
	Mark SessionMark
	// Waiting counts its approvals that wait for the owner, as the
	// client's list holds them; Waits is the oldest one's tool and what it
	// works on, else what its question asks (QuestionWaits).
	Waiting int
	Waits   string
	// Question says its question waits for the owner's answer (ADR-0048).
	Question bool
	// Badge is what the badge at the row's end says: how many approvals
	// wait, when the client's list holds them; QuestionBadge when none
	// does and its question waits; "new" for NEW; else "". A session its
	// summary says waits, before the approvals load or after their fetch
	// failed, shows no count: the client does not know it (Codex review of
	// #40, round 1).
	Badge string
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
	// Unlisted: the profiles loaded, and do not list the agent. Before
	// they load, or when their fetch failed, no agent is unlisted (Codex
	// review of #40, round 1).
	Unlisted bool
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
	// Pinned are the owner's pins for the service signed in to.
	Pinned Pins
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

// Children are the rows under k, newest first, at most GroupSessions of
// them; nil for a row with none. A group holds its sessions, but each
// worker nested under the row of its starter there; a session's row, in
// any section, holds the workers it started (protocol rule 23a), the
// owner's choice ("nest workers only"); a worker's row holds none.
func (sb *Sidebar) Children(k SidebarKey) []SidebarKey {
	if k.Kind == SidebarPlace || k.Under != 0 {
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

// Choice is a row of the sidebar chosen in a section.
type Choice struct {
	Section string
	Key     SidebarKey
}

// TopChoice is k's row at the top of the first section that lists it
// there, as pocket's; false for none.
func (sb *Sidebar) TopChoice(k SidebarKey) (Choice, bool) {
	for _, sec := range sb.Sections {
		if slices.Contains(sec.Items, k) {
			return Choice{Section: sec.ID, Key: k}, true
		}
	}
	return Choice{}, false
}

// ChoiceOf is the row that shows k chosen: at the top of the first section
// that lists it there; else, for a session, under its group in the team,
// which unfolds to show it; else, for a worker, nested under its starter's
// row, under the starter's group in the team or at the top of a section.
// False when no row shows k, as for a session past both lists' limits:
// then the sidebar chooses no row, rather than keep one that stands for
// something the window no longer shows (Codex review of #40, round 1).
func (sb *Sidebar) ChoiceOf(k SidebarKey) (Choice, bool) {
	k = k.StandsFor()
	if c, ok := sb.TopChoice(k); ok {
		return c, true
	}
	r, ok := sb.sessions[k.Session]
	if k.Kind != SidebarSession || !ok {
		return Choice{}, false
	}
	g := GroupKey(r.Agent)
	team, inTeam := sb.TopChoice(g)
	if inTeam && slices.Contains(sb.children[g], k) {
		return Choice{Section: team.Section, Key: k}, true
	}
	if r.Under == 0 {
		return Choice{}, false
	}
	nested, starter := NestedKey(k.Session, r.Under), SessionKey(r.Under)
	if inTeam && slices.Contains(sb.children[g], starter) {
		return Choice{Section: team.Section, Key: nested}, true
	}
	if c, ok := sb.TopChoice(starter); ok {
		return Choice{Section: c.Section, Key: nested}, true
	}
	return Choice{}, false
}

// AfterGroupClick is the row chosen after a click on group g's row, which
// folded g when folded is true. A click on a group's row moves the choice
// no more than its arrow does: the row chosen before the click (before, nil
// for none) stays chosen, unless the fold hid it, as it hides a session
// chosen under g, or a worker under one of them (choosing it again would
// unfold g: Codex review of #40, round 1). Then the choice goes to the row
// of what the main pane shows (shown, nil for nothing) at the top of a
// section, as Recent lists it; else to g itself, as the arrow moves it.
func (sb *Sidebar) AfterGroupClick(g SidebarKey, folded bool, before *Choice, shown *SidebarKey) Choice {
	team, _ := sb.TopChoice(g)
	hidden := before != nil && folded && before.Section == team.Section && sb.holds(g, before.Key)
	if before != nil && !hidden {
		return *before
	}
	if shown != nil {
		if c, ok := sb.TopChoice(*shown); ok {
			return c
		}
	}
	return Choice{Section: team.Section, Key: g}
}

// Lists reports whether the row c names shows in its section: at the
// section's top, or under a row there, folded or not. A pin or an unpin
// moves a session's row from Recent to Pinned or back, and the row chosen
// there with it: one that no longer shows is chosen again where it shows
// now (ChoiceOf).
func (sb *Sidebar) Lists(c Choice) bool {
	for _, sec := range sb.Sections {
		if sec.ID != c.Section {
			continue
		}
		for _, k := range sec.Items {
			if k == c.Key || sb.under(k, c.Key) {
				return true
			}
		}
	}
	return false
}

// under reports whether row k is under row parent, at any depth.
func (sb *Sidebar) under(parent, k SidebarKey) bool {
	for _, child := range sb.Children(parent) {
		if child == k || sb.under(child, k) {
			return true
		}
	}
	return false
}

// holds reports whether row k is under group g: one of its sessions, or a
// worker nested under one of them.
func (sb *Sidebar) holds(g, k SidebarKey) bool {
	if slices.Contains(sb.children[g], k) {
		return true
	}
	return k.Under != 0 && slices.Contains(sb.children[g], SessionKey(k.Under))
}

// Sidebar makes the sidebar: the places, the sessions pinned that the
// Data holds (Pins), the team (the owner's own sessions as a group of
// their own, then each agent's group in the order GET /v1/profiles lists
// them, then those of the agents it no longer lists, by id) and the recent
// sessions, those pinned left out; or, with a filter, the sessions it
// finds, pinned or not. An agent counts as no longer listed only once the profiles
// loaded: before, or when their fetch failed, its group shows as any other
// (ProfilesStatus says why). A worker nests under the row of the session
// that started it (starters), in its starter's agent's group.
func (d *Data) Sidebar(o SidebarOptions) *Sidebar {
	sb := &Sidebar{children: map[SidebarKey][]SidebarKey{}, sessions: make(map[int64]SessionRow, len(d.sessions)),
		groups: map[string]GroupRow{"": {}}}
	loaded := d.profilesList.status == ListLoaded
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
	under := d.starters()
	agentOf := map[int64]string{}
	for _, s := range d.sessions {
		agentOf[s.ID] = s.Profile
	}
	var unlisted []string
	members := map[string][]int64{} // each group's sessions, newest first
	for _, s := range d.sessions {
		agent := s.Profile
		if p := under[s.ID]; p != 0 {
			agent = agentOf[p]
			k := SessionKey(p)
			if len(sb.children[k]) < GroupSessions {
				sb.children[k] = append(sb.children[k], NestedKey(s.ID, p))
			}
		}
		_, asked := WaitingQuestion(s)
		st := d.stateOf(s.ID, s.State, len(waits[s.ID]) > 0 || asked)
		sb.sessions[s.ID] = d.sessionRow(s, agent, under[s.ID], st, waits[s.ID], o)
		g, ok := sb.groups[agent]
		if !ok {
			g = GroupRow{Agent: agent, Name: agent, Unlisted: loaded}
			unlisted = append(unlisted, agent)
		}
		switch st {
		case proto.SessionWaiting:
			g.Waiting++
		case proto.SessionRunning:
			g.Running++
		}
		sb.groups[agent] = g
		members[agent] = append(members[agent], s.ID)
	}
	for agent, ids := range members {
		sb.children[GroupKey(agent)] = groupRows(ids, under)
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
	var pinned []SidebarKey
	isPinned := map[int64]bool{}
	for _, id := range o.Pinned {
		if _, held := sb.sessions[id]; held && !isPinned[id] {
			pinned = append(pinned, SessionKey(id))
			isPinned[id] = true
		}
	}
	var recent []SidebarKey
	for _, s := range d.sessions {
		if len(recent) == RecentSessions {
			break
		}
		if !isPinned[s.ID] {
			recent = append(recent, SessionKey(s.ID))
		}
	}
	sb.Sections = []SidebarSection{{ID: SectionPlaces, Title: "PocketGo", Items: places}}
	if len(pinned) > 0 {
		sb.Sections = append(sb.Sections, SidebarSection{ID: SectionPinned, Title: "Pinned", Items: pinned})
	}
	sb.Sections = append(sb.Sections,
		SidebarSection{ID: SectionTeam, Title: "Team", Items: team},
		SidebarSection{ID: SectionRecent, Title: "Recent", Items: recent})
	return sb
}

// groupRows are the rows of a group whose sessions are ids, newest first:
// at most GroupSessions of them, without each worker whose row nests under
// its starter's row there (under). A worker whose starter is no row of the
// group, as one past its limit, is a row of its own, as any session. Each
// worker that shows on its own may push the group's oldest row past the
// limit, a starter's included, whose worker then shows on its own too: the
// rows are worked out again until no more do.
func groupRows(ids []int64, under map[int64]int64) []SidebarKey {
	own := map[int64]bool{} // the workers whose starter is no row of the group
	for {
		var rows []SidebarKey
		listed := map[int64]bool{}
		for _, id := range ids {
			if len(rows) == GroupSessions {
				break
			}
			if under[id] != 0 && !own[id] {
				continue
			}
			rows = append(rows, SessionKey(id))
			listed[id] = true
		}
		more := false
		for _, id := range ids {
			if p := under[id]; p != 0 && !own[id] && !listed[p] {
				own[id], more = true, true
			}
		}
		if !more {
			return rows
		}
	}
}

// starters are the workers that nest in the sidebar, each with the session
// whose row it nests under: a session whose route is a worker's nests
// under the session that started it (protocol rule 23a), when the Data
// holds that session and it is no worker itself, so no row nests under its
// own. Nothing else nests: a thread's specialist does not nest under its
// supervisor (the owner's choice, "nest workers only").
func (d *Data) starters() map[int64]int64 {
	worker := make(map[int64]bool, len(d.sessions))
	for _, s := range d.sessions {
		worker[s.ID] = IsWorkerRoute(s.Route)
	}
	out := map[int64]int64{}
	for _, s := range d.sessions {
		isWorker, held := worker[s.StartedBy]
		if worker[s.ID] && s.StartedBy != 0 && held && !isWorker {
			out[s.ID] = s.StartedBy
		}
	}
	return out
}

// sessionRow is what s's row shows, in state st, with its approvals that
// wait, oldest first: s belongs to agent, and its row nests under the row
// of session under (0 for none).
func (d *Data) sessionRow(s proto.Session, agent string, under int64, st proto.SessionState, waits []proto.Approval, o SidebarOptions) SessionRow {
	r := SessionRow{ID: s.ID, Agent: agent, Under: under, Title: SessionTitle(s), Tag: d.sessionTag(s, agent), Mark: d.markOf(s, st, o.Seen, o.Now),
		Waiting: len(waits)}
	if len(waits) > 0 {
		a := waits[0]
		r.Waits = strings.TrimSpace(a.Tool + " " + ToolSummary(a.Tool, a.Arguments))
	}
	if q, ok := WaitingQuestion(s); ok {
		r.Question = true
		if r.Waits == "" {
			r.Waits = QuestionWaits(q)
		}
	}
	switch {
	case r.Mark == MarkWaiting && r.Waiting > 0:
		r.Badge = strconv.Itoa(r.Waiting)
	case r.Mark == MarkWaiting && r.Question:
		r.Badge = QuestionBadge
	case r.Mark == MarkNew:
		r.Badge = "new"
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

// sessionTag names agent, the agent s belongs to, unless its title starts
// with the agent's name or that name's first word, as "Odrade: plan the
// week" does; for the owner's own session, the folder it works in.
func (d *Data) sessionTag(s proto.Session, agent string) string {
	if agent == "" {
		if s.Cwd == "" {
			return ""
		}
		return path.Base(s.Cwd)
	}
	name := d.AgentName(agent)
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
	g := GroupRow{Agent: p.ID, Name: p.ID, Problem: p.Problem}
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
		repos := make([]string, len(p.Repositories))
		for i, r := range p.Repositories {
			repos[i] = r.Name
		}
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
