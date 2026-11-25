package nsgoclient

import (
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/frostyard/pocketgo/desktop/internal/state"
	"github.com/nsgo-dev/nsgo/core"
	"github.com/nsgo-dev/nsgo/ui/chat"
)

type turnPart struct {
	key  string
	work bool
	rows []state.Row
}

// User messages delimit turns. Questions, approval requests, service notices
// and explicit navigation targets stay in chronological order outside folds.
func groupWork(rows []state.Row, reveal string) []turnPart {
	var out []turnPart
	for len(rows) > 0 {
		n := 1
		work := foldableRow(rows[0], reveal)
		if work {
			for n < len(rows) && foldableRow(rows[n], reveal) {
				n++
			}
		}
		key := rows[0].Key
		if work {
			key = "work/" + key
		}
		out = append(out, turnPart{key, work, rows[:n]})
		rows = rows[n:]
	}
	return out
}
func foldableRow(r state.Row, reveal string) bool {
	return r.Key != reveal && (r.Kind == state.RowAssistant || r.Kind == state.RowTool && r.Tool.State != state.ToolWaiting)
}
func (c *client) workTurn(cx *core.Scope, part func() turnPart, view *chat.ConversationState) core.Node {
	rows := core.NewMemoFunc(cx, func() []state.Row { return part().rows }, func(a, b []state.Row) bool { return reflect.DeepEqual(a, b) }).Get
	last := func() state.Row { rs := rows(); return rs[len(rs)-1] }
	hasFinal := func() bool { r := last(); return r.Kind == state.RowAssistant && r.Text != "" }
	// Step views outlive individual revisions. Resolve their row by stable
	// identity so streamed prose and tool output keep updating in place.
	lookup := func(key string) state.Row {
		for _, r := range rows() {
			if r.Key == key {
				return r
			}
		}
		return state.Row{}
	}
	steps := func() []chat.Step {
		var result []chat.Step
		for i, r := range rows() {
			key := r.Key
			if r.Thinking != "" {
				result = append(result, chat.Step{ID: key + "/thinking", Message: func(*core.Scope) core.Node {
					return chat.Reasoning{Conversation: view, Text: func() string { return lookup(key).Thinking }}
				}})
			}
			switch {
			case r.Kind == state.RowTool:
				result = append(result, chat.Step{ID: key, Message: func(*core.Scope) core.Node {
					return chat.ActivityRow{Conversation: view, Activity: func() chat.Activity { return toolActivity(func() *state.ToolRow { return lookup(key).Tool }) }}
				}})
			case r.Text != "" && (i != len(rows())-1 || !hasFinal()):
				result = append(result, chat.Step{ID: key, Message: func(*core.Scope) core.Node {
					return c.markdown(func() string { return lookup(key).Text }, view.Search())
				}})
			}
		}
		return result
	}
	return chat.Turn{Conversation: view, Steps: steps, HasFinal: hasFinal,
		Summary: func() string {
			r := last()
			if !r.Started.IsZero() && r.At.After(r.Started) {
				return "Worked for " + r.At.Sub(r.Started).Round(time.Second).String()
			}
			n := len(steps())
			if n == 1 {
				return "1 work step"
			}
			return fmt.Sprintf("%d work steps", n)
		},
		Status: func() chat.Status {
			if last().Live {
				return chat.Running
			}
			return chat.Completed
		},
		Working: func() string { return "Working…" },
		Final: func(*core.Scope) core.Node {
			return c.markdown(func() string { return last().Text }, view.Search())
		},
		ShowActions: hasFinal, Actions: func(*core.Scope) core.Node {
			return chat.Actions{Receipt: state.CopyHold, Text: func() string { t, _ := last().Copy(); return t }}
		}}
}
func toolActivity(current func() *state.ToolRow) chat.Activity {
	t := current()
	if t == nil {
		return chat.Activity{}
	}
	a := chat.Activity{ID: t.CallID, Action: t.Name, Detail: t.Summary + " · " + t.State.String(), Status: chat.Completed, Mono: true, Content: func(*core.Scope) core.Node {
		return core.Text{Selectable: core.SelectableOn, ContentFunc: func() string {
			t := current()
			if t == nil {
				return ""
			}
			return strings.TrimSpace(t.Arguments + "\n" + t.Result)
		}}
	}}
	switch t.State {
	case state.ToolFailed:
		a.Status = chat.Failed
	case state.ToolRunning, state.ToolQueued:
		a.Status = chat.Running
	case state.ToolWaiting:
		a.Status = chat.Waiting
	}
	return a
}
