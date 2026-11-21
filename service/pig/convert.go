package pig

import (
	"bytes"
	"encoding/json"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/MichaelKinsy/PiG/durable"
	"github.com/MichaelKinsy/PiG/durable/harness"
	"github.com/frostyard/pocketgo/proto"
)

// The conversions read Pi's JSON wire form of messages and live state, not
// PiG's Go structs: the JSON is Pi's stable interchange format, and reading it
// keeps PiG's Go API churn out of this file.

type wireMessage struct {
	Role         string          `json:"role"`
	Content      json.RawMessage `json:"content"`
	Provider     string          `json:"provider"`
	Model        string          `json:"model"`
	Usage        *wireUsage      `json:"usage"`
	StopReason   string          `json:"stopReason"`
	ErrorMessage string          `json:"errorMessage"`
	Timestamp    int64           `json:"timestamp"`
	ToolCallID   string          `json:"toolCallId"`
	ToolName     string          `json:"toolName"`
	IsError      bool            `json:"isError"`
}

type wireBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Thinking  string          `json:"thinking"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

type wireUsage struct {
	Input       int64 `json:"input"`
	Output      int64 `json:"output"`
	CacheRead   int64 `json:"cacheRead"`
	CacheWrite  int64 `json:"cacheWrite"`
	TotalTokens int64 `json:"totalTokens"`
	Cost        struct {
		Total float64 `json:"total"`
	} `json:"cost"`
}

func (u *wireUsage) wire() *proto.Usage {
	if u == nil {
		return nil
	}
	return &proto.Usage{Input: u.Input, Output: u.Output, CacheRead: u.CacheRead, CacheWrite: u.CacheWrite, Total: u.TotalTokens, Cost: u.Cost.Total}
}

// blocks reads message content: a plain string, or an array of blocks.
func blocks(content json.RawMessage) []wireBlock {
	content = bytes.TrimSpace(content)
	if len(content) == 0 || string(content) == "null" {
		return nil
	}
	if content[0] == '"' {
		var text string
		if json.Unmarshal(content, &text) == nil {
			return []wireBlock{{Type: "text", Text: text}}
		}
		return nil
	}
	var out []wireBlock
	_ = json.Unmarshal(content, &out)
	return out
}

func joinText(bs []wireBlock, kind string) string {
	var parts []string
	for _, b := range bs {
		switch {
		case kind == "text" && b.Type == "text" && b.Text != "":
			parts = append(parts, b.Text)
		case kind == "thinking" && b.Type == "thinking" && b.Thinking != "":
			parts = append(parts, b.Thinking)
		}
	}
	return strings.Join(parts, "\n\n")
}

func millisTime(ms int64) time.Time {
	if ms == 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms).UTC()
}

// messageEntry fills an entry from one message's JSON.
func messageEntry(entry *proto.Entry, data []byte) {
	var m wireMessage
	if json.Unmarshal(data, &m) != nil {
		return
	}
	bs := blocks(m.Content)
	entry.Text = joinText(bs, "text")
	entry.At = millisTime(m.Timestamp)
	switch m.Role {
	case "assistant":
		entry.Thinking = joinText(bs, "thinking")
		for _, b := range bs {
			if b.Type == "toolCall" {
				args := b.Arguments
				if len(args) == 0 {
					args = json.RawMessage(`{}`)
				}
				entry.ToolCalls = append(entry.ToolCalls, proto.ToolCall{ID: b.ID, Name: b.Name, Arguments: args})
			}
		}
		if m.Provider != "" || m.Model != "" {
			entry.Model = &proto.ModelRef{Provider: m.Provider, ID: m.Model}
		}
		entry.Usage = m.Usage.wire()
		entry.StopReason = m.StopReason
		entry.Error = m.ErrorMessage
	case "toolResult":
		entry.ToolResult = &proto.ToolResult{CallID: m.ToolCallID, Name: m.ToolName, IsError: m.IsError, Text: entry.Text}
		entry.Text = ""
	case "user":
		// The images an owner's message gave the model (ADR-0029): its
		// text names every file.
		for _, b := range bs {
			if b.Type == "image" {
				entry.Images++
			}
		}
	}
}

var entryKinds = map[string]proto.EntryKind{
	durable.UserEntry.Kind:       proto.EntryUser,
	durable.AssistantEntry.Kind:  proto.EntryAssistant,
	durable.ToolResultEntry.Kind: proto.EntryToolResult,
	durable.SystemEntry.Kind:     proto.EntrySystem,
	durable.CompactionEntry.Kind: proto.EntryCompaction,
	durable.ResetEntry.Kind:      proto.EntryReset,
}

// PiG wraps a compaction's summary in these before the model reads it;
// the entry carries the summary alone (summaryOf).
const (
	summaryPrefix = "The conversation history before this point was compacted into the following summary:\n\n<summary>\n"
	summarySuffix = "\n</summary>"
)

// summaryOf is a compaction entry's summary: its message's text without
// PiG's wrapping, or the whole text when it is not wrapped so.
func summaryOf(text string) string {
	if inner, ok := strings.CutPrefix(text, summaryPrefix); ok {
		if summary, ok := strings.CutSuffix(inner, summarySuffix); ok {
			return summary
		}
	}
	return text
}

// wireEntry is the protocol form of a transcript entry.
func wireEntry(record durable.EntryRecord) proto.Entry {
	entry := proto.Entry{ID: int64(record.Id), SessionID: int64(record.ConversationId)}
	kind, ok := entryKinds[record.Kind]
	if !ok {
		entry.Kind = proto.EntryOther
		entry.Raw = record.Kind
		return entry
	}
	entry.Kind = kind
	if len(record.Model) > 0 {
		if data, err := json.Marshal(record.Model[0]); err == nil {
			messageEntry(&entry, data)
		}
	}
	if kind == proto.EntryCompaction {
		entry.Text = summaryOf(entry.Text)
	}
	return entry
}

// wireLive is the protocol form of a conversation's pi.live document,
// given as its JSON (nil when the document is absent), without the queue
// (queueCache).
func wireLive(sessionID int64, liveJSON []byte) proto.Live {
	live := proto.Live{SessionID: sessionID}
	if len(liveJSON) == 0 {
		return live
	}
	var state struct {
		Run        json.RawMessage `json:"run"`
		Generation *struct {
			Message json.RawMessage `json:"message"`
		} `json:"generation"`
		Tools []struct {
			CallID string `json:"callId"`
			Name   string `json:"name"`
			Status string `json:"status"`
		} `json:"tools"`
		RunFailure *struct {
			Reason string  `json:"reason"`
			Detail *string `json:"detail"`
		} `json:"runFailure"`
		Compactions       []json.RawMessage `json:"compactions"`
		CompactionFailure *struct {
			Error string `json:"error"`
		} `json:"compactionFailure"`
	}
	if json.Unmarshal(liveJSON, &state) != nil {
		return live
	}
	live.Busy = len(state.Run) > 0 && string(state.Run) != "null"
	if state.Generation != nil && len(state.Generation.Message) > 0 && string(state.Generation.Message) != "null" {
		partial := proto.Entry{SessionID: sessionID, Kind: proto.EntryAssistant}
		messageEntry(&partial, state.Generation.Message)
		live.Partial = &partial
	}
	for _, tool := range state.Tools {
		live.Tools = append(live.Tools, proto.LiveTool{CallID: tool.CallID, Name: tool.Name, Status: proto.ToolStatus(tool.Status)})
	}
	if state.RunFailure != nil {
		live.Failure = state.RunFailure.Reason
		if state.RunFailure.Detail != nil && *state.RunFailure.Detail != "" {
			live.Failure = state.RunFailure.Reason + ": " + *state.RunFailure.Detail
		}
	}
	live.Compacting = len(state.Compactions) > 0
	if state.CompactionFailure != nil {
		live.CompactionFailure = state.CompactionFailure.Error
		if live.CompactionFailure == "" {
			live.CompactionFailure = "the compaction failed"
		}
	}
	return live
}

// queueCache reads Live.Queued out of a conversation's pi.inbox, the queue
// of submissions that wait for a run: its user inputs, in order, without
// its passive entry writes. It lists at most proto.MaxQueuedListed, each
// with at most proto.MaxQueuedText bytes of text, and counts the rest: a
// queue grows with every message, and a live event must stay readable.
//
// It keeps what it read between live states. A queued submission never
// changes, so only a new one's content is read, and an inbox of the same
// submissions gives the same slice, which every live event the hub keeps
// shares: a reply streamed while messages wait neither reads nor copies
// them again. The zero value is ready.
type queueCache struct {
	ids    []int64 // every queued input, in order
	byID   map[int64]proto.QueuedMessage
	queued []proto.QueuedMessage
	more   int
}

func (c *queueCache) read(inbox durable.JsonObject) ([]proto.QueuedMessage, int) {
	if inbox == nil {
		*c = queueCache{}
		return nil, 0
	}
	raw, _ := inbox.Get("items")
	items, _ := raw.([]any)
	ids := make([]int64, 0, len(items))
	inputs := make([]durable.JsonObject, 0, len(items))
	for _, value := range items {
		item, ok := value.(durable.JsonObject)
		if !ok {
			continue
		}
		if mode, _ := item.Get("mode"); mode == string(harness.InboxWrite) {
			continue
		}
		raw, _ := item.Get("id")
		id, ok := jsonInt(raw)
		if !ok {
			continue
		}
		ids = append(ids, id)
		inputs = append(inputs, item)
	}
	if slices.Equal(ids, c.ids) && (len(ids) == 0 || c.byID != nil) {
		return c.queued, c.more
	}
	byID := make(map[int64]proto.QueuedMessage, min(len(ids), proto.MaxQueuedListed))
	var queued []proto.QueuedMessage
	more := 0
	for i, id := range ids {
		if len(queued) == proto.MaxQueuedListed {
			more++
			continue
		}
		q, ok := c.byID[id]
		if !ok {
			content, _ := inputs[i].Get("content")
			data, _ := json.Marshal(content)
			text, cut := cutText(joinText(blocks(data), "text"), proto.MaxQueuedText)
			q = proto.QueuedMessage{SubmissionID: id, Text: text, Cut: cut}
		}
		byID[id] = q
		queued = append(queued, q)
	}
	c.ids, c.byID, c.queued, c.more = ids, byID, queued, more
	return queued, more
}

// jsonInt reads a JSON number as an int64.
func jsonInt(value any) (int64, bool) {
	switch n := value.(type) {
	case float64:
		return int64(n), n == float64(int64(n))
	case int64:
		return n, true
	case int:
		return int64(n), true
	case json.Number:
		i, err := n.Int64()
		return i, err == nil
	}
	return 0, false
}

// cutText is text's first n bytes at most, cut at a character, and whether
// it cut anything.
func cutText(text string, n int) (string, bool) {
	if len(text) <= n {
		return text, false
	}
	for n > 0 && !utf8.RuneStart(text[n]) {
		n--
	}
	// A copy of its own: the cut shares the whole message's memory, and the
	// hub keeps thousands of live events.
	return strings.Clone(text[:n]), true
}

// viewLive reads the pi.live and pi.inbox documents out of a conversation
// view, the queue through queue.
func viewLive(sessionID int64, view harness.ConversationView, queue *queueCache) proto.Live {
	live := wireLive(sessionID, docJSON(view, "pi.live"))
	inbox, _ := view.Docs.Get("pi.inbox")
	live.Queued, live.QueuedMore = queue.read(inbox)
	live.Pending = inboxPending(inbox)
	return live
}

// inboxPending says pi.inbox holds anything a run will take: a message,
// or a write the queue does not list, as a compaction's summary that
// waits for its run's end (ADR-0026).
func inboxPending(inbox durable.JsonObject) bool {
	if inbox == nil {
		return false
	}
	raw, _ := inbox.Get("items")
	items, _ := raw.([]any)
	return len(items) > 0
}

// docJSON is the JSON of a view's document kind, nil when it is absent.
func docJSON(view harness.ConversationView, kind string) []byte {
	value, ok := view.Docs.Get(kind)
	if !ok {
		return nil
	}
	data, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	return data
}

// wireUsageState sums a conversation's usage ledger over its models.
func wireUsageState(state *harness.UsageState) proto.Usage {
	var total proto.Usage
	if state == nil {
		return total
	}
	for _, usage := range state.Models {
		data, err := json.Marshal(usage)
		if err != nil {
			continue
		}
		var u wireUsage
		if json.Unmarshal(data, &u) == nil {
			total = total.Add(*u.wire())
		}
	}
	return total
}
