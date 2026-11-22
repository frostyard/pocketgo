package search

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/frostyard/pocketgo/proto"
)

var t0 = time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC)

// The index holds a user entry's text and an agent's reply with the names
// of its tool calls; never a tool's output or arguments, thinking, or a
// system, compaction or reset entry.
func TestDocOfHoldsWhatIsSearched(t *testing.T) {
	call := func(name string) proto.ToolCall {
		return proto.ToolCall{ID: name, Name: name, Arguments: json.RawMessage(`{"command":"secret-argument"}`)}
	}
	for _, tc := range []struct {
		name  string
		entry proto.Entry
		want  string
		field proto.SearchField
	}{
		{"the owner's message", proto.Entry{ID: 1, Kind: proto.EntryUser, Text: " Fix the arm64 job. "}, "Fix the arm64 job.", proto.SearchUser},
		{"a reply", proto.Entry{ID: 2, Kind: proto.EntryAssistant, Text: "The runner is old.", Thinking: "hidden thought"}, "The runner is old.", proto.SearchAssistant},
		{"a reply with tool calls", proto.Entry{ID: 3, Kind: proto.EntryAssistant, Text: "Looking.", ToolCalls: []proto.ToolCall{call("bash"), call("team_message")}},
			"Looking.\nTools: bash, team_message", proto.SearchAssistant},
		{"tool calls alone", proto.Entry{ID: 4, Kind: proto.EntryAssistant, ToolCalls: []proto.ToolCall{call("read")}}, "Tools: read", proto.SearchAssistant},
	} {
		d, ok := DocOf(tc.entry)
		if !ok || d.Text != tc.want || d.Field != tc.field || d.Entry != tc.entry.ID {
			t.Errorf("%s: DocOf = %+v, %v; want %q", tc.name, d, ok, tc.want)
		}
		if strings.Contains(d.Text, "secret-argument") || strings.Contains(d.Text, "hidden") {
			t.Errorf("%s: the index holds arguments or thinking: %q", tc.name, d.Text)
		}
	}
	for _, e := range []proto.Entry{
		{ID: 5, Kind: proto.EntryToolResult, ToolResult: &proto.ToolResult{CallID: "c", Name: "bash", Text: "tool output"}},
		{ID: 6, Kind: proto.EntrySystem, Text: "system"},
		{ID: 7, Kind: proto.EntryCompaction, Text: "summary"},
		{ID: 8, Kind: proto.EntryReset},
		{ID: 9, Kind: proto.EntryOther, Raw: "custom"},
		{ID: 10, Kind: proto.EntryUser, Text: "  \n "},
		{ID: 11, Kind: proto.EntryAssistant, Thinking: "only thinking"},
	} {
		if d, ok := DocOf(e); ok {
			t.Errorf("entry %d (%s) is searched: %+v", e.ID, e.Kind, d)
		}
	}
	long := strings.Repeat("é", MaxIndexed) // two bytes each
	d, ok := DocOf(proto.Entry{ID: 12, Kind: proto.EntryUser, Text: long})
	if !ok || len(d.Text) > MaxIndexed || !utf8.ValidString(d.Text) || len(d.Text) < MaxIndexed-1 {
		t.Errorf("a long text is kept as %d bytes, valid %v", len(d.Text), utf8.ValidString(d.Text))
	}
}

func TestMatchAndLimit(t *testing.T) {
	if got := Match([]string{"arm64", "job"}); got != `"arm64"* AND "job"*` {
		t.Errorf("Match = %s", got)
	}
	if got := Match([]string{`a"b`}); got != `"a""b"*` {
		t.Errorf("Match quotes a quote: %s", got)
	}
	for raw, want := range map[string]int{"": proto.DefaultSearchLimit, "1": 1, "200": 200, "37": 37} {
		if got, err := Limit(raw); err != nil || got != want {
			t.Errorf("Limit(%q) = %d, %v", raw, got, err)
		}
	}
	for _, raw := range []string{"0", "201", "-1", "x", "1.5", "+5", "05", " 5"} {
		if _, err := Limit(raw); err == nil {
			t.Errorf("Limit(%q) is taken", raw)
		}
	}
}

// A title holds the words when each begins one of its words, whatever its
// case, as the index matches an entry.
func TestHolds(t *testing.T) {
	words := func(q string) []string {
		w, err := proto.SearchWords(q)
		if err != nil {
			t.Fatal(err)
		}
		return w
	}
	for _, tc := range []struct {
		text, q string
		want    bool
	}{
		{"Fix the flaky CI in nsl", "flaky", true},
		{"Fix the flaky CI in nsl", "FLA ci", true},
		{"Fix the flaky CI in nsl", "laky", false},
		{"Fix the flaky CI in nsl", "flaky arm64", false},
		{"team_delegate went wrong", "delegate", true},
		{"Été à Paris", "été", true},
		{"ADR-0050 search", "adr-0050", true},
	} {
		if got := Holds(tc.text, words(tc.q)); got != tc.want {
			t.Errorf("Holds(%q, %q) = %v", tc.text, tc.q, got)
		}
	}
}

