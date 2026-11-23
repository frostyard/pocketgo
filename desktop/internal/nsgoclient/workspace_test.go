package nsgoclient

import (
	"encoding/json"
	"github.com/frostyard/pocketgo/desktop/workspaceui"
	"github.com/nsgo-dev/nsgo/core"
	"github.com/nsgo-dev/nsgo/nsgotest"
	"testing"
	"time"

	"github.com/frostyard/pocketgo/desktop/internal/pocketdtest"
	"github.com/frostyard/pocketgo/proto"
)

func TestBusyTeamWorkspace(t *testing.T) {
	s := pocketdtest.New()
	pocketdtest.AddBusyTeam(s)
	app := signedInApp(t, s, 1230, 800)
	app.WaitFor("text:A46 asks you", 5*time.Second)
	app.Press("text:Work board")
	app.Press("text:View ask A43")
	app.Press("label:Open session for A43")
	app.WaitFor("label:Message", 5*time.Second)
	app.Find("label:Answer to A43")
	app.Press("label:Back")
	app.WaitFor("label:Open session for A43", 5*time.Second)
	app.Press("label:Back")
	app.WaitFor("text:Verify browser reconnection", 5*time.Second)
	app.Press("label:Forward")
	app.WaitFor("label:Open session for A43", 5*time.Second)
	app.Press("text:Release supervisor")
	app.WaitFor("text:Candidate 64 passed both target checks.", 5*time.Second)
	app.Find("label:Worked for 8s")
	app.Find("text:Earlier messages")
	app.Screenshot("busy-team-conversation")
	app.Press("label:Worked for 8s")
	app.Find("text:read")
	app.Press("id:composer-model")
	app.Press("text:Scribe")
	app.WaitUntil(5*time.Second, func() bool { return len(s.RequestsTo("POST", "/v1/sessions/10/model")) == 1 })
	app.WaitUntil(3*time.Second, func() bool { return !app.Exists("id:composer-effort") })
	app.Press("label:Context usage")
	app.Find("text:Context usage")
}

func TestInspectorPreservesRename(t *testing.T) {
	s := pocketdtest.New()
	s.AddSession(proto.Session{ID: 5, Title: "Private workspace", Renamable: true, State: proto.SessionIdle})
	app := signedInApp(t, s, 1230, 800)
	app.Press("text:Private workspace")
	app.Press("label:Session details")
	app.Clear("label:Session title")
	app.Type("label:Session title", "New title")
	app.Focus("label:Session title")
	field := app.Find("label:Session title").ID
	s.Emit(proto.Event{Type: proto.EventAsk, Ask: &proto.Ask{ID: 25, Question: "Use the new name?", State: proto.AskOpen, SessionID: 5}})
	app.WaitFor("text:A25 asks you", 5*time.Second)
	if f := app.Find("label:Session title"); f.ID != field || f.Text != "New title" || !f.Focused {
		t.Fatal("incoming activity replaced the title editor")
	}
}

func TestSavedPaneWidths(t *testing.T) {
	s := pocketdtest.New()
	s.AddSession(proto.Session{ID: 4, Title: "Layout preview", State: proto.SessionIdle})
	app := signedInApp(t, s, 1230, 800, nsgotest.Storage(map[string]string{"pane-widths": `{"Navigation":260,"Inspector":320}`}))
	app.Press("text:Layout preview")
	app.Press("label:Session details")
	app.WaitUntil(3*time.Second, func() bool {
		return app.Find("id:sidebar-panel").Bounds().Size.Width == 260 && app.Find("id:right-panel").Bounds().Size.Width == 320
	})
	app.Drag("label:Resize navigation", core.Point{X: 38})
	app.Drag("label:Resize right panel", core.Point{X: -30})
	app.Advance(time.Second)
	var saved workspaceui.PaneWidths
	app.WaitUntil(3*time.Second, func() bool {
		v, _ := app.Storage().GetItem("pane-widths")
		return json.Unmarshal([]byte(v), &saved) == nil && saved.Navigation > 260 && saved.Inspector > 320
	})
}
