package nsgoclient

import (
	"fmt"
	"testing"
	"time"

	"github.com/frostyard/pocketgo/desktop/internal/pocketdtest"
	"github.com/frostyard/pocketgo/proto"
)

// Mounting this standing session in one pass previously hit NSGo's
// one-million-effect settling limit before the owner could use the composer.
func TestSupervisorHistory(t *testing.T) {
	s := pocketdtest.New()
	s.AddProfile(proto.Profile{ID: "lead", Name: "Lead", Role: proto.RoleSupervisor, Standing: true})
	const reports = 3200
	var es []proto.Entry
	for i := 1; i <= reports; i++ {
		report := fmt.Sprintf("Report %d\n\n- First item\n- Second item\n\n```go\nfunc main() {}\n```\n\n| Date | Work |\n| ---- | ---- |\n| Wed | Review |\n\n`notes.md`", i)
		if i == 50 {
			report = "Historical checkpoint"
		}
		es = append(es, proto.Entry{
			ID: int64(2*i - 1), Kind: proto.EntryUser, Text: "[Report] Specialist finished.",
			Sender: &proto.Sender{Kind: proto.SenderService},
		}, proto.Entry{
			ID: int64(2 * i), Kind: proto.EntryAssistant, Text: report, Thinking: "Checking reports.",
			ToolCalls: []proto.ToolCall{{ID: fmt.Sprint(i), Name: "team_list", Arguments: []byte("{}")}},
		})
	}
	s.AddSession(proto.Session{ID: 1, Title: "Duty supervisor", Profile: "lead", Standing: true, State: proto.SessionIdle}, es...)
	app := signedInApp(t, s, 1230, 800)
	app.WaitFor("text:Duty supervisor", 5*time.Second)
	app.Press("text:Duty supervisor")
	app.WaitFor("text:Report 3200", 10*time.Second)
	if app.Exists("text:Historical checkpoint") {
		t.Fatal("distant history was mounted on open")
	}
	app.Press("text:Earlier messages")
	app.WaitFor("text:Report 3188", 5*time.Second)
	app.Press("text:Later messages")
	app.WaitFor("text:Report 3200", 5*time.Second)
	app.Key("mod+f")
	app.WaitFor("id:chat-find-input", 5*time.Second)
	app.Type("id:chat-find-input", "checkpoint")
	app.WaitFor("text:Historical checkpoint", 5*time.Second)
	app.Press("id:chat-find-close")
	app.WaitFor("text:Report 3200", 5*time.Second)
	app.Press("text:Search")
	app.Type("label:Search all sessions", "checkpoint")
	app.Press("text:Search sessions")
	app.WaitFor("text:Historical checkpoint Tools: team_list", 5*time.Second)
	app.Press("text:Historical checkpoint Tools: team_list")
	app.WaitFor("text:Historical checkpoint", 5*time.Second)
	app.Press("text:Latest messages")
	app.WaitFor("text:Report 3200", 5*time.Second)
	app.Type("label:Message", "Continuing the thread.")
	app.Press("label:Send message")
	app.WaitFor("text:Continuing the thread.", 5*time.Second)
}
