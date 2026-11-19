package state

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/frostyard/pocketgo/proto"
)

func kinds(rows []Row) []string {
	names := map[RowKind]string{RowUser: "user", RowAssistant: "assistant", RowTool: "tool", RowError: "error", RowNote: "note", RowWorking: "working"}
	var out []string
	for _, r := range rows {
		out = append(out, names[r.Kind]+":"+r.Key)
	}
	return out
}

func call(id, name, args string) proto.ToolCall {
	return proto.ToolCall{ID: id, Name: name, Arguments: json.RawMessage(args)}
}

func TestBuildRowsShowsAResultWithItsCall(t *testing.T) {
	entries := []proto.Entry{
		{ID: 1, Kind: proto.EntryUser, Text: "Run the tests"},
		{ID: 2, Kind: proto.EntryAssistant, Text: "Running them.", Thinking: "The owner wants tests.",
			ToolCalls: []proto.ToolCall{call("c1", "bash", `{"command":"go test ./..."}`), call("c2", "read", `{"path":"go.mod"}`)}},
		{ID: 3, Kind: proto.EntryToolResult, ToolResult: &proto.ToolResult{CallID: "c1", Name: "bash", Text: "ok"}},
		{ID: 4, Kind: proto.EntryToolResult, ToolResult: &proto.ToolResult{CallID: "c2", Name: "read", IsError: true, Text: "no such file"}},
		{ID: 5, Kind: proto.EntryAssistant, Text: "All green."},
	}
	rows := BuildRows(entries, proto.Live{}, nil)
	want := "user:e1 assistant:e2 tool:cc1 tool:cc2 assistant:e5"
	if got := strings.Join(kinds(rows), " "); got != want {
		t.Fatalf("rows %s, want %s", got, want)
	}
	if rows[1].Thinking != "The owner wants tests." {
		t.Errorf("thinking %q", rows[1].Thinking)
	}
	bash, read := rows[2].Tool, rows[3].Tool
	if bash.State != ToolDone || bash.Result != "ok" || bash.Summary != "go test ./..." || !strings.Contains(bash.Arguments, "\n  \"command\"") {
		t.Errorf("bash row %+v", bash)
	}
	if read.State != ToolFailed || read.Result != "no such file" {
		t.Errorf("read row %+v", read)
	}
}

func TestBuildRowsShowsTheRunAtWork(t *testing.T) {
	entries := []proto.Entry{
		{ID: 1, Kind: proto.EntryUser, Text: "Write hello.txt"},
		{ID: 2, Kind: proto.EntryAssistant, ToolCalls: []proto.ToolCall{call("w1", "write", `{"path":"hello.txt","content":"hi"}`), call("r1", "read", `{"path":"a"}`)}},
	}
	live := proto.Live{SessionID: 1, Busy: true,
		Partial: &proto.Entry{Kind: proto.EntryAssistant, Text: "Meanwhile", ToolCalls: []proto.ToolCall{call("b1", "bash", `{"command":"ls"}`)}},
		Tools: []proto.LiveTool{{CallID: "r1", Name: "read", Status: proto.ToolRunning}, {CallID: "b1", Name: "bash", Status: proto.ToolPending},
			{CallID: "g1", Name: "grep", Status: proto.ToolRunning}, {CallID: "d1", Name: "find", Status: proto.ToolDone}}}
	approvals := []proto.Approval{approval("a1", 1, "w1", 0)}
	rows := BuildRows(entries, live, approvals)
	want := "user:e1 tool:cw1 tool:cr1 assistant:live tool:cb1 tool:cg1"
	if got := strings.Join(kinds(rows), " "); got != want {
		t.Fatalf("rows %s, want %s", got, want)
	}
	if w := rows[1].Tool; w.State != ToolWaiting || w.ApprovalID != "a1" {
		t.Errorf("the call waiting for approval shows %+v", w)
	}
	if rows[2].Tool.State != ToolRunning {
		t.Errorf("the running read shows %s", rows[2].Tool.State)
	}
	if !rows[3].Live || rows[3].Text != "Meanwhile" {
		t.Errorf("the reply being written shows %+v", rows[3])
	}
	if rows[4].Tool.State != ToolQueued || !rows[4].Live {
		t.Errorf("the pending call of the partial shows %+v", rows[4].Tool)
	}
	if rows[5].Tool.Name != "grep" || rows[5].Tool.State != ToolRunning {
		t.Errorf("a tool at work without a written call shows %+v", rows[5].Tool)
	}
}

