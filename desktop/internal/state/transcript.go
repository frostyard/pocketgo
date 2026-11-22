package state

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/frostyard/pocketgo/proto"
)

// RowKind is what a transcript row shows.
type RowKind int

const (
	// RowUser is a message to the agent: Text, as Markdown. It is the
	// owner's, unless Sender names who else sent it (FromSupervisor). The
	// owner's shows the files it attached (Files) and the number of images
	// it gave the model (Images).
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
	// RowWorking says the run works on without words to show yet; Text,
	// when set, says what it does, as a compaction of the context.
	RowWorking
	// RowCompaction is a compaction of the session's context: Text is the
	// summary that stands for what came before it, which the view folds
	// (ADR-0026).
	RowCompaction
	// RowQuestion is the session's question that waits for the owner's
	// answer (ADR-0048): Question. It follows its tool call's row.
	RowQuestion
)

// ToolState is where a tool call of a row stands.
type ToolState int

const (
	// ToolQueued: the run has the call but has not started it.
	ToolQueued ToolState = iota
	// ToolRunning: the tool runs.
	ToolRunning
	// ToolWaiting: the call waits for the owner's approval, or, a
	// question's, for the owner's answer.
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
	// Queued marks a message that waits for a run (Live.Queued): taken,
	// and without an entry yet.
	Queued bool
	// Files are the files the owner's message attached, and Images how
	// many images it gave the model (ADR-0029).
	Files  []RowFile
	Images int
	// Sender, on a RowUser, names who sent the message when it is not the
	// owner's, as pocketd marks its entry or its queued submission: a
	// supervisor's (ADR-0032). nil for the owner's.
	Sender *proto.Sender
	// Question, on a RowQuestion, is the question that waits.
	Question *proto.Question
}

// FromSupervisor is the supervisor's session that sent a user row, and
// whether a supervisor sent it (ADR-0032): pocketd's mark alone says so,
// never the message's text.
func (r Row) FromSupervisor() (session int64, ok bool) {
	if r.Kind != RowUser || r.Sender == nil || r.Sender.Kind != proto.SenderSupervisor {
		return 0, false
	}
	return r.Sender.Session, true
}

// RowFile is a file the owner's message attached: its name, and its path
// on the service host.
type RowFile struct{ Name, Path string }

// userRow is the row of a message's text. The owner's message that
// attached files (files, as pocketd counts them) shows its words, then
// the files as pocketd named them (proto.SplitAttachments), whose lines
// leave the text. Any other shows its text whole: a message another sent
// (sender), as pocketd names files only in the owner's message, and one
// that attached none, whose words may quote a list (ADR-0029, ADR-0032;
// Codex review of #49, round 8).
func userRow(key, text string, images, files int, queued bool, sender *proto.Sender) Row {
	row := Row{Key: key, Kind: RowUser, Text: text, Images: images, Queued: queued, Sender: sender}
	if sender != nil || files == 0 {
		return row
	}
	words, named := proto.SplitAttachments(text)
	row.Text = words
	for _, f := range named {
		row.Files = append(row.Files, RowFile{Name: f.Name, Path: f.Path})
	}
	return row
}

