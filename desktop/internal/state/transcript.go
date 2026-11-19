package state

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/frostyard/pocketgo/proto"
)

// RowKind is what a transcript row shows.
type RowKind int

const (
	// RowUser is the owner's message: Text, as Markdown.
	RowUser RowKind = iota
	// RowAssistant is the model's words: Text as Markdown, and Thinking,
	// which the view folds.
	RowAssistant
	// RowTool is one tool call with what came of it: Tool.
	RowTool
	// RowError is a reply that failed: Text.
	RowError
	// RowNote is a quiet line: a stopped run, a system entry, or an entry
	// this client does not show.
	RowNote
	// RowWorking says the run works on without words to show yet.
	RowWorking
)

// ToolState is where a tool call of a row stands.
type ToolState int

const (
	// ToolQueued: the run has the call but has not started it.
	ToolQueued ToolState = iota
	// ToolRunning: the tool runs.
	ToolRunning
	// ToolWaiting: the call waits for the owner's approval.
	ToolWaiting
	// ToolDone: the tool returned a result.
	ToolDone
	// ToolFailed: the tool returned an error, or the owner denied it.
	ToolFailed
	// ToolNoResult: the call has no result and no run works on it, as
	// after an abort.
	ToolNoResult
)

// String names the state as the row's status says it.
func (s ToolState) String() string {
	switch s {
	case ToolQueued:
		return "queued"
	case ToolRunning:
		return "running"
	case ToolWaiting:
		return "waiting for you"
	case ToolDone:
		return "done"
	case ToolFailed:
		return "failed"
	}
	return "no result"
}

// ToolRow is a tool call as a row shows it.
type ToolRow struct {
	CallID, Name string
	// Summary is the call in one line: what it works on, as a command or
	// a path. Arguments are its arguments as indented JSON.
	Summary, Arguments string
	State              ToolState
	// Result is the tool's text, when it returned.
	Result string
	// ApprovalID is the approval the call waits for (ToolWaiting).
	ApprovalID string
}

// Row is one row of a session's transcript.
type Row struct {
	// Key is the row's identity, stable as rows come and go: "e<id>" for
	// an entry's words, "c<call id>" for a tool call, "live" and
	// "working" for the run at work.
	Key      string
	Kind     RowKind
	Text     string
	Thinking string
	Tool     *ToolRow
	// Live marks the reply still being written: its text streams in.
	Live bool
}

