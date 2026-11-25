package nsgoclient

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/frostyard/pocketgo/desktop/internal/pocketdtest"
	"github.com/frostyard/pocketgo/proto"
)

func TestInboxAsks(t *testing.T) {
	s := pocketdtest.New()
	s.AddSession(proto.Session{ID: 7, Title: "Release coordination", State: proto.SessionIdle})
	s.AddThread(proto.Thread{ID: 31, Title: "Prepare the release candidate", Session: 7, Agent: "editor", State: proto.ThreadOpen})
	first := proto.Ask{ID: 42, Thread: 31, SessionID: 7, Question: "The upgrade checks passed on Linux. Should I tag the release candidate or wait for the browser smoke test?", Options: []string{"Wait for the browser test", "Tag the candidate"}, State: proto.AskOpen}
	second := proto.Ask{ID: 43, Thread: 31, SessionID: 7, Question: "Which changes should lead the release notes?", Options: []string{"Workspace improvements", "Storage changes"}, State: proto.AskOpen}
	s.AddAsk(first)
	s.AddAsk(second)
	app := signedInApp(t, s, 1230, 800)
	app.Press("label:Settings")
	app.Press("text:Dark")
	app.Press("label:Close settings")
	app.WaitFor("text:2 asks wait for you", 3*time.Second)
	app.Find("label:Inbox, 2 asks wait for you")
	app.Find("text:A42 asks you")
	app.Find("text:A43 asks you")
	app.Screenshot("inbox-asks")
	app.Press("label:Open session for A42")
	app.WaitFor("label:Message", 3*time.Second)
	app.Press("text:Work board")
	app.Press("text:View ask A43")
	app.WaitFor("label:Answer to A43", 3*time.Second)
	app.Type("label:Answer to A43", "Lead with the offline workflow")
	field := app.Find("label:Answer to A43").ID
	s.Emit(proto.Event{Type: proto.EventAsk, Ask: &proto.Ask{ID: 44, Thread: 31, SessionID: 7, Question: "Use the new screenshots?", State: proto.AskOpen}})
	app.WaitFor("text:A44 asks you", 3*time.Second)
	if f := app.Find("label:Answer to A43"); f.ID != field || f.Text != "Lead with the offline workflow" {
		t.Fatal("a new ask replaced the active answer field")
	}
	app.Press("text:Wait for the browser test")
	app.WaitUntil(3*time.Second, func() bool { return !app.Exists("text:A42 asks you") })
	app.Press("label:Send answer to A43")
	app.WaitUntil(3*time.Second, func() bool { return !app.Exists("text:A43 asks you") })
	for id, want := range map[int]string{42: "Wait for the browser test", 43: "Lead with the offline workflow"} {
		reqs := s.RequestsTo("POST", fmt.Sprintf("/v1/asks/%d", id))
		var got proto.AnswerAsk
		if len(reqs) != 1 || json.Unmarshal(reqs[0].Body, &got) != nil || got.Answer != want {
			t.Fatalf("answer to A%d = %q, want %q", id, got.Answer, want)
		}
	}
	app.Type("label:Answer to A44", "Keep the dark theme")
	s.Emit(proto.Event{Type: proto.EventAsk, Ask: &proto.Ask{ID: 44, Thread: 31, SessionID: 7, Question: "Use the new screenshots?", State: proto.AskWithdrawn}})
	app.WaitFor("text:A44 was withdrawn", 3*time.Second)
	app.Find("text:Keep the dark theme")
	if app.Exists("label:Answer to A44") {
		t.Fatal("withdrawn ask is still answerable")
	}
	app.Find("label:Inbox")
}

func TestBoardRevealsAsk(t *testing.T) {
	s := pocketdtest.New()
	s.AddSession(proto.Session{ID: 9, Title: "Packaging", State: proto.SessionIdle})
	s.AddThread(proto.Thread{ID: 11, Session: 9, Title: "Publish artifacts", State: proto.ThreadBlocked})
	for id := int64(1); id <= 10; id++ {
		thread := int64(8)
		if id == 10 {
			thread = 11
		}
		s.AddAsk(proto.Ask{ID: id, Thread: thread, SessionID: 9, State: proto.AskOpen, Question: fmt.Sprintf("Approve artifact %d?", id)})
	}
	app := signedInApp(t, s, 1100, 700)
	app.WaitFor("text:A10 asks you", 3*time.Second)
	if y := app.Find("text:A10 asks you").Bounds().Origin.Y; y < 700 {
		t.Fatalf("target already visible at %v", y)
	}
	app.Press("text:Work board")
	// Its thread has this one ask: Open the Inbox goes to its card.
	app.Press(card(11) + " >> text:Open the Inbox")
	app.WaitFor("text:A10 asks you", 3*time.Second)
	app.WaitUntil(3*time.Second, func() bool {
		y := app.Find("text:A10 asks you").Bounds().Origin.Y
		return y > 0 && y < 650
	})
}
