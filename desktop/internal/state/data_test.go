package state

import (
	"slices"
	"testing"
	"time"

	"github.com/frostyard/pocketgo/proto"
)

func ids(list []proto.Session) []int64 {
	var out []int64
	for _, s := range list {
		out = append(out, s.ID)
	}
	return out
}

func approvalIDs(list []proto.Approval) []string {
	var out []string
	for _, a := range list {
		out = append(out, a.ID)
	}
	return out
}

func TestASessionEventDuringAFetchWinsOverItsAnswer(t *testing.T) {
	var d Data
	f := d.Begin()
	running := session(1, "demo", 0)
	running.State = proto.SessionRunning
	d.Apply(proto.Event{Type: proto.EventSession, Session: &running})
	// The fetch was answered before the run started, and arrives after.
	d.SetSessions(f, []proto.Session{session(1, "demo", 0), session(2, "other", -time.Minute)})
	if got := d.StateOf(1); got != proto.SessionRunning {
		t.Errorf("session 1 is %s after a stale list, want running", got)
	}
	if !slices.Equal(ids(d.Sessions()), []int64{1, 2}) {
		t.Errorf("sessions %v, want [1 2]", ids(d.Sessions()))
	}
	// A fetch begun after the event takes the answer's copy.
	f = d.Begin()
	d.SetSessions(f, []proto.Session{session(1, "renamed", 0)})
	if s, _ := d.Session(1); s.Title != "renamed" || s.State != proto.SessionIdle {
		t.Errorf("a fetch begun after the event kept the event's copy: %+v", s)
	}
}

func TestASessionCreatedDuringAFetchStays(t *testing.T) {
	var d Data
	f := d.Begin()
	created := session(3, "new", time.Minute)
	d.Apply(proto.Event{Type: proto.EventSession, Session: &created})
	d.SetSessions(f, []proto.Session{session(1, "old", 0)})
	if !slices.Equal(ids(d.Sessions()), []int64{3, 1}) {
		t.Errorf("sessions %v, want the created one kept first: [3 1]", ids(d.Sessions()))
	}
}

func TestAnOlderFetchAnsweredLastIsDropped(t *testing.T) {
	var d Data
	older, newer := d.Begin(), d.Begin()
	d.SetSessions(newer, []proto.Session{session(1, "new title", 0)})
	d.SetSessions(older, []proto.Session{session(1, "old title", 0)})
	if s, _ := d.Session(1); s.Title != "new title" {
		t.Errorf("an older fetch answered last won: %q", s.Title)
	}
	d.SetApprovals(newer, nil)
	d.SetApprovals(older, []proto.Approval{approval("a1", 1, "c1", 0)})
	if len(d.Approvals()) != 0 {
		t.Errorf("an older approval list answered last put back %v", approvalIDs(d.Approvals()))
	}
}

func TestADecidedApprovalStaysGoneWhenAStaleListArrives(t *testing.T) {
	var d Data
	d.SetApprovals(d.Begin(), []proto.Approval{approval("a1", 1, "c1", 0)})
	f := d.Begin()
	decided := approval("a1", 1, "c1", 0)
	decided.State = proto.ApprovalAllowed
	d.Apply(proto.Event{Type: proto.EventApproval, Approval: &decided})
	d.SetApprovals(f, []proto.Approval{approval("a1", 1, "c1", 0)})
	if len(d.Approvals()) != 0 {
		t.Errorf("a decided approval came back from a stale list: %v", approvalIDs(d.Approvals()))
	}
	if d.StateOf(1) == proto.SessionWaiting {
		t.Error("the session still waits")
	}
}

func TestAnApprovalRequestedDuringAFetchStays(t *testing.T) {
	var d Data
	f := d.Begin()
	a := approval("a2", 2, "c2", time.Minute)
	d.Apply(proto.Event{Type: proto.EventApproval, Approval: &a})
	d.SetApprovals(f, []proto.Approval{approval("a1", 1, "c1", 0)})
	if !slices.Equal(approvalIDs(d.Approvals()), []string{"a1", "a2"}) {
		t.Errorf("approvals %v, want [a1 a2]", approvalIDs(d.Approvals()))
	}
}

