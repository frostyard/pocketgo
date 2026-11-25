package nsgoclient

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/frostyard/pocketgo/desktop/internal/pocketdtest"
	"github.com/frostyard/pocketgo/proto"
)

// TestTheOwnerForgetsALessonInForce checks the Inbox's list of lessons in
// force: folded, after what waits, with each lesson's number, agent and
// text, and Forget, which removes it at once (ADR-0015). It holds for the
// NSGo client what the test of the same name holds for the MyGo client.
func TestTheOwnerForgetsALessonInForce(t *testing.T) {
	s := pocketdtest.New()
	s.AddLesson(proto.Lesson{ID: 4, Agent: "vera", Text: "Squash before merging.", State: proto.LessonActive, Writer: "vera", CreatedAt: time.Now().Add(-time.Minute)})
	app := signedInApp(t, s, 1230, 800)
	app.WaitFor("text:Lessons in force", 5*time.Second)
	app.Find("text:You’re caught up")
	if app.Exists("text:Squash before merging.") {
		t.Fatal("the folded list shows its lessons")
	}
	app.Press("text:Lessons in force")
	app.WaitFor("text:Squash before merging.", 5*time.Second)
	app.Find("text:L4 · vera")
	app.Press("label:Forget L4")
	app.WaitForGone("text:Lessons in force", 5*time.Second)
	var body proto.DecideLesson
	reqs := s.RequestsTo(http.MethodPost, proto.Path(proto.PathLesson, 4))
	if len(reqs) != 1 || json.Unmarshal(reqs[0].Body, &body) != nil || body.Decision != proto.LessonForget || body.Text != "" || body.Forget {
		t.Fatalf("the forget sent %d requests: %+v", len(reqs), body)
	}
}
