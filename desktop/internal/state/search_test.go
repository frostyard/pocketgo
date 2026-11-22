package state

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/frostyard/pocketgo/desktop/internal/pocketdtest"
	"github.com/frostyard/pocketgo/proto"
)

// The filter offers its search as its last row only while it holds a word
// pocketd would search, and quotes a long filter cut (ADR-0050).
func TestSearchOfferNeedsAWord(t *testing.T) {
	for _, tc := range []struct {
		filter, want string
		ok           bool
	}{
		{"", "", false},
		{"   ", "", false},
		{" -- ** ", "", false},
		{"a b c d e f g h i", "", false},
		{" capacitor ", "Search all sessions for “capacitor”", true},
		{"arm64 runner", "Search all sessions for “arm64 runner”", true},
		{strings.Repeat("ab ", 20), "Search all sessions for “" + strings.Repeat("ab ", 13) + "a…”", true},
		{strings.Repeat("x", 45), "Search all sessions for “" + strings.Repeat("x", 40) + "…”", true},
	} {
		got, ok := SearchOffer(tc.filter)
		if ok != tc.ok || (ok && got != tc.want) {
			t.Errorf("SearchOffer(%q) = %q, %v; want %q, %v", tc.filter, got, ok, tc.want, tc.ok)
		}
	}
	d := &Data{}
	d.SetSessions(d.Begin(), []proto.Session{session(1, "Fix the capacitor", -time.Minute), session(2, "Other", -time.Hour)})
	sb := d.Sidebar(SidebarOptions{Filter: "capacitor", Now: pocketdtest.Now})
	if items := sb.Sections[0].Items; len(items) != 2 || items[0] != SessionKey(1) || items[1] != SearchKey() {
		t.Errorf("the filter lists %+v, want session 1, then the search", items)
	}
	if Shows(SearchKey()) || !Searches(SearchKey()) || Searches(SessionKey(1)) || Searches(PlaceKey(PlaceInbox)) {
		t.Error("the search row searches as the keys pass over it, or another row searches")
	}
	if _, ok := PinOf(SearchKey()); ok {
		t.Error("the search row pins")
	}
	sb = d.Sidebar(SidebarOptions{Filter: "--", Now: pocketdtest.Now})
	if slices.Contains(sb.Sections[0].Items, SearchKey()) {
		t.Error("a filter with no word offers a search")
	}
}

// The hits group by session, in the order of each session's newest hit;
// a session the client does not hold is named by its id.
func TestSearchGroupsHitsBySession(t *testing.T) {
	d := &Data{}
	s1 := session(1, "Fix the flaky CI", -time.Minute)
	s1.Profile = "vera"
	d.SetSessions(d.Begin(), []proto.Session{s1, session(2, "", -time.Hour)})
	d.SetProfiles(d.Begin(), []proto.Profile{{ID: "vera", Name: "Vera"}})
	f := d.BeginSearch("flaky")
	d.SetSearch(f, "flaky", proto.SearchResults{Words: []string{"flaky"}, Hits: []proto.SearchHit{
		{SessionID: 2, EntryID: 9, Field: proto.SearchUser},
		{SessionID: 1, EntryID: 0, Field: proto.SearchTitle},
		{SessionID: 2, EntryID: 3, Field: proto.SearchAssistant},
		{SessionID: 7, EntryID: 1, Field: proto.SearchUser},
		{SessionID: 1, EntryID: 2, Field: proto.SearchUser},
	}})
	var got []string
	for _, g := range d.SearchGroups() {
		var ids []string
		for _, h := range g.Hits {
			ids = append(ids, fmt.Sprint(h.EntryID))
		}
		got = append(got, g.Title+"|"+g.Tag+"|"+strings.Join(ids, ","))
	}
	want := []string{UntitledSession + "|demo|9,3", "Fix the flaky CI|Vera|0,2", "Session 7||1"}
	if !slices.Equal(got, want) {
		t.Errorf("groups %q, want %q", got, want)
	}
	if got := SearchNote(d.Search().Results, 3); got != "5 hits in 3 sessions" {
		t.Errorf("note %q", got)
	}
	r := d.Search().Results
	r.Cut, r.Indexing = true, true
	if got := SearchNote(r, 3); got != "5 hits in 3 sessions, the newest: more words find older ones" {
		t.Errorf("cut note %q", got)
	}
	if IndexingNote(r) == "" || IndexingNote(proto.SearchResults{}) != "" {
		t.Error("the indexing note")
	}
	if HitField(proto.SearchTitle) != "Title" || HitField(proto.SearchUser) != "Message" || HitField(proto.SearchAssistant) != "Reply" {
		t.Error("the fields' names")
	}
}