func TestApprovalsShowInTheInboxOldestFirstAndInTheirSession(t *testing.T) {
	var d Data
	for _, a := range []proto.Approval{approval("late", 1, "c3", 2*time.Minute), approval("early", 2, "c1", 0), approval("mid", 1, "c2", time.Minute)} {
		d.Apply(proto.Event{Type: proto.EventApproval, Approval: &a})
	}
	if got := approvalIDs(d.Approvals()); !slices.Equal(got, []string{"early", "mid", "late"}) {
		t.Errorf("the Inbox lists %v, want oldest first: [early mid late]", got)
	}
	if got := approvalIDs(d.ApprovalsFor(1)); !slices.Equal(got, []string{"mid", "late"}) {
		t.Errorf("session 1 docks %v, want its own, oldest first: [mid late]", got)
	}
	if len(d.ApprovalsFor(3)) != 0 {
		t.Error("a session without approvals docks some")
	}
}

func TestStateOfFollowsTheServiceUnlessAnApprovalIsPending(t *testing.T) {
	var d Data
	s1, s2 := session(1, "a", 0), session(2, "b", 0)
	s2.State = proto.SessionRunning
	d.SetSessions(d.Begin(), []proto.Session{s1, s2})
	if d.StateOf(1) != proto.SessionIdle || d.StateOf(2) != proto.SessionRunning {
		t.Fatalf("states %s, %s; want idle, running", d.StateOf(1), d.StateOf(2))
	}
	// The approval's event comes before the session's own.
	a := approval("a1", 2, "c1", 0)
	d.Apply(proto.Event{Type: proto.EventApproval, Approval: &a})
	if d.StateOf(2) != proto.SessionWaiting {
		t.Errorf("a session with a pending approval is %s, want waiting", d.StateOf(2))
	}
	if waiting, running := d.Counts(); waiting != 1 || running != 0 {
		t.Errorf("counts %d waiting, %d running; want 1, 0", waiting, running)
	}
	if d.StateOf(99) != proto.SessionIdle {
		t.Error("an unknown session is not idle")
	}
	// The open session's live state comes before its session event.
	d.SetOpen(1)
	d.Apply(proto.Event{Type: proto.EventLive, Live: &proto.Live{SessionID: 1, Busy: true}})
	if d.StateOf(1) != proto.SessionRunning {
		t.Errorf("the open session with a busy run is %s, want running", d.StateOf(1))
	}
	d.Apply(proto.Event{Type: proto.EventApproval, Approval: &proto.Approval{ID: "a2", SessionID: 1, State: proto.ApprovalPending}})
	if d.StateOf(1) != proto.SessionWaiting {
		t.Errorf("the open session waiting for an approval is %s, want waiting", d.StateOf(1))
	}
}

func TestADetailThatFailsSaysWhy(t *testing.T) {
	var d Data
	d.SetOpen(1)
	d.SetDetailError(2, "not for this session")
	d.SetDetailError(1, "not_found: no such session")
	if o := d.Open(); o.Err != "not_found: no such session" || o.Loaded {
		t.Errorf("open session %+v", o)
	}
	d.SetDetail(d.Begin(), proto.SessionDetail{Session: session(1, "demo", 0)})
	if o := d.Open(); o.Err != "" || !o.Loaded {
		t.Errorf("a detail after the error left %+v", o)
	}
}

func TestALateDetailDoesNotUndoAnEndedRun(t *testing.T) {
	var d Data
	d.SetOpen(1)
	f := d.Begin()
	d.Apply(proto.Event{Type: proto.EventLive, Live: &proto.Live{SessionID: 1}}) // the run ended
	d.SetDetail(f, proto.SessionDetail{Session: session(1, "demo", 0), Live: proto.Live{SessionID: 1, Busy: true}})
	if d.Open().Live.Busy {
		t.Error("a detail answered before the run ended made it busy again")
	}
	if !d.Open().Loaded {
		t.Error("the detail did not load")
	}
	// Without a live event since, the detail's live state stands.
	f = d.Begin()
	d.SetDetail(f, proto.SessionDetail{Session: session(1, "demo", 0), Live: proto.Live{SessionID: 1, Busy: true, Failure: "boom"}})
	if !d.Open().Live.Busy || d.Open().Live.Failure != "boom" {
		t.Errorf("live %+v, want the detail's", d.Open().Live)
	}
}

