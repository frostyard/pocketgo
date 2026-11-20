package state

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/frostyard/pocketgo/desktop/internal/pocketdtest"
	"github.com/frostyard/pocketgo/proto"
)

// agentSession is session id of agent (its profile id, "" for the
// owner's own), updated at updated from pocketdtest.Now.
func agentSession(id int64, agent, title string, updated time.Duration) proto.Session {
	s := session(id, title, updated)
	s.Profile = agent
	return s
}

// sidebarData is a Data holding profiles and sessions, as their fetches
// answered.
func sidebarData(profiles []proto.Profile, sessions ...proto.Session) *Data {
	d := &Data{}
	d.SetProfiles(d.Begin(), profiles)
	d.SetSessions(d.Begin(), sessions)
	return d
}

var team = []proto.Profile{
	{ID: "duncan", Name: "Duncan", Role: proto.RoleAgent, Home: "/home/team/agents/duncan", Cwd: "/home/team/agents/duncan",
		Runs: proto.ModelRef{Provider: "anthropic", ID: "claude-opus-5-5"}, Repositories: []string{"frostyard/snow", "frostyard/nsl"}},
	{ID: "odrade", Name: "Odrade", Role: proto.RoleClerk},
	{ID: "sheeana", Name: "Sheeana", Role: proto.RoleSupervisor},
}

// keysOf is what a list of sidebar rows names, for messages.
func keysOf(list []SidebarKey) []string {
	var out []string
	for _, k := range list {
		switch k.Kind {
		case SidebarPlace:
			out = append(out, "place:"+k.Place)
		case SidebarGroup:
			out = append(out, "group:"+k.Agent)
		default:
			out = append(out, fmt.Sprint(k.Session))
		}
	}
	return out
}

func sectionIDs(sb *Sidebar) []string {
	var out []string
	for _, sec := range sb.Sections {
		out = append(out, sec.ID+" "+sec.Title)
	}
	return out
}

// The sidebar lists the places, then the team: the owner's own sessions
// first, as a group of their own, then each agent in the profiles' order,
// then the agents the profiles no longer list, by id; then the recent
// sessions. A group holds its sessions, newest first.
func TestTheSidebarGroupsSessionsByTheirAgent(t *testing.T) {
	d := sidebarData(team,
		agentSession(1, "odrade", "Clerk: Desk batch", -time.Minute),
		agentSession(2, "", "Fix the flaky CI", -2*time.Minute),
		agentSession(3, "zia", "T4 · Old work", -3*time.Minute),
		agentSession(4, "duncan", "T5 · Snow boots", -4*time.Minute),
		agentSession(5, "odrade", "Desk sweep", -5*time.Minute),
		agentSession(6, "abel", "T1 · Gone", -6*time.Minute),
	)
	sb := d.Sidebar(SidebarOptions{Places: []string{"inbox", "board"}, Now: pocketdtest.Now})
	if got, want := sectionIDs(sb), []string{"places PocketGo", "team Team", "recent Recent"}; !slices.Equal(got, want) {
		t.Fatalf("sections %q, want %q", got, want)
	}
	for _, tc := range []struct {
		section int
		want    []string
	}{
		{0, []string{"place:inbox", "place:board"}},
		{1, []string{"group:", "group:duncan", "group:odrade", "group:sheeana", "group:abel", "group:zia"}},
		{2, []string{"1", "2", "3", "4", "5", "6"}},
	} {
		if got := keysOf(sb.Sections[tc.section].Items); !slices.Equal(got, tc.want) {
			t.Errorf("section %s lists %q, want %q", sb.Sections[tc.section].ID, got, tc.want)
		}
	}
	for agent, want := range map[string][]string{"": {"2"}, "odrade": {"1", "5"}, "duncan": {"4"}, "zia": {"3"}} {
		if got := keysOf(sb.Children(GroupKey(agent))); !slices.Equal(got, want) {
			t.Errorf("group %q holds %q, want %q", agent, got, want)
		}
	}
	// An agent with no session is a row with nothing to open, as pocket's.
	if got := sb.Children(GroupKey("sheeana")); got != nil {
		t.Errorf("Sheeana's group holds %v, want nil", got)
	}
	// A session and a place hold no rows (a thread's worker nests under its
	// session in a later change).
	if sb.Children(SessionKey(4)) != nil || sb.Children(PlaceKey("inbox")) != nil {
		t.Error("a session or a place holds rows")
	}
	if g, _ := sb.Group("zia"); !g.Unlisted || g.Name != "zia" {
		t.Errorf("an agent the profiles do not list shows %+v, want its id, unlisted", g)
	}
	if g, _ := sb.Group(""); g.Unlisted || g.Agent != "" {
		t.Errorf("the owner's group is %+v", g)
	}
}

