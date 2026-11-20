package state

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/frostyard/pocketgo/desktop/internal/pocketdtest"
	"github.com/frostyard/pocketgo/proto"
)

// A pin goes after the others; pinning a pinned session unpins it, and
// neither changes the slice the sidebar may still read.
func TestTogglePinsAfterTheOthersAndUnpins(t *testing.T) {
	p := Pins{4, 2}
	more := p.Toggle(7)
	if !slices.Equal(more, Pins{4, 2, 7}) || !more.Has(7) {
		t.Errorf("pinning 7 gives %v, want [4 2 7]", more)
	}
	less := more.Toggle(4)
	if !slices.Equal(less, Pins{2, 7}) || less.Has(4) {
		t.Errorf("unpinning 4 gives %v, want [2 7]", less)
	}
	if !slices.Equal(p, Pins{4, 2}) || !slices.Equal(more, Pins{4, 2, 7}) {
		t.Errorf("toggling changed the slices it was given: %v, %v", p, more)
	}
	if got := Pins(nil).Toggle(1); !slices.Equal(got, Pins{1}) {
		t.Errorf("pinning on no pins gives %v", got)
	}
}

// Only a session's row pins, nested or not: what it pins is its session.
func TestOnlyASessionsRowPins(t *testing.T) {
	for k, want := range map[SidebarKey]int64{
		SessionKey(3): 3, NestedKey(5, 3): 5, PlaceKey("inbox"): 0, GroupKey("duncan"): 0, GroupKey(""): 0,
	} {
		id, ok := PinOf(k)
		if id != want || ok != (want != 0) {
			t.Errorf("%v pins %d (%v), want %d", keysOf([]SidebarKey{k}), id, ok, want)
		}
	}
}

// The sessions pinned show in a section of their own, first after the
// places (the owner's choice), in the order pinned; Recent leaves them out
// and lists as many others as it would; each stays in its agent's group.
// A pin the Data does not hold, or one named twice, shows once or not at
// all, and no pins show no section. A filter finds pinned sessions as any.
func TestPinnedSessionsShowFirstAndLeaveRecent(t *testing.T) {
	var sessions []proto.Session
	for i := range RecentSessions + 5 {
		sessions = append(sessions, agentSession(int64(i+1), "duncan", fmt.Sprint("task ", i+1), -time.Duration(i)*time.Minute))
	}
	d := sidebarData(team, sessions...)
	sb := d.Sidebar(SidebarOptions{Places: []string{"inbox"}, Now: pocketdtest.Now, Pinned: Pins{7, 99, 2, 7}})
	if got, want := sectionIDs(sb), []string{"places PocketGo", "pinned Pinned", "team Team", "recent Recent"}; !slices.Equal(got, want) {
		t.Fatalf("sections %q, want %q", got, want)
	}
	if got, want := keysOf(sb.Sections[1].Items), []string{"7", "2"}; !slices.Equal(got, want) {
		t.Errorf("Pinned lists %q, want %q", got, want)
	}
	recent := sb.Sections[3].Items
	if len(recent) != RecentSessions || slices.Contains(recent, SessionKey(2)) || slices.Contains(recent, SessionKey(7)) ||
		recent[RecentSessions-1] != SessionKey(RecentSessions+2) {
		t.Errorf("Recent lists %q, want sessions 1 to %d without 2 and 7", keysOf(recent), RecentSessions+2)
	}
	if kids := sb.Children(GroupKey("duncan")); !slices.Contains(kids, SessionKey(2)) || !slices.Contains(kids, SessionKey(7)) {
		t.Errorf("Duncan's group %q leaves out a pinned session", keysOf(kids))
	}
	if c, ok := sb.ChoiceOf(SessionKey(7)); !ok || c.Section != SectionPinned {
		t.Errorf("a pinned session shows chosen as %+v (%v), want in Pinned", c, ok)
	}
	if got := sectionIDs(d.Sidebar(SidebarOptions{Now: pocketdtest.Now, Pinned: Pins{99}})); slices.Contains(got, "pinned Pinned") {
		t.Errorf("only pins the Data does not hold show sections %q", got)
	}
	found := d.Sidebar(SidebarOptions{Filter: "task 7", Now: pocketdtest.Now, Pinned: Pins{7}})
	if got := sectionIDs(found); !slices.Equal(got, []string{"found Sessions"}) || !slices.Contains(found.Sections[0].Items, SessionKey(7)) {
		t.Errorf("a filter shows %q, %q; want the pinned session found", got, keysOf(found.Sections[0].Items))
	}
}