// BuildRows lays a transcript out as rows: each entry's words, then each of
// its tool calls with its result, then the reply being written and the
// tools at work. A tool result shows with its call; a result whose call is
// not in the transcript shows alone. approvals are the session's pending
// approvals: a call that waits for one says so.
func BuildRows(entries []proto.Entry, live proto.Live, approvals []proto.Approval) []Row {
	results := map[string]*proto.ToolResult{}
	calls := map[string]bool{}
	for i := range entries {
		e := &entries[i]
		if e.ToolResult != nil && e.ToolResult.CallID != "" {
			results[e.ToolResult.CallID] = e.ToolResult
		}
		for _, call := range e.ToolCalls {
			calls[call.ID] = true
		}
	}
	waiting := map[string]string{}
	for _, a := range approvals {
		if a.State == "" || a.State == proto.ApprovalPending {
			waiting[a.CallID] = a.ID
		}
	}
	liveTools := map[string]proto.LiveTool{}
	for _, t := range live.Tools {
		liveTools[t.CallID] = t
	}
	b := rowBuilder{keys: map[string]bool{}}
	tool := func(call proto.ToolCall, livePart bool) {
		// The state: an approval it waits for, its result, the live
		// tool's status, then the run's.
		row := &ToolRow{CallID: call.ID, Name: call.Name, Summary: ToolSummary(call.Name, call.Arguments), Arguments: IndentArguments(call.Arguments)}
		lt, inLive := liveTools[call.ID]
		res := results[call.ID]
		switch {
		case waiting[call.ID] != "":
			row.State, row.ApprovalID = ToolWaiting, waiting[call.ID]
		case res != nil:
			row.State, row.Result = ToolDone, res.Text
			if res.IsError {
				row.State = ToolFailed
			}
		case inLive && lt.Status == proto.ToolRunning:
			row.State = ToolRunning
		case inLive && lt.Status == proto.ToolDone:
			row.State = ToolDone
		case inLive, live.Busy:
			row.State = ToolQueued
		default:
			row.State = ToolNoResult
		}
		b.add(Row{Key: "c" + call.ID, Kind: RowTool, Tool: row, Live: livePart})
	}
	for _, e := range entries {
		key := fmt.Sprintf("e%d", e.ID)
		switch e.Kind {
		case proto.EntryUser:
			b.add(Row{Key: key, Kind: RowUser, Text: e.Text})
		case proto.EntryAssistant:
			if strings.TrimSpace(e.Text) != "" || strings.TrimSpace(e.Thinking) != "" {
				b.add(Row{Key: key, Kind: RowAssistant, Text: e.Text, Thinking: e.Thinking})
			}
			for _, call := range e.ToolCalls {
				tool(call, false)
			}
			switch {
			case e.Error != "":
				b.add(Row{Key: key + "x", Kind: RowError, Text: e.Error})
			case e.StopReason == "error":
				b.add(Row{Key: key + "x", Kind: RowError, Text: "The model stopped with an error."})
			case e.StopReason == "aborted":
				b.add(Row{Key: key + "x", Kind: RowNote, Text: "Stopped"})
			}
		case proto.EntryToolResult:
			r := e.ToolResult
			if r == nil || calls[r.CallID] {
				continue
			}
			state := ToolDone
			if r.IsError {
				state = ToolFailed
			}
			b.add(Row{Key: "c" + r.CallID, Kind: RowTool, Tool: &ToolRow{CallID: r.CallID, Name: r.Name, State: state, Result: r.Text}})
		case proto.EntrySystem:
			if strings.TrimSpace(e.Text) != "" {
				b.add(Row{Key: key, Kind: RowNote, Text: e.Text})
			}
		default:
			name := e.Raw
			if name == "" {
				name = string(e.Kind)
			}
			b.add(Row{Key: key, Kind: RowNote, Text: "An entry this client does not show (" + name + ")"})
		}
	}
	if p := live.Partial; p != nil {
		if strings.TrimSpace(p.Text) != "" || strings.TrimSpace(p.Thinking) != "" {
			b.add(Row{Key: "live", Kind: RowAssistant, Text: p.Text, Thinking: p.Thinking, Live: true})
		}
		for _, call := range p.ToolCalls {
			calls[call.ID] = true
			tool(call, true)
		}
	}
	// Tools at work that no call shows: their call is not written yet.
	for _, t := range live.Tools {
		if calls[t.CallID] || t.Status == proto.ToolDone {
			continue
		}
		calls[t.CallID] = true
		tool(proto.ToolCall{ID: t.CallID, Name: t.Name}, true)
	}
	if live.Busy && len(waiting) == 0 && !streamingText(live) {
		b.add(Row{Key: "working", Kind: RowWorking, Live: true})
	}
	return b.rows
}

// streamingText reports whether the reply being written shows words, which
// tell that the run works without a row of their own.
func streamingText(live proto.Live) bool {
	return live.Partial != nil && strings.TrimSpace(live.Partial.Text) != ""
}

type rowBuilder struct {
	rows []Row
	keys map[string]bool
}

// add appends r, giving it a key no other row has: a model can repeat a
// call id, and a list must not.
func (b *rowBuilder) add(r Row) {
	key := r.Key
	for n := 2; b.keys[key]; n++ {
		key = fmt.Sprintf("%s#%d", r.Key, n)
	}
	b.keys[key] = true
	r.Key = key
	b.rows = append(b.rows, r)
}

// summaryKeys are the arguments that say what a call works on, in the
// order a summary looks for them.
var summaryKeys = []string{"command", "path", "file_path", "pattern", "query", "url", "name", "description"}

// ToolSummary says a call in one line: the first argument of summaryKeys
// it has, else its first string argument by name, else "". The line is at
// most 160 characters, and only the first line of a longer value.
func ToolSummary(name string, arguments json.RawMessage) string {
	var args map[string]any
	if json.Unmarshal(arguments, &args) != nil || len(args) == 0 {
		return ""
	}
	pick := func(k string) string {
		v, _ := args[k].(string)
		return strings.TrimSpace(v)
	}
	value := ""
	for _, k := range summaryKeys {
		if value = pick(k); value != "" {
			break
		}
	}
	if value == "" {
		keys := make([]string, 0, len(args))
		for k := range args {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if value = pick(k); value != "" {
				break
			}
		}
	}
	line, _, more := strings.Cut(value, "\n")
	return Truncate(line, 160, more)
}

// Truncate shortens s to at most n characters, ending it with "…" when it
// cut something or when more says s goes on.
func Truncate(s string, n int, more bool) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n-1]) + "…"
	}
	if more {
		return s + " …"
	}
	return s
}

// IndentArguments is a tool call's arguments as indented JSON, or as they came
// when they do not parse.
func IndentArguments(arguments json.RawMessage) string {
	if len(arguments) == 0 || string(arguments) == "null" {
		return ""
	}
	var out bytes.Buffer
	if json.Indent(&out, arguments, "", "  ") != nil {
		return string(arguments)
	}
	return out.String()
}