// An agent counts as no longer listed only once the profiles loaded: while
// they load, or when their fetch failed, a session's agent shows as any
// other (Codex review of #40, round 1).
func TestNoAgentIsUnlistedBeforeTheProfilesLoad(t *testing.T) {
	d := &Data{}
	d.SetSessions(d.Begin(), []proto.Session{agentSession(1, "duncan", "T5 · Snow boots", 0)})
	if g, _ := d.Sidebar(SidebarOptions{Now: pocketdtest.Now}).Group("duncan"); g.Unlisted {
		t.Errorf("Duncan is unlisted while the profiles load: %+v", g)
	}
	d.SetProfilesError(d.Begin(), "502 Bad Gateway")
	if g, _ := d.Sidebar(SidebarOptions{Now: pocketdtest.Now}).Group("duncan"); g.Unlisted {
		t.Errorf("Duncan is unlisted though the profiles failed to load: %+v", g)
	}
	d.SetProfiles(d.Begin(), []proto.Profile{{ID: "odrade", Name: "Odrade"}})
	if g, _ := d.Sidebar(SidebarOptions{Now: pocketdtest.Now}).Group("duncan"); !g.Unlisted {
		t.Errorf("Duncan is listed though the profiles loaded without him: %+v", g)
	}
}

// A session whose summary says it waits, before the approvals load or
// after their fetch failed, shows no count: the client does not know it.
// A known count shows, and NEW shows "new" (Codex review of #40, round 1).
func TestAWaitingSessionShowsACountOnlyWhenItIsKnown(t *testing.T) {
	waiting := agentSession(1, "", "waits", 0)
	waiting.State = proto.SessionWaiting
	d := sidebarData(nil, waiting, agentSession(2, "", "new", 0), agentSession(3, "", "waits twice", 0))
	if r, _ := d.Sidebar(SidebarOptions{Now: pocketdtest.Now}).Session(1); r.Mark != MarkWaiting || r.Badge != "" {
		t.Errorf("a session waiting on approvals the client has not loaded shows %+v, want no count", r)
	}
	d.SetApprovals(d.Begin(), []proto.Approval{approval("a1", 3, "c1", 0), approval("a2", 3, "c2", 0)})
	sb := d.Sidebar(SidebarOptions{Now: pocketdtest.Now})
	if r, _ := sb.Session(3); r.Mark != MarkWaiting || r.Badge != "2" {
		t.Errorf("a session with 2 approvals loaded shows %+v, want the badge 2", r)
	}
	if r, _ := sb.Session(2); r.Badge != "new" {
		t.Errorf("a NEW session shows the badge %q", r.Badge)
	}
}

// A group lists at most GroupSessions of its sessions and Recent at most
// RecentSessions, the newest; a group counts all of its sessions that wait
// or run.
func TestTheSidebarListsTheNewestSessionsUpToItsLimits(t *testing.T) {
	var sessions []proto.Session
	for i := range 50 {
		s := agentSession(int64(i+1), "duncan", fmt.Sprint("task ", i+1), -time.Duration(i)*time.Minute)
		if i == 45 {
			s.State = proto.SessionRunning
		}
		sessions = append(sessions, s)
	}
	d := sidebarData(team, sessions...)
	d.SetApprovals(d.Begin(), []proto.Approval{approval("a1", 48, "c1", -time.Minute), approval("a2", 48, "c2", 0), approval("a3", 2, "c3", 0)})
	sb := d.Sidebar(SidebarOptions{Now: pocketdtest.Now})
	kids := sb.Children(GroupKey("duncan"))
	if len(kids) != GroupSessions || kids[0] != SessionKey(1) || kids[GroupSessions-1] != SessionKey(GroupSessions) {
		t.Errorf("Duncan's group holds %q, want sessions 1 to %d", keysOf(kids), GroupSessions)
	}
	recent := sb.Sections[2].Items
	if len(recent) != RecentSessions || recent[0] != SessionKey(1) || recent[RecentSessions-1] != SessionKey(RecentSessions) {
		t.Errorf("Recent lists %q, want sessions 1 to %d", keysOf(recent), RecentSessions)
	}
	if g, _ := sb.Group("duncan"); g.Waiting != 2 || g.Running != 1 {
		t.Errorf("Duncan's group counts %d waiting and %d running, want 2 and 1, its sessions past the limit included", g.Waiting, g.Running)
	}
	if r, _ := sb.Session(48); r.Mark != MarkWaiting || r.Waiting != 2 || r.Waits != "bash go test ./..." {
		t.Errorf("session 48 shows %+v, want 2 approvals waiting, the oldest named", r)
	}
}

