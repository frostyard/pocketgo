package app

import (
	"testing"

	"github.com/egoist/mygo/ui"
	"github.com/frostyard/pocketgo/proto"
)

const serviceReport = "[T300 · Murbella · done] **Merged** frostyard/pocketgo#151: the launch now ends at the next 06:45 by default.\n\n## What changed\n\n- `team.LaunchEnd` defaults to 06:45\n- the end chooser opens on Until\n\nCI green; no asks."
const serviceReportLine = "Merged frostyard/pocketgo#151: the launch now ends at the next 06:45 by default."
const serviceReportLabel = "[T300 · Murbella · done] " + serviceReportLine

func (x *testApp) addServiceSession() {
	x.srv.AddSession(proto.Session{ID: 30, Title: "Team reports", Profile: "odrade", Route: "profile", State: proto.SessionIdle,
		Cwd: "/home/team/agents/odrade", Model: proto.ModelRef{Provider: "anthropic", ID: "claude-opus-5-5"}, UpdatedAt: at(0)},
		proto.Entry{ID: 1, SessionID: 30, Kind: proto.EntryUser, Text: "Merge what is ready, then tell me what is left."},
		proto.Entry{ID: 2, SessionID: 30, Kind: proto.EntryAssistant, Text: "Delegated T300 to Murbella."},
		proto.Entry{ID: 3, SessionID: 30, Kind: proto.EntryUser, Sender: &proto.Sender{Kind: proto.SenderService}, Text: serviceReport},
		proto.Entry{ID: 4, SessionID: 30, Kind: proto.EntryAssistant, Text: "T300 is done."},
		proto.Entry{ID: 5, SessionID: 30, Kind: proto.EntryUser, Sender: &proto.Sender{Kind: proto.SenderService}, Text: "[scheduled] Morning brief: list what waits for the owner."},
		proto.Entry{ID: 6, SessionID: 30, Kind: proto.EntryUser, Text: "[T300 · Murbella · done] typed by me, stays whole"},
	)
}

func (x *testApp) openServiceSession(t *testing.T) {
	t.Helper()
	x.a.openSession(30)
	x.until(t, func() bool { o := x.a.st.Data.Open(); return o != nil && o.Loaded && o.ID == 30 })
}

func (x *testApp) serviceBodyHidden(t *testing.T) {
	t.Helper()
	for _, text := range []string{"What changed", "CI green; no asks."} {
		if x.tt.HasText(text) {
			t.Errorf("folded service message shows %q", text)
		}
	}
}

func TestAServiceMessageShowsFoldedSnapshot(t *testing.T) {
	x := newTestApp(t)
	x.seedTeam()
	x.addServiceSession()
	x.connect(t)
	x.openServiceSession(t)
	x.hasAll(t, "[T300 · Murbella · done]", serviceReportLine, "[scheduled]", "[T300 · Murbella · done] typed by me, stays whole", "You")
	x.serviceBodyHidden(t)
	if n, ok := x.tt.AccessByRole(ui.RoleButton, serviceReportLabel); !ok || n.Expanded || !n.Focusable {
		t.Fatalf("folded button: %+v, found %v", n, ok)
	}
	x.snapshot(t, "session-service-messages-folded")
	x.click(t, serviceReportLabel)
	x.hasAll(t, "What changed", "CI green; no asks.")
	if n, ok := x.tt.AccessByRole(ui.RoleButton, serviceReportLabel); !ok || !n.Expanded {
		t.Fatalf("open button: %+v, found %v", n, ok)
	}
	x.snapshot(t, "session-service-message-open")
	x.click(t, serviceReportLabel)
	x.serviceBodyHidden(t)
}

func TestAServiceMessageOpensFromTheKeyboard(t *testing.T) {
	x := newTestApp(t)
	x.seedTeam()
	x.addServiceSession()
	x.connect(t)
	x.openServiceSession(t)
	x.serviceBodyHidden(t)
	for range 40 {
		if x.tt.Focused(serviceReportLabel) {
			break
		}
		x.tt.Key(0, ui.KeyTab)
		x.settle()
	}
	if !x.tt.Focused(serviceReportLabel) {
		t.Fatal("Tab did not focus the service message's button")
	}
	x.tt.Key(0, ui.KeyEnter)
	x.settle()
	x.hasAll(t, "What changed", "CI green; no asks.")
	x.tt.Key(0, ui.KeySpace)
	x.settle()
	x.serviceBodyHidden(t)
}

func TestAPhoneFoldsAServiceMessageSnapshot(t *testing.T) {
	x := newTestApp(t)
	x.addServiceSession()
	x.phoneApp(t, false, nil)
	x.openServiceSession(t)
	x.hasAll(t, "[T300 · Murbella · done]", serviceReportLine)
	x.serviceBodyHidden(t)
	x.snapshot(t, "phone-service-message-folded")
}