// A search's answer never undoes a newer one's, and the view says the
// newest is loading until it answers; a failed search shows no hits of
// another query.
func TestALateSearchNeverUndoesANewerOne(t *testing.T) {
	d := &Data{}
	older := d.BeginSearch("arm")
	newer := d.BeginSearch("arm64")
	if s := d.Search(); !s.Loading || s.Query != "arm64" || s.Status != ListLoading {
		t.Fatalf("before any answer: %+v", s)
	}
	d.SetSearch(newer, "arm64", proto.SearchResults{Hits: []proto.SearchHit{{SessionID: 1, EntryID: 2}}})
	d.SetSearch(older, "arm", proto.SearchResults{Hits: []proto.SearchHit{{SessionID: 3}, {SessionID: 4}}})
	if s := d.Search(); s.Loading || s.Answered != "arm64" || len(s.Results.Hits) != 1 || s.Status != ListLoaded {
		t.Fatalf("after a late answer: %+v", s)
	}
	failed := d.BeginSearch("capacitor")
	if !d.Search().Loading {
		t.Error("a new search is not loading")
	}
	d.SetSearchError(failed, "capacitor", "pocketd is gone")
	if s := d.Search(); s.Loading || s.Status != ListFailed || s.Problem != "pocketd is gone" || len(s.Results.Hits) != 0 || s.Answered != "capacitor" {
		t.Fatalf("after a failure: %+v", s)
	}
}

// A query pocketd would refuse sends nothing; one it takes comes back as
// the fake's hits.
func TestStoreSearchChecksTheQueryFirst(t *testing.T) {
	h := newHarness(t)
	h.srv.AddSession(session(1, "Snow image fails to boot", -time.Hour),
		proto.Entry{ID: 1, SessionID: 1, Kind: proto.EntryAssistant, Text: "The flux capacitor drifted.", At: pocketdtest.Now.Add(-time.Hour)})
	h.connect(t)
	before := len(h.srv.RequestsTo("GET", proto.PathSearch))
	h.store.Search("  ?? ")
	if s := h.store.Data.Search(); s.Status != ListFailed || s.Problem != proto.ErrNoSearchWords.Error() {
		t.Fatalf("a query with no word: %+v", s)
	}
	h.settle(t)
	if n := len(h.srv.RequestsTo("GET", proto.PathSearch)); n != before {
		t.Errorf("%d searches sent for a query with no word", n-before)
	}
	h.store.Search("Capacitor")
	h.loop.RunUntil(t, func() bool { return !h.store.Data.Search().Loading })
	s := h.store.Data.Search()
	if s.Status != ListLoaded || len(s.Results.Hits) != 1 || s.Results.Hits[0].EntryID != 1 {
		t.Fatalf("search %+v", s)
	}
	spans := SnippetSpans(s.Results.Hits[0].Snippet, s.Results.Hits[0].Marks)
	if len(spans) < 2 || !spans[1].Marked || spans[1].Text != "capacitor" {
		t.Errorf("spans %+v", spans)
	}
}