// A filter lists the sessions whose title or folder holds it, whatever its
// case, at most FoundSessions, in place of every other section.
func TestTheFilterSearchesEverySessionsTitleAndFolder(t *testing.T) {
	sessions := []proto.Session{
		agentSession(1, "duncan", "Snow image fails to boot", 0),
		agentSession(2, "", "Draft the release notes", -time.Minute),
		agentSession(3, "odrade", "", -2*time.Minute),
	}
	sessions[1].Cwd = "/home/team/projects/SNOWFLAKE"
	for i := range 120 {
		sessions = append(sessions, agentSession(int64(100+i), "", "snow day", -time.Duration(10+i)*time.Minute))
	}
	d := sidebarData(team, sessions...)
	sb := d.Sidebar(SidebarOptions{Places: []string{"inbox"}, Filter: "  sNoW ", Now: pocketdtest.Now})
	if got := sectionIDs(sb); !slices.Equal(got, []string{"found Sessions"}) {
		t.Fatalf("a filter shows the sections %q, want only what it found", got)
	}
	hits := sb.Sections[0].Items
	if len(hits) != FoundSessions || hits[0] != SessionKey(1) || hits[1] != SessionKey(2) || hits[2] != SessionKey(100) {
		t.Errorf("the filter found %q, want 1, 2 (by its folder), then the others, %d in all", keysOf(hits[:3]), FoundSessions)
	}
	// A session with no title is found by the title it shows.
	sb = d.Sidebar(SidebarOptions{Filter: "new sess", Now: pocketdtest.Now})
	if got := keysOf(sb.Sections[0].Items); !slices.Equal(got, []string{"3"}) {
		t.Errorf("found %q, want the untitled session 3", got)
	}
	// A blank filter is none.
	if sb = d.Sidebar(SidebarOptions{Filter: "   ", Now: pocketdtest.Now}); len(sb.Sections) != 3 {
		t.Errorf("a blank filter shows %q", sectionIDs(sb))
	}
}

// A session's tag names the agent it belongs to, as "Clerk: Desk batch
// [Odrade]" does for the session the Clerk, Odrade, works in; not the
// agent that started it. A title that starts with the agent's name, or its
// first word, needs none; the owner's own session is tagged with its
// folder.
func TestASessionsTagNamesTheAgentItBelongsTo(t *testing.T) {
	profiles := append(slices.Clone(team), proto.Profile{ID: "irulan", Name: "Princess Irulan", Role: proto.RoleAgent},
		proto.Profile{ID: "broken", Problem: "no model"})
	own := agentSession(5, "", "Fix the flaky CI", 0)
	own.Cwd = "/home/team/projects/nsl"
	d := sidebarData(profiles,
		agentSession(1, "odrade", "Clerk: Desk batch", 0),
		agentSession(2, "odrade", "Odrade: plan the week", 0),
		agentSession(3, "irulan", "Princess Irulan's notes", 0),
		agentSession(4, "irulan", "Princess for a day", 0),
		own,
		agentSession(6, "zia", "T4 · Old work", 0),
		agentSession(7, "broken", "T9 · Review", 0),
	)
	sb := d.Sidebar(SidebarOptions{Now: pocketdtest.Now})
	for id, want := range map[int64]string{1: "Odrade", 2: "", 3: "", 4: "", 5: "nsl", 6: "zia", 7: "broken"} {
		if r, _ := sb.Session(id); r.Tag != want {
			t.Errorf("session %d (%q) is tagged %q, want %q", id, r.Title, r.Tag, want)
		}
	}
}

