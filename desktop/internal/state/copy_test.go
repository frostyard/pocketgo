package state

import (
	"testing"
	"time"

	"github.com/frostyard/pocketgo/proto"
)

// Only a finished reply with words copies, its Markdown byte for byte and
// never its thinking (ADR-0067).
func TestCopyKeepsOnlyFinishedReplyMarkdown(t *testing.T) {
	const markdown = "## Plan\n\n- **one**\n- `two`\n"
	entries := []proto.Entry{
		{ID: 1, Kind: proto.EntryUser, Text: "owner"},
		{ID: 2, Kind: proto.EntryUser, Text: "supervisor", Sender: &proto.Sender{Kind: proto.SenderSupervisor, Session: 7}},
		{ID: 3, Kind: proto.EntryAssistant, Text: markdown, Thinking: "secret thought"},
		{ID: 4, Kind: proto.EntryAssistant, Text: "Reading now.", ToolCalls: []proto.ToolCall{call("read1", "read", `{}`)}},
		{ID: 5, Kind: proto.EntryToolResult, ToolResult: &proto.ToolResult{CallID: "read1", Text: "file contents"}},
		{ID: 6, Kind: proto.EntryAssistant, Thinking: "secret thought"},
		{ID: 7, Kind: proto.EntryAssistant, Text: "  \n\t"},
	}
	live := proto.Live{Busy: true, Partial: &proto.Entry{Kind: proto.EntryAssistant, Text: "Still writing"}}
	rows := BuildRows(entries, live, nil, nil)
	want := map[string]string{"e3": markdown, "e4": "Reading now."}
	seen := map[string]bool{}
	for _, row := range rows {
		text, ok := row.Copy()
		expected, eligible := want[row.Key]
		if ok != eligible || text != expected {
			t.Errorf("%s Copy() = %q, %v; want %q, %v", row.Key, text, ok, expected, eligible)
		}
		seen[row.Key] = true
	}
	for _, key := range []string{"e1", "e2", "e3", "e4", "cread1", "e6", "live"} {
		if !seen[key] {
			t.Errorf("missing fixture row %s", key)
		}
	}
}

// No row but an agent's reply has a Copy button (ADR-0067).
func TestCopyEligibilityByRowKind(t *testing.T) {
	for _, kind := range []RowKind{RowUser, RowAssistant, RowTool, RowError, RowNote, RowCompaction, RowQuestion, RowWorking} {
		r := Row{Kind: kind, Text: "words", Thinking: "secret thought"}
		text, ok := r.Copy()
		if want := kind == RowAssistant; ok != want || (want && text != r.Text) || (!want && text != "") {
			t.Errorf("kind %v Copy() = %q, %v", kind, text, ok)
		}
	}
	for _, r := range []Row{
		{Kind: RowUser, Text: "supervisor", Sender: &proto.Sender{Kind: proto.SenderSupervisor, Session: 7}},
		{Kind: RowAssistant, Text: "words", Live: true},
		{Kind: RowAssistant, Thinking: "secret thought"},
		{Kind: RowAssistant, Text: "  \n\t"},
	} {
		if text, ok := r.Copy(); ok || text != "" {
			t.Errorf("Copy(%+v) = %q, %v", r, text, ok)
		}
	}
}

// Copied shows on the row copied last, in its session, for CopyHold.
func TestCopiedShowsOnlyTheLastRowDuringTheHold(t *testing.T) {
	at := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	m := Copied{Session: 1, Key: "e5", At: at}
	if CopyHold != 1500*time.Millisecond {
		t.Fatalf("CopyHold = %v, want 1.5s", CopyHold)
	}
	for _, c := range []struct {
		name    string
		mark    Copied
		session int64
		key     string
		now     time.Time
		want    bool
	}{
		{"zero", Copied{}, 0, "", at, false},
		{"zero at zero time", Copied{}, 0, "", time.Time{}, false},
		{"start", m, 1, "e5", at, true},
		{"last nanosecond", m, 1, "e5", at.Add(CopyHold - time.Nanosecond), true},
		{"end", m, 1, "e5", at.Add(CopyHold), false},
		{"before", m, 1, "e5", at.Add(-time.Nanosecond), false},
		{"other key", m, 1, "e6", at, false},
		{"other session", m, 2, "e5", at, false},
	} {
		if got := c.mark.Shows(c.session, c.key, c.now); got != c.want {
			t.Errorf("%s: Shows = %v, want %v", c.name, got, c.want)
		}
	}
	for _, c := range []struct{ elapsed, want time.Duration }{
		{0, CopyHold}, {500 * time.Millisecond, time.Second},
		{CopyHold - time.Nanosecond, time.Nanosecond}, {CopyHold, 0},
		{CopyHold + time.Nanosecond, 0}, {-time.Nanosecond, 0},
	} {
		if got := m.Left(at.Add(c.elapsed)); got != c.want {
			t.Errorf("Left at %v = %v, want %v", c.elapsed, got, c.want)
		}
	}
	if got := (Copied{}).Left(at); got != 0 {
		t.Errorf("zero Left = %v", got)
	}
}