func TestEntriesMergeByIDAcrossTheDetailAndEvents(t *testing.T) {
	var d Data
	d.SetOpen(1)
	f := d.Begin()
	for _, id := range []int64{4, 3} {
		d.Apply(proto.Event{Type: proto.EventEntry, Entry: &proto.Entry{ID: id, SessionID: 1, Kind: proto.EntryUser, Text: "event"}})
	}
	d.Apply(proto.Event{Type: proto.EventEntry, Entry: &proto.Entry{ID: 9, SessionID: 2, Kind: proto.EntryUser}}) // another session's
	d.SetDetail(f, proto.SessionDetail{Session: session(1, "demo", 0), Entries: []proto.Entry{
		{ID: 1, SessionID: 1, Kind: proto.EntryUser}, {ID: 2, SessionID: 1, Kind: proto.EntryAssistant}, {ID: 3, SessionID: 1, Kind: proto.EntryUser, Text: "detail"},
	}})
	var got []int64
	for _, e := range d.Open().Entries {
		got = append(got, e.ID)
	}
	if !slices.Equal(got, []int64{1, 2, 3, 4}) {
		t.Errorf("entries %v, want [1 2 3 4]", got)
	}
	if d.Open().Entries[2].Text != "event" {
		t.Error("an entry already there was replaced")
	}
}

func TestAnAssistantEntryEndsTheReplyBeingWritten(t *testing.T) {
	var d Data
	d.SetOpen(1)
	d.Apply(proto.Event{Type: proto.EventLive, Live: &proto.Live{SessionID: 1, Busy: true, Partial: &proto.Entry{Kind: proto.EntryAssistant, Text: "Hel"}}})
	d.Apply(proto.Event{Type: proto.EventEntry, Entry: &proto.Entry{ID: 1, SessionID: 1, Kind: proto.EntryUser, Text: "hi"}})
	if d.Open().Live.Partial == nil {
		t.Fatal("a user entry ended the reply")
	}
	d.Apply(proto.Event{Type: proto.EventEntry, Entry: &proto.Entry{ID: 2, SessionID: 1, Kind: proto.EntryAssistant, Text: "Hello"}})
	if d.Open().Live.Partial != nil {
		t.Error("the reply shows twice: as the entry and as the live partial")
	}
	if !d.Open().Live.Busy {
		t.Error("the entry ended the run; only its live event does")
	}
}

func TestLiveEventsOfAnotherSessionChangeNothing(t *testing.T) {
	var d Data
	d.SetOpen(1)
	d.Apply(proto.Event{Type: proto.EventLive, Live: &proto.Live{SessionID: 2, Busy: true}})
	if d.Open().Live.Busy {
		t.Error("another session's live state reached the open one")
	}
}

func TestAnEntryMovesItsSessionUp(t *testing.T) {
	var d Data
	d.SetSessions(d.Begin(), []proto.Session{session(1, "a", 0), session(2, "b", -time.Hour)})
	at := session(1, "", time.Minute).UpdatedAt
	d.Apply(proto.Event{Type: proto.EventEntry, Entry: &proto.Entry{ID: 7, SessionID: 2, Kind: proto.EntryUser, At: at}})
	if !slices.Equal(ids(d.Sessions()), []int64{2, 1}) {
		t.Errorf("sessions %v, want the one with the new entry first: [2 1]", ids(d.Sessions()))
	}
}

func TestResyncAsksForARefetch(t *testing.T) {
	var d Data
	if !d.Apply(proto.Event{Type: proto.EventResync, Seq: 5}) {
		t.Error("resync did not ask for a refetch")
	}
	if d.Apply(proto.Event{Type: "future", Seq: 6}) {
		t.Error("an unknown event asked for a refetch")
	}
}

func TestOpeningAnotherSessionStartsItEmpty(t *testing.T) {
	var d Data
	d.SetOpen(1)
	d.Apply(proto.Event{Type: proto.EventEntry, Entry: &proto.Entry{ID: 1, SessionID: 1, Kind: proto.EntryUser}})
	d.SetOpen(1)
	if len(d.Open().Entries) != 1 {
		t.Error("opening the open session again dropped its transcript")
	}
	d.SetOpen(2)
	if d.OpenID() != 2 || len(d.Open().Entries) != 0 || d.Open().Loaded {
		t.Errorf("session 2 opened with %+v", d.Open())
	}
	f := d.Begin()
	d.SetOpen(3)
	d.SetDetail(f, proto.SessionDetail{Session: session(2, "two", 0)})
	if d.Open().Loaded {
		t.Error("the detail of a session no longer open loaded into the open one")
	}
	if _, ok := d.Session(2); !ok {
		t.Error("the detail's session did not reach the list")
	}
	d.SetOpen(0)
	if d.Open() != nil || d.OpenID() != 0 {
		t.Error("closing left a session open")
	}
}
