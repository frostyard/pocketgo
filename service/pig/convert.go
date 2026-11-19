package pig

import (
	"bytes"
	"encoding/json"
	"strings"
	"time"

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
	}
}

var entryKinds = map[string]proto.EntryKind{
	durable.UserEntry.Kind:       proto.EntryUser,
	durable.AssistantEntry.Kind:  proto.EntryAssistant,
	durable.ToolResultEntry.Kind: proto.EntryToolResult,
	durable.SystemEntry.Kind:     proto.EntrySystem,
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
	return entry
}

// wireLive is the protocol form of a conversation's pi.live document, given
// as its JSON (nil when the document is absent).
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
	return live
}

// viewLive reads the pi.live document out of a conversation view.
func viewLive(sessionID int64, view harness.ConversationView) proto.Live {
	value, ok := view.Docs.Get("pi.live")
	if !ok {
		return proto.Live{SessionID: sessionID}
	}
	data, err := json.Marshal(value)
	if err != nil {
		return proto.Live{SessionID: sessionID}
	}
	return wireLive(sessionID, data)
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