// NEW marks a session updated since the owner last had it open, or ever,
// for one never seen, less than NewFor ago: unless it waits or runs, which
// its row says first, or the window shows it.
func TestNewMarksASessionUpdatedSinceTheOwnerSawIt(t *testing.T) {
	now := pocketdtest.Now
	waiting := agentSession(4, "", "waits", -time.Hour)
	running := agentSession(5, "", "runs", -time.Hour)
	running.State = proto.SessionRunning
	d := sidebarData(nil,
		agentSession(1, "", "never seen", -time.Hour),
		agentSession(2, "", "seen as it is", -time.Hour),
		agentSession(3, "", "updated since", -time.Hour),
		waiting, running,
		agentSession(6, "", "a month old", -NewFor),
		agentSession(7, "", "open", -time.Hour),
	)
	d.SetApprovals(d.Begin(), []proto.Approval{approval("a1", 4, "c1", 0)})
	d.SetOpen(7)
	seen := Seen{2: now.Add(-time.Hour), 3: now.Add(-2 * time.Hour)}
	sb := d.Sidebar(SidebarOptions{Seen: seen, Now: now})
	for id, want := range map[int64]SessionMark{1: MarkNew, 2: MarkSeen, 3: MarkNew, 4: MarkWaiting, 5: MarkRunning, 6: MarkSeen, 7: MarkSeen} {
		if r, _ := sb.Session(id); r.Mark != want {
			t.Errorf("session %d (%s) is marked %d, want %d", id, r.Title, r.Mark, want)
		}
	}
	// Once the window shows another place, the session it showed is NEW
	// again until the owner's mark for it moves.
	d.SetOpen(0)
	if r, _ := d.Sidebar(SidebarOptions{Seen: seen, Now: now}).Session(7); r.Mark != MarkNew {
		t.Errorf("session 7, no longer shown, is marked %d, want NEW", r.Mark)
	}
	s7, _ := d.Session(7)
	seen, moved := seen.Saw(s7)
	if r, _ := d.Sidebar(SidebarOptions{Seen: seen, Now: now}).Session(7); !moved || r.Mark != MarkSeen {
		t.Errorf("session 7, seen as it is, is marked %d (mark moved: %v)", r.Mark, moved)
	}
}

// pocket dims an idle session the owner saw, last updated more than
// OldAfter ago; a NEW one, or one that runs or waits, stays bright.
func TestAnOldSeenSessionIsDimmed(t *testing.T) {
	now := pocketdtest.Now
	old := -OldAfter - time.Minute
	running := agentSession(3, "", "runs", old)
	running.State = proto.SessionRunning
	d := sidebarData(nil, agentSession(1, "", "old and seen", old), agentSession(2, "", "old and new", old), running,
		agentSession(4, "", "a week old", -OldAfter))
	seen := Seen{1: now.Add(old), 3: now.Add(old), 4: now.Add(-OldAfter)}
	sb := d.Sidebar(SidebarOptions{Seen: seen, Now: now})
	for id, want := range map[int64]bool{1: true, 2: false, 3: false, 4: false} {
		if r, _ := sb.Session(id); r.Dim != want {
			t.Errorf("session %d (%s) dim %v, want %v", id, r.Title, r.Dim, want)
		}
	}
}

// A mark never moves back, and Prune drops only the marks that tell
// nothing a missing one does not.
func TestSeenMarksNeverMoveBackAndPruneOnlyWhatTellsNothing(t *testing.T) {
	now := pocketdtest.Now
	var seen Seen
	s := agentSession(1, "", "demo", -time.Hour)
	seen, moved := seen.Saw(s)
	if !moved || !seen[1].Equal(s.UpdatedAt) {
		t.Fatalf("a first sight marked %v (moved %v)", seen[1], moved)
	}
	older := s
	older.UpdatedAt = s.UpdatedAt.Add(-time.Minute)
	if _, moved := seen.Saw(older); moved || !seen[1].Equal(s.UpdatedAt) {
		t.Errorf("an older copy moved the mark back to %v", seen[1])
	}
	if _, moved := seen.Saw(s); moved {
		t.Error("the same copy moved the mark")
	}
	seen[2] = now.Add(-NewFor)
	seen[3] = now.Add(-NewFor + time.Second)
	seen.Prune(now)
	if _, ok := seen[2]; ok {
		t.Error("a mark NewFor old was kept")
	}
	if _, ok := seen[3]; !ok {
		t.Error("a mark younger than NewFor was dropped")
	}
	// Pruning changes no mark: each session shows as it did.
	d := sidebarData(nil, agentSession(2, "", "seen long ago", -NewFor+time.Hour), agentSession(4, "", "seen and old", -NewFor-time.Hour))
	before := Seen{2: now.Add(-NewFor - 2*time.Hour), 4: now.Add(-NewFor - time.Hour)}
	after := Seen{2: before[2], 4: before[4]}
	after.Prune(now)
	for _, id := range []int64{2, 4} {
		b, _ := d.Sidebar(SidebarOptions{Seen: before, Now: now}).Session(id)
		a, _ := d.Sidebar(SidebarOptions{Seen: after, Now: now}).Session(id)
		if a.Mark != b.Mark || a.Dim != b.Dim {
			t.Errorf("session %d shows %+v once pruned, %+v before", id, a, b)
		}
	}
}