func TestBuildRowsSaysTheRunWorksWithoutWords(t *testing.T) {
	entries := []proto.Entry{{ID: 1, Kind: proto.EntryUser, Text: "hi"}}
	rows := BuildRows(entries, proto.Live{Busy: true}, nil)
	if got := strings.Join(kinds(rows), " "); got != "user:e1 working:working" {
		t.Errorf("rows %s, want the working row last", got)
	}
	thinking := proto.Live{Busy: true, Partial: &proto.Entry{Thinking: "hmm"}}
	if got := strings.Join(kinds(BuildRows(entries, thinking, nil)), " "); got != "user:e1 assistant:live working:working" {
		t.Errorf("while it thinks: %s", got)
	}
	words := proto.Live{Busy: true, Partial: &proto.Entry{Text: "Hello"}}
	if got := strings.Join(kinds(BuildRows(entries, words, nil)), " "); got != "user:e1 assistant:live" {
		t.Errorf("while words stream in: %s", got)
	}
	waiting := BuildRows(entries, proto.Live{Busy: true}, []proto.Approval{approval("a1", 1, "c1", 0)})
	if got := strings.Join(kinds(waiting), " "); got != "user:e1" {
		t.Errorf("while it waits for the owner: %s", got)
	}
}

func TestBuildRowsAfterAnAbort(t *testing.T) {
	entries := []proto.Entry{
		{ID: 1, Kind: proto.EntryAssistant, StopReason: "aborted", ToolCalls: []proto.ToolCall{call("c1", "bash", `{"command":"sleep 100"}`)}},
		{ID: 2, Kind: proto.EntryAssistant, Error: "overloaded"},
		{ID: 3, Kind: proto.EntryAssistant, StopReason: "error"},
	}
	rows := BuildRows(entries, proto.Live{}, nil)
	if got := strings.Join(kinds(rows), " "); got != "tool:cc1 note:e1x error:e2x error:e3x" {
		t.Fatalf("rows %s", got)
	}
	if rows[0].Tool.State != ToolNoResult || rows[1].Text != "Stopped" || rows[2].Text != "overloaded" {
		t.Errorf("rows %+v", rows)
	}
}

func TestBuildRowsShowsOtherEntries(t *testing.T) {
	entries := []proto.Entry{
		{ID: 1, Kind: proto.EntryToolResult, ToolResult: &proto.ToolResult{CallID: "lost", Name: "bash", Text: "out"}},
		{ID: 2, Kind: proto.EntrySystem, Text: "Context compacted"},
		{ID: 3, Kind: proto.EntryOther, Raw: "branch_summary"},
		{ID: 4, Kind: "brand-new"},
		{ID: 5, Kind: proto.EntrySystem},
		{ID: 6, Kind: proto.EntryAssistant},
	}
	rows := BuildRows(entries, proto.Live{}, nil)
	if got := strings.Join(kinds(rows), " "); got != "tool:clost note:e2 note:e3 note:e4" {
		t.Fatalf("rows %s", got)
	}
	if rows[0].Tool.Result != "out" || rows[0].Tool.State != ToolDone {
		t.Errorf("a result without its call shows %+v", rows[0].Tool)
	}
	if !strings.Contains(rows[2].Text, "branch_summary") || !strings.Contains(rows[3].Text, "brand-new") {
		t.Errorf("unknown entries show %q and %q", rows[2].Text, rows[3].Text)
	}
}

func TestBuildRowsGivesRepeatedCallIDsTheirOwnKeys(t *testing.T) {
	entries := []proto.Entry{
		{ID: 1, Kind: proto.EntryAssistant, ToolCalls: []proto.ToolCall{call("x", "bash", `{}`)}},
		{ID: 2, Kind: proto.EntryAssistant, ToolCalls: []proto.ToolCall{call("x", "bash", `{}`)}},
	}
	rows := BuildRows(entries, proto.Live{}, nil)
	if len(rows) != 2 || rows[0].Key == rows[1].Key {
		t.Errorf("keys %v", kinds(rows))
	}
}