// marked is a snippet with its marks in brackets.
func marked(snippet string, marks []proto.SearchMark) string {
	var b strings.Builder
	at := 0
	for _, m := range marks {
		b.WriteString(snippet[at:m.Start] + "[" + snippet[m.Start:m.End] + "]")
		at = m.End
	}
	b.WriteString(snippet[at:])
	return b.String()
}

func TestSnippet(t *testing.T) {
	long := strings.Repeat("lorem ipsum ", 30) // 360 characters
	for _, tc := range []struct {
		name, text string
		words      []string
		want       string
	}{
		{"a short text, every match marked", "Deploy the deployment, then DEPLOY.", []string{"deploy"},
			"[Deploy] the [deploy]ment, then [DEPLOY]."},
		{"the longest word that begins a word", "deployment notes", []string{"dep", "deploy"}, "[deploy]ment notes"},
		{"spaces run together", "first line\n\n   second\tline", []string{"second"}, "first line [second] line"},
		{"a word inside another is no match", "redeploy", []string{"deploy"}, "redeploy"},
		{"cut before the match, at a word", long + "the arm64 runner timed out " + long, []string{"arm64"},
			"…lorem ipsum lorem ipsum lorem ipsum the [arm64] runner timed out lorem ipsum lorem ipsum lorem ipsum lorem ipsum lorem ipsum lorem ipsum lorem ipsum lorem ipsum…"},
		{"no match shows the start", long, []string{"absent"},
			"lorem ipsum lorem ipsum lorem ipsum lorem ipsum lorem ipsum lorem ipsum lorem ipsum lorem ipsum lorem ipsum lorem ipsum lorem ipsum lorem ipsum lorem ipsum…"},
		{"letters of many bytes", "Ünïcode café — CAFÉ au lait", []string{"café"}, "Ünïcode [café] — [CAFÉ] au lait"},
	} {
		s, marks := Snippet(tc.text, tc.words)
		if got := marked(s, marks); got != tc.want {
			t.Errorf("%s:\n got %q\nwant %q", tc.name, got, tc.want)
		}
		body := strings.TrimSuffix(strings.TrimPrefix(s, "…"), "…")
		if n := utf8.RuneCountInString(body); n > proto.SearchSnippet {
			t.Errorf("%s: %d characters", tc.name, n)
		}
		for i, m := range marks {
			if m.Start < 0 || m.End > len(s) || m.Start >= m.End || (i > 0 && marks[i-1].End > m.Start) ||
				!utf8.RuneStart(s[m.Start]) || (m.End < len(s) && !utf8.RuneStart(s[m.End])) {
				t.Errorf("%s: mark %d %+v is not a range of whole characters in order", tc.name, i, m)
			}
		}
	}
	// A word longer than the window still shows its match.
	huge := strings.Repeat("x", 300) + " needle"
	s, marks := Snippet(huge, []string{"needle"})
	if len(marks) != 1 || s[marks[0].Start:marks[0].End] != "needle" {
		t.Errorf("the match of a text with a huge word: %q %+v", s, marks)
	}
}

// An answer's hits are newest first, a title's before an entry's at one
// time, and at most the limit, which says when it left one out.
func TestArrange(t *testing.T) {
	titles := []proto.SearchHit{{SessionID: 1, Field: proto.SearchTitle, At: t0.Add(-time.Hour)}, {SessionID: 2, Field: proto.SearchTitle, At: t0}}
	entries := []proto.SearchHit{
		{SessionID: 2, EntryID: 30, Field: proto.SearchUser, At: t0},
		{SessionID: 3, EntryID: 20, Field: proto.SearchAssistant, At: t0.Add(-time.Minute)},
		{SessionID: 1, EntryID: 10, Field: proto.SearchAssistant, At: t0.Add(-2 * time.Hour)},
	}
	hits, cut := Arrange(titles, entries, 10)
	var got []int64
	for _, h := range hits {
		got = append(got, h.SessionID*100+h.EntryID)
	}
	if want := []int64{200, 230, 320, 100, 110}; !slices.Equal(got, want) || cut {
		t.Errorf("Arrange = %v, cut %v; want %v", got, cut, want)
	}
	hits, cut = Arrange(titles, entries, 3)
	if len(hits) != 3 || !cut || hits[2].EntryID != 20 {
		t.Errorf("Arrange to 3 = %+v, cut %v", hits, cut)
	}
	if hits, cut := Arrange(nil, nil, 5); len(hits) != 0 || cut {
		t.Errorf("Arrange of nothing = %+v, %v", hits, cut)
	}
}
