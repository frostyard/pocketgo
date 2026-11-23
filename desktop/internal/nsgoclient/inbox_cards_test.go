package nsgoclient

import (
	"testing"
	"time"

	"github.com/frostyard/pocketgo/desktop/internal/pocketdtest"
	"github.com/frostyard/pocketgo/proto"
)

func TestMixedInboxCards(t *testing.T) {
	s := pocketdtest.New()
	s.AddSession(proto.Session{ID: 3, Title: "Desktop release", State: proto.SessionWaiting})
	s.PutQuestion(proto.Question{ID: "formats", SessionID: 3, Question: "Which platforms should get the release candidate?", Options: []string{"Linux", "Web"}, Multiple: true})
	s.AddApproval(proto.Approval{ID: "write", SessionID: 3, Tool: "write", Arguments: []byte(`{"path":"release/notes.md"}`), State: proto.ApprovalPending})
	s.AddPlan(proto.Plan{ID: 8, Thread: 41, Lead: "maintainer", Title: "Verify the release candidate", Text: "Run the smoke tests on Linux and in Chromium.", State: proto.PlanReview, Rev: "abc1234", Graph: &proto.PlanGraph{Rev: "abc1234", Version: 2, Tasks: []proto.PlanTask{{ID: "T1", Title: "Compare the two clients", Agent: "reviewer", State: proto.TaskReady}}}})
	lesson := proto.Lesson{ID: 9, Agent: "maintainer", State: proto.LessonPending, Text: "Record the tested commit in every release report."}
	s.AddLesson(lesson)
	app := signedInApp(t, s, 1230, 980)
	app.Press("label:Settings")
	app.Press("label:Theme")
	app.Press("text:Dark")
	app.Press("label:Close settings")
	for _, label := range []string{"Session question", "Tool approval", "Plan review", "New lesson", "Approve plan", "Approve lesson"} {
		app.WaitFor("text:"+label, 3*time.Second)
	}
	app.Screenshot("inbox-review-cards")

	app.Type("label:Decision reason", "Use the staging directory")
	field := app.Find("label:Decision reason").ID
	lesson.State = proto.LessonActive
	lesson.Change = &proto.LessonChange{Forget: true}
	s.Emit(proto.Event{Type: proto.EventLesson, Lesson: &lesson})
	app.WaitFor("text:Approve removal", 3*time.Second)
	app.Find("text:" + lesson.Text)
	if f := app.Find("label:Decision reason"); f.ID != field || f.Text != "Use the staging directory" {
		t.Fatal("a lesson update replaced the approval editor")
	}
	app.Press("text:Approve removal")
	app.WaitUntil(3*time.Second, func() bool { return !app.Exists("text:Forget lesson") })
	app.Resize(390, 730)
	for _, label := range []string{"Approve plan", "Allow", "Copy arguments"} {
		r := app.Find("label:" + label).Bounds()
		if r.Origin.X < 0 || r.Origin.X+r.Size.Width > 390 {
			t.Fatalf("%s outside compact card: %v", label, r)
		}
	}
}
