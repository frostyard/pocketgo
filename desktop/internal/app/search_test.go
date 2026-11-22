package app

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"

	"github.com/frostyard/pocketgo/desktop/internal/state"
	"github.com/frostyard/pocketgo/proto"
)

// oldReply is the entry of the old session's reply that alone says
// "capacitor".
const oldReply = 108

// seedSearch gives the fake pocketd the seeded sessions, and an old
// session whose long transcript says "capacitor" in one reply, far from
// its end, and a newer one that says "arm64".
func (x *testApp) seedSearch() {
	x.seed()
	model := proto.ModelRef{Provider: "anthropic", ID: "claude-opus-5-5"}
	start := at(-9 * 24 * time.Hour)
	var entries []proto.Entry
	for i := int64(0); i < 40; i++ {
		id := 100 + i
		e := proto.Entry{ID: id, SessionID: 9, At: start.Add(time.Duration(i) * time.Minute)}
		switch {
		case id == oldReply:
			e.Kind, e.Text = proto.EntryAssistant, "Found it: the flux capacitor in the initramfs drifted out of tune, so the first boot after an update hangs on its hook. "+
				"I pinned it in the image's dracut config and rebuilt."
		case i%2 == 0:
			e.Kind, e.Text = proto.EntryUser, fmt.Sprintf("Step %d: what do the boot logs say now?", i/2+1)
		default:
			e.Kind, e.Text = proto.EntryAssistant, fmt.Sprintf("Step %d: the journal shows the unit waiting on the root mount again.", i/2+1)
			e.ToolCalls = []proto.ToolCall{{ID: fmt.Sprintf("c%d", id), Name: "bash", Arguments: json.RawMessage(`{"command":"journalctl -b -1"}`)}}
		}
		entries = append(entries, e)
	}
	x.srv.AddSession(proto.Session{ID: 9, Title: "Snow: first boot hangs", Cwd: "/home/team/projects/snow", Model: model,
		State: proto.SessionIdle, CreatedAt: start, UpdatedAt: start.Add(40 * time.Minute)}, entries...)
	x.srv.AddSession(proto.Session{ID: 10, Title: "Port the capacitor tests", Cwd: "/home/team/projects/snow", Model: model,
		State: proto.SessionIdle, UpdatedAt: at(-2 * 24 * time.Hour)})
}

// hitLabel is what a hit's button says: its session's title and snippet.
func hitLabel(t *testing.T, x *testApp, session, entry int64) string {
	t.Helper()
	for _, g := range x.a.st.Data.SearchGroups() {
		for _, h := range g.Hits {
			if g.Session == session && h.EntryID == entry {
				return g.Title + ": " + h.Snippet
			}
		}
	}
	t.Fatalf("no hit of session %d at entry %d in %+v", session, entry, x.a.st.Data.Search().Results)
	return ""
}

// The issue's Done when (ADR-0050): Ctrl+Shift+F opens the Search view,
// a word that only an old session's reply says finds that session, and
// its hit opens the session at the reply, highlighted.
func TestTheSearchSnapshot(t *testing.T) {
	x := newTestApp(t)
	x.seedSearch()
	x.connect(t)
	cmd, ok := x.a.table.Command("search")
	if !ok || cmd.Title != "Search Sessions…" || len(cmd.Bindings) != 1 || cmd.Bindings[0].Mods != ui.Cmd|ui.Shift || cmd.Bindings[0].Key != ui.KeyF {
		t.Fatalf("the search command %+v, %v", cmd, ok)
	}
	x.tt.Key(ui.Cmd|ui.Shift, ui.KeyF)
	x.settle()
	if x.a.place != placeSearch {
		t.Fatalf("Ctrl+Shift+F shows place %d", x.a.place)
	}
	x.hasAll(t, "Search every session")
	// The field has the focus.
	x.tt.Type("capacitor")
	x.settle()
	x.tt.Key(0, ui.KeyEnter)
	x.until(t, func() bool { s := x.a.st.Data.Search(); return !s.Loading && s.Status == state.ListLoaded })
	x.hasAll(t, "Snow: first boot hangs", "Port the capacitor tests", "2 hits in 2 sessions")
	if got := x.srv.RequestsTo("GET", proto.PathSearch); len(got) != 1 || !strings.Contains(got[0].Query, "q=capacitor") {
		t.Errorf("searches sent %+v", got)
	}
	x.snapshot(t, "search")

	if err := x.tt.Click(hitLabel(t, x, 9, oldReply)); err != nil {
		t.Fatal(err)
	}
	// The reply shows highlighted as the session opens at it: the frames
	// of its first half second.
	x.loop.RunUntil(t, func() bool { o := x.a.st.Data.Open(); return o != nil && o.ID == 9 && o.Loaded })
	for range 5 {
		x.loop.Flush()
		x.tt.Frame()
		x.tt.Advance(100 * time.Millisecond)
	}
	x.tt.Frame()
	if x.a.tr.flash != fmt.Sprintf("e%d", oldReply) {
		t.Errorf("the row highlighted is %q", x.a.tr.flash)
	}
	x.snapshot(t, "search-open")
	if x.a.place != placeSession || x.a.tr.seek != (state.Seek{}) {
		t.Fatalf("the hit shows place %d, seek %+v", x.a.place, x.a.tr.seek)
	}
	row, ok := state.TargetRow(x.a.tr.rows, oldReply)
	if !ok || x.a.tr.rows[row].Key != fmt.Sprintf("e%d", oldReply) {
		t.Fatalf("no row for the reply: %d %v", row, ok)
	}
	if first, last := x.a.tr.list.Visible(); row < first || row > last {
		t.Errorf("the reply is row %d, and the transcript shows rows %d to %d", row, first, last)
	}
	if first, _ := x.a.tr.list.Visible(); first == 0 || x.a.tr.list.AtEnd() {
		t.Errorf("the transcript shows its start or its end, not the reply: from row %d, at the end %v", first, x.a.tr.list.AtEnd())
	}
	// The highlight goes in a moment.
	for range 10 {
		x.settle()
	}
	if x.a.tr.flash != "" {
		t.Errorf("the reply is still highlighted: %q", x.a.tr.flash)
	}
}