// A snippet draws only the marks it can trust: in it whole, on
// characters' bounds, in order and apart.
func TestSnippetSpansDrawOnlyCheckedMarks(t *testing.T) {
	render := func(spans []Span) string {
		var b strings.Builder
		for _, s := range spans {
			if s.Marked {
				b.WriteString("[" + s.Text + "]")
			} else {
				b.WriteString(s.Text)
			}
		}
		return b.String()
	}
	snippet := "…the café runner" // "…" is 3 bytes, "é" 2
	for _, tc := range []struct {
		name  string
		marks []proto.SearchMark
		want  string
	}{
		{"none", nil, "…the café runner"},
		{"two", []proto.SearchMark{{Start: 3, End: 6}, {Start: 13, End: 19}}, "…[the] café [runner]"},
		{"multi-byte", []proto.SearchMark{{Start: 7, End: 12}}, "…the [café] runner"},
		{"mid-character", []proto.SearchMark{{Start: 1, End: 6}, {Start: 7, End: 11}}, "…the café runner"},
		{"overlapping", []proto.SearchMark{{Start: 3, End: 8}, {Start: 6, End: 12}}, "…[the c]afé runner"},
		{"backwards and empty", []proto.SearchMark{{Start: 6, End: 3}, {Start: 5, End: 5}}, "…the café runner"},
		{"out of range", []proto.SearchMark{{Start: 14, End: 99}, {Start: -1, End: 2}}, "…the café runner"},
		{"the whole", []proto.SearchMark{{Start: 0, End: len(snippet)}}, "[…the café runner]"},
	} {
		if got := render(SnippetSpans(snippet, tc.marks)); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
	if spans := SnippetSpans("", nil); len(spans) != 1 || spans[0].Text != "" {
		t.Errorf("an empty snippet: %+v", spans)
	}
}

// A hit opens its session at the first row its entry built, else at the
// next entry's row; a title's hit, and an entry past the transcript, open
// it where it opens.
func TestTargetRowIsTheEntrysFirstRow(t *testing.T) {
	entries := []proto.Entry{
		{ID: 1, Kind: proto.EntryUser, Text: "Why does it not boot?"},
		{ID: 2, Kind: proto.EntryAssistant, ToolCalls: []proto.ToolCall{{ID: "c1", Name: "bash", Arguments: json.RawMessage(`{"command":"ls"}`)}}},
		{ID: 3, Kind: proto.EntryToolResult, ToolResult: &proto.ToolResult{CallID: "c1", Name: "bash", Text: "boot"}},
		{ID: 4, Kind: proto.EntrySystem, Text: "  "},
		{ID: 5, Kind: proto.EntryAssistant, Text: "The capacitor drifted.", Thinking: "Hmm."},
	}
	rows := BuildRows(entries, proto.Live{SessionID: 1}, nil, nil)
	for _, tc := range []struct {
		entry int64
		key   string
		ok    bool
	}{
		{1, "e1", true},
		{2, "cc1", true}, // a reply of tool calls alone opens at its first call
		{3, "e5", true},  // a result shows with its call's row: the next row
		{4, "e5", true},  // a system entry of no words shows no row
		{5, "e5", true},
		{6, "", false},
		{0, "", false},
	} {
		row, ok := TargetRow(rows, tc.entry)
		if ok != tc.ok || (ok && rows[row].Key != tc.key) {
			key := ""
			if ok {
				key = rows[row].Key
			}
			t.Errorf("TargetRow(%d) = %d %q, %v; want %q, %v", tc.entry, row, key, ok, tc.key, tc.ok)
		}
	}
	// A queued message and the run at work stand for no entry.
	live := proto.Live{SessionID: 1, Busy: true, Queued: []proto.QueuedMessage{{SubmissionID: 9, Text: "next"}}}
	for _, r := range BuildRows(nil, live, nil, nil) {
		if r.Entry != 0 {
			t.Errorf("row %q stands for entry %d", r.Key, r.Entry)
		}
	}
}

// A seek waits for its session's transcript to load, then scrolls to its
// entry's row, or leaves the session where it opens.
func TestASeekWaitsForItsSessionToLoad(t *testing.T) {
	rows := BuildRows([]proto.Entry{{ID: 1, Kind: proto.EntryUser, Text: "one"}, {ID: 2, Kind: proto.EntryAssistant, Text: "two"}}, proto.Live{}, nil, nil)
	seek := Seek{Session: 4, Entry: 2}
	if _, _, done := seek.Target(&OpenSession{ID: 4}, rows); done {
		t.Error("a seek is done before its session loaded")
	}
	if row, found, done := seek.Target(&OpenSession{ID: 4, Loaded: true}, rows); !done || !found || row != 1 {
		t.Errorf("loaded: row %d found %v done %v", row, found, done)
	}
	if _, found, done := (Seek{Session: 4, Entry: 99}).Target(&OpenSession{ID: 4, Loaded: true}, rows); !done || found {
		t.Errorf("an entry past the transcript: found %v done %v", found, done)
	}
	if _, found, done := seek.Target(&OpenSession{ID: 5, Loaded: true}, rows); !done || found {
		t.Error("a seek of another session scrolls this one")
	}
	if _, found, done := seek.Target(nil, rows); !done || found {
		t.Error("a seek with no session open waits")
	}
}

// On a compact window, the Search view is the Sessions tab's, and a hit's
// session goes back to the hits.
func TestBackFromASearchHitGoesToTheHits(t *testing.T) {
	if tab, ok := TabOf(PlaceSearch); !ok || tab != TabSessions {
		t.Errorf("TabOf(search) = %v, %v", tab, ok)
	}
	if got := BackFrom(TabSessions, PlaceSearch); got != PlaceSearch {
		t.Errorf("BackFrom(sessions, search) = %q", got)
	}
	if got := BackTitle(TabSessions, PlaceSearch); got != "Search" {
		t.Errorf("BackTitle = %q", got)
	}
	for _, tab := range Tabs {
		for _, from := range []string{PlaceInbox, PlaceSessions, PlaceBoard, ""} {
			if got := BackFrom(tab, from); got != BackTo(tab) {
				t.Errorf("BackFrom(%v, %q) = %q, want %q", tab, from, got, BackTo(tab))
			}
			if got := BackTitle(tab, from); got != tab.Title() {
				t.Errorf("BackTitle(%v, %q) = %q", tab, from, got)
			}
		}
	}
	if got := BackFrom(TabInbox, PlaceSearch); got != PlaceInbox {
		t.Errorf("a search hit shown over the Inbox goes back to %q", got)
	}
}