// BuildRows lays a transcript out as rows: each entry's words, then each of
// its tool calls with its result, then the reply being written and the
// tools at work, then the messages that wait for a run (live.Queued), but
// for one whose entry the transcript has already: its live state comes
// after the entry. A message cut short ends with "…", and a note counts
// the queued messages the live state leaves out. approvals are the
// session's pending approvals.
//
// A model may use one call id twice, in two turns, so results, approvals
// and live states pair with call occurrences, not ids (pairCalls): a result
// belongs to the latest call of its id before it that has none, and a
// result without such a call shows alone. Only a call's outstanding
// occurrence, the last of its id without a result, waits for an approval
// or takes the live tool's state. The run's busy state queues only the
// calls of the current round, written after the last user message: a call
// an earlier run left without a result stays without one.
//
// question is the session's question that waits for the owner, nil for
// none (ADR-0048): its call's outstanding occurrence waits for the owner,
// and a RowQuestion follows its row, or ends the run's rows when no row
// shows its call. No working line shows while it waits.
func BuildRows(entries []proto.Entry, live proto.Live, approvals []proto.Approval, question *proto.Question) []Row {
	if question != nil && question.State != "" && question.State != proto.QuestionWaiting {
		question = nil
	}
	p := pairCalls(entries, live.Partial)
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
	asked := false // the question's row is placed
	questionRow := func() {
		if question != nil && !asked {
			asked = true
			q := *question
			b.add(Row{Key: "question:" + q.ID, Kind: RowQuestion, Question: &q, Live: true})
		}
	}
	tool := func(at occurrence, call proto.ToolCall, livePart bool) {
		row := &ToolRow{CallID: call.ID, Name: call.Name, Summary: ToolSummary(call.Name, call.Arguments), Arguments: IndentArguments(call.Arguments)}
		res := p.results[at]
		lt, inLive := liveTools[call.ID]
		outstanding := p.outstanding[call.ID] == at
		ask := question != nil && call.ID == question.CallID && outstanding && res == nil
		switch {
		case res != nil:
			row.State, row.Result = ToolDone, res.Text
			if res.IsError {
				row.State = ToolFailed
			}
		case !outstanding:
			row.State = ToolNoResult
		case ask:
			row.State = ToolWaiting
		case waiting[call.ID] != "":
			row.State, row.ApprovalID = ToolWaiting, waiting[call.ID]
		case inLive && lt.Status == proto.ToolRunning:
			row.State = ToolRunning
		case inLive && lt.Status == proto.ToolDone:
			row.State = ToolDone
		case inLive, live.Busy && at.entry >= p.round:
			row.State = ToolQueued
		default:
			row.State = ToolNoResult
		}
		b.add(Row{Key: "c" + call.ID, Kind: RowTool, Tool: row, Live: livePart})
		if ask {
			questionRow()
		}
	}
	for i, e := range entries {
		key := fmt.Sprintf("e%d", e.ID)
		switch e.Kind {
		case proto.EntryUser:
			b.add(userRow(key, e.Text, e.Images, e.Files, false, e.Sender))
		case proto.EntryAssistant:
			if strings.TrimSpace(e.Text) != "" || strings.TrimSpace(e.Thinking) != "" {
				b.add(Row{Key: key, Kind: RowAssistant, Text: e.Text, Thinking: e.Thinking})
			}
			for j, call := range e.ToolCalls {
				tool(occurrence{entry: i, call: j}, call, false)
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
			if r == nil || p.matched[i] {
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
		case proto.EntryCompaction:
			b.add(Row{Key: key, Kind: RowCompaction, Text: e.Text})
		case proto.EntryReset:
			b.add(Row{Key: key, Kind: RowNote, Text: StartedOverNote})
		default:
			name := e.Raw
			if name == "" {
				name = string(e.Kind)
			}
			b.add(Row{Key: key, Kind: RowNote, Text: "An entry this client does not show (" + name + ")"})
		}
	}
	if pe := live.Partial; pe != nil {
		if strings.TrimSpace(pe.Text) != "" || strings.TrimSpace(pe.Thinking) != "" {
			b.add(Row{Key: "live", Kind: RowAssistant, Text: pe.Text, Thinking: pe.Thinking, Live: true})
		}
		for j, call := range pe.ToolCalls {
			tool(occurrence{entry: len(entries), call: j}, call, true)
		}
	}
	// Tools at work that no outstanding call shows: their call is not
	// written yet.
	for j, t := range live.Tools {
		if _, shown := p.outstanding[t.CallID]; shown || t.Status == proto.ToolDone {
			continue
		}
		at := occurrence{entry: len(entries) + 1, call: j}
		p.outstanding[t.CallID] = at
		tool(at, proto.ToolCall{ID: t.CallID, Name: t.Name}, true)
	}
	// A question whose call no row shows, as one the transcript has not
	// loaded yet, ends the run's rows.
	questionRow()
	if live.Busy && len(waiting) == 0 && question == nil && !streamingText(live) {
		b.add(Row{Key: "working", Kind: RowWorking, Live: true})
	}
	if live.Compacting {
		b.add(Row{Key: "compacting", Kind: RowWorking, Text: CompactingNote, Live: true})
	}
	placed := map[int64]bool{}
	for _, e := range entries {
		if e.SubmissionID != 0 {
			placed[e.SubmissionID] = true
		}
	}
	for _, q := range live.Queued {
		if placed[q.SubmissionID] {
			continue
		}
		if q.Cut {
			// A preview cut short may end within its files' lines: its
			// text shows whole, so no list looks complete that is not
			// (Codex review of #49, round 7).
			b.add(Row{Key: fmt.Sprintf("q%d", q.SubmissionID), Kind: RowUser, Text: q.Text + " …", Queued: true, Sender: q.Sender})
			continue
		}
		b.add(userRow(fmt.Sprintf("q%d", q.SubmissionID), q.Text, 0, q.Files, true, q.Sender))
	}
	if live.QueuedMore > 0 {
		b.add(Row{Key: "queued-more", Kind: RowNote, Text: fmt.Sprintf("%d more queued", live.QueuedMore)})
	}
	return b.rows
}

// The notes a transcript shows for a standing session's context
// (ADR-0026).
const (
	// StartedOverNote marks a reset: the agent sees nothing before it.
	StartedOverNote = "Started over: the agent's context begins here"
	// CompactingNote says a compaction of the context runs.
	CompactingNote = "Compacting the context…"
)

// occurrence is where a tool call is: its entry's index (len(entries) for
// the reply being written) and its index among the entry's calls.
type occurrence struct{ entry, call int }

// pairing is what pairCalls found.
type pairing struct {
	// results are the results of call occurrences.
	results map[occurrence]*proto.ToolResult
	// matched are the indexes of result entries paired with a call.
	matched map[int]bool
	// outstanding is, for each call id, its last occurrence, when that one
	// has no result.
	outstanding map[string]occurrence
	// round is the index of the first entry of the current round: the
	// one after the last user message.
	round int
}

// pairCalls pairs each result entry with the latest call of its id before
// it that has no result yet.
func pairCalls(entries []proto.Entry, partial *proto.Entry) pairing {
	p := pairing{results: map[occurrence]*proto.ToolResult{}, matched: map[int]bool{}, outstanding: map[string]occurrence{}}
	open := map[string][]occurrence{} // calls without a result, by id, in order
	last := map[string]occurrence{}   // the last call of each id
	for i := range entries {
		e := &entries[i]
		if e.Kind == proto.EntryUser {
			p.round = i + 1
		}
		for j, call := range e.ToolCalls {
			at := occurrence{entry: i, call: j}
			open[call.ID] = append(open[call.ID], at)
			last[call.ID] = at
		}
		if r := e.ToolResult; r != nil {
			if calls := open[r.CallID]; len(calls) > 0 {
				p.results[calls[len(calls)-1]] = r
				p.matched[i] = true
				open[r.CallID] = calls[:len(calls)-1]
			}
		}
	}
	if partial != nil {
		for j, call := range partial.ToolCalls {
			last[call.ID] = occurrence{entry: len(entries), call: j}
		}
	}
	for id, at := range last {
		if p.results[at] == nil {
			p.outstanding[id] = at
		}
	}
	return p
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

// The most an approval card's preview of a call's arguments shows.
const (
	PreviewLines = 12
	PreviewBytes = 2000
)

// ArgumentsPreview is the start of full, a call's arguments as
// IndentArguments gives them: at most PreviewLines lines and PreviewBytes
// bytes, cut at a character, and whether it left anything out. A card that
// shows a cut preview must also show full, every byte of it, before the
// owner allows the call.
func ArgumentsPreview(full string) (preview string, cut bool) {
	preview = full
	if i := nthIndex(preview, '\n', PreviewLines); i >= 0 {
		preview = preview[:i]
	}
	if len(preview) > PreviewBytes {
		end := PreviewBytes
		for end > 0 && !utf8.RuneStart(preview[end]) {
			end--
		}
		preview = preview[:end]
	}
	return preview, len(preview) < len(full)
}

// nthIndex is the index of the nth b in s, or -1.
func nthIndex(s string, b byte, n int) int {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			if n--; n == 0 {
				return i
			}
		}
	}
	return -1
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