// The sidebar filter's last row searches every session for its words; a
// filter with no word offers none.
func TestTheFilterOffersToSearchEverySession(t *testing.T) {
	x := newTestApp(t)
	x.seedSearch()
	x.connect(t)
	x.typeInto(t, "Filter sessions", "-- ")
	if slices.ContainsFunc(x.tt.Texts(), func(s string) bool { return strings.HasPrefix(s, "Search all sessions for") }) {
		t.Errorf("a filter with no word offers a search: %q", x.tt.Texts())
	}
	x.a.filter = ""
	x.settle()
	x.typeInto(t, "Filter sessions", "capacitor")
	x.hasAll(t, "Port the capacitor tests", "Search all sessions for “capacitor”")
	x.click(t, "Search all sessions for “capacitor”")
	x.until(t, func() bool { s := x.a.st.Data.Search(); return !s.Loading && s.Status == state.ListLoaded })
	if x.a.place != placeSearch || x.a.search.query != "capacitor" || x.a.st.Data.Search().Answered != "capacitor" {
		t.Fatalf("the filter's search shows place %d, field %q, %+v", x.a.place, x.a.search.query, x.a.st.Data.Search())
	}
	x.hasAll(t, "Snow: first boot hangs")
	if k, _, ok := x.a.sidebar.Selected(); !ok || k != state.SearchKey() {
		t.Errorf("the sidebar chose %+v, %v", k, ok)
	}
	// A title's hit opens its session where it opens.
	x.click(t, hitLabel(t, x, 10, 0))
	x.until(t, func() bool { return x.a.st.Data.OpenID() == 10 })
	if x.a.tr.seek != (state.Seek{}) || x.a.tr.flash != "" {
		t.Errorf("a title's hit seeks %+v, flashes %q", x.a.tr.seek, x.a.tr.flash)
	}
}

// A search that finds nothing, or fails, says so.
func TestASearchSaysWhatItFoundOrWhyNot(t *testing.T) {
	x := newTestApp(t)
	x.seedSearch()
	x.connect(t)
	x.srv.SetSearchIndexing(true)
	x.a.goSearch("quasar")
	x.until(t, func() bool { s := x.a.st.Data.Search(); return !s.Loading && s.Status == state.ListLoaded })
	x.hasAll(t, "Nothing found", "No session says “quasar”.", state.IndexingNote(proto.SearchResults{Indexing: true}))
	x.srv.FailOnce("GET", proto.PathSearch, 500)
	x.a.goSearch("capacitor")
	x.until(t, func() bool { return x.a.st.Data.Search().Status == state.ListFailed })
	x.click(t, "Try Again")
	x.until(t, func() bool { s := x.a.st.Data.Search(); return !s.Loading && s.Status == state.ListLoaded })
	x.hasAll(t, "Snow: first boot hangs")
}

// On an iPhone (ADR-0044) the Sessions tab's field offers the same search,
// its hits open as a list, and a hit's session goes back to them.
func TestThePhoneSearchSnapshot(t *testing.T) {
	if !inPhoneProcess(t) {
		return
	}
	x := newTestApp(t)
	x.seedSearch()
	x.phoneApp(t, true, nil)
	x.tap(t, "Sessions")
	x.typeInto(t, "Filter sessions", "capacitor")
	x.hasAll(t, "Search all sessions for “capacitor”")
	x.tap(t, "Search all sessions for “capacitor”")
	x.until(t, func() bool { s := x.a.st.Data.Search(); return !s.Loading && s.Status == state.ListLoaded })
	x.hasAll(t, "Search", "Snow: first boot hangs", "Port the capacitor tests")
	if !x.hasTabs() {
		t.Error("the bar of tabs is gone over the hits")
	}
	label := hitLabel(t, x, 9, oldReply)
	x.within(t, ui.RoleButton, label)
	x.snapshot(t, "phone-search")
	x.tap(t, label)
	x.until(t, func() bool { o := x.a.st.Data.Open(); return o != nil && o.ID == 9 && o.Loaded })
	x.settle()
	x.hasAll(t, "Snow: first boot hangs")
	x.tap(t, "Back to Search")
	if x.a.place != placeSearch || len(x.a.st.Data.Search().Results.Hits) != 2 {
		t.Fatalf("back from a hit shows place %d", x.a.place)
	}
	x.tap(t, "Back to Sessions")
	if x.a.place != placeSessions {
		t.Fatalf("back from the hits shows place %d", x.a.place)
	}
}
