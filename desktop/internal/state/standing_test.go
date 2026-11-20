package state

import (
	"net/http"
	"strings"
	"testing"

	"github.com/frostyard/pocketgo/proto"
)

// A standing profile's New Session names the profile alone: its standing
// session has its own folder, model and title (rule 7c). Any other sends
// what the form says.
func TestNewSessionRequestForAStandingProfileNamesItAlone(t *testing.T) {
	model := proto.ModelRef{Provider: "anthropic", ID: "claude-opus-5-5"}
	odrade := &proto.Profile{ID: "odrade", Name: "Odrade", Role: proto.RoleSupervisor, Standing: true}
	if got := NewSessionRequest(odrade, "/home/team", "Mine", model, "high"); got != (proto.CreateSession{Profile: "odrade"}) {
		t.Errorf("standing: %+v", got)
	}
	vera := &proto.Profile{ID: "vera", Name: "Vera", Role: proto.RoleAgent}
	if got := NewSessionRequest(vera, "/home/team", "Mine", model, "high"); got != (proto.CreateSession{Profile: "vera", Cwd: "/home/team", Title: "Mine", Model: model, Thinking: "high"}) {
		t.Errorf("a plain profile: %+v", got)
	}
	if got := NewSessionRequest(nil, "/w", "", model, ""); got != (proto.CreateSession{Cwd: "/w", Model: model}) {
		t.Errorf("no profile: %+v", got)
	}
	if note := StandingNote(*odrade); !strings.HasPrefix(note, "Odrade keeps one standing session") {
		t.Errorf("note %q", note)
	}
}

// New Session's profile row says that a profile keeps a standing session.
func TestAStandingProfilesChoiceSaysSo(t *testing.T) {
	choices := ProfileChoices([]proto.Profile{
		{ID: "odrade", Name: "Odrade", Role: proto.RoleSupervisor, Standing: true},
		{ID: "vera", Name: "Vera", Role: proto.RoleAgent},
	})
	if len(choices) != 3 || choices[1].Label != "Odrade · supervisor · standing session" || choices[2].Label != "Vera · agent" {
		t.Fatalf("choices %+v", choices)
	}
}

// The transcript shows a compaction as a row with its summary, a reset as
// a note, and a compaction at work as working.
func TestBuildRowsShowsCompactionsAndResets(t *testing.T) {
	entries := []proto.Entry{
		{ID: 1, Kind: proto.EntryUser, Text: "Plan the week."},
		{ID: 2, Kind: proto.EntryCompaction, Text: "## Goal\nPlan the week."},
		{ID: 3, Kind: proto.EntryReset},
	}
	rows := BuildRows(entries, proto.Live{SessionID: 1, Compacting: true}, nil)
	if len(rows) != 4 {
		t.Fatalf("rows %+v", rows)
	}
	if r := rows[1]; r.Kind != RowCompaction || r.Text != "## Goal\nPlan the week." {
		t.Errorf("the compaction %+v", r)
	}
	if r := rows[2]; r.Kind != RowNote || r.Text != StartedOverNote {
		t.Errorf("the reset %+v", r)
	}
	if r := rows[3]; r.Kind != RowWorking || r.Text != CompactingNote || !r.Live {
		t.Errorf("the compaction at work %+v", r)
	}
	if rows := BuildRows(entries, proto.Live{SessionID: 1}, nil); len(rows) != 3 {
		t.Errorf("no compaction at work: %+v", rows)
	}
}

// A Start Over tried again unchanged keeps its request id; one after the
// session wrote more, its first's reset included even when that answer was
// lost, is a new request, so it starts over again instead of replaying.
func TestAStartOverTriedAgainKeepsItsRequestIDUntilTheSessionMoves(t *testing.T) {
	h := newHarness(t)
	odrade := session(1, "Odrade", 0)
	odrade.Standing = true
	h.srv.AddSession(odrade, proto.Entry{ID: 1, SessionID: 1, Kind: proto.EntryUser, Text: "hello"})
	h.connect(t)
	h.store.Open(1)
	h.loop.RunUntil(t, func() bool { return h.store.Data.Open().Loaded })
	startOver := func() string {
		h.store.StartOver(1)
		h.loop.RunUntil(t, func() bool { return !h.store.StartingOver(1).Busy })
		return h.store.StartingOver(1).Err
	}
	resets := func() int {
		n := 0
		for _, e := range h.store.Data.Open().Entries {
			if e.Kind == proto.EntryReset {
				n++
			}
		}
		return n
	}
	p := proto.Path(proto.PathStartOver, 1)
	h.srv.FailOnce(http.MethodPost, p, http.StatusBadGateway)
	if err := startOver(); err == "" {
		t.Fatal("the failure was not kept")
	}
	if err := startOver(); err != "" {
		t.Fatal(err)
	}
	h.loop.RunUntil(t, func() bool { return resets() == 1 })
	// Its answer lost after it took effect: the reset arrives all the
	// same, and the next Start Over is a request of its own.
	h.srv.LoseAnswerOnce(http.MethodPost, p)
	if err := startOver(); err == "" {
		t.Fatal("the lost answer was not kept")
	}
	h.loop.RunUntil(t, func() bool { return resets() == 2 })
	if err := startOver(); err != "" {
		t.Fatal(err)
	}
	h.loop.RunUntil(t, func() bool { return resets() == 3 })
	ids := sentIDs(t, h, p)
	if len(ids) != 4 || ids[0] != ids[1] || ids[2] == ids[1] || ids[3] == ids[2] {
		t.Fatalf("request ids %q", ids)
	}
}