// An agent's row names it, badges a supervisor or a clerk, and its hint
// says who it is and where it works; a profile with a problem says the
// problem, with its id for a name.
func TestAnAgentsRowNamesItsRoleAndWhereItWorks(t *testing.T) {
	profiles := append(slices.Clone(team), proto.Profile{ID: "broken", Problem: "no sign-in serves its model"},
		proto.Profile{ID: "nameless", Role: proto.RoleAgent, Cwd: "/home/team/work", Model: proto.ModelRef{ID: "deepseek-v4"}})
	sb := sidebarData(profiles).Sidebar(SidebarOptions{Now: pocketdtest.Now})
	for _, tc := range []struct{ agent, name, badge, problem string }{
		{"duncan", "Duncan", "", ""},
		{"odrade", "Odrade", "clerk", ""},
		{"sheeana", "Sheeana", "supervisor", ""},
		{"broken", "broken", "", "no sign-in serves its model"},
		{"nameless", "nameless", "", ""},
	} {
		g, ok := sb.Group(tc.agent)
		if !ok || g.Name != tc.name || g.Badge != tc.badge || g.Problem != tc.problem || g.Unlisted {
			t.Errorf("group %s shows %+v, want name %q, badge %q, problem %q", tc.agent, g, tc.name, tc.badge, tc.problem)
		}
	}
	if g, _ := sb.Group("duncan"); g.Hint != "Duncan · Specialist\nclaude-opus-5-5\nHome: /home/team/agents/duncan\nRepositories:\n  frostyard/nsl\n  frostyard/snow" {
		t.Errorf("Duncan's hint is %q", g.Hint)
	}
	if g, _ := sb.Group("nameless"); g.Hint != "nameless · Specialist\ndeepseek-v4\nWorkspace: /home/team/work" {
		t.Errorf("the nameless agent's hint is %q", g.Hint)
	}
	if g, _ := sb.Group("broken"); g.Hint != "" {
		t.Errorf("a broken profile hints %q; its problem shows instead", g.Hint)
	}
	if g, _ := sb.Group("sheeana"); !strings.HasPrefix(g.Hint, "Sheeana · Supervisor") {
		t.Errorf("Sheeana's hint is %q", g.Hint)
	}
}

// A click on a group's row folds or unfolds its sessions, as its arrow
// does, the owner's group's included; a group with none has nothing to
// fold. Choosing a place or a session shows it, and choosing a group shows
// nothing of its own.
func TestAClickOnAGroupFoldsItsSessions(t *testing.T) {
	sb := sidebarData(team, agentSession(1, "duncan", "T5 · Snow boots", 0), agentSession(2, "", "Fix the flaky CI", 0)).
		Sidebar(SidebarOptions{Places: []string{"inbox"}, Now: pocketdtest.Now})
	for k, want := range map[SidebarKey]bool{
		GroupKey("duncan"): true, GroupKey(""): true, GroupKey("sheeana"): false, SessionKey(1): false, PlaceKey("inbox"): false,
	} {
		if got := sb.Folds(k); got != want {
			t.Errorf("a click on %q folds: %v, want %v", keysOf([]SidebarKey{k}), got, want)
		}
	}
	for k, want := range map[SidebarKey]bool{GroupKey("duncan"): false, GroupKey(""): false, SessionKey(1): true, PlaceKey("inbox"): true} {
		if got := Shows(k); got != want {
			t.Errorf("choosing %q shows it: %v, want %v", keysOf([]SidebarKey{k}), got, want)
		}
	}
}

