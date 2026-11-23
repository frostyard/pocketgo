package app

import (
	"encoding/json"
	"errors"
	"os"
	"testing"

	"github.com/egoist/mygo/ui"
	"github.com/frostyard/pocketgo/desktop/internal/state"
	"github.com/frostyard/pocketgo/proto"
)

const copyMarkdown = "## Plan\n\n- **one**\n- `two`\n"
const copySession = 41

func (x *testApp) seedCopy() {
	x.srv.AddSession(proto.Session{ID: copySession, Title: "Plan the change", Model: opusRef},
		proto.Entry{ID: 1, Kind: proto.EntryUser, Text: "Make a plan."},
		proto.Entry{ID: 2, Kind: proto.EntryUser, Text: "Keep it small.", Sender: &proto.Sender{Kind: proto.SenderSupervisor, Session: 7}},
		proto.Entry{ID: 3, Kind: proto.EntryAssistant, Text: copyMarkdown, Thinking: "secret thought"},
		proto.Entry{ID: 4, Kind: proto.EntryAssistant, Text: "Reading the file.", ToolCalls: []proto.ToolCall{{ID: "read1", Name: "read", Arguments: json.RawMessage(`{"path":"go.mod"}`)}}},
		proto.Entry{ID: 5, Kind: proto.EntryToolResult, ToolResult: &proto.ToolResult{CallID: "read1", Name: "read", Text: "module example"}},
		proto.Entry{ID: 6, Kind: proto.EntryAssistant, Text: "The plan is ready."},
	)
}

func copyApp(t *testing.T, live bool) *testApp {
	t.Helper()
	x := newTestApp(t)
	x.tt = ui.NewTesterWith(x.a.View, 1280, 1100, ui.TesterOptions{Dark: os.Getenv("POCKETGO_DARK") != ""})
	x.seedCopy()
	if live {
		x.srv.SetLive(proto.Live{SessionID: copySession, Busy: true, Partial: &proto.Entry{Kind: proto.EntryAssistant, Text: "Still writing"}})
	}
	x.connect(t)
	x.openCopySession(t)
	return x
}

func (x *testApp) openCopySession(t *testing.T) {
	t.Helper()
	x.a.openSession(copySession)
	x.until(t, func() bool { o := x.a.st.Data.Open(); return o != nil && o.ID == copySession && o.Loaded })
}

func (x *testApp) copyButtons(label string) []ui.AccessInfo {
	var nodes []ui.AccessInfo
	for _, n := range x.tt.AccessAll() {
		if n.Role == ui.RoleButton && n.Name == label {
			nodes = append(nodes, n)
		}
	}
	return nodes
}

func (x *testApp) wantCopyButtons(t *testing.T, copyCount, copiedCount int) {
	t.Helper()
	if got := len(x.copyButtons("Copy")); got != copyCount {
		t.Fatalf("Copy buttons = %d, want %d", got, copyCount)
	}
	if got := len(x.copyButtons("Copied")); got != copiedCount {
		t.Fatalf("Copied buttons = %d, want %d", got, copiedCount)
	}
}

// Each finished reply has a Copy button, which puts its Markdown on the
// clipboard, without its thinking, and shows Copied for state.CopyHold
// (ADR-0067).
func TestACopyButtonCopiesAReplysMarkdown(t *testing.T) {
	x := copyApp(t, false)
	x.wantCopyButtons(t, 3, 0)
	x.click(t, "Copy")
	if got := x.tt.Clipboard(); got != copyMarkdown {
		t.Fatalf("clipboard = %q, want Markdown source %q", got, copyMarkdown)
	}
	x.wantCopyButtons(t, 2, 1)
	x.tt.Advance(state.CopyHold)
	x.settle()
	x.wantCopyButtons(t, 3, 0)
}

// The reply still being written has no Copy button (ADR-0067).
func TestNoCopyButtonOnAReplyStillBeingWritten(t *testing.T) {
	x := copyApp(t, true)
	x.hasAll(t, "Still writing")
	x.wantCopyButtons(t, 3, 0)
}

// A copy the clipboard refused says why, and shows no Copied (ADR-0067).
func TestAFailedCopySaysSo(t *testing.T) {
	x := copyApp(t, false)
	x.tt.FailClipboard(errors.New("the clipboard is busy"))
	x.click(t, "Copy")
	x.hasAll(t, "Copying the reply failed: the clipboard is busy")
	x.wantCopyButtons(t, 3, 0)
}

// The Copy button takes a finger's size on iOS alone (ADR-0067).
func TestTheCopyButtonIsTouchSizedOnIOS(t *testing.T) {
	x := copyApp(t, false)
	x.wantCopyButtons(t, 3, 0)
	for _, n := range x.copyButtons("Copy") {
		if n.Bounds.H >= 44 {
			t.Errorf("desktop Copy height = %v, want below 44", n.Bounds.H)
		}
	}
	phone := newTestApp(t)
	phone.phoneApp(t, false, nil)
	phone.seedCopy()
	phone.openCopySession(t)
	buttons := phone.copyButtons("Copy")
	if len(buttons) == 0 {
		t.Fatal("no phone Copy button")
	}
	for _, n := range buttons {
		if n.Bounds.W < 44 || n.Bounds.H < 44 {
			t.Errorf("phone Copy bounds = %+v, want at least 44 by 44", n.Bounds)
		}
	}
}

// The Copy buttons under the replies, and one that copied (ADR-0067).
func TestCopyButtonSnapshot(t *testing.T) {
	x := copyApp(t, false)
	x.wantCopyButtons(t, 3, 0)
	x.snapshot(t, "session-copy")
	x.click(t, "Copy")
	x.wantCopyButtons(t, 2, 1)
	x.snapshot(t, "session-copied")
}