// A pinned session holds its workers as it does in Recent, and a worker
// pinned on its own shows at Pinned's top too.
func TestAPinnedSessionHoldsItsWorkers(t *testing.T) {
	d := sidebarData(team,
		agentSession(2, "duncan", "T7 · Fix the flaky CI", -2*time.Minute),
		workerSession(3, 2, "T7 · worker", -time.Minute),
	)
	sb := d.Sidebar(SidebarOptions{Now: pocketdtest.Now, Pinned: Pins{2, 3}})
	if got, want := keysOf(sb.Sections[1].Items), []string{"2", "3"}; sb.Sections[1].ID != SectionPinned || !slices.Equal(got, want) {
		t.Fatalf("section %s lists %q, want Pinned with %q", sb.Sections[1].ID, got, want)
	}
	if got := keysOf(sb.Children(SessionKey(2))); !slices.Equal(got, []string{"3<2"}) {
		t.Errorf("the pinned specialist holds %q, want its worker", got)
	}
	if !sb.Lists(Choice{Section: SectionPinned, Key: NestedKey(3, 2)}) {
		t.Error("the worker nested under the pinned specialist does not show in Pinned")
	}
}

// Lists says whether a chosen row still shows in its section, under a row
// there or at its top: a pin takes a session's rows out of Recent, and
// an unpin out of Pinned, and a row chosen there is chosen again where it
// shows now (ChoiceOf).
func TestListsSaysWhetherAChosenRowStillShows(t *testing.T) {
	d := sidebarData(team,
		agentSession(2, "duncan", "T7 · Fix the flaky CI", -2*time.Minute),
		workerSession(3, 2, "T7 · worker", -time.Minute),
		agentSession(4, "", "Homewiki", -3*time.Minute),
	)
	before := d.Sidebar(SidebarOptions{Places: []string{"inbox"}, Now: pocketdtest.Now})
	after := d.Sidebar(SidebarOptions{Places: []string{"inbox"}, Now: pocketdtest.Now, Pinned: Pins{2}})
	for _, tc := range []struct {
		c             Choice
		before, after bool
	}{
		{Choice{SectionRecent, SessionKey(2)}, true, false},
		{Choice{SectionRecent, NestedKey(3, 2)}, true, false},
		{Choice{SectionRecent, SessionKey(3)}, true, true},
		{Choice{SectionPinned, SessionKey(2)}, false, true},
		{Choice{SectionPinned, NestedKey(3, 2)}, false, true},
		{Choice{SectionTeam, SessionKey(2)}, true, true},
		{Choice{SectionTeam, NestedKey(3, 2)}, true, true},
		{Choice{SectionTeam, GroupKey("")}, true, true},
		{Choice{SectionTeam, SessionKey(4)}, true, true},
		{Choice{SectionPlaces, PlaceKey("inbox")}, true, true},
		{Choice{SectionRecent, SessionKey(9)}, false, false},
	} {
		if got := before.Lists(tc.c); got != tc.before {
			t.Errorf("before the pin, %s %v shows: %v, want %v", tc.c.Section, keysOf([]SidebarKey{tc.c.Key}), got, tc.before)
		}
		if got := after.Lists(tc.c); got != tc.after {
			t.Errorf("after the pin, %s %v shows: %v, want %v", tc.c.Section, keysOf([]SidebarKey{tc.c.Key}), got, tc.after)
		}
	}
}