// The sidebar shows a row chosen at the top of the first section that
// lists it there, as pocket's; a session past Recent's limit under its
// group; and no row for a session past its group's limit too, rather than
// keep a row that stands for something else (Codex review of #40, round
// 1).
func TestTheSidebarChoosesTheRowThatShowsWhatTheWindowShows(t *testing.T) {
	var sessions []proto.Session
	for i := range GroupSessions + RecentSessions {
		agent := "duncan"
		if i >= RecentSessions {
			agent = "odrade"
		}
		sessions = append(sessions, agentSession(int64(i+1), agent, "task", -time.Duration(i)*time.Minute))
	}
	sb := sidebarData(team, sessions...).Sidebar(SidebarOptions{Places: []string{"inbox"}, Now: pocketdtest.Now})
	for k, want := range map[SidebarKey]string{
		PlaceKey("inbox"): SectionPlaces, GroupKey("duncan"): SectionTeam, SessionKey(1): SectionRecent,
		SessionKey(RecentSessions + 1): SectionTeam,
	} {
		if c, ok := sb.ChoiceOf(k); !ok || c.Section != want || c.Key != k {
			t.Errorf("%v shows chosen as %+v (%v), want in %s", keysOf([]SidebarKey{k}), c, ok, want)
		}
	}
	// Duncan has 40 sessions: his group lists the newest 30, and Recent
	// the newest 40 sessions of all, so session 31 is under neither.
	sb = sidebarData(team, func() []proto.Session {
		var out []proto.Session
		for i := range RecentSessions + 1 {
			out = append(out, agentSession(int64(i+1), "duncan", "task", -time.Duration(i)*time.Minute))
		}
		return out
	}()...).Sidebar(SidebarOptions{Now: pocketdtest.Now})
	if c, ok := sb.ChoiceOf(SessionKey(RecentSessions + 1)); ok {
		t.Errorf("a session no row shows is chosen as %+v", c)
	}
	if _, ok := sb.ChoiceOf(PlaceKey("inbox")); ok {
		t.Error("a place the sidebar does not list is chosen")
	}
}

// A click on a group's row moves the choice no more than its arrow does: the
// row chosen before stays, unless the fold hid it; then the row of what
// the window shows in Recent, else the group (Codex review of #40, round
// 1: choosing the hidden row again would unfold the group).
func TestAClickOnAGroupKeepsTheChoiceUnlessItsFoldHidesIt(t *testing.T) {
	var sessions []proto.Session
	for i := range RecentSessions + 1 {
		sessions = append(sessions, agentSession(int64(i+1), "duncan", "task", -time.Duration(i)*time.Minute))
	}
	sessions = append(sessions, agentSession(100, "", "own", -time.Hour))
	sb := sidebarData(team, sessions...).Sidebar(SidebarOptions{Places: []string{"inbox"}, Now: pocketdtest.Now})
	duncan := GroupKey("duncan")
	inbox := Choice{Section: SectionPlaces, Key: PlaceKey("inbox")}
	underDuncan := Choice{Section: SectionTeam, Key: SessionKey(2)}
	inRecent := Choice{Section: SectionRecent, Key: SessionKey(2)}
	shown := SessionKey(2)
	old := SessionKey(RecentSessions + 1)
	for _, tc := range []struct {
		name   string
		folded bool
		before *Choice
		shown  *SidebarKey
		want   Choice
	}{
		{"a fold keeps a place chosen", true, &inbox, nil, inbox},
		{"an unfold keeps a session chosen under the group", false, &underDuncan, &shown, underDuncan},
		{"a fold keeps the session chosen in Recent", true, &inRecent, &shown, inRecent},
		{"a fold that hides the session chosen chooses it in Recent", true, &underDuncan, &shown, inRecent},
		{"a fold that hides a session past Recent chooses the group", true, &Choice{Section: SectionTeam, Key: SessionKey(30)}, &old, Choice{Section: SectionTeam, Key: duncan}},
		{"a click with nothing chosen chooses what the window shows", false, nil, &shown, inRecent},
		{"a click on an empty group keeps the choice", false, &inbox, nil, inbox},
	} {
		g := duncan
		if tc.name == "a click on an empty group keeps the choice" {
			g = GroupKey("sheeana")
		}
		if got := sb.AfterGroupClick(g, tc.folded, tc.before, tc.shown); got != tc.want {
			t.Errorf("%s: chose %+v, want %+v", tc.name, got, tc.want)
		}
	}
}