// TestAReusedCallIDPairsResultsWithTheirOwnCalls has a model use one call id
// in three turns: each result goes with its own call, and only the last,
// outstanding call waits for the approval of that id.
func TestAReusedCallIDPairsResultsWithTheirOwnCalls(t *testing.T) {
	entries := []proto.Entry{
		{ID: 1, Kind: proto.EntryAssistant, ToolCalls: []proto.ToolCall{call("x", "bash", `{"command":"first"}`)}},
		{ID: 2, Kind: proto.EntryToolResult, ToolResult: &proto.ToolResult{CallID: "x", Name: "bash", Text: "one"}},
		{ID: 3, Kind: proto.EntryUser, Text: "again"},
		{ID: 4, Kind: proto.EntryAssistant, ToolCalls: []proto.ToolCall{call("x", "bash", `{"command":"second"}`)}},
		{ID: 5, Kind: proto.EntryToolResult, ToolResult: &proto.ToolResult{CallID: "x", Name: "bash", IsError: true, Text: "two"}},
		{ID: 6, Kind: proto.EntryUser, Text: "and again"},
		{ID: 7, Kind: proto.EntryAssistant, ToolCalls: []proto.ToolCall{call("x", "bash", `{"command":"third"}`)}},
	}
	live := proto.Live{Busy: true, Tools: []proto.LiveTool{{CallID: "x", Name: "bash", Status: proto.ToolPending}}}
	rows := BuildRows(entries, live, []proto.Approval{approval("a1", 1, "x", 0)})
	var tools []*ToolRow
	for _, r := range rows {
		if r.Kind == RowTool {
			tools = append(tools, r.Tool)
		}
	}
	if len(tools) != 3 {
		t.Fatalf("rows %v, want three calls and no result alone", kinds(rows))
	}
	for i, want := range []struct {
		summary, result string
		state           ToolState
	}{{"first", "one", ToolDone}, {"second", "two", ToolFailed}, {"third", "", ToolWaiting}} {
		if got := tools[i]; got.Summary != want.summary || got.Result != want.result || got.State != want.state {
			t.Errorf("call %d: %q %q %s; want %q %q %s", i, got.Summary, got.Result, got.State, want.summary, want.result, want.state)
		}
	}
	// An aborted call and a later one of the same id: the result is the
	// later one's.
	aborted := []proto.Entry{
		{ID: 1, Kind: proto.EntryAssistant, StopReason: "aborted", ToolCalls: []proto.ToolCall{call("y", "bash", `{"command":"sleep 100"}`)}},
		{ID: 2, Kind: proto.EntryAssistant, ToolCalls: []proto.ToolCall{call("y", "bash", `{"command":"ls"}`)}},
		{ID: 3, Kind: proto.EntryToolResult, ToolResult: &proto.ToolResult{CallID: "y", Name: "bash", Text: "go.mod"}},
	}
	rows = BuildRows(aborted, proto.Live{}, nil)
	if rows[0].Tool.State != ToolNoResult || rows[2].Tool.Result != "go.mod" {
		t.Errorf("rows %+v %+v", rows[0].Tool, rows[2].Tool)
	}
}

// TestALaterRunDoesNotQueueAnEarlierRunsCall leaves a call without a result
// after an abort, then starts another run: the old call stays without a
// result, and only the current round's calls are queued.
func TestALaterRunDoesNotQueueAnEarlierRunsCall(t *testing.T) {
	entries := []proto.Entry{
		{ID: 1, Kind: proto.EntryUser, Text: "build it"},
		{ID: 2, Kind: proto.EntryAssistant, StopReason: "aborted", ToolCalls: []proto.ToolCall{call("old", "bash", `{"command":"make"}`)}},
		{ID: 3, Kind: proto.EntryUser, Text: "test it instead"},
		{ID: 4, Kind: proto.EntryAssistant, ToolCalls: []proto.ToolCall{call("new", "bash", `{"command":"make test"}`)}},
	}
	rows := BuildRows(entries, proto.Live{Busy: true}, nil)
	states := map[string]ToolState{}
	for _, r := range rows {
		if r.Kind == RowTool {
			states[r.Tool.CallID] = r.Tool.State
		}
	}
	if states["old"] != ToolNoResult {
		t.Errorf("the earlier run's call shows %s while another run works, want no result", states["old"])
	}
	if states["new"] != ToolQueued {
		t.Errorf("the current round's call shows %s, want queued", states["new"])
	}
}

func TestToolStatesSayWhereTheCallStands(t *testing.T) {
	want := map[ToolState]string{ToolQueued: "queued", ToolRunning: "running", ToolWaiting: "waiting for you",
		ToolDone: "done", ToolFailed: "failed", ToolNoResult: "no result"}
	for s, w := range want {
		if s.String() != w {
			t.Errorf("%d says %q, want %q", s, s.String(), w)
		}
	}
}

func TestToolSummary(t *testing.T) {
	for _, tc := range []struct{ args, want string }{
		{`{"command":"go test ./...","timeout":60}`, "go test ./..."},
		{`{"content":"x","path":"hello.txt"}`, "hello.txt"},
		{`{"zeta":"z","alpha":"a"}`, "a"},
		{`{"command":"make\nmake test"}`, "make …"},
		{`{"n":3}`, ""},
		{`not json`, ""},
		{`{"command":"` + strings.Repeat("y", 200) + `"}`, strings.Repeat("y", 159) + "…"},
	} {
		if got := ToolSummary("tool", json.RawMessage(tc.args)); got != tc.want {
			t.Errorf("ToolSummary(%s) = %q, want %q", tc.args, got, tc.want)
		}
	}
}
